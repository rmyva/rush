package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Aliases: []string{"r"},
	Use:     "run [prompt...]",
	Args:    cobra.ArbitraryArgs,
	Short:   "Run a single non-interactive prompt",
	Long: `Run a single prompt in non-interactive mode and exit.

  WARNING — auto-bypass of inner CLI permissions:
  ` + "`rush run`" + ` is non-interactive by design (no human at the keyboard),
  so any inner CLI sub-process it spawns (claude / codex / gemini) is
  launched with its own bypass-permissions flag (claude
  --dangerously-skip-permissions, codex --approval-mode yolo, gemini
  --yolo). The sub-process can read, write, and execute anywhere the
  invoking user has permission. There is no per-tool confirmation. Use
  ` + "`rush run`" + ` only in workspaces you can afford to lose; if you need
  real isolation, wrap the invocation in an OS-level sandbox (Sandboxie
  Plus, Windows Sandbox, WSL2 + landlock, Docker, etc.). Interactive
  sessions (TUI / web) keep the normal permission flow — this WARNING
  applies only to ` + "`rush run`" + `.

--role is REQUIRED: every invocation must declare which model slot it
wants. This avoids silently burning premium tokens on a one-liner.
Four roles exist:
  smart              the strong default slot (top-level agent runs here)
  fast               the cheap slot, for trivial work
  worker             optional, no alias; cheap slot for delegated
                      hands-on sub-task work. Never auto-selected — only
                      reachable via --role worker directly, OR indirectly
                      when a worker model is configured and this run uses
                      --role smart: sub-agents spawned by the "agent" tool
                      then run on worker, get a work-capable toolset
                      (edit/multiedit/write/bash/todos/download/fetch/
                      ask_question), and the default sub-agent ban below
                      is lifted so the orchestrator can delegate at all —
                      even against an explicit --agents single, since a
                      configured worker means delegation is the intent.
                      The smart agent's edit/multiedit/write tools are
                      also REMOVED (bash and read tools stay, for
                      verification), and its system prompt gains an
                      "Orchestrator mode" rule to delegate in
                      context-sized chunks.
  reviewer           optional, no alias; the strongest slot, for explicit
                      review invocations. Never auto-selected anywhere —
                      reachable only via --role reviewer.
                      Reviewer auto-pass: with a Reviewer model configured,
                      a successful --role smart run is automatically
                      continued with one more turn on the Reviewer model
                      that reviews everything the session did and states
                      its own conclusion. The executor's own last answer
                      stays the run's output ("final_text" in --json); the
                      reviewer's conclusion is carried additively in the
                      "review" field of the --json envelope (empty when no
                      reviewer ran). "review_verdict" carries the parsed
                      first line: pass, pass_with_notes, fail, unverified
                      (the reviewer passed the run without a single
                      read-tool call of its own), unparsed (no verdict
                      line) or error (the review turn failed). The verdict
                      is an opinion of a second model and the exit code is
                      not changed by it. The reviewer turn's toolset is
                      read-only (view/grep/glob/ls/git_read/
                      read_delegation_transcript and the fs_* read tools; no
                      write, no shell, no sub-agents, no network, no
                      questions), so a reviewer pass can never mutate the
                      workspace it audits, and it is bounded by a 60-minute
                      hang-guard on top of the run's own --timeout. An error
                      in the review turn does not change the run's outcome:
                      the executor's answer and its exit code are kept. Runs
                      that failed, were canceled, timed out, or queued are
                      returned unchanged, and an explicit --role reviewer
                      invocation is never extended. Unconfigure the reviewer
                      to disable the pass.
worker/reviewer are configured with "rush models use <smart> <fast>
--worker <model> --reviewer <model>" (or the web UI / rush.json's
models.worker / models.reviewer directly). The actual model id behind
each role comes from "rush models show"; --model overrides it for one
invocation. If the session already has a per-session worker/reviewer
override (set via the web UI's model selector), that override wins over
the config default — same precedence as the smart/fast per-session pin.

Prompt sources (combined as "<stdin>\n\n<args>"):
  - positional args:   rush run "your prompt"
  - stdin pipe/redir:  echo "hello" | rush run     |     rush run < prompt.md
  - both (stdin = context, args = question)

Sessions: --session takes either an existing session id (or hash prefix)
to continue, OR an arbitrary new id to start a fresh session with that
exact id — handy for CI where the build matrix maps to a stable key.

System prompt: --system-prompt / --system-prompt-file persists the prompt
on the session so subsequent runs with the same --session pick it up.
If neither is passed and .rush/system-prompts/default.md exists, it is
loaded automatically (see "rush claude-init" for a starter template).

Session title: if the session title is empty or "Untitled Session", the
first 60 characters of the user prompt are used as the title. This makes
"sessions list" immediately useful without a --title flag.

Budget persistence: --max-cost, --max-tokens, and --timeout values are
saved on the session row. "sessions show" displays cost vs budget, and
"sessions locks" shows an ELAPSED / BUDGET column. ended_reason is written
when the run ends and equals its exit_reason (the --json envelope's): the
model's finish (end_turn, stop, max_tokens, ...), "error" (a failed run; also
a --max-cost/--max-tokens exit), "timeout" (--timeout or the default 6h cap),
or "canceled" (Ctrl-C, caller cancellation, "sessions cancel"; an early
cancel can leave the older spelling "cancelled"). It is empty while a run is in progress
and appears in "sessions show" and "sessions list --json" (ended_reason).

Wake timers: the model can schedule its own wakeups with the wakein/wakeon
(one-shot) and loop (recurring) tools. A ONE-SHOT timer holds this process
open: the run waits until it fires, reacts to the wake_fired event, and
exits as usual (--timeout, the 6h cap, Ctrl-C and "sessions cancel" still
stop the wait: owned deadlines end "timeout", caller cancellation "canceled";
an overdue timer is not caught up). An endless loop schedule never holds a non-interactive run open: when
the scope closes with no other open work, the session's loop schedules are
cancelled at run end (one stderr line and a --json warnings entry), while a
Ctrl-C/--timeout exit leaves them in the DB to fire when any rush process
of that data directory runs.

Output modes (mutually exclusive --stream / --json):
  - default (terse): tool-call names on stderr as "▶ <toolName>"; only
    the final assistant message on stdout.
  - --stream:        every assistant token streamed live to stdout.
  - --json:          a single JSON object on stdout when the run ends —
                     {session_id, exit_reason, final_text, review,
                     review_verdict, assistant_notes, tool_calls, usage,
                     duration_ms, error}. "review" carries the auto
                     reviewer pass's verdict (empty when none ran) and
                     "review_verdict" its parsed first line —
                     pass/pass_with_notes/fail/unverified/unparsed/error;
                     final_text stays the executor's answer and the exit
                     code stays the executor's. Tool-call
                     heartbeat still goes to stderr so wrappers can show
                     progress.

Shaping the model's output:
  --format <preset|string>  Appends an in-prompt hint that asks the model
                            to produce a specific shape for this turn
                            only (not persisted). Presets:
                              json              -> final answer is one
                                                   raw JSON value, no
                                                   fence, no prose.
                              json-schema:<f>   -> same + conform to <f>.
                              @<file>           -> use <file> contents
                                                   verbatim as the hint.
                              <any other text>  -> freeform "Output
                                                   format: ..." instruction.
                            With --json or --format json, the envelope's
                            final_text is also post-processed: a
                            triple-backtick "json" fence and prose
                            preamble/suffix are stripped so
                            "jq .final_text" works directly. The
                            original (unstripped) text is preserved in
                            envelope.assistant_notes when stripping ran.

Sub-agent policy:
  --agents <single|with-agents|agent-allow>
                            (unset)       -- DEFAULT. No sub-agents: the
                                             "agent" and "agentic_fetch"
                                             tools are removed from the
                                             toolset for this run. A
                                             non-interactive run has no UI
                                             to surface sub-agent work and a
                                             nested run burns tokens out of
                                             view (and risks recursion), so
                                             the model EXECUTES instead of
                                             re-delegating.
                            single        -- same as the default, explicit.
                                             Still yields to a configured
                                             Worker model under --role smart
                                             (see "worker" above) — a
                                             configured worker means
                                             delegation is the intent.
                            agent-allow   -- opt in: tools present, the
                                             model decides whether to use
                                             them.
                            with-agents   -- opt in + nudge the model to fan
                                             out via the "agent" tool when
                                             work is decomposable.

Sub-agent aggregation (only matters when --agents permits fan-out):
  --aggregation <summary|concat|attach>
                            summary  -- default. Parent composes a
                                        wrap-up; sub-agent detail lives
                                        only in the DB. Easy on the
                                        envelope, can lose information.
                            concat   -- prompt-nudge asks the parent to
                                        include each sub-agent's reply
                                        verbatim in final_text. Bigger
                                        final_text but no detail lost.
                            attach   -- each sub-agent's last assistant
                                        text is collected into the
                                        envelope's sub_agent_outputs
                                        array; final_text becomes a
                                        brief wrap-up. Best for
                                        machine consumers that want the
                                        structured set.
                            A reduction-loss warning ALWAYS fires in
                            envelope.warnings when parent collapses
                            sub-agent outputs to <40% of their combined
                            character count.

Runaway protection:
  --max-cost 0.50     abort when session cost (USD) crosses the threshold
  --max-tokens 100k   abort when prompt+completion tokens cross the limit
                      (supports k/M suffixes: 100k = 100000, 1M = 1000000)
  Both checks fire after each agent step. The session is saved with its
  partial work so you can inspect or fork from it.
  Without --timeout a 6h default wall-clock cap applies: a GRACEFUL deadline
  like --timeout (a run waiting on jobs ends through its normal exit, with an
  envelope and exit_reason "timeout"); a process still alive 60s past it
  (deadlock / a read that ignores ctx) is force-killed. A wait longer than the
  cap needs an explicit --timeout. Override the cap via
  RUSH_RUN_DEFAULT_HARD_TIMEOUT (plain number = seconds, or a Go duration like
  30m/2h; invalid/non-positive falls back to the 6h default).
  Timeout error names duration/source; resume_command preserves role/session.
  Stderr: plain error + exact resume command + stored-work hint; exit 1 (124
  for the hard backstop). Compatibility: owned deadlines changed "canceled"
  to "timeout"; caller/parent cancellation remains "canceled". Inner deadlines
  while the run context is live are not run timeouts.

Peak-hours override:
  --allow-peak-hours  bypass a provider's configured peak_hours refusal for
                      this single invocation. No persistent config-level
                      equivalent exists; the override is conscious and
                      one-off by design.
                      WARNING: this flag must NEVER be added by an
                      orchestrating agent on its own initiative. Only pass
                      --allow-peak-hours when a human operator has explicitly
                      asked, in this specific request, to override peak hours.
                      An agent that adds this flag without an explicit human
                      instruction is violating the operator's intent.

Lifecycle hooks:
  --on-finish "cmd"   run a shell command after the agent finishes.
                      Env vars set: RUSH_SESSION_ID, RUSH_EXIT_REASON,
                      RUSH_COST_USD, RUSH_TOKENS, RUSH_DURATION_SEC.

External cancellation: use "rush sessions cancel <id>" from another
terminal to set a DB flag; the running agent checks it after each step
and stops gracefully.

Permissions: non-interactive runs auto-approve every permission request
(no one is on the keyboard to confirm). The agent gets the full tool
set with no prompting. This is fast but irreversible — only run
"rush run"" in a workspace whose contents you can afford to lose, and
prefer --cwd /some/sandbox-or-temp-dir for one-shot scripts.

MCP servers: by default only servers with mcp.<id>.enabled_in_cli=true
in rush.json are started for "rush run"" (and every other non-web CLI
subcommand) — most MCP servers add nothing to an unattended agent while
still paying real startup cost (a stdio server spawns its own child
process every invocation). Interactive "rush"" (the web UI) is
unaffected and always starts every non-disabled server.
  --all-mcp   start every configured, non-disabled MCP server for this
              run, ignoring enabled_in_cli. Use when this specific run
              genuinely needs an MCP server rush would otherwise skip.

Restricted-run allowlist (scoped permissions):
  --restrict-run           flip from auto-approve-everything to
                           deny-by-default; only allowlist matches are
                           approved. Also armed by setting
                           permissions.run.restrict=true in rush.json.
  --allow-bash '<pattern>' permit a bash command in restricted mode
                           (repeatable). Forms:
                             cmd args    word-boundary prefix
                                         (e.g. 'git diff' matches
                                         'git diff HEAD~1' but refuses
                                         'ls && rm -rf /')
                             exact:cmd   whole-string equality
                             glob:pat    filepath.Match glob
                             regex:pat   regexp on the command
  --allow-tool '<name>'    permit a non-bash tool or tool:action in
                           restricted mode (repeatable). e.g. 'view' or
                           'edit:write'. NOTE: 'bash' / 'bash:execute'
                           are ignored here — use --allow-bash for
                           command-level control of the bash tool.

  Config equivalent (rush.json):
    "permissions": {
      "run": {
        "restrict": true,
        "allow_tools": ["view"],
        "allow_bash": ["git diff", "glob:ls *"]
      }
    }
  CLI flags merge with config (union); --restrict-run forces restrict on.
  Precedence: permissions.allowed_tools is a global bypass that still
  wins over both lists (it is checked before the gate). Listing 'bash'
  there approves EVERY bash command; for command-level control, leave
  allowed_tools empty for bash and use --allow-bash / run.allow_bash.
  Restricted mode never affects interactive TUI / web sessions.

Protecting harness-owned files: when the orchestrator pipes "rush run""
output through a shell redirect, the model can pick the same path for
its write/edit tool (e.g. it sees ".tmp-audit-D.json" in the prompt
and decides "I'll just write findings there directly"). Result: a
mixed file where envelope and tool output overwrite each other in
non-deterministic order. To block this set the RUSH_FORBID_WRITES
env-var to a comma-separated list of paths the write/edit/multiedit
tools must NOT touch. Example:

  out=/tmp/audit.json
  RUSH_FORBID_WRITES="$out" \
    rush run --json --format json ... > "$out" 2>"$out.err"

The tools fail with a visible error to the model when a forbidden path
is targeted; the model then either retries with a different path or
falls back to returning the content via final_text — both of which
keep the redirect target intact.

Time limits (usually leave both alone): --timeout bounds the WHOLE run and
defaults to 0 (disabled), but a run is never unbounded: without --timeout
the 6h default wall-clock cap applies (see "Runaway protection" above;
override via RUSH_RUN_DEFAULT_HARD_TIMEOUT), and a wait known to be longer
(an 8h soak test) needs an explicit --timeout. --idle-timeout (default 15m)
ends the run if the agent goes quiet for that long DURING A TURN; it does not
watch a run that is waiting between turns on a job, a delegation or a retry
pause -- that wait is bounded only by --timeout or the cap. Only reach for
--timeout when a run must fit a hard external deadline (a CI job slot, a
cron window) or must outlive the cap.`,
	Example: `
# Run a simple prompt
rush run "Guess my 5 favorite Pokémon"

# Pipe input from stdin (stdin is prepended to the args prompt)
curl https://charm.land | rush run "Summarize this website"

# Read the prompt from a file
rush run < prompt.md

# Redirect output to a file
rush run "Generate a hot README for this project" > MY_HOT_README.md

# Quiet mode (hide the spinner) / verbose mode (show logs)
rush run --quiet  "Generate a README for this project"
rush run --verbose "Generate a README for this project"

# Continue a previous session by id (or hash prefix)
rush run --session {session-id} "Follow up on your last response"

# Continue the most recent session
rush run --continue "Follow up on your last response"

# Idempotent CI: same id across runs continues the same conversation;
# the first run creates it. Use a stable key like a PR number.
rush run --session "pr-42" "Review the latest changes"

# Override the session's system prompt from a flag
rush run --system-prompt "You are a terse senior reviewer." \
          --session "pr-42" "Review the latest changes"

# Or from a file (mutually exclusive with --system-prompt)
rush run --system-prompt-file ./reviewer-prompt.md \
          --session "pr-42" "Review the latest changes"

# Stdin user-prompt + file system-prompt + stable session id — the three
# inputs are independent, so this works as one pipeline:
git diff HEAD~1 | rush run --system-prompt-file ./reviewer-prompt.md \
                            --session "pr-42" \
                            "Review this diff"

# Required role flag — pick the strong model
rush run --role smart "design a migration for X"

# …or the cheap one for a quick one-liner
rush run --role fast "tldr this readme" < README.md

# Watch the agent think token-by-token (legacy output mode)
rush run --role smart --stream "explain this codebase"

# Runaway protection: cap cost and tokens (persisted to session budget)
rush run --role smart --max-cost 0.50 --max-tokens 100k "refactor storage"
# After run: rush sessions show <id> → "Cost: $0.32 / $0.50 budget (64%)"

# On-finish hook (env: RUSH_SESSION_ID, RUSH_EXIT_REASON, RUSH_COST_USD, ...)
rush run --role smart --on-finish "echo done >> /tmp/log" "analyze codebase"

# Auto-loaded repo-default system prompt (from .rush/system-prompts/default.md)
rush run --role smart --session "pr-42" "review this PR"
# → auto-loads scope-control / summary-required rules without --system-prompt-file

# Machine-readable summary for wrapper scripts
rush run --json --session "pr-42" "review the diff" | jq .final_text

# Force raw JSON in final_text (model is instructed AND envelope is
# post-stripped; markdown fences and prose preamble go to
# assistant_notes).
rush run --role smart --json --format json \
          --session "audit-A" "..." < /tmp/audit-prompt.md

# JSON conforming to a schema
rush run --role smart --json \
          --format json-schema:./schemas/audit.json \
          --session "audit-A" "..." < /tmp/audit-prompt.md

# Disable sub-agent fan-out (the "agent" tool is removed from the
# toolset entirely so the model cannot dispatch even if it wants to).
rush run --role smart --agents single --session "linear-task" "..."

# Tell the model to fan out (system-prompt nudge — still up to the
# model whether it actually decomposes the work).
rush run --role smart --agents with-agents --session "parallel-audit" "..."

# Fan-out and recover every sub-agent's full text via the envelope.
# Parent's final_text becomes a brief wrap-up; sub_agent_outputs
# array carries the verbatim sub-agent replies.
rush run --role smart --json --agents with-agents --aggregation attach \
          --session "structured-audit" < /tmp/p.txt > /tmp/audit.json

# Orchestrator mode: with a worker model configured (models.worker in
# rush.json / web UI), --role smart on its own is enough — the default
# sub-agent ban is lifted, sub-agents get a work-capable toolset and run
# on the cheap worker slot, and the smart agent's edit/multiedit/write
# tools are removed so hands-on work has to be delegated.
rush run --role smart --session "orchestrated-refactor" "..."

# Bypass the strongest slot for an explicit one-off review (never picked
# automatically — must be requested by role name).
rush run --role reviewer --session "pr-42-review" "review this diff"

# Fan-out but keep everything in final_text verbatim (one big string).
rush run --role smart --json --agents with-agents --aggregation concat \
          --session "flat-audit" < /tmp/p.txt > /tmp/audit.json

# Scope an unattended CI run: only view + the listed bash commands are
# approved; everything else is denied cleanly (no UI to hang on).
rush run --restrict-run --role fast \
          --allow-tool view \
          --allow-bash 'git diff' --allow-bash 'glob:go *' \
          --session "ci-123" "summarize the diff"

# Rarely needed: a run has no --timeout deadline by default (--idle-timeout ends
# it after 15m of inactivity; a graceful 6h wall-clock cap ends a very long
# wait). --timeout is for fitting a hard external deadline, e.g. a CI job
# slot, or for waiting longer than that cap.
rush run --role smart --timeout 5m --session "long-task" "refactor the storage layer"
  `,
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
			// Fork patch: batch 8 — timeout extension flags.
			timeoutExtendsOnProgress, _ = cmd.Flags().GetBool("timeout-extends-on-progress")
			timeoutHardCap, _           = cmd.Flags().GetString("timeout-hard-cap")
			// Fork patch: batch 24 — on-finish hook.
			onFinishHook, _ = cmd.Flags().GetString("on-finish")
			// Fork patch: batch 30 — runaway protection.
			maxCostStr, _   = cmd.Flags().GetString("max-cost")
			maxTokensStr, _ = cmd.Flags().GetString("max-tokens")
			// Fork patch (run allowlist): opt into the restricted-run
			// permission model and/or add one-off bash patterns for this
			// invocation. Merged with permissions.run from config.
			restrictRun, _ = cmd.Flags().GetBool("restrict-run")
			allowBash, _   = cmd.Flags().GetStringSlice("allow-bash")
			allowTool, _   = cmd.Flags().GetStringSlice("allow-tool")
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
		)

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

		// On Windows, filter out console-close/logoff/shutdown events so
		// only a genuine Ctrl+C cancels the run — otherwise a plain
		// `rush run --session X "message"` typed directly in a terminal
		// can abort with "Context canceled" from an ordinary console event
		// that has nothing to do with the user asking to stop. No-op on
		// other platforms. See console_ctrl_windows.go for the full
		// rationale.
		uninstallConsoleCtrlFilter := installConsoleCtrlFilter()
		defer uninstallConsoleCtrlFilter()

		// A closed stdout pipe fails the exit flush instead of killing the run
		// (hook and Shutdown still run, R8C-7).
		defer keepBrokenPipeAsError()()

		// Cancel on SIGINT or SIGTERM.
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
		defer cancel()

		// Last-resort teardown for the SECOND interrupt.
		//
		// NotifyContext restores the default disposition after the signal it
		// acts on, so a user who presses Ctrl-C again — the normal reaction
		// when the first one appears to do nothing — kills rush outright,
		// with none of its cleanup running. That used to be survivable: the
		// terminal delivers the signal to its whole foreground process group,
		// which the CLI child was part of, so the child died with us. It is
		// not survivable since CLI children were given their own process
		// groups; measured on Ubuntu 24.04, a SIGINT to the parent's group
		// reaches a child spawned without Setpgid and does NOT reach one
		// spawned with it (see internal/session/track_unix.go for the
		// numbers). The child and its own descendants would simply keep
		// running.
		//
		// So the second interrupt is caught rather than left to the default:
		// sweep the process groups (Unix) or jobs (Windows) this process
		// registered, then exit. Registering a handler means the second
		// Ctrl-C no longer kills us instantly, so the sweep must be
		// unconditionally followed by os.Exit — a hang here would take away
		// the user's ability to stop rush at all, which is worse than the
		// leak it prevents. 130 is the conventional status for
		// SIGINT-terminated.
		hardStop := make(chan os.Signal, 2)
		signal.Notify(hardStop, os.Interrupt)
		defer signal.Stop(hardStop)
		go func() {
			<-hardStop // first: NotifyContext is cancelling the run
			<-hardStop // second: the user means it
			if n := session.KillAllTrackedTrees(); n > 0 {
				fmt.Fprintf(os.Stderr, "\nrush: interrupted — killed %d CLI process tree(s)\n", n)
			}
			exitWithAudit(130)
		}()

		// Both run deadlines carry their owned cause through graceful cleanup.
		// Only a process still alive past hardKillGrace is force-killed.
		var stopDeadline func()
		ctx, stopDeadline = installRunDeadline(ctx, timeoutDur,
			resolveDefaultHardTimeout(os.Getenv("RUSH_RUN_DEFAULT_HARD_TIMEOUT")),
			hardKillGrace, os.Stderr, exitWithAudit)
		defer stopDeadline()

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
			ModelRole:                   modelType,
			DisableSubAgents:            agentsDisable,
			StripJSONFences:             formatFlag == "json" || strings.HasPrefix(formatFlag, "json-schema:"),
			AggregationMode:             aggregationMode,
			TimeoutExtendsOnProgress:    timeoutExtendsOnProgress, // Fork patch: batch 8
			TimeoutHardCap:              hardCapDur,               // Fork patch: batch 8
			OnFinishHook:                onFinishHook,             // Fork patch: batch 24
			MaxCost:                     maxCost,                  // Fork patch: batch 30
			MaxTokens:                   maxTokens,                // Fork patch: batch 30
			Timeout:                     timeoutDur,               // Fork patch: operator UX (budget display)
			IdleTimeout:                 idleTimeoutDur,           // Fork patch: inactivity backstop
			RestrictedRun:               restrictRun,              // Fork patch: run allowlist
			AllowBash:                   allowBash,                // Fork patch: run allowlist
			AllowTools:                  allowTool,                // Fork patch: run allowlist
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
		_, runErr = a.RunNonInteractiveWithResult(ctx, os.Stdout, prompt, overrides, hideSpinner, mode, sessionID, useLast)
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
	runCmd.Flags().String("role", "", "REQUIRED. Which preselected model slot to use: smart (the strong default; --role smart + a configured worker also enables orchestrator-mode sub-agent delegation) | fast (the cheap slot) | worker (cheap slot for delegated sub-task work, never auto-selected) | reviewer (the strongest slot, for explicit review invocations, never auto-selected). The actual model id comes from `rush models show`; override with --model.")
	runCmd.Flags().String("effort", "", "Reasoning effort for this turn: low|medium|high. Applies to whichever slot --role picked. Not persisted by rush run.")
	runCmd.Flags().Bool("stream", false, "Stream every assistant token to stdout. Default is terse: tool-call names on stderr + final answer on stdout.")
	runCmd.Flags().Bool("json", false, "Emit one JSON object on stdout summarising the run (session_id, final_text, tool_calls, usage, duration, exit_reason). Mutually exclusive with --stream.")
	runCmd.Flags().String("timeout", "0", "Abort the run after this duration (e.g. 30s, 5m, 900 — plain number = seconds). A hard wall-clock kill force-exits the process 60s past this even on a freeze. Default 0 (no --timeout deadline; a graceful 6h default cap still applies, RUSH_RUN_DEFAULT_HARD_TIMEOUT overrides it, and --timeout is how to wait longer; see --idle-timeout for the inactivity backstop).")
	runCmd.Flags().String("idle-timeout", "15m", "End the run if the agent produces no activity (no streamed output, no tool call/result) for this long — a stuck tool call still counts as activity and is bounded separately. Terminal: unlike a provider stall on other rush entry points, this never silently retries. e.g. 5m, 900, 0 to disable. Default 15m.")
	runCmd.Flags().Duration("stall-timeout", 30*60*time.Second, "Abort a turn that has made no durable progress (tool call/result, step finish, checkpoint) for this long (turn-stall abort policy). Default 30m; 0 disables the abort.")
	runCmd.Flags().StringP("model", "m", "", "Model to use. Accepts 'model' or 'provider/model' to disambiguate models with the same name across providers")
	runCmd.Flags().String("fast-model", "", "Fast model to use. If not provided, uses the default fast model for the provider")
	runCmd.Flags().StringP("session", "s", "", "Session ID to continue OR create. If a session with this id exists it is continued; otherwise a new one is created with this id. Accepts a hash prefix for existing sessions only.")
	runCmd.Flags().BoolP("continue", "C", false, "Continue the most recent session")
	runCmd.Flags().String("system-prompt", "", "Override the session's system prompt with this string (persisted on the session)")
	runCmd.Flags().String("system-prompt-file", "", "Read the system prompt from this file (mutually exclusive with --system-prompt)")
	// Fork patch (orchestrator UX): per-turn output-shape and sub-agent
	// policy. Neither persists on the session.
	runCmd.Flags().String("format", "", "Per-turn output-shape hint appended to the user prompt. Presets: 'json' (final answer must be a single JSON value, no fences, no prose) | 'json-schema:<file>' (json + conform to this schema) | '@<file>' (use file contents verbatim as the hint) | any other text (used as a freeform 'Output format:' instruction). With --json or --format json, the envelope's final_text is also post-processed to strip ```json fences and prose preamble; the original is preserved in assistant_notes.")
	runCmd.Flags().String("agents", "", "Sub-agent dispatch policy for this run. Default (unset) = NO sub-agents: the `agent`/`agentic_fetch` tools are removed from the toolset for this non-interactive process (no UI to surface sub-agent work; a nested run burns tokens out of view). 'single' (same — explicit) | 'agent-allow' (opt in: tools present, model decides) | 'with-agents' (opt in + nudge the model to fan out). NOT an absolute guarantee: if --role smart is used AND a Worker model is configured, the `agent` tool is restored regardless of 'single' — a configured Worker always means orchestrator mode. `agentic_fetch` stays stripped either way.")
	runCmd.Flags().String("aggregation", "", "How sub-agent fan-out output reaches the orchestrator. 'summary' (default — parent composes a wrap-up, sub-agent detail lives in DB only) | 'concat' (prompt-nudge: parent includes each sub-agent reply verbatim in final_text) | 'attach' (collect each sub-agent's last assistant text into envelope.sub_agent_outputs; final_text becomes a brief wrap-up). An always-on reduction-loss warning fires regardless when parent collapses sub-agent outputs to <40% of their combined size.")
	// Fork patch: batch 8 — timeout extension flags.
	runCmd.Flags().Bool("timeout-extends-on-progress", false, "Reset the stream watchdog deadline every time streaming progress occurs. Prevents killing healthy long compositions. Default: false (static deadline).")
	runCmd.Flags().String("timeout-hard-cap", "0", "Maximum wall-clock time the watchdog allows even with --timeout-extends-on-progress (e.g. 30s, 5m, 900 — plain number = seconds). Default: 0 (no cap).")
	// Fork patch: batch 24 — on-finish hook.
	runCmd.Flags().String("on-finish", "", "Shell command to execute after the run completes. Environment variables: RUSH_SESSION_ID, RUSH_EXIT_REASON, RUSH_COST_USD, RUSH_TOKENS, RUSH_DURATION_SEC. Hook errors are printed to stderr but don't affect exit code.")
	// Fork patch: batch 30 — runaway protection.
	runCmd.Flags().String("max-cost", "", "Abort the run if total cost (USD) exceeds this value. e.g. 0.50, 2.00")
	runCmd.Flags().String("max-tokens", "", "Abort the run if total prompt+completion tokens exceed this value. e.g. 100k, 1M, 500000")
	// Fork patch: MCP servers default to OFF in CLI mode (see mcpAppOptions
	// in root.go) unless mcp.<id>.enabled_in_cli=true; this is the one-off
	// escape hatch for a run that needs a server rush would otherwise skip.
	runCmd.Flags().Bool("all-mcp", false, `Force-start every configured, non-disabled MCP server for this run, ignoring each server's enabled_in_cli setting. Does not affect interactive TUI/web sessions, which always start every non-disabled server regardless of this flag.`)
	// Fork patch (run allowlist): scope what an unattended run may do.
	runCmd.Flags().Bool("restrict-run", false, `Opt into restricted permission mode for this run. When set (or when permissions.run.restrict is true in config), permission requests are auto-approved ONLY if they match the allowlist; everything else is denied cleanly. Default remains auto-approve-everything. Does not affect interactive TUI/web sessions.`)
	runCmd.Flags().StringSlice("allow-bash", nil, `Bash command patterns to permit in restricted-run mode (repeatable, also comma-separated). Merged with permissions.run.allow_bash from config. Forms: 'cmd args' (word-boundary prefix, chaining-guarded) | 'exact:cmd' | 'glob:pat' | 'regex:pat'. Examples: --allow-bash 'git diff' --allow-bash 'glob:ls *' --allow-bash 'regex:^go (test|build)'`)
	runCmd.Flags().StringSlice("allow-tool", nil, `Non-bash tools (or tool:action pairs) to permit in restricted-run mode (repeatable, also comma-separated). Merged with permissions.run.allow_tools from config. Entries for 'bash' / 'bash:execute' are ignored — use --allow-bash for command-level bash control. Examples: --allow-tool view --allow-tool edit:write`)
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
	runCmd.MarkFlagsMutuallyExclusive("session", "continue")
	runCmd.MarkFlagsMutuallyExclusive("system-prompt", "system-prompt-file")
	runCmd.MarkFlagsMutuallyExclusive("stream", "json")
}

