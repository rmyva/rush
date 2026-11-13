package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Aliases: []string{"r"},
	Use:     "run [prompt...]",
	Args:    cobra.ArbitraryArgs,
	Short:   "Run a single non-interactive prompt",
	Long:    runLongHelp,
	Example: runExample,
	RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		// FIRST, before any slow boot work (config load, MCP init): if all
		// three std streams are redirected (detached orchestrator launch),
		// drop the console so the parent shell's exit can't hard-kill us
		// via CTRL_CLOSE_EVENT. See console_detach_windows.go — this kill
		// is unavoidable from a ctrl handler and has hit runs mid-boot.
		maybeDetachConsole()
		var (
			quiet, _            = cmd.Flags().GetBool("quiet")
			verbose, _          = cmd.Flags().GetBool("verbose")
			stream, _           = cmd.Flags().GetBool("stream")
			asJSON, _           = cmd.Flags().GetBool("json")
			timeout, _          = cmd.Flags().GetString("timeout")
			idleTimeout, _      = cmd.Flags().GetString("idle-timeout")
			stallTimeout, _     = cmd.Flags().GetDuration("stall-timeout")
			role, _             = cmd.Flags().GetString("role")
			effort, _           = cmd.Flags().GetString("effort")
			smartModel, _       = cmd.Flags().GetString("model")
			fastModel, _        = cmd.Flags().GetString("fast-model")
			sessionID, _        = cmd.Flags().GetString("session")
			findingsFrom, _     = cmd.Flags().GetString("findings-from")
			failOnReview, _     = cmd.Flags().GetBool("fail-on-review")
			useLast, _          = cmd.Flags().GetBool("continue")
			systemPrompt, _     = cmd.Flags().GetString("system-prompt")
			systemPromptFile, _ = cmd.Flags().GetString("system-prompt-file")
			// Fork patch (orchestrator UX): --format injects a per-turn
			// output-shape hint into the user prompt; --agents controls
			// the sub-agent dispatch policy; --aggregation controls how
			// sub-agent fan-out output reaches the orchestrator. See
			// run_format.go.
			formatFlag, _      = cmd.Flags().GetString("format")
			agentsMode, _      = cmd.Flags().GetString("agents")
			aggregationMode, _ = cmd.Flags().GetString("aggregation")
			maxAgentDepth, _   = cmd.Flags().GetInt("max-agent-depth")
			// Fork patch: batch 8 — timeout extension flags.
			timeoutExtendsOnProgress, _ = cmd.Flags().GetBool("timeout-extends-on-progress")
			timeoutHardCap, _           = cmd.Flags().GetString("timeout-hard-cap")
			// Fork patch: batch 24 — on-finish hook.
			onFinishHook, _ = cmd.Flags().GetString("on-finish")
			// Fork patch: batch 30 — runaway protection.
			maxCostStr, _   = cmd.Flags().GetString("max-cost")
			maxTokensStr, _ = cmd.Flags().GetString("max-tokens")
			// Fork patch (peak-hours bypass): --allow-peak-hours skips the
			// per-provider peak_hours refusal for THIS invocation only.
			//
			// WARNING: this flag must NEVER be added by an orchestrating
			// agent on its own initiative. Only pass --allow-peak-hours when
			// a human operator has explicitly asked, in this specific
			// request, to override peak hours. An agent that adds this flag
			// without an explicit human instruction is violating the
			// operator's intent.
			allowPeakHours, _ = cmd.Flags().GetBool("allow-peak-hours")
			// Fork patch (supervision): opt out of, or retune, the periodic
			// root-session check-in for this invocation.
			noSupervision, _           = cmd.Flags().GetBool("no-supervision")
			supervisionIntervalFlag, _ = cmd.Flags().GetString("supervision-interval")
			jobTimeoutFlag, _          = cmd.Flags().GetString("job-timeout")
			// Fork patch (worktree delegation): --worktree/--base/--branch.
			worktreeSlug, _   = cmd.Flags().GetString("worktree")
			worktreeBase, _   = cmd.Flags().GetString("base")
			worktreeBranch, _ = cmd.Flags().GetString("branch")
		)

		if maxAgentDepth < 0 || maxAgentDepth > 32 {
			return fmt.Errorf("--max-agent-depth: must be between 0 and 32")
		}
		if effort != "" {
			switch effort {
			case "low", "medium", "high":
			default:
				return fmt.Errorf("--effort: invalid value %q (allowed: low|medium|high)", effort)
			}
		}

		modelType, err := validateRunRole(role)
		if err != nil {
			return err
		}
		roleSmart := modelType == config.SelectedModelTypeSmart

		if systemPrompt != "" && systemPromptFile != "" {
			return fmt.Errorf("--system-prompt and --system-prompt-file are mutually exclusive")
		}
		if systemPromptFile != "" {
			bts, err := os.ReadFile(systemPromptFile)
			if err != nil {
				return fmt.Errorf("failed to read --system-prompt-file: %w", err)
			}
			systemPrompt = string(bts)
		}
		// Fork patch (orchestrator UX, round 2 #13): auto-load a repo-local
		// default system prompt from .rush/system-prompts/default.md if it
		// exists AND neither --system-prompt nor --system-prompt-file was
		// supplied. Lets a team commit one set of "always apply these
		// rules" instructions (scope-control, summary-required, no-commits)
		// to git instead of every operator passing --system-prompt-file by
		// hand on every invocation. Explicit flags always win — silent
		// auto-load only fills the gap when nothing was passed.
		if systemPrompt == "" {
			if cwd, cwdErr := ResolveCwd(cmd); cwdErr == nil {
				autoPath := filepath.Join(cwd, ".rush", "system-prompts", "default.md")
				if bts, err := os.ReadFile(autoPath); err == nil && len(bts) > 0 {
					systemPrompt = string(bts)
					fmt.Fprintf(os.Stderr, "auto-loaded system prompt from %s (%d bytes)\n", autoPath, len(bts))
				}
			}
		}

		// Fork patch (orchestrator UX): validate --agents up-front so a
		// typo doesn't waste a billed turn. Allowed values mirror the
		// three real policies the user can want.
		var (
			agentsDisable bool
			agentsHint    string
		)
		switch agentsMode {
		case "", "single":
			// New non-interactive default: NO sub-agent fan-out. `rush run`
			// has no UI to surface sub-agent work, and a nested run silently
			// burns time/tokens out of view (and risks recursion). The model
			// should EXECUTE rather than re-delegate. The `agent` and
			// `agentic_fetch` tools are removed from the toolset for this
			// process. Opt back in explicitly with --agents agent-allow or
			// --agents with-agents.
			//
			// NOT an absolute guarantee: shouldBypassSubAgentBan (app.go)
			// restores the `agent` tool anyway when this run is --role smart
			// AND a Worker model is configured — even with --agents single
			// passed explicitly. A configured Worker always means "run this
			// as an orchestrator"; `agentic_fetch` stays stripped either way,
			// since it's an unrelated concern. Explicit user decision — see
			// docs/reviews/2026-08-01-multi-agent-stability-review.md task
			// #203/#209. If a genuine hard guarantee against delegation is
			// needed, don't configure a Worker for the run.
			agentsDisable = true
		case "agent-allow":
			// Explicit opt-in: tools present, no nudge — model decides.
		case "with-agents":
			agentsHint = agentsModePromptHint
		default:
			return fmt.Errorf("--agents: invalid value %q (allowed: single|with-agents|agent-allow)", agentsMode)
		}

		formatHint, err := resolveFormatHint(formatFlag)
		if err != nil {
			return err
		}

		// Fork patch (orchestrator UX): validate --aggregation up-front
		// and append the matching prompt hint to the user prompt below.
		// "" / "summary" = no hint (upstream-default behaviour).
		var aggregationHint string
		switch aggregationMode {
		case "", "summary":
			// no hint
		case "concat":
			aggregationHint = aggregationConcatPromptHint
		case "attach":
			aggregationHint = aggregationAttachPromptHint
		default:
			return fmt.Errorf("--aggregation: invalid value %q (allowed: summary|concat|attach)", aggregationMode)
		}

		// Parse flexible duration flags.
		timeoutDur, err := parseDurationFlexible(timeout)
		if err != nil {
			return fmt.Errorf("--timeout: %w", err)
		}
		hardCapDur, err := parseDurationFlexible(timeoutHardCap)
		if err != nil {
			return fmt.Errorf("--timeout-hard-cap: %w", err)
		}
		idleTimeoutDur, err := parseDurationFlexible(idleTimeout)
		if err != nil {
			return fmt.Errorf("--idle-timeout: %w", err)
		}
		supervisionIntervalDur, err := parseDurationFlexible(supervisionIntervalFlag)
		if err != nil {
			return fmt.Errorf("--supervision-interval: %w", err)
		}
		jobTimeoutDur, err := parseDurationFlexible(jobTimeoutFlag)
		if err != nil {
			return fmt.Errorf("--job-timeout: %w", err)
		}
		if idleTimeoutDur <= 0 {
			// "0" (or the flag's own zero value) means "disable this
			// call's inactivity backstop" — the underlying stream
			// watchdog has no native off-switch, so a very large
			// duration stands in for one. Still bounded eventually by
			// --timeout (if set) and the default wall-clock cap
			// below.
			idleTimeoutDur = idleTimeoutDisabledSentinel
		}

		// Fork patch: batch 30 — parse --max-cost and --max-tokens.
		var maxCost float64
		if maxCostStr != "" {
			maxCost, err = strconv.ParseFloat(maxCostStr, 64)
			if err != nil || maxCost <= 0 {
				return fmt.Errorf("--max-cost: invalid value %q (must be a positive number like 0.50 or 2.00)", maxCostStr)
			}
		}
		var maxTokens int64
		if maxTokensStr != "" {
			maxTokens, err = parseTokenCount(maxTokensStr)
			if err != nil || maxTokens <= 0 {
				return fmt.Errorf("--max-tokens: %w", err)
			}
		}

		// A closed stdout pipe fails the exit flush instead of killing the run
		// (hook and Shutdown still run, R8C-7).
		defer keepBrokenPipeAsError()()

		ctx, cancel := commandSignalContext(context.Background(), cancelSignals...)
		defer cancel()

		// Both run deadlines carry their owned cause through graceful cleanup.
		// Only a process still alive past hardKillGrace is force-killed.
		var stopDeadline func()
		ctx, stopDeadline = installRunDeadline(ctx, timeoutDur,
			resolveDefaultHardTimeout(os.Getenv("RUSH_RUN_DEFAULT_HARD_TIMEOUT")),
			hardKillGrace, os.Stderr, exitWithAudit)
		defer stopDeadline()

		// Fork patch (worktree delegation): --worktree resolves or
		// creates <main>/worktrees/<slug>, chdirs the run into it,
		// defaults --session to the slug, and mirrors stdout/stderr
		// into the main checkout's .rush/stdin so results survive
		// `rush worktree remove` (see run_worktree.go). Validated even
		// without --worktree so stray --base/--branch fail fast.
		if err := validateRunWorktreeFlags(cmd, worktreeSlug, worktreeBase, worktreeBranch); err != nil {
			return err
		}
		if worktreeSlug != "" {
			wt, err := prepareRunWorktree(ctx, cmd, worktreeSlug, worktreeBase, worktreeBranch, sessionID)
			if err != nil {
				return err
			}
			defer wt.finish()
			sessionID = wt.sessionID
		}

		a, err := setupRunApp(cmd, asJSON, sessionID)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		// resolveSessionID handles the lookup path (exact id / hash prefix).
		// If it fails to find anything we fall through with the raw value:
		// resolveSession's get-or-create branch will create a session
		// whose ID is exactly the user-supplied string.
		if sessionID != "" {
			if sess, err := resolveSessionID(ctx, a.Sessions, sessionID); err == nil {
				sessionID = sess.ID
			}
		}

		if !a.Config().IsConfigured() {
			return fmt.Errorf("no providers configured - please run 'rush' to set up a provider interactively")
		}
		// Turn-stall abort policy: --stall-timeout (0 = off) feeds the
		// config knob the run's CallOptions resolution reads.
		a.Config().Options.TurnStallTimeoutSeconds = int(stallTimeout.Seconds())

		// foldRoleModel validates --role and resolves --continue's session;
		// the role model itself resolves live per turn (plan D2).
		smartModel, sessionID, useLast, err = foldRoleModel(ctx, a, a.Config().Models, role, modelType, smartModel, sessionID, useLast)
		if err != nil {
			return err
		}

		if verbose {
			slog.SetDefault(slog.New(log.New(os.Stderr)))
		}

		prompt := strings.Join(args, " ")

		prompt, err = MaybePrependStdin(prompt)
		if err != nil {
			slog.Error("Failed to read from stdin", "error", err)
			return err
		}

		if prompt == "" {
			return fmt.Errorf("no prompt provided")
		}

		// Fork patch (orchestrator UX): append the format/agents/
		// aggregation hints AFTER stdin read so the user's prompt
		// content stays at the top of the model's context (attention
		// favours the start).
		prompt = composeUserPrompt(prompt, formatHint, agentsHint, aggregationHint)

		// Fork patch: --findings-from appends another run's review
		// findings (verdict, review, warnings) after the user's prompt;
		// a missing or invalid envelope aborts before the run starts.
		prompt, err = applyFindingsFrom(prompt, findingsFrom)
		if err != nil {
			return err
		}

		if stream && asJSON {
			return fmt.Errorf("--stream and --json are mutually exclusive")
		}
		mode := app.RunModeTerse
		switch {
		case asJSON:
			mode = app.RunModeJSON
		case stream:
			mode = app.RunModeStream
		}
		// JSON mode forces quiet (hide spinner) so the spinner glyphs don't
		// leak into stdout. The summary line on stderr we still emit.
		hideSpinner := quiet || verbose || asJSON
		overrides := app.RunOverrides{
			SmartModel:                  smartModel,
			FastModel:                   fastModel,
			SystemPrompt:                systemPrompt,
			ReasoningEffort:             effort,
			RoleSmart:                   roleSmart,
			RequestedRole:               requestedRunRole(role),
			ModelRole:                   modelType,
			DisableSubAgents:            agentsDisable,
			MaxAgentDepth:               &maxAgentDepth,
			StripJSONFences:             formatFlag == "json" || strings.HasPrefix(formatFlag, "json-schema:"),
			AggregationMode:             aggregationMode,
			TimeoutExtendsOnProgress:    timeoutExtendsOnProgress, // Fork patch: batch 8
			TimeoutHardCap:              hardCapDur,               // Fork patch: batch 8
			OnFinishHook:                onFinishHook,             // Fork patch: batch 24
			MaxCost:                     maxCost,                  // Fork patch: batch 30
			MaxTokens:                   maxTokens,                // Fork patch: batch 30
			Timeout:                     timeoutDur,               // Fork patch: operator UX (budget display)
			IdleTimeout:                 idleTimeoutDur,           // Fork patch: inactivity backstop
			AllowPeakHours:              allowPeakHours,           // Fork patch: peak-hours bypass
			NoSupervision:               noSupervision,            // Fork patch: supervision opt-out
			SupervisionInterval:         supervisionIntervalDur,   // Fork patch: supervision interval override
			BackgroundJobDefaultTimeout: jobTimeoutDur,            // Fork patch: default background-job deadline
			Origin:                      message.OriginCLI,        // Fork patch: entry-channel origin
		}
		// The CLI calls the App's RunNonInteractive directly: the sdk
		// facade no longer re-exports an internal *app.App wrapper in
		// its public API (sdk.Wrap and sdk.Client.RunNonInteractive
		// were removed as internal-only seams).
		// Fork patch (finalization gate): with options.finalization.enabled
		// the terminal stdout is buffered and flushed once after the owner's
		// finalization record seals; flag off keeps the legacy path.
		res, runErr := runGatedNonInteractive(ctx, a.Config().Options.AgentFinalizationEnabled(),
			a.FinalizationTerminalGate, sessionID, os.Stdout,
			func(out io.Writer) (*app.RunResult, error) {
				return a.RunNonInteractiveWithResult(ctx, out, prompt, overrides, hideSpinner, mode, sessionID, useLast)
			})
		// Fork patch: --fail-on-review maps a "fail" reviewer verdict
		// to a dedicated exit code. Guarded by runErr == nil so a run
		// that itself failed keeps its own error exit code, and by
		// res != nil for defensive symmetry with the result contract.
		if runErr == nil && res != nil {
			if _, hit := reviewVerdictExitCode(res.ReviewVerdict, failOnReview); hit {
				return &reviewFailError{verdict: res.ReviewVerdict, sessionID: res.SessionID}
			}
		}
		return runErr
	},
}

