package cliprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	gopty "github.com/aymanbagabas/go-pty"
)

func cliEventLogFields(raw []byte, rawEnabled bool) []any {
	if rawEnabled {
		return []any{"raw", string(raw)}
	}
	var event struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &event)
	switch event.Type {
	case "system", "assistant", "user", "result", "stream_event",
		"init", "message", "thread.started", "turn.started", "item.started",
		"item.updated", "item.completed", "turn.completed", "turn.failed", "error":
		// These are known CLI protocol event names, not arbitrary payload text.
	default:
		event.Type = "unknown"
	}
	return []any{"type", event.Type, "bytes", len(raw)}
}

func (m *cliModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	s, err := m.StartStream(ctx, call)
	if err != nil {
		return nil, err
	}
	return s.Stream(), nil
}

func usePromptStdin(spec CLISpec, noPTY bool, prompt string) bool {
	return spec.AlwaysStdin || noPTY || len(prompt) > effectiveMaxPromptArgLen(spec.Binary) ||
		strings.ContainsAny(prompt, "\r\n")
}

func (m *cliModel) StartStream(ctx context.Context, call fantasy.Call) (*StreamSession, error) {
	binaryPath, resolveErr := resolveLaunchBinary(m.spec.Binary)
	if resolveErr != nil {
		return nil, fmt.Errorf("resolve %s: %w", m.spec.Binary, resolveErr)
	}

	yolo := m.yoloFn != nil && m.yoloFn()
	// Fork patch: batch 14 — `rush run` has no human at the keyboard, so any
	// inner CLI process MUST get bypass-permissions or it will hang on the
	// interactive permission prompt. RunNonInteractive sets this context key.
	if !yolo {
		if v, ok := ctx.Value(NonInteractiveContextKey).(bool); ok && v {
			yolo = true
		}
	}

	// Save any attached files (images, etc.) to temp dir so the CLI agent
	// can access them via its file-reading tools.
	attachTmpDir, filePaths, fileErr := saveFileParts(call.Prompt)
	if fileErr != nil {
		slog.Warn("cliprovider: failed to save attachments", "err", fileErr)
	}
	if len(filePaths) > 0 {
		slog.Info("cliprovider: saved attachments to temp dir", "dir", attachTmpDir, "count", len(filePaths))
	}

	prompt := formatPrompt(call.Prompt, filePaths)

	// Will be overridden below if resuming (only the new user message is needed).
	var resumePrompt string

	args := m.spec.BuildArgs(yolo)

	// Apply dynamic reasoning effort from context so the UI toggle takes
	// effect. Dispatch is per-spec: this used to append `--effort <level>` to
	// whatever binary was being launched, which killed every non-claude run
	// the moment a session carried an effort. See effort.go.
	if effort, ok := ctx.Value(ReasoningEffortContextKey).(string); ok {
		args = m.spec.applyEffort(args, effort)
	}

	// Extract session ID from context (set by agent.go before calling Stream).
	sessionID, _ := ctx.Value(SessionIDContextKey).(string)

	// Resume a previous CLI session if available, to leverage API prompt caching.
	// The key includes the model ID so switching models starts a fresh session.
	// We also hash the conversation prefix (all messages except the last user
	// message) and only resume if the hash matches — this detects edits/deletes
	// that would make the CLI session's history stale.
	var resuming bool
	var cliSessionKey string
	var prefixHash uint64
	if m.spec.SupportsResume && sessionID != "" && ctx.Value(TitleGenerationContextKey) != true {
		cliSessionKey = sessionID + ":" + m.spec.ModelID
		prefixHash = hashPromptPrefix(call.Prompt)
		if entry, ok := m.cliSessions.Get(cliSessionKey); ok {
			if entry.PrefixHash == prefixHash {
				args = append(args, "--resume", entry.CLISessionID)
				resuming = true
				resumePrompt = extractLatestUserMessage(call.Prompt, filePaths)
				slog.Info("cliprovider: resuming CLI session", "rushSession", sessionID, "cliSession", entry.CLISessionID, "resumePromptLen", len(resumePrompt))
			} else {
				// History was edited/deleted — start fresh CLI session.
				m.cliSessions.Del(cliSessionKey)
				slog.Info("cliprovider: conversation prefix changed, starting fresh CLI session", "rushSession", sessionID)
			}
		}
	}
	if resuming {
		prompt = resumePrompt
	}

	// When running in non-yolo mode with a spec that opts into rush's MCP
	// server, start an in-process MCP server and pass its config to the CLI
	// so tool calls go through rush's permission dialog instead of the CLI's
	// own (invisible) permission prompts.
	// mcpSrv and mcpTmpCfg are cleaned up inside the returned closure, not
	// via defer here — defer in Stream() would fire when Stream() returns
	// (before the closure runs), deleting the config file before claude CLI
	// can read it.
	var mcpSrv *rushMCPServer
	var mcpTmpCfg string     // path to temp MCP config file (claude-style); "" if not used
	var qwenMCPName string   // registered name in ~/.qwen/settings.json; "" if not used
	var geminiMCPName string // registered name in ~/.gemini/settings.json; "" if not used
	var mcpSetupErr error
	// Fork patch: batch 20 — keep the MCP bridge active even in yolo/bypass mode.
	// Before this fix, `!yolo` here meant that `rush run` (which sets yolo=true via
	// NonInteractiveContextKey in batch 14) would skip MCP setup entirely. Inner
	// claude then ran with no --allowedTools and could reach for its native Bash /
	// Write / Task tools, bypassing agentguard (batch 16) and the MCP permission
	// dialog. yolo only controls whether claude needs the bypass-permissions flag,
	// not whether our MCP bridge sits in the loop.
	bridge := invocationToolBridge(ctx)
	if m.spec.UseRushMCP && (m.perms != nil || bridge != nil) {
		var err error
		mcpSrv, err = newRushMCPServer(ctx, m.perms, m.sessions, sessionID, m.workingDir, "", m.mcpProxy)
		if err != nil {
			if attachTmpDir != "" {
				_ = os.RemoveAll(attachTmpDir)
			}
			return nil, fmt.Errorf("cliprovider: required MCP server failed to start: %w", err)
		}
		cfgJSON, jsonErr := mcpSrv.mcpConfigJSON()
		if jsonErr != nil {
			mcpSrv.stop()
			if attachTmpDir != "" {
				_ = os.RemoveAll(attachTmpDir)
			}
			return nil, fmt.Errorf("cliprovider: required MCP config failed: %w", jsonErr)
		}
		tmpFile, tmpErr := os.CreateTemp("", "rush-mcp-*.json")
		if tmpErr != nil {
			mcpSrv.stop()
			if attachTmpDir != "" {
				_ = os.RemoveAll(attachTmpDir)
			}
			return nil, fmt.Errorf("cliprovider: required MCP config file failed: %w", tmpErr)
		}
		if _, werr := tmpFile.Write(cfgJSON); werr != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmpFile.Name())
			mcpSrv.stop()
			if attachTmpDir != "" {
				_ = os.RemoveAll(attachTmpDir)
			}
			return nil, fmt.Errorf("cliprovider: required MCP config write failed: %w", werr)
		}
		_ = tmpFile.Close()
		mcpTmpCfg = tmpFile.Name()
		args = append(args, "--mcp-config", mcpTmpCfg)
		slog.Info("cliprovider: MCP config written", "path", mcpTmpCfg, "addr", mcpSrv.addr)
	}

	// When rush's own MCP server is active, the CLI may use only rush's MCP
	// tools plus a few built-ins rush doesn't replicate (see rushMCPToolArgs).
	if mcpSrv != nil {
		args = append(args, rushMCPToolArgs(bridge, m.mcpProxy)...)
	}

	// Qwen MCP integration: register rush's MCP server in ~/.qwen/settings.json
	// using a stable per-project ID, suffixed with a session-specific hash because
	// concurrent rush sessions must not overwrite one another's registration.
	// Qwen doesn't support --mcp-config, so we write the settings directly.
	// The Authorization: Bearer header is stored in the settings (qwen CLI
	// supports custom headers for httpUrl transports); the server is
	// localhost-only with a random port.
	if m.spec.QwenMCPIntegration && (m.perms != nil || bridge != nil) {
		id, idErr := qwenMCPID(m.workingDir)
		if idErr != nil {
			mcpSetupErr = idErr
			slog.Warn("cliprovider: failed to get qwen MCP ID", "err", idErr)
		} else {
			var err error
			mcpSrv, err = newRushMCPServer(ctx, m.perms, m.sessions, sessionID, m.workingDir, "", m.mcpProxy)
			if err != nil {
				mcpSetupErr = err
				slog.Warn("cliprovider: failed to start qwen MCP server", "err", err)
			} else {
				// registerQwenMCP below unconditionally replaces
				// mcpServers[id] with a fresh map, so a leading
				// deregister-then-register here was always redundant
				// (removed: it also used to unsafely delete a possibly
				// concurrent session's live entry — see
				// deregisterQwenMCP's own doc for the full story).
				serverName := sessionMCPName(id, sessionID)
				if regErr := registerQwenMCP(serverName, mcpSrv.addr, mcpSrv.token); regErr != nil {
					mcpSetupErr = regErr
					slog.Warn("cliprovider: failed to register qwen MCP server", "err", regErr)
					mcpSrv.stop()
					mcpSrv = nil
				} else {
					qwenMCPName = serverName
					args = append(args, "--allowed-mcp-server-names", qwenMCPName)
					// Restrict qwen to only rush MCP tools so its built-in
					// tools (read_file, glob, etc.) cannot bypass rush's
					// permission system.
					// Also block the native todo_write so the model uses
					// mcp__rush__todos which persists tasks to the rush session.
					if bridge != nil {
						args = append(args, "--allowed-tools")
						for _, tool := range bridge.Tools() {
							args = append(args, "mcp__"+qwenMCPName+"__"+tool.WireName)
						}
						args = append(args, "--exclude-tools", "todo_write")
					} else {
						args = append(
							args,
							"--allowed-tools",
							"mcp__"+qwenMCPName+"__Bash",
							"mcp__"+qwenMCPName+"__Read",
							"mcp__"+qwenMCPName+"__Write",
							"mcp__"+qwenMCPName+"__Glob",
							"mcp__"+qwenMCPName+"__Grep",
							"mcp__"+qwenMCPName+"__todos",
							"--exclude-tools",
							"todo_write",
						)
					}
				}
			}
		}
	}

	// Gemini MCP integration: register rush's MCP server in ~/.gemini/settings.json
	// using a stable per-project ID, suffixed with a session-specific hash so
	// concurrent rush sessions retain independent registrations.
	// Authorization: Bearer headers and trust:true to bypass its own confirmation
	// prompts, so tool calls go directly to our MCP server and Rush permission UI.
	if m.spec.GeminiMCPIntegration && (m.perms != nil || bridge != nil) {
		id, idErr := geminiMCPID(m.workingDir)
		if idErr != nil {
			mcpSetupErr = idErr
			slog.Warn("cliprovider: failed to get gemini MCP ID", "err", idErr)
		} else {
			var err error
			mcpSrv, err = newRushMCPServer(ctx, m.perms, m.sessions, sessionID, m.workingDir, "", m.mcpProxy)
			if err != nil {
				mcpSetupErr = err
				slog.Warn("cliprovider: failed to start gemini MCP server", "err", err)
			} else {
				// registerGeminiMCP below unconditionally replaces
				// mcpServers[id] with a fresh map, so a leading
				// deregister-then-register here was always redundant
				// (removed: it also used to unsafely delete a possibly
				// concurrent session's live entry — see
				// deregisterGeminiMCP's own doc for the full story).
				serverName := sessionMCPName(id, sessionID)
				if regErr := registerGeminiMCP(serverName, mcpSrv.addr, mcpSrv.token); regErr != nil {
					mcpSetupErr = regErr
					slog.Warn("cliprovider: failed to register gemini MCP server", "err", regErr)
					mcpSrv.stop()
					mcpSrv = nil
				} else {
					geminiMCPName = serverName
					args = append(args, "--allowed-mcp-server-names", geminiMCPName)
					slog.Info("cliprovider: gemini MCP registered", "name", geminiMCPName, "addr", mcpSrv.addr)
				}
			}
		}
	}

	// Codex MCP integration: pass rush's MCP server URL to codex via -c flag
	// (inline config override). No persistent changes to ~/.codex/config.toml.
	// The token is passed via environment variable and Authorization: Bearer header
	// to avoid leaking secrets in process lists (query params are visible in /proc/<pid>/cmdline).
	if m.spec.CodexMCPIntegration && (m.perms != nil || bridge != nil) {
		var err error
		mcpSrv, err = newRushMCPServer(ctx, m.perms, m.sessions, sessionID, m.workingDir, "", m.mcpProxy)
		if err != nil {
			mcpSetupErr = err
			slog.Warn("cliprovider: failed to start codex MCP server", "err", err)
		} else {
			args = append(args, codexMCPConfigArgs(mcpSrv.addr)...)
			slog.Info("cliprovider: codex MCP configured", "addr", mcpSrv.addr)
		}
	}

	if mcpSrv == nil && (bridge != nil || (m.perms != nil && (m.spec.UseRushMCP || m.spec.QwenMCPIntegration || m.spec.GeminiMCPIntegration || m.spec.CodexMCPIntegration))) {
		if attachTmpDir != "" {
			_ = os.RemoveAll(attachTmpDir)
		}
		if bridge != nil {
			return nil, fmt.Errorf("cliprovider: configured tool bridge is unavailable for %s", m.spec.ModelID)
		}
		if mcpSetupErr != nil {
			return nil, fmt.Errorf("cliprovider: configured permission MCP server is unavailable for %s: %w", m.spec.ModelID, mcpSetupErr)
		}
		return nil, fmt.Errorf("cliprovider: configured permission MCP server is unavailable for %s", m.spec.ModelID)
	}

	noPTY := m.spec.NoPTY || testDisablePTY
	useStdin := usePromptStdin(m.spec, noPTY, prompt)
	if m.spec.PromptFlag != "" && !useStdin {
		args = append(args, m.spec.PromptFlag, prompt)
	}
	if useStdin && m.spec.StdinPrintFlag != "" {
		args = append(args, m.spec.StdinPrintFlag)
	}

	var proc procHandle

	// childPid is the direct CLI child's pid (0 when no start
	// succeeded). It feeds the stream closure's deferred
	// UntrackProcessTree, which releases Windows tree-teardown state
	// on every exit path of the stream.
	var childPid int

	if m.streamProcess != nil {
		proc = m.streamProcess(noPTY, args)
	}
	if proc.stdout == nil && !useStdin && !noPTY {
		// Use a PTY so the subprocess (e.g. Node.js claude CLI) sees a TTY on
		// stdout and does not buffer output internally. go-pty supports both
		// Unix PTY and Windows ConPTY transparently.
		//
		// On Windows, ClosePseudoConsole (called by p.Close) is what signals
		// EOF on the output pipe — the process exiting alone does not do it.
		// We therefore run Wait in a goroutine and close the PTY afterwards,
		// which guarantees the scanner always sees EOF on both platforms.
		p, ptyErr := gopty.New()
		if ptyErr == nil {
			// Resize to a very wide terminal to prevent the PTY from hard-wrapping
			// long JSON lines. Claude CLI emits lines that can be many KB; wrapping
			// at the default 80-column width splits them across scanner tokens,
			// causing json.Unmarshal to fail on every partial line.
			_ = p.Resize(8192, 50)
			// The launch path was classified before any request resources were
			// allocated. On Windows, go-pty/ConPTY may resolve bare names relative
			// to cmd.Dir instead of PATH, and falling back from a WSL-only resolver
			// rejection would launch the unsafe WSL executable.
			// ptyProc publishes the child's *os.Process for the same reason:
			// go-pty assigns ptycmd.Process only after its inner
			// exec.Cmd.Start returns (cmd_unix.go:51), while the ctx watcher
			// that calls Cancel is already running — reading
			// ptycmd.Process directly was a formal data race (review P3-2).
			var ptyProc atomic.Pointer[os.Process]
			ptycmd := p.CommandContext(ctx, binaryPath, args...)
			ptycmd.Dir = m.workingDir
			if runtime.GOOS != "windows" {
				// Cancellation kills the full group (Setpgid pipe / Setsid
				// PTY), even if the leader exits first. A nil load (ctx
				// cancelled inside the start window) returns
				// os.ErrProcessDone — the same benign no-op the old
				// nil-Process read produced, without the race.
				// killChildProcess keeps the per-platform dispatch (unix:
				// group kill via killChildProcessPid; Windows:
				// session.KillProcess — the closure is never installed on
				// Windows anyway). The stream's own ctx watchers
				// (iterStarted/killDone → legacyKill) still group-kill once
				// Start returns.
				ptycmd.Cancel = func() error {
					if proc := ptyProc.Load(); proc != nil {
						return killChildProcess(proc)
					}
					return os.ErrProcessDone
				}
			}
			if startErr := ptycmd.Start(); startErr == nil {
				ptyProc.Store(ptycmd.Process)
				// Track the child's tree for teardown. On Windows this
				// assigns a KILL_ON_JOB_CLOSE Job Object that
				// KillProcess terminates (go-pty's ConPTY spawn
				// hand-rolls CreateProcess, so post-Start tracking is
				// the only hook we get). On Unix, go-pty's start()
				// force-sets SysProcAttr{Setsid: true, Setctty: true},
				// so the child is its own session and process-group
				// leader (pgid == pid) by construction and the
				// registration below needs no getpgid probe on the
				// possibly-already-exited leader.
				childPid = trackChildTree(ptycmd.Process, m.dataDir, sessionID)
				// Log command-line diagnostics. In production mode, args are sanitized
				// to remove sensitive values (prompts, tokens). In diagnostic mode
				// (RUSH_CLIPROVIDER_LOG_RAW_PROMPT=1), the full args are logged.
				// The promptHead/promptTail fields are only included in diagnostic mode.
				argsToLog := strings.Join(sanitizeArgs(args), " ")
				if logRawPromptEnabled() {
					slog.Info(
						"cliprovider: using PTY",
						"binary", binaryPath,
						"args", argsToLog,
						"argsCount", len(args),
						"argsByteLen", argsByteLen(args),
						"promptLen", len(prompt),
						"promptHead", clipString(prompt, 200),
						"promptTail", tailString(prompt, 200),
					)
				} else {
					slog.Info(
						"cliprovider: using PTY",
						"binary", binaryPath,
						"args", argsToLog,
						"argsCount", len(args),
						"argsByteLen", argsByteLen(args),
						"promptLen", len(prompt),
					)
				}
				// go-pty's unixPty.Start() (cmd_unix.go) sets cmd.Stdin/Stdout/
				// Stderr = pty.slave directly — our own process keeps its own
				// open handle to the slave end in addition to the child's
				// inherited copy. Unless we close OUR copy, the master read
				// NEVER sees EOF, even after the child (and any grandchildren)
				// exit and close theirs: as long as any open slave fd exists
				// anywhere, the kernel keeps the master side alive. This is not
				// a corner case — it is unconditional on this library, and
				// without this close the scanner blocks forever on every normal
				// (non-cancelled) exit (confirmed by a CI hang: TestStreamExitError
				// timed out after 10m with the scanner parked in Read()).
				//
				// Closing our slave copy immediately after Start() is the
				// standard Unix PTY pattern: it lets the master reach genuine
				// EOF once every *other* holder (the direct child, and any
				// legitimate grandchild that inherited the tty) has also
				// closed its copy — which is exactly the natural-EOF signal
				// the scanner needs, and still respects a grandchild
				// legitimately holding the tty open (mirrors the pipe branch's
				// stderr-holding-grandchild protection). Windows ConPTY has no
				// separate slave fd (UnixPty type assertion fails there), so
				// this is a no-op on Windows and the existing eager-close
				// goroutine below remains the correct mechanism there.
				if up, ok := p.(gopty.UnixPty); ok {
					_ = up.Slave().Close()
				}

				// ptycmd.Wait() runs eagerly in a background goroutine for zombie
				// reaping. p.Close() (closing the master too) is deliberately
				// deferred to wait() — NOT called eagerly here — so a
				// fast-exiting child's already-buffered output can still be
				// drained via the natural EOF above before the master itself
				// is torn down. On Windows ConPTY, the child exit does NOT
				// deliver EOF on the master read pipe at all, so a separate
				// goroutine closes the PTY after the process exits to unblock
				// the scanner.
				var ptyWaitErr error
				ptyWaitDone := make(chan struct{})
				go func() {
					ptyWaitErr = ptycmd.Wait()
					close(ptyWaitDone)
				}()

				var closeOnce sync.Once
				closePTY := func() { closeOnce.Do(func() { _ = p.Close() }) }

				if runtime.GOOS == "windows" {
					// ConPTY: child exit alone does not signal EOF on the master
					// read pipe. Close the PTY as soon as the process exits so
					// the scanner sees EOF. (Not exercised by the test suite —
					// testDisablePTY=true on Windows forces pipe mode.)
					go func() {
						select {
						case <-ptyWaitDone:
						case <-ctx.Done():
						}
						closePTY()
					}()
				}

				var ptyKillOnce sync.Once
				proc = procHandle{
					stdout:   p,
					usingPTY: true,
					kill: func() {
						// Use sync.Once so this is safe to call from multiple goroutines
						// (context-cancel watcher + scanner loop) without double-killing.
						ptyKillOnce.Do(func() {
							if ptycmd.Process != nil {
								_ = killChildProcess(ptycmd.Process)
							}
						})
					},
					wait:    func() (string, error) { <-ptyWaitDone; return "", ptyWaitErr },
					closeIO: closePTY,
					cleanup: closePTY,
				}
			} else {
				_ = p.Close()
				slog.Info("cliprovider: PTY start failed, falling back to pipe", "err", startErr)
			}
		} else {
			slog.Info("cliprovider: PTY unavailable, falling back to pipe", "err", ptyErr)
		}
	}

	if proc.stdout == nil {
		// Pipe fallback: large prompt (stdin required) or PTY unavailable.
		// binaryPath was resolved before any request resources were allocated.
		// A genuinely missing binary remains bare so Start preserves the legacy
		// error surface; every other resolver rejection has already returned.
		cmd := platform.Command(ctx, binaryPath, args...)
		cmd.Dir = m.workingDir
		// Make the child a process-group leader so KillProcess can
		// kill its whole tree (grandchildren included) with one
		// killpg. No-op on Windows, where tree teardown is the Job
		// Object assigned by trackChildTree below.
		configureChildProcessGroup(cmd)
		if useStdin {
			cmd.Stdin = strings.NewReader(prompt)
		}

		// For Codex MCP integration, set the token env var referenced by
		// codexMCPConfigArgs's bearer_token_env_var override — see
		// codexMCPTokenEnvVar's doc for why this stays out of argv.
		if m.spec.CodexMCPIntegration && mcpSrv != nil {
			cmd.Env = append(os.Environ(), codexMCPTokenEnvVar+"="+mcpSrv.token)
		}
		// Log command-line diagnostics. In production mode, args are sanitized
		// to remove sensitive values (prompts, tokens). In diagnostic mode
		// (RUSH_CLIPROVIDER_LOG_RAW_PROMPT=1), the full args are logged.
		// The promptHead/promptTail fields are only included in diagnostic mode.
		argsToLog := strings.Join(sanitizeArgs(args), " ")
		if logRawPromptEnabled() {
			slog.Info(
				"cliprovider: launching pipe mode",
				"binary", m.spec.Binary,
				"args", argsToLog,
				"argsCount", len(args),
				"argsByteLen", argsByteLen(args),
				"useStdin", useStdin,
				"promptLen", len(prompt),
				"promptHead", clipString(prompt, 200),
				"promptTail", tailString(prompt, 200),
				"noPTY", m.spec.NoPTY,
			)
		} else {
			slog.Info(
				"cliprovider: launching pipe mode",
				"binary", m.spec.Binary,
				"args", argsToLog,
				"argsCount", len(args),
				"argsByteLen", argsByteLen(args),
				"useStdin", useStdin,
				"promptLen", len(prompt),
				"noPTY", m.spec.NoPTY,
			)
		}

		// For NoPTY models (e.g. npx wrappers), merge stdout and stderr
		// into a single reader via io.Pipe + concurrent copy goroutines.
		// Claude CLI may send JSON to stderr when the prompt is delivered
		// via stdin instead of -p flag. We use cmd.StdoutPipe/StderrPipe
		// (not os.Pipe) because Go's exec package handles Windows handle
		// inheritance correctly for those.
		var reader io.Reader
		var stderrBuf bytes.Buffer
		var drained func()
		if m.spec.NoPTY {
			stdoutPipe, pipeErr := cmd.StdoutPipe()
			if pipeErr != nil {
				return nil, fmt.Errorf("stdout pipe: %w", pipeErr)
			}
			stderrPipe, pipeErr := cmd.StderrPipe()
			if pipeErr != nil {
				return nil, fmt.Errorf("stderr pipe: %w", pipeErr)
			}
			if startErr := cmd.Start(); startErr != nil {
				return nil, fmt.Errorf("start %s: %w", m.spec.Binary, startErr)
			}
			pr, pw := io.Pipe()
			drained = mergeLines(pw, stdoutPipe, stderrPipe)
			go func() { drained(); pw.Close() }()
			reader = pr
		} else {
			cmd.Stderr = &stderrBuf
			pipe, pipeErr := cmd.StdoutPipe()
			if pipeErr != nil {
				return nil, fmt.Errorf("stdout pipe: %w", pipeErr)
			}
			if startErr := cmd.Start(); startErr != nil {
				return nil, fmt.Errorf("start %s: %w", m.spec.Binary, startErr)
			}
			reader = pipe
		}
		childPid = trackChildTree(cmd.Process, m.dataDir, sessionID)
		slog.Info("cliprovider: process started", "binary", m.spec.Binary, "pid", cmd.Process.Pid)
		proc = procHandle{stdout: reader, drained: drained, kill: func() {
			_ = killChildProcess(cmd.Process)
		}, wait: func() (string, error) { err := cmd.Wait(); return strings.TrimSpace(stderrBuf.String()), err }, stderrRaw: func() []byte { return stderrBuf.Bytes() }}
	}

	var parsePart func([]byte) (fantasy.StreamPart, bool)
	if m.spec.NewPartParser != nil {
		parsePart = m.spec.NewPartParser()
	}

	retained := newStreamSession(proc)
	var legacyWaitOnce sync.Once
	var legacyStderr string
	var legacyErr error
	waitCaller := func() (string, error) {
		result, err := retained.Wait(ctx)
		if proc.closeIO != nil {
			proc.closeIO()
		}
		if err != nil {
			return "", err
		}
		return result.Stderr, result.Err
	}
	legacyWait := func() (string, error) {
		if proc.usingPTY {
			return waitCaller()
		}
		legacyWaitOnce.Do(func() { legacyStderr, legacyErr = waitCaller() })
		return legacyStderr, legacyErr
	}

	var visibleOnce sync.Once
	visibleCleanup := func() {
		visibleOnce.Do(func() {
			session.UntrackProcessTree(childPid)
			session.UnregisterChildGroup(m.dataDir, sessionID, childPid)
			if geminiMCPName != "" {
				deregisterGeminiMCP(geminiMCPName, mcpSrv.addr)
			}
			if qwenMCPName != "" {
				deregisterQwenMCP(qwenMCPName, mcpSrv.addr)
			}
			if attachTmpDir != "" {
				_ = os.RemoveAll(attachTmpDir)
			}
			if mcpTmpCfg != "" {
				_ = os.Remove(mcpTmpCfg)
			}
			if mcpSrv != nil {
				mcpSrv.stop()
			}
			if m.streamVisibleCleanup != nil {
				m.streamVisibleCleanup()
			}
		})
	}
	iterStarted := make(chan struct{})
	var iterStartedOnce sync.Once
	go func() {
		select {
		case <-iterStarted:
		case <-ctx.Done():
			retained.legacyKill()
		}
	}()

	retained.stream = func(yield func(fantasy.StreamPart) bool) {
		iterStartedOnce.Do(func() { close(iterStarted) })
		defer visibleCleanup()
		defer func() { _, _ = legacyWait() }()
		killDone := make(chan struct{})
		defer close(killDone)
		go func() {
			select {
			case <-ctx.Done():
				retained.legacyKill()
			case <-killDone:
			}
		}()

		const textID = "0"
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: textID}) {
			retained.legacyKill()
			return
		}

		// toolCh is the read side of the MCP tool-event channel.
		// When nil (no MCP server), selecting on it never fires.
		var toolCh <-chan mcpToolEvent
		if mcpSrv != nil {
			toolCh = mcpSrv.toolCh
		}

		var finalUsage fantasy.Usage
		var diagnostics exitDiagnostics
		var failure cliFailureEvidence
		failureName := cliFailureName(m.spec)
		scanDone := false
		var scanErr error
		var linesSeen int
		for !scanDone {
			select {
			case <-ctx.Done():
				retained.legacyKill()
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()}) //nolint:errcheck
				return

			case ev := <-toolCh:
				// Emit ToolInputStart + Delta + End from the MCP tool event.
				id := ev.id
				if ev.name != "" {
					// start event
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: id, ToolCallName: ev.name}) {
						retained.legacyKill()
						return
					}
					if ev.input != "" {
						if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: id, Delta: ev.input}) {
							retained.legacyKill()
							return
						}
					}
				} else {
					// end event
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: id}) {
						retained.legacyKill()
						return
					}
				}

			case <-retained.ready(linesSeen):
				res, ok := retained.next(ctx, linesSeen)
				if !ok {
					continue
				}
				if res.done {
					scanDone = true
					scanErr = res.err
					break
				}
				linesSeen++

				// Strip ANSI/VT sequences that PTY drivers (especially Windows ConPTY)
				// inject into the output stream. JSON parsers need clean bytes.
				raw := res.raw
				// REVERT CHECK: remove this opt-in branch and raw CLI payloads
				// are written to debug logs even when raw-prompt logging is off.
				if logRawPromptEnabled() {
					slog.Debug("cliprovider: raw line", "raw", string(raw))
				} else {
					slog.Debug("cliprovider: CLI event", cliEventLogFields(raw, false)...)
				}
				line := bytes.TrimSpace(ansiEscape.ReplaceAll(raw, nil))
				diagnostics.add(line)
				failure.add(failureName, line)

				// Capture CLI session ID from the system init event for --resume.
				if m.spec.SupportsResume && cliSessionKey != "" && ctx.Value(TitleGenerationContextKey) != true {
					var initEv streamEvent
					if json.Unmarshal(line, &initEv) == nil && initEv.Type == "system" && initEv.Subtype == "init" && initEv.SessionID != "" {
						m.cliSessions.Set(cliSessionKey, cliSessionEntry{CLISessionID: initEv.SessionID, PrefixHash: prefixHash})
						slog.Info("cliprovider: captured CLI session ID", "key", cliSessionKey, "cliSession", initEv.SessionID)
					}
				}

				if m.spec.ParseUsageLine != nil {
					if u, ok := m.spec.ParseUsageLine(line); ok {
						finalUsage = u
					}
				}

				var part fantasy.StreamPart
				if parsePart != nil {
					var ok bool
					part, ok = parsePart(line)
					if !ok {
						continue
					}
				} else {
					clean := strings.TrimSpace(string(line))
					if clean == "" {
						continue
					}
					part = fantasy.StreamPart{
						Type:  fantasy.StreamPartTypeTextDelta,
						ID:    textID,
						Delta: clean + "\n",
					}
				}

				if !yield(part) {
					retained.legacyKill()
					return
				}
			}
		}

		// os.ErrClosed ("read |0: file already closed") is a benign
		// end-of-stream signal, not a real I/O failure. cmd.Wait() is no
		// longer started eagerly (it now runs lazily from legacyWait(), only
		// after the scanner itself reaches scanDone), so this can no longer be
		// caused by a concurrent Wait() racing the scanner's read. It is kept
		// as a defensive, platform-agnostic classification: some OS/exec
		// implementations can still surface a "file already closed" read
		// error as part of normal teardown once the child has exited. Treat
		// it like EOF and fall through to the normal legacyWait() path, which
		// still surfaces a genuine error if the child exited non-zero.
		if scanErr != nil && !errors.Is(scanErr, io.EOF) && !errors.Is(scanErr, os.ErrClosed) {
			// PTY master returns EIO (Unix) or similar when child exits.
			// Treat any scanner error in PTY mode as normal end-of-stream.
			if !proc.usingPTY {
				_, _ = legacyWait()
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: scanErr}) //nolint:errcheck
				return
			}
		}

		stderr, waitErr := legacyWait()
		// Fork patch (operator UX): also log stderr content (clipped) and
		// total lines seen on stdout. The original log said "err=null
		// stderrLen=0" for the silent-claude-exit bug which gave the
		// operator nothing actionable. Now they see if anything was
		// emitted at all and what the stderr tail looked like.
		slog.Info(
			"cliprovider: process finished",
			"binary", m.spec.Binary,
			"err", waitErr,
			"stderrLen", len(stderr),
			"stderrTail", tailString(stderr, 500),
			"linesSeen", linesSeen,
		)
		// If resume failed, clear the stale CLI session mapping so next call starts fresh.
		if (waitErr != nil || failure.seen) && resuming && cliSessionKey != "" {
			m.cliSessions.Del(cliSessionKey)
			slog.Warn("cliprovider: resume failed, cleared CLI session mapping", "key", cliSessionKey)
		}
		if waitErr != nil || failure.seen {
			var exitErr error
			if waitErr == nil {
				exitErr = fmt.Errorf("%s failed: CLI failure event", m.spec.Binary)
			} else if stderr != "" {
				exitErr = fmt.Errorf("%s failed: %w\nstderr: %s", m.spec.Binary, waitErr, stderr)
			} else if merged := strings.TrimSpace(diagnostics.text); merged != "" {
				// Merged (NoPTY) mode has no separate stderr; the bounded,
				// sanitized non-JSON output is the only explanation of the exit.
				exitErr = fmt.Errorf("%s failed: %w\noutput: %s", m.spec.Binary, waitErr, tailString(merged, 1000))
			} else {
				exitErr = fmt.Errorf("%s failed: %w", m.spec.Binary, waitErr)
			}
			if failure.text != "" {
				exitErr = fmt.Errorf("%w\nfailure: %s", exitErr, failure.text)
			}
			exitErr = mapCLIExitError(exitErr, stderr, diagnostics, failure)
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: exitErr}) //nolint:errcheck
			return
		}

		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: textID}) //nolint:errcheck
		yield(fantasy.StreamPart{                                                  //nolint:errcheck
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        finalUsage,
		})
	}
	return retained, nil
}