// resolveSessionID resolves a session by exact UUID or hash prefix.
func resolveSessionID(ctx context.Context, svc session.Service, id string) (session.Session, error) {
	if s, err := svc.Get(ctx, id); err == nil {
		return s, nil
	}

	sessions, err := svc.List(ctx)
	if err != nil {
		return session.Session{}, err
	}

	var matches []session.Session
	for _, s := range sessions {
		hash := session.HashID(s.ID)
		if hash == id || strings.HasPrefix(hash, id) {
			matches = append(matches, s)
		}
	}

	if len(matches) == 0 {
		return session.Session{}, fmt.Errorf("session not found: %s", id)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "session ID '%s' is ambiguous. Matches:\n\n", id)
	for _, s := range matches {
		fmt.Fprintf(&sb, "  %s  %s\n", session.HashID(s.ID), s.Title)
	}
	return session.Session{}, fmt.Errorf("%s", sb.String())
}

// parseDurationFlexible parses a duration string that can be a plain integer
// (interpreted as seconds) or a Go duration string (e.g. "30s", "5m", "1h").
func parseDurationFlexible(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(s)
}

// defaultHardKillTimeout is the default wall-clock cap of `rush run` when the
// operator does NOT pass --timeout (or passes --timeout 0): a graceful deadline
// like --timeout (installRunDeadline), whose hard-kill grace is the zombie
// backstop. 6h is long enough for ordinary work and short enough that a wedged
// process does not hold its session lock across days; a longer wait (a soak
// test) needs an explicit --timeout.
const defaultHardKillTimeout = 6 * time.Hour

