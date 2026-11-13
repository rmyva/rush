# Rush

**Rush is a coding agent you run from the command line — but built
to be driven by *other* AI agents, not by a human typing into a
terminal.** Point an orchestrator (Claude Code, your own LLM wrapper,
a CI pipeline, a multi-agent fleet) at `rush run`, and it gets a model-agnostic, wrapper-stable JSON envelope back. Multiple runs can use separate sessions against the same repository, but Rush's session/database coordination does not isolate or serialize workspace-file edits; use separate worktrees or sandboxes for overlapping edits.

- **npm package:** [`@phpcraftdream/rush`](https://www.npmjs.com/package/@phpcraftdream/rush)
- **Source:** [github.com/PHPCraftdream/rush](https://github.com/PHPCraftdream/rush)

```bash
npm install -g @phpcraftdream/rush
rush run --role smart "summarize what this repo does"
```

## Remote API

Connect to a configured Rush server with `rush api` for sessions, runs, events, permission management and batch tasks (`rush api task`, `rush api batch`). See the [CLI reference](docs/api/cli.md). A token can be bound to one session or to batch intake only (`rush tokens create --pin-session`, `--batch`), and a listener can make batch intake its only work route (`rush serve --route batch`); a binding confines the API surface, not the agent. See [Token bindings](docs/api/bindings.md) and [Batch intake](docs/api/batches.md).

## What is this, and why

Most coding-agent CLIs are built for a human sitting in a terminal,
reading streamed output and clicking through permission prompts. This
one is built for the opposite case: **something else is the operator**
— another LLM, a script, a CI job — and it needs a subprocess it can
call over and over, unattended, and trust the output of.

Typical ways people actually use it:

- **Claude Code (or any other agent) delegating sub-tasks.** The
  top-level agent hits a chunk of work that's cheap, mechanical, or
  just doesn't need its own context window — it shells out to
  `rush run`, gets a JSON envelope back, and keeps going. See the
  `/rush` skill shipped with this fork for the exact pattern.
- **Fan-out over a codebase.** Multiple `rush run` invocations can use
  distinct sessions against a shared data directory. Session locks and
  SQLite protect session/store state; they do **not** isolate concurrent
  workspace-file writes. Use separate git worktrees or sandboxes when
  delegated work overlaps on files.
- **CI pipelines that need an LLM step.** A build step that asks a
  model to review a diff, write a migration, or fix a failing test —
  and needs a predictable exit code and a parseable result, not a
  chat transcript to eyeball.
- **Headless automation on a schedule.** Cron jobs, webhooks, queue
  workers — anything that needs "run this prompt against this repo and
  tell me what happened" as a function call, not an interactive
  session.
- **A human who still wants to look in.** The React/Tailwind web UI
  covers that case, but it's the second-class entry point here — the
  CLI and its JSON contract are what this project is actually built
  around.

If what you actually want is a polished terminal UI for a human to
chat with an agent in, that's [upstream `charmbracelet/crush`](https://github.com/charmbracelet/crush)
— this repository started as a fork of it and has since diverged
substantially in that direction (no TUI, no upstream server model,
different provider/session engine). See `CHANGELOG.fork.md` for the
full list of what changed and why.

## What this fork actually is

The product is **`rush` as an agent's hands**, not as a human's coding
companion. Every divergence below follows from that single repositioning:

- The TUI is gone — a human is no longer the primary user of the
  process. A React/Tailwind **web UI** stays for the cases where a
  human DOES want to look in, but the design centre is the CLI.
- The CLI is the contract. `rush run` exposes a **wrapper-stable JSON
  envelope** with a small, frozen set of fields an orchestrator parses
  without surprises. New flags (`--role`, `--session`, `--format`,
  `--agents`, `--timeout`, …) all exist to give the upper LLM precise
  control over a delegated turn.
- Separate sessions can share one data directory, but session/store
  coordination does not isolate workspace-file edits; use separate
  worktrees when parallel work overlaps on files.
- Honest error reporting. When the model fails its contract (returns
  invalid JSON, runs out of context, stalls the stream) the envelope
  says so — there is no silent success because the agent on top
  cannot read the operator's mind.
- Bootstrap helpers (`rush claude-init`, `rush codex-init`, and the other
  `*-init` commands) install Rush delegation commands for supported CLI
  orchestrators. Install scope is global by default; `--local` or `--cwd`
  selects a project. After updating Rush, use the matching new binary's
  targeted init command to refresh explicitly chosen integrations. See
  [delegation installation](docs/delegation-installation.md) for paths and
  ownership limits.

The browser UI is the second-class entry point for humans peeking in,
the orchestrator-facing CLI is first-class.

| Area | Upstream | This fork |
| ---- | -------- | --------- |
| Primary user | Human in a terminal | Another agent (LLM, CI, orchestrator) calling the CLI |
| Front-end | Bubble Tea TUI (~495 files under `internal/ui/`) | React/Tailwind SPA in `web/`, embedded via `go:embed`. Optional. |
| Transport | REST `/v1/...` over Unix socket / Windows named pipe | WebSocket `/ws` over TCP loopback (single embedded server) |
| Auth | None (local-socket trust) | Token-based, see `internal/server/auth.go` |
| Sessions | One model per agent role, set globally | Per-session model overrides + per-session system prompt, all persisted in SQLite |
| Permissions | In-memory rules during a TUI run | Persistent per-session rules in SQLite; cross-process visible |
| Parallel runs | Not a target | Separate sessions with shared data; session locks and SQLite protect store state, not overlapping workspace-file edits (use separate worktrees or another isolation boundary). |
| `crush run` | Single-shot quick fire | Wrapper-friendly: `--role`, `--session` get-or-create, `--json`/`--format`/`--agents`/`--timeout`/`--stream`, JSON-envelope validation, `assistant_notes`, fallback error messages |
| CLI providers | Limited bridge | npx Claude Code, Gemini CLI, Codex CLI, MCP bridge for external tools, session resume for Anthropic prompt caching; Haiku available as `local-cli/cli-claude-haiku` (200k ctx, `@low\|medium\|high` effort) |
| Web UI features | n/a | Slash-command + skill autocomplete, dark/light theme, pinned messages, fork-session button, LSP/MCP/provider management modals, file/image attachments |

The full per-file decision log lives in [`CHANGELOG.fork.md`](./CHANGELOG.fork.md).
That document is also the survival guide for merging upstream `main`
into the fork — every divergence is annotated with a `// Fork patch:`
comment in the code so conflicts surface at the right line.

## Batch runtime rollout and data

Batch execution is the current runtime, unconditionally since the H4 cutover;
there is no legacy selector. Active data uses `.rush/rush_store_v1.db` with one embedded
initial migration. On first startup, durable history, settings, queues and
schedules are imported once from the untouched `.rush/rush.db` before the new
destination is opened. Old and preparation-era execution/control state is
discarded, not resumed. An existing destination always wins—even if malformed—
and import errors do not fall back to the old database.

For import, stop old processes and automatic restarts for the full quiescence
window; host enumeration is not a registration fence. Drain and stop every
preparation host before upgrading. Pre-SF bare-UUID owner-stop fences cannot
safely have their provenance inferred; there is no hot-continuity guarantee.
Requeued imported work may repeat external side effects, so this is not an
exactly-once execution promise. For rollback, stop new hosts and run the previous
binary against untouched `.rush/rush.db`: this restores the old snapshot, not
post-import activity. Preserve `rush_store_v1.db` and its sidecars; there is no
reverse importer or merge-back. See [job-batch rollout and controls](docs/jobbatch.md)
and [session operations](docs/session-operations.md).

Agent finalization is an opt-in brake for batch runs: with `options.finalization.enabled: true` a batch agent's final answer is held as a draft until its children finish, reaction debts settle and there is no cancellation or open mutation; the default (flag absent or false) is the exact pre-feature behavior. See [agent finalization](docs/agent-finalization.md).

## Running an experimental build side by side

`rush --global-dir <DIR>` (or the `RUSH_GLOBAL_DIR` environment variable; the flag wins) relocates ALL Rush-owned global state under `DIR` with a fixed layout: `DIR/config` (config, Rush-owned skills/commands), `DIR/data` (data file, OAuth journals, provider/model-catalog caches, projects.json, audit logs, heartbeat, bin) and `DIR/cache`. When set, `RUSH_GLOBAL_CONFIG`, `RUSH_GLOBAL_DATA`, `RUSH_CACHE_DIR` and `XDG_*` are ignored for Rush-owned global paths, so an experimental build never touches a production install's state. Things belonging to other tools deliberately do NOT move: `~/.claude`, `~/.agents/skills`, `agents/skills`, `~/.aws`, the GitHub Copilot CLI token file, and the legacy `crush.*` migration source paths.

```bash
rush --global-dir ~/rush-experimental run --role smart "try the new provider config"
```

The experimental instance needs its own login: run `rush providers login` (or configure API keys) inside the root. NEVER copy an OAuth refresh token from the stable instance into the experimental one — refresh-token rotation by either instance invalidates the other's token, which is exactly what the isolation prevents. The local-cli providers (claude/gemini/codex) use those CLIs' own credentials, which live outside Rush's control and are shared by design.

## Protecting settings

Use `rush lock <password>` to protect global settings, or `rush lock --local <password>` for the current workspace; use the matching `rush unlock` command to remove protection. A local protection applies to that workspace, while a global protection takes priority. `--password <password>` authorizes settings changes for a command. Do not expose this value to agent sessions: command-line arguments may appear in process listings, shell history, or transcripts. Models can only be changed with the `rush models` commands; direct edits of the settings file are detected at start-up. `rush web --password <password>` authorizes settings changes from every web client connected to that server for the server's lifetime. A forgotten password cannot be reset by rush; the owner of the machine can only remove the protection by hand from the settings file whose path `rush lock` printed. That is an operator action: agents are told on every refusal to ask the user instead. The stored protection value is a salted PBKDF2-SHA-256 hash of the password (600000 iterations), not the password or an unsalted digest; protection values written by older versions are not recognized — remove the stored value from the settings file by hand and run `rush lock` again.

When a command reports an unrecognized or legacy settings lock, it names the settings file that holds it: remove the `sync_rev` entry from that file by hand, then run `rush lock` again.

## Security — `rush run` permissions

`rush run` has no interactive approval prompt. By default, non-interactive runs
auto-approve permission requests; opt into enforceable deny-by-default gating
with `permissions.run.restrict: true`, then scope access with the config
allowlists (`run.allow_tools` / `run.allow_bash`). The global
`permissions.allowed_tools` list is checked
before this gate and can bypass it (listing `bash` approves every bash command).
See [Restricting `rush run`](#restricting-rush-run) for details.

This tool gate is not an OS sandbox or arbitrary filesystem boundary. `--cwd`
selects the working directory; a worktree is an operational boundary, not a
sandbox guarantee. Neither prevents access outside that location, and
restricted-run allowlisting does not contain filesystem access by allowed
commands. Use an
OS-level sandbox when containment is required.

## Running Rush in this fork

Two complementary entry points; pick whichever fits the job.

### 1. `rush web` — the browser UI

```bash
rush web                            # default port + open browser
rush web --port 8080 --no-open      # for a remote workstation
```

A long-lived process. Sessions live in `.rush/rush_store_v1.db`; the UI loads
the React bundle from inside the binary, the WebSocket is local-only +
token-authed. This replaces upstream's TUI.

### 2. `rush run` — the orchestration CLI (the main thing)

The canonical pattern an orchestrator should be writing:

```bash
out=/tmp/audit-A.json
RUSH_FORBID_WRITES="$out" \
  rush run --role smart --session "audit-A" \
            --json --format json \
            < /tmp/audit-A.prompt > "$out" 2>"$out.err"
jq -r '.exit_reason' "$out"   # "end_turn" on success, "invalid_json" if model broke contract, "error" otherwise
jq -r '.final_text'   "$out"  # the raw JSON the model produced (validated)
jq -r '.assistant_notes' "$out" # any prose preamble that was stripped
jq -r '.error' "$out"         # error.message if non-success
```

#### Flags

- **`--role` (required)** — four slots exist: `smart` (the
  strong default; combined with a configured worker model this also
  triggers orchestrator mode — see below), `fast` (the cheap
  slot), `worker` (optional, no alias, cheap slot for delegated
  hands-on sub-task work — reachable directly via `--role worker`, or
  indirectly when a worker is configured and a `--role smart` run
  dispatches a sub-agent via the `agent` tool), and `reviewer`
  (optional, no alias, the strongest slot, for explicit review
  invocations — never auto-selected as the run's role, but note the
  reviewer auto-pass below, which uses the configured reviewer model
  for an automatic follow-up turn after a successful `--role smart`
  run). `worker`/`reviewer` are
  configured via the web UI or the config file's `models.worker` /
  `models.reviewer` (`rush models use` manages smart/fast; see
  `--worker`/`--reviewer` flags below for the other two). No silent
  default to the expensive model either way.
- **`--session <id>`** — get-or-create. Pass the same id again to
  continue, or a new id to start fresh. Works as a stable key for CI
  matrices and orchestrator wrappers.
- **`--json`** — emits a single wrapper-stable envelope on stdout:
  `{session_id, exit_reason, final_text, assistant_notes, stripped_bytes,
  tool_calls, usage, duration_ms, error, warnings}`.
- **`--format json | json-schema:<f> | @<f> | <any text>`** — appends a
  per-turn output-shape hint to the prompt AND post-validates `final_text`.
  With `json` or `json-schema:`, the envelope is also post-processed:
  markdown fences and prose preamble are stripped; `json.Valid` runs on
  what remains. **If the model returns syntactically broken JSON**
  (e.g. forgot a `]` somewhere), `exit_reason="invalid_json"` is set,
  the original (unstripped) text is preserved in `final_text`, the
  failed strip attempt goes to `assistant_notes`, and `error` carries
  a `json.SyntaxError` with a byte offset. Wrappers can branch on
  `exit_reason` instead of trusting the model's optimistic `"stop"`.
- **`--agents single | with-agents | agent-allow`** — sub-agent fan-out
  policy. Unset or `single` normally removes `agent` and `agentic_fetch`.
  A configured worker model on a `--role smart` run overrides that ban
  for the `agent` tool (including with explicit `--agents single`), so
  this flag is not a hard no-delegation guarantee in that configuration;
  `agentic_fetch` remains disabled. Without that worker override,
  `with-agents` nudges fan-out and `agent-allow` enables the tools without
  a nudge.
- **`--aggregation summary | concat | attach`** — how sub-agent fan-out
  output reaches the orchestrator. `summary` (default) lets the parent
  compose a wrap-up; detail lives in the DB only. `concat` adds a
  prompt nudge so the parent includes each sub-agent's reply verbatim
  in `final_text`. `attach` collects each sub-agent's last assistant
  text into `envelope.sub_agent_outputs` so the orchestrator gets the
  structured set; `final_text` becomes a brief wrap-up. An always-on
  warning fires in `envelope.warnings` when parent collapses sub-agent
  outputs to <40% of their combined character count, regardless of
  which mode is in use.
- **`--timeout-extends-on-progress`** — when set, the stream watchdog resets
  its idle deadline whenever streaming activity occurs. This affects the
  per-turn stream watchdog only, not the total run deadline; see the
  [`--timeout` total run cap below](#run-time-cap).
- **`--timeout-hard-cap <duration>`** — maximum wall-clock time the stream
  watchdog allows even with progress. Without this watchdog cap, progress can
  keep that watchdog from firing; the whole run is still bounded by `--timeout`
  or the default cap below.
- **`--on-finish "cmd"`** — runs a shell command after the agent finishes.
  Environment variables set for it: `RUSH_SESSION_ID`, `RUSH_EXIT_REASON`,
  `RUSH_COST_USD`, `RUSH_TOKENS`, `RUSH_DURATION_SEC`. Hook errors are
  printed to stderr but do not affect the exit code.
- **`--allow-peak-hours`** — bypasses a provider's configured
  `peak_hours` refusal window for this single invocation only. No
  persistent config-level equivalent exists; the override is conscious
  and one-off by design. **Never add this flag on an orchestrating
  agent's own initiative** — only when a human operator has explicitly
  asked, in that specific request, to override peak hours.
- **`--system-prompt[-file]`** — persists onto the session so follow-up
  runs inherit it.
- **`--stream`** — streams every token to stdout for live wrappers.
- **`--idle-timeout <duration>`** (default `15m`) — ends the run if the
  agent goes quiet for this long during a turn (no streamed output, no
  tool call/result; a long-running tool call is not "quiet"). Terminal —
  unlike a plain provider stall elsewhere, it never silently retries.
  It watches turns only: a run waiting between turns on a job, a
  delegation or a retry pause is not covered by it. Usually left alone.
- <a id="run-time-cap"></a>**`--timeout <duration>`** (default `0`, meaning no explicit
  deadline) — sets a fixed wall-clock cap for the whole run. When unset or
  zero, the 6 h default wall-clock cap is six hours total elapsed time, including active
  healthy work and time spent waiting on delegations or jobs; progress does not
  reset it. Override the default with `RUSH_RUN_DEFAULT_HARD_TIMEOUT` (seconds
  or a Go duration such as `30m`/`2h`; invalid or non-positive values fall back
  to 6 hours). At the deadline Rush requests cancellation and attempts normal
  shutdown. If the process follows that path, the run reports `timeout` (exit
  1, with an envelope); graceful shutdown and an envelope are not guaranteed.
  If the process is still alive 60 seconds after the deadline, it is
  force-killed with exit 124. On the graceful path, the envelope's `error`
  names the effective duration and source, and `resume_command` preserves the
  role and session; stderr prints the error, exact command and stored-work
  hint. Partial work remains available for continuation. **Compatibility:**
  owned deadlines previously reported `canceled`; now they report `timeout`.
  Ctrl-C, caller cancellation and parent deadlines still report `canceled`. An
  inner provider/reviewer deadline while the run context is live is not a run
  timeout.
- **`--worktree <slug>`** (+ `--base`/`--branch`) — run inside an
  isolated git worktree under `<main>/worktrees/<slug>` instead of the
  current checkout. Creation is idempotent (an existing matching
  worktree is reused), the run's output is mirrored into
  `<main>/.rush/stdin/<session>.{out,err}` so results survive
  `rush worktree remove`, and `rush worktree list|remove` manage the
  worktrees. Incompatible with `--cwd` and `--data-dir`. See
  [Worktree delegation](#worktree-delegation-worktree) below and
  [docs/worktree-run.md](docs/worktree-run.md).

#### <a id="worktree-delegation-worktree"></a>Worktree delegation (`--worktree`)

One command instead of the manual chain. Before, an orchestrator had
to run `git worktree add -b <slug> worktrees/<slug> <base>`, `cd` into
it, invoke `rush run ... < prompt > main/.rush/stdin/<s>.out 2> .err`
by hand, and afterwards do a manual `git worktree remove` plus branch
cleanup — which on Windows routinely leaves locked directories behind.
`--worktree` does all of it atomically and cleans up honestly:

```bash
rush run --role smart --worktree "task-a" --json "refactor the parser" \
  > /tmp/out.json 2>/tmp/out.err
jq -r '.exit_reason' /tmp/out.json
rush worktree list
rush worktree remove "task-a"
```

The redirection above is optional: stdout (the `--json` envelope, or
the terse final answer) and everything on stderr are ALSO mirrored
into `<main>/.rush/stdin/task-a.{out,err}` (each capped at the last
8 MiB), so results survive `rush worktree remove`.

- **Slug rules & containment** — the slug must match
  `[a-z0-9][a-z0-9._-]*`: no uppercase, no leading punctuation, no
  separators, no `..`; Windows reserved device names (`con`, `prn`,
  `aux`, `nul`, `com1-9`, `lpt1-9`) are rejected. A worktree can
  therefore only ever be created under `<main>/worktrees/`. The main
  checkout is resolved via git, so this works when rush itself runs
  inside a linked worktree.
- **Creation & reuse** — branch `<branch|slug>` is created from
  `--base` (default: the main checkout's HEAD); if the branch already
  exists locally it is checked out instead of created (then `--base`
  is ignored with a note). Idempotent: a registered worktree whose
  branch matches is reused (`reusing worktree <path> (branch <b>)` on
  stderr). A path occupied by anything else, a detached worktree, or a
  branch mismatch (which names both branches and suggests `--branch`)
  are clean errors.
- **Session** — if `--session` is not given, the session id is the
  slug. Inside a linked worktree rush already resolves the data
  directory to the main checkout's shared `<main>/.rush` (production
  binaries; dev builds isolate to `<wt>/.rush/dev`), so sessions live
  with the project.
- **Ignore warning** — if `worktrees/` is not git-ignored (`git
  check-ignore`), a warning is printed to stderr; rush never edits
  `.gitignore` itself.
- **Flag conflicts** — `--worktree` is incompatible with `--cwd` and
  `--data-dir` (clean errors); `--base`/`--branch` without `--worktree`
  is an error. All other run flags (`--json`, `--role`, `--session`,
  `--timeout`, ...) work unchanged.
- **`rush worktree list`** — SLUG / BRANCH / SESSION / PATH table for
  every registered worktree directly under `<main>/worktrees`; SESSION
  shows the id of a session whose lock is held right now or `-`.
- **`rush worktree remove <slug> [--keep-branch] [--force]`** —
  refuses while a live run holds a session lock for this worktree (the
  slug-named session in `<main>/.rush`, or any lock in
  `<wt>/.rush/locks`); **this refusal holds even with `--force`** —
  stop the run first (`rush sessions kill <id>`). Refuses a dirty
  worktree (uncommitted or untracked changes) without `--force`.
  Branch policy: default deletes with `git branch -d` (merged only —
  an unmerged branch is kept with a stderr notice and the command
  still succeeds, exit 0); `--force` deletes with `-D` (unmerged
  commits discarded); `--keep-branch` skips deletion; detached
  worktrees have no branch to delete. On Windows removal retries
  while AV/indexers hold the directory and, if a process still holds
  it, honestly reports what remains on disk instead of claiming
  success. Works from any checkout of the repo.

#### Session liveness, batch state, and completion

For safe retry, require physical process exit and no live or unknown batch scope; inspect current roots/nodes rather than relying on session-only `why`/`watch` or `end_turn`. Resume in the original checkout with the original role and session. `rush sessions compact` has two meanings: without a session argument it runs SQLite `VACUUM`; with a session ID it summarizes that session's model history.

`rush sessions status <id> [--json]` answers all of this in one read-only, machine-readable snapshot: process liveness (a real OS probe, not a recorded fact), phase, seconds since real activity (sub-agent tree included), live children, and whether the session's batch work is terminal. It combines what `why`, `locks`, `last` and `watch` each show partially. Its honesty rule: anything the evidence cannot establish is the literal value `"unknown"` instead of a guess, and a recorded `ended_reason` is never treated as proof that a process exited. Field reference and guarantees: [sessions status](docs/sessions-status.md).

For a diagnosis rather than a verdict, the snapshot commands are available
individually: `rush sessions why <id>` prints a combined session/batch
evidence snapshot (session lock, batch root, host locks under `.rush/hosts`,
live-work classification such as `batch live` / `batch unknown` / `crashed`);
`rush sessions locks` lists the lock files; `rush sessions watch <id>` shows a
live activity view. None of `why`, `locks`, `watch` or `show` proves that a
process physically exited — `why` saying `crashed` or `locks` saying
`offline` is evidence, never process-exit proof; only the launched process's
completion establishes that. `sessions status` above is the one-shot
machine-readable snapshot combining them.

Model-context compaction summarizes existing session history, distinct from SQLite maintenance. In an ordinary direct Codex run/resume, only an exact structured `context_length_exceeded` certified before any output may trigger one silent-prefix summary and continuation in the same session; partial, opaque, uncertified output, transport errors, and NoReplay-fenced failures are never replayed. See [session operations](docs/session-operations.md) for recovery limits and [session compaction](docs/sessions-compact.md) for the explicit command.

The batch controls identify work directly. `batch_run` returns the batch
`root_id`; `batch_status` reports the root and its task `node_id`s.
`batch_output` takes a `root_id`, a task `node_id` of that root and a
`cursor`, and serves best-effort output from the detached cache (10-minute
TTL; pressure eviction or process exit may end it sooner). `batch_stop`
takes a `root_id`, or a `root_id` plus a `node_id` to stop one descendant.
An accepted `batch_stop` is a cancellation intent, not a termination
confirmation. The legacy `job_output` / `job_kill` controls are removed in
the batch-only runtime. See
[batch-runtime-migration.md](docs/batch-runtime-migration.md) for the
native replacements and how stale names behave in `AllowedTools`,
`permissions.allowed_tools` and `options.disabled_tools`.

#### Envelope fields worth knowing

- `stripped_bytes` — how many bytes were removed from `final_text` by
  the JSON stripper (when `--json`+`--format json` were active). Graph
  it across runs to track how often your model wraps in prose.
- `tool_calls: [{name, count}]` — post-hoc inventory of what tools the
  model actually used. Useful to verify `--agents single` actually
  blocked fan-out.
- `sub_agent_outputs[]` — present only with `--aggregation attach`.
  Each entry is `{session_id, title, final_text, char_count}` for one
  sub-session the parent's `agent` tool dispatched during this run.
- `warnings[]` — non-fatal observations. Includes `final_text appears
  truncated` when the run errored mid-composition (so the operator
  sees the model was about to continue); `final_text is empty after N
  sub-agent fan-out call(s)` when the model dispatched sub-agents but
  never composed a top-level reply; and `reduction-loss: final_text
  is X% of N combined sub-agent chars` when the parent over-summarised
  (re-run with `--aggregation=attach` or `concat` to recover).
- `resume_command` — present for `timeout` and `provider_limit`; preserves
  the run's role and session ID. Timeout `error` identifies the limit/source.
- `error` — present whenever `exit_reason` is non-success. If the
  provider's Finish part had no message (some providers emit a bare
  error finish), a fallback names the most likely causes (provider
  HTTP error, stream stall, OOM, context overflow). One `exit_reason`
  value is a **deliberate stop, not a failure**: `"awaiting_answer"`,
  set when the model called `ask_question` — see below.
- `recovered_partial` — present when the session had an orphaned
  partial assistant message from a previous interrupted run (detected
  by `Finish{Partial: true}` on an unfinished row). Shape:
  `{message_id, chars, last_flush_at, text}`. An always-on WARN in
  `warnings[]` fires when this field is populated: *"recovered N chars
  of partial assistant text — model run was interrupted"*. The text
  may be incomplete but is usually the bulk of what the model produced
  before the kill.

#### Pausing mid-turn: `ask_question` and orchestrator mode

Because `rush run` has no synchronous way to block mid-turn for an answer, calling `ask_question` can force-finish the turn only when no live own jobs or delegations would be orphaned; otherwise the tool returns a hint and the run continues. When it exits with `exit_reason: "awaiting_answer"`, that is an intentional continuation point, not a failure: resume the same session with the original role and checkout, as shown below.

```bash
rush run --json --role smart --session "deploy-1" "deploy the release" > out.json
jq -r '.exit_reason' out.json   # "awaiting_answer"
jq -r '.error'       out.json   # question + suggested options + resume command
```

- `exit_reason: "awaiting_answer"` is **not a failure** — treat it like
  a normal continuation point, not something to retry.
- The question, suggested options, and the exact resume command live in
  `.error` (not `.final_text`).
- Resume with the role preserved, e.g. `rush run --role smart --session <id> "<your answer>"` — **not**
  `rush sessions inject`, since the process already exited. The stopped run's
  actual role (smart, fast, worker, or reviewer) is included in its resume
  command; if the role is unknown, guidance omits the command and asks you to
  choose a supported role rather than suggesting a rejected invocation.

**Orchestrator mode:** when `--role smart` is used and a `worker` model
is configured, the smart agent's system prompt gains an "Orchestrator
mode" instruction: understand the task's shape, but delegate hands-on
work (editing, writing, running commands) to the `agent` tool in
worker-context-sized chunks instead of implementing inline — one file
or logical change per delegation, with enough standalone context since
the worker doesn't see the parent conversation.

**Reviewer auto-pass:** when a `reviewer` model is configured and a `--role smart` run finishes cleanly, Rush automatically continues the same session with one read-only reviewer turn. The executor's answer remains `final_text`; the reviewer's conclusion is added in the `review` field and its parsed first line in `review_verdict`. The verdict is advisory and does not change the run's exit code. Runs that failed, were canceled, timed out, or queued are returned unchanged; an explicit `--role reviewer` invocation is never extended.

A worker sub-agent can itself call `ask_question` and pause. That does
**not** end the orchestrator's turn — it surfaces as a normal,
non-error tool result such as `SUB-AGENT QUESTION (session <id>): <question>`.
For a legacy child paused between turns, answer by calling `agent` again
with `resume_session_id="<id>"` and the answer as the prompt. If
`inspect_agent` reports a current batch `held_question`, do **not**
resume or re-delegate: answer the held ask in that same batch execution
with `inject_agent(child_session_id="<exact child id>", message="<answer>")`.
Resuming in that case queues a new run behind the held ask. See
[session operations](docs/session-operations.md).

#### Env-vars to know

- **`RUSH_FORBID_WRITES`** — comma-separated paths the `write`/`edit`/
  `multiedit` tools must NOT touch. **Set this to the stdout-redirect
  target before every `rush run`** — otherwise the model can pick the
  same filename it sees in the prompt and overwrite your envelope
  output. Tool calls to forbidden paths fail visibly to the model;
  it then falls back to returning content via `final_text`.
- **`RUSH_PROVIDER_CACHE_TTL`** — duration (`24h` default, `0s` to
  always refresh). Caches the Catwalk/Hyper provider catalog locally
  so `rush models show` and similar read-only commands skip the
  ~3-second HTTP round-trip when the on-disk cache is fresher than
  the TTL.
- **`RUSH_COLOR_SCHEME`** — `light` \| `dark` \| `auto` (default
  `auto`). Forces the CLI help/error color palette onto a light or
  dark background, working around unreliable terminal light/dark
  auto-detection. The auto path queries the terminal's background
  color with an OSC 11 escape sequence and a hard 2-second timeout;
  if the terminal doesn't reply in time (or stdin/stdout aren't both
  real TTYs, e.g. when an orchestrator spawns `rush` with redirected
  stdin), lipgloss's `HasDarkBackground` **falls back to assuming a
  dark background** — so on a light-themed terminal the help renders
  grey-on-white with low contrast. This has been reported on WezTerm
  on Windows. Set `RUSH_COLOR_SCHEME=light` (or pass
  `--color-scheme light`, which is global and wins over the env var)
  to force the light palette.
  > **Windows gotcha:** `setx RUSH_COLOR_SCHEME light` only writes the
  > variable to the registry for *future* processes — it does **not**
  > update any terminal window that's already open. Open a new
  > terminal tab/window (or restart the shell) before checking whether
  > it took effect, or you'll see the old behavior and wrongly
  > conclude the flag doesn't work.

#### Reading a run's result: rush result

`rush run --json` writes a JSON result envelope. `rush result
<session-id|path>` prints it back as readable text without fighting Windows
codepages, and `rush run --findings-from <session-id|path>` feeds a previous
run's review findings into the next run without manual copy-paste. Full
details: [docs/result-command.md](docs/result-command.md).

The argument is a session id — looked up as
`<main-checkout>/.rush/stdin/<id>.out` via `git rev-parse --git-common-dir`,
so id lookup also works from inside a linked git worktree (it resolves the
file `rush run --worktree` writes) — or a direct path to an envelope file (a
path that exists always wins). Four modes:

```
rush result s1-review                    # human-readable text
rush result s1-review --review           # verdict + review only
rush result s1-review --field final_text # raw final_text for scripting
rush result s1-review --json | jq .exit_reason
rush result /abs/path/envelope.out       # explicit file
```

`--json` writes the stored bytes exactly (no re-marshalling); `--field`
prints string values raw (no quotes) and other types as JSON. Output is
always raw UTF-8 bytes on stdout regardless of the Windows console code page
(Go performs no ANSI/OEM transcoding on `os.Stdout`). Exit codes: 0 = envelope
read, non-zero = not found or not valid JSON.

`rush run --findings-from <session-id|path>` appends a `## Findings from
previous review` block (verdict, review text, warnings) read from another
run's envelope after the prompt; a missing or invalid envelope fails the run
before it starts. Typical loop:

```sh
rush run --role smart --session t1 --json "review the diff"   # (with --worktree, the envelope lands in <main>/.rush/stdin/t1.out)
rush result t1 --review                                       # read the verdict
rush run --role smart --session t2 --findings-from t1 "fix every finding"
```

`rush run --fail-on-review` gives a `review_verdict: "fail"` a dedicated
exit code instead of the default exit 0 (which makes background completion
notifications look successful): exit 3 when the run succeeded and the verdict
is exactly `"fail"`; `pass`/`pass_with_notes`, other verdicts and runs with
no reviewer pass stay 0, a failed run keeps its own exit code, and the stdout
envelope is byte-identical either way. Without the flag nothing changes.
`rush run` exit codes:

| Code | Meaning |
| --- | --- |
| 0 | success (including review-fail without the flag, and `pass`/`pass_with_notes` with `--fail-on-review`) |
| 1 | generic run error |
| 3 | review verdict `"fail"` with `--fail-on-review` (public contract for wrapper scripts; `rush ping` uses its own separate code 3 for "degraded") |
| 124 | timeout hard backstop force-kill |

### 3. Parallel processes against one `.rush/`

For a run assigned to a particular session, include its required role flag, for example:

```sh
rush run --role smart --session X "..."
```

Locks, SQLite and additive accounting protect Rush-owned state, but do not isolate concurrent workspace-file edits; for overlapping changes use [`rush run --worktree`](#worktree-delegation-worktree) or another isolation boundary.

- Per-session OS flock (`internal/session/lock.go`) — two processes
  cannot share a session id.
- SQLite WAL + `busy_timeout=30000` + single-writer-per-process
  connection pool.
- Cost mutations go through additive SQL (`IncrementSessionCost`) so
  concurrent sub-agent goroutines AND parallel processes cannot lose
  cost via read-modify-write.
- Atomic file writes (`fsext.AtomicWriteFile`) in `write`/`edit`/
  `multiedit` tools — `kill -9` mid-write cannot truncate the user's
  file.
- Per-process `pid=N` attribute in every log line — interleaved Windows
  log writes can be split post-hoc with `jq 'select(.pid==N)'`.
- Permission grants ("Always allow") are DB-direct on every check, so
  a grant made in process A is immediately visible in process B
  without restart.
- MCP `qwen/gemini-mcp-id` and `~/.{qwen,gemini}/settings.json` writes
  are flock'd with a 30s timeout so a wedged sibling cannot freeze
  the fleet.

See [`CHANGELOG.fork.md`](./CHANGELOG.fork.md) Section 4.I for the full
parallel-process audit and the trade-offs we explicitly kept (e.g. N
processes still spawn N stdio children of every configured MCP server
— use HTTP/SSE-transport MCPs in parallel runs).

#### Injecting a message into a running session from another process

```bash
rush sessions inject <session-id> -m "also update the changelog"
rush sessions inject <session-id> -f ./notes/next-step.md
rush sessions inject <session-id> -m "stop, wrong approach" --interrupt
rush sessions inject 8a3f0c -m "continue" --json
```

Use this when a `rush run --session X` you already launched (from an
orchestrator, another terminal, or a `/rush` sub-agent) is mid-turn
and you want to hand it new information without killing it. `<id>`
accepts a full session id or the short hash printed by `sessions
list`.

- The message is persisted immediately as a normal user message —
  Role `user`, same as if it were typed — so it renders in the web UI
  exactly like anything the operator sends themselves.
- By default (no `--interrupt`) it merges into the session's next
  provider request without cancelling the in-flight turn — same
  latency as the web UI's non-stopping inject.
- With `--interrupt` the running turn is cancelled and immediately
  restarted with the new message, mirroring the web UI's
  interrupt-and-send.
- If no process is currently running the session, the message is
  still persisted and picked up the next time the session runs; the
  command tells you so instead of failing.
- A `rush run` loop waiting between turns (on a job, a delegation or a
  retry) counts as running the session: the result says `running:
  true` and `between_turns: true` (JSON), no turn is in flight to
  interrupt, and the message reaches the loop's next turn if it runs
  one (otherwise the session's next run).
- A session waiting between turns on its own running background job or a
  live delegation (the web case: no driver marker, no lock) counts as
  running too (`running: true`); the message reaches its next turn.

Delivery costs nothing at rest: `rush sessions inject` writes a
signal row to a `pending_injects` table, and the running process only
checks it at points it already visits on every turn (next provider
step for the merge case, a lightweight 3s ticker bound to the active
turn for `--interrupt`) — no standing poll loop, no open port.

### 4. Token & prompt-cache statistics

Every assistant message records its own token accounting, so you can ask
how much a model actually cost and how well the prompt cache is working:

```bash
rush sessions cache <session-id>          # one session, per model
rush sessions cache --by model            # every session, per model
rush sessions cache --since 7d --by day   # last week, day by day
rush sessions cache --since 30d --json    # machine-readable
```

Tokens are split into three **disjoint** classes, so the prompt size is
their sum: `INPUT` (fresh, full price), `READ` (served from the
provider's prompt cache, much cheaper) and `WRITE` (written into the
cache). `HIT` is `read / (input + read + write)`.

Grouping is by the model that **actually produced** each message, so a
session that switched models mid-conversation is split correctly. That
is the difference from `rush sessions cost`, which groups by the
session's *current* model and whose `TOKENS` column sums last-snapshot
session counters rather than real totals — the two read different
sources and are deliberately not merged into one table.

The output refuses to state numbers it cannot back up: `HIT` prints
`n/a` rather than `0%` when a provider does not report caching (a
fabricated zero is indistinguishable from a real miss), every table
reports its **coverage** when some messages have no usage recorded, and
`--by day` omits `HIT` entirely because a day can span providers whose
cache visibility differs.

### 5. Bootstrap helpers for an orchestrator

Install or refresh Rush delegation commands after updating the Rush binary. The embedded sources are `claude_slash_command.yaml` and `claude_crush_fallback_command.yaml` for `/rush` and `/rush-fallback`, plus `claude_wrush_command.md` and `claude_wcrush_command.md` for `/wrush` and `/wcrush`; `multi_cli_convert.go` renders them for each supported CLI. The commands manage only the artifacts in the table below. Use the matching `*-init` command for each explicitly selected integration and scope; do not infer ownership from a directory or recursively refresh installations. See [delegation installation](docs/delegation-installation.md) for file-level ownership behavior and refresh limits.

| CLI | Init command | Artifacts written (global default / project with `--local` or `--cwd`) |
| --- | --- | --- |
| Claude Code | `rush claude-init` | `~/.claude/commands/{rush,rush-fallback,wrush,wcrush}.md` / `<cwd>/.claude/commands/` |
| Codex CLI | `rush codex-init` | `~/.agents/skills/{rush,rush-fallback,wrush,wcrush}/SKILL.md` / `<cwd>/.agents/skills/` |
| OpenCode | `rush opencode-init` | `~/.config/opencode/commands/{rush,rush-fallback,wrush,wcrush}.md` / `<cwd>/.opencode/commands/` |
| Oh My Pi | `rush omp-init` | `~/.omp/agent/commands/{rush,rush-fallback,wrush,wcrush}.md` / `<cwd>/.omp/commands/` |
| Gemini CLI | `rush gemini-init` | `~/.gemini/commands/{rush,rush-fallback}.toml` / `<cwd>/.gemini/commands/` |
| Grok Build CLI | `rush grok-init` | `~/.grok/skills/{rush,rush-fallback}/SKILL.md` / `<cwd>/.grok/skills/` |
| Qwen Code CLI | `rush qwen-init` | `~/.qwen/commands/{rush,rush-fallback}.md` / `<cwd>/.qwen/commands/` |

The default is global; `--local` or `--cwd <project>` selects project scope (`--cwd` implies local). `--global` is explicit global scope. Existing artifacts containing Rush's ownership sentinel are replaced in full with the current embedded version; edits to those files are not merged. A same-name artifact without the sentinel is skipped with a warning, which may still result in exit status 0. The init commands do not install external per-model artifacts; see the separate `cc-arch-hands` installer for those.

`claude-init --local` also strips legacy Rush delegation blocks from that project's `CLAUDE.md`, and removes the file if stripping leaves it empty. Do not use it when the update must not touch `CLAUDE.md`; choose the global scope or make that side effect part of the explicit project target decision.

`cli-refresh` is not a substitute for refreshing every installer. It covers only Claude, Codex, Gemini, Grok, and Qwen (not OpenCode or OMP); moreover, its Claude path writes and removes only `/rush` and `/rush-fallback`, not `/wrush` or `/wcrush`. Use each intended `*-init` after the binary update instead.

### 6. `rush models` — picking and inspecting models

Four model slots exist: `smart`/`fast` (the pair every
`rush run` uses by default) plus two optional ones, `worker` (cheap
slot for delegated sub-task work — see orchestrator mode above) and
`reviewer` (strongest slot, explicit-only). Commands covering the surface:

```bash
rush models list             # show available atoms + raw provider/model ids (reads cache; no network)
rush models list --refresh   # force a network refresh of provider data before listing
rush models use <smart> <fast> [--worker <atom>] [--reviewer <atom>] [--global | --local]
rush models use --fast <atom>   # set just one slot — --smart/--fast/--worker/--reviewer are all independent
rush models state             # what's effective + per-scope breakdown (alias: `show`)
rush models efforts [model]   # explain reasoning-effort levels and how to set them
rush models bump <role> up|down  # step a role's effort by one level
rush models unset [smart|fast|worker|reviewer|both|all] [--global|--local]
```

> **No side effects by default:** `rush models list` reads the on-disk
> provider cache (or the embedded provider list bundled with Rush when
> no cache exists yet) and does NOT trigger a network fetch or write any
> cache files. Pass `--refresh` to force a fresh fetch from Catwalk and
> Hyper before rendering. The output shape (text and `--json`) is
> identical in both modes.

**Atoms** are short, friendly aliases. `list` prints them filtered by your
currently-enabled providers — disabled providers' atoms are hidden so the
list only shows what actually works right now:

```
ATOMS (combine as `rush models use <smart> <fast>`):

  Anthropic:
    via local `claude` CLI
    opus-low, opus-medium, opus-high, opus-xhigh, opus-max            Claude Opus    (1M ctx)
    sonnet-low, sonnet-medium, sonnet-high, sonnet-xhigh, sonnet-max  Claude Sonnet  (1M ctx)
    haiku-low, haiku-medium, haiku-high, haiku-xhigh, haiku-max       Claude Haiku   (200k ctx)

  Zai:
    glm5_3        GLM 5.3      (1M ctx)
    glm5_turbo    GLM 5 turbo  (200k ctx)
    ...
```

Anthropic atoms require a level suffix (`opus-high`, `sonnet-low`, etc.) —
the level list comes from parsing `claude --help` at first use (falls back
to a static `low/medium/high/xhigh/max` list if parsing fails).

### Model shortcodes

`rush models list` prints the generated model/effort table and a
`generated from cc-arch-hands <version>` footer.
Claude codes use **family-slot-effort**: `o/s/h/f` for Opus/Sonnet/Haiku/Fable,
no slot for top, `1` for previous, `2` for the next older entry, followed by
`l/m/h/x/xx` (low/medium/high/xhigh/max). For example, `ox` is top Opus
xhigh, `o3x` is Opus 4.7 xhigh, and `h1l` is pinned Haiku 4.5 low.
Codex uses **effort-family-slot**: `l/m/h/x/xx/u` followed by
`s/t/l/a` (Sol/Terra/Luna/Astra), then the optional older slot;
`hs` is top Sol high and `hs1` is the previous Sol high.

Top codes move with the catalog: **`ox` now means Opus 5.5, not 4.8**;
use `o2x` for 4.8. The legacy hand-written version codes
`o48*/o47*/o46*/s46*/s45*/h45*` were **removed** and are now rejected with an
error. Use the generated codes (`rush models list`), long-form atoms such as
`opus47-high`, or raw `local-cli/<slug>@<effort>`.
`hl` belongs to Haiku low; use **`local-cli/cli-codex-gpt-6-luna@high`** for GPT 6 Luna high.
In cc-arch-hands 0.16.1, `us1` selects GPT 6 Sol ultra and `ua` selects
GPT 6 Astra ultra. `ul1` was removed: Luna has no ultra effort.
Raw forms remain `local-cli/cli-codex-gpt-6-sol@ultra` and
`local-cli/cli-codex-gpt-6-astra@ultra`.

```bash
rush models use o3x h1l
rush models use ox hl --reviewer xxs
```

Plain `go build`, `go install`, and `go test` consume committed generated
files only, without refreshing the catalog. Artifact entrypoints
(`go run build.go`, npm publishing, and GoReleaser) refresh automatically
before compilation. Local refresh failures warn and keep committed outputs;
`CI=true` implies strict failure, and release pipelines explicitly refresh
with `-mode refresh -strict` (not check-only).
Manual refresh: `go run ./internal/tools/cahsync`; `-strict` makes failures
fatal, `-check -strict` detects drift without writing, and `-offline` skips
network and output I/O. Refresh never downgrades below the committed cah version. Successful refresh logs the source version and
changed paths. CI drift checking is a separate nonblocking warning job;
ordinary test/lint jobs are not coupled to catalog acquisition.

Z.AI atoms are **not** all effort-less: **GLM-5.3** (`glm5_3`) has 3 real
wire states (`off`/`high`/`max`) settable via the long-form suffix
(`glm5_3-max`) or raw `zai/glm-5.3@max` — one more
than every *other* Z.AI/GLM atom (5-turbo, 4.7, 4.6, ...), which
exposes only a boolean thinking toggle (`off`/`on`). Both forms are
validated against the atom's real levels; `rush models efforts <model>`
prints the exact list and commands for any specific model. (GLM-5.3's
context window/levels are provisional — see the comment above its entry
in `internal/cmd/models_atoms.go`.) The web UI's model picker also shows
GLM-5.3: since docs.z.ai and the upstream catwalk provider registry don't
list it yet, `internal/config/load.go`'s `configureProviders` synthesizes
the same provisional entry into the Z.AI provider's model list (skipped
once catwalk or your own `providers.zai.models` config actually provides
one), so both the CLI atom and the web picker agree.

```bash
rush models use opus-high glm5_turbo                # mixed Anthropic smart + Z.AI fast
rush models use --local glm5_3 glm5_turbo           # workspace-only override
rush models use openai/gpt-5@high zai/glm-5-turbo   # raw provider/model fallback for anything not in the atom list

# Also set worker/reviewer in the same call (independent of smart/fast)
rush models use opus-high haiku-low --worker glm5_turbo --reviewer opus-max

# Change ONE slot only, leaving the other three exactly as they are —
# --smart/--fast work just like --worker/--reviewer always have. The two
# positional args and --smart/--fast are mutually exclusive per call.
rush models use --fast glm4_7_flash
rush models use --smart opus-high

# Discover effort levels for a specific model (or run with no arg for the
# full per-provider overview, including the Z.AI graduated-vs-boolean split)
rush models efforts glm5_3

# Step a role's effort by one level instead of retyping the full atom name
rush models bump reviewer up
rush models bump worker down --local
```

`models state` shows the currently-effective values for all four slots and
the per-scope breakdown so you always know whether your `--local` workspace
overrides your global default or vice versa.

**The cascade has a third level: session.** `--global`/`--local` (system/
workspace) both live in the config file (`rush.ktav`, or legacy `rush.json`)
and are what `models state` reports. On top of that, the **web UI** lets each open session pin its own
smart/fast/worker/reviewer, stored in that session's DB row, not in any
config file — so it's invisible to `models state` and to other sessions.
Resolution order is always **system → folder → session**: a session with no
override inherits whatever `models state` would show; setting one there
wins for that session only, and clearing it (the model picker's "Inherit"
entry, or the "Default models" modal's per-slot clear button) falls straight
back to the folder/system value. The web UI's header **"Default models"**
button opens a modal with three blocks — System, Folder, Session — each
showing all four slots: what's explicitly set at that level, or, when
unset, the inherited value and which level it's coming from.

<!-- stale-slot-ok: historical record of the removed command's exact spelling -->

> **Removed in batch 11:** `rush models set --large X --small Y` and the
> entire `rush models preset` subtree (save/use/list/delete). Both
> commands now print a redirect notice pointing at `rush models use`.

To clear an override and fall back to the other scope: `rush models unset
[smart|fast|worker|reviewer|both|all] [--local|--global]`. `both` (the
default when the arg is omitted) clears smart+fast only; `all` clears all
four slots. Missing keys are a no-op.

## When NOT to use this fork

- **You want the TUI experience.** Use upstream — the fork removed it.
- **You want a stable, blessed-by-Charm distribution path.** This fork
  does not publish Homebrew/winget/AUR releases.
- **You want the official REST `/v1/...` protocol for wrapping.** This
  fork speaks WebSocket only.
- **You're a human typing into one terminal session at a time.**
  Upstream's TUI is genuinely nicer for that. This fork's CLI is shaped
  for scripts and orchestrators; the web UI is for peeking in, not for
  daily-driving conversational work.

## When this fork is exactly the right tool

- You're building a multi-agent system where one LLM delegates code
  work to another. `rush run` is that worker; the envelope is the
  protocol between them.
- You run a multi-section audit / refactor / migration as 5+ parallel
  `rush run` invocations against one repo and need the cost
  accounting + lock-file + atomic-write guarantees that follow.
- You wrap LLMs in CI: stable `--session` key per build matrix, `--json`
  for jq-parseable output, `--format json` for raw JSON contracts with
  validation, `--timeout` when a run must fit a hard job-slot deadline.
- You want a long-running embedded coding agent reachable over a
  browser-served WebSocket from a thin React UI.

---

The original upstream README follows below, kept verbatim because most
of its content (installation, configuration, MCP/LSP setup, model
providers) applies unchanged to this fork. Where the fork diverges,
either the text above or `CHANGELOG.fork.md` overrides.

---

# Rush (upstream)

> Logo, release badge, build-status badge and demo GIF removed — they
> point at upstream `charmbracelet/crush` artifacts (Charm's logo,
> upstream's GitHub Actions status, upstream's release tag) and would
> misrepresent this fork's identity, release cadence and CI status.
> The text below is the upstream README's prose, kept verbatim because
> the installation / configuration / providers material applies to
> this fork unchanged.

## Features

- **Multi-Model:** choose from a wide range of LLMs or add your own via OpenAI- or Anthropic-compatible APIs
- **Flexible:** switch LLMs mid-session while preserving context
- **Session-Based:** maintain multiple work sessions and contexts per project
- **LSP-Enhanced:** Rush uses LSPs for additional context, just like you do
- **Extensible:** add capabilities via MCPs (`http`, `stdio`, and `sse`)
- **Works Everywhere:** first-class support in every terminal on macOS, Linux, Windows (PowerShell and WSL), Android, FreeBSD, OpenBSD, and NetBSD
- **Industrial Grade:** built on the Charm ecosystem, powering 25k+ applications, from leading open source projects to business-critical infrastructure

## Installation

This fork ships as an npm package:

```bash
npm install -g @phpcraftdream/rush
rush run --role smart "your prompt here"
```

Or build from source (requires Go 1.26+):

```bash
git clone https://github.com/PHPCraftdream/rush.git
cd rush
go build -o rush .
```

> [!NOTE]
> The package managers upstream `charmbracelet/crush` publishes through
> (Homebrew, Winget, Scoop, apt/yum via `repo.charm.sh`, the Nix NUR
> module, `go install github.com/charmbracelet/crush@latest`) install
> **the upstream project, not this fork** — this fork's Go module path is
> `github.com/PHPCraftdream/rush`, distinct from upstream. The npm package above
> and building from [this repository](https://github.com/PHPCraftdream/rush)
> directly are the two ways to get this fork specifically.

> [!WARNING]
> Productivity may increase when using Rush and you may find yourself nerd
> sniped when first using the application.

## Embedding Rush as a Go library

Prefer to call Rush from your own Go process instead of shelling out to
the CLI? See [`sdk/README.md`](sdk/README.md) for the embeddable SDK —
`sdk.Open` + `Client.Run` gets you a typed result envelope back, with
support for explicit per-call provider credentials, ephemeral in-memory
sessions, and concurrent multi-tenant use on one `Client`.

Application-mode SDK clients acquire an exclusive process-wide MCP owner, so
a second concurrent application-mode `sdk.Open` can fail with `ErrOwnerBusy`
(wrapped through `app.New`). Use `ModeLibrary` to skip MCP entirely; library
clients do not acquire that owner and may coexist with an application-mode
client or with one another. See the SDK lifecycle section for the exact
error-wrapping and shutdown contract.

## Getting Started

The quickest way to get started is to grab an API key for your preferred
provider such as Anthropic, OpenAI, Groq, OpenRouter, or Vercel AI Gateway and just start
Rush. You'll be prompted to enter your API key.

That said, you can also set environment variables for preferred providers.

| Environment Variable        | Provider                                           |
| --------------------------- | -------------------------------------------------- |
| `HYPER_API_KEY`             | Charm Hyper                                        |
| `ANTHROPIC_API_KEY`         | Anthropic                                          |
| `OPENAI_API_KEY`            | OpenAI                                             |
| `VERCEL_API_KEY`            | Vercel AI Gateway                                  |
| `GEMINI_API_KEY`            | Google Gemini                                      |
| `SYNTHETIC_API_KEY`         | Synthetic                                          |
| `ZAI_API_KEY`               | Z.ai                                               |
| `ZHIPU_API_KEY`             | Z.ai (fallback when `ZAI_API_KEY` is unset)        |
| `MINIMAX_API_KEY`           | MiniMax                                            |
| `HF_TOKEN`                  | Hugging Face Inference                             |
| `CEREBRAS_API_KEY`          | Cerebras                                           |
| `OPENROUTER_API_KEY`        | OpenRouter                                         |
| `IONET_API_KEY`             | io.net                                             |
| `ALIBABA_SINGAPORE_API_KEY` | Alibaba (Singapore)                                |
| `GROQ_API_KEY`              | Groq                                               |
| `AVIAN_API_KEY`             | Avian                                              |
| `OPENCODE_API_KEY`          | OpenCode Zen & Go                                  |
| `VERTEXAI_PROJECT`          | Google Cloud VertexAI (Gemini)                     |
| `VERTEXAI_LOCATION`         | Google Cloud VertexAI (Gemini)                     |
| `AWS_ACCESS_KEY_ID`         | Amazon Bedrock (Claude)                            |
| `AWS_SECRET_ACCESS_KEY`     | Amazon Bedrock (Claude)                            |
| `AWS_REGION`                | Amazon Bedrock (Claude)                            |
| `AWS_PROFILE`               | Amazon Bedrock (Custom Profile)                    |
| `AWS_BEARER_TOKEN_BEDROCK`  | Amazon Bedrock                                     |
| `AZURE_OPENAI_API_ENDPOINT` | Azure OpenAI models                                |
| `AZURE_OPENAI_API_KEY`      | Azure OpenAI models (optional when using Entra ID) |
| `AZURE_OPENAI_API_VERSION`  | Azure OpenAI models                                |

### Subscriptions

If you prefer subscription-based usage, here are some plans that work well in
Rush:

- [Synthetic](https://synthetic.new/pricing)
- [GLM Coding Plan](https://z.ai/subscribe)
- [Kimi Code](https://www.kimi.com/membership/pricing)
- [MiniMax Coding Plan](https://platform.minimax.io/subscribe/coding-plan)

### ChatGPT Codex subscription

Rush can use a ChatGPT subscription directly through OpenAI's Codex Responses
backend. Run `rush login openai-codex` for the browser OAuth (PKCE) flow, or
`rush login openai-codex --device` when a local browser is unavailable. Rush
stores and refreshes its own OAuth credential; it does not require the Codex
CLI, import Codex CLI credentials, or use `OPENAI_API_KEY` for this provider.
The separate `openai` provider and its API-key authentication are unchanged.

The login prints the URL without opening a browser. In an interactive terminal,
press the physical `O` key to open it or `C` to copy the full link, regardless
of the active Windows keyboard layout. `F2`/`F3` do the same on terminals
that do not report physical keys. The shortcuts appear below the URL.

If the token exchange is rejected, the CLI and `.rush/logs/rush.log` report
the HTTP status, response kind (`json`, `html`, `empty`, or `other`), content
type, response byte count, an allowlisted OAuth error code, and any recognized
request IDs. Authorization codes, tokens, and raw response bodies are never
logged. These fields distinguish OAuth errors from gateway blocks; they do
not identify the cause of an earlier failure before diagnostics were enabled.

After login, the model list is fetched for the authenticated ChatGPT account:

```bash
rush models list
rush models use --smart openai-codex/<listed-model>@high
rush ping --role smart
```

Use an effort supported by the listed model. Codex access is account-dependent;
an account without access returns no catalog. The OAuth login remains saved if
discovery is unavailable; retry `rush models list` or `rush providers
fetch-models openai-codex` after access or connectivity is restored. Rush does
not invent a default model while the account catalog is unavailable.

### Live model catalogs

Rush reads authenticated model lists from the ChatGPT Codex, StepFun, and
Z.AI model endpoints. A successful StepFun roster replaces bundled models;
Z.AI's roster is merged with known models because its endpoint can omit live
models. Codex and StepFun report per-model effort levels in their model
catalogs. When Z.AI's catalog omits levels, Rush reads explicit model/level
statements from Z.AI's published thinking documentation instead. Unknown
models receive no guessed effort control.

Authenticated catalogs are cached globally for seven days per provider and
credential identity; Z.AI's public documentation has its own seven-day cache.
To fetch fresh data on the next process start:

```bash
rush models cache clear                  # all three providers
rush models cache clear stepfun          # one provider
```

Restart an already-running WebUI server after clearing the cache. Run
`rush models cache clear --help` for all accepted provider names.

### By the Way

Rush also checks provider/model values not present in the local catalog when used with `rush models use`. A successful provider roster is cached for seven days, making newly released models known on subsequent loads; use `rush providers update <provider>` to refresh it explicitly. Models absent from the live roster remain settable, with unknown cost and context-window facts noted.

Is there a provider you’d like to see in Rush? Is there an existing model that needs an update?

Rush’s default model listing is managed in [Catwalk](https://github.com/charmbracelet/catwalk), a community-supported, open source repository of Rush-compatible models, and you’re welcome to contribute.

(Upstream's Catwalk badge image removed — see the project at
[charmbracelet/catwalk](https://github.com/charmbracelet/catwalk).)

## Upgrading from the pre-rename builds

This project was renamed from "Crush" to "Rush". If you have existing Crush
installations, run `rush migrate` to handle everything a filesystem tool can
handle automatically, then follow its final report for anything it can't.

`rush migrate` renames, in one pass:

- `.crush/` directories to `.rush/` and `crush.json` files to `rush.json` —
  both workspace configs and the global config/data locations (and, on Unix,
  the root-owned `/etc/crush/crush.json` system config — this one may need
  elevated privileges)
- Known artifact files inside a migrated directory: `crush.db` → `rush.db`
  and `logs/crush.log` → `logs/rush.log`, so existing session history and
  logs stay reachable under the names the app now looks for
- Legacy-named *values inside* a migrated `rush.json`: `disabled_skills`
  entries referencing a renamed builtin skill ID (`crush-config` →
  `rush-config`, `crush-hooks` → `rush-hooks`), and `.crush`-referencing path
  segments (including the `~/.config/crush` convention) or the old
  `CRUSH.md` context-file name in `skills_paths`, `global_context_paths`,
  and `data_directory` — this is a narrow, field-scoped rewrite, not a blind
  find-and-replace, so unrelated content (e.g. a hook command string that
  happens to mention "crush") is left untouched
- Leftover files in an already-existing global target directory: if
  `~/.config/rush` (or the data-dir equivalent) already existed before
  migrating — common, since the app itself may have created it on a prior
  run — any other legacy files still sitting in the old directory (`skills/`,
  `auth.json`, etc.) are also moved into the target directory, one item at a
  time, refusing only the specific items whose names already conflict there

> [!NOTE]
> Rush-owned config now lives as `rush.ktav` (the preferred spelling);
> `rush.json` keeps working until Rush's automatic migration converts it. To
> convert by hand, write the Ktav equivalent of the JSON content into
> `rush.ktav` — never merely rename a `rush.json` to `rush.ktav` (a `.ktav`
> file is decoded as Ktav, and JSON bytes under that name are an error with
> no fallback). If `rush.json` is a symbolic link (a dotfiles-managed
> repository), migration publishes `rush.ktav` beside the link and removes
> only the link — the physical target is left untouched and acts as the
> backup (no `.bak`). See [docs/ktav-config.md](./docs/ktav-config.md).

By default it operates on the current working directory (or `--cwd` if set)
and always attempts the global/system locations above; `--recursive` walks
the entire directory tree from the given root, including the root itself.
Use `--dry-run` to preview changes without touching anything. Conflicting
targets are never clobbered — a conflicting item is refused and reported,
and everything else still proceeds.

At the end of every run, `rush migrate` prints a "Manual follow-up needed"
section for the two things a rename tool cannot fix by itself:

- Any `CRUSH_*`-prefixed environment variable still set in your shell with
  no automatic Rush equivalent — the running app only reads `RUSH_*` names
  now (e.g. `RUSH_FORBID_WRITES`, `RUSH_CACHE_DIR`, `RUSH_SKILLS_DIR`), so an
  old `CRUSH_*` variable left in `.bashrc`/`.zshrc`/your PowerShell profile
  is silently ignored rather than erroring. This is computed from your
  actual shell environment at the time you run the command, not a fixed
  list, so it only flags what you personally still have set.
- A reminder to run the matching `*-del` command (`claude-del`, `codex-del`,
  `gemini-del`, `grok-del`, `qwen-del`) if you still have old crush-named
  slash-commands/agents installed by a pre-rename `*-init` run — the current
  `*-init` commands install new rush-named files alongside them but never
  touch the old ones, so cleanup is a separate, explicit step.

Instead of running all 10 `*-init`/`*-del` commands by hand, `rush
cli-refresh` runs each tool's del-then-init pair back to back for all 5
integrations at once (local dir by default, `--recursive [root]` for a
directory tree, or `--global`; supports `--dry-run` too). For Codex, that
refresh includes all three delegation Skills: `rush`, `rush-fallback` and
`wrush`.

Also update scripts that reference the old names directly: commands like
`crush run` → `rush run`, paths like `.crush/` → `.rush/`, `crush.json` →
`rush.json` (Rush prefers the `rush.ktav` spelling once Ktav config is
active; the legacy `rush.json` name is still read), and `crush.db` →
`rush.db`.

## Configuration

> [!TIP]
> Rush ships with a builtin `rush-config` skill for configuring itself. In
> many cases you can simply ask Rush to configure itself.

Rush runs great with no configuration. That said, if you do need or want to
customize Rush, configuration can be added either local to the project itself,
or globally, with the following priority:

1. `rush.ktav` / `.rush.ktav` (Ktav; preferred)
2. `$HOME/.config/rush/rush.ktav`

Items in group 1 are searched by name in the project root, walking up the
directory tree from `cwd` — a file at `./.rush/rush.ktav` (inside the
`.rush/` subdirectory) is not discovered; that directory holds ephemeral
data, not config. At a single directory, the visible `rush.ktav` beats the
hidden `.rush.ktav`.

Rush prefers `rush.ktav` per location: when both `rush.ktav` and
`rush.json` exist in the same place, the Ktav file wins and one warning is
logged. The legacy JSON spelling (`rush.json` / `.rush.json`, including
`$HOME/.config/rush/rush.json`) is still valid input and is read when no
Ktav sibling exists — but an invalid `rush.ktav` is an error with no
fallback to `rush.json`.

> See [docs/ktav-config.md](./docs/ktav-config.md) for the full Ktav
> configuration guide (migration, discovery, rollback).

Configuration itself is stored as a Ktav object:

```ktav
this-setting: {
    this: that
}
that-setting: [
    ceci
    cela
]
```

As an additional note, Rush also stores ephemeral data, such as application
state, in one additional location:

```bash
# Unix
$HOME/.local/share/rush/rush.ktav

# Windows
%LOCALAPPDATA%\rush\rush.ktav
```

(The legacy `rush.json` names at these paths are still read.)

> [!TIP]
> You can override the user and data config locations by setting:
>
> - `RUSH_GLOBAL_CONFIG`
> - `RUSH_GLOBAL_DATA`

Workspace trust is fail-closed: if the workspace config or its working
directory cannot be statted, or the config is owned by a different user,
Rush refuses to load or reload that workspace configuration. Missing workspace
config is still allowed; foreign-owned global and project candidates are
skipped during discovery.

### LSPs

Rush can use LSPs for additional context to help inform its decisions, just
like you would. LSPs can be added manually like so:

```ktav
lsp: {
    go: {
        command: gopls
        env: {
            GOTOOLCHAIN: go1.24.5
        }
    }
    typescript: {
        command: typescript-language-server
        args: [
            --stdio
        ]
    }
    nix: {
        command: nil
    }
}
```

### MCPs

Rush also supports Model Context Protocol (MCP) servers through three transport
types: `stdio` for command-line servers, `http` for HTTP endpoints, and `sse`
for Server-Sent Events.

Shell-style value expansion (`$VAR`, `${VAR:-default}`, `$(command)`, quoting,
nesting) works in `command`, `args`, `env`, `headers`, and `url`, so
file-based secrets work out of the box. You can use values like `"$TOKEN"`
or `"$(cat /path/to/secret/token)"`. Expansion runs through Rush's embedded
shell, so the same syntax works on every supported system, Windows included.

Unset variables expand to the empty string by default, matching bash. For
required credentials, use `${VAR:?message}` so an unset variable fails loudly
at load time with `message` instead of silently resolving to empty:

```ktav
api_key: ${CODEBERG_TOKEN:?set CODEBERG_TOKEN}
```

Headers (both MCP `headers` and provider `extra_headers`) whose value
resolves to the empty string are dropped from the outgoing request rather
than sent as `Header:`. That keeps optional env-gated headers like
`"OpenAI-Organization": "$OPENAI_ORG_ID"` clean when the variable is unset.

Provider `extra_body` is a non-expanding JSON passthrough; put env-driven
values in `extra_headers` or the provider's `api_key` / `base_url`, all of
which do expand.

> **Security note:** the config file (`rush.ktav`, or legacy `rush.json`) is
> trusted code. Any `$(...)` in it runs at
> load time with your shell's privileges, before the UI appears. Don't launch
> Rush in a directory whose config file you haven't reviewed.

```ktav
mcp: {
    filesystem: {
        type: stdio
        command: node
        args: [
            /path/to/mcp-server.js
        ]
        timeout: 120
        disabled: false
        disabled_tools: [
            some-tool-name
        ]
        env: {
            NODE_ENV: production
        }
    }
    github: {
        type: http
        url: https://api.githubcopilot.com/mcp/
        timeout: 120
        disabled: false
        disabled_tools: [
            create_issue
            create_pull_request
        ]
        headers: {
            Authorization: Bearer $GH_PAT
        }
    }
    streaming-service: {
        type: sse
        url: https://example.com/mcp/sse
        timeout: 120
        disabled: false
        headers: {
            API-Key: $(echo $API_KEY)
        }
    }
}
```

### Hooks

Rush has preliminary support for hooks. For details, see
[the hook guide](./docs/hooks/).

### Global context files

Rush automatically includes two files for cross-project instructions.

- `~/.config/rush/RUSH.md`: Rush-specific rules that would confuse other
  agentic coding tools. If you only use Rush, this is the only one you need to
  edit.
- `~/.config/AGENTS.md`: generic instructions that other coding tools might
  read. Avoid referring to Rush-specific features or workflows here. You
  probably only care about this if you use multiple agentic coding tools and
  want to share instructions between them.

You can customize these paths using the `global_context_paths` option in your
configuration:

```ktav
options: {
    global_context_paths: [
        ~/path/to/custom/context/file.md
        ## recursively load all .md files in folder:
        /full/path/to/folder/of/files/
    ]
}
```

### Ignoring Files

Rush respects `.gitignore` files by default, but you can also create a
`.rushignore` file to specify additional files and directories that Rush
should ignore. This is useful for excluding files that you want in version
control but don't want Rush to consider when providing context.

The `.rushignore` file uses the same syntax as `.gitignore` and can be placed
in the root of your project or in subdirectories.

### Allowing Tools

By default, Rush will ask you for permission before running tool calls. If
you'd like, you can allow tools to be executed without prompting you for
permissions. Use this with care.

```ktav
permissions: {
    allowed_tools: [
        view
        ls
        grep
        edit
        mcp_context7_get-library-doc
    ]
}
```

### Restricting `rush run`

Non-interactive `rush run` invocations auto-approve every permission request
by default (no human is on the keyboard). The `permissions.run` block flips
that to deny-by-default so an unattended run can be scoped to a known-safe
allowlist. Interactive sessions (TUI / web) are never affected.

Set `permissions.run.restrict` to `true`, then list the non-bash tools
(`allow_tools`, same `tool` / `tool:action` syntax as `allowed_tools`) and
bash command patterns (`allow_bash`) the run may use. Anything outside those
lists is denied cleanly.

```ktav
permissions: {
    run: {
        restrict: true
        allow_tools: [
            view
            edit:write
        ]
        allow_bash: [
            git diff
            glob:ls *
            regex:^go (test|build)
        ]
    }
}
```

`allow_bash` entries take one of four forms:

- `cmd args` — word-boundary prefix match (e.g. `"git diff"` matches
  `"git diff HEAD~1"` but not `"git difftool"`). Chaining metacharacters
  (`;`, `|`, `&&`, `$(`, `` ` ``) are refused, so `"ls"` can never approve
  `"ls && rm -rf /"`.
- `exact:cmd` — whole-string equality after trimming whitespace; same
  chaining guard.
- `glob:pat` — `filepath.Match` against the raw command string. No chaining
  guard (explicit user wildcard).
- `regex:pat` — regexp match against the raw command string. No chaining
  guard (explicit user pattern).

`allow_tools` entries for `"bash"` / `"bash:execute"` are intentionally
ignored by the run gate — bash is governed solely by `allow_bash`, so an
operator can't accidentally authorise arbitrary shell commands by listing
the tool name. To bypass the gate for bash wholesale, use the global
`permissions.allowed_tools` (which is checked before the gate); otherwise
leave `bash` out of `allowed_tools` and use `run.allow_bash`.

There are no CLI flags for this: the former `rush run` permission flags
(`--restrict-run`, `--allow-bash`, `--allow-tool`) were removed, so the
`permissions.run` block in `rush.json` is the only way to arm and scope
restricted runs. Run-permission management is expected to continue
through roles, not CLI flags.

### Disabling Built-In Tools

If you'd like to prevent Rush from using certain built-in tools entirely, you
can disable them via the `options.disabled_tools` list. Disabled tools are
completely hidden from the agent.

```ktav
options: {
    disabled_tools: [
        bash
        sourcegraph
    ]
}
```

To disable tools from MCP servers, see the [MCP config section](#mcps).

### Disabling Skills

If you'd like to prevent Rush from using certain skills entirely, you can
disable them via the `options.disabled_skills` list. Disabled skills are hidden
from the agent, including builtin skills and skills discovered from disk.

```ktav
options: {
    disabled_skills: [
        rush-config
    ]
}
```

### Agent Skills

Rush supports the [Agent Skills](https://agentskills.io) open standard for
extending agent capabilities with reusable skill packages. Skills are folders
containing a `SKILL.md` file with instructions that Rush can discover and
activate on demand.

The global paths we looks for skills are:

* `$RUSH_SKILLS_DIR`
* `$XDG_CONFIG_HOME/agents/skills` or `~/.config/agents/skills/`
* `$XDG_CONFIG_HOME/rush/skills` or `~/.config/rush/skills/`
* `~/.agents/skills/`
* `~/.claude/skills/`
* On Windows, we _also_ look at
  * `%LOCALAPPDATA%\agents\skills\` or `%USERPROFILE%\AppData\Local\agents\skills\`
  * `%LOCALAPPDATA%\rush\skills\` or `%USERPROFILE%\AppData\Local\rush\skills\`
* Additional paths configured via `options.skills_paths`

On top of that, we _also_ load skills in your project from the following
relative paths:

* `.agents/skills`
* `.rush/skills`
* `.claude/skills`
* `.cursor/skills`

Skills from other tools' directories are loaded and available as slash commands, but only Rush's own and Agent Skills spec directories are advertised in the agent system prompt.

```ktav
options: {
    skills_paths: [
        ## Windows: %LOCALAPPDATA%\rush\skills
        ~/.config/rush/skills
        ./project-skills
    ]
}
```

You can get started with example skills from [anthropics/skills](https://github.com/anthropics/skills):

```bash
# Unix
mkdir -p ~/.config/rush/skills
cd ~/.config/rush/skills
git clone https://github.com/anthropics/skills.git _temp
mv _temp/skills/* . && rm -rf _temp
```

```powershell
# Windows (PowerShell)
mkdir -Force "$env:LOCALAPPDATA\rush\skills"
cd "$env:LOCALAPPDATA\rush\skills"
git clone https://github.com/anthropics/skills.git _temp
mv _temp/skills/* . ; rm -r -force _temp
```

#### User-Invocable Skills

Skills can be made invocable as commands from the commands palette (Ctrl+P). Add `user-invocable: true` to the skill's YAML frontmatter:

```yaml
---
name: my-skill
description: A skill that can be invoked as a command.
user-invocable: true
---
```

User-invocable skills appear in the commands palette with a `user:` or `project:` prefix:
- Skills from global directories show as `user:skill-name`
- Skills from project directories show as `project:skill-name`

When invoked, the skill's instructions are loaded into the conversation context.

To prevent the model from auto-triggering a skill (while still allowing user invocation), add `disable-model-invocation: true`:

```yaml
---
name: my-skill
description: Only invocable by users, not the model.
user-invocable: true
disable-model-invocation: true
---
```

Skills with `disable-model-invocation` won't appear in the model's available skills list but can still be invoked manually by users.

### Desktop notifications

Rush sends desktop notifications when a tool call requires permission and when
the agent finishes its turn. They're only sent when the terminal window isn't
focused _and_ your terminal supports reporting the focus state.

```ktav
options: {
    ## default:
    disable_notifications: false
}
```

To disable desktop notifications, set `disable_notifications` to `true` in your
configuration. On macOS, notifications currently lack icons due to platform
limitations.

### Initialization

When you initialize a project, Rush analyzes your codebase and creates
a context file that helps it work more effectively in future sessions.
By default, this file is named `AGENTS.md`, but you can customize the
name and location with the `initialize_as` option:

```ktav
options: {
    initialize_as: AGENTS.md
}
```

This is useful if you prefer a different naming convention or want to
place the file in a specific directory (e.g., `RUSH.md` or
`docs/LLMs.md`). Rush will fill the file with project-specific context
like build commands, code patterns, and conventions it discovered during
initialization.

### Attribution Settings

By default, Rush adds attribution information to Git commits and pull requests
it creates. You can customize this behavior with the `attribution` option:

```ktav
options: {
    attribution: {
        trailer_style: co-authored-by
        generated_with: true
    }
}
```

- `trailer_style`: Controls the attribution trailer added to commit messages
  (default: `assisted-by`)
  - `assisted-by`: Adds `Assisted-by: Rush:[ModelID]` as specified in [the convention](https://docs.kernel.org/process/coding-assistants.html#attribution)
  - `co-authored-by`: Adds `Co-Authored-By: Rush <rush@charm.land>`
  - `none`: No attribution trailer
- `generated_with`: When true (default), adds `💘 Generated with Rush` line to
  commit messages and PR descriptions

### Custom Providers

Rush supports custom provider configurations for both OpenAI-compatible and
Anthropic-compatible APIs.

> [!NOTE]
> Note that we support two "types" for OpenAI. Make sure to choose the right one
> to ensure the best experience!
>
> - `openai` should be used when proxying or routing requests through OpenAI.
> - `openai-compat` should be used when using non-OpenAI providers that have OpenAI-compatible APIs.

#### OpenAI-Compatible APIs

Here’s an example configuration for Deepseek, which uses an OpenAI-compatible
API. Don't forget to set `DEEPSEEK_API_KEY` in your environment.

```ktav
providers: {
    deepseek: {
        type: openai-compat
        base_url: https://api.deepseek.com/v1
        api_key: $DEEPSEEK_API_KEY
        models: [
            {
                id: deepseek-chat
                name: Deepseek V3
                cost_per_1m_in: 0.27
                cost_per_1m_out: 1.1
                cost_per_1m_in_cached: 0.07
                cost_per_1m_out_cached: 1.1
                context_window: 64000
                default_max_tokens: 5000
            }
        ]
    }
}
```

#### Anthropic-Compatible APIs

Custom Anthropic-compatible providers follow this format:

```ktav
providers: {
    custom-anthropic: {
        type: anthropic
        base_url: https://api.anthropic.com/v1
        api_key: $ANTHROPIC_API_KEY
        extra_headers: {
            anthropic-version: 2023-06-01
        }
        models: [
            {
                id: claude-sonnet-4-20250514
                name: Claude Sonnet 4
                cost_per_1m_in: 3
                cost_per_1m_out: 15
                cost_per_1m_in_cached: 3.75
                cost_per_1m_out_cached: 0.3
                context_window: 200000
                default_max_tokens: 50000
                can_reason: true
                supports_attachments: true
            }
        ]
    }
}
```

### Amazon Bedrock

Rush currently supports running Anthropic models through Bedrock, with caching disabled.

- A Bedrock provider will appear once you have AWS configured, i.e. `aws configure`
- Rush also expects the `AWS_REGION` or `AWS_DEFAULT_REGION` to be set
- To use a specific AWS profile set `AWS_PROFILE` in your environment, i.e. `AWS_PROFILE=myprofile rush`
- Alternatively to `aws configure`, you can also just set `AWS_BEARER_TOKEN_BEDROCK`

### Vertex AI Platform

Vertex AI will appear in the list of available providers when `VERTEXAI_PROJECT` and `VERTEXAI_LOCATION` are set. You will also need to be authenticated:

```bash
gcloud auth application-default login
```

To add specific models to the configuration, configure as such:

```ktav
providers: {
    vertexai: {
        models: [
            {
                id: claude-sonnet-4@20250514
                name: VertexAI Sonnet 4
                cost_per_1m_in: 3
                cost_per_1m_out: 15
                cost_per_1m_in_cached: 3.75
                cost_per_1m_out_cached: 0.3
                context_window: 200000
                default_max_tokens: 50000
                can_reason: true
                supports_attachments: true
            }
        ]
    }
}
```

### Provider Network (Proxy, Custom DNS, DoH)

Outbound LLM provider connections can be routed through a proxy and/or use a
custom DNS resolver instead of the operating system's. This is configured
entirely in the config file (`rush.ktav`, or legacy `rush.json`) — there is no CLI flag. It applies to every provider
type that makes outbound HTTP calls (Anthropic, OpenAI, OpenRouter, Vercel,
Azure, Bedrock, Google, Vertex, OpenAI-compatible and custom providers).

**Global default** — `options.network` applies to every provider:

```ktav
options: {
    network: {
        proxy: socks5://127.0.0.1:1080
    }
}
```

**Proxy types** — `proxy` is a URL; the scheme picks the type:
`socks5://` (also `socks5h://`, same behavior) or `http://` (HTTP CONNECT
tunnel). Credentials go in the userinfo (`socks5://user:pass@host:1080`).
Any other scheme (`https://`, `ftp://`, ...) is rejected at startup.

**Per-provider override** — `providers.<id>.network` overrides the global
block. A provider's `proxy` **replaces** the global proxy entirely (they are
never combined or chained), so one provider can use a different proxy — or a
different proxy type — than everyone else:

```ktav
options: {
    network: {
        proxy: socks5://127.0.0.1:1080
    }
}
providers: {
    my-provider: {
        type: openai-compat
        base_url: https://api.example.com/v1
        api_key: $EXAMPLE_API_KEY
        network: {
            proxy: http://proxy.corp.internal:8080
        }
        models: []
    }
}
```

Here `my-provider` goes through the HTTP proxy and every other provider through
the global SOCKS5 proxy.

**Disable the proxy for one provider** — `proxy: none` (case-insensitive) on a
provider cancels the global proxy for that provider only; its requests go
direct:

```ktav
options: {
    network: {
        proxy: socks5://127.0.0.1:1080
    }
}
providers: {
    local-llm: {
        type: openai-compat
        base_url: http://192.168.1.20:8000/v1
        network: {
            proxy: none
        }
        models: []
    }
}
```

`proxy: none` is also accepted under `options.network` (no global proxy), where
it is the same as leaving `proxy` out.

Resolution order, per provider:

| Global `options.network.proxy` | Provider `network.proxy` | Provider uses |
|---|---|---|
| unset / `none` | unset | direct connection |
| `socks5://g:1080` | unset (or no `network` block) | `socks5://g:1080` |
| `socks5://g:1080` | `http://p:8080` | `http://p:8080` |
| `http://g:8080` | `socks5://p:1080` | `socks5://p:1080` |
| `socks5://g:1080` | `none` | direct connection |
| `none` / unset | `http://p:8080` | `http://p:8080` |

Only `proxy` is replaced as a whole. `dns_server` and `doh_url` still cascade
**per field**: a provider that sets only `proxy` (or only `proxy: none`) keeps
the global `dns_server`/`doh_url`, and vice versa. A field set on both uses the
provider's value.

**Combined proxy + DoH** — when both are configured, even the DNS lookup is
tunneled through the proxy, so no hostname ever resolves on the local
network:

```ktav
options: {
    network: {
        proxy: socks5://user:pass@10.0.0.2:1080
        doh_url: https://cloudflare-dns.com/dns-query
    }
}
```

How the pieces interact:

- **Proxy only** — the proxy resolves hostnames itself. HTTP proxies
  (`http://`) are tunneled via CONNECT; SOCKS5 (`socks5://` or `socks5h://`)
  passes hostnames to the server unresolved. Userinfo authentication
  (`socks5://user:pass@host:port`) is supported.
- **DNS/DoH only** — hostnames resolve through the configured server and the
  connection dials the resolved IP directly. If both `dns_server` and
  `doh_url` are set, `doh_url` wins (documented precedence, not an error); a
  `dns_server` without a port gets port 53 appended.
- **Both** — the DNS/DoH lookup itself goes through the proxy and the final
  connection uses the same proxy. Nothing touches the OS resolver.

A malformed `proxy` or `doh_url` (unsupported scheme, unparseable URL; `none` is not
malformed) fails
the provider build loudly at startup instead of silently falling back to a
direct connection. CLI providers (`claude`, `gemini`, `codex`, `qwen` — the
`cliprovider` type) shell out to a local binary and ignore this setting.

Every request that is addressed to a provider follows this cascade: its LLM
requests, OAuth token refresh, model-list discovery at startup, `rush models use`
verification of unknown models, `rush providers update` and `rush providers test`.
A malformed `network` block makes those requests fail with a clear message instead
of silently connecting directly. Traffic that is not tied to a provider — MCP
servers, the web fetch/download tools, and the public Z.AI documentation page used
for reasoning levels — does not read `network.proxy`; it follows the standard
`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` environment variables instead.

### Local Models

Local models can also be configured via OpenAI-compatible API. Here are two common examples:

#### Ollama

```ktav
providers: {
    ollama: {
        name: Ollama
        base_url: http://localhost:11434/v1/
        type: openai-compat
        models: [
            {
                name: Qwen 3 30B
                id: qwen3:30b
                context_window: 256000
                default_max_tokens: 20000
            }
        ]
    }
}
```

#### LM Studio

```ktav
providers: {
    lmstudio: {
        name: LM Studio
        base_url: http://localhost:1234/v1/
        type: openai-compat
        models: [
            {
                name: Qwen 3 30B
                id: qwen/qwen3-30b-a3b-2507
                context_window: 256000
                default_max_tokens: 20000
            }
        ]
    }
}
```

## Logging

Sometimes you need to look at logs. Luckily, Rush logs all sorts of
stuff. Logs are stored in `./.rush/logs/rush.log` relative to the project.

The CLI also contains some helper commands to make perusing recent logs easier:

```bash
# Print the last 1000 lines
rush logs

# Print the last 500 lines
rush logs --tail 500

# Follow logs in real time
rush logs --follow
```

Want more logging? Run `rush` with the `--debug` flag, or enable it in the
config:

```ktav
options: {
    debug: true
    debug_lsp: true
}
```

## Provider Auto-Updates

By default, Rush automatically checks for the latest and greatest list of
providers and models from [Catwalk](https://github.com/charmbracelet/catwalk),
the open source Rush provider database. This means that when new providers and
models are available, or when model metadata changes, Rush automatically
updates your local configuration.

### Disabling automatic provider updates

For those with restricted internet access, or those who prefer to work in
air-gapped environments, this might not be want you want, and this feature can
be disabled.

To disable automatic provider updates, set `disable_provider_auto_update` into
your config file (`rush.ktav`, or legacy `rush.json`):

```ktav
options: {
    disable_provider_auto_update: true
}
```

Or set the `RUSH_DISABLE_PROVIDER_AUTO_UPDATE` environment variable:

```bash
export RUSH_DISABLE_PROVIDER_AUTO_UPDATE=1
```

## Provider management

Rush provides a suite of commands to inspect and manage LLM providers:

### List providers

```bash
# List all configured providers across global and workspace scopes
rush providers list

# Filter by id, name, or type (case-insensitive substring match)
rush providers list --grep zai

# Emit JSON for further processing
rush providers list --json | jq '.[] | select(.type=="openai")'
```

Output shows ID, name, type, status (enabled/disabled), model count, and (masked) API key.

### Show provider details

```bash
# Show full details for a provider
rush providers show openai

# Emit JSON
rush providers show openai --json
```

### Enable/disable a provider

```bash
# Enable a disabled provider (re-enables it and refreshes models)
rush providers enable zai

# Disable a provider (keeps credentials, sets disabled flag)
rush providers disable openai
```

### Add a new provider

```bash
# Add a catwalk-known provider (uses default base URL from catwalk)
rush providers add zai --name "Z.AI" --type openai-compat --api-key $ZAI_API_KEY

# Add with a custom base URL
rush providers add local-llm --name "Local LLM" --type openai-compat \
  --base-url http://localhost:8000/v1 --api-key none

# Add but don't enable
rush providers add myProvider --name "My Provider" --type openai \
  --api-key $KEY --no-enable
```

### Peak-hours refusal window and message

A provider can be configured to refuse requests during a local-time
window, with an optional custom note appended to the refusal.

```bash
# Refuse during business hours (local time); clear with "off"
rush providers set openai --peak-hours 09:00-18:00

# Custom message shown when a request is refused inside the window
rush providers set openai --peak-hours-message "Ping #ops-oncall before retrying"

# Set window and message together, written only to the workspace config
rush providers set openai --local --peak-hours 09:00-18:00 \
  --peak-hours-message "Low quota until 18:00"

# Same flags at creation time
rush providers add zai --name "Z.AI" --type openai-compat \
  --peak-hours 09:00-18:00 --peak-hours-message "Low quota until 18:00"
```

Notes:

- A time-only `--peak-hours` update preserves the configured message.
- `--peak-hours-message ""` clears the message and keeps the window;
  `--peak-hours off` clears both.
- The message requires a window — set `--peak-hours` first.
- Like the other provider fields, `--local` writes only to the
  workspace config (`./.rush/rush.ktav`, or legacy `./.rush/rush.json`); the default is the global
  config.
- `rush providers show <id>` prints the configured message.

### Remove a provider

```bash
# Remove a provider with confirmation
rush providers remove openai

# Remove without prompting (required in non-interactive mode)
rush providers remove openai --yes
```

### Update provider models

```bash
# Refresh models for a single provider
rush providers update zai

# Refresh models for all enabled providers
rush providers update --all
```

Shows a diff of added/removed models. Warns if any currently-preferred model is orphaned.

### Filter providers (grep sugar)

```bash
# Equivalent to `providers list --grep pattern`
rush providers grep openai-compat
```

### Manually updating providers

Manually updating providers is possible with the `rush update-providers`
command:

```bash
# Update providers remotely from Catwalk.
rush update-providers

# Update providers from a custom Catwalk base URL.
rush update-providers https://example.com/

# Update providers from a local file.
rush update-providers /path/to/local-providers.json

# Reset providers to the embedded version, embedded at rush at build time.
rush update-providers embedded

# For more info:
rush update-providers --help
```

## Metrics

Rush records pseudonymous usage metrics (tied to a device-specific hash),
which maintainers rely on to inform development and support priorities. The
metrics include solely usage metadata; prompts and responses are NEVER
collected.

Details on exactly what’s collected are in the source code
([here](https://github.com/PHPCraftdream/rush/tree/main/internal/event)
and [here](https://github.com/PHPCraftdream/rush/blob/main/internal/config/config.go)).

You can opt out of metrics collection at any time by setting the environment
variable by setting the following in your environment:

```bash
export RUSH_DISABLE_METRICS=1
```

Or by setting the following in your config:

```ktav
options: {
    disable_metrics: true
}
```

Rush also respects the [`DO_NOT_TRACK`](https://donottrack.sh/) convention
which can be enabled via `export DO_NOT_TRACK=1`.

## Q&A

### Why is clipboard copy and paste not working?

Installing an extra tool might be needed on Unix-like environments.

| Environment         | Tool                     |
| ------------------- | ------------------------ |
| Windows             | Native support           |
| macOS               | Native support           |
| Linux/BSD + Wayland | `wl-copy` and `wl-paste` |
| Linux/BSD + X11     | `xclip` or `xsel`        |

## Contributing

Open an issue or pull request on [this fork's repository](https://github.com/PHPCraftdream/rush).

[CI documentation](docs/ci.md)

## Whatcha think?

Questions or issues specific to this fork: use [GitHub Issues on
this repository](https://github.com/PHPCraftdream/rush/issues).

## License

[FSL-1.1-MIT](https://github.com/PHPCraftdream/rush/raw/main/LICENSE.md)

---

This fork is an independent project maintained by
[PHPCraftdream](https://github.com/PHPCraftdream), living under the
same FSL-1.1-MIT license as upstream, with no affiliation to Charm
Industries beyond the shared origin codebase (see the top of this
document for the link to upstream).

<!--prettier-ignore-->
Charm热爱开源 • Charm loves open source
