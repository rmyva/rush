# Session operations: liveness, batches, and compaction

Rush records separate facts for a model turn, the session that owns it, and any batch/task execution. A message finish, session status, batch state, and physical process exit are not interchangeable completion signals.

## Session lifecycle and completion

A session lock is held during a turn, then released between turns. A released or missing lock by itself does not prove the session ended: a live `rush run` driver, a running own job, a live delegation, or an open one-shot wake schedule can keep the session active. Use `sessions why` to inspect combined session and batch evidence, `sessions watch` to follow activity while a lock or live work remains, and `sessions locks` to inspect lock files; no single one is a physical-process or batch-completion oracle.

```sh
rush sessions why <session-id>
rush sessions watch <session-id>
rush sessions locks <session-id>
```

Lock listings are read-only by default. Heartbeat age alone does not prove death, especially between turns. The opt-in `sessions locks --prune` removes only a lock proven to have no live OS holder. The default `sessions locks` view also collapses idle lock files — no holder PID recorded, offline pulse, no live-driver evidence, i.e. what a clean release leaves behind — into a single summary line; `--all` lists them anyway, and `--json` output ends with a `{"summary":{...}}` object carrying the collapse count. `sessions reap` additionally reclaims ancient idle lock files: a PID-less file whose mtime is older than `--idle-older-than` (default 7d) is removed only after the same real OS-lock probe plus a file-identity check immediately before unlinking, and `--dry-run` previews the candidates without touching or probing them. `watch` tails messages and remains open for live work between turns; interrupting it stops watching, not the session.

`end_turn` finishes an assistant turn, not the session or every task it started. `between_turns` means live work still holds the session open. An `unknown` batch/session read is incomplete evidence, not proof of absence, idleness, or completion. In batch state, `cancelling` is nonterminal until termination is confirmed; a stop request or accepted acknowledgement is not that confirmation. Check the batch tree with `rush batch show <root-id>` and use terminal node/root states and their outcomes to determine task completion.

## Checkout ownership

Sessions belong to the workspace root that created them; a run from a different checkout/worktree cannot drive that session. `git_branch` is audit attribution, not an ownership test. This ownership boundary does not serialize concurrent edits to the same files, so use separate worktrees when independent work overlaps.

## Question pauses and safe continuation

A top-level `ask_question` stops a `rush run` with `exit_reason: "awaiting_answer"` only when it has no live own jobs or delegations that would be orphaned. That exit reason is an intentional continuation point, not a failed run or completed task. Answer the question and resume the same session with the role and session ID supplied in the run's guidance, for example:

```sh
rush run --json --role smart --session <session-id> "<your answer>"
```

For a safe retry after an interrupted/uncertain run, first establish that the original process has physically exited **and** that no live or unknown batch scope still owns work. A returned `end_turn`, a session-only `why`/`watch` result, or stop acknowledgement is not sufficient proof. Inspect batch roots/nodes and do not resubmit work while a live or unknown batch scope remains. Resume/retry using the original checkout/worktree and the original role and session; do not move the same session to a different checkout or create a replacement submission just because a turn ended.

If the session still has its own running jobs or live delegations, `ask_question` instead gives the model a hint and does not stop the turn; the model should wait with `await_tasks` or finish its turn so the work can be delivered.

There are two child-agent question paths; use the matching one:

- A legacy child agent paused between turns on its own `ask_question` is resumed by the orchestrator calling `agent` again with `resume_session_id` set to that child session and the answer as the prompt.
- A child paused inside a **live batch** ask has an actionable held question in the current batch execution. Answer it with `inject_agent(child_session_id="<exact child id>", message="<answer>")`. Do **not** use `resume_session_id` or create another delegation for this case: that queues a new run behind the held ask rather than answering it. `inspect_agent` reports `held_question` for this batch case; answer the current question before waiting for task completion.

## Batch execution and recovery

`rush batch run` runs unattended in the owning process and waits synchronously for its root to settle. It has no question-answer surface; use the served batch intake when held questions need an answer route. `--detach` is unsupported. `rush batch add` requires the owner process and refuses from another process; separate processes can inspect or request stop through the supported CLI controls. A stop is cancellation intent; even an accepted request does not mean the task has physically terminated.

Startup and periodic maintenance perform bounded dead-host recovery. Only a host proven dead is recovered: live work in retained CLI roots is interrupted and its controls are closed; unannounced non-CLI reservations may be deleted, while CLI roots are retained. This recovery records interrupted work; it does not replay or automatically resubmit it. A new execution requires a new, explicit submission. The scan is bounded and can continue on a later pass, so inspect current batch state rather than interpreting one pass or a missing read as successful completion. This is distinct from the one-time database import/upgrade procedure, where the documented requeued imported work can repeat external side effects.

## The two compaction boundaries

Model-context compaction summarizes an existing session's conversation history; SQLite `VACUUM` reclaims free database pages. They are different operations. With no session argument, `rush sessions compact` retains its database-only `VACUUM` behavior. Database `--dry-run` can checkpoint the WAL, so it is not side-effect-free.

For model-history compaction, target an existing session explicitly:

```sh
rush sessions compact <session-id>
rush sessions compact <session-id> --role worker
```

The role defaults to `worker`; supported overrides are `worker`, `smart`, `fast`, and `reviewer`. The session can be selected by exact ID or a unique hash prefix. Rush uses the selected role's session override, then that role's configured slot; if it is absent, unavailable, or unsupported, compaction errors rather than falling back. Custom roles are not supported. `--role` without a session is rejected. `--dry-run`, `--force`, and `--json` are database-only options and are rejected with a session argument, including when explicitly set to false.

This command summarizes and persists the existing history using the ordinary summarization operation (`SummarizeNow`); it is not a conversational `Run`, does not create a new leaf, and does not replay the original tools. It is available only for standalone direct providers: a backend of type `cli` is refused before model construction. Rush checks combined activity; live or unreadable activity refuses compaction, using the normal mailbox and OS-lock coordination. A busy result is immediate: it does not queue, kill a process, bypass ownership, or automatically resume. Run it from the correct project/config/data root; do not create a fresh session to evade session ownership.

Existing pinned-message, history, publication, cancellation, and failure semantics still apply. A summary can exceed the worker's context capacity; on error, the original history is preserved. This explicit command is not a universal recovery guarantee. Ended external processes are not automatically resurrected. See [session compaction](sessions-compact.md) for the command contract.

### Automatic context-overflow recovery

Only an ordinary direct Codex `rush run`/resume has this recovery: recovery is eligible only for an exact structured `context_length_exceeded` error certified before any output or usage (HTTP 400 or SSE `response.failed` or strictly guarded nested/flat `type=error` envelopes). Narrowly validated nested response lifecycle configuration metadata does not count as output; accepted or opaque output and any usage preclude recovery. That permits one silent-prefix summary and continuation in that same session. Partial, opaque, or uncertified output, transport errors, and NoReplay-fenced failures are never replayed.

The summary uses only the captured matching worker session override/config (or covered per-call credentials); there is no silent role/provider fallback, and CLI workers are unsupported. Rush preserves pinned context, the original user message, and the accepted tool-call/result tail, then continues with the original model, captured tools, and authority without re-executing accepted tools. Missing/unavailable workers, unknown or insufficient capacity, budget caps, or failed/cancelled summaries fail truthfully; history is not deleted before successful publication. There is one recovery attempt and continuation; a second overflow is terminal. Capacity reserve accounts for effective output allowance plus cache-inclusive occupancy; no task size, including task 371, is guaranteed to fit. This does not resurrect an already-exited process; an ordinary run/resume requires the matching installed release.
