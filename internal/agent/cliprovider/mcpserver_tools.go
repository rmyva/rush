package cliprovider

// Tool implementations registered on the in-process rush MCP server:
// the external MCP proxy bridge, bash, read, write, glob, grep and todos,
// plus the shared helpers (result builders, path resolution, shell
// execution, line slicing) those tools use.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent/agentguard"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerExternalMCPTools exposes all enabled external MCP tools (from the
// internal mcp package) on the rush MCP HTTP server, so CLI models can call
// them. Tool names are prefixed with the server name to avoid collisions.
// Each tool call goes through perms.Request so the user can approve/deny it
// in the rush UI (or auto-approve in yolo mode).
func registerExternalMCPTools(ctx context.Context, srv *mcp.Server, perms permission.Service, sessionID string, workingDir string, proxy ExternalMCPProxy, toolCh chan mcpToolEvent) {
	for _, ext := range proxy.ListTools() {
		ext := ext // capture
		toolName := ext.ServerName + "__" + ext.Name

		// Build the InputSchema as json.RawMessage from the external tool's schema.
		var rawSchema json.RawMessage
		if ext.InputSchema != nil {
			if b, err := json.Marshal(ext.InputSchema); err == nil {
				rawSchema = b
			}
		}
		if rawSchema == nil {
			rawSchema = json.RawMessage(`{"type":"object"}`)
		}

		srv.AddTool(&mcp.Tool{
			Name:        toolName,
			Description: ext.Description,
			InputSchema: rawSchema,
		}, func(reqCtx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id := uuid.New().String()
			inputJSON := string(req.Params.Arguments)

			emitToolStart(toolCh, id, toolName, inputJSON)
			defer emitToolEnd(toolCh, id)

			// Request permission via rush UI (respects yolo mode).
			if perms != nil {
				var params any
				_ = json.Unmarshal(req.Params.Arguments, &params)
				granted, err := perms.Request(reqCtx, permission.CreatePermissionRequest{
					SessionID:   sessionID,
					ToolCallID:  id,
					ToolName:    "mcp_" + toolName,
					Description: fmt.Sprintf("call %s on MCP server %s", ext.Name, ext.ServerName),
					Action:      "execute",
					Params:      params,
					Path:        workingDir,
				})
				if err != nil {
					return &mcp.CallToolResult{
						Content: []mcp.Content{&mcp.TextContent{Text: "permission request failed: " + err.Error()}},
						IsError: true,
					}, nil
				}
				if !granted {
					return &mcp.CallToolResult{
						Content: []mcp.Content{&mcp.TextContent{Text: "tool call denied by user"}},
						IsError: true,
					}, nil
				}
			}

			result, err := proxy.CallTool(reqCtx, ext.ServerName, ext.Name, inputJSON)
			if err != nil {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "error: " + err.Error()}},
					IsError: true,
				}, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: result}},
			}, nil
		})

		slog.Info("cliprovider: registered external MCP tool", "tool", toolName, "server", ext.ServerName)
	}
}

// ── bash ─────────────────────────────────────────────────────────────────────

type mcpBashInput struct {
	Command     string `json:"command"     description:"Shell command to execute"`
	Description string `json:"description" description:"Brief description of what the command does"`
	WorkingDir  string `json:"working_dir,omitempty" description:"Working directory (defaults to project root)"`
}

// RunAllowlistCommand lets the restricted-run gate apply allow_bash
// pattern scrutiny to this tool's command string exactly as it does for
// the built-in bash tool (see internal/permission/runallowlist.go).
func (p mcpBashInput) RunAllowlistCommand() string { return p.Command }

