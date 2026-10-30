# Changelog

User/operator-facing changes to this fork. Not to be confused with
`CHANGELOG.fork.md`, which tracks upstream-merge decisions for future
mergers — this file tracks what actually changed in behavior.

Format loosely follows [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

- `RunWithCredentials`: the automatic follow-up turn after a delegated worker finishes no longer goes to the Client's configured (operator) provider; it is skipped and the completion is delivered with the session's next call.

## [0.2.0-alpha.19] - 2026-10-30

- Encrypted OpenAI reasoning that arrives without any summary text (OpenRouter and Vercel gateway `openai/*` models) is now sent back with the next request; it used to be dropped from the rebuilt history, so the model lost its hidden reasoning between tool calls.

## [0.2.0-alpha.18] - 2026-10-29

- Vertex AI providers (`google-vertex`) now get the Gemini thinking options (budget or level, include_thoughts) like the plain Google provider; the reasoning effort chosen for a Vertex model used to be never sent.
- An assistant reasoning block with empty text but an Anthropic or Gemini signature is no longer dropped when the provider history is rebuilt from the database.
- An assistant step with several reasoning blocks now keeps each block with its own text, signature and position (Anthropic signatures used to be concatenated into one invalid signature); reasoning is replayed before the text and tool calls, in the order it was stored.
- Anthropic `redacted_thinking` blocks are kept in the stored message and sent back to Anthropic in their original position; they used to be dropped.
- OpenAI Responses reasoning metadata (`item_id`, `encrypted_content`, `summary`) is recovered when a stored message is reloaded; it used to come back empty.
- The resume command printed when a run stops on a question (`exit_reason: "awaiting_answer"`) now carries the stopped run's `--role`; it used to print a command that `rush run` rejected. For an unknown role no command is printed.
- The shipped delegation skills (`/rush`, `/wrush`, `/wcrush`, `/rush-fallback`) are about half as long, corrected against the code, and state two rules callers kept getting wrong: do not forbid `smart` from delegating, and nested executors starting is not a reason to stop or ask the operator.
- The Codex renderings of those skills no longer wait for a `rush_done` marker that nothing produces any more: `/rush`, `/wrush` and `/wcrush` tell Codex to block on `write_stdin` until `rush run` exits.

## [0.2.0-alpha.17] - 2026-10-29

- When Rush's MCP bridge is required, the Claude CLI provider no longer falls back to the CLI's native, unpoliced tools if the bridge cannot start; the call now fails instead.
- When Rush's MCP integration is required, the Qwen, Gemini and Codex CLI providers no longer fall back to the CLI's native, unpoliced tools if the MCP server cannot be set up; the call now fails with the setup error instead.
- A provider's HTML error page (for example a Cloudflare 403 in front of the Codex endpoint) no longer floods the run error, the `--json` error field and stderr: it is shown as `HTTP <status> <title>: <short text>`.
- OAuth refresh: a newer Codex credential found on disk that the session has not seen yet is adopted again (and exchanged when expired); the stale-client rejection added with the credential-publication fix applied to it too broadly.
- Web UI: editing an assistant message that finishes while the edit is being applied no longer overwrites the finished reply with the older snapshot; the edit is rejected and the message can be reloaded and edited again.
- `rush batch run --session <id>` now refuses a session that has a persistent custom-provider requirement, like `rush run` does; it used to run the batch on the default provider.
- Web UI: changing the reasoning effort of one model slot no longer rewrites the other slot of its pair, so two quick changes (for example smart and fast) can no longer undo each other.
- Web UI server: a web-triggered shutdown now stops the WebSocket hub and closes its connections, a closing connection can no longer block after the hub has stopped, the login request body is limited to 4 KiB, and the log viewer reads only the tail of the log file.
- Long-running steps keep saving their partial reply: every periodic checkpoint write now gets its own 30 s deadline instead of sharing one that started with the step.
- Provider replay of reasoning: editing a reasoning part (appending text or a signature, finishing it) no longer drops its thought signature, tool id or OpenAI responses data.
- Purging expired batches now also removes a runner's checkpoint once that runner has no remaining batch nodes, and the batch history snapshot derives its counts and delivery state from the same database read.
- Cancelling a synchronous batch caller while its result was being delivered now stops the batch call instead of leaving it running unattended, and stopping the wake scheduler waits for wakes already dispatched.
- `rush sessions locks` looks up sub-agent activity in batched queries instead of one query per lock, so listings with many lock files are faster.
- In orchestrator mode the model is now told to treat a worker that ends without a report or evidence as failed, to answer a worker's question at once, to wait only on `await_tasks`, and to end with the work report; worker claims about commands stay claims until visible in evidence.
- An unfinished streaming snapshot can no longer be published after the final state of an assistant message and overwrite it in the browser.
- When a session reuses a tool-call id for different commands, each live-work row now shows its own command instead of the first one.
- Old, terminal, unannounced CLI batches can now expire even when the CLI left their completion notifications unsettled; active tasks, pending controls and delegation-child notification protections still apply.
- Concurrent Qwen/Gemini sessions in one project no longer overwrite each other's Rush MCP registration (endpoint and token) in the CLI's settings file: each session registers under its own name and cleans up only that.
- On Windows, a session lock held by a live process that rush cannot open (access denied) is no longer treated as orphaned; the liveness probe used to count every open failure as a dead process.
- Commands that take no positional arguments now reject them instead of ignoring them: `rush sessions cost <id>` used to print the totals of every session as if they were that session's.
- A tool that fails with an internal error inside a batch is now reported to the model as an error with that message; it used to come back as a successful empty result.
- Malformed or empty tool-call arguments from the model now come back to it as a tool error instead of ending the whole run.
- `rush batch run` now exits non-zero when the batch does not complete (failed, timed out, cancelled, interrupted); it used to exit 0. The result is still printed first.
- `rush sessions tree` shows each session's own cost again (it printed $0.00 for every node): the session list read now loads the full row.
- Web UI: editing a custom provider (name, models, peak hours, rename) no longer erases its API key; entering a new key still replaces it.
- A batch notice that goes back to pending after its message was deleted (history edit or rerun) now wakes the session again to redeliver it; a previously reacted notice used to stay silent.
- `rush sessions watch` now prints an assistant reply once it is finished; a reply it first saw while still streaming used to be shown empty or partial and never updated (also in sub-agent streams).
- `rush batch run --session <id>` now refuses a session owned by another workspace or a child session (as `rush run` does) and fails fast when the session is busy; it used to run the batch on it regardless.
- A long text or reasoning reply is now saved while it streams; checkpoints only fired when a new part was added, so watchers saw no progress and a crash lost the whole partial answer.
- A web session woken by a finished background job or batch notice now keeps the model chosen in the web UI; the wake turn used to fall back to the configured models.
- Delegated workers are told to wait for their own background jobs with `await_tasks` (prompt and the detached-start reply); a worker used `ask_question` to wait and hung until it was cancelled.
- `rush sessions reap --dry-run` no longer probes the locks it lists: the probe rewrote an orphan's PID, so the real `reap` that followed skipped it. Aged locks are now reported as `would probe`, untouched.
- `rush sessions locks` no longer panics when a lock file disappears between listing the directory and reading its metadata; the entry is skipped.
- Web settings: adding/removing a skills path or adding/editing/removing a custom provider whose write fails (e.g. settings locked) now replies with an error and leaves the live config untouched; it used to change the running config, broadcast it and reply OK. Renaming a provider onto an existing id is rejected instead of overwriting it.
- Claude CLI models now run with only rush's MCP tools and a few built-ins rush doesn't replicate (`--tools`): `--allowedTools` alone blocked nothing under `--dangerously-skip-permissions`, so the CLI's native Bash/Write/Edit bypassed agentguard, `--restrict-run`, folder scopes and rush's tool accounting.
- Tests can no longer reach the real global config: test binaries started from a `rush` process (which exports `RUSH_GLOBAL_DIR`) bypassed the temp-dir isolation in packages with their own `TestMain`. Every test binary now isolates through testguard, and resolving a global path in a test without it panics.
- Retrying the same `batch_run` call ID now reattaches to its live reservation instead of resubmitting and failing the Runner's unique owner/call reservation.
- `rush sessions reap` on Linux/macOS now unlinks an orphan lock while still holding the probe lock; it used to release the probe first and unlink later, so a `rush run` starting in between kept a lock on a deleted file while a second run created a new one — two owners of one session.
- One slow batch wake handler (a session whose reaction blocks) no longer delays the wake-ups of every other session: wakes now dispatch concurrently (bounded), and shutdown waits for in-flight handlers.
- Leaves submitted by the model through `batch_run`/`batch_add` now get their scheduling group, so per-group concurrency limits (io, cli/agent drivers) apply to them; they used to run unbounded.
- Config: a setting written while a JSON→Ktav migration completes is no longer lost into the now-ignored JSON file; an interrupted symlinked-config migration is recovered instead of stranding the record; a model edit with a stale `cache_rev` is no longer adopted after a mid-session migration.
- OAuth: concurrent credential setters can no longer publish an older key over a newer one, and a Codex refresh whose credential was superseded on disk no longer exchanges through the caller's stale client.
- Security: the git-write guard on the MCP `Bash` tool is now keyed on the session's own working directory, not the `working_dir` the model passes, so a model could no longer sidestep it by pointing the shell at another directory; and `fs_write`/`fs_replace`/`fs_write_lines`/`fs_delete` now request permission for every target path of a batch, not just the first.
- A batch runtime fault (exit code 70) now says why: `rush run` prints `rush run: batch runtime fault: <error>; exiting with code 70` to stderr, and with `--json` emits an envelope with `exit_reason: "batch_fault"` when no result had been written yet. Before, the run died silently with an empty stdout.
- An invalid `timeout` in a `bash`/`run_command`/`agent` call (out of range, malformed, or set together with `timeout_seconds`) now comes back to the model as a tool error; it used to end the whole run with `failed to start agent processing stream`.
- `rush models state` labels a role that no scope sets (built-in default) as `default` instead of `unknown`; `--json` is unchanged.
- `rush models state` prints EFFECTIVE and SCOPES as bordered tables with separate columns (role/scope, provider, model, effort, source scope or status, atom, note) instead of free-form lines; `--json` is unchanged.
- `rush ping` through a CLI provider (claude/codex/gemini/qwen) no longer gets "your request is empty" back: the default prompt now carries the reply-OK instruction instead of a bare `ping`, which the CLI saw beneath its own system prompt and the project's instructions.
- The automatic JSON→Ktav migration no longer moves `rush.json` (or the provider/catalog caches) to `.bak`, nor removes a symlinked `rush.json`: the JSON stays in place for older rush binaries that only read JSON, while newer ones use `rush.ktav`. The two files are not kept in sync afterwards. A leftover `rush.json.bak` beside an active `rush.json` no longer blocks the migration. Pending OAuth journals are still archived, since a stale duplicate could replay over rotated tokens.
- A batch commit now waits out SQLite's full 30s lock wait (budget 45s, was 5s): a few seconds of database contention no longer faults the batch runtime and kills `rush run` with exit 70 and an empty envelope.
- The `sonnet` and `haiku` model atoms now run Claude Sonnet 5.5 and Haiku 5.5, and `fable` runs Fable 5.1, instead of the claude CLI's lagging moving aliases.
- `--allow-peak-hours` (and the SDK's `AllowPeakHours`) now holds for the whole turn: the call build and the mid-turn peak-hours checks no longer abort a bypassed run whose model's provider is inside its peak window.
- The `opus` model atom (`rush models use opus-<effort>`) now runs Claude Opus 5.5 instead of Opus 4.8; `opus48` keeps the 4.8 pin.
- An unknown subcommand of a command group (e.g. `rush sessions lsit`) now fails with an error and a suggestion instead of printing help and exiting 0.
- Invalid or legacy settings locks now fail closed with a recovery hint naming the active file; remove `sync_rev` manually and run `rush lock` again.
- `rush batch run` rejects parallel `on_fail` (leaving the omitted default empty) and keeps each owning construction registry through accepted root completion; unsupported caller-supplied disk providers fail explicitly.
- Session/queue CLI output no longer cuts multibyte titles and previews mid-rune (e.g. a Russian title rendering as `в сооб\uFFFD…`): user-visible truncation (`truncate`, `truncatePreview`, stall summaries, show purpose) now cuts by display cell width with an ellipsis tail, so Cyrillic, CJK wide runes and emoji stay valid UTF-8 and table columns hold.
- `rush queue run` now persists the child run's resolved session ID and full session token/cost totals from the nested `--json` usage envelope, including when the run ends with an error.
- `rush --global-dir <DIR>` (also `RUSH_GLOBAL_DIR`; the flag wins) relocates all Rush-owned global state — config, data, caches, Rush-owned skills/commands, and the settings lock — under one directory with a `config`/`data`/`cache` layout, ignoring `RUSH_GLOBAL_CONFIG`/`RUSH_GLOBAL_DATA`/`RUSH_CACHE_DIR`/`XDG_*` for Rush-owned paths so an experimental build can run side by side with a production install. Third-party locations (`~/.claude`, `~/.agents/skills`, the GitHub Copilot CLI token file, legacy `crush.*` migration sources) are untouched.
- Workers may use `batch_stop` only on batches they own or that are owned by descendants; orchestrators retain existing worker-stop authority, while unknown owners and stale runner incarnations are refused.
- The `RUSH_BATCH_RUNTIME` switch is removed and has no effect: the batch-only runtime is the only runtime since the H4 cutover.
- The batch-only runtime is the only runtime. Startup imports durable history, settings, queues and schedules once from untouched `.rush/rush.db` into active `.rush/rush_store_v1.db` before opening it; old/preparation execution and control state is discarded. Stop old processes and automatic restarts throughout import; drain/stop preparation hosts before upgrade. Requeued work may repeat external side effects. Rollback means stopping new hosts and using the previous binary with untouched `rush.db` (old snapshot only); preserve the new store and sidecars, with no reverse import/merge-back. See [job-batch rollout](docs/jobbatch.md). This note records intended current behavior, not evidence of CI gates passing.
- Breaking: settings protection values (`rush lock`) are now salted PBKDF2-SHA-256 hashes (600000 iterations). Protection values written by older versions are no longer recognized and the settings stay protected: remove the stored protection value from the settings file by hand and run `rush lock` again.
- Breaking: `rush sessions show --json` renamed `async_jobs_total` / `async_jobs_running` to `batch_tasks_total` / `batch_tasks_running` (human output: `Batch tasks:`); there is no alias. The `job_output` and `job_kill` tools are gone: use `batch_status` / `batch_output` / `batch_stop` (see [batch runtime migration](docs/batch-runtime-migration.md)).
- Breaking: Rush-owned configuration is now Ktav by default. New config files are `rush.ktav`; at startup a legacy `rush.json` (global, workspace, project) is converted to `rush.ktav` and the original is renamed `rush.json.bak`; a symlinked `rush.json` is converted to a regular `rush.ktav` and only the link is removed (its target is left untouched); OAuth journals and the provider and model-catalog caches convert the same way. `##` comments in `rush.ktav` are not preserved by programmatic writes of Rush until the Ktav library offers key edits. Stop old Rush versions before the first start of this version. See [Ktav configuration](docs/ktav-config.md).

- WebUI adds a structured batch-tree view with held questions, notice state and conservative server refresh; stop remains cancellation intent, output is retained-summary-only, and unsupported controls are refused. See [job batches](docs/jobbatch.md).

## [0.2.0-alpha.16] - 2026-10-11

- `rush run` no longer stays alive after the agent has finished because of an orphaned `await_tasks` `max_wait` timer: the timer is now remembered and cancelled for `until: any` as well as `all`, and a run start cancels the session's leftover `max_wait` timers from an earlier process (for example one that hit the run timeout). Timers the model schedules itself (`wakein`, `wakeon`, loops) still hold the run open.
- `await_tasks` called by a delegated worker now tracks the exact jobs that were live when it was called instead of counting them: a job finishing while another one is launched no longer hides the finish from `until: any`, and jobs launched later neither wake `any` nor extend `all`.
- OAuth outbox recovery no longer replaces a newer unrelated live credential with an external journal token: an external journal record is adopted only when the live token equals it, equals the owned pending token, or is its proven predecessor, so a token another process rotated meanwhile is kept.
- Every rush process now re-reads the global OAuth credentials from disk about every 5 seconds in the background (a cheap file-stat check first, a full read only when the file changed) and lifts a token rotated by another process into memory, so a long-running process, delegated child or web session no longer keeps an already burned token until its next 401. The adoption goes through the same single arbiter as a refresh preflight and never signs the account out or changes the provider set.

## [0.2.0-alpha.15] - 2026-10-11

- Recovery of an OAuth token journal record that carries no predecessor digest (a refresh that returned an unchanged refresh token) no longer replaces an equal-expiry or newer live token with the older journalled one: a live token sharing the refresh token is treated as the journal token's predecessor only when it is strictly older. Not reachable with the current providers (only Codex uses the journal and its refresh token changes on every exchange), but required before any other provider opts in.
- GitHub Copilot and Hyper requests now use the current OAuth access token on every send instead of the token baked into the client when it was built, so delegated children and pinned models keep working across a token refresh (Copilot tokens live about 30 minutes). A 401 that arrives before any output is retried once after a successful refresh produced a different access token (only for a request whose body can be replayed); otherwise the original 401 reaches the existing recovery unchanged. The bearer is only ever substituted into a request that already carries an `Authorization` header, so it is never added to a cross-host redirect hop. A logged-out or removed provider answers a local 401 without any network traffic.
- OAuth token refreshes of GitHub Copilot and Hyper are now serialized per provider, in-process and across rush processes (the same refresh sidecar lock the Codex refresh uses): concurrent refreshes run one at a time and a waiter adopts the winner's token from disk instead of making a second exchange. Their refresh semantics are otherwise unchanged. The Codex journal engine is now descriptor-driven (a provider opts in with `rotating`; only Codex does), a predecessor digest is recorded only when the refresh token actually changed, and startup journal recovery covers every rotating provider.
- A delegated child's held question is no longer lost when its host dies: recovery of an interrupted delegation now puts the question (with its suggested options and any preamble) into the interrupted result the parent receives, instead of the generic "finished with no textual response" text. The question notice itself is still voided with the delegation, so the parent gets it exactly once.

## [0.2.0-alpha.14] - 2026-10-11

- OpenAI Codex requests now read the current OAuth token from memory before every send (nothing is copied when a client is built), so delegated children, pinned models and wake-ups keep working across a token rotation by this or another process; a 401 that arrives before any output is retried once after the token store refreshed (or adopted) the credentials, and a failed refresh still returns the original 401 to the existing turn-level recovery. Error responses are drained only up to a bound.

- Fix concurrent OpenAI Codex OAuth refreshes: one exchange per provider in-process and across processes (a separate sidecar lock, not the general config write lock), compare-and-refresh so a caller whose token was already rotated does not burn the new refresh token, the rotated token is published to memory first and then to a dedicated write-ahead journal next to the config (`<config>.oauth-pending-<hash>.json`, mode 0600), the config edit is retried without limit by a store-owned worker, and a crash or an unwritable/corrupt config no longer forces a re-login: the next process recovers the token from the journal before any exchange. A journal that cannot be written within a bounded time is handed to the worker so running agents are never blocked. Behaviour change: a corrupt journal is moved aside to `.corrupt-*` (at most five kept) and never blocks a refresh; logout and a later-expiring external token win over a pending rotation. Other providers keep the previous refresh path.

- Task-tree active forms now obey the title byte limit (empty remains valid), and fieldless edits are rejected; explicitly unchanged edits remain no-ops.
- Task protocol rejects payloads over 4 MiB or 128 structural container levels before decoding, returning `invalid_input` with the current summary and no writes.
- `tasklab` top-level and subcommand help now use stdout with exit 0; usage remains stderr/exit 2 and output failures exit 1.

- Added the autonomous task-tree component and `tasklab` development CLI: nested groups, stable IDs, incremental agent/operator commands, versioned atomic updates, idempotent invocation receipts, deletion protection, and validated checkpoint export/resume. It is independently runnable without Rush app/model/MCP/DB initialization; existing Rush `todos` and WebUI behavior remain unchanged pending the separate integration gate.
- Concurrent-map JSON-schema discovery retains map value types without a value receiver copying its mutex; this also fixes the full-tree `go vet` copy-lock failure.

## [0.2.0-alpha.13] - 2026-10-09

- A billing wall that a provider serves as an HTTP 5xx (MiniMax answers `api_error: insufficient balance (1008)` with a 500) is no longer retried as a transient server error: it ends the turn at once and triggers the `provider_limit` drain. The match needs the provider's own host, so a look-alike host or an ordinary 5xx stays transient.
- **Behaviour change:** a local `claude`/`codex`/`qwen` CLI run that reports its own failure is now a failed turn even when the process exits 0 — a Codex `{"type":"error"}` / `turn.failed` event, or a Claude/Qwen stream-json `{"type":"result","is_error":true}` line (before, such runs were treated as successes). If the failure text carries a usage-limit wall ("hit your usage limit", "You've hit your limit", "usage limit reached", "limit will reset", "try again at", `usage_limit_reached`, "quota"…) it becomes a hard provider limit (`rush run` drains live work and exits `provider_limit`) unless it also says it is per-minute/overload; only failure-shaped events are read, never assistant or tool output. The reset time is now extracted from the CLI wording — an epoch after `|`, "try again at Oct 9th, 2026 11:19 PM" (machine-local time), "resets 4pm (Asia/Kuala_Lumpur)" — and shown in `provider_limit` messages. The fixtures come from public reports and were not reproduced locally.
- Hard-limit detection now also covers the other API providers, from one rule table keyed by the serving host (an exact host match, never a suffix of a look-alike host): Moonshot/Kimi (`exceeded_current_quota_error` hard, rate/overload types transient), MiniMax (1008 and 2056 hard, 1002 transient), DeepSeek and OpenRouter (402 hard), xAI (used all credits / monthly spending limit), Groq (tokens per day hard, per-minute transient), Gemini/Vertex (`PerDay` quota hard, `PerMinute` transient), Bedrock (too many tokens per day), OpenAI (`insufficient_quota`), while Alibaba DashScope `insufficient_quota`/`Throttling.*` stay transient (they are TPS/TPM throttles there) and its overdue-bill and plan-window walls are hard, and Azure and Mistral stay transient unless the reset is 30 minutes or more. Groq "try again in 1h2m3s" and Gemini `RetryInfo` delays feed the long-reset rule. The fixtures come from vendor docs and public reports and were not reproduced locally.
- A provider rate limit's `Retry-After` / `Retry-After-Ms` header is now honoured: the headers arrive from net/http with canonical names (`Retry-After`), but the rate-limit wait looked them up by the lowercase key only, so it never saw them and always fell back to its own 10 s × 3 backoff. The lookup is now case-insensitive.
- Hard provider limits are now recognised by one shared, keyword-driven classifier (`internal/agent/limitwords`) with provider-aware exceptions, so `rush run` drains live work and exits with `provider_limit` for them instead of treating them as ordinary failures or retrying. Covered here: the ChatGPT/Codex backend (`usage_limit_reached` in the HTTP body or the stream, with `resets_in_seconds`/`resets_at`), Z.ai/GLM business codes (hard: 1113, 1308–1310, 1316–1321; transient: 1302, 1305) and StepFun (402 hard, 429 transient). Any 429 whose reset or `Retry-After` is 30 minutes or more is a hard wall, since the rate-limit wait budget can never cover it; per-minute and overload wording keeps a 429 transient. The fixtures come from vendor docs and public reports and were not reproduced locally.
- An Anthropic account that ran out of credits is now treated as a hard provider limit: the API answers HTTP 400 `invalid_request_error` "Your credit balance is too low to access the Anthropic API…" (not a 429), which used to end the turn as an ordinary error. `rush run` now lets live work finish and exits with `provider_limit` (and a resume command), like for a quota 429. Only that exact captured phrase is recognised (also on a 402); ordinary 400s, 401/403 and context-too-large errors are unchanged.
- **Behaviour change:** a run that hits its own deadline — `--timeout` or the default 6 h wall-clock cap — now ends with `exit_reason: "timeout"` instead of `"canceled"`. The envelope `error` names the effective limit and where it came from (`run timeout 1h30m exceeded (source: --timeout)`, or the default cap with a hint to pass `--timeout`), `resume_command` carries `rush run --role <role> --session <id>`, and stderr prints one plain line with the same text, the command and a note that the work is stored in the session, instead of the generic boxed "Context deadline exceeded." (which was easy to misread as a context-window limit). Ctrl-C, `sessions cancel` and a parent deadline still report `canceled`, and a timeout of an inner tool or provider call is not a run timeout; exit codes are unchanged (1 for the graceful end, 124 for the hard backstop). Wrappers that matched `canceled` for timeouts must also match `timeout`.
- A local-CLI provider (`claude`, `codex`, `gemini`, `qwen`) whose process exits with an error and prints a usage/quota wall (`usage limit`, `limit will reset`, `reset at`, `quota` — the same wording that marks an HTTP quota 429) is now reported as a hard provider quota error: no pointless retries, `rush run` drains live work and exits with `provider_limit`, and a stated reset time is kept. Structured JSON failure events are not interpreted yet, because no captured CLI failure output exists to verify them against.
- `rush run` no longer stalls or orphans work when the session's own provider hits a hard quota/usage limit (a 429 that is a quota wall, not a momentary rate limit). The failing model gets no further reaction turns in that run; the run keeps waiting only for live work — its own background jobs and delegations, which may run on other providers — so they finish and their results are stored in the session, then exits at once with the new `exit_reason: "provider_limit"` and a non-zero status instead of sitting on `await_tasks`/wake schedules that could only produce another failing turn. stderr and the JSON envelope (`resume_command`, `quota_reset_at` when the provider names a reset time, `finished_work`, `pending_child_questions`) say what finished while draining and how to resume; a sub-agent's pending question stays unanswered for the resumed run. Retryable rate limits keep their existing wait.
- **Breaking:** short codes for the local `claude` and `codex` CLI models now come only from the cc-arch-hands catalog (the same names as its slash-commands and sub-agents: `ox`, `o1h`, `s2m`, `hl`, `xs`, `us1`, …; suffix N means N releases behind the top model). The old hand-written version codes (`o48*`, `o47*`, `o46*`, `s46*`, `s45*`, `h45*`) are removed and fail with a migration hint; `ox` now selects Opus 5.5. Long-form atoms and raw `provider/model@effort` still work, and stored `cli-claude-*`/`cli-codex-*` model ids stay valid. The tables, per-model context windows and accepted efforts are generated from the cc-arch-hands package (`go run ./internal/tools/cahsync`), refreshed on every artifact build (`build.go`, the npm publish workflow, goreleaser): release builds fail if the catalog cannot be fetched, local builds keep the committed tables and never downgrade them, and a non-blocking CI job reports drift.
- The Anthropic HTTP provider no longer sends a blank `Authorization:` header next to `X-Api-Key` (or a blank `X-Api-Key` next to a Bearer token): the unused one was only blanked to keep the process environment out of the request, and the SDK put the empty header on the wire. Blank auth headers are now dropped by the provider's HTTP client.

## [0.2.0-alpha.12] - 2026-10-08

- Delegated workers can use `await_tasks` too. For a worker the call blocks inside its own turn until its background jobs finish (`any`/`all`), so the delegation stays open and the worker continues with the results instead of handing the orchestrator a premature answer; every worker wait is capped at 40 minutes (or `max_wait_seconds`, if shorter), is never moved to the background by the stall detector, and ends at once on `stop_agent`, an interrupting `inject_agent` or Stop. Workers also get `job_output`, so they can read their own background jobs.

- New agent tool `await_tasks`: the top-level agent ends its turn and sleeps while its own background jobs or delegations are running. `until: "any"` (default) wakes it on the first completion, `until: "all"` defers every wake-up turn until all running work has finished (a delegation's question or a human message still wakes it at once); optional `max_wait_seconds` (60..21600) schedules a safety wake that is cancelled as soon as the sleep ends. With nothing running the call is refused, so it cannot hang a run. The orchestrator rule in the system prompt tells the model to wait with `await_tasks` instead of `ask_question` or polling.
- `ask_question` in `rush run` no longer ends the run with `awaiting_answer` while delegations (sub-agents) are still running — before, only the agent's own background jobs counted, so an orchestrator that "asked" to wait for its workers exited and orphaned them. It now returns a hint to call `await_tasks` instead.
- A settings write to a redirected workspace file reached through a directory alias (a symlinked dir, macOS `/var` → `/private/var`, Windows short 8.3 names) is no longer refused by the lock in the workspace's natural location: the target and the workspace directory are now compared in canonical form.

## [0.2.0-alpha.11] - 2026-10-07

- A background job that Stop cancelled before its "started" result was written keeps its durable `cancelled` record when that write then fails (the abort path used to delete every unannounced row), and the job still leaves the in-memory ledger, so it never holds a job slot or keeps the session "running".
- A queued compaction (`/compact` or an automatic summary) whose Summarize loses the session lock to another process is put back in the queue for the session's next owner instead of being silently dropped; the caller now sees it as queued rather than as an error.

## [0.2.0-alpha.10] - 2026-10-07

- New commands `rush opencode-init` / `rush opencode-del` and `rush omp-init` / `rush omp-del` install and remove the `/rush`, `/rush-fallback`, `/wrush` and `/wcrush` delegation commands for OpenCode (`~/.config/opencode/commands/`, or `.opencode/commands/` with `--local`/`--cwd`) and Oh My Pi (`~/.omp/agent/commands/`, or `.omp/commands/`) as Markdown commands with `$ARGUMENTS`; files without Rush's sentinel are never overwritten or removed. Each target gets its own launch and liveness wording, taken from what the two harnesses report about their tools: OpenCode's `bash` has no background mode and a 2-minute default timeout given in milliseconds, so `rush run` runs in the foreground with a large `timeout` and liveness is checked before launch and after return; omp launches it with `"async": true` and `"timeout": 0`, is woken on completion, and keeps the turn alive with `wait` in print mode. `opencode-init` warns when a Codex-targeted `rush` Skill exists in `.agents/skills`, because OpenCode loads it too. For every non-Claude target (Codex, Gemini, Grok, Qwen, OpenCode, omp) `rush.md` no longer claims that an armed `/rush-fallback` switches silently — those variants cannot arm a switch, so on a hard limit the agent reports the error and asks the operator.
- `rush run` now prints one stderr line per provider rate-limit wait (`rush run: session "<id>" waiting for provider rate limit until HH:MM (attempt N)`), the wait deadline is recorded in the session's heartbeat row (`rate_limited_until`, cleared when the wait ends, also when it is cut short), and `rush sessions why` shows it for a running session in local time. Heartbeat source labels now reach the turn: a session's stored model reads `session-override`, an explicit per-call model or a credential-set model reads `per-call` (before, every turn read `slot@start`, so `rush ps` STALE-MODEL blamed the wrong cause). The app, `rush ps` and `rush logs prune` resolve the heartbeat directory through one helper, so `RUSH_HEARTBEAT_DIR` now also moves where the app writes. A panic inside a stall-detached tool now completes its job instead of leaving it "still running" forever, and finished detached jobs no longer accumulate for the process lifetime (dropped once their final output is served, on `job_kill` of a finished job, and at the end of the session's run).
- `--password` is checked only when it is given, so a read-only command no longer fails on an unreadable lock source (settings writes still fail closed); a wrong password against a workspace lock is now refused and audited (`command_denied`) the same way as against the global lock; `rush unlock` without `--local` says when only the workspace is locked.
- A provider rate limit (HTTP 429 that is not a quota/usage wall) no longer ends a run after a few short retries. It waits on its own budget, separate from `stream_stall_retries`: the provider's `Retry-After`/`retry-after-ms` when sent, otherwise 10 s growing ×3, at most 5 minutes per wait and 30 minutes in total; the budget does not run out while the run's own background work (delegations, jobs) is still live, and `--timeout`, cancellation and interrupts still end the wait at once. Each wait is logged with its deadline (`rush.log`). A quota 429 still fails fast, `stream_stall_retries: 0` disables the wait, and reaction (drain) turns keep a single attempt.
- Supervision check-ins now tell a working background command from a hung one. Each running job is listed with its label (the call's description or the command's first line), its age and how long it has been silent; a job running for at least 30 minutes and silent for at least max(20 minutes, half its age) is flagged "possibly stuck", the check-in asks the model to verify liveness with `job_output` (a live process is not progress) and to stop it with `job_kill` otherwise, a job still silent on the next check-in gets an explicit kill-or-justify line, and a check-in that pauses supervision names the oldest silent job. `job_output` on a running shell job reports "last output N ago" or "no output yet". New opt-in default deadline for background `bash`/`run_command` calls that pass no timeout: `rush run --job-timeout <duration>` or `options.background_job_default_timeout_seconds` (default off; an explicit per-call timeout always wins).
- The `glob`, `ls` and `grep` tools and the `view` "did you mean" suggestions can no longer walk a directory tree without bounds (a `glob` over the OS temp directory with ~250 000 entries used to hang a whole turn for 30+ minutes, because tool calls of one message run one after another). Directories are read in batches of 256 entries, and every walk checks cancellation, a 60 s wall-clock budget and a 200 000-entry budget; hitting a budget returns the partial result with a note "(search stopped after N entries / 60s under <dir>: results are incomplete; narrow path or pattern)". `glob` starts at the literal base of the pattern (a missing base answers "No files found" at once) and never descends deeper than a pattern without `**` can match.
- A `rush` turn can no longer hang silently. A process-wide sampler (every 15 s) tracks durable progress per turn (tool calls and results, step finishes, checkpoints, text; reasoning alone does not count). After 10 minutes without progress it logs a WARN and writes a goroutine dump (`stall-<session>-…`, mode 0600, at most every 30 minutes). A synchronous tool stuck that long (e.g. a `glob` over a huge directory) is moved to a background job and the model gets an immediate result naming the job, so the remaining tool calls of the same message run and the model can wait with `job_output` or stop it with `job_kill`; delegations and bash/run_command are never detached and rush never kills a tool on its own. Past `--stall-timeout` (default 30m, `0` disables the abort only) a stalled provider request is cancelled and re-issued at most twice, each retry getting a full 10-minute window, an internal stage aborts at once, and the run ends with `exit_reason "stalled"` and a non-zero exit; a stalled reviewer pass is restarted at most twice and then reported as `review failed: reviewer unresponsive` with the executor's result kept. `rush sessions why` shows the pulse age, the last persisted message and, for a running session without a pulse for 10 minutes, a `STALLED` line with its open tool calls (arguments only for read-only tools); `rush sessions locks` marks such rows `STALLED` in the PULSE column.
- New command `rush ps` shows which rush processes are using which models right now: one row per process with state, last model, roles, session, launch dir, pid and request/token/cost totals, including hidden consumers (title, summary, cache keepalive, agentic fetch, ping) with a per-purpose split, and a STALE-MODEL column when a live process uses a model no current slot points to (cause: `slot@start`, `per-call` or `session-override`). `--all` adds stopped and stale rows, `--model` filters, `--json` is machine-readable, `--watch` redraws every 2 seconds. It reads only the global heartbeat directory and the global settings file.
- Every model call of a rush process (agent turns, sub-agents, titles, summaries, cache keepalives, agentic fetch, `rush ping`) is now recorded in a per-process heartbeat snapshot next to the global settings file: requests, failures, limit hits, tokens and cost per model, attributed to the root session; a cache keepalive replay counts towards the turn that scheduled it. `rush logs prune` removes snapshots older than `--heartbeat-days` (default 7, `0` disables). Test binaries without an isolated global data directory never write the real snapshot directory.
- Every `rush` invocation now writes a `command_start` and a `command_end` record (command path, exit code, duration, first line of the error) into the daily audit journal, including failed, refused, panicking and usage-error runs and commands that exit early through their own exit codes (`ping`, `providers ping`, `login`, `models set/preset`, `run`). Arguments are recorded only through two allowlists (safe flag values and positional arguments of a few commands); prompts, passwords, keys and patterns are never written. Shell completion requests are not journaled.
- Models can only be changed with the `rush models` commands: every settings write that touches `models` stores a signature of the models block next to it (`cache_rev`), and start-up checks it once for the global and the workspace file. A mismatch (an edited models block, a missing signature while any settings lock exists, or a signature that is not a non-empty string) makes `rush run`, the web UI and every other full-app command refuse to start with an explanation; `rush models use/unset/bump` only warn, so the operator can repair the setting by writing it again, and `rush models state` reports `edited_directly`. Files written before this change without a signature are accepted while no settings lock exists.
- New commands `rush lock [--local] <password>` and `rush unlock [--local] <password>` protect the settings from changes (global by default, `--local` for the current workspace; the global lock takes priority), and a global flag `--password <password>` authorizes settings changes for one command (for the web server: for its lifetime). A wrong password fails the command before it runs; every blocked or applied settings change and every denied command is journaled in the audit journal with the launch directory. Keep the password out of agent sessions: it is visible in process lists, shell history and transcripts.
- Loading the configuration no longer rewrites a model slot that is missing from the provider catalog onto disk: the slot is replaced by the default in memory only, with a log warning, and the settings file stays as it is. Previously such a silent rewrite of the global `rush.json` could change the operator's model without their command.
- Foundation of the settings protection (in `internal/config`; the `lock`/`unlock` commands and the `--password` flag are wired in a separate step): every user-initiated settings write is checked under the same file lock as the write itself; two levels — global (all folders) and local (only processes in that folder), with the global one taking priority. Housekeeping writes (`recent_models.*`, OAuth token refresh) are not blocked. The refusal is written for agents: "settings change is forbidden by the user … ask the user instead". The lock fails closed: a lock source that cannot be read refuses the write; an environment redirect (`RUSH_GLOBAL_DATA`, `XDG_DATA_HOME`, `LOCALAPPDATA`) or `-D/--data-dir` no longer hides a lock from a process that writes shared files (an isolated redirected settings file itself stays writable); path spellings that sjson resolves to a reserved key are refused; `rush migrate` refuses to touch settings while a lock is active (dry run still works) and now returns a walk error instead of printing it and exiting 0.

- The reaction chain guard now treats a relaunch of the byte-identical async command from a reaction turn as idle (previously only literal `sleep`/`echo` were idle): an orchestrator that waits for a sub-agent by relaunching the same command on every completion (`… git log … >/dev/null; echo q`, 24 times in 3 hours) has its automatic turns refused after `ReactionChainLimit` attempts. The first launch, a different command and any real action (edit, write, read) stay progress and clear the memory of the last command.
- Every `rush.log` line now carries a `launch_cwd` field — the directory the process was started from (before the `--cwd` chdir); it differs from `ws` when the process is launched from another folder. New package `internal/audit` — a central append-only journal `audit-YYYY-MM-DD.jsonl` next to the global `rush.json` (one record per line, one file per day, no rename rotation); `rush logs prune` now also deletes journal files older than `--audit-days N` (default 30; 0 — all), after truncating `rush.log`.
- `rush models state` prints a `RESTORE` block — a list of `rush models use --<role> <model-effort> [--local]` commands that re-create the current models by role (smart, fast, worker, reviewer) with efforts, one command per explicitly set slot and scope; `--json` gets a `restore_commands` array. For Claude/local-CLI atoms without a stored effort the `provider/model` form is printed, because `models use` rejects a bare atom.

## [0.2.0-alpha.9] - 2026-10-06

- A successful step retry after a provider stream cut (including Codex mid-body) no longer appends the full answer to the partial one already accumulated: the cut attempt's text and reasoning are discarded on the retry's first delta (in the persisted message too). If the retry produced no delta at all (all internal attempts failed), the partial text stays — the coordinator's continuation relies on it, so the result still contains both halves of the answer.
- A `rush run` loop reviewer turn that failed for its own reason no longer masks `rush sessions cancel` and limit overruns: the run ends `canceled` (flag cleared) or with a limit error, not with success and the executor's saved answer.
- Reviewer evidence: a bash-command completion notice that arrives in its real shape (header `Async job <call id>` and body `Background job 001 ...`) is bound to the command by call id, and a non-zero exit that bash reports as an "Exit code N" tail under the "finished." header is no longer reported as "exit 0".
- `rush run --model B --effort E` (and `--role fast --model F --effort E`) sends the explicit effort on the first turn again: it travels on the model override of its slot instead of only being written to the session row (a regression of the effort-inheritance change: while the model slot was not yet written, the effort was lost). Effort is still not inherited when the model is switched without an explicit flag.
- `rush sessions cancel` or exceeding `--max-cost`/`--max-tokens` during a todo-reminder turn is no longer masked by dropping the reminder: the run ends `canceled` (flag cleared) or with a limit error, not with an `end_turn` success.
- `rush sessions why` and `rush sessions list` no longer show "waiting for your answer" for a question of an already finished, voided or vanished delegation: the question is dropped by the same rule as when pulling notices; reading goes through the reader pool.
- `rush sessions cost --by day`: a group that mixes priced and unpriced sessions no longer takes the "priced" flag from the group's last session and prints `n/a (no price)`, and `--json` returns `priced:false`.
- `rush sessions list`: the COST column shows the subtree budget (including delegation spend), like `--json cost_usd`, instead of the root's own cost only.
- `rush sessions compact` no longer brings up the whole application (MCP servers, coder agent, run-queue pump, interrupted-turn recovery): it needs only the config and the DB, so leftovers in a crashed process's queue cannot start a paid turn in the middle of VACUUM.
- In terse mode without `captureResult` (the SDK default) a successful review no longer replaces the result: the executor's answer is printed, not the reviewer model's text, as on the CLI/JSON path (A10: the review verdict is an additional field); on FAIL/"unparsed"/"unverified" the verdict is visible as a `reviewer verdict: …` line on stderr. In stream mode the reviewer's text is still streamed as it arrives.
- With `notify_on_background_job_done=false`, the completion of a delegated child's background shell releases the parent's delegation immediately: the row observer drops the hold itself and triggers a re-check instead of waiting for the 60-second pass. With notifications on, the hold is still dropped by the notifier after the notice is saved.
- The todo-reminder turn about unfinished todos no longer redoes the run session's setup: it does not erase a pending `rush sessions cancel` request and does not overwrite a model change the operator made mid-run (setup happens once with the first turn, as in Drain).
- A Drain started after the review pass of a `rush run` loop no longer replaces the result: `final_text` stays the executor's answer, and `review` and `review_verdict` are not lost.
- On Unix, Ctrl-C on the `rush login` link screen no longer copies the link but aborts the login the same way as SIGINT (before, the login waited up to 10 minutes); the slash-command instructions no longer promise delegations in the open-jobs count — only the session's own jobs are counted.
- `job_kill` of a background shell releases the completion hold immediately: the parent's delegation no longer waits up to 10 minutes if the shell process does not exit.
- A race between `job_kill` and the shell's natural exit no longer produces two contradictory notices ("stopped" and "finished") and an extra wake: the decision is made under the common notice-arrival lock; an already finished process answers "already terminal" with the real output, and a completion that lost the race inserts nothing.
- Reviewer evidence block: a git warning on stderr from a successful command is no longer parsed as a `status` entry or a branch header, and the overall 6000 limit is counted in characters, not bytes — with Cyrillic the trailing sections (commands, tool calls, final-text flags) are no longer cut off twice as early.
- A failure of the todo-reminder turn for unfinished todos (provider error, busy session) no longer turns a successful executor turn into `exit_reason=error`: the reminder is dropped with a `todo reminder turn failed` warning, the executor's answer stays the result, and there are no repeated reminders; a question asked by the reminder turn itself still ends the run as `awaiting_answer`.
- If reading the session cost before the run failed, the envelope and the `--on-finish` hook report zero run cost instead of the whole historical cost; the hook cost is never negative. The todo-reminder prompt no longer asks for the non-existent `cancelled` status: unneeded items are to be removed from the list.
- The reviewer slot and a web relaunch without their own reasoning effort no longer inherit the executor session's effort that is pinned to another model: the effort is taken from the session only if the override points at the same model of the same provider.
- The wake-schedule planner no longer spins idle for up to 5 minutes when a row is already leased and its due time has come (a failure while firing, a killed process): the timer aims at the lease expiry rather than at the past `next_run_at`.
- On Windows `rush sessions compact` checks the space reserve against the bytes available to the caller (taking disk quotas into account), not against the free space of the whole volume.
- A background shell no longer mistakes another process's row for its own: the shell id is a per-process counter while `rush.db` is shared, so `ClaimShell` with an id that a running row of a foreign host already holds is refused (the command must be relaunched, the id will be new), and a dead host's row is recovered first; `job_kill` by a foreign process's id answers "not found" and does not cancel the foreign row.
- A parent's answer to `child_question` reaches the held delegation regardless of the call's origin: an idle web parent or a nested parent woken by a notice no longer creates a second delegation, hangs waiting, or receives the child's result twice; the answering turn runs with the delegation's own origin.
- A process in a regular git checkout no longer disowns its own bound sessions when `git` is unavailable (not in PATH, `dubious ownership`, resource exhaustion): a git launch failure is not cached for the whole process, and the workspace root is found by walking up to `.git`.
- An explicit `--data-dir` or `options.data_directory` pointing at `<main>/.rush` from a linked worktree no longer makes the process "home": homeness is determined by the directory itself, not by how it was chosen, so the workspace binding and data migration cannot be bypassed.
- The `rush run` loop no longer exits in the window where the observer has already closed the background shell's row but the completion notice is not inserted yet: the undelivered notice keeps the scope open, and the shell's result is handled in the same run.
- Review-pass warnings added after the loop totals snapshot (late ones, from the reviewer turn) are no longer lost when the totals are merged: `loopTotals.applyTo` keeps them without duplicates.
- The background-shell row observer no longer repeats "no row" forever after the row disappears (for example when the session is deleted while the shell runs): after 30 s it logs a Warn and exits instead of holding a goroutine until the process ends; retrying other errors (busy DB) is unchanged (ox round 9, B9-3, #1207).
- The `fs_write` shrink guard no longer accumulates across the items of one call: each item's size is compared with the LARGER of the file on disk and the content after the previous item, so several items for one path do not shrink the file to (1/4)^N or zero it below the threshold (ox round 9, B9-10, #1208).
- The `rush providers update` dump no longer pins stale catalog numbers as "user-set": a positive `context_window`/`default_max_tokens` in `rush.json` that exactly equals the catalog value of the same model (for known providers the catwalk catalog, for openai-codex the account's live catalog) stops blocking the model facts table, so `gpt-6.*` shows 1 050 000 again instead of the raw 272 000 in the runtime, `rush models list` and the web; a value that differs from the catalog, and values of custom providers, stay user-set (ox round 9, C9-7, #1204).
- The review pass (`reviewer`) no longer sees every async command longer than ~3 s as "exit=unknown (still running?)": the exit code is recovered from the "Async job … finished./failed." notice (0 for finished, `Exit code N` from the tail for failed; timeout, stop, cancel and interrupt stay "unknown"), which removes false FAILs on good work (ox round 9, C9-6, #1203).
- A Codex response stream cut mid-transfer (truncated body, `unexpected EOF`) no longer ends a worker turn without a retry: the error is wrapped into a retryable provider error, like for the other providers (ox round 9, B9-4, #1202).

## [0.2.0-alpha.8] - 2026-10-05

- A loop schedule created by a turn after the scope's first close (a todo-reminder or review turn) no longer outlives `rush run`: loop schedules are cancelled on every scope close, not once per run, so no host of the data directory wakes the session on a timer indefinitely (ox round 9, C9-3, #1200).
- The unfinished-todos reminder no longer overrides the root's question: if the last turn ended with `ask_question`, `rush run` exits with `exit_reason=awaiting_answer` (the question reaches the caller) instead of sending a reminder on top of the question and ending with `end_turn` and code 0 (ox round 9, C9-2, #1199).
- `ExecuteRun` no longer panics (nil dereference) when the review turn failed in terse/stream mode without `captureResult` (the SDK default mode): the executor's answer stays the run's result, and the review failure is recorded through the nil-safe `recordReviewFailure` (ox round 9, C9-1, #1198).
- A background shell whose claim inserted the `async_jobs` row but could not announce it (`MarkAnnounced` failed with SQLITE_BUSY or the context expired) no longer leaves the row `running` forever on a live host: `ClaimShell` deletes the just-inserted unannounced row by its `claim_id`, and `rush run` does not hang on it until `--timeout` (ox round 9, B9-1, #1197).
- `fs_write` (the replacement of `write` in folder-restricted runs) refuses an item that would replace an existing file of 2 KB or more with content smaller than a quarter of its size: the refusal names `fs_replace`/`fs_write_lines` for partial edits, a deliberate replacement is `allow_shrink=true` on the item; new files and growth are unaffected (the same guard as `write`, #1196).
- `job_kill` by the shell_id of a background shell that has an `async_jobs` row (a shell from a synchronous bash call) moves the row to `cancelled` BEFORE killing the process: a process that refuses to die no longer leaves the row in `running`, and its exit does not write "finished: exit 1" or wake the session a second time; the `job_kill` result is merged with the row (#1189).
- The `write` tool refuses to replace an existing file of 2 KB or more with content smaller than a quarter of its size (before, a worker wiped the laws registry with one line): the reply explains that `write` overwrites the whole file and suggests `edit`/`multiedit`; a deliberate replacement uses the `allow_shrink=true` parameter (#1192). `fs_write` is unaffected.
- A background shell started in a turn without a completion callback (`notify_on_background_job_done=false`, SDK Drain/wake turns) no longer leaves the `async_jobs` row in `running` forever and does not keep the session scope open: the terminal transition is written by the observer registered at claim, independent of the notice (#1188).
- A worker's question (`ask_question`) is no longer written to rush.log as ERROR "tool call failed, ending the run": it is a normal turn pause, now INFO "tool paused the run: awaiting an answer" with `level_kind=pause` (#1194).
- A Codex stream failure with code `server_is_overloaded` / `server_error` / `rate_limit_exceeded` no longer ends the turn without a retry: it gets status 503/500/429 and is retried by the common mechanism (before, a gpt-6 worker died on the first server overload); quota-exhaustion codes stay terminal (#1191).
- The `max` effort for z.ai (GLM-5.3, glm-5.2…) and DeepSeek no longer fails the call with the error "reasoning model `max` not supported": the value goes only into `extra_body`, not into the top-level `reasoning_effort` field that the library validates against the OpenAI vocabulary. Before, every review pass failed this way with a reviewer slot at `@max` (#1190).
- `stop_agent` / `inject_agent` without `child_session_id` say where to get the id: from the `agent` response or from `inspect_agent` without arguments (before, a bare "is required"; #1186).
- The refusal to re-delegate to a running sub-agent ("still has work running") now says what to do: the result will arrive by itself, no repeat is needed, and you can write to it via `inject_agent`; the same advice was added to the orchestrator-mode rule. An orchestrator on wmeta repeated such a delegation 60 times in an hour (#1185).
- `internal/oauth/codex`: Codex login error messages start with a lowercase letter ("codex device authorization timed out" and the like), tests use context-aware network calls; `golangci-lint run ./internal/oauth/...` — 0 findings (#1184).
- A zero context window or output limit in `rush.json` (a `rush providers update` dump) no longer overwrites the catalog value of the same model: zero is treated as "unknown" and the catalog value is substituted; a positive user value still takes precedence (#1183).
- GLM-5.3 family context (glm-5.3, -flash, -flashx) = 1M tokens, max output 131072; a single table of documented model facts `internal/discover/model_facts.go` instead of several copies (#1181): the final overlay is applied once at the end of provider loading, the user's value from rush.json wins, zero = unknown; atom context labels in `rush models list` come from the runtime (glm-4.7 204k, glm-4.6v 131k).
- "Waiting for an answer" for a delegation whose child ended its turn with a question (#1158): `sessions why` prints a "waiting for your answer" section (the child, the question ≤200, the answer path `agent(resume_session_id=<child>)`, and without a live `rush run` the relay command `sessions inject <root>`); `sessions list` shows the sub-state "(awaiting answer)" and the `awaiting_answer`/`awaiting_child`/`awaiting_question` fields in `--json`; the web "Agents" panel puts an "awaiting your answer" badge with the question text on the delegation row (`awaitingAnswer`/`awaitingQuestion` in the live-work snapshot). The source is the already existing pending `child_question` notices of #1157; read-only, no new writes.
- Background shells outside the async wrapper got an `async_jobs` row (R-BG-1, #1126): the sync branch of `asyncTool` (SDK origin, Drain/wake turns), when bash goes to the background, claims a kind=`bg_shell` row (`tool_call_id` = shell id, fail-closed when the DB refuses), and the shell's completion writes state+`result_summary` in one CAS (`delivery=done/reacted=1`, the debt and the cap stay on the `bg_shell_done` notice); migration 20261005000001 widens the kind CHECK (table rebuild); the dead-host sweep moves running `bg_shell` rows to interrupted (DUR-6). Law ASYNC-13 (`docs/async-invariants.md`).
- `rush sessions compact` returns space from rush.db after deletions (SQLite VACUUM, #1161): it refuses while live processes use this DB (`--force` skips the host/session lock check, but not the startup lock or open transactions), checks free space beforehand, prints the size and freelist before/after; `--dry-run`, `--json`. `sessions gc/purge` suggest the command when free pages are ≥ 64 MB and ≥ 50% of the file. There is no automatic VACUUM.

- A linked git worktree now works on the project's shared data directory
  `<main>/.rush` (#1143 SD-D): sessions, logs and `async_jobs` are written there and
  survive `git worktree remove`; directory selection order: `--data-dir` →
  `options.data_directory` → an existing local `.rush` with `rush.db`
  (legacy, kept; without the shared-mode marker it stays local, with the
  marker it is ignored with the WARN "stray local DB ignored") → the shared `<main>/.rush`
  for a linked worktree → `<wt>/.rush/dev` for dev builds (`go run`, `go build -o`),
  so that a branch build does not touch the shared DB (and does not migrate it) → `<cwd>/.rush`.
  The source of the choice is visible in `ConfigStore.DataDirSource()`; `ResolveDataDirectory`
  (rescue commands) gives the same result; a reload changes neither the directory nor the source.
- The `reasoning_effort` label in message rows now records the effort of THIS
  call, not of the session row: a review turn's column gets the effort of the
  reviewer slot (for example `max`), not the one inherited from the smart slot (#1166).
- The review pass became a verifier (#1165): the reviewer turn gets a read-only set
  of tools (`view`, `grep`, `glob`, `ls`, `git_read`,
  `read_delegation_transcript`, `fs_read`/`fs_list`/`fs_find`/`fs_grep`) —
  writes, shell, sub-agents, MCP, network and `ask_question` disappear from the set
  (through the same per-call role filter that forbids sub-agents). There is no
  call budget: the reviewer reads as much as it needs for a confident verdict;
  the pass is bounded only by a 60-minute hang safeguard.
- The reviewer prompt was rewritten and is paired with a deterministic evidence block
  `<review_evidence>`: an explicit role change (the orchestrator's work is finished, the reviewer is
  another agent), "who wrote what" (the messages above are other people's claims, not
  observed facts), mandatory checks (the request, the changes, at least three
  file-level claims, vacuous tests, stubs, the final answer, the process)
  and the verdict rules. The block is computed by rush itself from the git tree and the session DB:
  changes made during the run, the original request, the latest test/build commands with
  exit codes, open todos, call counters, `final_text` quality flags.
- The JSON envelope gained a `review_verdict` field (`pass`, `pass_with_notes`,
  `fail`, `unverified`, `unparsed`, `error`). A pass without a single own
  read tool call is downgraded to `unverified`. The verdict is the opinion of a second
  model and does not change the exit code; a failed review turn no longer replaces the executor's
  answer — the primary result is kept with a warning.
- The in-turn "no progress" guard (#1149, steps 3–4): a turn stopped by the guard
  is now visible — one warning in the `warnings` of the `rush run --json` response
  ("turn stopped by the in-turn progress guard: …", kept even if another turn
  became the answer) and one stderr line per message; `exit_reason`
  stays `end_turn`. The session's system prompt is reconciled with the call's tool set:
  if a worker appeared/disappeared in the set (orchestrator mode), the stored
  prompt is rebuilt and re-saved, so rule 7 is no longer missing
  from the prompt of a turn built before the worker was configured (A24).
- A session's async-job cap refusal now counts as a no-progress step (#1155, step 3):
  a launch that did not happen because of the limit (`async_cap` metadata in the tool response)
  is classified as a refusal, so a turn in which the model, with the limit busy, repeats
  a real command is stopped by the "no progress" guard after H=5 steps instead of
  an endless paid series. The single decision point is `classifyStepCall`
  (`internal/agent/step_progress.go`); the reaction chain guard reads it the same way.
- An empty wait with `cd` no longer counts as work (#1150): `cd <dir> && sleep N; echo X`
  is now recognised as a wait — `shell.IsNoOpCommand` accepts a leading literal
  `cd <path> &&` before an empty command (`cd` observes nothing). Before, such timers
  slipped past the "no progress" guard and did not make it into the timer list of the async-job
  cap refusal. The path must be literal: `cd $D && sleep 1`, `cd $(foo) && sleep 1`,
  `cd x; sleep 1`, `cd - && sleep 1` and a lone `cd x` are not waits.
- Reliability: the supervision check-in no longer crashes the process when the coordinator
  has no agent yet — `agentFor` returned nil and a panic in the timer goroutine took down
  the whole process (seen in a full internal/agent run); such a fire is now a
  no-op. The test `TestJQ_CtxCancel_MidReadAll` was rewritten deterministically: cancellation
  on the reader's signal ("N chunks read") instead of a wall-clock race
  (50 ms timer, 1 s threshold) that falsely failed it under load.
- Context of GPT-6 models in the Codex catalog (#1172): `gpt-6-luna`, `gpt-6-sol`,
  `gpt-6-astra` — 1 050 000 tokens per the official OpenAI pages (input up to
  922 000, output up to 128 000). An understated catalog value for these models
  is raised to the documented one, a missing one is taken from the table;
  the other `gpt-6*` models without a catalog value get the same 1 050 000.
- Provider loading (#1174): an `openai-codex` entry with an OAuth token no longer
  prints the false WARNs "Provider is missing API key" and "Skipping custom provider
  due to missing API endpoint" — such an account has no API key, and the address is set in
  code. An entry without a token still warns about both.
- Obsolete model families hidden (#1171, #1173): for the `openai-codex` provider
  the lists (`rush models`, `rush models list`,
  `rush providers fetch-models`, the web picker) keep only `gpt-6`
  and newer models (gpt-5.x, o3, codex-mini are hidden); for all providers GLM models
  below 5.3 are hidden (glm-5.3+ are visible, an entry like `cc-glm-5.1` is filtered too).
  Unparseable or unfamiliar ids are not lost. An old model chosen explicitly in the config
  keeps working and does not break config loading.
- Binding sessions to a working directory (WS-1, #1142, step C): a new session
  remembers its checkout root and branch (`workspace_root`, `git_branch`;
  the branch is read from `.git/HEAD` without launching git), and the run queue stores
  the root too. Migration `20261002000001` is additive: old rows and old
  binaries have an empty value. A process serves only the sessions of its checkout and
  old unbound rows if it is "home" (owns its data
  directory); for a foreign session `--session`/`--continue`, a turn, queue
  processing, the wake scheduler, restart and "delete the others" in the web are refused, and
  the turn arbiter defers its debt without pacing or re-checking. Until SD-D (#1143)
  all processes are home (`config.WorkspaceHome()` is always `true`), so for
  existing sessions behaviour does not change.
- Flake `TestTwoAppScenarioA_CrashThenContinueDeliversOneInterrupted` (#1167):
  the recovery scenarios held the job with a command for 3 seconds by the wall
  clock (`ping -n 4` / `sleep 3`), and under load by the time of the state check the row
  was already `completed`. The command now waits for a sentinel file (running until released by the
  test, finishing one iteration after), and the cleanup releases it and
  stops the application before `t.TempDir` is removed, so an orphaned
  process does not hold the directory on Windows.
- Flake `TestRunNonInteractive_PeakRefusalNeverSettles` (#1175): the test set the peak-hours
  window `00:00`–`23:59`, and the end of the window is exclusive, so from 23:59:00 to
  23:59:59 local time the provider was not "in peak" and the turn reached
  the provider (the test failed once a day, caught at 23:59:46). The window is now built
  from the current time (an hour either way, wrapping past midnight).
- A worker's question no longer drowns the orchestrator (#1157): if a delegated
  worker called `ask_question` while its own background
  jobs were still running, the parent immediately gets a `child_question` notice (wake=1) with the question
  and the exact answer/decline calls; `inspect_agent` shows
  `awaiting_answer`; the supervision tick marks the delegation "AWAITING YOUR
  ANSWER"; an answer via `agent resume_session_id=...` is accepted into the
  held delegation (no new row, the result arrives under the original job),
  the race with release is closed by the `answerHold` flag.
- Orchestrator documentation for the shared data directory (#1144 SD-E): the `/rush`
  and `/wrush` texts say that a session id must be unique within the project (all
  linked worktrees write to one DB `<main>/.rush`, a repeated id continues the old
  session), that a session belongs to the checkout where it was created (`--session` from another
  checkout is refused with "belongs to <root>"; history can be continued there only by
  `rush sessions fork <id> --cwd <path>`), that `rush sessions list/locks/watch/why`
  from the root see all agents, and a worktree with an already existing local
  `.rush/rush.db` keeps using it.
- `rush sessions audit` (#1179): session timestamps were read as milliseconds,
  while in `rush.db` they are in seconds, so on a real DB the CREATED/UPDATED column
  showed 1970 dates and the `--since` window contained no messages at all.
  The tests agreed with the mistake (the fixtures were in milliseconds too); now
  seconds, and the regression test takes the time from the real session service.
- `rush models list` and the picker showed a context of 272k for `openai-codex/gpt-6-*` instead of the
  documented 1 050 000 (#1180): the values came from the account catalog cache
  (`model-catalog-openai-codex.json`, lives 7 days) written by a version before the #1172 fix,
  and the cache served them as they were. Now the context rules are re-applied to Codex models
  when serving from the cache; the threshold is extended to all `gpt-6.N` releases of the
  astra/sol/luna lines (in particular `gpt-6.1-sol`), a larger declared value is kept.
- `rush models list`: the context in millions is printed with two decimals (1.05M, 1M, 1.5M)
  instead of rounding to one (1 050 000 read as "1.1M", i.e. as a different window).
- Flake `TestHeartbeatTouchesFile` (#1168): `Release` returned control before
  the heartbeat goroutine closed the lock file, and the `t.TempDir` cleanup on
  Windows failed with "file in use"; heartbeat tests now wait for the file to be closed.
- New subcommand `rush logs summary` (#1164): a summary over one or
  several `rush` log files — how many WARNs and ERRORs of each text
  (counters by `msg`, with an example of one group line's fields), which processes
  started (`process start`: version, command, ws, session, data_dir, in
  order) and where the log was silent longer than a threshold (`--gap`, default 15m).
  The command is strictly read-only: only `os.Open` for reading, the log is not written, not
  truncated and not deleted (that is `logs prune`), the DB is not opened. Lines
  that are not JSON are counted as irregular instead of failing the command; empty
  sections are printed as `(none)`. Flags `--since`, `--file` (repeatable),
  `--gap`, `--json` and the inherited `--data-dir`/`--cwd`; without `--file` `<data-dir>/logs/rush.log` is read,
  resolved in the same read-only way as `sessions audit`.
- `rush sessions cost`: the model is no longer "(unknown)" (#1163). The session's
  `smart_model_id` field is written only by an explicit per-session override
  (web picker, `/model`), so an ordinary session that inherited the default model
  had NULL and fell into the `(unknown)` group; now in that case
  the model that actually generated the session's messages is taken
  (dominant by message count, ties broken by tokens). Also:
  a model without set prices no longer prints `$0.000` — such a row gets
  `n/a (no price)`, and TOTAL gets a `+ N unpriced` counter (tokens are
  counted as before). The price is set by the keys `cost_per_1m_in`,
  `cost_per_1m_out`, `cost_per_1m_in_cached`, `cost_per_1m_out_cached` (USD per
  million tokens) of the model in `providers.<id>.models[]` — this is now written in the
  command help. In `--json` every row gained a `priced:false` field, and for
  `--by total` — `unpriced_sessions`.
- New subcommand `rush sessions audit` (strictly read-only, #1162): scans the session
  DB for anomalies — repeated views of one file (≥ 3),
  repeated normalised bash commands (≥ 3), "waiting" commands
  (≥ 3), tool errors by class (≥ 3), notices of
  supervision/wake_failed (≥ 1) and unfinished async_jobs
  (running/interrupted, ≥ 1). Flags `--all`, `--since`,
  `--worktrees`, `--json` and the inherited `--data-dir` are available. The DB is opened in
  `mode=ro` + `PRAGMA query_only` mode (without setupApp, which would open
  it for writing and run migrations): no migrations, locks or writes —
  the rush.db file does not change, and -wal/-shm/journal files are neither created nor
  deleted. For each DB a DB health block is printed (size,
  page_size × page_count, freelist and the percentage of free space). An unfamiliar
  or too old schema is merely reported — the command succeeds
  and creates nothing.
- The in-turn "no progress" guard (A22/A23, #1149 steps 1–2): after S=2
  consecutive steps without progress (an empty `sleep`/`echo` wait, a repeated
  read of already-read file windows) such calls are refused softly, after
  H=5 the turn is ended by the guard; the call classifier is one for the turn guard and for
  the reaction chain guard (#1113), a guard refusal is neutral for the chain.
- bash/run_command/agent: the `timeout` parameter accepts a bare number of seconds
  (#1159): `"timeout": 300` now means `{"seconds": 300, "kind":
  "wake_only"}` instead of a cryptic unmarshal error; a string and garbage
  are rejected with a clear text.
- inspect_agent without `child_session_id` (#1160) instead of the error "child_session_id
  is required" returns the list of the session's live delegations (id, tool,
  status, age, last activity); exactly one delegation — straight its
  full summary.
- Session async limit (A27, #1155): the limit of 50 now counts only
  unfinished jobs — a finished but not yet delivered job no longer
  occupies a slot, so a lost acknowledgement does not block new
  bash jobs forever. The refusal on a full limit now explains that
  nothing is queued, lists up to 10 running jobs (timers
  first) with `async_cap` metadata and prescribes freeing slots with
  `job_kill` or ending the turn without a tool call (not repeating the call
  and not asking a question via ask_question). The same advice is now in the
  message of the background shell process limit (`maximum number of background
  jobs`).
- DB migrations became safe for a DB shared by several processes
  (preparation for step B of the shared-data-dir design): the whole sequence
  "read versions → decide → apply" now runs under
  the inter-process file lock `<dataDir>/migrate.lock`; before that
  a parallel start of two rush processes after a deploy led to
  `duplicate column name` in the second one. A version-set check was added
  (applied vs built-in): when the schema history diverges
  (`unknown` + `pending`) the process does not start, failing with `ErrSchemaDiverged` instead of
  a silent migration; with only `unknown` the DB is opened without migrations with
  a single WARN. The `WithMayMigrate(false)` option appeared
  (`ErrSchemaMigrationNotAllowed` when pending) — no path sets it
  yet, the default behaviour has not changed. `FileLock` was moved
  from `internal/session` to the leaf package `internal/filelock` without changing
  behaviour (alias wrappers are left in session).

- `rush sessions kill` / `cancel` / `reset --force` (A26): they now also remove
  the session's queued runs, pending injects and orphan-call outbox rows. Before,
  a session killed with a queued `sessions inject` was resurrected by the next
  rush process on the same data directory (its run queue pump claimed the
  leftover rows) and kept running. Conversation history is not touched; an
  inject into a session that was not stopped is delivered as before.

- CLI and web: short commands (`bash`/`run_command` that finished within ~3
  seconds) now return their result directly in the tool response
  instead of "Async bash job ... started" and a separate notice later.
  Models can see this: the result arrives immediately, in one message, without
  an intermediate turn; long commands and all other cases behave
  as before.

- `rush run` (A15): a top-level `ask_question` asked while the session still
  has its own running background tasks no longer ends the run with
  `awaiting_answer` (which orphaned that work). The tool hands the model a
  hint — finish the turn without tool calls, results arrive as messages —
  and the run continues. Without running own work the behavior is unchanged.

- `rush run` (A18): ending a turn while todos are still pending/in_progress
  no longer silently ends the run on an announcement. The loop fires at most
  `cliTodoNudgeLimit` unfinished-todos reminder turns (budget resets when the
  todos change); once spent without progress the run ends with an
  "ended with unfinished todos" envelope warning.

- Changed: `rush run --json` (A10) — with an automatic reviewer pass,
  `final_text` now stays the executor's own last answer, and the reviewer's
  verdict moved to a new additive `review` field (empty when no reviewer
  ran). Previously the verdict overwrote `final_text`.
- `rush run` agents working in a linked git worktree can no longer mutate
  shared git state (`checkout`/`reset`/`commit`/`add`/`stash`/… via the
  bash tool): such commands are refused with "git writes are not allowed
  for this run; the orchestrator commits". Read-only git is unaffected;
  runs rooted in a main checkout behave exactly as before.

- Commands launched by an agent's bash tool now run with
  `GIT_OPTIONAL_LOCKS=0` (an explicit user value wins), so a timed-out or
  killed `git status` can no longer leave a stale
  `.git/worktrees/<n>/index.lock` that blocks later git writes.

- Calling a non-existent tool now returns the closest real tool name
  ("unknown tool \"gash\" — did you mean \"bash\"?") or, when nothing is
  close, the list of available tools — instead of a bare "tool not found".
- `rush run` cost model (#1130): "cost including children" is now a query
  over the delegation tree at read time, not a transfer ledger. Each session
  charges only its OWN cost (`cost_self`, monotonic); the per-node columns
  `cost_self`/`cost_base`/`cost_parent_id` are added by migration
  `20261001000001_run_cost_subtree.sql`, whose backfill preserves the old
  displayed totals exactly (additive only: `cost` stays as a mirror for old
  binaries; rollback = drop of the new columns). Visible contract changes:
  `cost_usd` keeps its meaning (subtree budget since the last
  `sessions reset`), `sessions show --json` adds `own_cost_usd`;
  `--max-cost` now compares the root's subtree budget (a delegated child's
  spend is visible to the cap the moment the child spends, previously only
  after transfer points); `--max-tokens` still compares the root row only.
  `sessions reset` no longer lowers any cost: it freezes the budget
  (`cost_base`), so a reset can never lose spend from the ledger; the
  `rush run` cost window (`delta_cost_usd`, `RUSH_COST_USD`) is a subtree
  difference and is unaffected by resets. `sessions cost --since` and
  `--continue`/list ordering now follow delegation-subtree activity
  (max `updated_at` over the tree) — a busy child keeps its root visible
  without writing the parent's `updated_at`.

- Changed: skills from other tools' directories (`~/.claude/skills`, the
  project's `.claude/skills` and `.cursor/skills`) are no longer advertised
  in the agent system prompt; the discovery defaults that feed the prompt now
  list only Rush's own dirs and the Agent Skills spec locations
  (`~/.agents/skills`, `$XDG_CONFIG_HOME/agents/skills`, `.agents/skills`,
  `.rush/skills`). Slash-command loading from those directories is unchanged
  (skills installed by other tools remain available as `user:`/`project:`
  commands), and an explicit `options.skills_paths` entry still adds any
  directory to the prompt.

- Web session list: the sidebar's liveness and external-ownership
  annotations now come from the same session-activity classifier the
  `sessions` commands use (one batched read per poll, previously a second,
  independent lock-inspection model per session). Visible differences: a
  session whose lock holder is dead no longer flips "externally owned"
  while its heartbeat mtime is still fresh; the ownership check no longer
  trusts mtime alone. The read-only badge can therefore clear slightly
  earlier for a crashed holder, together with the `sessions list` verdict.

- `sessions` (`sessions list/why/watch/tail/locks/reap/inject/cancel/reset`):
  the "is this session alive, and on what" verdict now comes from ONE
  classifier (`sessions why` prints the same verdict `sessions list` shows).
  Behavior changes by design: live work (a live `rush run` driver, a running
  job, a live delegation, an open once schedule) outranks a dead recorded
  lock PID (shown as a "stale lock" annotation, not a crash); "done" is only
  ever derived from a run's `ended_reason` -- a clean `end_turn` finish
  without one reads at rest; a dead-PID lock with no live work is "crashed"
  even when the last turn finished cleanly; debt (a pending reaction) keeps a
  session open only while a live driver exists; a driver marker or running
  job on a provably dead host is reported as "crashed"; a freshly released
  (empty) lock file is no longer "running" for the 20s heartbeat window.
  `sessions locks` / `--prune` map lock files to real session ids through the
  shared stem map, so slug ids get the between-turns label and the prune
  guard.

### Added

- **Limit reset time in machine-local time**: a provider's zone-less
  "limit will reset at <stamp>" hint (z.ai, CST) is now rewritten to the
  machine's zone with an offset and a countdown in `rush run` stderr, the
  JSON envelope `error` and the turn's finish message, instead of showing
  the remote server's wall clock.

- **CLI wake-schedule lifetime (stage 5a, #1106)**: a ONE-SHOT wake timer
  (`wakein`/`wakeon`) now holds `rush run` open — the process waits for the
  fire, reacts to the `wake_fired` event and exits as usual; `--timeout`, the
  default 6h cap, Ctrl-C and `sessions cancel` stop the wait as `canceled`
  (an overdue timer is never caught up). A `loop` schedule never holds a
  non-interactive run: when the scope closes with no other open work, the
  session's loop schedules are cancelled at run end (one stderr line and a
  `--json` warnings entry), so an endless timer cannot pin the process.
  `sessions why`/`list`/`watch`/`tail`/`reset` report a session held open by
  a once schedule as running and name the schedule
  ("waiting on wake schedule <id> at HH:MM"); `sessions cancel` also cancels
  the session's wake schedules so the waiting run is not orphaned on its
  timer. Registry: SCHED-10 in `docs/async-invariants.md`.
- **Reaction chain guard (#1113)**: a session that waits on async commands by
  re-launching pure wait commands (`sleep 90`, `echo tick`, ...) no longer
  chains unbounded empty automatic turns. A Drain leg that only launches
  wait commands (`sleep`/`echo`/`printf`/`true`/`:` via bash; `sleep`/
  `timeout` via run_command) with no real action is a "link"; after 3
  consecutive links whose entire reaction debt is their own completions,
  automatic turns for the session are deferred (`reaction chain without
  progress`): the CLI loop prints one stderr line, adds an envelope warning
  and exits leaving the results for the next turn; a web-driven session gets
  one advisory `reaction_chain` marker notice (never debt; voided by Rerun
  like `wake_failed`). Real work (edit/write/view/grep, a bash command with
  substance, a delegation, `job_kill`) or a human message resets the guard.

### Fixed

- **`rush sessions locks`: a session waiting between turns no longer reads as
  "stopping".** A released lock ages from the moment a turn ends, so for
  10-20 s the pulse showed "ping"/"stopping" for a loop that was simply
  waiting on its jobs; the live verdict now labels it "between-turns" at
  every age, and an in-turn holder the verdict proves alive is no longer
  shown "offline".

- **Log lines are no longer lost or overwritten when several rush
  processes write to one log.** `logs/rush.log` is now append-only
  (`O_APPEND`, no in-process rotation): the previous lumberjack writer
  truncated other processes' lines (non-appending open), lost records
  on rename-based rotation, and on Windows stopped a process from
  logging entirely whenever the file was held open by another rush or
  had reached the rotation size. Every record now also carries a `ws`
  attribute (the canonical checkout root) alongside the existing `pid`,
  and each process writes a single `process start` record (pid, ppid,
  version, command, cwd, ws, branch, data_dir, `--session`). Size is
  controlled externally with `rush logs prune`; its help text now
  matches the new behavior. lumberjack is gone from go.mod.

- **#1137 (B1): job_output no longer lies "not found … or already delivered"
  for a job that already finished.** A fast background command can finish and
  leave the ledger's in-memory map while its durable row is still
  delivery='pending' (the result is delivered as the next session message).
  job_output on such a job_id now answers with plain text -- "finished; its
  result is delivered as the next message -- end your turn now" (or an honest
  "already delivered above" once the row is delivery='done') -- instead of an
  error that spent the turn.

- **#1135 (A13): job_output(wait=true) on the session's own still-running
  ledger job now ends the turn.** The answer carries StopTurn ("end your turn
  now; its result arrives as a session message"), so the model cannot spend
  dozens of poll calls (up to 59 job_output per 19 bash calls observed on
  glm) waiting on work whose completion wakes the session anyway. A raw
  shell_id (background shells outside the ledger) keeps the old poll-hint
  behavior.

- **#1139: zai sessions no longer lose their cache hit ratio.** openai-compat
  providers are now classified as cache reporters by the configured provider
  ID (e.g. `zai`) instead of the wire type, so the cache-silent warm-up turns
  no longer degrade a whole session's aggregate to "no cache reporting" and
  `rush sessions cache` shows the real ~99% hit ratio; unlisted
  openai-compat endpoints (local, cache-silent) keep the honest n/a.

- **#1138: session title generation no longer fails on z.ai/glm reasoning models.**
  The title request now disables thinking at the request level (`extra_body.thinking`
  `{type:disabled}` for z.ai-configured providers; GLM-5.3 gets its minimal
  `low` effort), and when reasoning still streams the whole answer with no text
  part, the title is recovered from the completed reasoning stream (sanitised,
  length-capped) instead of falling back to "Untitled Session"; a
  length-truncated reasoning stream is never used. Misses are logged as
  warnings with finish reason and text/reasoning lengths.

- **#1101: a refused first turn no longer touches the session's state.** The
  preparatory writes a `rush run` / SDK turn performed before admission —
  clearing a stale `sessions cancel` request, persisting the budget, clearing
  `ended_reason` — now run at ADMISSION, once the turn owns the session's
  mailbox and inter-process lock (`sessionAgent.runOwned`). A first turn
  refused by another process's session lock leaves the owner's unread cancel
  request, budget and `ended_reason` untouched, and a web/SDK human turn that
  is locked out no longer spends the one-shot cancel flag (it is spent by the
  turn that is actually admitted).

### Added

- **Web: wake events and schedules (stage 5b)**: the web UI now surfaces the
  wake/async lifecycle. Fired wakes and other system notices render with a
  kind-specific chip (`⏰ wake`, supervision, timeouts) on the left-side
  system-event card — never as a human-typed reply. Async tool-call blocks
  show a distinguishable outcome: `timed out`, `stopped by user` (job_kill)
  and `cancelled` are distinct from a plain `error`, derived from the
  completion notice the client already received. The live-work panel gains a
  Schedules tab: the session's durable wake schedules (kind, message, next
  fire in local time, occurrences, max_runs/until, state) via new
  `get_session_wake_schedules`/`session_wake_schedules` WS messages (same
  coalescing change-worker as the Commands/Agents tabs, no DB polling,
  session-isolated), and an explicit Cancel button (shared confirm dialog) →
  `cancel_wake_schedule` → store `CancelSchedule`; an unknown or foreign
  schedule id is an explicit "not found" error, a repeat cancel is
  idempotent, and the scheduler worker's timer is recomputed after the
  cancel commits.

- **Wake tools (stage 4c)**: `wakein`, `wakeon`, `loop`, `wake_list` and
  `wake_cancel` (wake-tools contract §1). The coder (and the root session)
  can schedule one-shot delays, absolute RFC3339 wake-ups (explicit zone
  required, past is a validation error) and recurring loops (minimum 5
  minutes, optional `max_runs`/`until`); list only its own active
  schedules; and cancel by `schedule_id` (a repeat or a foreign id returns
  the same "not found" error, never a disguised success). Limit: 20 active
  schedules per session. Every successful create/cancel recomputes the
  stage-4b worker's timer; in sessions without the worker the tools answer
  with an honest "scheduler unavailable" error. Fired occurrences carry the
  §5.1 texts (`Wake ... fired (scheduled for ...)`, loop occurrences with
  their `of <max_runs>` bound) as `wake_fired` notices — system events, not
  human messages. Task agents get none of the five. New invariant SCHED-09
  in `docs/async-invariants.md`.

- **Wake-schedule worker (stage 4b)**: a single per-process timer
  (`internal/agent/wake_scheduler.go`) over the `wake_schedules` store:
  sleeps until the nearest due schedule, recovers expired leases on start,
  never catches up missed intervals (one immediate firing after downtime,
  loops stay on their original grid), and delivers each fired occurrence
  through the ordinary durable wake path (`wake_fired` session notice +
  `coordinator.wakeSession` — a system event, not a human message). Wired
  into App startup/shutdown via the new `agent.WakeScheduleController`
  interface; adds `HasOpenOnceWakeSchedule` for stage 5a (an active
  one-shot timer will hold `rush run` open, a loop will not). New
  invariants SCHED-06…SCHED-08 in `docs/async-invariants.md`.
- **Durable wake schedules (stage 4a)**: a `wake_schedules` table (migration
  `20260929000005`) and the `session.WakeScheduleStore` service behind it:
  `once`/`loop` schedules owned by a session, lease-based `ClaimDue`,
  exactly-once `FireOccurrence` (fired occurrences reach the session through
  a `wake_fired` session notice), idempotent owner-checked cancel, lease
  recovery, the 20-active-per-session limit, and cascade/cancel on session
  delete/reset. Worker and tools are stages 4b/4c. New invariants SCHED-01…
  SCHED-05 in `docs/async-invariants.md`.
- **Sub-agent control tools** `inspect_agent` / `inject_agent` / `stop_agent`
  (plan stage 3, wake-tools contract §4): check a delegated child's status
  (running / awaiting_answer / idle / terminal), deliver a message into its
  running turn (or only queue it when idle; `interrupt:true` replaces the
  current turn), and stop just that child's turn and armed delegation
  (idempotent; history preserved; resume with `agent(resume_session_id=...)`).
  Ownership-checked against the caller session; `job_kill`/`job_output` on a
  delegation job_id now point at these tools.

- **Web UI: server-backed live-work panel (#1058)** — the Commands/Agents
  tabs are now populated by the server from the durable `async_jobs` table
  (`get_session_live_work` request + `session_live_work` push on change
  events; no polling), so work hosted by another process (a live `rush run`)
  is visible the same as local work. Rows carry state and terminal cause
  (running / done / failed / timed out / stopped by user / killed /
  interrupted), started/finished/last-activity times, the abbreviated
  command text, and the sub-agent's session id; a click still jumps to the
  tool call in the transcript. A snapshot covers only the requested session
  and its delegation descendants.
- **`rush sessions jobs <id>`** lists a session's own durable async jobs
  (bash/run_command/agent/agentic_fetch) plus its whole delegation tree
  (following `child_session_id`, any state): tool call id, kind, state,
  liveness (alive/dead/unknown, via the shared host-lock probe), host
  id/pid/label, started/updated times, delivery/wake/reacted, and a short
  result summary. Table and `--json`. A running row on a LIVE host owned by
  another process prints that host's PID and the command that stops that
  whole process (`kill -INT <pid>` on POSIX, `taskkill /F /T /PID <pid>` on
  Windows) and says so: everything else that host runs stops with it. `-INT`
  because rush catches SIGINT, not SIGTERM, and its graceful exit kills the
  job process groups; a plain `kill <pid>` would end rush and leave the job
  children running. The Windows form is forced (no cleanup runs; the job
  children die with the process tree, and with rush's kill-on-close Job Object
  when it could join one). `rush sessions
  kill` kills the holder of the SESSION lock, which nothing holds between
  turns, so it cannot stop such a job. There is still no job-level kill from
  the CLI.
- **`sessions gc --jobs-older-than <duration>`** purges terminal,
  delivered-or-voided `async_jobs`/`session_notices` rows older than the
  given age (`--dry-run` counts only). A `running` row, an undelivered
  (`pending`) row and delivered-but-unreacted debt (`wake=1, reacted=0`)
  are never purged, whatever their age; an age of zero or less is
  rejected. `--json` prints one extra first line
  `{"kind":"async_jobs","dry_run":...,"async_jobs":N,"session_notices":M}`
  with the purged (or would-be-purged) counts; the plain output reports the
  same counts on stderr. Opt-in: omitting the flag leaves job retention to
  the automatic 7-day pass (see "Changed", retention).
- **`sessions show`** now prints a one-line async job count summary
  (`N total, M running`) for sessions that own any, pointing at
  `sessions jobs <id>` for the full delegation-tree view.
- **`sessions why`** now prints an "Async jobs:" section: totals, each
  running job with its host and liveness, and the reaction-debt state. A
  completed job whose result no model turn has reacted to yet is reported
  as pending debt both when the result is already in history and when it
  has not been pulled into history yet (the latter used to print "none").
  The section reads the session's own rows. A root that only waits on its
  own live plain background job (bash/run_command, no delegation) is
  reported "running", not "done"/"at rest", by `sessions why` (naming the
  job), `sessions list`, and the web list (new optional `HasLiveOwnWork`
  field on the session; a crashed root stays crashed).

### Changed

- **`rush run` and a busy session.** The first turn of `rush run --session
  <busy>` fails fast with the session-busy error, as before phase 4 (the
  JSON envelope is still printed and `--on-finish` still runs). Only the
  loop's own later reaction turns (they can legitimately race a web tab that
  is pulling notices) retry on a lock-busy refusal: every 0.5s, for at most
  30s of continuous contention, with a message on stderr, then the run
  exits with `exit_reason: "error"`. A reaction turn that fails in setup
  before it reaches the provider (a read error on the durable run queue, a
  model override that no longer resolves) takes the same path: retried
  every 0.5s for at most 30s, visible on stderr, then `exit_reason:
  "error"`. Every exit after the first turn ran (lock-busy or setup give-up,
  cancellation, a wait error, a budget cap, stuck debt, scope closed)
  flushes the last completed turn's answer, and its cost, tokens, tool
  calls, warnings, sub-agent outputs and duration cover every turn of the
  run. A run refused before its first turn (the session is driven by another
  loop), or whose first turn fails before it is launched (a session-setup
  write or the pending-work drain failed), has nothing to flush: it exits with
  that error, no envelope is printed and `--on-finish` does not run (no run
  started). While the loop waits on running work it prints
  a stderr heartbeat naming the job and host it waits on, once when the
  wait starts and then at most every 60s over the whole run, and a
  persistently unreadable database ends the wait after 30s instead of
  retrying forever. A Ctrl-C or `--timeout`
  that lands right after a turn finished cleanly ends the run with the
  cancellation error and `exit_reason: "canceled"` (it used to exit 0 with a
  canceled envelope).
- **`rush run` no longer waits on debt it will not react to.** The loop asks
  the same session policy the web wakes use whether a reaction turn is
  allowed: a notice no automatic turn is allowed for (an SDK background-shell
  completion while `auto_resume_on_job_done` is off, a released delegation
  child, a session under a live foreign driver, a Stop-suspended session) is
  neither waited on nor settled. The loop ends once nothing is running,
  prints one line on stderr, and the notice stays for the next turn; the
  exit reason is unaffected. Before, such a notice kept the loop re-checking
  every 5s until `--timeout`/Ctrl-C.
- **A second `rush run` on a session another live `rush run` loop already
  drives fails fast, before it starts a turn or writes to the session,
  naming the pid** ("session X is already driven by another `rush run`
  (host H, pid P, alive); wait for it to finish"). The refused run has
  already done its once-per-run dead-host sweep and retention purge and
  registered its own host, which lives until it exits. A host whose
  liveness cannot be determined counts as alive; the marker of a crashed
  loop is taken over. The loop keeps its claim until it exits (also on
  Ctrl-C).
- **A web process never runs reaction turns on a CLI-driven root.** The
  driving loop is recorded durably in the new `session_drivers` table (host
  id, pid; the driver is alive exactly while its host lock is held, no
  clock involved). While it is alive no other process starts a reaction turn
  for that session, and a reaction turn that was already admitted only moves
  notices into history without calling the provider; the loop sees the debt
  within 5s and reacts itself, so its final answer and JSON envelope carry
  the reaction. If the loop dies (`kill -9`) the web process takes the
  session over on its next hint or its 60s pass. To make this possible
  `rush run` registers a host (a `hosts/<uuid>.lock` file plus an
  `async_hosts` row) as soon as it claims the session, even when it never
  starts an async job, and keeps it until the process exits; a clean exit
  removes both and a crashed one is reaped by the ordinary dead-host purge.
- **The web auto-turn cap applies only to SDK background-shell
  auto-resume** (`AutoResumeOnJobDone`), as before phase 4. Wakes for async
  jobs, delegations, supervision check-ins and `wake_only` timeouts are
  uncapped again (an intermediate phase-4 build had capped them). Stop still
  pauses every kind of automatic turn until the next human message, through
  a per-session suspension state of its own; five background-shell
  auto-resumes fill only the cap counter and no longer pause async-job,
  delegation or supervision wakes. A human message clears both.
- **Stop is transitive over the delegation tree and leaves a "cancelled"
  notice per stopped job.** Stopping a session cancels the live turn of the
  session and of every session below it through running delegation rows,
  stops their jobs, pauses automatic turns for all of them and durably
  clears `wake` on every pending or delivered notice of the tree, so a job
  that finished a moment before Stop cannot grant a stopped delegation a
  turn. Each stopped plain job (bash/run_command) is recorded as a
  `cancelled` row that wakes nobody; its "cancelled" notice is delivered on
  the session's next turn (turn start or step boundary). A released,
  timed-out or stopped delegation's child session gets no further reaction
  turn; it is recognised by a delegation row of its parent, so a session
  created by `sessions fork --child` is not affected.
- **`job_kill` answers from the committed row.** If the job finished (or
  was stopped by Stop or a timeout) between the call and the stop, the tool
  says so with the committed outcome and does not touch the shell; a repeat
  call reports "not found ... or already stopped". When its own stop wins,
  its result stays the tool's answer (the real output captured before the
  stop) and is also recorded as delivered, so it is never pulled into
  history a second time. Applies to a background `bash` job and a
  `run_command` job. If that result cannot be recorded with the row (an
  error result, a failed write, a cancelled turn) the row goes back to the
  ordinary pull, so the captured output is delivered instead of lost; a
  stop whose shell is already gone (a concurrent Stop or timeout) still
  answers with the captured output.
- **Rerun (web "rerun from here") is one atomic step.** The target message,
  the tail after it and the ledger reconciliation commit in a single
  transaction. If the transaction fails, nothing was changed and the error
  says to retry; a cancel before the commit point also changes nothing. Only
  the deleted tail's jobs are stopped, after the commit (recorded as stopped by
  the session, like Stop: `cancelled`, no wake, the row stays void); jobs started before
  the rerun point keep running and their result reaches the new branch as a
  notice. A delivered notice whose message was in the deleted tail is
  delivered again to the new branch; a `wake_failed` marker in the tail is
  dropped, since it described the deleted branch. Rerun now cancels only the
  live turn instead of a full Stop: it no longer stops kept jobs, pauses
  automatic turns or clears `wake` for the tree.
- **`agent`/`agentic_fetch` with `resume_session_id` now refuses immediately
  (before reporting "started") if that child session still has a delegation
  running from a previous call, instead of queuing behind it.** The async
  job ledger's durable core (phase 4) enforces at most one running
  delegation per child session; a second one arrives as a tool error asking
  the caller to wait for the first result. If the conflicting row's host is
  provably dead, it is recovered in the same claim attempt and the new
  delegation starts immediately instead of being refused.
- **A tool-call id may be reused once its earlier call is history.** A
  provider that numbers calls per response (`call_0`) starts a new job again
  instead of being refused for up to 7 days ("already started earlier"): a
  row that is terminal and delivered (or voided) is renamed out of the
  active key (`<id>#reused#<uuid>`, still addressable by its message for
  Rerun) when the id comes back. A terminal row that is announced but not
  delivered yet is renamed the same way (its notice is still delivered, under
  the id the model used); only a terminal row whose "started" result has not
  been written yet keeps the id: a repeat of that call is refused with a tool
  error ("was already started earlier; not starting it again", or, for a
  different input, "is already in use by an unfinished call with different
  input") and never starts a second executor. `job_kill` attaches its result
  message to the killed job by its claim, not by the id, so a reused id cannot
  misdirect it.
- **Notices are durable rows, visible in history only when a turn pulls
  them.** Async job outcomes, supervision check-ins, `wake_only` timeout
  check-ins, wake-failure markers and SDK background-shell completions are
  `async_jobs`/`session_notices` rows. A session's own turn moves them into
  history (turn start or step boundary, one transaction per notice; nothing
  writes them into a session that is between turns) and reacts to them where
  the session policy allows a turn. Elsewhere (a CLI-driven root between
  loop turns, a Stop-suspended session, causes that never wake such as Stop
  or `job_kill`) a notice waits for the next turn. Every such message
  carries `AutoResumed` and `BackgroundJobNotice`, plus its `NoticeKind` for
  supervision, timeout and wake-failure notices.
- **Async job state is durably recorded in SQLite as it happens and
  survives a host restart.** A single transactional CAS is the only writer
  of a job's terminal state; in-memory state is a cache that converges to
  the row. If the database is unavailable, starting a new async job errors
  instead of silently running untracked. If the host process of a running
  job dies (crash, kill, power loss), the next process to touch that
  session's scope, the next host to register, or the periodic sweep
  recovers it: an announced job is marked `interrupted` and delivered as a
  notice on the next turn; a job whose "started" result never made it to
  the model is deleted without a trace (also when it had already finished); a
  `job_kill` whose result message was never written (the host died between the
  kill and the message) is delivered as a notice on the next turn, while one
  whose result message was written is not delivered a second time; a delegation whose child messages
  cannot be read is left `running` for a later sweep instead of reporting a
  false empty answer. Recovery only records what happened: it never writes
  history and never wakes a session, and it does not hold the dead host's lock
  while it works, so a crashed `rush run`'s session can be taken over at once
  instead of reading "alive" until the recovery ends. A natural completion that races a
  graceful shutdown is still recorded (only jobs that shutdown itself
  cancels stay `running` for the next host), and a Stop between a job's
  claim and its "started" result no longer orphans the row. The old
  in-memory ready queue, the CLI loop's poll/BFS over descendant work and
  `internal/session/descendant_liveness.go`'s lock-file heartbeat heuristic
  are gone.
- **Forced shutdown keeps the host lock.** When the shutdown grace period
  expires with turns still running, the process keeps its host lock until it
  really exits, so another process cannot recover rows the still-running
  goroutines are writing. Only a clean shutdown releases it. There is no later
  in-process release: the goroutines that outlive the grace have no single
  completion signal to wait on, so a long-lived embedding process that gets
  `ShutdownResult.Forced` (or discards it with `Shutdown()`) must exit its
  process, as the CLI does; until it does, the rows of that host stay
  `running` and a new App in the same process cannot recover them.
- **A sub-agent delegation whose last turn produced no text (only reasoning
  or tool calls) now reports "завершено без итогового ответа" ("finished without a final answer") to its
  parent**, instead of resurfacing the stale text captured when the
  delegation was parked mid-flight. A child whose reaction was closed by
  failure finishes its delegation as failed with the text of the closed
  notices; that failure flag is cleared by the child's next real reaction
  and by a new delegation on the child, so one transient failure no longer
  marks every later delegation failed.
- **Retention (7 days) and where it runs.** Terminal, delivered-or-voided
  `async_jobs`/`session_notices` rows older than 7 days, dead drivers'
  markers, empty dead hosts' lock files and orphan `hosts/*.lock` files are
  purged. `running` rows, undelivered rows, unreacted debt and a delegation
  row whose child session still has running work or unreacted debt never are, so
  a Rerun reaches back only as far as retention keeps a delivered row. The
  web server runs the purge every 60s together with the dead-host sweep, the
  re-check of parked delegations and the re-check set. The re-check set's
  wakes run detached (one per session, at most four at a time), so a slow
  reaction turn never delays the next tick's sweep, purge or delegation
  re-check. `rush run` runs the same 60s pass while its loop is alive (the
  ticker starts with the loop's driver claim and stops at the process's
  shutdown) and
  the dead-host sweep and retention purge once more when the loop starts, and
  a process that registers a host sweeps dead hosts once at registration. A
  CLI-only install with no loop running has no ticker. Every turn start and
  scope check recovers the session's own dead-host rows. Library/SDK use, and
  commands other than `rush run` (except `sessions gc --jobs-older-than`),
  never purge.
- **Schema.** New migrations `20260929000001` (plain `(owner)` index on
  `session_notices`), `20260929000002` (`async_jobs.claim_id`, closes the
  terminal-transition ABA), `20260929000003` (`async_jobs.announce_message_id`,
  lets Rerun void by the message it deleted) and `20260929000004` (the
  `session_drivers` table). The phase-4 core migration
  `20260928000002_add_async_phase4_core.sql` was edited in place during
  development (three commits) before it shipped: a data directory that ran
  an intermediate dev build of it keeps the old schema and must be
  recreated. A data directory created by the rejected `async-phase4`
  development branch (its migration `20260929000001_add_async_job_ledger.sql`)
  shares the version number `20260929000001` with the index migration above, so
  goose refuses it (it reports `20260928000002` as missing); only unreleased
  development directories are affected and such a directory must be recreated.
  Shipped migrations are never renumbered. From now on only new migration files
  are added.
- **Web session-list re-polls read on a separate connection**, so they no
  longer stall behind write transactions (up to the 30s busy timeout); `sessions
  jobs` reads there too. The list computes every session's live-work flags with
  one batched reader call (one query per delegation-tree level and one
  liveness probe per distinct host for the whole list, not per session). No
  reader waits on a process's first host registration any more: registration
  serialises on a lock of its own that no reader and no write transaction
  takes, so it also cannot deadlock against a notice pull.

### Removed

- **`rush run --codex-thread-id` and the Codex completion callback are
  removed.** Codex delegations wait on the retained `exec_command` process
  handle again; the installed Codex skills no longer mention `codex queue`
  or `CODEX_THREAD_ID`.

### Fixed

- **`rush sessions reset` is a real clean slate, and refuses while the
  session is still worked on.** The wipe used to delete the messages only:
  the session's background-job and supervision notices and its
  done-but-unreacted debt survived, so the next `rush run --session <id>`
  injected "job … cancelled / interrupted" into the fresh history. Now the
  messages are deleted and every pending/delivered notice row of the session
  is voided in one transaction. `reset` (with or without `--force`) refuses,
  changing nothing, while a `rush run` loop drives the session (even between
  turns), or one of its background jobs or delegations is running; stop it
  first with `rush sessions cancel <id>` (or `rush sessions kill <id>` for a
  hung process). `--force` still kills the process holding the session lock,
  but no longer wipes under a live loop. Rows left by a force-killed run
  (dead host) are recovered and voided as part of the reset.
- **A legacy session's spend after a pre-fix reset is no longer lost when
  `sessions reset` runs before the parent was charged.** A child whose cost
  sits below its `parent_cost_accounted` (reset by an old binary, then
  spending again) had its new spend dropped by `sessions reset`; it is
  charged to the parent first, the same rule the transfer already applied.
- **`rush run --continue --role worker|reviewer` uses the model pinned on the
  session it continues.** `--continue` names its session only implicitly, and
  the role fold used to look the pin up before that session was resolved, so
  the worker/reviewer override set on the session was ignored in favour of
  the config default. `--session` still takes precedence over `--continue`.
- **Reaction turns (the turn a session takes after a job or delegation
  finishes) no longer fail in the ways the first phase-4 review found.** A
  reaction turn started from a mailbox release is no longer cut at 30s (it
  used to close the debt as failed and write a wake-failure marker). A
  release caused by another process holding the session lock no longer
  hot-loops; the session is re-checked by the 60s pass instead. A transient
  provider failure is no longer retried at once: the retry comes after a 60s
  pause, and three failed passes (or a quota/401-class
  error) close the debt with a visible wake-failure marker. Stop, shutdown,
  Ctrl-C, `--timeout` and a pending question no longer close debt as
  failed; only rows already visible in history when the turn
  started can be closed, settle and marker are one transaction, and the
  marker names a real tool call id, never an internal placeholder. An
  auto-summarize inside a reaction turn keeps its continuation instead of
  ending the run with tool results unanswered. The `rush run` loop paces
  itself on a row whose pull keeps failing instead of spinning, and a
  reaction write that always fails is bounded by the same three-pass
  counter.
- **A reaction turn is one counted attempt, retried at a pace, never
  hot-looped.** Each reaction turn is accounted once, by the turn loop that
  ran it, from what the database says: a turn that reached the provider and
  left its notices unreacted (an error, an empty reply, a reaction write that
  never landed, a watchdog stall, a `--max-tokens`/`--max-cost` cut) counts
  as one attempt, waits 60s (a newer event does not shorten the pause after a
  paid failure, so a one-minute provider outage never closes a notice; a
  human message does) and, on the third attempt of THAT notice, or at once on
  a quota-class error or a 401 whose credentials cannot be refreshed, closes
  it with the visible wake-failure marker (a notice that arrived later keeps
  its own count and is not closed with an older one). A 401 is refreshed
  first (an expired OAuth token after a long job, also before a web or
  delegated-child reaction turn starts): when the refresh works the attempt
  counts and the next one runs on the new token. Ctrl-C, the run's own
  `--timeout`, Stop, interrupts (including a human message that cuts the
  reaction turn off), shutdown and any refusal before the provider (lock
  busy, peak hours, provider not configured) count as nothing; a network
  timeout of the provider call is a provider failure and counts. A reaction
  turn makes one provider attempt (no coordinator retry loop). If both the
  reaction write and the settle keep failing, launches stop after three
  attempts until a human message; if only the notice pull keeps failing,
  three empty launches pause until a new event or a human message. A question the assistant asks while reacting is
  its reaction: the notice is closed, automatic turns pause until you answer,
  and a delegated child's question reaches its parent as the delegation
  result. A step that crosses `--max-tokens`/`--max-cost` is recorded as the
  reaction, and `rush run` ends with an error instead of paying for the same
  reaction again. `rush run` now also retries a delegated child's failed
  reaction turn with the same 60s pass the web process runs, and its loop
  follows the same launch decision as the web wake (run, wait for the retry,
  or stop on stuck debt with one stderr line; `sessions why` shows the
  stuck notice). Interrupting a reaction turn keeps the previous answer and
  still adds the turn's usage to the run total.
- **A Rerun no longer races a reaction turn.** From the moment the live turn
  is cancelled until the replacement turn takes over, automatic reaction
  turns are held off the session; stopping the voided jobs no longer keeps
  the rerun waiting. While a delegated child is held, its parent's delegation
  stays open (the hold is a temporary pause, not "nothing left to do"), so it
  is no longer handed over with stale text.
- **A delegated child's question reaches its parent as a question even when
  the child wrote text first.** The text used to win and the delegation was
  reported as failed without the question, options or resume guidance (the
  parent then redid the work); now the question is delivered, with the text
  kept as its preamble.
- **A network timeout of the provider call is a provider failure, not your
  `--timeout`.** A black-holed provider used to look like an operator deadline:
  the reaction turn was relaunched at once with no attempt count, no pause and
  dozens of "Run timeout exceeded" messages. The decision now comes from the
  turn's own context, so such a turn is counted, paced and closed at three
  attempts like any other failure, and only a real deadline of the run is
  exempt. A reaction turn cut off by a message you send (interrupt-and-send,
  Stop or Cancel with a message queued) is no longer counted as a failed
  attempt either.
- **Reaction turns refresh an expired OAuth token.** A web or delegated-child
  reaction turn after a long job used to send the old token, get a 401 and
  close the debt on the first attempt; the token is refreshed before the call
  and its client rebuilt (also in `rush run`), and a 401 that still happens is
  refreshed and counted like a transient failure. A delegated child whose
  provider takes its key from a `$(command)` template now also runs its next
  reaction turn on the refreshed key, and the refresh has its own 10-second
  budget, so an unresponsive auth server no longer keeps the notice from being
  closed with its failure marker.
- **The background-shell auto-resume cap (5 per human message) cannot be
  bypassed by a re-check.** A shell finishing while the last permitted reaction
  turn was running started a chain of further turns through the release
  re-check; the re-check now compares the cap without spending it. A shell
  that finished while an earlier reaction turn was paused or refused has
  already used its slot: its notice is still retried by the re-check and
  closed after three attempts like any other, and only completions that
  arrive after all five slots are used wait for your next message. The same
  holds for a reaction turn queued behind the last permitted one: a shell
  finishing while that turn runs no longer gets a sixth turn. Over-cap
  completions are now tracked by their saved notice, so a completion whose
  notice could not be saved (a locked database) no longer hides a notice that
  is still owed, and an older owed notice is retried even after newer ones
  were answered.
- **Per-session bookkeeping is freed in a long-lived web process.** The 60s
  pass drops the idle in-memory entries of sessions with no work (also a
  retry pause that has already passed), and the auto-resume state of deleted
  sessions.
- **A root `rush run` no longer ends while a descendant sub-agent or async
  command it owns is still live at any depth.** The non-interactive loop
  holds the run open and feeds each descendant's terminal result back as the
  next root turn; the reviewer pass runs once, when that work has drained and
  the scope first closes, on the final text (work the review turn itself starts
  is waited on too: see the review round 4 bullet below).
- **`rush run` reaction turns no longer break the run's answer, totals or
  session (review round 3).** The automatic reviewer pass runs once, when the
  scope first closes (after the reaction turns so far), on the final text, and
  its verdict is the run's answer unless reaction turns to work it started
  follow it -- the answer is always the last completed turn (it used to be
  started inside the last reaction turn, come back as a "queued" no-turn and
  leave the first turn's text and usage as the result).
  A first turn that fails before it is launched (a session-setup write, the
  pending-work drain) ends the run with that error instead of being hidden by
  a later reaction. A reaction turn that committed right before Ctrl-C or
  `--timeout` keeps its answer. Reaction turns no longer re-write the session's
  model overrides (a model changed in the web UI during a long wait is kept).
  The envelope lists each sub-agent once and drops the `final_text` warnings of
  turns another turn superseded; every failed, retried reaction attempt prints
  one stderr line naming the error and the retry time; the reviewer-pass skip
  line goes to the run's own stderr writer; a turn that queued behind another
  owner no longer drops the usage the session spent meanwhile;
  `--continue --role worker|reviewer` continues the very session whose pin it
  read.
- **`rush run`: work the reviewer pass starts is waited on, and the run's cost is
  the whole session's (review round 4).** The reviewer turn keeps `bash`, and
  every CLI `bash` is an async job: the run used to end right after the review
  turn, so its "tests are running" text became the answer and the job was
  cancelled at exit. The loop now goes back to waiting after the review turn:
  the job's result is reacted to on the reviewer's model and options, the
  reviewer runs at most once, and the answer is the last completed turn.
  `delta_cost_usd` (and `RUSH_COST_USD` for `--on-finish`) is the session's cost
  between the driver claim and the exit, so a delegated child's spend, charged
  to the parent between its turns, and a human turn on the same session are
  counted (`--max-cost` caps the session's total, and its error names that
  total; the envelope reports this run's window, from the driver claim to the
  exit -- the two differ when `--session` continues a session that already
  had spend); tokens stay the sum of the turns' last-snapshot deltas. `rush run --continue` resolves the
  most recently updated top-level session (a worker's child session updated
  later is no longer taken for it); a CLI run on a coordinator that cannot drive
  the loop fails with an error instead of silently running one plain turn.
- **A delegated child whose background shell finishes is woken even when
  auto-resume would not launch it, and its reaction turns are charged to the
  parent (review round 6).** A child's `bash` that moved to the background and
  finished while its delegation was parked left the child owing a reaction that
  nothing launched (web with `AutoResumeOnJobDone` off, web with every slot
  spent, and every `rush run`): the parent's delegation, and with it the run,
  waited until Stop or the 6-hour cap. A child whose delegation row is still
  running (read from the durable row, so a delegation another process owns
  counts) now gets its Drain whatever the auto-resume claim says; the launch
  gate and the policy (Stop, a question, a released child) are unchanged and a
  root session keeps the auto-resume cap. The child's own spend after its first
  turn (its reaction turns) reaches the parent when the delegation releases,
  before the notice commits, and at every end of the child's turn (so a Stop
  charges it too), through the same `parent_cost_accounted` delta as every other
  charge: a normal exit and a Ctrl-C exit report the same cost. The test-only
  supervision interval override no longer lets a value under one second defeat
  the floor.
- **A stream cut by the default wall-clock cap counts as a deadline on every
  transport (review round 8).** On plain HTTP/1.1 providers (Ollama, LM Studio,
  vLLM on `http://localhost`) net/http returns the cancellation cause itself,
  and the cap's cause did not wrap `context.DeadlineExceeded`: the turn ended
  as a generic "Provider Error" instead of the run-timeout text naming the cap,
  and a Drain cut off by it was charged as a failed attempt (three such runs
  closed the notice as failed) while the same cut by `--timeout` was exempt. The
  cause now wraps the deadline error.
- **A killed background shell that never exits stops holding its session's
  scope after 10 minutes (review round 8).** A shell whose process tree kill
  missed a descendant holding the output pipe (an ssh `ControlPersist` master,
  the adb server) stays unfinished after `job_kill`; its completion hold had no
  end, so a delegated child's parent stayed parked until the run's 6 h cap. The
  hold now expires 10 minutes after the shell was taken out of the job table.
- **Stop during an async `bash` start releases the child's driver (review round
  8).** A Stop landing in the first second of a child's async `bash` (the fast
  failure check) left the child's driver record and permission-allowlist entry
  registered until the process ended; the finishing executor now re-checks its
  owner.
- **A Drain that fails because the history no longer fits the model's context
  window says so (review round 8).** The "Event saved; continuation at the next
  turn" marker was false for that failure (the events stay in history and every
  later request fails the same way); it now names the cause and the remedy
  (summarize the session or start a new one). Detected from the provider's
  flagged context-size error, a 413, or a 400 naming the overflow.
- **A message injected into an idle session reaches the next turn's prompt once
  (review round 8).** `sessions inject` saves the user message before it queues
  the pending row, so the next turn loaded it with the history and then spliced
  it in a second time at step 1 (the model could act twice); a pending row whose
  message is already in the turn's history is now only consumed.
- **`sessions cancel` is a one-shot request (review round 8, agent part).** The
  cancel flag stayed set after it stopped a turn, so a web session's next prompt
  (and every automatic turn after it) was aborted after one step, and a child
  resumed by a delegation likewise. A human-initiated turn (web/SDK `Run`, a
  delegation resuming a child) now starts with the flag cleared, and an in-turn
  abort that honours it clears it; a session driven by this process's `rush run`
  loop keeps it for the loop, and Drain turns never clear it at their start.
- **A delegated child's finished background shell keeps the delegation
  parked until its result is recorded (review round 7).** A child's shell that
  had exited but whose completion notice was not written yet (the write can wait
  on the process-wide arrival lock and the single writer connection) counted as
  no work: a delegation re-check landing in that window (the 60-second pass, a
  release hook) handed the parent the child's stale first-turn text, and the
  shell's result reached neither the child nor the parent. The shell manager now
  keeps a per-shell completion hold from the moment its completion callback is
  registered until the notice is durable (also released when the callback
  returns or panics, and after 10 minutes at the latest), and the scope counts
  it; the callback's own re-check runs after the release.
- **Stopping a delegation releases its child's driver (review round 7).** A
  delegation stopped while its child was parked on the child's own async job
  left the child's driver record and permission-allowlist entry registered for
  the life of the web process, and later Stop/inject on that child reached an
  idle agent. Stop (and the Rerun tree stop) now re-checks every session of the
  stopped tree.
- **A run cut off by the default wall-clock cap says so (review round 7).** The
  "Run timeout exceeded" message of a turn ended by the 6 h default cap named a
  `--timeout` nobody passed; it now names the cap and
  `RUSH_RUN_DEFAULT_HARD_TIMEOUT`.
- **Documented: a delegated child's chain of background-shell wakes has no cap
  of its own (review round 7).** Root supervision does not bound it; the bounds
  are Stop, the delegation's own `timeout` (`terminate_and_wake`) and, in the
  CLI, `--timeout`/the default cap and `--max-cost` (`docs/async-invariants.md`
  ASYNC-09).
- **`rush run`: a reviewer pass refused by the session lock keeps the run's
  answer, and every loop exit records its exit reason (review round 6).** When
  another process (a web tab's human turn, `sessions inject`) holds the
  session lock as the loop's scope closes, the automatic reviewer turn is
  skipped with a stderr line instead of replacing the run's answer with an
  empty error envelope: the JSON envelope and terse output keep the last
  completed answer, the exit is clean, and the refused turn no longer writes
  `ended_reason` "error". A loop exit while waiting between turns (Ctrl-C,
  `--timeout`, stuck debt, a cap or `sessions cancel` at the precheck, an
  unreadable DB) now stores the envelope's `exit_reason` in the session's
  `ended_reason` once, so `sessions show` no longer says "end_turn" beside an
  envelope that says "canceled". The `HasLiveOwnWork` field is documented as
  what it is: an own running job OR a live `rush run` driver between turns
  (it does not imply a job exists); comments and docs only.
- **`rush run`: `--no-supervision` / `--supervision-interval` cover the reviewer's
  jobs, and cancel exits count a running child's spend (review round 5).** The
  reviewer turn now carries the run's supervision options: since its jobs are
  waited on, a `--no-supervision` run no longer gets check-in notices (and a
  paid Drain on the reviewer model) for a long reviewer job, and
  `--supervision-interval` applies to it. A refusal streak that ended when the
  scope closed no longer counts against the first Drain refused after the
  reviewer (it had made that refusal give up at once, without a "retrying"
  line). On Ctrl-C, `--timeout` or a cap exit with a delegation still running,
  the child's spend so far is charged to its parent (deepest first, delta-based
  through `parent_cost_accounted`, so the later charge on shutdown adds only what
  accrued after it) before the run's cost is read: `delta_cost_usd` and
  `RUSH_COST_USD` include it, and `sessions cost` shows the same number. A live
  `rush run` loop between turns (a paced retry after a failed reaction turn: no
  session lock, no running row) is no longer reported at rest: `sessions why`
  says `running` and names the loop's PID, `sessions list` shows `running`, and
  the web session list sets `HasLiveOwnWork`, all from the durable driver marker
  (host liveness, unknown counts as alive), read in one query for a whole list.
- **`sessions watch`, `sessions tail --follow` and `sessions locks` no longer
  call a live `rush run` loop ended or offline while it waits between turns
  (review round 6).** The session lock is held only during a turn and its file is
  truncated on release, so a loop waiting on a job (every CLI `bash` is one)
  left an empty, aging lock file: `watch` printed "session ended" and exited 0,
  `tail --follow` stopped at the first turn's finish, `locks` said `offline`, and
  the `/rush` guidance concluded "the holder died" and advised a relaunch that
  `rush run` then refuses. They now keep going while the driver marker, a running
  own job or a live delegation exists, and say what they wait on; `locks` shows
  `between turns (rush run PID n)` (JSON `between_turns`, `driver_pid`; not stale,
  never pruned). The `/rush` guidance sends `offline` to `sessions why <id>`
  first. A failed reaction turn's paced retry is `running` in `sessions list` and
  `sessions why`, not `crashed`: an empty (PID-less) or driver-owned lock is a
  clean release, and `crashed` stays for a recorded dead PID of another process
  and for sessions with no live work.
- **`rush run` without `--timeout` no longer force-kills a waiting loop at the
  6 h default cap (review round 6).** The cap is now a graceful deadline like
  `--timeout`: the loop ends through its normal exit (envelope, `--on-finish`,
  shutdown; exit reason `canceled`) with a stderr line naming the cap;
  `os.Exit(124)` remains only for a process still alive 60 s past the deadline.
  The exit status of a run ended by the cap is 1 (it used to be 124): scripts
  that matched 124 were matching a force-kill with no envelope; 124 now means
  only that the 60 s hard-kill backstop fired.
  A wait longer than the cap (`RUSH_RUN_DEFAULT_HARD_TIMEOUT`, default 6 h) needs
  an explicit `--timeout`. `--timeout` itself is unchanged.
- **A message injected while a tool runs no longer breaks the session history.**
  A web `InjectMessage` or `rush sessions inject` landing between an assistant
  `tool_use` and the tool's result was persisted in that gap, so the next turn
  sent the provider `tool_use, user, tool_result` and it rejected the request on
  every later turn. History assembly now moves such user messages after the tool
  round (a round whose result never arrives is unchanged).
- **`rush run --json` always prints its envelope when the deadline cuts the first turn.**
  A `--timeout` (or the default cap) expiring while the user's own turn was still
  generating left stdout empty with exit status 1; the run now flushes one
  envelope (`exit_reason: "canceled"`, the deadline error, usage so far) and
  records `ended_reason` like every other cancel.
- **`sessions cancel` stops a `rush run` that is waiting on a job (review round
  7).** The flag was read only before a paid turn, so a loop waiting on a
  long-running job (or on the launch gate's pause) ignored the cancel until the
  job ended. The loop now reads it at every wake of that wait (at least every
  5 s) and ends `canceled` like Ctrl-C: envelope, `--on-finish`, `ended_reason`,
  and the jobs the run started are cancelled with the process.
- **`sessions locks`, `sessions inject` and `sessions tail` handle ids with
  path characters, loops between turns and hashes (review round 7).** A
  session id with `/`, `\`, `:`, `*`, `?`, `"`, `<`, `>`, `|` or a space (for
  example `--session fix/login-timeout`) has a lock file named with `_`, so the
  round-6 "between turns" detection and the `--prune` guard missed it: such a
  loop read `offline`, and `--prune` could take its lock. They now match by the
  lock-file name. `sessions inject` into a session a live `rush run` loop drives
  between turns answers `running: true` (JSON `between_turns`, `driver_pid`; the
  message names the loop) instead of "no process is currently running this
  session"; `--interrupt` has no running turn to cancel there and applies when
  the loop starts its next one. `sessions tail <hash>` (the HASH column of
  `sessions list`) prints the messages and `--follow` ends, and Ctrl-C ends
  `tail --follow` with exit 0 as documented.
- **`sessions show` prints how the last run ended and its budget, and
  `sessions list --json` carries `ended_reason` (review round 7).** The
  round-6 change that keeps `ended_reason` in step with the envelope was
  invisible: `Sessions.Get` dropped the column, so `Ended:` and the budget never
  appeared. `run.go`'s help now says what is written: the run's `exit_reason`
  (the model's finish such as `end_turn`, `error` -- also for a
  `--max-cost`/`--max-tokens` exit -- or `canceled` for Ctrl-C, `--timeout`, the
  default cap and `sessions cancel`), empty while a run is in progress.
- **`sessions inject`, `sessions reap`, `sessions locks` and `sessions cancel
  --all` fixed (review round 8).** `sessions inject` into a web session that
  waits between turns on its own running background job or a live delegation
  (no `rush run` driver marker) answers `running: true` and names what keeps
  the session open, instead of "persisted-offline". `sessions reap` keeps every
  lock file that records no PID (what a clean release leaves: a `rush run` loop
  or a web session between turns) unprobed, so it no longer takes the lock a
  live loop needs next or reports "provably dead" for a live process; a crash's
  PID-recording lock is still reclaimed. `sessions locks` finds the sub-agent
  pulse of slug ids such as `fix/login-timeout` by the real session id, and
  `sessions locks <id>` (id or hash prefix) now lists only that session's lock,
  as the `/rush` guidance's watchdog recipe assumes. `sessions cancel --all`
  flags only sessions with live work (a live lock, a `rush run` driver, a running
  job or a live delegation) and prints how many idle sessions it skipped: the
  cancel flag is a one-shot request, and one left on an idle web session would
  abort its next turn. A named `sessions cancel <id>` still flags.
- **`rush run` help, the README and the `/rush` guidance no longer say a run
  has no time limit (review round 8).** Without `--timeout` a 6 h default
  wall-clock cap applies (`RUSH_RUN_DEFAULT_HARD_TIMEOUT`); `--idle-timeout`
  watches turns only, so a run waiting between turns on a job, a delegation or
  a retry pause is bounded by `--timeout` or the cap alone.
- **`ended_reason` stays empty while a `rush run` is still running (review
  round 8).** The per-turn write stored the first turn's `end_turn` while the
  loop kept waiting on a job, so `sessions show` printed `Ended: end_turn` and
  `sessions list --json` gave `{"status":"running","ended_reason":"end_turn"}`
  for a live session (and kept saying it after a kill -9). The loop's turns no
  longer write it; its exit writes the envelope's `exit_reason` once. Callers
  that are not the loop (the SDK) keep the per-turn write. A run refused
  because another process owns the session no longer writes `error` into that
  session's row.
- **`--max-cost` / `--max-tokens` bound a `rush run` that is waiting (review
  round 8).** While the loop waited on a delegation or a job it checked only
  `sessions cancel`, so a child polling in a chain of reaction turns (each
  charged to the root) ran to the 6 h cap before the limit reported. The wait
  now reads the limits at every wake (one session-row read, also answering the
  cancel flag) and ends `error` through the usual exit: envelope,
  `--on-finish`, and the run's jobs are cancelled.
- **A `sessions cancel` that stopped a `rush run` no longer aborts the next
  turn (review round 8).** The flag stayed set after the loop honoured it, so a
  later web turn or takeover reaction turn on the same session was cancelled
  after its first step. The loop clears it when the run ends `canceled`, before
  the envelope is flushed.
- **A closed stdout pipe no longer kills `rush run` before `--on-finish` and
  shutdown (review round 8, Unix).** The envelope write raised SIGPIPE and the
  process died with status 141, leaving the run's jobs running and the hook
  unrun. Writes to a closed stdout/stderr now fail with EPIPE through the
  normal exit path (hook and shutdown run, exit status 1), and spawned jobs keep
  the default SIGPIPE behaviour.
- **`sessions list` and `sessions why` no longer report a root as done
  while a descendant session still has live work**, cross-process.
  `sessions why` names the live descendant, and the session-list API now
  carries `HasLiveDescendantWork` / `LiveDescendantIDs` for the web UI
  (rendering pending). This now reads the durable `async_jobs` ledger
  (a live delegation row, checked against its host's liveness) rather than
  a session lock file, so it also covers a delegation whose child session
  has not yet taken its own turn. The delegation walk counts delegation rows;
  a session whose only live work is its own plain background job is reported
  separately, as "running" (see Added: `HasLiveOwnWork`).
- **Parked sub-agent outcomes are delivered exactly once and cancellations
  stay cancellations.** A canceled child can no longer be reported to its
  parent as a success carrying the child's last text.
- **Generated session titles now persist and reach the web UI
  immediately.** Title generation no longer races turn teardown, and
  `Rename` publishes the session-updated event so tabs stop waiting for the
  5s poll.
- **The web composer history no longer recalls messages that did not
  originate from the user** (notice/system-origin messages were polluting
  `$myPrompts`).
- **The composer's ArrowUp recall and history dropdown no longer leak
  supervision check-ins, one-time timeout check-ins, or wake-failure
  markers.** A server-computed `HumanTyped` field (role, hidden/summary
  flags, `NoticeKind`, `AutoResumed`, `BackgroundJobNotice`, origin; never
  the text) and a single shared client-side filter decide what the recall
  history shows. Every notice the driver pulls into history carries
  `AutoResumed` and `BackgroundJobNotice` (supervision, timeout and
  wake-failure ones also a `NoticeKind`), so any of the markers excludes it,
  for every current and future notice kind.
- **A delegated sub-agent in `rush run` now receives the results of its own
  async commands.** They used to land on a queue only the root read, so the
  sub-agent never saw its command output and the parent got the sub-agent's
  pre-result text. The result is also delivered when the command finishes
  while the sub-agent's turn is still running: the wake now goes to the same
  agent that drives the sub-agent (previously a second agent collided with
  its own session lock and the result was dropped).
- **A released session lock is always closed and unlocked.** A race between
  `Release` and a slow heartbeat touch could leave nobody closing the lock
  file, keeping the session "busy" until the process exited; a re-acquire of
  the same session in the same process now waits for its own pending unlock
  instead of failing with "session is already in use".
- **The task agent is never given the `agent` tool**, which would make tool
  construction recurse forever and hang every run.
- **A config write on Windows retries when another reader holds the file
  open**, instead of failing with "Access Denied". The NT rename fallback
  reported the conflict as an NTSTATUS the retry check did not recognise.
- **`job_kill` and `job_output` accept the job id the model was given**
  (`job_id`), so a running async `bash` or `run_command` command can be
  stopped or inspected before it finishes. Previously they needed an
  internal shell id the model only learned once the job was over.
- **`job_kill` now produces a distinct "stopped (job_kill)" notice instead
  of describing the killed process's own exit.** The ledger records the
  stop request before the kill, so the model can tell it cancelled the job
  on purpose instead of it failing on its own.
- **A delegated sub-agent's driver and restricted-run allowlist entry are
  released once its scope actually closes**, instead of living for the
  rest of the process. A long-lived web server no longer accumulates one
  entry per delegation forever; a later `resume_session_id` on the same
  child still re-establishes both before the child's next turn runs.
- **`run_command` jobs are now controllable.** `job_kill` stops one by
  killing its whole process tree (not just the direct process, on Windows
  included); `job_output` returns accumulated output while it is still
  running via a new bounded live-output buffer. Previously both were
  refused with "cannot control yet".
- **`job_output` accepts a `cursor`** (and returns `next_cursor`) so a
  repeated call can fetch only output written since the last one instead of
  the whole buffer every time. A stale or out-of-range cursor is treated as
  0, not an error.
- **`job_kill`/`job_output` on a sub-agent delegation's job id now say
  plainly that it cannot be stopped/inspected this way and its result will
  arrive as a session message**, instead of a generic "not a command"
  message. (`stop_agent`/`inspect_agent` land in a later stage and will
  replace this text once they exist.)
- **A background job's completion notice is always persisted before Rush
  tries to resume the session on it.** Previously, if the mailbox was busy
  or the resume attempt failed for any other reason, the notice could be
  lost entirely with only a Debug log line. Now the notice is written to
  the transcript first (idempotently, so a retry never duplicates it), and
  if the resume attempt itself fails, a second, visible message is added
  to the session explaining that the event was saved and will be picked up
  on the next turn.
- **A sub-agent resumed while its mailbox is busy (e.g. right after its own
  async job just woke it) no longer reports "Sub-agent completed but
  produced no text output."** The parent now waits for that queued turn's
  real result instead of misreading a `(nil, nil)` "not started yet" as an
  empty response.
- **`Cancel` and `InjectMessage` on a delegated sub-agent's session id now
  reach the sub-agent's own driver**, not the root coder agent's (idle,
  untouched) mailbox. This had no live caller before this change but was a
  precondition for future `stop_agent`/`inject_agent` tools.
- **`bash`, `run_command`, and `agent` accept an explicit `timeout` (`{seconds,
  kind}`, 5s–7d)** for `wake_only` (a one-time check-in while the work keeps
  running) or `terminate_and_wake` (stop it and report partial output). A
  single per-process timer services every armed deadline. `run_command`'s
  legacy `timeout_seconds` is now an alias for `terminate_and_wake`; setting
  both `timeout` and `timeout_seconds` on the same call is a validation
  error.
- **A restricted-run sub-agent delegation's allowlist no longer expires
  after its first turn.** A woken continuation used to fall back to the
  process-wide policy instead of the delegation's own; the allowlist now
  lives as long as the delegation and is re-inherited on every wake.
- **`bash`/`run_command`/`agent` calls from a non-CLI/non-web origin (SDK)
  now go through the same job registry, CAS, and explicit-timeout support
  as CLI/web calls**, instead of a separate synchronous branch with no
  registry and no timeout support. The response returned to the caller is
  unchanged; a `terminate_and_wake` timeout on such a call returns a timed-out error
  result carrying the partial output.

### Added

- **Messages can carry a `NoticeKind`** (e.g. `timeout_wake_only`,
  `timeout_terminated`, `wake_failed`, `job_stopped`) distinguishing the
  kind of system-generated notice, for future web rendering.
- **The root session is now periodically supervised while it has open
  background work.** After 5 minutes (configurable, `supervision_enabled`/
  `supervision_interval_minutes` in config, `--no-supervision`/
  `--supervision-interval` on `rush run`) of silence in the root's own chat
  with a running command or sub-agent delegation still open and no turn in
  progress, the root is woken with a summary of what is running, for how
  long, and its last output line — ending with guidance to keep waiting,
  inspect with `job_output`, or stop with `job_kill`. Never holds the run
  open by itself: `rush run` still exits normally the instant its other
  work drains. The check-in interval doubles on consecutive ticks with no
  progress (capped at 60 minutes) and pauses after 6 such ticks until real
  progress happens; older check-ins reach the model as a one-line marker.
- **Each queued web message now has its own "Send now" / "Interrupt & send"
  buttons**, shown only while the session is busy. "Send now" merges the
  message into the running turn via `inject_message` without cancelling it;
  "Interrupt & send" cancels the turn and starts a new one with it via
  `interrupt_and_send`. The message is removed from the local queue the
  instant one of these is clicked, so the later agent_busy=false flush can
  never send it a second time; a failed request restores it to its original
  queue position with a visible error. A request that times out or loses the
  connection (rather than getting an explicit error back) is ambiguous — the
  server may have already acted on it — so the restored error says to check
  the chat before resending instead of implying it definitely failed; a
  session that goes idle while a restored message is still queued shows a
  plain "Send" instead, so it isn't stranded until an unrelated future turn
  happens to flush it.
- **`rush run --role worker`/`--role reviewer` now honor a per-session
  worker/reviewer model override** (set via the web UI's model selector,
  same `set_session_models` path as the existing smart/fast per-session
  pin), instead of only ever reading the config-wide default for that
  role. `sessions show` also now prints the fast/worker/reviewer slots
  (previously only smart was shown).
- **The web model selector now covers all four role slots** (smart, fast,
  worker, reviewer), not just smart/fast: each has its own icon, recent
  models, reasoning effort, and a reset-to-default action; worker/reviewer
  live in a compact expandable panel next to the always-visible smart/fast
  pickers.
- **The chat's bottom panel is now a Tasks / Commands / Agents tabbed
  view**, replacing the bare todo list in that spot. Tasks (the existing
  todo list) is always shown; Commands (running bash/run_command async
  jobs) and Agents (running agent/agentic_fetch delegations) appear only
  while non-empty, with the live count in the tab title, and fall back to
  Tasks the instant the selected tab's list empties. Clicking an item
  scrolls the chat to its tool call and expands that tool call's block,
  showing a short hint instead if the call isn't in the loaded transcript.
  The wire contract (`session_live_work` event, `get_session_live_work`
  request) is wired end-to-end on the client; the server-side emitter that
  would populate the lists from live jobs/sessions is not written yet (the
  DB-backed readers it needs, `AsyncJobStore.LiveJobs`/`LiveWorkForRoots`,
  ship in the same release), so the panel shows Tasks only for now.

### Changed

- **Title generation grace rose from 5s to 10s**, so more generated titles
  finish before a turn yields.

## [0.2.0-alpha.7] - 2026-09-24

### Changed

- **Installed delegation skills now give Claude and Codex distinct guidance.**
  Every Codex variant explicitly passes `--codex-thread-id` when launching
  or resuming Rush, so completion returns to the originating Codex thread.
  The `wcrush` workflow also requires the orchestrator to commit verified
  changes after transferring them into the primary branch; the sub-agent
  still never commits or pushes.

### Fixed

- **Message timestamps stay clear of hover actions in the web UI.** A fixed
  16 px gap separates the time from copy/edit controls on user and assistant
  messages, with a small optical vertical alignment adjustment.
- **Go tests no longer open visible command windows on Windows.** Test
  subprocesses use the existing hidden-window launcher, and a guard now
  checks test files as well as production code against regressions.
- Stabilized the background-shell buffer-retention test when the Windows
  scheduler is busy.

## [0.2.0-alpha.6] - 2026-09-24

### Added

- **Commands and sub-agents run asynchronously in CLI and web sessions.**
  `bash`, `run_command`, `agent`, and `agentic_fetch` return a job ID promptly.
  Rush adds their final output or error as a new session message and resumes
  the agent, so the model can continue independent work without polling.
- **`rush run` stays alive while asynchronous work is pending.** Completion
  messages continue the same session, including beyond five consecutive
  completions, and JSON mode emits one final result after all work ends.
  The web UI keeps command and sub-agent blocks running until their completion
  notice arrives, then marks them done or failed.

### Fixed

- **The default text-mode CLI could lose its final answer after an async job.**
  It now captures the selected final turn, prints only that answer, and runs
  the finish hook once after the whole session completes.

## [0.2.0-alpha.5] - 2026-09-24

### Added

- **`rush run --codex-thread-id` notifies the originating Codex thread when
  a CLI attempt ends.** The message distinguishes completion, failure,
  cancellation, queued work, and a question awaiting an answer. It includes
  a short result or error summary, preserves quotes by passing the message
  as one process argument, and does not change Rush's exit status if delivery
  fails. The notification runs after the JSON result is written.
- **Delegation skills can have shared and CLI-specific instructions.** Skill
  sources may remain Markdown or use ordered YAML blocks with common and
  per-target text. Claude and Codex now receive their own launch and
  completion guidance; Codex's fallback skill no longer refers to
  Claude-only scheduling tools.

### Fixed

- **A fresh `rush run` could overtake pending durable work.** Queue draining
  now rechecks after a local owner releases the session and returns an error
  if pending work cannot be drained, instead of silently starting the new
  turn. Cancellation during that wait is reported to the caller.
- **Config reload could publish a stale candidate after a concurrent
  write.** Final disk checks no longer hold publication locks, and a disk
  write generation fences candidates verified before the write.
- **Canceled JSON runs could report a provisional `end_turn`.** An
  unconfirmed canceled turn now reports `canceled` with its error and does
  not extract JSON from partial text.
- **OAuth refresh could reuse a removed provider's stale credentials.** If
  the provider disappeared from the current config snapshot, Rush now
  refuses the refresh before making a network request.

## [0.2.0-alpha.4] - 2026-09-23

### Fixed

- **`rush run` could still exit "queued behind an active run" after
  draining pending work.** When the drained turn lost its lease (the DB
  stalled past the watchdog margin) it was still unwinding when the drain
  returned, and the new turn queued behind it. After draining, `rush run`
  now waits until this process's current turn on the session actually
  releases it.

## [0.2.0-alpha.3] - 2026-09-23

### Added

- **`--peak-hours-message` for `rush providers set` / `add`.** The custom
  text shown when a provider refuses a request inside its peak-hours
  window is now settable from the CLI (previously web UI only). A
  message-only update keeps the window, `--peak-hours-message ""` clears
  it, a missing window is an error, and `--local` writes a complete
  `peak_hours` object to the workspace config only.

### Fixed

- **`rush run` hung forever with "queued behind an active run".** When a
  run ended (e.g. by `--timeout`) with a call still queued, that call was
  saved as durable work. Every later `rush run` on the session lost the
  race to its own process's background pump, exited "queued", and that
  exit cancelled the pump's turn — so the session never progressed, and
  `sessions kill` / `reset` / `reap` did not help. `rush run` now first
  executes (or waits for) the session's pending durable work, then its
  own prompt. Already-stuck sessions recover on the next run.
- **Agents wasted turns on "outside every folder scope" refusals.** The
  scoped `fs_*` tools were offered even without a `--folder` scope, where
  they deny every path. Unscoped runs no longer expose them.
- **Peak-hours refusal in `--json` lost the guidance and operator
  message.** The envelope's `error` now carries `RESUME AT` and the
  configured message for both a start-of-run refusal and a mid-turn stop.
- **401 retry skipped silently.** The reason a credential-refresh retry
  was skipped is now logged instead of swallowed.
- **npm launcher leaked `.tmp-launch-*` directories on Windows** when the
  just-exited binary was still image-locked; removal now retries.

## [0.2.0-alpha.2] - 2026-09-22

### Added

- **Complete Codex delegation Skill installation.** `rush codex-init` and
  `rush codex-del` now include `rush`, `rush-fallback` and `wrush`, with
  sentinel-safe overwrite/removal behavior. `rush cli-refresh` refreshes all
  three Codex Skills as well. The Codex-specific `wrush` renderer rewrites
  canonical `rush.md` references to the sibling `../rush/SKILL.md` path used
  by Codex's nested Skill layout. Generated Codex/Grok Skills now begin with
  YAML frontmatter, with the ownership sentinel placed immediately afterward,
  so strict Skill parsers accept them while safe refresh/removal still works.

- **`/wrush` slash-command**, installed by `rush claude-init` alongside
  `/rush` and `/rush-fallback`. Behaviorally identical to `/rush` with
  one added hard rule: every delegated task runs inside a dedicated
  git worktree instead of the primary checkout, even for a solo,
  non-parallel task — the orchestrator creates the worktree, launches
  `rush run` there, verifies the diff, merges the reviewed result back
  into the primary checkout, then removes the worktree. Use it when a
  delegation must not be able to touch the primary checkout's working
  tree or index under any circumstance. `rush claude-del` removes it
  along with the other two commands.

- **Auto-reviewer pass.** After a clean `--role smart` run, if a reviewer
  model is configured, `rush run` automatically follows up with a second,
  isolated pass asking the reviewer to add its own conclusion — the
  reviewer runs with its own model/tool policy (sub-agent delegation
  disabled, no session-model persistence) and its result, not the
  primary pass's, becomes the run's final output. Skipped entirely for
  credentialed (tenant) calls, so a tenant's transcript never reaches a
  globally-configured reviewer it didn't request.

- **Provider network configuration.** A provider can now be routed through
  an HTTP or SOCKS5 proxy and/or a custom DNS server / DNS-over-HTTPS
  resolver, configured per-provider in `rush.json`. All of the resulting
  network paths (CONNECT tunnel, SOCKS5 handshake, plain and DoH DNS
  lookups) are bounded by their own deadline and respect cancellation, so
  a silent or slow proxy/resolver cannot hang a request indefinitely.

- **`rush run --idle-timeout`** (default 15 minutes). `--timeout` now
  defaults to *disabled* — a run has no overall time limit unless you set
  one — and `--idle-timeout` is the new backstop: if the agent produces no
  activity (not just no output — tool execution keeps the clock paused)
  for the configured duration, the run ends with a dedicated error instead
  of hanging forever. Unlike a transient stream stall, an idle-timeout
  expiry is terminal — the run does not auto-retry.

- **Peak-hours exit message.** A provider's peak-hours refusal window
  (`rush providers set <id> --peak-hours HH:MM-HH:MM`) can now carry an
  optional custom message, set from the web UI's provider settings (shown
  once the peak-hours checkbox is enabled). When a run exits because of
  the window, the message is appended after a blank-line separator to the
  guidance shown on stderr and recorded in the session's finish details.

- Web UI: message timestamps are now always visible, not just on hover;
  sub-agent block headers show the model and reasoning effort used; and
  sub-agent tool-call blocks show an argument preview alongside their
  timestamp.

### Changed

- A turn interrupted mid-stream by a transient provider failure (stall,
  rate limit, brief outage) and then successfully continued now returns
  its FULL answer, byte-exact, across the entire continuation chain —
  including when the continuation itself calls a tool before finishing.
  Previously the returned text could silently drop the interrupted first
  half, or corrupt structured output (e.g. `--format json`) by inserting
  a stray blank line at the exact byte the stream was cut.
- Retry/continuation decisions are now scoped strictly to the assistant
  message the current call's own attempt produced. Previously, on a
  session shared by two concurrent callers, one caller's transient stall
  could be misread as evidence belonging to a *different* caller's
  already-successful or already-refused call, triggering a spurious extra
  turn built from the wrong caller's partial text.
- Split `ExecuteRun` into smaller same-package files without changing its
  public API or execution behavior. Removed the corresponding file-size
  exemption.
- Made watchdog shutdown and configuration-reload handoff regression tests
  event-driven, and tightened database-connection cleanup checks for failed
  application initialization.

- Workspace configuration now fails closed on trust uncertainty. A workspace
  config whose ownership cannot be verified, or whose working directory/config
  cannot be statted, causes load or reload to fail instead of being silently
  accepted. Missing workspace config remains valid; foreign global and project
  candidates continue to be skipped during discovery.

### Fixed

- macOS configuration case-sensitivity detection now uses the supported
  libSystem `pathconf` wrapper instead of a deprecated direct syscall,
  preserving fail-closed handling for unknown filesystem capabilities.
- **MCP isolation across application lifecycles.** Closing an installed or
  standalone owner no longer clears another owner's connections, advertised
  data, event subscriptions, or disabled/error/starting states. Surviving
  owners retain their server-lease identity across installed-owner rollover;
  closing the last owner still cleans up its resources. This applies to
  owners with disjoint server names; sharing a server name remains unsupported.
- **Concurrent launches of different npm binary builds.** A launch now pins
  its executable in a private temporary directory before running it, so another
  build's cache cleanup cannot cause a startup `ENOENT`. If hardlinks are
  unavailable or the shared cache entry was already removed, the launcher
  copies the installed executable instead. Private launch files are removed
  after the child exits; unavailable-cache fallback remains supported.

## [0.2.0-alpha.1] - 2026-08-28

### Added

- **`git_read` and `run_command` agent tools.** Two narrower alternatives
  to `bash` for the two things it was used for most: inspecting a repo
  and running a program. `git_read` exposes a fixed, closed set of
  non-mutating git operations (`status`/`diff`/`log`/`show`/`blame`/
  `branch_list`) — there is no way to reach `checkout`/`reset`/`commit`/
  `push` through it, because the tool never accepts a raw command or raw
  flags, only typed parameters assembled into one of those six fixed
  shapes. It is safe to run concurrently from different subdirectories
  of the same repo. `run_command` runs one program with an argument
  list and no shell at all — no `;`/`&&`/`|`/`$()`/backticks, because
  nothing parses the arguments as shell syntax — which is both faster
  (bypasses the Windows shell-emulation layer `bash` goes through) and
  closes a class of shell-injection that a restricted `bash` allowlist
  cannot. Both are covered by the same `--restrict-run`/`--allow-bash`
  surface as `bash`: `run_command`'s program and arguments are
  synthesized into one command string for pattern matching, so an
  existing `--allow-bash 'go test'` rule now also permits `run_command`
  running `go test`.
- **`rush mcp add`/`rush mcp set --enabled-in-cli`** and a `CLI` column
  in `rush mcp list` to manage the new `enabled_in_cli` field (see
  Changed below) without hand-editing `rush.json`.

- **`rush migrate`** renames existing on-disk state from the old
  crush-era names to the new ones (see the rename entry under Changed):
  `.crush/` → `.rush/`, `crush.json` → `rush.json`, `CRUSH.md` →
  `RUSH.md`, `.crushignore` → `.rushignore`, plus both global
  locations (config and data are genuinely separate paths gated by
  different env vars, so both are always attempted). `--recursive`
  walks a tree and migrates every project it finds; `--dry-run` prints
  what would happen and touches nothing. It never merges or clobbers:
  if the rush-named target already exists, that one item is refused
  with an explicit message while everything else in the same run still
  proceeds, and the command exits non-zero if anything needs manual
  attention. A gitignored `.crush/` still migrates; a `crush.json`
  genuinely nested inside `node_modules` does not.
- **`rush cli-refresh`** re-installs the slash-commands and sub-agents
  for all five tool integrations (claude, codex, gemini, grok, qwen) in
  one go, replacing crush-branded ones with their rush-branded
  equivalents. Local by default, or `--recursive` (only directories
  that already have an integration) / `--global`, with `--dry-run`. It
  is deliberately a separate command from `rush migrate` and is never
  invoked by it — config state and installed tooling are two different
  things to update, on two different schedules.

- **A Retry button on every user message** in the web UI, not just the
  last one. The server already implemented the atomic
  cancel-tail-delete-and-resend; only the last message ever exposed it.
  Retrying an earlier message now warns, in a confirm dialog, that it
  and everything after it will be deleted. The selection checkbox also
  stopped moving: it used to flip sides based on who wrote the message,
  and now sits at a fixed left edge.
- **Right-click a message's checkbox to select everything above or
  below it.** Both add to the current selection rather than replacing
  it, matching shift-click's existing behaviour.
- **A "More" dropdown in the chat toolbar.** The button cluster had
  grown crowded, so the occasional-configuration buttons (Prompt, MCP,
  Providers, Default models, Settings, Logs) moved into one menu.
  Moment-to-moment session controls — Compact, Collapse all, the
  update badge, the theme/keep-alive toggles, the sitter pill — stay
  where they were.
- **A button to shut the server down from the web UI.** It travels on
  the same non-starvable control-plane path as cancel/interrupt, so a
  shutdown request never queues behind a long-running turn.

- **rush refuses to run from inside its own source tree.** A scratch
  dev binary built into the repo goes stale silently: one was found
  mid-session shipping a *new* system prompt that told delegated agents
  their edit tools were "absent by design" alongside *old* code that
  had not actually removed them, so those agents cheerfully called the
  tools they had just been told they did not have. The binary now
  refuses to start from that location and says how to build or install
  a real one.

- **A badge when a newer rush exists.** Since the TUI was removed
  nothing told anyone about a new release: upstream published a message
  the Bubble Tea UI rendered, this fork deleted that UI, and the startup
  check was left computing an answer nobody read. The server now checks
  once at startup and broadcasts only when a newer version exists — so
  the UI has no "up to date" state to draw — and the header links to the
  releases page. It does not self-update: this binary is installed by
  the operator and must not rewrite itself under a running agent
  session. A failed check is logged at debug, because an offline machine
  is a legitimate state and warning on every offline start would cry
  wolf about something nobody can act on.
- **`RUSH_MAX_BACKGROUND_JOBS`** raises the concurrent background-job
  cap for one process. The default stays at 50, and that is a measured
  decision rather than a cautious one: raising it to 500 made this
  repository's own test suite unstable (one package went from 5s to
  149s, and timing-sensitive tests in another began failing), because
  the jobs a higher cap admits are real processes competing for the
  machine. Raising it is now the operator's call, on a host they know
  can take it. A malformed value falls back to the default and says so.
- **A warning as the background-job cap approaches**, at 80%, with the
  live count and the command being started. The cap itself is not new;
  its invisibility was the problem. It previously gave no signal at all
  until the run that hit it was already dead — one real session died
  that way, and the next brief written for that project told the model
  to stop delegating altogether.
- **`pnpm typecheck` for the web UI**, wired into the pre-push hook.
  Nothing checked TypeScript types before: `pnpm build` uses rsbuild,
  which does not, and no script or workflow ran `tsc`. Five errors had
  accumulated unnoticed, one of which changed what users saw.

- **Higher prompt-cache hit rates**, ported from investigating why
  oh-my-pi gets much better ones than rush did. Three changes: the
  todo-list reminder injected into every turn moves from the *front*
  of the conversation history to the tail — it's regenerated from the
  session's current todos every turn, so prepending it busted the
  longest-common-prefix cache match on every single todo mutation,
  the single largest source of cache invalidation rush had. An idle
  keep-alive now extends Anthropic/Bedrock/Vercel's ~5-minute cache
  TTL for a paused conversation: after a turn writes to the cache, a
  timer fires a detached, silent replay of the same cache-marked
  messages just under the TTL, up to 3 times (~15 minutes), cancelled
  the moment a real turn resumes. And OpenAI-compatible providers
  (Chat Completions and Responses API) now get a stable
  `prompt_cache_key` per session, matching the affinity hint the
  request headers already used, so repeated calls have a better
  chance of landing on the same upstream cache shard.
- **Per-message token & prompt-cache statistics** — every assistant
  message now records its own token accounting, split into three
  disjoint classes (fresh input / served from cache / written to cache)
  plus reasoning tokens, cost, and the model that actually produced it.
  This is also the tool that proves the cache-hit-rate work above
  actually helps: `rush sessions cache` reports it per session, per
  model or per day, with `--since` to bound the period and `--json`
  for machines.
  Grouping is by the *producing* model, so a session that switched
  models mid-conversation is attributed correctly — something `sessions
  cost` cannot do, as it groups by the session's current model.
  The output declines to state numbers it cannot back up: the hit rate
  prints `n/a` rather than `0%` for providers that don't report caching
  (a fabricated zero is indistinguishable from a real miss), every
  table states its coverage when some messages have no usage recorded,
  and the per-day view omits the hit rate entirely because a day can
  span providers whose cache visibility differs. It also flags
  warm-to-cold cache transitions per session, distinguishing a genuine
  prompt-prefix change from the cache simply expiring after a normal
  idle gap.
- **Reasoning effort is now dispatched per CLI**, and validated against
  what each model actually accepts — codex's own registry stops
  `gpt-5.5` at `xhigh` while `gpt-5.6-sol` accepts `ultra`.
- **Claude 5 CLI models** — `claude-opus-5[1m]`, `claude-sonnet-5[1m]`
  and pinned `claude-fable-5`, with matching `opus5` / `sonnet5` /
  `fable5` short codes. The `[1m]` suffix is a real context-window
  switch: `claude-opus-5` reports 200k, `claude-opus-5[1m]` reports 1M.
- **Default-models modal (System / Folder / Session)** — the web UI's
  header now has a "Default models" button opening a modal with three
  blocks, one per level of the model cascade (system → folder →
  session). Each block shows all four slots (large, small, worker,
  reviewer): what's explicitly set at that level, or — when unset — the
  inherited value and which level it came from. The Session block can
  also set worker/reviewer per session for the first time (previously
  only large/small were session-scoped). Every explicit override can be
  cleared with one click, falling back to the level above.
- **"Inherit" in the session model selector** — the large/small model
  dropdowns in the chat toolbar now show an "Inherit" entry whenever the
  session has an explicit override for that slot, clearing it back to
  following the folder/system default instead of staying pinned forever.
- **GLM-5.3 support** — added as a new `glm5_3` atom (`rush models use
  glm5_3`, `rush ping --model zai/glm-5.3`) and, since neither docs.z.ai
  nor the upstream catwalk provider registry list the model yet, the
  Z.AI provider's model list is now supplemented with a provisional
  GLM-5.3 entry so it also shows up in the web UI's model picker.
  Verified live via `rush ping --model zai/glm-5.3`; context
  window/reasoning-level numbers are copied from GLM-5.2 pending
  official documentation.

### Changed

- **BREAKING — the tool is now called `rush`, not `crush`.** This fork
  had diverged far enough from upstream that sharing a name with it
  caused real confusion: which binary is on `$PATH`, which project an
  issue belongs to, which config a directory holds. The rename is a
  clean break with no compatibility shims, and it touches everything
  a user can see:

  - the binary is `rush` (`rush run`, `rush sessions`, …);
  - **all ~52 `CRUSH_*` environment variables are now `RUSH_*`**;
  - config and state move: `.crush/` → `.rush/`, `crush.json` →
    `rush.json`, `CRUSH.md` → `RUSH.md`, `.crushignore` →
    `.rushignore`, `~/.config/crush` → `~/.config/rush`,
    `~/.local/share/crush` → `~/.local/share/rush`,
    `%LocalAppData%\crush` → `…\rush`, `/etc/crush/crush.json` →
    `/etc/rush/rush.json`, and inside the data dir `crush.db` →
    `rush.db` and `crush.log` → `rush.log`;
  - the Go module path is `github.com/PHPCraftdream/rush`;
  - the npm package is `@phpcraftdream/rush`;
  - the installed slash-commands become `/rush` (`rush claude-init`
    and friends now write rush-named files under a new sentinel).

  **Existing installations do not migrate themselves** — run `rush
  migrate` (see Added) to rename on-disk state, and `rush cli-refresh`
  to update installed slash-commands. The one deliberate
  backward-compatibility carve-out: every `*-del` command still removes
  legacy crush-named files as well as rush-named ones, so uninstalling
  an old integration still works; a file at a legacy path that carries
  no recognized sentinel is refused with a warning rather than deleted.

- **The orchestrator can no longer edit files itself — it has to
  delegate.** With a worker model configured, three real `rush run
  --role smart` runs made *zero* calls to the `agent` tool out of 50,
  51 and 24 tool calls: the smart model did every edit itself while the
  worker sat unused. Everything about that was working as designed
  except the part that matters — the prompt asked it to delegate, and
  asking was not enough. `edit`/`multiedit`/`write` are now removed
  from the orchestrator's toolset outright when a worker is active, so
  hands-on work has to go through the worker. `bash` and every read
  tool stay, deliberately: the orchestrator's own zero-trust
  verification pass has to be able to re-read files and re-run tests
  itself, and delegating verification would mean trusting a worker's
  report about checking a worker's report.

- **Config-only commands start ~14× faster.** `rush models state`
  took ~10.85s: `setupApp` unconditionally ran startup recovery,
  started every MCP server, and built the whole agent coordinator
  (including skill discovery) even for commands that never touch a
  session or the coordinator. The `models`, `providers`, `mcp`,
  `login` and `queue add/list/show/rm/clear` families now skip all
  three — audited command by command, not guessed.

- **Startup recovery no longer scales with session count.** It used to
  list every session and pull each one's *entire* message history just
  to ask "was this session's last message an interrupted turn?" —
  measured at 137 sessions: 10.03s, which hit the sweep's own 10s
  deadline before it finished. A partial index plus a query that
  returns only the sessions whose chronologically last message is an
  unfinished assistant turn replaces the linear scan. This fork's
  design intent is N concurrent sessions, so a high session count is
  the normal case, not an edge case.

- **BREAKING — MCP servers no longer start automatically during `rush
  run` (and every other non-interactive CLI command) unless explicitly
  opted in.** New per-server config field `enabled_in_cli` (default
  `false`) gates it; the interactive web UI is unaffected and always
  starts every non-disabled server exactly as before. Profiling `rush
  run`'s memory found a globally-configured `stdio` MCP server
  (an `npx`-launched Node process) committing ~90-100MB on every single
  invocation — more than rush's own process — regardless of whether the
  task ever called it. **Existing MCP servers you actually want
  available to `rush run` need `"enabled_in_cli": true` added**
  (`rush mcp set <id> --enabled-in-cli=true`), or pass `--all-mcp` on a
  one-off invocation that needs everything.

- **BREAKING — the model slots are now called `smart` and `fast`**, not
  `large` and `small`. There were already two names for each of these
  two slots: `--role smart` / `--role fast` were the CLI's words, and
  everything under them called the same things large/small, with a
  translation layer in the middle. One vocabulary now runs end to end —
  config key, CLI flag, HTTP/WebSocket field, database column, and the
  description the agent itself reads when deciding which slot to ask
  for.

  **Existing configurations stop applying and must be edited.** A
  `rush.json` (global or workspace) with `"models": {"large": …,
  "small": …}` no longer matches either slot, so both fall back to
  their defaults. Rename the two keys to `"smart"` and `"fast"`. There
  is deliberately no alias-reading fallback: keeping one would have
  preserved exactly the two-names-for-one-thing problem this change
  exists to remove.

  Also renamed, in the same sweep: `rush models use --large/--small`
  → `--smart/--fast`; `rush models unset large|small` →
  `smart|fast`; `rush models bump large|small` → `smart|fast`;
  `rush run --small-model` → `--fast-model`; the `largeModel` /
  `smallModel` WebSocket fields → `smartModel` / `fastModel`; and the
  `sessions` table's six `large_model_*` / `small_model_*` columns →
  `smart_model_*` / `fast_model_*` (a `RENAME COLUMN` migration —
  existing session state is preserved, not rebuilt). `--role smart` and
  `--role fast` are unchanged: they were already right, and the rest of
  the system moved to meet them.

### Removed

- **The `glm5_2`, `glm5_1` and `glm5` short codes.** GLM-5.3 supersedes
  all three, and carrying four near-identical GLM-5 atoms made the list
  harder to read than it made anything easier to reach. `glm5_3` and
  `glm5_turbo` remain, as does the whole 4.x family. Nothing became
  unreachable: the raw form (`rush models use zai/glm-5.2@max`) still
  works for any Z.AI model id, whether or not it has a short code —
  only the abbreviation is gone.

### Fixed

- **The Inject button was silently doing something else.** It sends a
  message into the *currently running* turn — that is its entire
  reason to exist, and it is only visible while a turn is busy — but a
  refactor had every send path reshaped into `send_message`, so every
  online use of Inject quietly degraded into "queue this as the next
  turn" instead. No error, no visible difference until the turn
  finished and the message arrived late.
- **Queued messages could vanish on a reconnect race.** The queue was
  drained and *then* sent; if the socket closed between those two
  steps the messages were already gone from the queue and the send
  returned false. Sends now route through the offline outbox, and
  content that the outbox refuses (it is bounded at 100 frames) goes
  back into the per-session queue rather than disappearing.
- **The UI could get stuck "busy" forever after a disconnect.**
  Several places started a request, ignored that the send had failed,
  and then waited for a reply that could never arrive — worst case the
  Providers modal, which had no failure handling at all. All of them
  (provider edits, system-prompt save, project initialize, log
  loading, skills update) now go through one bounded request helper.
- **Three ways to open a window past the PowerShell guard**, each
  found by review rather than by incident: the `saps`/`sajb` aliases
  for `Start-Process`/`Start-Job` were not recognized at all; the
  window check did not look inside `iex`/`Invoke-Command` wrappers, so
  `powershell -c "iex 'saps notepad'"` passed; and `-ScriptBlock { … }`
  had its braced body treated as an opaque flag argument, so
  `Invoke-Command -ScriptBlock { Start-Process notepad }` was never
  inspected. All three now recurse into the wrapped content through
  the same checks as any other command.
- **A retried turn no longer duplicates your message.** When a turn
  ended in an error with no content, the retry re-sent the same prompt
  but did not tell the agent it was reusing the existing message row —
  so each attempt persisted the prompt again. Found investigating a
  real session where one user message appeared three or four times in
  its own history.
- **Scrolling up during streaming works.** Every streamed update
  re-triggered "scroll to bottom" if the view had last been at the
  bottom, and only a real scroll *event* could clear that flag — which
  the browser dispatches on a later task, not synchronously with the
  wheel. During fast streaming the snap-back kept winning the race,
  making it feel nearly impossible to scroll away. The wheel now
  disengages the magnet immediately.
- **GLM-5.3 and GLM-5.3-Flash reasoning effort.** Both models actually
  expose `low`/`high`/`max` — not the `off`/`on` toggle other GLM-5
  models use — so `rush models use --smart zai/glm-5.3-flash@high`
  failed, and `rush models state` showed a stale `on` value neither
  model accepts. Fixed everywhere the vocabulary is declared (CLI
  atoms, the wire-format switch, `rush models efforts`, and the web
  UI). Also fixed
  a more serious bug found while chasing this down: loading config could
  silently discard and overwrite a smart/fast model selection that
  wasn't yet in the cached provider catalog — on every single config
  load, including read-only commands like `rush models state` — making
  "select a model rush doesn't have cached yet" effectively impossible
  to keep.
- **Windows console windows no longer flash during `rush run`.**
  `SysProcAttr.HideWindow` alone does not set `CREATE_NO_WINDOW`; it
  only hides an already-created window after the fact, a race that lost
  under load. Both flags are now set together on every spawned child.
- **CI `lint` was failing on every push** (a reusable workflow owned by
  a different GitHub organization silently refuses external forks). The
  job is now self-contained.
- **A rare "close of closed channel" panic under heavy load**, traced to
  the checkpoint-write ticker attempting one more write with an
  already-cancelled context after being told to stop — internal-only,
  never reached a real user-visible failure, since the redundant write
  simply failed the same way any other cancellation would have.
- **Two missing database indexes** found by profiling: `rush run
  --continue` did a full table scan of every session to find the most
  recent one, and loading a session's message history paid an
  in-memory sort despite an existing index, because no index covered
  both the filter and the sort order together. Both closed with
  covering composite indexes, verified with `EXPLAIN QUERY PLAN`
  against a real database before and after.

- **`rush run` no longer reports success for work it did not do.** Three
  separate ways it could, all on the path that finishes a durable
  continuation before the process exits. A session held by another live
  owner counted as "completed here" the moment its row was leased, so a
  cancelled run became exit code 0 with a success envelope while the work
  sat queued for some later process. A turn whose commit failed after
  running was also reported as complete, leaving the row to be recovered
  and run a second time. And a run's own `--timeout` could not stop a
  continuation that had already begun, because the execution context was
  rooted in `context.Background()` and the passed-in one went unused
  entirely — the CLI returned an error while the turn kept writing
  messages behind it. That last one also let `App.Shutdown` close the
  database underneath a live execution, since the synchronous drain was
  not registered in the pump's shutdown accounting at all.
- **Two executions for one session can no longer clear each other's
  marker.** The background pump and the synchronous drain both read and
  wrote one per-session boolean, and disagreed about when: one checked
  then marked around a lease, the other leased first and marked
  unconditionally. Whichever finished first cleared it, leaving the other
  running while the session looked free to the next tick and to shutdown.
  Both now go through one atomic gate, and the only way to clear a mark
  is a closure held by whoever was admitted.
- **A malformed orphan-outbox row is quarantined instead of retried
  forever.** An entry that can never be enqueued was logged at ERROR and
  retried every 15 seconds for the life of the process — the retry budget
  had been dropped along with an older locking model. It is counted
  again, in a separate write that takes no ownership of the row, so the
  atomic drain is unchanged. A quarantined row stays in the table with
  its last error for an operator to look at; nothing retries it.
- **The update badge reaches a browser opened later.** It was sent once at
  server start and then evicted: the replay buffer new clients receive is a
  bounded ring, and one streaming turn pushes thousands of events past it.
  Start the server, let an agent run, then open the UI - which is how people
  actually use a UI - and the badge was already gone. It is now re-sent on
  every connect, independently of the ring.
- **No more spurious stall report at the end of a successful turn.** A turn
  waits briefly for its session title after the work is done, and
  `--timeout-hard-cap` is measured from turn start, so a turn finishing just
  inside the cap could be pushed over it by that wait alone - producing a
  stall dump for a turn that had succeeded, and killing the title it was
  waiting for. Opt-in, and cosmetic when it fired, but it reported a failure
  that had not happened.

- **A stale mid-stream snapshot can no longer overwrite a newer one.** The
  turn replaces its checkpoint writer every step and waits only briefly for
  the old one; that wait exists precisely for a database or filesystem that
  ignores cancellation, so a stale write could still land last. Crashing in
  that window left recovery reading an outdated checkpoint and replaying tool
  actions that had already run. The check moved out of the writer's memory
  and into the write itself.
- **The system prompt is built from the same configuration the model was
  chosen from.** A config reload landing mid-construction could produce a
  model from one generation and a prompt from the next - different context
  paths, skills and options than the model about to run them.

- **A guard against silently corrupted SQL codegen.** sqlc miscounts
  query spans when a `.sql` comment contains a multi-byte character,
  truncating the generated statement with a clean exit code — the failure
  only surfaces at process start. Every query file was ASCII by luck; the
  pre-push hook now makes it a rule.


- **A session title no longer goes missing when the model answers
  quickly.** Sessions could end up as "Untitled Session" with nothing in
  the log but two `context canceled` errors. The turn cancels its own
  context explicitly before returning, and title generation runs on a
  context derived from it — so any title that had not already finished
  was cancelled, and only then waited for. The wait exists precisely so
  a title can land. It now happens before the cancellation. This looked
  like a rare flake (about one test run in 27) purely because a fast
  provider usually won the race; on a loaded machine it would lose it
  more often.
- **The current tool activity group stays open again in assistant
  messages.** Every tool burst rendered in a message collapsed
  immediately, contradicting the surrounding code's own description and
  disagreeing with the identical component used elsewhere in the chat.
  The last burst of a message that is still streaming is now open; once
  the turn ends the transcript settles into collapsed history.
- **A message is now attributed to the model that produced it,
  consistently.** An assistant message carries the model twice: once on
  the row itself (which `rush stats` counts by) and once on its usage
  record (which `rush sessions cache` reports tokens and cost by). The
  per-turn write recorded the configured selection in both, while the
  summarisation paths recorded the executing model — so for a provider
  whose canonical id differs from the configured one, the same messages
  appeared under two names depending on which command you asked.
  Both now record the executing model, which is the one that can be
  checked against what the provider actually billed. Note `rush
  sessions cost` is unaffected either way: it groups by the session's
  model, not the message's. Rows written before this change keep their
  old attribution — there is no migration.
- **`rush sessions reap` removes a lock's `.pid` and `.gen`
  companions.** They were previously cleared only as a side effect of
  the probe's own background cleanup, which the process does not wait
  for — so a fast sweep could report `reclaimed 1 lock(s)` and leave two
  files in the directory it exists to tidy.
- **Two type errors in the web UI** that had been invisible for want of
  a typecheck, plus three dead declarations. One of them is the
  collapsed-tool-group bug above.
- **`rush sessions reap` no longer abandons a lock because something
  held the file open for a moment.** On Windows a file cannot be
  unlinked while any handle to it is open, and reap tried exactly once:
  if the handle let go a millisecond later, the operator was still told
  `failed to remove (… used by another process) … reclaimed 0 lock(s)`
  and had to run the command again. The likeliest holder was reap's own
  probe, since releasing a lock clears the holder's metadata in the
  background and reopens the file to do it, so the command could lose
  the race to itself. It now retries the unlink, using the same helper
  `rush sessions kill` has always removed locks with.

  The retry is bounded by a single three-second budget for the whole
  sweep, not per lock: a lock nobody can delete — a read-only volume, a
  directory whose ACL denies it — would otherwise cost three seconds
  each, turning a backlog of forty into two silent minutes. Once the
  budget is spent the remaining locks get one attempt each and the
  output says so. Unlike kill's `--wait`, this is not tunable.
- **A tool refusing your input no longer kills the whole run.** A
  handful of ordinary, correctable situations used to end the session
  outright, with nothing written to the log — the operator saw a run
  that simply stopped. The cause was one shared with every tool: in the
  agent library a tool that *returns* a Go error unwinds the entire
  agent loop, while a tool that returns an error *response* hands the
  message to the model, which can then fix its input and carry on. The
  two forms differ by a reflex-level edit in Go, so the dangerous one
  had accumulated. Now fixed where it was reachable:

  - a todo item whose `status` was misspelled or omitted — one bad
    enum value ended a run 42 seconds into a 75 000-character prompt;
  - a malformed or unreachable URL passed to `download`, `fetch` or
    `sourcegraph` — an HTTP 404 was already recoverable while a DNS
    failure was fatal, though both mean "the model gave a bad URL";
  - a path the OS rejects in `view`, `edit`, `write` or `multiedit`
    while the file is being *examined* — permission denied, a name too
    long, an embedded NUL, or a file that vanished between two
    syscalls. Note the limit: this covers the stat/read step only.
    Directory creation and the final write are a separate, still-open
    class, so a path whose parent is a file (`notes.txt/child.md`)
    still ends the run — on Windows always, since `os.Stat` reports
    that case as plain "not found" and the check above never sees it;

  - a full background-job table. This one hurt most: because rush
    auto-backgrounds long commands, an ordinary `ls` ended the session
    whenever 50 jobs happened to be alive — a normal state for a
    session running dev servers or watchers. The refusal even carries
    the text "Please terminate or wait for some jobs to complete",
    advice the model was structurally prevented from ever reading.

  Genuine infrastructure failures still end the run, and deliberately:
  a missing session ID or an unusable database fails identically on
  every retry, so continuing would only loop. The rule separating the
  two is now written down in `internal/agent/tools/tools.go` and
  enforced by a test that feeds every tool the kind of input a model
  actually sends.

- **Killing a CLI provider no longer leaves its grandchildren
  running.** `rush sessions kill`, a cancelled turn and a watchdog
  timeout all terminated only the direct child, so the processes it had
  spawned — `claude.cmd` → `cmd.exe` → `node.exe`, plus any MCP servers
  the CLI started itself — survived and kept running. On Unix there was
  no tree kill at all; on Windows `taskkill /T` walks the parent chain,
  which under Git-Bash/MSYS leads to a transient helper process that is
  already gone, so the grandchild could never be found. Children are
  now started as process-group leaders and killed as a group on Unix,
  and enclosed in a kill-on-close Job Object on Windows, which does not
  depend on the parent chain. The same leak had already been fixed once
  for stdio MCP servers, where it had accumulated 15+ orphaned
  processes over two days.

  Two limits worth knowing, both Unix-only. The Windows Job Object
  doubles as a safety net — if rush itself is killed, the OS closes
  the handle and the tree goes with it — and Unix has no equivalent, so
  `rush sessions kill`, which targets rush's own pid, does not reach
  a CLI child that now sits in its own process group. For the same
  reason a second Ctrl-C (the first is handled; the second gets Go's
  default handler and kills rush outright) no longer reaches the CLI
  through the terminal's foreground group. Both are being tracked; the
  ordinary cancel, watchdog and timeout paths are unaffected.

- **Reasoning effort no longer breaks non-Claude CLI runs.** The
  session's stored effort was appended as `--effort <level>` to
  whatever CLI was launched, but only `claude` has that flag: codex
  aborts with `unexpected argument '--effort'`, gemini and qwen with
  `Unknown argument: effort`. That is 9 of the 19 registered CLI model
  specs. Because the effort is a persisted session column, the live
  path was "set an effort on a Claude model, switch that session to
  codex/gemini/qwen, every turn dies". Effort is now dispatched per
  CLI, and a level the target model doesn't accept is dropped (falling
  back to the model's own default) rather than forwarded into a
  provider error.
- **Codex no longer double-counts cached prompt tokens.** Its
  `input_tokens` already includes `cached_input_tokens`, but the two
  were summed — a measured turn reported 23 768 prompt tokens where the
  real total was 16 856, a 41% overstatement that inflated the session's
  token counter and pulled auto-summarization forward.
- **Gemini's cache statistics are no longer discarded.** The CLI emits
  `cached` and an exclusive `input` alongside `input_tokens`, but
  neither field was declared in the parser struct, so both were dropped
  at unmarshal and gemini appeared not to support caching at all.
- **Prompt-token accounting no longer omits cache-write tokens.** The
  session's prompt counter summed only fresh input plus cache reads,
  ignoring tokens written *into* the cache. Since the three classes are
  disjoint this understated the prompt for every provider reporting
  them separately: a measured turn recorded 5 842 tokens where the real
  prompt was 22 826, a 74% understatement — and in the dangerous
  direction, since the counter drives auto-summarization and
  understating it delays compaction until the context window overruns.
- **Switching a session's model no longer changes the system-wide
  default.** Picking a model in the chat toolbar (or, more subtly, just
  sending a message) used to call the same code path as `rush models
  use`, silently rewriting the global `rush.json`'s default model to
  whatever the most recently active session happened to be running —
  so every OTHER session, every other folder, and the next CLI
  invocation would drift to a model nobody explicitly chose there. The
  chat toolbar's model pickers are now session-scoped only; the global
  default changes only through `rush models use` or the new
  System/Folder blocks in the "Default models" modal above.
- **Switching only the large (or only the small) model no longer
  freezes the other slot.** The picker used to always send both
  large+small together, re-pinning whichever one you didn't touch to
  its then-current value — so a session that only ever had its large
  model switched would stop following later changes to the folder/
  system default for its small model. Each slot is now set (or cleared)
  independently.
- **A freshly created session no longer freezes its model at birth.**
  New sessions used to have the then-effective large/small model
  written into their own row immediately, which — same root cause as
  above — silently opted every untouched session out of ever following
  a later folder/system default change. New sessions now start with no
  override and genuinely inherit, the same way `rush run` sessions
  always have.

- **`rush models use --large`/`--small`** — the two positional args
  (`rush models use <large> <small>`) always set large and small
  together, so there was no way to change just the fast/small model (or
  just the smart/large one) without retyping the other. `--large` and
  `--small` now exist alongside the already-independent `--worker`/
  `--reviewer` flags, so any of the four slots can be set on its own —
  e.g. `rush models use --small glm4_7_flash`. The positional form and
  the `--large`/`--small` flags are mutually exclusive per call (mixing
  them is rejected with a clear error, rather than silently preferring
  one).

- **Nine release-blocking concurrency bugs from the 2026-08-07
  release-concurrency review, closed and independently verified** (see
  `docs/reviews/2026-08-07-release-concurrency-review.md`). The review's
  central invariant: once an action is accepted, exactly one of three
  things must be true — a live owner is responsible for it, a freshly
  started runner is responsible for it, or a durable record exists that
  a future worker can claim. Every owner-exit path (success, cancel,
  error, panic, shutdown, or OS-lock contention) must transfer or
  reject all pending work atomically; nothing may be silently dropped.

  - **Ownership exit could silently drop an accepted call** — both the
    normal end-of-turn path and manual compaction's exit path had
    windows where a call already accepted into the mailbox could be
    lost instead of handed off or requeued. Replaced with one unified
    ownership-exit finalizer used by both paths.
  - **A call orphaned by OS-lock contention during a detached restart
    could be lost after retries were exhausted** — the retry loop gave
    up silently on persistent contention. It now requeues the call for
    a future `Run()` durably, and the same fix applies to the
    cross-process interrupt-inject path (the `pending_injects` row is
    recreated instead of vanishing after `ConsumeInterruptInject`
    already deleted it).
  - **Web "rerun message" could race a still-stopping session's
    transcript writes** — `rerun` now fails closed (rejects the
    request) instead of proceeding while the session's prior turn is
    still tearing down.
  - **Manual/standalone compaction could summarize using a model or
    provider that had already changed underneath it, and a failed
    cleanup write could be silently skipped if the context was
    already cancelled** — the summary snapshot (model + provider) is
    now captured immutably at the start of the summarize call, and the
    final DB cleanup write uses a cancel-immune bounded context so it
    still runs even after the parent context is gone.
  - **Shutdown could return before live dispatchers actually stopped**
    — `App.Shutdown()` fired a 5-second context that nothing
    unconditionally waited on. It now genuinely joins the live
    dispatchers (bounded, so a stuck one can't hang shutdown forever)
    and logs a warning if it had to force-exit while an agent was
    still busy.
  - **Web "busy"/"queued" events and the summarize-queue drain didn't
    track real mailbox ownership** — a client could see `Busy: false`
    broadcast while another owner still held the session, and a
    summarize request queued from a non-web path (CLI, detached run)
    could sit un-drained. Both are now tied to the mailbox's actual
    ownership transitions instead of a best-effort approximation.
  - **Manual compaction released the OS session lock through a raw,
    synchronous call instead of the same `mbReleasing` state machine a
    normal turn uses** — same-process observers (`IsSessionBusy`,
    `Cancel`) could briefly see "idle" while a slow lock release was
    still in flight. Manual compaction now goes through the same
    `beginRelease`/`finishRelease` epoch-protected transition as a
    normal turn's drain.
  - **Manual/standalone compaction had no idle-timeout watchdog, and
    provider cancellation wasn't a tested execution-boundary
    contract** — an HTTP provider stream could ignore a cancelled
    context indefinitely with nothing catching it. Compaction now runs
    under the same stream-watchdog mechanism as a normal turn, and
    provider cancellation is now covered by a conformance test.
  - **`internal/session` lock release cleaned up best-effort diagnostic
    metadata before the actual OS-level unlock**, so a hang in the
    diagnostic part could delay the real unlock. `SessionLock.Release()`
    now unlocks first, unconditionally, before touching diagnostics.
  - **Final audit pass**: two of the nine regression tests added by the
    fixes above proved only that a call was durably *stored* (in a
    queue or a recreated DB row), not that it was ever actually
    *executed* by a subsequent owner — the review's own "persistence is
    not execution" bar. Both were strengthened with a second phase that
    releases the OS lock, triggers the real pickup path, and asserts on
    an atomic provider-call counter plus message history.

- **Thirteen more release-blocking concurrency findings from the
  2026-08-09 follow-up review round** (see
  `docs/reviews/2026-08-09-release-concurrency-followup-review.md` and
  `docs/reviews/2026-08-09-oh-round-review.md`), closed and
  independently verified with an executable release-gate suite (see
  `docs/release_gate_summary.md`, `docs/release_gate_report.md`,
  `go test -run TestReleaseGate ./internal/agent/... ./internal/app/...`):
  - **`SessionLock.Release()` could still leave the mailbox permanently
    stuck in `mbOwned`/`mbReleasing`** if the unbounded filesystem I/O
    in its diagnostic-metadata cleanup hung — the OS-level unlock and
    file close now happen synchronously, first, unconditionally; the
    best-effort diagnostic-metadata cleanup (clearing the stale PID a
    `sessions kill`/`sessions why` reader could otherwise see) moved to
    a goroutine that Release() waits on for a short bound (50ms) before
    giving up and letting it finish in the background — long enough to
    cover real disk I/O, short enough that a genuinely stuck filesystem
    still can't meaningfully delay the caller. (A stricter guard that
    re-checked the OS lock before touching the metadata was tried and
    reverted: under real scheduler contention it occasionally collided
    with the SAME process's own fast re-acquire of a session it had just
    released, which was a worse regression than the narrow, accepted,
    cosmetic risk it was meant to close — see `sessions_kill.go`'s
    re-probe of the real OS lock immediately before any destructive
    action, which is what actually guards against a stale PID mattering.)
  - **Detached/orphaned calls that exhausted their retry budget had no
    guaranteed runner** — some could be lost entirely instead of
    landing in a durable queue. Replaced the three separate ad hoc
    detached-run policies (`restartOrphaned` / `restartOrphanedWithRetry`
    / `startDetachedRun`) with one durable per-session run queue
    (`session_run_queue`) and an independent `RunQueuePump` that
    autonomously picks up and executes queued calls, including across
    process restarts.
  - **Retry tails could requeue and re-execute a call that had already
    left a persistent trace**, producing duplicate provider calls and
    duplicate messages — retryable vs. terminal failures are now
    classified via an `AlreadyAttempted` marker interface
    (`ErrCallAlreadyAttempted`), and a call already recorded is never
    replayed.
  - **Manual/queued summary (`/compact`) could pick up a different
    session's model or provider mid-flight** if models were changed
    concurrently — it now resolves and freezes one immutable snapshot
    (model, provider options, prompt prefix) from the target session at
    the start of the call.
  - **A second manual `/compact` requested while the first was still
    running could get stranded** — the queued request is now drained
    and coalesced from every terminal ownership transition, not just
    the web handler's own code path.
  - **Shutdown could close the database while a non-cooperative agent
    was still genuinely running**, and forced shutdown could hold the
    global DB connection-pool mutex across a slow `sql.DB.Close()` —
    graceful shutdown now does a real join on every live dispatcher
    (not a polling loop) before releasing the DB, and forced shutdown
    skips the DB release entirely rather than risk closing it under a
    live writer.
  - **Provider cancellation on ctx timeout wasn't a verified contract
    per adapter** — added a conformance suite covering every HTTP
    provider category (openaicompat, openai, anthropic, azure, bedrock,
    vercel, openrouter) plus the CLI-provider process-tree-kill path,
    each proving a hung stream stops within 5 seconds of cancellation.
  - **A durable-queue entry that exhausted its max retry attempts could
    never actually be removed from the queue** — the cleanup call
    required the entry to be in the `leased` state, but an
    attempts-exhausted entry is scanned while still `pending`, so the
    delete silently never matched and the pump re-attempted (and
    re-failed) the same cleanup on every tick, forever. Fixed to lease
    the entry before the terminal-fail delete.
  - Several package-global mutable test seams used under `t.Parallel()`
    (a data-race and cross-test-pollution risk) were replaced with
    per-instance functional options, and a batch of regression tests
    from this round that had silently weakened over revisions (sleep-
    based races, gutted fault injections, mock-only coverage of the
    riskiest changes) were rewritten to restore their original
    guarantees.

- **`sessions locks` showed a healthy, actively-working session as
  "offline"** — observed live: a session sat at `PULSE_AGE == ELAPSED ==
  36s` while the process was alive and running real tool calls. The
  heartbeat mtime only advances on LLM-stream-chunk-gated activity, not
  on tool-call execution itself (a deliberate earlier design choice, to
  avoid a hung tool being masked as healthy by a timer that fires on
  tool-open alone) — so a session can legitimately run several tool
  calls in a row with no heartbeat touch for well over the offline
  threshold. The underlying call-tree activity signal already tracked a
  session's own top-level activity correctly; the display code was
  discarding it whenever it didn't come from a sub-agent delegation.
  Fixed to use the freshest signal regardless of source, while still
  only labeling it as a sub-agent delegation when it actually is one.

- **Two messages created in the same second could be permanently
  skipped in `sessions watch`/`sessions tail`, not just printed out of
  order** — timestamps are stored with one-second precision, and
  same-second ties used to fall back to comparing message IDs (random
  UUIDs). Whenever that comparison happened to lose, the message was
  silently dropped for good: the "already printed" cursor never moved
  past it, so the same losing comparison repeated identically on every
  later poll. The database already orders messages deterministically
  (insertion order as a tiebreaker); the display code now trusts that
  order instead of re-deriving a worse one.

- **`sessions pick` could hand off to a session in the wrong data
  directory** — after picking a session with `--data-dir` pointed
  somewhere non-default, the follow-up `sessions tail`/`sessions last`
  it spawns didn't forward that flag, so the child re-resolved to the
  default location and reported "session not found" for the very
  session just displayed.

- **Same-named file attachments uploaded within the same second could
  silently overwrite each other** — attachment filenames were built
  from a one-second-precision timestamp plus the original filename
  alone; two uploads of e.g. `report.txt` in the same second landed on
  the same path, and the second silently replaced the first's content.

- **Concurrent goroutine diagnostic dumps from one process could
  collide** — several stuck sub-agent turns firing the stream watchdog
  within the same second could overwrite or interleave each other's
  diagnostic dump, exactly when that evidence is most needed.

- **The hard-cap watchdog's diagnostic log could report a near-zero
  turn duration for a multi-hour timeout** — a copy/paste-style gap
  meant the out-of-tool branch reported time since the last stream
  chunk instead of the actual turn length, which is misleading
  precisely in the one situation (a postmortem) this log line exists
  for. Does not affect the timeout decision itself, only its own log
  message.

- **Shutdown could still begin a brand-new turn, and an interrupt during
  shutdown was reported as accepted** — a closing review of the fixes
  below found the shutdown work only half done. Refusing to hand more
  queued work to a turn already in progress does not stop a *new* one
  from starting: sessions are tracked lazily, so a request for a session
  the shutdown sweep never saw got fresh state and ran a full turn that
  nothing would then cancel. Shutdown now refuses new work at the agent
  level, before any session is claimed, and says so with a clear error
  instead of silently accepting a request that could never run. An
  interrupt arriving during shutdown likewise no longer reports success
  for a message that will not be executed.

- **A hung title could overwrite a good one, repeatedly** — a regression
  from the title fix below. Once a stuck title attempt is abandoned, a
  later turn generates a title normally; the abandoned attempt could then
  finish and overwrite it with the placeholder name, which in turn made
  the next turn try again, and so on. A late-finishing attempt now only
  fills the title in if it is still empty.

- **A delegated sub-agent could hang forever on its very first command,
  making the parent look busy while nothing happened at all** — the most
  visible bug of this round, reproduced live: a session sat in
  `delegating` with a healthy heartbeat for 41 minutes while its
  sub-agent was stuck on a bare `wc -l`. A non-interactive `rush run`
  auto-approves only the session id it was given, but a sub-agent runs
  under its own child session id, so its first command outside the
  read-only safe list waited on a permission prompt that does not exist
  in that mode. Sub-agents now inherit their parent's auto-approve
  status. Deliberately inheritance and not a blanket grant: a sub-agent
  of an interactive session still prompts as before, and
  `--restrict-run` still applies on top.

- **Shutdown could start a new turn instead of stopping** — cancelling
  every session on exit cancelled only each one's current turn, and the
  cancel-handling path then immediately pulled the next queued message
  in and began a fresh provider request, on a context nothing had
  cancelled. The process then tore its database down underneath an agent
  that was still working. Shutdown now latches every session closed
  first, so a cancelled turn can no longer pick up more work, and
  cancels the whole run rather than just the current turn. Anything
  still queued is left in place — it is already saved, and a later run
  picks it up — rather than being discarded.

- **An interrupt sent just as a turn finished could be accepted and then
  never run** — the interrupt was recorded and reported as successful,
  but only the cancellation path ever looked for it. When the cancel
  lost the race to the turn's own completion (which it can), the normal
  end-of-turn path released the session without ever seeing the pending
  message. Every end-of-turn decision now checks it.

- **Interrupting an idle session accepted the message but never ran it**
  — with no turn in progress there was nothing to interrupt, and the
  message was queued for a runner that did not exist, while the UI was
  told it had been queued successfully. It now starts a run.

- **A hung title generation could hold a whole turn open indefinitely** —
  the turn waited unconditionally for its background title goroutine,
  which is itself unbounded when a provider ignores cancellation. The
  session's ownership and lock stayed held for work that had already
  finished. The wait is now bounded; a title that overruns is abandoned
  and still saves itself if it eventually completes.

- **A wedged tool looked exactly like a working one** — the session
  heartbeat was refreshed on a timer for as long as any tool call was
  open, regardless of whether anything was happening, so `sessions
  locks`/`why`/`list` reported a stuck session as healthy for up to 45
  minutes (38 minutes of false "activity" were observed in practice).
  Activity is now recorded only for real progress. Long delegations stay
  covered without the timer: a sub-agent's genuine progress already
  propagates up through every ancestor session.

- **`sessions watch` spammed a once-per-second "sub-agent active" line
  and hid what the sub-agent was actually doing** — the line's throttle
  compared its own text, which embedded a live-updating age, so it
  changed every second and never throttled. Watch now live-tails
  delegated sessions the same way it shows the main session — real tool
  calls and results as they land, prefixed so they stay distinguishable
  — and covers every session in the call tree, so parallel delegations
  and a sub-agent's own sub-agent are visible too.

- **An interrupt or cancel landing right after a normal follow-up message
  was queued mid-turn could also silently do nothing** — a fourth
  independent review found the same defect the fix above closed on the
  legacy-queue reclaim path was still present on the far more commonly
  hit path: the current, non-legacy way a follow-up message gets queued
  while a turn is running. Fixed the same way — restore a working cancel
  handle at the moment ownership continues to the next turn — plus a
  small hardening (explicitly re-assert ownership state on reclaim
  instead of relying on it already being true) aimed at the same class
  of defect before it can resurface on a future code path.

- **A same-process concurrent send landing right as a turn finished could
  get a spurious "session already in use" error and be silently dropped,
  and an interrupt/cancel landing in the legacy-queue reclaim window could
  silently do nothing** — a third independent review of the mailbox
  migration (following the two fixes above) found no remaining bugs
  inside the mailbox's own state machine, but found two narrower defects
  at the seam between the mailbox and the OS-level session lock/cancel
  lifecycle. The end-of-turn drain used to flip the mailbox to "idle"
  well before the OS-level session lock was actually released (only once
  the whole call unwound, including up to several seconds waiting on
  title generation) — so a second call for the same session could
  legitimately see "not busy," try to start, and hit "already in use"
  from its own process's not-yet-finished prior turn, with the message
  lost rather than requeued. Fixed by releasing the OS lock and flipping
  the mailbox to idle as one atomic step, so "not busy" now always means
  the lock is genuinely free. Separately, reclaiming ownership from the
  legacy message queue left both of the mailbox's cancel handles pointing
  at nothing usable until the reclaimed turn got underway — a Cancel or
  interrupt landing in that narrow window silently did nothing (an
  interrupt-and-replace even reported success while cancelling nothing).
  Fixed by restoring a working cancel handle at the same moment ownership
  is reclaimed.

- **A message queued via the legacy path could still permanently wedge a
  session busy, and a message dropped on turn error was lost instead of
  retried** — two narrower follow-ups to the P0-2/P0-3 mailbox-migration
  fixes above, found by a second independent review of the fix itself.
  Reclaiming ownership from the not-yet-migrated legacy message queue
  used to grant a brand new ownership era without telling the turn loop,
  which kept using its original (now stale) era id for the rest of the
  call — the next release attempt then saw a mismatch, assumed a
  different owner held the session, and left it stuck busy forever with
  nobody running it. Reclaiming now explicitly continues the SAME era
  instead of starting a new one. Separately, a message still queued when
  a turn ended on a genuine error (not a cancellation) used to be logged
  and discarded by the cleanup step; it's now put back on the queue so
  the next run picks it up, matching how a stale reservation used to
  recover before the migration.

- **`rush run` hung for a full 5 seconds on exit after any turn ran, and a
  turn that errored out with a message still queued could wedge that
  session permanently busy** — introduced by the per-session owner/mailbox
  migration's first two stages (P0-3, P0-2) and found the same day by an
  independent review of that migration. Two related defects:
  - `IsBusy()`/`CancelAll()` still read the `activeRequests` map directly,
    but the migration stopped clearing its plain-session-id entry on
    release (only the mailbox's own state is cleared now) — so
    `IsBusy()` returned `true` forever after a session's first turn, and
    `App.Shutdown()` (reached by every `rush run` via
    `defer a.Shutdown()`) always burned its full 5-second busy-drain
    timeout instead of returning immediately once idle. Both now read the
    mailbox's state directly.
  - `Run`'s cleanup step ran unconditionally on every return path — not
    just the pre-loop bail-outs it was meant for — including after the
    turn loop's own drain had already released the session, or after an
    early-error return that left work still queued with nobody left to
    run it. On the "still queued" branch it left the session marked
    busy forever with no owner; on the "already released" branch, if a
    concurrent turn had since claimed the session, it could silently
    flip that live turn's ownership back to idle out from under it. The
    mailbox now tracks which ownership era each caller was granted, so a
    stale cleanup call is a safe no-op instead of clobbering a different,
    later owner, and it always fully releases the session (logging what
    was dropped) instead of leaving it stuck.

- **Starting any `rush` process could kill a long-running session in a
  DIFFERENT process (release blocker)** — the startup recovery sweep
  (`recoverInterruptedTurns`, which runs on every process start) walked
  **every** session in the data directory, not just this process's own,
  and stamped `FinishReasonError: "Process restarted"` on any assistant
  message that lacked a finish part and was older than 30 seconds. It
  performed no cross-process liveness check of any kind. Since a turn is
  legitimately unfinished for as long as it runs — a sub-agent
  delegation is bounded at 45 minutes — essentially every non-trivial
  turn was exposed: a sibling `rush` merely starting up marked it as
  crashed. Worse, `message.Update` rewrites the whole `Parts` blob from
  the snapshot the sweep read, so the stamp also **clobbered** whatever
  the live owner had streamed in between. This hit the fork's core use
  case (N concurrent `rush run` sessions sharing one data directory)
  routinely rather than rarely; an observed 38-minute delegation was
  killed this way and produced zero file edits. The sweep now proves no
  other live process owns a session — via `session.InspectSessionLock`,
  the same discipline `sessions kill`/`locks`/`reap` already use —
  before touching it, and skips it entirely if a live holder exists.
  The 30-second age threshold is demoted to a secondary check. A
  genuinely orphaned turn is still recovered exactly as before.

- **A message queued at the exact instant a turn released its session
  reservation could be silently orphaned (P0-3)** — the owning turn's
  final check ("is anything queued?") and the reservation release
  (`activeRequests.Del`, run from a `defer` back in `Run`) were two
  separate, non-atomically-linked steps. A concurrent send landing in
  the gap between them would see the session still marked busy, queue
  itself, and then nobody was left watching that queue: the owner had
  already decided "nothing queued" and returned, and its deferred
  release ran afterward with no further check. First step of the
  per-session owner/mailbox migration
  (`docs/plans/2026-08-04-session-owner-mailbox-design.md`) closes
  this: the "is anything queued" check and the reservation release now
  happen inside the SAME lock (`mailbox.drainOrRelease`), so a
  concurrent send can only ever land before or after that atomic
  transition — either the departing owner picks it up, or the sender
  becomes the new owner — never in a gap where neither does. Also
  applies to the standalone `/compact` follow-up drain that had the
  same shape.

- **"Interrupt and send" discarded the very message it was sending
  (P0-2)** — `InterruptAndSend` queued the replacement and then called
  `Cancel`, and `Cancel` unconditionally cleared the message queue — so
  the replacement was deleted by the line immediately after the one that
  queued it, on every call, deterministically. The web UI's interrupt
  button and `rush sessions inject --interrupt` therefore silently
  dropped the user's new request. Both now route through one atomic
  `InterruptAndReplace` operation that records the replacement and
  cancels only the in-flight generation under the same lock, so the
  turn loop stays alive to run it next. Related, previously unnoticed:
  a **bare** cancel (Ctrl-C, `sessions kill`, a cost/token cap) also
  wiped anything a caller had queued moments earlier for unrelated
  reasons; `Cancel` no longer touches queued work at all, leaving
  `ClearQueue` as the one explicit "discard everything" operation.

- **`queue run`'s spawned children now inherit the parent's explicit
  `--data-dir`** — `queue run` resolves its own data directory once
  (honoring an explicit `--data-dir` on the parent invocation, or a
  configured `data_directory`) to open the queue DB and acquire
  `queue.lock`, but `runQueueTask` never forwarded that resolved path
  to the `rush run --session ...` subprocess it spawns per task. Each
  spawned child independently re-resolved its own data directory
  starting from `--cwd`, which diverges from the parent's when
  `--data-dir` was passed explicitly — a queued task's child process
  could read/write its session and messages against a different DB
  than the one the queue claimed the task from. `runQueueTask` now
  takes the parent's resolved `dataDir` and passes it through as
  `--data-dir` on the child's argv.

- **Web UI attachments now honor the configured data directory** —
  `saveAttachmentToDisk` (in `internal/server/handlers.go`) always wrote
  uploaded attachments to `<cwd>/.rush/attachments/`, hardcoding both the
  working directory and the `.rush` segment instead of using the
  resolved `data_directory`/`--data-dir`. With a non-default data
  directory configured, attachments landed in a location the rest of the
  app doesn't read from. It now takes the already-resolved data
  directory (the same `externalOwnershipDataDir` helper that
  `annotateExternalOwnership` uses) and writes
  to `<dataDir>/attachments/`; a nil-config edge case defensively falls
  back to the old `<cwd>/.rush` default rather than hard-failing an
  otherwise best-effort save.

A separate review flagged the session heartbeat as reporting "alive"
for a fully deadlocked process (no real progress, mtime still fresh)
and a backlog of eight lower-priority follow-ups from the stability
review below. Closed together, one task/commit at a time:

- **Session heartbeat now reflects real activity, not a blind timer**
  — the lock-file heartbeat touched its mtime on every 10s tick
  unconditionally, so a wedged session with zero forward progress
  still looked alive to diagnostics forever. It's now gated on actual
  activity recorded since the previous tick (`SessionLock.RecordActivity`),
  and that activity signal is wired through the agent's normal turn
  loop (every stream callback) and propagated up through a
  delegation chain, so a parent session's heartbeat correctly stays
  alive purely from a sub-agent's real progress while the parent is
  blocked waiting on it — not just during the parent's own stream
  callbacks. A follow-up review caught a real gap in this fix: the
  activity signal only covered stream callbacks, not a tool actually
  *executing* — a healthy session blocked on one long tool call (up to
  45 minutes) recorded zero activity for its whole duration, which
  `sessions locks`' auto-delete (age > 60s) and `sessions watch`'s
  liveness check (age > 20s) both still read as "the process is dead."
  `sessions locks` could then delete a *live* session's lock file
  (unlinking a still-held OS lock lets a second process create a fresh
  one at the same path — two processes, one session id) and
  `sessions watch` could print a false "session finished" summary for
  a session still actively working. Fixed: the watchdog now records
  activity on every tick a tool is in flight and healthy, not just at
  start/finish; both commands additionally verify against the real OS
  lock / process liveness before trusting a stale-looking mtime. A
  further review pass found the same stale-mtime blind spot in two more
  places: the web UI's session-ownership indicator could flicker off
  during a long tool call (letting the composer re-enable and a send
  fail with "already in use," or the live tail stop following); and
  `sessions inject --json` could report a running session as
  `persisted-offline`. Both now fall back to a real process-liveness
  check when the heartbeat mtime looks stale, matching `sessions
  locks`/`sessions watch`'s existing fix. Separately, `sessions locks`
  itself still ignored a configured `--data-dir`/`data_directory` (the
  same class of bug already fixed for `sessions kill`/`reset --force`)
  and read a lock's PID in a way that missed the Windows PID-sidecar
  fallback — both fixed. A residual, narrow TOCTOU window in the
  auto-delete probe (proving a lock is dead, then removing it as a
  separate step) is now explicitly documented rather than silently
  assumed airtight.
- Added missing regression coverage for the queued-message-continues-
  after-summarize/compact behavior (both the mid-turn auto-compact
  path and the standalone `/compact` path), which had zero tests.
- Fixed a pre-existing, unrelated data race in a stream-watchdog test
  helper (an unsynchronized counter shared between the test goroutine
  and a fake HTTP handler goroutine).
- **A `--timeout-hard-cap` fire could be misreported as the wrong kind
  of timeout, in two steps.** First, when the hard cap fired while a
  tool happened to be in flight, the watchdog internally misclassified
  it as a tool-specific timeout (reporting `toolTimeout=true` even
  though the never-freeze tool-pause backstop never fired) — fixed by
  correcting that boolean so the hard-cap-with-tool-in-flight case
  agreed with the plain hard-cap-on-idle case. That fix alone wasn't
  enough: both cases then collapsed into the SAME "not a tool timeout"
  signal as a genuine provider idle-stall, so the user-facing finish
  message still had only two branches ("Tool timeout" / "Stream
  stalled") and every hard-cap fire fell into "Stream stalled" —
  falsely blaming the provider and citing `idleTimeout` instead of the
  hard cap that actually fired. The watchdog now carries a three-way
  cause (tool timeout / hard cap / idle stall) all the way through to
  the finish message, which gets its own "Turn timeout" title citing
  the actual configured `--timeout-hard-cap` duration and blames
  neither the provider nor a tool.
- **`sessions kill` and `sessions reset --force` ignored a configured
  data directory** — both hardcoded the lock/data path to `<cwd>/.rush`,
  so an operator using `--data-dir` or a project's `data_directory`
  config silently got "no lock file found" instead of the rescue these
  commands exist to perform. Both now resolve the actual configured
  data directory. A follow-up review found the fix still diverged from
  `reset --force` for a *relative* `--data-dir` combined with `--cwd`
  (resolved against the wrong base directory) and depended on a config
  load path that made a 45s network call and could write to the
  operator's real global config as a side effect of just killing a
  stuck lock — both fixed; `sessions kill` now resolves the data
  directory the same way `reset --force` does, without any network or
  config-persistence dependency.
- Three follow-ups to the stdin idle-timeout fix: partial stdin
  content returned after the producer goes idle now carries an
  explicit marker (visible to the model, not just a log line) noting
  it may be truncated; a narrow boundary race that could silently drop
  a chunk arriving at almost the same instant the idle timer fired is
  now guarded against; and a test-only mutable-package-global pattern
  was replaced with dependency injection. The truncation marker also
  wrongly claimed "the producer went idle" when the real cause was a
  read error — it now names the actual cause.
- Removed a tautological sessions-kill test that never called the
  function it claimed to test, and documented (without attempting to
  structurally close, out of proportion for a manual rescue CLI) a
  narrow residual PID-reuse race between proving a session is held and
  actually killing its holder.

An independent review of the batch below (task-276 investigation + the
multi-agent stability review) found four more issues in the fixes
themselves, closed in the same way:

- **The parent/child delegation cleanup grace didn't actually change
  the race it fixed** — the 90s grace was added to both the parent's
  wait AND the child sub-agent's own watchdog identically, so it
  canceled out of the "child fires first" inequality algebraically
  and the parent still always won. The grace now applies only to a
  top-level (delegating) session; a sub-agent, which can never itself
  be waiting on a further nested delegation, gets none — so its own
  stuck-tool detection fires strictly before the parent's.
- **`rush sessions reset --force` had the same stale-PID kill bug
  just fixed for `sessions kill`** — a sibling code path that read a
  lock file's recorded PID and killed it unconditionally, missed by
  the original fix because it lives in a different file. Now routes
  through the same probe-before-kill helper.
- **A web `/cancel` during a queued turn's DB preamble could silently
  no-op** — the turn loop's cancel function was only re-registered
  once, before the first turn; from the second queued turn onward, a
  cancel request during that turn's DB preamble found a spent,
  already-fired cancel func from the previous turn and did nothing.
  Now re-armed before every turn.
- A second, near-identical `t.Parallel()` + mutable-package-global
  pattern (title-generation's own timeout, not the DB-preamble one
  fixed earlier) was converted to the same per-agent injectable field.

Two more, explicit product decisions/fixes:

- **`--agents single` vs. a configured Worker** — confirmed as
  intentional: a configured Worker model always wins over an explicit
  `--agents single` (unchanged runtime behavior). What WAS wrong was
  the documentation: the `--agents` flag's `--help` text, an inline
  code comment, and the installed `/rush` slash-command guide all
  flatly claimed `--agents single` was an absolute guarantee against
  delegation — the literal opposite of the real behavior, and
  actively misleading for an orchestrating agent deciding how to
  invoke `rush run`. All three corrected, no behavior changed.
- **stdin named-pipe read could hang indefinitely again** — the
  previous fix (above) bounded only the wait for the *first* byte,
  then read to EOF with no further timeout at all; a producer that
  wrote one chunk and then went silent forever (no close, no more
  data) hung `rush run` indefinitely. Replaced with a real idle
  timeout applied to the whole read, chunk by chunk, that resets on
  every chunk received — `rush run` can now only ever block for
  "the grace window since the last byte was seen," never longer,
  regardless of how the pipe behaves afterward.

A second, independent review of the fixes above (this time targeting
the follow-up commits themselves, not the original batch) found two
more things:

- **The `agentic_fetch` tool's own nested delegation wasn't
  classified as a sub-agent** — it runs through the same delegation
  path as a real worker sub-agent, but its `SessionAgent` never set
  `IsSubAgent`, so it was invisible to the parent/child cleanup-grace
  fix above and quietly kept the old symmetric-cancel-out bug on that
  one path. One-line fix.
- **The cleanup-grace fix's own documentation overstated what it
  guarantees** — it only protects a child that wedges *early*
  (roughly within the grace window of being delegated to); a child
  that works productively for a while and only wedges deep into its
  turn still loses the race to the parent's watchdog, same as before
  that fix. Not a correctness issue (the parent still terminates
  correctly, and cost accounting is already cancel-immune, see
  above) — just a claim that needed correcting before someone trusted
  it. Documented as an explicit, tested, known limitation instead of
  a silently overstated guarantee.

Concurrency and reliability hardening, found via an independent
external code review of the hot-swap-binary work and verified/fixed
one at a time:

- **`deploy.go`** — the Windows rename-aside replace could leave the
  target binary missing if the second rename failed after the first
  succeeded. It now rolls back to the original binary on that failure,
  uses a unique per-deploy temp path instead of a shared one, and
  documents the accepted risk for multi-destination replacement.
- **Session updates** — renaming a session (and other narrow session
  updates) went through a whole-row `Save` that could silently
  overwrite a concurrent agent turn's token/summary/todos update.
  Replaced with column-scoped updates (`Rename`, `SetUsage`,
  `SetSummaryAndUsage`, `SetTodos`); the broad `Save` method was
  removed.
- **Transcript pagination** — reading a delegation transcript page by
  page could drop or duplicate messages, because `created_at` is
  second-granularity and a single agent turn can produce dozens of
  messages within one second, with no secondary sort key to break
  ties deterministically. Pagination now orders by `created_at DESC,
  rowid DESC` and caps how deep an offset-based page can go.
- **WebSocket replay buffer** — the reconnect replay buffer was capped
  by event count only, not by total size, and evicted by reslicing
  (O(n) copy per eviction once full). It's now a real fixed-capacity
  ring buffer with a 16 MiB total byte budget and a 1 MiB per-event
  cap.
- **Background job limit** — `MaxBackgroundJobs` was checked against
  the total number of tracked jobs, including ones that had already
  finished and were only being retained so their output could still be
  read. It's now checked against a separate active-job counter, so
  finished jobs no longer block new ones from starting.
- **Permission race** — a duplicate or concurrent response to the same
  permission request (e.g. a UI double-click) could block the second
  caller forever on an already-drained response channel. Grant/Deny
  now atomically claim the pending request before responding; a
  late/duplicate response is a no-op instead of a hang.
- **HTTP debug logging** — with debug logging enabled, the HTTP
  transport used to buffer an entire response body in memory before
  returning it to the caller, breaking streaming responses even when
  the configured log level wouldn't actually emit anything. Response
  bodies now stream through immediately; a bounded preview is
  captured on the side and logged once the body is fully read.
  Retries on 5xx are now limited to idempotent requests.
- **WSL vs. Git Bash on Windows** — resolving a shebang script's `bash`
  interpreter could pick the WSL launcher (`System32\bash.exe`) ahead
  of Git Bash, which then fails on the Windows-style script path it's
  given. The resolver now skips the WSL launcher in favor of Git Bash
  when both are present, and returns a clear error if only the WSL
  launcher is available.
- **grep fallback timeout** — the memory-bounded fallback path could
  keep reading past an expired context on a file containing one very
  large line with no newline, since cancellation was only checked
  between lines. It's now checked periodically within a single line
  read as well.
- **Session fork** — forking a session copied its history through
  several unguarded writes with most errors silently discarded; a
  failure partway through could leave (and report success for) a
  partially copied fork. Forking is now one transaction that rolls
  back entirely on any failure.
- **Config file races across processes** — two `rush run` processes
  writing the config file at the same time could silently lose one
  process's change (in-memory locking only protected against races
  within a single process). Config writes now also take a
  cross-process file lock, reusing the same locking mechanism already
  used for MCP config writes.

Two further, independent full-project reviews (#152-171) turned up
additional concurrency, security, and reliability issues, fixed and
verified one at a time in the same way as the batch above:

- **Sub-agent sessions weren't cross-process locked** (#163, critical)
  — a sub-agent runs under its own child session id, distinct from
  its parent's, so the parent's inter-process lock never covered it;
  two processes could write into the same sub-agent session
  concurrently. The lock is now taken for sub-agent sessions too.
- **Config reload held the publish lock during shell substitution**
  (#164) — resolving `${...}` values in config could block on an
  external command for up to 5 minutes while holding the same lock
  every config read/write needs; the resolve timeout is now 30s and
  the slow disk+shell-resolve phase runs without holding the publish
  lock at all. The same fix replaced environment-mutating shell-var
  push/pop with a non-destructive overlay that never touches the
  process's real environment.
- **WebSocket handler panics could crash the whole `rush web`
  process** (#154) — a panic in any per-connection message handler
  is now recovered and logged with a stack trace instead of taking
  down the server; concurrent handlers per connection are also capped.
- **`permissionService.Request` held its lock for the entire wait on
  a human response** (#156), serializing every other session's
  permission prompts behind whichever one the user hadn't answered
  yet. The lock is now released before waiting.
- **Config write lock could stall for up to 30s under load** (#153),
  an availability regression from an earlier change; write-lock
  acquisition under an already-held publish lock is now bounded to 2s
  with a stall warning instead of silently blocking full-length.
- **`projects.Register` wasn't atomic across processes** (#157) — a
  read-modify-write with no cross-process lock and a non-atomic file
  write could corrupt the projects file under concurrent `rush run`
  invocations. It now takes the same file lock as config writes and
  writes atomically.
- **WebSocket `CheckOrigin` always returned true** (#159) — a
  cross-site WebSocket hijacking (CSWSH) exposure with only the
  `SameSite` cookie attribute as a mitigation. Origin is now validated
  against the actual bound host/port; token comparisons across
  cookie/bearer/query/body now use a constant-time compare.
- **Read-heavy operations serialized behind the single SQLite
  connection** (#161) — every read and write shared one connection,
  so a slow read (deep transcript pagination, a large call-tree query)
  queued every other database operation, including message writes,
  behind it. Reads now use a separate read-only connection pool that
  runs concurrently with the writer under WAL.
- **Grant/Deny could produce contradictory granted+denied outcomes**
  (#167) — the outcome was published before the atomic claim of the
  request; both are now inverted so only the actual race winner acts.
- **Config snapshots could be mutated after publishing** (#168) —
  some production code paths mutated already-published config objects
  in place instead of going through the copy-on-write path, breaking
  the immutable-snapshot contract readers rely on.
- **Two concurrent deploys could mix binary versions** (#169) — the
  hot-swap deploy steps weren't cross-process locked; a second deploy
  starting mid-way through the first could interleave rename
  operations. Deploys now take a cross-process lock around the
  rename-aside steps. The same task added keyset-based pagination for
  live transcript reads, replacing a racy separate count+fetch.
- **Token accounting could go non-deterministic** (#165) — background
  title generation and the main turn both wrote token counts through
  the same additive/overwrite path, racing each other. Title
  generation's cost is now tracked separately from the main turn's
  token/cost snapshot.
- **`cliprovider` could pick the WSL `bash.exe` launcher** (#166) on
  Windows when it happened to sit ahead of Git Bash/MSYS on `PATH`,
  the same class of bug fixed for shell scripts earlier (#149) but
  missed in the CLI-provider binary resolver at the time.
- **HTTP debug logs could leak request/response body secrets** (#170)
  — enabling `--debug` alone used to be enough to log full LLM
  request/response bodies (system prompts, message history, tool
  output, sometimes API keys echoed in JSON fields). Body logging now
  requires a separate `RUSH_LOG_HTTP_BODIES=1` opt-in on top of
  `--debug`, and known secret-shaped JSON fields are redacted even
  when it's on.
- **npm cache key was spoofable via size+mtime** (#170) — the
  platform-package launcher's binary cache key was derived from file
  size and mtime, so an in-place binary replacement that preserved
  both (e.g. a deploy tool that restores timestamps) would keep
  serving a stale cached build. The key is now a SHA-256 hash of the
  binary's actual content.
- A further batch of lower-severity nits and nondeterminism fixes
  (#160, #162): grep's fallback reader no longer returns a spurious
  partial-match error on a clean EOF, `ListUserMessagesBySession`
  queries gained a tie-breaker for deterministic ordering, deploy's
  temp-file close errors are now checked, lock-contention errors are
  now a typed `ErrLockContended` instead of a string match, and
  several small goroutine/singleflight/atomic races were closed.
- **Orchestrator-mode prompt regression fixed before it ever shipped**
  (#171) — an uncommitted edit to the coder prompt template had
  compressed its worker-delegation rule to defer zero-trust
  verification of delegated chunks to a single pass at the end instead
  of verifying each chunk as it lands, letting mistakes compound
  across chunks; fixed to verify per chunk again, and the "under 4
  lines of prose" rule now explicitly exempts diagnosis/security-review/
  handoff turns instead of relying on an implicit carve-out in a
  different rule.

A concurrent sub-agent hang investigation (task-276) and a follow-up
independent stability review (`docs/reviews/2026-08-01-multi-agent-stability-review.md`,
range `66c4d062..e9544a8f`) turned up a chain of related lifecycle,
watchdog, and process-safety issues, all fixed and verified one at a
time in the same way as the batches above:

- **`rush run` could hang forever on a named pipe with no data
  buffered yet** — `MaybePrependStdin` used to block indefinitely on
  `io.ReadAll(os.Stdin)` for an inherited/piped stdin fd that never
  closes. It's now bounded by a grace window for the first byte, with
  the leaked reader goroutine documented as an accepted single-
  goroutine cost, not a hang.
- **...and then that fix silently dropped data a slow-but-real
  producer had already sent** — the first version raced the WHOLE
  read against the grace window, so a producer that wrote real data
  but hadn't closed the pipe within it lost everything, with the log
  actively claiming "produced no data" even though data existed.
  `stdinReadGrace` now only bounds the wait for the *first* byte; once
  a producer proves it's alive, the rest is read to EOF with no
  further timeout.
- **A wedged sub-agent delegation could freeze the whole parent
  process with zero diagnostics** — an earlier fix that exempted
  sub-agent delegations from the stream watchdog's tool-execution cap
  removed the cap entirely instead of raising it, so a hung child
  could block the parent's turn forever with no error, no finish
  part, and no goroutine dump. The watchdog now applies one generous,
  always-finite cap (`toolExecutionMaxDefault`, 45m) to every tool,
  including delegations — never no cap at all — and captures a full
  goroutine dump to disk the moment it actually fires, so the next
  occurrence is diagnosable without a live debugger attach (which, on
  a stripped release binary, can only kill the process, not inspect
  it).
- **`rush sessions kill`/`ReadLockPID` couldn't identify a live
  Windows holder** — `LockFileEx`'s mandatory whole-file lock made a
  plain file read of the lock file fail for any genuinely live
  holder, so `sessions kill` on a live session used to see PID 0. A
  never-locked `.pid` sidecar file, written alongside the lock, is now
  the primary read path.
- **`rush -v` printed the same version string for every build** —
  `Commit`/`BuildTime` are now stamped into every build path
  (local dev build, goreleaser, the npm publish workflow) via
  `-ldflags`, so a deployed binary's provenance is identifiable.
- **`Run()`'s DB preamble had no timeout** — `sessions.Get`/
  `getSessionMessages`/`createUserMessage` ran on an unbounded context
  before the stream watchdog even starts, so a wedged single-writer
  SQLite connection (`SetMaxOpenConns(1)`) could hang a turn
  invisibly, with no watchdog running yet to catch it. Now bounded by
  a 60s timeout, injectable per-agent for tests instead of a shared
  package var.
- **`Run()` deadlocked on its own session lock while draining a
  queued message** — three call sites recursed into `a.Run(...)` from
  inside `Run`'s own still-executing stack frame, before the prior
  turn's `defer ipcLock.Release()` had run; since the inter-process
  lock isn't reentrant even within one process, the recursive call
  collided with its own parent and failed with "session already in
  use," silently dropping the queued message. Replaced with an
  explicit turn loop that reuses one lock acquisition across every
  queued turn. A related non-atomic busy-check/registration race
  (two concurrent `Run()` calls for the same session could both
  observe "not busy") was closed with an atomic check-and-claim.
- **A hung title-generation call could hold `Run()` open forever** —
  the background title goroutine ran on the raw, unbounded context
  instead of the per-turn context the stream watchdog actually
  cancels, so a stuck title provider blocked `Run()`'s return even
  after the main turn's own watchdog and the new DB-preamble timeout
  had both already fired correctly. Title generation is now derived
  from the turn's own cancellable context plus an independent 2-minute
  backstop.
- **A cancelled turn could permanently lose a sub-agent's spend** —
  the child-to-parent cost transfer ran on the same context as the
  whole turn, so a watchdog firing or a user Ctrl-C right as the
  child finished made the transfer's `BeginTx` fail immediately;
  the failure was only logged, and a one-shot sub-agent never invoked
  again meant that spend was gone from the parent's ledger for good.
  The transfer now runs on a short, cancel-immune detached timeout.
- **`sessions kill` could kill an unrelated process on a stale PID** —
  a cleanly-released lock left its old PID sitting in the lock
  file/sidecar; if the OS later recycled that PID for an unrelated
  process, `sessions kill` would force-kill it on trust alone. Release
  now clears the PID metadata while it still holds the lock, and
  `sessions kill` probes for a real OS-level lock before ever touching
  a PID — only a genuine contention error triggers the kill.
- **A parent delegation could be cancelled before its child had a
  chance to clean up** — after the tool-cap unification above, both
  the parent's wait and the child's own turn shared the identical
  cap, but the parent's clock starts counting earlier (from the
  moment it decides to delegate) than the child's own watchdog (which
  starts once the delegation is actually executing) — so the parent
  always won the race and force-cancelled the whole delegation before
  the child could persist its finish part, cost transfer, or
  diagnostics. The parent's wait now gets an additional 90s cleanup
  grace on top of the shared cap, so the child always gets a chance
  to unwind on its own terms first.
- **`--timeout-hard-cap` wasn't hard without
  `--timeout-extends-on-progress`** — the wall-clock hard-cap check
  outside of tool execution only ran inside the progress-extension
  branch; with that flag off (the default) and a provider that kept
  the stream alive with regular activity, an explicit hard cap was
  silently ignored. The check is now unconditional.

A third independent review pass over this same batch found five more,
lower-severity issues, closed the same way:

- **The watchdog's own goroutine-dump-on-fire could re-introduce the
  hang it was added to diagnose** — `onFire` wrote the diagnostic dump
  to disk synchronously before `cancel()` ran, so a hung/slow disk
  write could itself block cancellation indefinitely. The fire cause
  is now recorded first, then the (unawaited) dump write runs on its
  own goroutine, off the critical path to `cancel()`. A follow-up
  review found that fix went too far: dispatching the *entire* dump
  (including `runtime.Stack`'s goroutine-stack capture, not just the
  disk write) to an async goroutine meant the capture itself could
  race a fast unwind and record post-cancellation state instead of
  the actual hang, or never run at all if the process exited first.
  The stack capture (fast, no I/O, cannot block on disk) now runs
  synchronously in `onFire`, at the moment the hang is detected;
  only the write to disk is dispatched asynchronously.
- **Five more CLI commands ignored a configured data directory** —
  `sessions list`'s status column, `sessions reap`, `sessions watch`,
  `sessions why`, and `queue` all independently hardcoded or
  preferred a raw `--data-dir` flag over the resolved config, the
  same class of bug already fixed for `sessions kill`/`reset --force`/
  `sessions locks`. All five now use the same resolved data directory.
- **A failed lock cleanup in `sessions locks`' auto-delete path
  vanished silently** — when a lock was proven to belong to a dead
  holder but the subsequent file removal itself failed (e.g. a
  lingering open handle on Windows), the entry disappeared from the
  listing with no warning either way. It now surfaces a warning and
  falls through to the normal display path instead of silently
  dropping the entry. A follow-up review caught an over-correction in
  that same fix: it treated *every* removal failure as worth warning
  about, including the file already being gone (`fs.ErrNotExist`) —
  which happens routinely when a concurrent `sessions reap`/`kill`/
  `reset --force`, or another parallel `sessions locks` invocation,
  wins the race to delete the same stale lock first. That specific
  case is the removal's goal already being met by someone else, not a
  failure, so it's now reported as the normal success message (no
  warning, no phantom row for the vanished file) — the warning and
  display fallback are reserved for genuine removal errors.
- **A reused PID could pin a session's liveness indicator "alive"
  forever** — `InspectSessionLock`'s fallback to real process liveness
  when the heartbeat mtime looks stale (see above) had no upper bound
  on how stale the mtime could be before that fallback stopped being
  trusted; a sufficiently old lock whose PID got recycled by the OS
  for an unrelated process would read as live indefinitely. The
  fallback is now bounded to 60 minutes. A follow-up review pass found
  two more independent copies of the exact same unbounded check —
  `sessions watch`'s end-of-session detection and `sessions list`'s
  STATUS column — that hadn't received the bound; `sessions watch` now
  delegates to the same, now-bounded `InspectSessionLock` instead of
  re-implementing the check, and `sessions list` applies the same
  60-minute bound (exported as `session.MaxPidFallbackAge`)
  independently, since its "trust a confirmed-alive PID unconditionally"
  shape isn't a drop-in match for `InspectSessionLock`'s. A further review
  pass found a FOURTH independent copy in `sessions why`'s status explainer
  (the very command meant to diagnose this verdict) with the same unbounded
  trust; it now applies the same `session.MaxPidFallbackAge` bound too. All
  four known copies of this check are now bounded. A later pass found the
  fourth copy's fix left the printed *reason text* factually wrong in
  exactly the PID-reuse case it targets — it said the recorded PID "is not
  alive" when that PID was, in fact, genuinely alive (just untrusted due to
  lock age). `sessions why` now gives that case its own accurate wording
  ("no longer trustworthy — likely OS PID reuse") instead of reusing the
  genuinely-dead-PID phrasing. Two more review passes caught the reason
  text still wrong on both sides of that fix: the age-bound branch printed
  "likely OS PID reuse" unconditionally, even for the dominant case of a
  lock whose recorded PID is genuinely dead (now checks `IsProcessAlive`
  and only claims reuse when the PID is actually confirmed alive, and now
  also prints the lock's real age alongside the bound threshold), and the
  separate unreadable-PID case (`pid <= 0`, normal on Windows, but here
  combined with a stale heartbeat) claimed a fictional "holder PID 0 is
  not alive" — it now cites the real evidence, a stale heartbeat, instead
  of a PID that was never actually read.
- De-duplicated four independently-hardcoded copies of the
  "Stream stalled" finish-title string in internal/agent (the retry
  logic's own constant, the watchdog's actual production value, and
  two tests) into one source of truth, so a future reword of either
  side can no longer silently break transparent stall-retry matching
  without a test catching it. A fifth, cross-language copy in the web
  UI (web/src/components/Message.tsx — TypeScript can't import the Go
  constant) is intentionally not merged; it's kept in sync by a
  comment cross-reference on both sides rather than wiring the
  literal through the WS/JSON protocol (LOW severity).
- Two classes of this batch's own regression tests were themselves flaky
  under `-race` load; both are now deterministic. The PID-fallback
  boundary tests across `sessions_list_test.go`, `sessions_watch_test.go`,
  `sessions_why_test.go`, and `internal/session/lock_test.go` used a
  1-second timing margin around `MaxPidFallbackAge` that was too tight
  under load; widened to 2 minutes. A separate test in
  `internal/agent/stream_watchdog_test.go` — whose 100ms timing budget went
  stale once its diagnostic capture became synchronous by design — now
  uses a direct file-presence check instead of a timing budget that a
  widened timeout would have made unable to catch its own regression. Also
  fixed a read-before-write race in that same watchdog test (polled for a
  dump file's existence rather than its content, occasionally reading it
  mid-write) and made a probabilistic ENOENT-race regression test
  deterministic via a test-only hook instead of a goroutine race that could
  pass even against a reverted fix.

A further round closed the last P0-4 (summarization/compaction) gaps and
finished the mailbox migration:

- **Summarization could run outside session ownership and delete live
  history out from under a turn (P0-4)** — `/compact` and the auto-summarize
  path used a separate, non-atomic busy check from the mailbox's own
  turn-ownership tracking, so a manual `/compact` and an ordinary turn
  could both believe they owned a session at once; whichever finished last
  won, and the loser's in-flight message could vanish along with the
  history it referenced. Summarization now goes through the same atomic
  `beginCompact` check-and-reserve the mailbox uses for turns, so a
  compaction and a turn (or two compactions) are now mutually exclusive by
  construction, not by convention.
- **Cross-process compaction was not serialized — only in-process (P0-4
  residual)** — `beginCompact` closed the gap above only within one
  process; a *second* `rush` process holding the OS session lock could
  still start an ordinary turn while this process's compaction was mid
  commit, the same "one process deletes history the other believes is
  live" shape P0-4 itself was about. In a fork whose whole point is N
  concurrent `rush run` processes over one tree, that's a routine
  configuration, not a theoretical one. Manual `/compact` now takes the
  same OS session lock a turn takes, held across the compaction's commit
  phase, and releases it explicitly (not via `defer`) before handing off
  to a drained queued message's own turn — a `defer` would have kept the
  lock held across that hand-off and made the process reject itself with
  "already in use," naming its own PID. Inline compaction (already running
  under a turn's own lock) is unaffected; it never takes a second lock.
  Also hardened compaction's commit phase (writing the summary pointer,
  then deleting the old messages) against cancellation the same way an
  earlier fix protected the "silent" auto-compact path: a cancellation
  landing between the two writes used to leave a session's history in a
  half-updated, unrecoverable state. An independent review of this fix
  found two further issues, both closed the same day: manual `/compact`
  released its OS lock only AFTER flipping the session's in-memory state
  to idle, reopening the exact same-process "not busy, but the OS lock
  says otherwise" race `mbReleasing` exists to prevent — the lock is now
  released first; and manual `/compact` acquired its own OS lock but never
  wired it into the session's activity-notify chain, so a long-running
  manual compaction's lock file went heartbeat-stale for the same reason
  the next bullet describes for the mid-turn path — it's wired now, the
  same way.
- **Silent (background) compaction was invisible to the session
  heartbeat** — a healthy mid-turn compaction stream produced no heartbeat
  activity, so `sessions watch`/`locks` could report a working session as
  heartbeat-stale mid-compaction. It's wired the same way an ordinary
  turn's stream is. Separately, the same stream also now bumps the stream
  watchdog, so a compaction that legitimately runs longer than the idle-
  stall timeout is not killed as "no provider activity" with a misleading
  warning and a goroutine dump; this half only applies to compaction
  running under a turn's own watchdog (mid-turn/silent) — manual
  `/compact` has no watchdog of its own to bump, only the unrelated
  10-minute overall timeout it already had.
- **An interrupt landing between two turns of the same session could still
  kill the whole dispatcher instead of just redirecting it, and could
  drop the message that triggered the interrupt** — a follow-up to the
  DB-preamble interrupt fix above: the same race existed in the narrower
  window between one turn ending and the next one starting. Fixed the
  same way, plus a fix for the message itself: the pre-empted call that
  the interrupt was replacing used to be silently discarded rather than
  requeued, which meant that with two or more messages queued, exactly
  one was destroyed — chosen by scheduling luck, not deterministically.
  It's now put back at the front of the queue instead.
- **Finished migrating `QueueMessage` off the legacy message queue** — the
  fire-and-forget queue primitive was the last production caller of the
  pre-mailbox `messageQueue` structure, left behind when the rest of the
  mailbox migration landed. It now queues through the mailbox directly,
  and the legacy structure and its remaining call sites are gone. As part
  of this, `PrepareStep`'s behavior of folding an already-queued message
  into the current turn's own prompt was deliberately removed: in the
  mailbox model, each queued call is its own turn with its own DB record
  and its own response, and silently merging two into one broke that
  contract.
- **The release gate's own regression coverage had two real gaps, found
  by an independent audit that first gave a passing verdict its own
  evidence didn't support** — every gate scenario now has a test that is
  independently revert-checked (fix removed → test fails on its own
  assertion → fix restored), not just green. That process caught: a
  test asserting the wrong thing for one scenario had been accepted on
  the strength of an unexplained panic rather than a clean failure (now
  has a reasoned explanation tying the panic to the exact removed guard);
  and one scenario — an abandoned title-generation goroutine's deferred
  fallback overwriting a *later* turn's real title with the placeholder
  name — had no regression test at all despite being a named, previously
  fixed defect. It does now.
- **A final `@oh` review of the release gate suite above found six more
  real defects, then a second confirming pass over that very fix found
  six new ones introduced by it** — both rounds independently
  re-verified (never accepted from review narration alone), each with a
  genuine FAIL→PASS revert-check.
  - First pass: a durable queue entry whose lease mismatched the one
    the pump actually scanned could be terminal-failed by ID instead of
    released back to pending, deleting the wrong entry; a busy OS
    session lock (routine, expected contention from another live
    process) was counted as a failed attempt toward the queue's max-
    retries budget exactly like a genuine failure, exhausting healthy
    entries under load; `SessionLock.Release()`'s metadata cleanup ran
    fully async with zero wait, so a caller could observe a stale PID
    immediately after `Release()` returned; and a couple of doc-comment
    corrections.
  - Second pass, on the fix for the first: `App.New()` started the run
    queue pump *before* `InitCoderAgent` assigned the coordinator it
    reads — an unsynchronized data race per Go's memory model between
    `InitCoderAgent`'s write and the pump's read, plus a work-loss path
    where an already-started pump could dead-letter a durable entry if
    `InitCoderAgent` failed or the app was unconfigured. Fixed by moving
    pump construction and `Start()` to strictly after `InitCoderAgent`
    succeeds, removing the hazard architecturally. Also: a new pump-
    wiring regression test called real `app.New()` without environment
    isolation (the same failure class that once hung a stress run for
    9+ minutes, see `internal/cmd/providers_test.go`); removing the
    lease-attempt increment from the lease query (part of the first
    pass's fix) left crashed/hung executions never accumulating
    attempts via lease-expiry cleanup, risking an endless retry loop
    for a poison entry; and further doc-comment corrections, including
    an assertion in `docs/release_gate_summary.md` that a regression
    was "confirmed pre-existing via `git stash`" — a check performed
    after the regressing commits were already on `main`, with nothing
    left to stash back to; it is a genuine regression, and was already
    fixed by the bounded synchronous wait mentioned above.
  - Third pass, on `run_queue_pump.go`'s original design (in-range for
    the durable run queue itself, not either fix round): the pump
    could Ack (delete) a durable queue entry whose call was never
    actually executed, only appended to another live, in-process
    owner's mailbox queue — reachable both from a self-inflicted
    lease-expiry race (the 30-second lease TTL is far shorter than a
    real LLM turn, and execution never renewed its lease) and from two
    distinct queued entries for one session being dispatched back to
    back within the same pump tick. Either way, the second dispatch's
    call was silently duplicated into the first's mailbox (re-run when
    the owner drained its queue) while its own durable row was wrongly
    deleted — data loss if the process crashed before the mailbox
    drained, a duplicate turn if it didn't. Fixed by tracking which
    sessions this pump instance currently has an execution in flight
    for and refusing to lease further entries for the same session
    until it clears (closes the self-inflicted paths at the source),
    plus a distinct signal for the residual case of a genuinely
    external owner (neither Acked, since the work has not run, nor
    immediately retried, since that would itself append another
    duplicate — left leased for the existing lease-expiry mechanism to
    naturally recover).
  - Fourth pass: the third pass's fix above closed concurrent
    duplicate dispatch but not SEQUENTIAL duplicate dispatch — the
    30-second lease TTL is far shorter than a real LLM turn, and
    without lease renewal the durable row could flip back to pending
    while its own execution was still genuinely running; the eventual
    Ack would then silently fail to match, leaving the row for a LATER
    tick to lease and dispatch again — a real duplicate execution on
    any turn longer than 30 seconds, i.e. most real turns. Fixed with
    a lease-renewal loop that keeps a long-running execution's row
    genuinely leased for its whole duration. Separately, the third
    pass's "leave it leased, do nothing" handling for the
    externally-owned case relied on the same lease-expiry cleanup that
    unconditionally counts an attempt on every recovery — a session
    that stayed externally busy for a few minutes would have its
    accepted, never-actually-failed work silently deleted once
    attempts exhausted, the same class of bug the OS-lock-contention
    case was already protected against. Fixed via an immediate,
    no-attempt-penalty release paired with a local backoff deadline
    that prevents this pump instance specifically from re-attempting
    the same session too soon (a different process remains free to try
    immediately). A first attempt at this second fix (a single lease
    renewal call) was tried and found not to work by an isolated debug
    test before the real fix was written — the renewal landed
    essentially the same deadline leasing had already set, since it
    fired almost instantly after the lease was taken.
  - Fifth pass: the four queries that finalize a leased entry (ack,
    two kinds of release-back-to-pending, terminal-fail) matched on
    the row's id and leased status alone, with no check that the
    caller was still the current lease holder. If an executor ever
    lost its lease to a recovery (rare after the fourth pass's
    renewal loop, but not impossible under a severe scheduling stall),
    its late-arriving Ack or Nack could silently mutate or delete a
    row a different, currently-live executor now owned. All four
    queries are now scoped to the current lease holder. The reviewer's
    own verdict on this pass: the underlying concurrency design is
    settled, and this was the last targeted follow-up needed rather
    than the start of another full review round.

- **Three more bugs found while pushing and monitoring CI for the whole
  #337-349 batch** (this range had accumulated 39 unpushed commits and
  had not been tested against a genuinely clean environment until this
  push) — all confirmed as real via independent local reproduction and
  revert-checks, not accepted from CI's report alone:
  - **A forced app shutdown could leak the database file handle for the
    rest of a test binary's process lifetime**, breaking Go's own
    `t.TempDir()` cleanup on Windows ("process cannot access the
    file"). `App.Shutdown()`'s forced-shutdown path (taken whenever an
    agent doesn't finish within its grace period) intentionally skips
    releasing the database — correct for a real CLI/server process,
    which exits immediately after and lets the OS reclaim the handle —
    but a test helper that paired one `db.Connect` with exactly one
    `db.Release` assumed Shutdown always contributed its own share of
    releases, silently leaving the pool's reference count above zero
    forever whenever a test deliberately exercised the forced-shutdown
    path. Fixed with a new `db.ReleaseAll(dataDir)`, scoped to just
    that data directory's own pool entry, that guarantees teardown
    regardless of how many releases Shutdown's own policy performed.
  - **Building the coordinator could fail with "coder agent
    configuration is missing"/"coder agent not configured" outside of
    a normal config-file load** — `config.Load`/reload only populate
    the coder agent's config entry when a provider was already
    configured at the exact moment they run; several tests (and,
    structurally, any future caller) that configure a provider
    programmatically after an initial empty load bypass that entirely.
    Fixed by making both `App.InitCoderAgent` and
    `agent.NewCoordinator` self-heal: derive the missing config on the
    spot (a cheap, side-effect-free operation) instead of requiring
    every caller to remember to trigger it themselves.
  - **The same class of gap for the selected large/small model** — a
    handful of coordinator tests configured a provider but never
    selected a model to use with it, silently relying on the same
    leftover state the previous bug's fix now closes off. Unlike the
    coder-agent-config gap, this one is not self-healed in production
    code (correctly re-deriving a default model selection needs a
    resolved provider registry, which risks reintroducing a
    network-dependent test hang this codebase has hit before) — fixed
    by giving each affected test its own explicit model selection
    instead.

- **Eleven findings from a full-project `rush run --role reviewer`
  audit, closed and independently verified** (2 security blockers, 4
  bugs, 5 code-smell items with a code change — see the full report
  referenced from `docs/checkpoints/2026-08-11-0805.md`). Unlike prior
  rounds, this sweep covered the whole repository rather than one
  subsystem: `internal/cmd`, `internal/agent/cliprovider`,
  `internal/agent/tools`, `internal/config`, `internal/permission`,
  `internal/csync`. Every finding was independently re-verified by
  reading the actual code before acting, and every code change shipped
  with a genuine revert-check (temporarily undone, confirmed the exact
  predicted failure, restored, confirmed pass again).
  - **`env`/`nice`/`nohup`/`time`/`timeout` in the bash tool's
    "safe read-only command" allowlist let a model bypass the
    permission prompt entirely** — these are command-wrappers that run
    an arbitrary subcommand (`env rm -rf ./secrets`, `timeout 10
    ./exfil.sh`), so matching the allowlist's prefix check skipped
    `permissions.Request()` completely, defeating `--restrict-run`'s
    deny-by-default guarantee. All five removed from the allowlist
    (`printenv` kept as the safe alternative to bare `env`).
  - **The same wrapper class bypassed the sub-agent recursion guard
    too** — `agentguard`'s strip-list only recognized `exec`/`command`/
    `time`/`nohup`, so `env claude -p ...`, `nice claude ...`, `timeout
    30 claude ...` all slipped past it, and combined with the bug
    above, a single command could bypass both the permission system
    and the recursion guard at once. Added flag-aware stripping for
    `env`/`nice`/`timeout` (they take their own leading flags/
    positional args before the real command, unlike the pre-existing
    bare wrappers).
  - **A failed or cancelled `download` left a truncated file on disk
    with no size limit on the transfer** — a later `view`/`edit` would
    read the partial file as valid content. Rewritten to a temp-file +
    atomic-rename pattern (matching `fsext.AtomicWriteFile`'s existing
    convention) with a new 500 MiB cap.
  - **A CLI-provider session's MCP server registration could be
    deleted out from under a still-running concurrent session in the
    same project** — Gemini/Qwen MCP registration uses a stable
    per-project server name by design (survives process restarts), but
    the matching deregister-on-exit deleted that entry unconditionally
    even if a newer session had since overwritten it with its own
    endpoint. Deregister now compares against the exact value its own
    registration wrote and is a no-op if someone else now owns the
    entry.
  - **Two attachments with the same filename in one CLI-provider
    request silently overwrote each other on disk** — both message
    entries kept pointing at the same, last-write-wins file. Fixed with
    filename-collision disambiguation, mirroring the pattern the web
    server's own attachment handler already uses.
  - **The max-cost/max-tokens budget-cap enforcement's turn-stopping
    mechanism turned out to rely on an undocumented, easy-to-break
    invariant** — investigated as a possible fifth bug, but confirmed
    (independently, twice) to be structurally unreachable in the
    current code: the internal map that budget-cap's abort path reads
    a cancel function from is populated before the turn's first
    callback can ever run, and nothing in the codebase ever clears that
    entry early. Not an active bug today, so left as a pinned regression
    test plus a warning comment instead of restructuring sensitive
    turn-lifecycle code for a scenario that can't currently occur.
  - **The Qwen CLI MCP integration used a 32-bit-entropy stable ID as
    its auth token, passed via a `?token=` URL query parameter that
    gets logged** — justified in code as "qwen doesn't support custom
    headers." Re-checked against the actually-installed qwen CLI
    package and found that claim outdated: it does support a `headers`
    field on HTTP MCP servers. Switched to `Authorization: Bearer`
    with a full 256-bit random token, matching the Claude/Gemini/Codex
    integrations.
  - **The MCP server's built-in tools (Bash/Read/Write/Glob/Grep) each
    generated two unrelated UUIDs per call** — one for internal event
    tracking, one for the permission request — leaving the permission-
    notification stream uncorrelated with the tool-event stream for
    the same call. Unified to one ID per call, matching the pattern the
    external-MCP-tool handler already used.
  - **`download`/`fetch` had no defense against a model (directly, or
    via a prompt-injected web page) requesting an internal or
    cloud-metadata URL** (e.g. `http://169.254.169.254/latest/
    meta-data/`) and leaking the response into `final_text` — the only
    prior gate was the permission system, which auto-approves in
    non-interactive `rush run`/web contexts. Added a shared SSRF guard
    that blocks loopback/private/link-local/metadata destinations at
    dial time (`net.Dialer.Control`, which sees the actually-resolved
    IP after DNS lookup but before connect — closes both the trivial
    bypass and DNS-rebinding). Callers that legitimately need loopback
    (local dev, self-hosted) inject their own client via the tools'
    existing `*http.Client` constructor parameter.
  - Two smaller findings closed without behavior risk: a `go vet`
    warning about a value-receiver copying a mutex was investigated and
    left as the existing, correctly-suppressed no-op (the "fix" would
    have broken config JSON-schema generation or config-file loading,
    depending on which half of the conflict was resolved); an unchecked
    type assertion on a `singleflight` result was switched to the
    comma-ok form; a copilot token refresh gained a 30s timeout; three
    session-permission CRUD methods (currently unused in production,
    reserved for a future web UI surface) now accept a caller `ctx`
    instead of using `context.Background()` internally; a misplaced
    doc-comment was moved to the function it actually documents.

  **Closing independent review of this round's own fixes** found 1
  blocker and 3 bugs in the fixes above — all confirmed and closed the
  same way, with revert-checks on the security-relevant ones:
  - **The new SSRF guard only covered 2 of 6 model-facing HTTP tools**
    — `download`/`fetch` were guarded, but `agentic_fetch`, `web_fetch`
    (which `agentic_fetch`'s spawned sub-agent gets, and which
    explicitly skips the permission system), `web_search`, and
    `sourcegraph` each built their own unguarded client, leaving the
    exact exfiltration path the guard was supposed to close still open
    through those four. All six now share the same guarded default.
  - **The guard's documented "inject a client with `allowPrivate=true`"
    escape hatch didn't actually exist anywhere in production wiring**
    — a routine local-dev fetch (`http://localhost:3000/...`) was
    permanently blocked with no way to restore it short of a code
    change. Added an off-by-default `allow_private_network_fetch`
    config option, wired through to all six guarded tools.
  - **The guard was bypassable via `HTTP_PROXY`/`HTTPS_PROXY`** — the
    dial-time check only ever sees the address actually dialed, which
    is the proxy's address when one is configured, not the request's
    real target. The guarded transport now disables proxying outright.
  - **The attachment-filename-collision fix (above) still collided on
    a mixed input set** — e.g. `["image.png", "image.png",
    "image-1.png"]`: the second `image.png` disambiguated to
    `image-1.png`, and the third, literal `image-1.png` then silently
    reused that generated name. Fixed by tracking the set of names
    actually written instead of a per-original-name counter.
  - **The download atomic-write fix (above) silently dropped the
    downloaded file's permissions** from world-readable to owner-only
    (`os.CreateTemp`'s default `0600`, never chmod'd back up before the
    rename, unlike the codebase's own `fsext.AtomicWriteFile`
    convention it was modeled on).

- **Fifteen findings from a 2026-08-11 release-readiness concurrency
  review, closed and independently verified** (3 release blockers, 5
  high-priority, 6 medium-priority items — see
  `docs/reviews/2026-08-11-release-readiness-concurrency-and-code-review.md`).
  This round covers the core session/mailbox/durable-queue lifecycle,
  the same subsystem seven prior review passes already hardened; every
  fix was implemented via a delegated `/rush` session and then
  independently re-verified: the diff read line by line, every new test
  checked for whether it would actually fail without the fix, and every
  safety/concurrency-critical fix personally revert-checked (temporarily
  undone, confirmed the exact predicted failure, restored, confirmed
  green again). That verification pass itself found real, non-trivial
  bugs in most of the delegated diffs, listed inline below.
  - **A durable-queue call could execute twice after handoff into a
    busy session's mailbox** — `mailbox.submit` unconditionally
    appended durable-originated calls to the in-memory queue even
    though the durable row itself is already the retry path, so the
    live owner would run the in-memory copy and the pump would
    independently re-lease and run the same durable row after backoff.
    Calls originating from the durable queue no longer get a second,
    redundant in-memory queue entry.
  - **An "accepted" call — an idle interrupt, or orphaned mailbox work
    found during release — could still be lost before its durable-queue
    commit, with no error surfacing anywhere** — `startDetachedRun` and
    `restartOrphaned` both silently swallowed enqueue failures. Both
    now propagate the failure to their own callers instead of
    discarding it; for the cross-process interrupt-inject path, a
    failed durable enqueue now recreates the `pending_injects` row for
    a future retry instead of vanishing.
  - **`RunQueuePump.Stop()` returned before in-flight `executeEntry`
    workers finished**, so a shutdown could drop an Ack/Nack that was
    mid-flight and cause the same durable row to be re-run after
    restart. `Stop()` now joins a `workerWg` covering every dispatched
    worker (5-second grace, matching `CancelAll`'s own pattern), guarded
    by an admission gate that closes the same class of "check state,
    then start work" race the fix itself was designed to prevent —
    found in the delegated diff's own new code during verification.
  - **`CancelAll`'s shutdown join didn't cover manual `Summarize()`
    calls or the title-generation goroutine** — a summarize/title
    operation still writing to the database after `CancelAll` returned
    could race a subsequent session teardown. Both are now covered by
    the same `runWg` `CancelAll` already joins, via an admission gate
    (`admitMu`/`shuttingDown`) that closes the same
    check-then-`Add` race pattern found in the RunQueuePump fix above —
    Go's `sync.WaitGroup` forbids a concurrent `Add(positive)` starting
    from zero unless it happens-before a concurrent `Wait()`, and an
    unsynchronized "check shutting-down, then Add" is a real bug even
    when each half looks correct in isolation.
  - **A lease lost during execution didn't stop the in-flight,
    non-idempotent LLM/tool work still running under it** — the
    execution context stayed uncancellable after a lease-loss recovery
    reassigned the row to a new owner, so both the original executor
    and the new owner could run the same call concurrently. The
    execution context is now cancelled the moment lease loss is
    detected, and outcome writes (Ack/Nack/terminal-fail) are skipped
    once that happens rather than racing the new owner's own writes.
  - **FIFO ordering for the durable queue was undefined for two calls
    enqueued within the same second** — `created_at` alone isn't a
    stable sort key at one-second resolution. Added an implicit-`rowid`
    tie-breaker to both scan queries (SQLite gives every non-`WITHOUT
    ROWID` table a free, already-monotonic rowid — no migration
    needed).
  - **The pump's fan-out after a backlog or restart was unbounded** — a
    large recovery backlog could dispatch unlimited concurrent
    `executeEntry` workers at once. Added a bounded semaphore (10
    concurrent by default, test-overridable) that every dispatch path
    acquires before leasing and releases on every exit.
  - **Async lock-metadata cleanup could clobber a new, live owner's
    PID** — after a slow `Release()`'s best-effort cleanup goroutine
    outlives its bound, it could still overwrite a `.pid` sidecar a
    brand-new owner had since written. Added a generation-versioned
    `.gen` sidecar; cleanup now compares its recorded generation
    against the current one and skips only on a genuine positive
    mismatch (a missing or unreadable sidecar still falls back to the
    pre-fix unconditional cleanup, closing a regression the delegated
    diff's first attempt introduced by treating "missing" the same as
    "mismatched").
  - **The durable-queue "idempotency key" was actually a fresh unique ID
    on every call, not a stable retry key** — generated from
    `time.Now().UnixNano()` each time, so a caller-level retry of the
    same logical request produced a different key and could create a
    second durable row for what should be one logical call. Added a
    `LogicalCallID`, generated once when a call is first constructed,
    and keyed idempotency off `SessionID+LogicalCallID` (falling back to
    the old timestamp behavior only if unexpectedly empty). Making the
    key genuinely stable exposed a second gap the naive fix alone would
    have made worse — `EnqueueRunQueueEntry`'s insert had no conflict
    handling at all — closed with `ON CONFLICT(id) DO NOTHING`.
  - **`agentguard`'s sub-agent recursion guard was still bypassable via
    `env -S`/`--split-string`**, and `CheckWindowSafety` used a
    separately-maintained wrapper list that had already drifted from
    the main guard once before (the exact class of bug a prior review
    round's `env`/`nice`/`timeout` fix closed for the primary path
    only). `env -S`/`--split-string` payloads are now recursively
    re-checked, and both guards now share one `resolveCommandHead`
    helper so the wrapper-stripping logic can't diverge between them
    again.
  - **The Codex CLI provider's MCP token was still exposed via a
    `?token=` URL query parameter**, visible in argv/
    `/proc/<pid>/cmdline` — unlike the Gemini/Qwen providers, which
    already use an `Authorization` header. Moved to an environment
    variable referenced through Codex's own `bearer_token_env_var`
    config key, verified against the actually-installed `codex` CLI
    (not assumed from documentation) to confirm the mechanism works and
    is never persisted to `~/.codex/config.toml`.
  - **The SSRF guard's range denylist missed several IANA
    special-purpose ranges** not covered by any Go stdlib `net.IP`
    method — CGNAT (`100.64.0.0/10`), the IPv4 benchmark range
    (`198.18.0.0/15`), the TEST-NET-1/2/3 documentation ranges, the
    reserved `240.0.0.0/4` block, and IPv6 equivalents (discard-only,
    documentation, ORCHIDv2). Added a static CIDR table checked
    alongside the existing loopback/private/link-local/unspecified
    checks, with 42 new boundary test cases.
  - **The mailbox's ownership-exit finalizer could hand a newer era's
    queued work to the wrong recovery path** — releasing ownership and
    draining any leftover queued work were two separate lock
    acquisitions, leaving a window where a new owner's own queued call
    could be misrouted into detached recovery instead of staying for
    that owner's normal end-of-turn drain. Both steps are now one
    atomic critical section.
  - **Recovery-subsystem architecture cleanup**: audited which of six
    documented complexity/drift smells in the run-queue pump were
    already resolved by the fixes above before touching anything (per
    this task's own explicit instruction) — three were already closed,
    two were genuinely still open and fixed (an unused
    `RunQueuePumpConfig.DataDirectory` field removed; the pump's
    "executed entry successfully" log no longer prints unconditionally
    when the following database ack actually failed), and two are
    flagged as legitimate future-scoped work rather than force-fit into
    a mechanical cleanup pass (the file's line growth from this round's
    own correctness fixes; a `terminal_failure` column that is
    currently dead state, since a terminal failure deletes the row
    rather than ever setting the flag — closing it properly needs a
    schema migration on the exactly-once durable queue this whole round
    protects, judged out of scope here).

- **Two UI bugs: switching a model in the web UI mutated global config
  instead of the current session** — reported live during this round.
  Model resolution is now a pure function of `(session, config)` instead
  of mutating shared agent state, and the global-default vs
  session-scoped model APIs are split, fixing the server call sites that
  were building a system prompt from the wrong (globally-mutated) model.

  **Closing independent review of this round's own fixes** (an
  18-commit range) returned a NO-GO with one build failure and one new
  release blocker, both introduced within the round itself — all
  confirmed and closed the same way, with a personal revert-check on
  the concurrency-relevant one:
  - **`internal/agent` did not compile under `go test`** — the
    recovery-subsystem cleanup above (the `DataDirectory` field
    removal) was verified only against the two `internal/session`
    packages it touched, missing ten more call sites in
    `internal/agent` that also set the now-removed field. Every
    regression test this round produced under `internal/agent` had
    not actually run since that commit landed. All ten sites fixed;
    `go vet ./...` is now clean except one pre-existing, unrelated
    finding.
  - **A long-running turn could be executed twice** — the very
    `RunQueuePump.Stop()` fix earlier in this round introduced a
    single 30-second database-write context created before
    `Coordinator.Run` was even called, then reused for both lease
    renewal and the final outcome write. Any real turn longer than 30
    seconds silently stopped renewing its lease and then failed to
    Ack/Nack with "context deadline exceeded," leaving the row
    leased/pending for the next tick to re-lease and re-execute —
    exactly the duplicate-execution bug that `Stop()` fix exists to
    prevent, reintroduced by the same commit. Fixed by minting a fresh
    write budget per individual database call instead of one shared,
    prematurely-started context; the renewal loop's own lifecycle is
    now independent of that budget so it survives the whole turn.
  - **A second instance of the mailbox era-boundary reordering gap**
    (see the mailbox finalizer fix above) was found in a code path the
    original fix didn't touch — the manual-compaction success path
    that hands off to the next queued summarize request. Closed with
    the same atomic-pop pattern as the original fix.

- **Ten findings from a 2026-08-12 post-fix release-readiness review,
  closed and independently verified** (3 release blockers, 4
  high-priority, 1 security, 1 medium-priority, plus one
  cross-branch coordination gap found during the merge itself — see
  `docs/reviews/2026-08-12-post-fix-release-readiness-review.md`).
  Genuinely-independent work was parallelized across git worktrees
  (one branch per finding) and merged back after each was individually
  verified; every fix went through the same zero-trust process as the
  prior round — diff read line by line, every new test checked for
  whether it would actually fail without the fix, every
  concurrency/correctness-critical fix personally revert-checked. Real,
  non-trivial defects were found and closed in the majority of the
  delegated diffs, listed inline below.
  - **A durable-queue call's idempotency key (`LogicalCallID`) was
    dropped across the JSON round-trip through the run-queue's
    serialized call data**, so a retried call landed with a fresh,
    non-idempotent key and could execute twice. `LogicalCallID` is now
    carried through both directions of the conversion
    (`ToSessionAgentCallData`/`FromSessionAgentCallData`) and
    `RebuildSessionAgentCall`.
  - **Cross-process interrupt handoff deleted the pending-inject row
    before the durable enqueue that was supposed to replace it was
    guaranteed to succeed** — any fallible step in between (message
    read, model/call build, marshal, enqueue) lost the event
    permanently once the row was gone, since the next tick had nothing
    left to retry. `ConsumeInterruptInjectAndEnqueue` now deletes and
    durably enqueues in one SQL transaction; failures before that point
    simply leave the still-undeleted row for the next tick to retry
    naturally. Verification found the delegated diff's own atomic
    query re-selected "the oldest pending row" instead of matching the
    specific row a caller had peeked and built its call data from — a
    session with more than one queued interrupt could silently consume
    and lose the wrong one if a concurrent path deleted the peeked row
    in between. Fixed to match on the exact peeked row ID, returning a
    safe no-op instead of a wrong-row substitution when it has vanished.
  - **The checkpoint writer's cancellation was dead code** — the
    per-cycle write context's cancel function was only ever invoked via
    `defer` inside its own goroutine (a no-op against a hang, since the
    goroutine that would call it is the one that's stuck), so
    `stopCheckpoint` had no real way to interrupt a blocked write and
    fell through to a 5-second grace wait every time regardless of
    whether the write was actually still live. The cancel function is
    now stored on the enclosing scope and called directly from
    `stopCheckpoint`, immediately unblocking a genuinely stuck write.
  - **Orphan recovery still had a runnerless fallback** — when durable
    enqueue failed for a call recovered during ownership handoff, the
    old fallback queued it into a (potentially idle) in-memory mailbox
    that only a future, unrelated `Run()` would ever drain; if the
    session was truly idle, the call was lost with no further trace.
    Replaced with a bounded, lifecycle-joined detached run
    (`startBoundedDetachedRun`, 30s timeout, tracked in the same
    `runWg` `CancelAll` already joins) that actually attempts the call
    in-process instead of merely queuing it, with all failures logged
    at ERROR level. Documented as a best-effort improvement, not a
    durability guarantee — a process crash during the bounded window
    still loses the call, since durable enqueue (the actual durable
    path) already failed to get there in the first place.
  - **`RunQueuePump.executeEntry`'s lease-renewal loop only reacted to
    an explicit "lease reassigned" response**, not to renewal calls
    that simply stopped succeeding (a DB outage or network partition) —
    execution could keep running real, non-idempotent work indefinitely
    after another process had already taken over the same lease. Added
    a fail-closed timeout: if no renewal has succeeded within a full
    lease TTL, execution now cancels itself the same way an explicit
    lease-loss response does. Documented the durable queue's
    at-least-once (not exactly-once) semantics for persistent side
    effects and the residual overlap window this timeout bounds but
    does not eliminate.
  - **The session-model cache had no invalidation path** — introduced
    as a regression by an earlier round's per-session model-isolation
    fix, a config change or credential refresh (401 retry) left
    `resolveSessionModels`'s cache silently serving a stale
    model/provider pairing. Cache keys now fold in a monotonic config
    generation counter, and both `UpdateModels` and the 401-retry path
    explicitly clear the cache.
  - **Two cross-process interrupt call sites (`requeueInterruptMessage`,
    `InterruptAndSend` without explicit overrides) still fell back to
    the shared/global model instead of a session's own persisted
    override** when handed a nil model snapshot — the exact isolation
    gap an earlier round's fix was supposed to have closed everywhere.
    `buildCall`/`runInternal` now reject a nil model snapshot outright
    instead of silently substituting shared state, and both call sites
    resolve session models first. Merging this fix's branch against
    this round's separately-landed atomic interrupt-handoff rewrite
    (above) surfaced a third call site with the identical bug,
    introduced by that same rewrite after this fix's scope had already
    been fixed — closed the same way before the merge landed.
  - **The CLI provider logged the full prompt text and raw argv at INFO
    level** by default, including any secrets embedded in a prompt.
    Both are now redacted unless
    `RUSH_CLIPROVIDER_LOG_RAW_PROMPT=1` is explicitly set for
    diagnostics.
  - **The lock-metadata generation guard's residual gap** (a narrow
    window where async cleanup can still race a brand-new owner despite
    the generation check) is now documented as an explicit accepted
    risk — citing the prior reverted architectural attempt and
    `sessions kill`'s own re-probing safety net — rather than left as
    an ambiguous "known gap" comment.
  - **`RunQueuePump.Stop()`'s wait on the main polling loop was
    unbounded** — only the worker-drain half of `Stop()` had a
    deadline; a stuck main loop could hang shutdown indefinitely.
    `Stop()` now bounds both halves to a single shared 5-second budget
    (not 5+5=10s) via one timeout context and a three-way select.

- **Nine more release blockers from the 2026-08-12 post-fix
  release-readiness follow-up review, closed, zero-trust reviewed, and
  gated on a composite 9-property release verification** (see
  `docs/reviews/2026-08-12-post-fix-release-readiness-follow-up.md` and
  `docs/reviews/2026-08-13-release-gate.md`, verdict: GO). The review
  re-audited the round above's fixes and found three of them had
  reopened the same class of problem in a new shape:
  - **A cross-process interrupt could execute twice** — the atomic
    durable-enqueue fix from the round above still let the same call
    also land in the live mailbox as an in-memory replacement, so the
    live owner would run it once and the durable queue's pump would run
    it again later. Calls originating from the durable queue no longer
    set the in-memory replacement; only the durable row is retried.
  - **A late checkpoint could publish a stale, incomplete snapshot as
    the last event a client sees, after the real terminal message had
    already been published** — the SQL update that's supposed to no-op
    once a terminal finish has landed reported success either way, so
    the service always published the checkpoint regardless. It's now
    conditioned on the DB actually having changed a row. A related data
    race — the generation counter that fences overlapping checkpoint
    writers was read and written without a lock in some places — is
    also fixed.
  - **An orphaned call whose durable re-enqueue failed had no durability
    guarantee at all** — the fallback ran the call in-process for up to
    30 seconds and then gave up with nothing left to show for it on
    crash or shutdown. It now writes to a durable retry outbox first;
    only if that also fails does it return a clear error instead of
    quietly losing the work. (Draining that outbox back into normal
    execution is tracked separately, not yet wired up — see the "known
    gaps" note below.)
  - **The new orphan-recovery fallback logged the user's full prompt at
    ERROR level** by default — the same class of leak the CLI provider
    fix above had already closed, reopened in a different function.
    Only length and a hash are logged by default now; raw prompt needs
    the same explicit `RUSH_CLIPROVIDER_LOG_RAW_PROMPT=1` opt-in.
  - **A lease-renewal stall could let an executor keep running for up to
    a full TTL past the point another pump instance could legitimately
    take over the same row** — the existing fail-closed check ran in
    the same goroutine as the renewal call itself, so a hung renewal
    delayed the check that was supposed to catch it. An independent
    watchdog now cancels execution at a fixed safety margin before
    expiry regardless of what the renewal call is doing.
  - **A 401 retry could reuse the exact same (now-invalid) provider
    client that just failed** — credentials were refreshed, but the
    retry replayed the original call object, built before the refresh.
    The call is now rebuilt with fresh models after a successful
    credential refresh, before the retry.
  - **A config reload landing mid-request could mix fields from two
    different config generations into one cache entry** — model
    config, provider config, and generation were each read separately;
    a reload between any two of those reads could produce an
    internally inconsistent cache key. All three are now read from one
    atomic snapshot. A related bug — one runtime provider-config update
    path mutated config without bumping the generation counter at all,
    silently breaking the cache's own invalidation contract — is also
    fixed.
  - **A second cross-process interrupt landing while a replacement turn
    was already running would never fire** — the interrupt watcher
    exited after handling the first interrupt instead of continuing to
    watch for the session's whole lifetime. It's now continuous, and
    its shutdown properly waits for any in-flight interrupt handling to
    finish before returning (closing a real deadlock found during this
    fix: two `defer` statements in the wrong order meant the wait could
    block forever on a goroutine nobody had told to stop).
  - Two smaller cleanups: same-second interrupt messages could process
    in an unspecified order (now tie-broken by insertion order, matching
    an existing pattern elsewhere in the codebase), and interrupt-recovery
    code paths that had become unreachable dead weight — kept alive only
    by their own tests, after the transactional rewrite above superseded
    them — were removed.

  **Update**: the durable retry outbox's drain gap noted above is now
  closed — see the 2026-08-13 entry below, which wires it into the main
  run queue and makes the whole recovery path crash-safe.

- **Four more release-blocking bugs from a second, independent static
  audit, plus four related hardening fixes** (see
  `docs/reviews/2026-08-13-release-readiness-static-audit.md`). This
  audit reviewed the fixes above and found new gaps introduced or left
  by them.

  - **CI flake that was actually a real bug**: a background test
    helper opened SQLite as `:memory:`; under load, Go's database
    driver can silently recycle a "bad" connection into a brand-new,
    empty in-memory database after a context cancellation. A test
    could write data, have its connection recycled, then read back
    from an empty database and see "table does not exist." Switched
    the affected test helpers to file-backed temp databases.

  - **A durable request's continuation after auto-summarization could
    be silently dropped, and its DB record deleted as if the whole
    request had succeeded.** When a long-running request needed to
    auto-compact its history mid-turn, the follow-up turn was
    resubmitted through the same guard that (correctly) blocks a
    *different* class of duplicate-execution — which meant this
    particular resubmission was silently discarded instead of queued.
    The follow-up turn now runs directly as part of the same request,
    the way it always should have.

  - **A slow-but-successful lease renewal could let two workers pick up
    the same durable request at once.** The safety timer that protects
    against overrunning a request's time-box compared against when the
    renewal call *returned*, not against the actual expiry time written
    to the database — so an unusually slow (but successful) renewal
    could make the timer believe it had more time left than it
    actually did. It's now anchored to the real, persisted expiry.
    (Fixing this surfaced a second, smaller bug: very short expiry
    windows — only used internally by tests — could be recorded as
    already expired due to a rounding error; expiry is now rounded in
    the safe direction.)

  - **A crash at exactly the wrong moment could leave a recovered
    request stuck forever, invisible to any future recovery attempt.**
    The orphan-request recovery path (added in the fix above) claimed a
    request, then separately re-queued it, then separately marked it
    done — a crash between any two of those steps left it stuck in an
    intermediate state nothing ever looked at again. Recovery is now a
    single atomic database step: either the request is fully re-queued,
    or nothing happened at all. There's no window for a crash to land
    in anymore.

  - **Orphan-request recovery only ran on a slow timer**, so a
    short-lived `rush run` invocation could start and finish without
    ever attempting to recover a pending request that was waiting on
    it. Recovery now also runs once immediately on startup, the same
    way normal request dispatch already did.

  - **Config changes made mid-session could very rarely leak into a
    request that had already pinned an earlier configuration**, in two
    places this round's earlier config-snapshot fix (2026-08-12/13,
    above) didn't quite cover: worker-model selection, and provider
    credentials used to retry after an auth failure. Both now
    consistently use the same pinned configuration as the rest of the
    request that's already using it.

  - **Removing a provider's saved API key could, in rare timing,
    revert a config change another part of the app had already
    published**, for the same underlying reason as a fix from the
    previous round (2026-08-12/13, above) — this was the same class of
    bug in a second, separate method that hadn't been audited yet.
    Fixed the same way.

  - **Hardening, not a confirmed live bug**: the periodic check for a
    stuck cross-process interrupt handler now has its own bounded
    time budget, independent of how long the surrounding request has
    been running. **Correction** (caught by a second-pass independent
    review after this entry was first written): this bounds a
    dependency that's slow but still honors cancellation — it cannot,
    and does not claim to, force-stop a hypothetical future dependency
    that never checks cancellation at all, which isn't something Go's
    context package can do without a separate detached-goroutine
    pattern that would reintroduce its own resource-safety risk. No
    such fully cancellation-ignoring dependency was found to exist
    today.

  **Known gap, not blocking**: the atomic orphan-recovery fix above
  means a request that can *never* successfully re-queue (e.g. because
  its data is malformed) will now retry forever instead of eventually
  giving up and marking itself failed — safer than the bug it replaces
  (a stuck, invisible request), but noisier. Tracked as follow-up work.

- **A follow-up round closing out a second, independent review of the
  fixes directly above it, plus a series of pre-existing CI-only test
  flakes surfaced while confirming the round on real CI.** None of the
  test fixes below change runtime behavior — they only make the test
  suite's *own* timing assumptions match how slow a real (especially
  Windows) CI runner can legitimately be under contention.

  - Two more places were still reading live configuration where they
    should have used the same pinned snapshot as the rest of a request
    (the same class of bug as several entries above), fixed the same
    way, plus a corrected, narrower test for the interrupt-handler
    time-budget fix above (it proves the fix bounds a slow dependency,
    not one that ignores cancellation entirely — a small overclaim in
    the original description). One test had gone stale after an
    earlier fix in this same round changed which internal call it
    needed to intercept — updated to match, restoring its regression
    coverage.

  - One review finding (a claimed ~10% flake on a specific test tied to
    this round's CI-flake fix above) could not be reproduced after 85+
    isolated attempts locally, nor on the actual CI run that shipped
    this round's fixes — real CI's `internal/session` package passed
    clean. Left as an open, documented disagreement rather than
    resolved either way.

  - Real CI subsequently surfaced five *different*, pre-existing test
    timing issues unrelated to any change in this round — three were
    simply too-tight wall-clock margins for a slower CI runner (widened
    with a documented rationale for why widening doesn't weaken what
    each test actually verifies); one was a genuine ambiguity bug in a
    test's own completion check (a row that's actively being processed
    reads identically to "already done" through a raw pending-count
    query, so the test could stop waiting a moment too early); one
    (macOS-only) was investigated but not fixed — an attempted fix was
    tested empirically and found to make the failure deterministic
    instead of occasionally flaky, so it was reverted with the
    investigation notes kept in place rather than ship a confidently
    wrong "fix". Windows CI runners remain the least reliable in this
    repo's matrix under full-suite `-race` load; tracked as follow-up
    work rather than chased indefinitely.
