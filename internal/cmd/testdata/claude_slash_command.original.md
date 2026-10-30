---
description: Delegate this task to a rush sub-agent instead of doing it yourself
---

**Opt-in only:** run `/rush <task>` (or “delegate this to rush”) once; never auto-invoke on follow-ups unless asked again.

## When not to delegate
Refuse in one sentence and offer direct help for interactive merges/conflict resolution/design/debugging, work dependent on this chat's decisions/files/partial state, requests where the user supplied the fix (you must verify it), one- or two-line changes, or a plan being worked out with the user.

## Smart means launch and wait
`rush run --role smart` reads the repo, plans, delegates, reviews, and reports. Tell it **what and why** (goal, observable acceptance, evidence, hard scope/constraints), not how. Launch in repo/worktree and await completion. Do not prescribe implementation plans, file:line edits, sequence, tools/roles/workers, verification, or forbid its internal delegation. Nested executors are expected: don't stop or ask; wait.
Only `--role`, `--session`, `--json`, and when warranted `--restrict-run`; don't tune models/effort/`--agents`. Don't coach mid-run: `sessions inject` is only for genuinely new facts or changed operator requirements. A follow-up passes reviewer findings, CI output, or new requirements verbatim into the same `--session`.

## Fallback when `rush` hits rate limits

When using Claude, an armed `/rush-fallback <agent>` means switch to that agent immediately and silently on either trigger: peak-hours refusal (match `is in peak hours (` in failed-run stderr or JSON `.error`) or hard limit (weekly/monthly budget exhausted, suspension, or context/quota exceeded without a retry-after in this session). This fallback never authorizes `--allow-peak-hours`; add that flag only when a human explicitly authorizes it in this request.

**Hard limit (all CLI targets):** exhausted weekly/monthly quota, suspended account, or context/quota exceeded with no retry-after that lands within this session. With no armed fallback, deny the reroute immediately: report exact error, provider, and retry/reopen time if supplied; stop and wait for human direction. Do not prompt for permission to bypass or choose a route. Claude's armed fallback is the sole automatic exception described above; other CLIs cannot arm it.
Never fallback for recoverable issues: larger `--timeout` in same session if yours fired; retry once in same session after default-15m `--idle-timeout`, then report if repeated; wait/retry short 429; retry 5xx/network blips and escalate if persistent; fix bad flags/workspace/prompt. Local fallback is last resort.

## Never self-add `--allow-peak-hours`
`rush providers set <id> --peak-hours HH:MM-HH:MM` configures the provider's local-time window (overnight allowed; `--peak-hours off` clears); `--peak-hours-message "..."` sets and `--peak-hours-message ""` clears the note; both honor `--local`. `rush run --allow-peak-hours` bypasses one invocation only; no persistent equivalent. Never add it unless a human explicitly authorizes it in this request. Without armed fallback, report refusal/retry-after and wait; don't switch providers, add the flag, or rerun until the local-time window closes. Armed Claude fallback switches as specified above.

## Resuming after `exit_reason: "awaiting_answer"`
`rush run --json` may force-finish with `exit_reason: "awaiting_answer"` when its own agent calls `ask_question`; this is deliberate, not a crash/provider refusal. `.error` includes the question, suggested options (if any), and ready resume command; `.final_text` is usually empty:
```
jq -r '.error' .rush/stdin/<task>.out
rush run --role <original-role> --session <id> "<your answer>"
```
Use the resume command above with the original role. Answer from known task/chat/files context yourself; for genuine product/design/risk choices, surface the question and suggested options verbatim to the operator and wait. Resume with a fresh `rush run --role <original-role> --session <id> "<answer>"`, never `sessions inject`: the asking process exited, and same session resumes it. **Do not issue a resume command while its live background jobs remain**: the run cannot exit yet; it gets a hint to finish without tool calls, receives job results, and continues. Resume only after it actually exits with `exit_reason: "awaiting_answer"`. `STALLED` status alone is not a resume signal; do not confuse it with `exit_reason: "stalled"`.

## Orchestrator mode — and why a worker's question is NOT your problem
With `--role smart` and configured worker (`rush models use ... --worker <model>`), Rush restores the `agent` tool despite its default sub-agent ban; `agentic_fetch` remains banned. Orchestrator mode delegates hands-on edits/writes/commands in worker-context-sized chunks and treats every report as a CLAIM: independently verify actual diff/test evidence.
A configured worker overrides `--agents single` for smart; that flag only prevents delegation without a worker or outside smart. For a hard no-delegation guarantee, don't configure a worker.
Configured reviewer adds one final review turn to successful smart runs and reviews the whole session; executor text remains `final_text`, reviewer conclusion is additive in JSON `review` (empty if none). Failed/canceled/timed-out/queued runs unchanged; explicit reviewer role isn't extended; override isn't persisted. Unconfigure reviewer to disable.