func registerBashTool(srv *mcp.Server, perms permission.Service, sessionID string, workingDir string, toolCh chan mcpToolEvent) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "Bash",
		Description: "Execute a shell command. Requires user approval.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpBashInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP Bash called", "command", input.Command, "description", input.Description)

		wd := workingDir
		if input.WorkingDir != "" {
			wd = input.WorkingDir
		}

		// Fork patch: batch 16 — refuse invocations of other AI agent CLIs
		// (claude, codex, gemini, opencode, aider, rush itself, …) before
		// they reach the shell, and — on Windows — commands that would pop
		// a brand-new, visible console/GUI window via start / Start-Process
		// / Start-Job, regardless of the outer shell's own HideWindow
		// attribute. A model running unattended via `rush run` has no
		// legitimate reason to open a window on the operator's desktop, and
		// every such window steals focus and covers whatever the operator
		// was doing. Both guards now live behind agentguard.CheckAll, the
		// single entry point shared with the built-in Bash tool so the two
		// surfaces cannot diverge again. See internal/agent/agentguard.
		if policyErr := agentguard.CheckCommandPolicy(input.Command); policyErr != nil {
			return toolError(policyErr.Error()), nil, nil
		}
		if guardErr := agentguard.CheckAll(input.Command); guardErr != nil {
			return toolError(guardErr.Error()), nil, nil
		}
		if shell.CommandBlocked(input.Command) {
			return toolError("command is not allowed for security reasons"), nil, nil
		}

		// A17: in runs rooted in a linked git worktree the orchestrator
		// commits — refuse state-mutating git from this shell surface too,
		// keyed on the server's trusted working directory.
		if agentguard.IsLinkedWorktree(workingDir) {
			if gitErr := agentguard.CheckGitWrites(input.Command); gitErr != nil {
				return toolError(gitErr.Error()), nil, nil
			}
		}

		id := uuid.New().String()
		inputJSON, _ := json.Marshal(input)
		emitToolStart(toolCh, id, "Bash", string(inputJSON))
		defer emitToolEnd(toolCh, id)

		granted, err := perms.Request(ctx, permission.CreatePermissionRequest{
			SessionID:   sessionID,
			ToolCallID:  id,
			ToolName:    "bash",
			Description: input.Description,
			Action:      "run",
			Params:      input,
			Path:        wd,
		})
		if err != nil {
			slog.Debug("cliprovider: MCP Bash permission error", "err", err)
			return toolError("permission request failed: " + err.Error()), nil, nil
		}
		if !granted {
			slog.Debug("cliprovider: MCP Bash denied by user")
			return toolError("command denied by user"), nil, nil
		}

		if testMCPBashSpawn != nil {
			out, spawnErr := testMCPBashSpawn(ctx, "Bash", input.Command)
			if spawnErr != nil {
				return toolError(spawnErr.Error()), nil, nil
			}
			return toolText(out), nil, nil
		}
		out, runErr := runShell(ctx, input.Command, wd)
		slog.Debug("cliprovider: MCP Bash executed", "command", input.Command, "output_len", len(out), "err", runErr)
		if runErr != nil {
			return toolError(fmt.Sprintf("command failed: %v\n%s", runErr, out)), nil, nil
		}
		return toolText(out), nil, nil
	})
}

// ── view / read ───────────────────────────────────────────────────────────────

type mcpViewInput struct {
	Path      string `json:"path"                description:"File path to read"`
	StartLine int    `json:"start_line,omitempty" description:"First line to read (1-based, 0 = beginning)"`
	EndLine   int    `json:"end_line,omitempty"   description:"Last line to read (0 = end of file)"`
}

func registerViewTool(srv *mcp.Server, perms permission.Service, sessionID string, workingDir string, toolCh chan mcpToolEvent) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "Read",
		Description: "Read the contents of a file.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpViewInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP Read called", "path", input.Path)

		id := uuid.New().String()
		inputJSON, _ := json.Marshal(input)
		emitToolStart(toolCh, id, "Read", string(inputJSON))
		defer emitToolEnd(toolCh, id)

		path := resolvePath(input.Path, workingDir)
		granted, err := perms.Request(ctx, permission.CreatePermissionRequest{
			SessionID:   sessionID,
			ToolCallID:  id,
			ToolName:    "view",
			Description: "Read file: " + input.Path,
			Action:      "read",
			Params:      input,
			Path:        path,
		})
		if err != nil || !granted {
			return toolError("read denied"), nil, nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			slog.Debug("cliprovider: MCP Read error", "path", path, "err", err)
			return toolError(err.Error()), nil, nil
		}

		content := string(data)
		// Fork patch: batch 15 — when a sub-agent (called via this MCP)
		// reads CLAUDE.md it gets the OPERATOR-facing delegation guidance
		// inserted by `rush claude-init`. That guidance tells the reader
		// to "delegate work to rush sub-agents" — which causes a
		// sub-agent that just read this to spawn a NEW rush sub-agent,
		// recursing until timeout. Strip the block before returning so
		// the sub-agent never sees the instruction it would loop on.
		// Filesystem file is untouched; only THIS read sees the filtered
		// content. Operator reading via shell or external tools still
		// sees the original.
		if isClaudeMdPath(path) {
			content = stripRushClaudeInitBlock(content)
		}
		if input.StartLine > 0 || input.EndLine > 0 {
			content = sliceLines(content, input.StartLine, input.EndLine)
		}
		slog.Debug("cliprovider: MCP Read ok", "path", path, "bytes", len(data))
		return toolText(content), nil, nil
	})
}