func init() {
	// A failed/incomplete run returns an error to drive a non-zero exit code
	// (see app.RunNonInteractive / runIncompleteError). That is a runtime
	// outcome, not a usage mistake — don't dump the (very long) help text on
	// it. The concise error still prints to stderr.
	runCmd.SilenceUsage = true
	runCmd.Flags().BoolP("quiet", "q", false, "Hide spinner")
	runCmd.Flags().BoolP("verbose", "v", false, "Show logs")
	runCmd.Flags().String("role", "", "REQUIRED. Built-in model slot smart|fast|worker|reviewer, or an exact configured custom role name.")
	runCmd.Flags().String("effort", "", "Reasoning effort for this turn: low|medium|high. Applies to whichever slot --role picked. Not persisted by rush run.")
	runCmd.Flags().Bool("stream", false, "Stream every assistant token to stdout. Default is terse: tool-call names on stderr + final answer on stdout.")
	runCmd.Flags().Bool("json", false, "Emit one JSON object on stdout summarising the run (session_id, final_text, tool_calls, usage, duration, exit_reason). Mutually exclusive with --stream.")
	runCmd.Flags().String("timeout", "0", "Abort the whole run after this total wall-clock duration (e.g. 30s, 5m, 900 — plain number = seconds), including active work and waits. On deadline Rush requests graceful cancellation; if the process remains alive 60s later, the hard backstop force-exits it. Default 0 means no explicit deadline, but a 6h total default cap still applies; streaming progress does not reset it. RUSH_RUN_DEFAULT_HARD_TIMEOUT overrides that cap. See --idle-timeout for the separate inactivity backstop.")
	runCmd.Flags().String("idle-timeout", "15m", "End the run if the agent produces no activity (no streamed output, no tool call/result) for this long — a stuck tool call still counts as activity and is bounded separately. Terminal: unlike a provider stall on other rush entry points, this never silently retries. e.g. 5m, 900, 0 to disable. Default 15m.")
	runCmd.Flags().Duration("stall-timeout", 30*60*time.Second, "Abort a turn that has made no durable progress (tool call/result, step finish, checkpoint) for this long (turn-stall abort policy). Default 30m; 0 disables the abort.")
	runCmd.Flags().StringP("model", "m", "", "Model to use. Accepts 'model' or 'provider/model' to disambiguate models with the same name across providers")
	runCmd.Flags().String("fast-model", "", "Fast model to use. If not provided, uses the default fast model for the provider")
	runCmd.Flags().StringP("session", "s", "", "Session ID to continue OR create. If a session with this id exists it is continued; otherwise a new one is created with this id. Accepts a hash prefix for existing sessions only.")
	runCmd.Flags().String("findings-from", "", "Append a 'Findings from previous review' block (review verdict, review text, warnings) read from another run's envelope to this run's prompt. Value: a session id (resolves <main-checkout>/.rush/stdin/<id>.out, the file 'rush run --worktree' writes; see 'rush result') or a path to an envelope file. Missing or invalid envelope fails the run BEFORE it starts. Example: rush run --role smart --session fix-2 --findings-from fix-1 \"fix every finding\"")
	runCmd.Flags().Bool("fail-on-review", false, "Exit with code 3 when this run's automatic reviewer verdict is \"fail\". pass and pass_with_notes stay 0; a run with no reviewer pass keeps 0; a run that itself failed keeps its own exit code (1/124). The stdout envelope is unchanged — only the process exit code is affected. Read the review afterwards with 'rush result <session> --review'.")
	runCmd.Flags().BoolP("continue", "C", false, "Continue the most recent session")
	runCmd.Flags().String("system-prompt", "", "Override the session's system prompt with this string (persisted on the session)")
	runCmd.Flags().String("system-prompt-file", "", "Read the system prompt from this file (mutually exclusive with --system-prompt)")
	// Fork patch (orchestrator UX): per-turn output-shape and sub-agent
	// policy. Neither persists on the session.
	runCmd.Flags().String("format", "", "Per-turn output-shape hint appended to the user prompt. Presets: 'json' (final answer must be a single JSON value, no fences, no prose) | 'json-schema:<file>' (json + conform to this schema) | '@<file>' (use file contents verbatim as the hint) | any other text (used as a freeform 'Output format:' instruction). With --json or --format json, the envelope's final_text is also post-processed to strip ```json fences and prose preamble; the original is preserved in assistant_notes.")
	runCmd.Flags().String("agents", "", "Sub-agent dispatch policy for this run. Default (unset) = NO sub-agents: the `agent`/`agentic_fetch` tools are removed from the toolset for this non-interactive process (no UI to surface sub-agent work; a nested run burns tokens out of view). 'single' (same — explicit) | 'agent-allow' (opt in: tools present, model decides) | 'with-agents' (opt in + nudge the model to fan out). NOT an absolute guarantee: if --role smart is used AND a Worker model is configured, the `agent` tool is restored regardless of 'single' — a configured Worker always means orchestrator mode. `agentic_fetch` stays stripped either way.")
	runCmd.Flags().Int("max-agent-depth", 2, "Maximum remaining agent delegation edges (0..32). Default 2.")
	runCmd.Flags().String("aggregation", "", "How sub-agent fan-out output reaches the orchestrator. 'summary' (default — parent composes a wrap-up, sub-agent detail lives in DB only) | 'concat' (prompt-nudge: parent includes each sub-agent reply verbatim in final_text) | 'attach' (collect each sub-agent's last assistant text into envelope.sub_agent_outputs; final_text becomes a brief wrap-up). An always-on reduction-loss warning fires regardless when parent collapses sub-agent outputs to <40% of their combined size.")
	// Fork patch: batch 8 — timeout extension flags.
	runCmd.Flags().Bool("timeout-extends-on-progress", false, "Reset the per-turn stream watchdog deadline every time streaming progress occurs. Does not extend the total --timeout/default-cap run deadline. Default: false (static watchdog deadline).")
	runCmd.Flags().String("timeout-hard-cap", "0", "Maximum wall-clock time the stream watchdog allows even with --timeout-extends-on-progress (e.g. 30s, 5m, 900 — plain number = seconds); watchdog-only, separate from the total run deadline. Default: 0 (no cap).")
	// Fork patch: batch 24 — on-finish hook.
	runCmd.Flags().String("on-finish", "", "Shell command to execute after the run completes. Environment variables: RUSH_SESSION_ID, RUSH_EXIT_REASON, RUSH_COST_USD, RUSH_TOKENS, RUSH_DURATION_SEC. Hook errors are printed to stderr but don't affect exit code.")
	// Fork patch: batch 30 — runaway protection.
	runCmd.Flags().String("max-cost", "", "Abort the run if total cost (USD) exceeds this value. e.g. 0.50, 2.00")
	runCmd.Flags().String("max-tokens", "", "Abort the run if total prompt+completion tokens exceed this value. e.g. 100k, 1M, 500000")
	// Fork patch: MCP servers default to OFF in CLI mode (see mcpAppOptions
	// in root.go) unless mcp.<id>.enabled_in_cli=true; this is the one-off
	// escape hatch for a run that needs a server rush would otherwise skip.
	runCmd.Flags().Bool("all-mcp", false, `Force-start every configured, non-disabled MCP server for this run, ignoring each server's enabled_in_cli setting. Does not affect interactive TUI/web sessions, which always start every non-disabled server regardless of this flag.`)
	// Fork patch (peak-hours bypass): --allow-peak-hours skips the per-provider
	// peak_hours refusal for THIS invocation only. There is intentionally NO
	// config-level "always allow" equivalent — the whole point is a conscious
	// one-off override.
	//
	// WARNING: this flag must NEVER be added by an orchestrating agent on its
	// own initiative. Only pass --allow-peak-hours when a human operator has
	// explicitly asked, in this specific request, to override peak hours. An
	// agent that adds this flag without an explicit human instruction is
	// violating the operator's intent.
	runCmd.Flags().Bool("allow-peak-hours", false, `Bypass the per-provider peak_hours refusal for this single invocation only. WARNING: this flag must NEVER be added by an orchestrating agent on its own initiative — only pass it when a human operator has explicitly asked, in this specific request, to override peak hours. An agent that adds this flag without an explicit human instruction is violating the operator's intent. There is no persistent config-level equivalent; the override is conscious and one-off by design.`)
	runCmd.Flags().Bool("no-supervision", false, "Disable the periodic root-session supervision check-in for this run (default on, fires after 5m of chat silence while background jobs/delegations are still open; never fires with no open work or mid-turn).")
	runCmd.Flags().String("supervision-interval", "", "Override the supervision check-in's initial/reset silence interval (e.g. 10m, 600 — plain number = seconds). Empty = config's supervision_interval_minutes or the built-in 5m default.")
	runCmd.Flags().String("job-timeout", "", "Default terminate_and_wake deadline for background bash/run_command jobs that pass no explicit timeout (e.g. 2m, 90s — plain number = seconds). Empty = off (config's background_job_default_timeout_seconds can also set it; a per-call timeout always wins).")
	// Fork patch (worktree delegation): run inside an isolated git
	// worktree created/reused under the main checkout's worktrees/
	// directory. See run_worktree.go.
	runCmd.Flags().String("worktree", "", "Resolve or create the git worktree <main>/worktrees/<slug> and run this invocation with cwd inside it. The slug is a single path segment ([a-z0-9][a-z0-9._-]*). Reuses a registered worktree whose branch matches; refuses a path occupied by anything else or a branch mismatch (pass --branch to match). Defaults --session to the slug and mirrors stdout/stderr into <main>/.rush/stdin/<session>.{out,err}. Incompatible with --cwd and --data-dir. Manage with `rush worktree list` / `rush worktree remove <slug>`.")
	runCmd.Flags().String("base", "", "With --worktree: the ref the worktree's branch is created from (default: the main checkout's HEAD). Ignored when the branch already exists. Requires --worktree.")
	runCmd.Flags().String("branch", "", "With --worktree: the branch the worktree uses, created when missing (default: the slug). Required to match when reusing a worktree that sits on a different branch. Requires --worktree.")
	runCmd.MarkFlagsMutuallyExclusive("session", "continue")
	runCmd.MarkFlagsMutuallyExclusive("system-prompt", "system-prompt-file")
	runCmd.MarkFlagsMutuallyExclusive("stream", "json")
}