**Worker questions are not top-level questions.** A worker's `ask_question` doesn't exit Rush: the orchestrator receives and handles questions from workers reachable directly or indirectly through nested delegation, and continues. You see no prompt or resume task. Never inject into a worker child session. By contrast, the top-level question above exits (unless own jobs are running) and requires your fresh same-session resume.

## Launching (Claude)
Use `--role smart` for non-trivial work, `fast` for one-liners; worker roles are optional and may be reached directly or indirectly through smart orchestration; reviewer is optional. Neither role is auto-selected. The `rush models use` configuration command accepts `--worker <model>` or `--reviewer <model>` without positional arguments; example combined configuration: `rush models use <smart> <fast> --worker <model> --reviewer <model>`. Example reviewer-only run: `rush run --role reviewer --session "pr-42-review" "review this diff"`.
Use stable task-meaningful session IDs, unique across the project: linked worktrees share `<main>/.rush`, so old IDs resume. Session belongs to creating checkout; another checkout gets `belongs to <root>`; use `rush sessions fork <id> --cwd <path>` to continue there.
No `--timeout` by default (0; six-hour wall cap); `--idle-timeout` defaults 15m. Don't add arbitrary timeout.
Run with Claude `Bash` `run_in_background: true`, redirect output/error to `.rush/stdin/<task>.out` and `.err`. Launch one run at a time; wait for its real completion notification, verify that run and its diff, then launch the next. Never sleep-poll or launch a duplicate into a live/unknown session. After all runs have finished and each diff is verified, stage/commit yourself, sequentially. If independent fan-out is necessary, keep scopes isolated and do not stage/commit until all runs have actually finished and every diff is verified. Bash command must be bare `rush run ...`; never append `&`/wrapper (false completion causes duplicate locked-session launches). One command per call:
```
Bash({ command: "rush run --role smart --session foo --json < .rush/stdin/foo.prompt > .rush/stdin/foo.out 2> .rush/stdin/foo.err", run_in_background: true })
```
Write multiline prompt to `.rush/stdin/<task>.prompt`, feed `< file`; avoid multiline positional quotes. Rush permissions are auto-approved: use only workspaces you can lose; consider `--restrict-run`.
Default one run per worktree. Parallelize only for independent file sets when user asks or task requires fan-out. Every prompt must name its scope and siblings/scopes, forbid touching other scopes, and forbid git writes (`commit`, `add`, `stash`, `reset`, `checkout`/`restore`, `rebase`, `merge`); read-only `status`/`diff`/`log` okay. Orchestrator stages/commits sequentially after verifying diffs. Use separate worktrees for overlapping edits. Tests must stay within assigned packages (e.g. `go test ./internal/foo/...`, never `./...`) or be skipped and reported; run shared suite once after merge.

## Scoping permissions for a delegation — `--restrict-run`
Permissions auto-approve by default. For untrusted/exploratory prompts or overly broad workspaces, use deny-by-default:
```
rush run --restrict-run --role fast --allow-tool view --allow-bash 'git diff' --allow-bash 'glob:go *' --session "ci-123" "summarize the diff"
```
`--allow-tool <name>` repeats for non-bash tools or `tool:action` (e.g. `view`, `edit:write`); `bash`/`bash:execute` are no-ops. `--allow-bash '<pattern>'` repeats: word-boundary prefix (`git diff` matches `git diff HEAD~1`, not `git difftool`), `exact:cmd`, `glob:pat`, `regex:pat`. First three reject chaining metacharacters (`;`, `|`, `&&`, `$(`, backtick); regex alone bypasses guard, use deliberately. `run_command` program+argv is synthesized and matched like bash. `git_read` supports fixed read-only git commands and needs only `--allow-tool git_read`. Any tool call or command not matched by an allow rule is denied immediately; there is no permission prompt.
Equivalent `rush.json`: `{"permissions":{"run":{"restrict":true,"allow_tools":["view"],"allow_bash":["git diff","glob:ls *"]}}}`. CLI lists union with config; `--restrict-run` forces restrict on. Global non-run `permissions.allowed_tools` is an earlier bypass: `bash` there approves all shell regardless, so leave it clear when patterns should govern. Applies only to noninteractive `rush run`, not TUI/web. Add restrict when appropriate without human authorization: narrows risk, unlike peak-hours bypass.