// rushClaudeInitBlockPattern is the same regex `internal/cmd/claude_init.go`
// uses to identify our injected block. Duplicated here to avoid a cmd→
// cliprovider import (cmd already imports a lot from the agent layer).
// If the marker scheme ever changes, update both sites.
var rushClaudeInitBlockPattern = regexp.MustCompile(`(?s)<!-- crush-claude-init:v\d+ -->.*?<!-- /crush-claude-init -->\s*`)

func isClaudeMdPath(path string) bool {
	// Split on BOTH separators regardless of host OS: the path may be a
	// Windows path (…\CLAUDE.md) even when rush runs on Linux/macOS, where
	// filepath.Base only understands "/".
	base := path
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	// Case-insensitive — Windows users sometimes write "Claude.md".
	return strings.EqualFold(base, "CLAUDE.md")
}

func stripRushClaudeInitBlock(content string) string {
	return rushClaudeInitBlockPattern.ReplaceAllString(content, "")
}

// ── write ─────────────────────────────────────────────────────────────────────

type mcpWriteInput struct {
	Path    string `json:"path"    description:"File path to write"`
	Content string `json:"content" description:"Content to write to the file"`
}

func registerWriteTool(srv *mcp.Server, perms permission.Service, sessionID string, workingDir string, toolCh chan mcpToolEvent) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "Write",
		Description: "Write content to a file, creating or overwriting it.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpWriteInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP Write called", "path", input.Path, "bytes", len(input.Content))

		// Emit start event with path only (omit large content from stream part).
		id := uuid.New().String()
		inputJSON, _ := json.Marshal(struct {
			Path string `json:"path"`
		}{Path: input.Path})
		emitToolStart(toolCh, id, "Write", string(inputJSON))
		defer emitToolEnd(toolCh, id)

		path := resolvePath(input.Path, workingDir)
		granted, err := perms.Request(ctx, permission.CreatePermissionRequest{
			SessionID:   sessionID,
			ToolCallID:  id,
			ToolName:    "write",
			Description: "Write file: " + input.Path,
			Action:      "write",
			Params:      input,
			Path:        path,
		})
		if err != nil || !granted {
			return toolError("write denied"), nil, nil
		}

		if err := os.WriteFile(path, []byte(input.Content), 0o644); err != nil {
			slog.Debug("cliprovider: MCP Write error", "path", path, "err", err)
			return toolError(err.Error()), nil, nil
		}
		slog.Debug("cliprovider: MCP Write ok", "path", path)
		return toolText("file written"), nil, nil
	})
}

// ── glob ──────────────────────────────────────────────────────────────────────

type mcpGlobInput struct {
	Pattern string `json:"pattern" description:"Glob pattern (e.g. **/*.go)"`
	Path    string `json:"path,omitempty" description:"Directory to search in"`
}

func registerGlobTool(srv *mcp.Server, perms permission.Service, sessionID string, workingDir string, toolCh chan mcpToolEvent) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "Glob",
		Description: "Find files matching a glob pattern.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpGlobInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP Glob called", "pattern", input.Pattern, "path", input.Path)

		id := uuid.New().String()
		inputJSON, _ := json.Marshal(input)
		emitToolStart(toolCh, id, "Glob", string(inputJSON))
		defer emitToolEnd(toolCh, id)

		dir := workingDir
		if input.Path != "" {
			dir = resolvePath(input.Path, workingDir)
		}

		granted, err := perms.Request(ctx, permission.CreatePermissionRequest{
			SessionID:   sessionID,
			ToolCallID:  id,
			ToolName:    "glob",
			Description: "Find files: " + input.Pattern,
			Action:      "read",
			Params:      input,
			Path:        dir,
		})
		if err != nil || !granted {
			return toolError("glob denied"), nil, nil
		}

		// Use doublestar.Glob for safe, shell-free glob matching with ** support.
		fsys := os.DirFS(dir)
		matches, globErr := doublestar.Glob(fsys, input.Pattern)
		if globErr != nil {
			return toolError("glob error: " + globErr.Error()), nil, nil
		}
		const maxGlobResults = 200
		if len(matches) > maxGlobResults {
			matches = matches[:maxGlobResults]
		}
		slog.Debug("cliprovider: MCP Glob ok", "matches", len(matches))
		return toolText(strings.Join(matches, "\n")), nil, nil
	})
}

