---
description: Delegate this task to a rush sub-agent that must work only inside an isolated git worktree, never in the main checkout
---

`/wrush <task>` is opt-in; do not auto-invoke on later turns. **Before doing anything else, read the `rush.md` file in this same directory in full** — it defines the playbook `/wrush` inherits unchanged: refusal criteria, rate-limit fallback, `--allow-peak-hours`, `awaiting_answer` resumption, smart/orchestrator behavior, launch flags, `--restrict-run`, monitoring, `sessions inject`, lock recovery, and zero-trust verification. Follow it except where this skill adds stricter worktree rules.

## Mandatory: dedicated git worktree for every invocation

Unlike `/rush`, which isolates parallel/overlapping runs, `/wrush` always uses a dedicated git worktree—even for solo, trivial, or self-performed work. Never edit the primary checkout; there is no fallback path that works there.

1. Keep worktrees inside `<repo-root>/worktrees/`, never beside or elsewhere (external cleanup paths can trigger unattended safety prompts). Before the repo's first `/wrush` worktree, ensure `.gitignore` has a `worktrees/` entry (accept `/worktrees/`, `worktrees/`, or `worktrees`); if absent, append:
   ```
   # /wrush task worktrees — always created here, in-tree
   /worktrees/
   ```
   Do this once per repo.
2. From the current base (default `HEAD`, unless instructed otherwise), create a unique project-wide task/session slug and branch:
   ```
   git worktree add -b <task-slug> <repo-root>/worktrees/<task-slug> <base>
   ```
   Add an issue/PR number or date; never reuse a finished task's ID or use bare `fix`/`test`. Linked worktrees share the primary checkout's `<main>/.rush` sessions, messages, batch work, and logs, which remain after removal; `rush sessions list/locks/watch/why` from the primary checkout sees all agents. A pre-existing local `.rush` containing `rush.db` remains in use only while this worktree has no shared-mode marker; once pinned, that local DB is stray and ignored. The marker is `<main>/.rush/workspaces/<hash-of-canonical-worktree-root>`. Dev builds (`go run`, or `go build -o` inside a checkout or temp directory) use `<wt>/.rush/dev` in a linked worktree; other builds use `<main>/.rush`. A session belongs to its creating checkout: using `--session <id>` elsewhere is refused (`belongs to <root>`); use `rush sessions fork <id> --cwd <path>` to continue elsewhere.
3. **Launch `rush run` with cwd inside the worktree (`cd` in the same
   Bash call) — every edit, git op, and test the sub-agent runs stays
   inside that tree. Redirect `.rush/stdin/<task>.{out,err}` to the
   PRIMARY checkout so results survive the eventual worktree removal.**

   Worktree-scoped `rush run` is delegation layer one. `/rush`'s `--agents single` and orchestrator-mode rules govern layer two: whether that sub-agent may delegate to workers via `agent`. `--agents single` neither enables nor disables worktree isolation, and isolation neither changes the flag's effect. Cwd isolation is inherited by the process and its workers; no separate isolation step is needed.

   Task text may constrain the deliverable, but must not prohibit smart's internal delegation. Nested executors starting is expected, not a reason to stop or ask the operator: launch and wait for the run to finish.
4. If `/rush`'s refusal list means doing the work yourself, still create the worktree first; edit, verify, and merge its diff as for a sub-agent. Never edit the primary checkout directly.
5. After verification using `/rush`'s checklist inside the worktree, merge the reviewed diff into the primary checkout yourself: use `cp`/`Edit` for non-overlapping files and hand-reconcile concurrent `/wrush` edits to the same file. Prefer this to `git merge`/`git pull` unless explicitly requested.
6. Review the **staged diff** (`git diff --cached`) before committing. Stage only this task's files; preserve unrelated changes. The sub-agent never commits; this does not authorize a push. Skip an empty commit. Commit the transferred changes in the primary branch only after this staged-diff review.
7. After the merge is committed, remove the worktree (`git worktree remove`) and branch (`git branch -D`), unless asked to keep them. It is always an in-repo ignored-directory cleanup.
8. Parallel `/wrush` runs follow `/rush` unchanged: disjoint scopes, sibling names, no git writes, scoped tests; only isolation becomes mandatory.

Do not report completion or stop until steps 5–7 are personally done: unmerged or unremoved worktrees are unfinished and not delivered. Never delegate or defer this cleanup.

## Task

$ARGUMENTS