## Monitoring
`rush sessions watch` is primary: auto-detects end, summarizes duration/tokens/cost, unlike `sessions tail --follow` doesn't hang on dead locks. `rush sessions watch` opens picker; `watch <id>` tails directly (short hash okay). Fresh pulse means alive, not progress; trust `sessions why` for STALLED and check its last-activity evidence. Resume the same session with `--session` only after it exits with `exit_reason: stalled`; a `STALLED` status alone is not that exit reason. Watch/tree may show transient worker child sessions; expected, don't inject into them.
Live-tail shows tool calls with key arguments inline:
```
[tool: bash] go test ./internal/agent/...
[tool: edit] internal/cmd/sessions.go
[tool-result: edit] internal/cmd/sessions.go: <result> (+3 lines)
```
Read-only alternatives: `sessions list` (STATUS), `locks` (`alive`/`ping`/`stopping`/`offline`/`between turns`; lock held only during turn, so offline alone doesn't mean dead), `show <id> --with-messages`, `last <id> [--n N]`, `why <id>` (prefer its explanation to cross-checking list/locks). `Ctrl+C` in watch reports `(interrupted — session still running)` without a summary.

**Liveness watchdog:** check about every 10 minutes while runs are live with `rush sessions why <id>` and `rush sessions locks <id>`; this probes liveness, not output. Offline/stopping/aging lock is normal between turns while waiting on bash job, delegation, or retry. `why` reporting `running` means keep waiting; `crashed` means process gone. For batches, `batch live` means work remains; `batch unknown` (incomplete read) means the batch read did not establish state. Never relaunch a live or unknown session/batch. `batch unknown` (incomplete read) is not termination evidence. Inspect output and `sessions last` only after confirmed process termination; relaunch same session only then.
Never relaunch into a live or unknown session: completion may describe a rejected duplicate, and incomplete batch reads are not evidence of termination. Confirm process termination with `why` or `show <id> --with-messages`; never infer it from completion or `locks offline`. For batches, require state other than `live` or `unknown` before relaunch; `batch unknown` (incomplete read) is not a terminated state. Nudge live runs with `rush sessions inject <id> -m "<msg>"` (`--interrupt` to cut in).
**Tear the watchdog down when it has nothing left to watch.** Once a session finishes, if you're not launching a replacement and no other `/rush` runs are in flight, drop the liveness loop. Re-arm it only when you launch the next run.

## Steering: `sessions inject`
Send new context to an existing run without killing/relaunching:
```
rush sessions inject <id> -m "also update the CHANGELOG"
rush sessions inject <id> -f ./notes/next-step.md
rush sessions inject <id> -m "stop — wrong approach" --interrupt
```
ID is launch `--session` (short hash okay). Default merge arrives next provider step without canceling current turn; `--interrupt` cancels current turn and restarts with message plus prior output. It persists as a normal user message visible in watch/last/web. If not running, persists for next run. Waiting between turns on job/delegation/retry counts as running (`running:true`, `between_turns:true`); no turn to cancel, message reaches next loop turn or next run. Cross-process via session DB; add `--json` for `{session_id,message_id,running,status}` and, between turns, `between_turns`/`driver_pid`.

## Repo defaults and model effort
`./.rush/system-prompts/default.md` auto-loads when no non-empty `--system-prompt` or `--system-prompt-file` content is provided; an explicitly empty prompt does not suppress the repo default. A non-empty explicit prompt or file takes precedence. Use repo-wide always-rules (scope, final message, no commit/push, tests, ambiguity). `rush models efforts [model]` lists/validates effort levels; `rush models bump <smart|fast|worker|reviewer> <up|down>` steps role effort (README.md `rush models` for details).

## When the lock is stuck
Don't `rm` lock (Windows OS handle prevents it). Use `rush sessions kill <id>` (kills PID/removes lock), `rush sessions kill <id> --wait 10s`, `rush sessions reset <id> --force` (also wipes history; refuses live work, cancel first), or `rush sessions reap` (all orphan locks). Windows kill uses `taskkill /F /T /PID` for whole `rush.exe`→`claude.cmd`→`node.exe` tree, waits for PID exit and retries lock removal.

## After the run finishes — you are responsible for verifying everything
Final text/JSON are claims, not receipts. Personally inspect all diff files/hunks/scope and claim-vs-diff; added/changed tests for real, non-vacuous regression value (confirm each would fail without the fix); rerun tests (prove flakiness); TODO/FIXME/placeholders, incomplete end-to-end wiring/mocks; and build/lint/typecheck. If wrong, redelegate same session with exact issue; don't hand-fix except true one-liner. Report only after seeing diff and test run; don't echo claims—say what actually changed, what you ran, compromises/redelegations.
Envelope: `.rush/stdin/<task>.out` first; `.warnings[]` `final_text is empty` can mean a fan-out without composed top-level reply, a tool_call without final text, or no tools/actionable output; inspect the specific warning, then use `git status` and `rush sessions last <id>` as needed. `sessions watch <id>` ends only when no lock, driver, job, or delegation remains; `sessions why <id>` explains.

## Time limits
`--timeout` defaults `0`; the `6 h` default cap is controlled by `RUSH_RUN_DEFAULT_HARD_TIMEOUT` (environment override applies to that default cap only). It attempts a graceful deadline and envelope; if the process remains alive 60 seconds later, the hard backstop force-kills it with exit 124, so graceful exit is not guaranteed. Set `--timeout <duration>` for longer waits or hard external deadlines; it caps the whole run. `--idle-timeout` (default `15m`) ends after no streamed output/tool call/result; unlike provider stall it doesn't silently retry. Running tool call counts as activity. Set duration or `0` to disable. It watches turns only; between-turn jobs/delegation/retry are bounded by explicit `--timeout` or the configured six-hour default cap.

## Task

$ARGUMENTS