// ── grep ──────────────────────────────────────────────────────────────────────

type mcpGrepInput struct {
	Pattern string `json:"pattern" description:"Regular expression to search for"`
	Path    string `json:"path,omitempty" description:"Directory or file to search in"`
	Glob    string `json:"glob,omitempty" description:"File glob filter (e.g. *.go)"`
}

// mcpGrepArgs keeps model-controlled grep operands out of option parsing.
func mcpGrepArgs(input mcpGrepInput, dir string) []string {
	args := []string{"-rn", "--color=never", "--max-count=100", "-e", input.Pattern}
	if input.Glob != "" {
		args = append(args, "--include="+input.Glob)
	}
	return append(append([]string{"grep"}, args...), "--", dir)
}

func registerGrepTool(srv *mcp.Server, perms permission.Service, sessionID string, workingDir string, toolCh chan mcpToolEvent) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "Grep",
		Description: "Search file contents using a regular expression.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpGrepInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP Grep called", "pattern", input.Pattern, "path", input.Path)

		id := uuid.New().String()
		inputJSON, _ := json.Marshal(input)
		emitToolStart(toolCh, id, "Grep", string(inputJSON))
		defer emitToolEnd(toolCh, id)

		dir := workingDir
		if input.Path != "" {
			dir = resolvePath(input.Path, workingDir)
		}

		granted, err := perms.Request(ctx, permission.CreatePermissionRequest{
			SessionID:   sessionID,
			ToolCallID:  id,
			ToolName:    "grep",
			Description: "Search: " + input.Pattern,
			Action:      "read",
			Params:      input,
			Path:        dir,
		})
		if err != nil || !granted {
			return toolError("grep denied"), nil, nil
		}

		args := mcpGrepArgs(input, dir)
		cmd := platform.Command(ctx, args[0], args[1:]...)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		_ = cmd.Run() // grep exits 1 when no matches — not an error

		// Cap total output size to avoid flooding the model context.
		const maxGrepBytes = 200 * 1024
		out := buf.String()
		if len(out) > maxGrepBytes {
			out = out[:maxGrepBytes] + "\n...(output truncated)"
		}
		slog.Debug("cliprovider: MCP Grep ok", "results_len", len(out))
		return toolText(out), nil, nil
	})
}

// ── todos ─────────────────────────────────────────────────────────────────────

// mergeMCPTodos merges the CLI model's desired todo list with the current DB
// state. The model's list is authoritative; the only protection applied is
// status protection: statuses can only advance (pending→in_progress→completed).
func mergeMCPTodos(dbTodos []session.Todo, modelItems []mcpTodoItem) []session.Todo {
	if len(dbTodos) == 0 {
		todos := make([]session.Todo, len(modelItems))
		for i, item := range modelItems {
			todos[i] = session.Todo{Content: item.Content, Status: session.TodoStatus(item.Status), ActiveForm: item.ActiveForm}
		}
		return todos
	}
	dbByContent := make(map[string]session.Todo, len(dbTodos))
	for _, t := range dbTodos {
		dbByContent[t.Content] = t
	}
	var result []session.Todo
	for _, item := range modelItems {
		wantStatus := session.TodoStatus(item.Status)
		if dbTodo, exists := dbByContent[item.Content]; exists {
			if mcpStatusLevel(dbTodo.Status) > mcpStatusLevel(wantStatus) {
				slog.Info("cliprovider: MCP todos protecting status from regression",
					"content", item.Content, "db_status", dbTodo.Status, "model_status", wantStatus)
				wantStatus = dbTodo.Status
			}
		}
		result = append(result, session.Todo{Content: item.Content, Status: wantStatus, ActiveForm: item.ActiveForm})
	}
	return result
}

func mcpStatusLevel(s session.TodoStatus) int {
	switch s {
	case session.TodoStatusInProgress:
		return 1
	case session.TodoStatusCompleted:
		return 2
	default:
		return 0
	}
}

