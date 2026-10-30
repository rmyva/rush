---
description: Handle peak-hours refusals and hard quota limits with harness-specific fallback behavior
---

`/rush-fallback <agent>` arms local `Agent` type `<agent>` (e.g. `sh`, `oh`, `fm`) for later; it does **not** switch now. Keep using `/rush` (`rush run`) until exactly one trigger:
1. **Peak hours:** `rush run` refused inside the provider's configured peak-hours window.
2. **Hard limit:** weekly/monthly quota exhausted, account suspended, or "context window exceeded"/"quota exceeded" without a retry-after within this session.

Nothing else counts. A slow run, a transient 5xx, a `Process restarted`, your own `--timeout`/`--idle-timeout` firing, a confusing error — all of those mean **re-run `rush run` against the same `--session`**, not switch.

When either trigger fires, switch to that agent immediately and silently: `Agent({subagent_type: "<agent>"})`. Arming is consent. Don't ask permission, request refusal text, or confirm. Say one line. Stay switched until the peak-hours cron fires or `/rush-fallback clear` ends a hard-limit fallback; do not retry `rush run` meanwhile (see active-state rules).

If `<agent>` is missing on invocation, **stop and ask** which agent type to use — that's the only thing you may never pick yourself.

## The eternal marker task

Store all state in ONE persistent TaskList task per session, found by subject prefix (never exact-match; suffix is live summary):
```
rush-fallback state (persistent — do not complete)
```
Use `TaskCreate` (`status: pending`) on first invocation; thereafter `TaskUpdate` subject and description in place. Never duplicate, complete, or delete the sentinel task. Subject is one line: exact prefix + `: ` + readable summary, e.g.:
```
rush-fallback state (persistent — do not complete): sh — armed, switches on peak-hours or hard-limit
rush-fallback state (persistent — do not complete): sh — ACTIVE (hard-limit), switch back with /rush-fallback clear
rush-fallback state (persistent — do not complete): dormant
```
It survives `/checkpoint`, `/resume`, and compaction so a fresh session can recover state via `TaskList`.

Description schemas:
```
STATUS: armed          # registered; /rush still in use
AGENT: <agent>
```
```
STATUS: active         # trigger fired; delegate via <agent>
AGENT: <agent>
TRIGGER: peak-hours | hard-limit
PROVIDER: <id>
UNTIL: <RFC3339>       # peak-hours only; "none" for hard-limit
CRON_JOB_ID: <id>      # peak-hours only; "none" for hard-limit
```
```
STATUS: dormant        # cleared
```
This is instruction-level, not a code hook: `/rush` does not read the marker; the operating agent must honor it.

On invocation, upsert `STATUS: armed` + `<agent>` and report: `fallback armed: <agent> (engages on peak-hours refusal or hard limit; /rush in use until then)`. Continue `/rush` unchanged. Re-invoking with another agent (armed or active) replaces only `AGENT`; preserve trigger and cron.

## Trigger 1 — peak-hours refusal

Match the literal `is in peak hours (` in failed-run stderr or `--json` `.error`; `exit_reason` alone is insufficient (`canceled`, `timeout`, and `error` can also describe non-peak-hours outcomes).

Handle both observed shapes:
- **Full guidance:** includes `RESUME AT:` (local date-time + RFC3339) and `peak-hours window`; emitted by `internal/agent/peak_hours_stop.go`'s `PeakHoursGuidance` for top-level refusal.
- **Short form:** `is in peak hours (HH:MM–HH:MM), refusing until HH:MM`, no `RESUME AT:` or `peak-hours window`; sub-agent stream startup path (`failed to start agent processing stream: …`). It may occur after 800+ seconds of work.
Requiring all three markers misses short form and keeps hammering the refused provider.

1. Determine reopen time: full block → copy `RESUME AT:` RFC3339 verbatim (exact; day-wrap handled); short form → interpret `refusing until HH:MM` as local time, tomorrow if not later than now. If already past, window closed while reading: stay on `/rush`, don't switch.
2. Set marker `STATUS: active`, `TRIGGER: peak-hours`, provider id, `UNTIL`.
3. Create the one-shot auto-revert cron below; save its id as `CRON_JOB_ID`.
4. Route subsequent delegated work through `<agent>`.

```
CronCreate({
  cron: "<MM> <HH> * * *",     // ReopensAt minute+hour, local time
  recurring: false,
  durable: true,
  prompt: "# rush-fallback resume\n\nThe peak-hours window for provider <id> has closed (was due <RFC3339>).\n\n1. Do NOT interrupt any <agent> run still in flight — let it finish.\n2. Stop launching NEW work through <agent>.\n3. Route subsequent delegated work back through `/rush` (rush run).\n4. TaskUpdate the `rush-fallback state (persistent — do not complete)` task's description back to `STATUS: armed` + `AGENT: <agent>` — do NOT complete or delete the task.\n5. Tell the user fallback has ended and `/rush` is back in use."
})
```
`durable: true` intentionally overrides CronCreate's default: a multi-hour wait must survive process exit or fallback would have no return.

## Trigger 2 — hard limit

Weekly/monthly budget exhausted, account suspended, or "context window exceeded"/"quota exceeded" with no retry-after inside this session. No reopen time exists, so: marker → `STATUS: active`, `TRIGGER: hard-limit`, `UNTIL: none`, `CRON_JOB_ID: none`, no cron armed. Route work through `<agent>`. Only `/rush-fallback clear` ends it.

## While active

While active, delegate every `rush run` task via `Agent({subagent_type: "<agent>", ...})`, with same briefing (goal, file-set, done criteria) and `/rush` hygiene: scope concurrent work, no parallel git-writing agents on one tree, scope tests to changes, zero-trust diff verification. Transport changes, not verification bar. Use one agent type for all work; reason is Rush unavailability, not task complexity.

### Once armed, stop checking — the cron IS the resume signal

While `STATUS: active`, do not use `/rush`: no `rush run` (even different/urgent tasks), manual `UNTIL` checks, retry/poll (`rush sessions why`, `sessions locks`, `sessions list`), editing `UNTIL`/`CRON_JOB_ID`, or second cron. The scheduled cron owns reopen decision. Retrying earns the same refusal and wastes time/tokens; sub-agent path may refuse after substantial work, leaving partial edits to triage.

Until the cron fires, route new delegated work through `<agent>` and leave marker unchanged. For `TRIGGER: hard-limit` (no cron), only `/rush-fallback clear` ends this state. `/rush`'s ~10-minute `rush sessions locks` watchdog applies only to `rush run` processes you launched—not `<agent>` runs; do not repurpose it to poll peak hours (harness notifies on sub-agent completion).

## `/rush-fallback clear`

If marker is `dormant`, report and stop. Otherwise `CronDelete` recorded `CRON_JOB_ID` if any, set `STATUS: dormant`, and report. In-flight `<agent>` runs finish; clear stops only new delegation.

## Task

$ARGUMENTS