// idleTimeoutDisabledSentinel stands in for "no inactivity backstop" when
// the operator passes --idle-timeout 0: the underlying stream watchdog has
// no native off-switch, only a threshold, so a duration long enough to
// never practically fire (well short of overflowing time.Time arithmetic)
// serves the same purpose. The run is still eventually bounded by
// --timeout (if set) and the default wall-clock cap.
const idleTimeoutDisabledSentinel = 100 * 365 * 24 * time.Hour

// resolveDefaultHardTimeout resolves the default hard-kill backstop duration
// from the RUSH_RUN_DEFAULT_HARD_TIMEOUT env var, falling back to
// defaultHardKillTimeout when the env var is unset, empty, or holds an
// unparseable / non-positive value. It NEVER returns an error: a malformed env
// var must not error out a run, it just silently falls back to the hardcoded
// default. Extracted as a pure function so it is directly unit-testable.
func resolveDefaultHardTimeout(envVal string) time.Duration {
	envVal = strings.TrimSpace(envVal)
	if envVal == "" {
		return defaultHardKillTimeout
	}
	d, err := parseDurationFlexible(envVal)
	if err != nil || d <= 0 {
		return defaultHardKillTimeout
	}
	return d
}

// parseTokenCount parses a token count string with optional k/M suffix.
// e.g. "500000", "100k", "1M" → int64.
func parseTokenCount(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSpace(s)
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(s, "M") || strings.HasSuffix(s, "m"):
		multiplier = 1_000_000
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "K") || strings.HasSuffix(s, "k"):
		multiplier = 1_000
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid token count %q", s)
	}
	return n * multiplier, nil
}