type mcpTodoItem struct {
	Content    string `json:"content"     description:"What needs to be done (imperative form)"`
	Status     string `json:"status"      description:"Task status: pending, in_progress, or completed"`
	ActiveForm string `json:"active_form" description:"Present continuous form (e.g. 'Running tests')"`
}

type mcpTodosInput struct {
	Todos []mcpTodoItem `json:"todos" description:"The updated todo list"`
}

func registerTodosTool(srv *mcp.Server, sessions session.Service, sessionID string) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "todos",
		Description: "Update the task list for the current session. Use this to create, update or complete tasks so the user can track progress.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpTodosInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP todos called", "session", sessionID, "count", len(input.Todos))

		sess, err := sessions.Get(ctx, sessionID)
		if err != nil {
			return toolError("failed to get session: " + err.Error()), nil, nil
		}

		// Validate statuses.
		for _, item := range input.Todos {
			switch item.Status {
			case "pending", "in_progress", "completed":
			default:
				return toolError(fmt.Sprintf("invalid status %q for todo %q", item.Status, item.Content)), nil, nil
			}
		}

		// Merge with current DB todos: protect status from regression and keep user-added tasks.
		todos := mergeMCPTodos(sess.Todos, input.Todos)

		slog.Info(
			"cliprovider: MCP todos tool updating todos",
			"session", sessionID,
			"prev", sess.Todos,
			"merged", todos,
		)
		if err := sessions.SetTodos(ctx, sessionID, todos, sess.DeletedTodos); err != nil {
			return toolError("failed to save todos: " + err.Error()), nil, nil
		}

		completedCount, pendingCount, inProgressCount := 0, 0, 0
		for _, t := range todos {
			switch t.Status {
			case session.TodoStatusPending:
				pendingCount++
			case session.TodoStatusInProgress:
				inProgressCount++
			case session.TodoStatusCompleted:
				completedCount++
			}
		}
		slog.Debug("cliprovider: MCP todos saved", "pending", pendingCount, "in_progress", inProgressCount, "completed", completedCount)
		return toolText(fmt.Sprintf("Todo list updated. Status: %d pending, %d in progress, %d completed", pendingCount, inProgressCount, completedCount)), nil, nil
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

func toolText(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

// resolvePath resolves a path relative to workingDir, guarding against
// path traversal for relative paths. Absolute paths are cleaned but not
// restricted (the caller may legitimately reference files outside workingDir).
func resolvePath(path, workingDir string) string {
	if strings.HasPrefix(path, "/") || (len(path) > 1 && path[1] == ':') {
		return filepath.Clean(path) // absolute — just normalise
	}
	resolved := filepath.Clean(filepath.Join(workingDir, path))
	// Block relative paths that escape workingDir (e.g. "../../etc/passwd").
	cleanWD := filepath.Clean(workingDir)
	if resolved != cleanWD && !strings.HasPrefix(resolved, cleanWD+string(filepath.Separator)) {
		slog.Warn("cliprovider: path traversal blocked", "path", path, "resolved", resolved)
		return workingDir
	}
	return resolved
}

func runShell(ctx context.Context, command, dir string) (string, error) {
	if policyErr := agentguard.CheckCommandPolicy(command); policyErr != nil {
		return "", policyErr
	}
	if testRunShellSpawn != nil {
		return testRunShellSpawn(ctx, command, dir)
	}
	var buf bytes.Buffer
	env := append(os.Environ(), shell.RushEnvMarkers()...)
	err := shell.Run(ctx, shell.RunOptions{
		Command:    command,
		Cwd:        dir,
		Env:        env,
		Stdout:     &buf,
		Stderr:     &buf,
		BlockFuncs: mcpBashBlockFuncs(),
	})
	return buf.String(), err
}

func mcpBashBlockFuncs() []shell.BlockFunc {
	return append(shell.CanonicalBlockFuncs(), agentguard.CommandBlockFunc())
}

func sliceLines(content string, start, end int) string {
	lines := strings.Split(content, "\n")
	first := 0
	if start > 0 {
		first = start - 1 // convert 1-based start to 0-based
		if first >= len(lines) {
			return ""
		}
	}
	last := len(lines)
	if end > 0 && end < last {
		last = end // end_line is an inclusive 1-based line number
	}
	if last < first {
		return ""
	}
	return strings.Join(lines[first:last], "\n")
}
