---
description: Delegate this task to a rush sub-agent that must work only inside an isolated git worktree and never run the tests — execution and verification move to a second, orchestrator-driven phase
---

`/wcrush <task>` is opt-in; do not auto-invoke on later turns. **Before doing anything else, read the `wrush.md` file in this same directory in full** — `/wcrush` inherits its worktree, refusal/fallback, `--allow-peak-hours`, `awaiting_answer`, smart/orchestrator, launch flags, `--restrict-run`, monitoring, `sessions inject`, lock recovery, and verification/merge/cleanup rules. Follow it except where this skill assigns testing to phase 2.

## Mandatory: two phases — the sub-agent does not run tests

**Why:** Concurrent `go test -race` runs have hit Windows `ERROR_COMMITMENT_LIMIT` (errno=1455), while writing costs little memory. Also, an agent previously hung during a revert-check after injecting `lifecycleMu.Lock()` around a disk read in `internal/agent/tools/mcp/init.go`, leaving production code broken. Worktree isolation kept it out of the primary checkout. Keeping execution/revert-checks out of phase 1 prevents this kind of test-induced landmine.

### Phase 1 — write and static checks, in parallel

The sub-agent writes production code and tests. It MUST run these memory-light checks (and the project's linter if cheap):

```
go build ./...
go vet ./...
gofmt -l .
```

Hand over code that compiles and passes static analysis. Phase 1 forbids `go test` in any form, revert-checks, debugging by running anything, and neutering production code to observe failure. These belong to phase 2.

### Handover — final message contract

The agent's final assistant message must state plainly that work is **NOT verified by execution**, and list:

1. Tests written and the failure scenario each catches.
2. For each, the exact revert-check (what to neuter and expected failure output).
3. Exactly what to run, and in what order.

Without this, phase 2 must reverse-engineer the intent.

### Phase 2 — with orchestrator permission, same session

Resume the **same** session from the worktree that created it. A session belongs to its creating checkout, so run this command with the worktree as the current directory; do not resume it from the primary checkout:

```
rush run --role smart --session <same id> "<permission and what to run>"
```

Context is preserved; only now may the agent run tests, perform revert-checks, and fix failures. `--role` is mandatory when resuming; omitting it exits with code 1.

**Reviewer auto-pass:** with a reviewer configured, this successful `--role smart` resume adds a Reviewer turn reviewing the complete phase-1+phase-2 session. Its conclusion is carried in the JSON envelope's additive `review` field; the executor's answer remains `final_text`. This is Rush-driven, not extra requested work; unconfigure the reviewer to disable it.

## Inherited rules that phase split changes

- **Delegation stays enabled:** task text may constrain the deliverable, but must not prohibit smart's internal delegation. Nested executors starting is expected, not a reason to stop or ask the operator: launch and wait for the run to finish.
- **OOM discipline belongs to phase 2** — the other half of the same
  bargain: `-parallel 2` for heavy packages, never two heavy runs at
  once, long runs backgrounded via the Bash tool's
  `run_in_background: true` parameter and never with a trailing `&`.
- **Zero trust:** phase-1 handover and phase-2 pass reports are claims, not receipts. The orchestrator personally verifies the diff, tests, and revert-checks, one at a time, inside the worktree, using wrush.md's checklist.
- Do not touch `CLAUDE.md`, `.github/workflows/`, or `web/dist/.gitkeep`; do not use `ReportFindings` or the `code-review` skill.

**`/wrush`'s dedicated git worktree, no-sub-agent-commit/push, orchestrator transfer/commit, and worktree removal rules remain in force. The orchestrator commits only those changes before removing the worktree. Do not report done until changes are merged, committed, and the worktree removed.**

## Task

$ARGUMENTS
