# Flexible weekly peak-hours (task #56)

Operator guide to per-provider peak-hours refusal windows: the weekly policy
format, the legacy single-window form, the CLI command family, pause/resume,
and what the runtime actually does when a provider is blocked.

## 1. Overview

Peak hours are configured **per provider**. New requests are refused inside a
peak window unless an operator pause is active. A pause temporarily suspends
peak-hours enforcement and allows requests. Already-started streams are never
interrupted; each subsequent provider request is checked again.

Key properties:

- The policy lives on the **provider entry** (`providers.<id>.peak_hours`).
  Roles and profiles that share a provider ID share its schedule; there is no
  per-role or per-profile override.
- There are **no named presets** and no global schedule catalog: every
  provider carries its own windows.
- There is deliberately **no per-role bypass**. Apart from a temporary
  operator pause, the invocation bypass is `rush run --allow-peak-hours`.
  The flag must never be self-added by agents.

## 2. Config format

The canonical form is an object under `providers.<id>.peak_hours`:

```json
{
  "timezone": "Asia/Shanghai",
  "windows": [
    { "days": "mon-fri", "start": "14:00", "end": "18:00" },
    { "days": "sat,sun", "start": "10:00", "end": "12:00" }
  ],
  "message": "Quota guard",
  "paused_until": "2026-11-09T18:00:00Z"
}
```

All fields are optional; a policy with no windows means the feature is off for
that entry.

**Timezone** — `""` (or `local`) means the machine's local clock; `UTC`; an
IANA name such as `Asia/Shanghai`; or a fixed offset such as `UTC+08:00`.

**Days grammar** (`days` field of a window): weekday tokens `mon`..`sun`,
written as a single day (`fri`), a comma list (`mon,wed,fri`), a range
(`mon-fri`), or a wrapping range (`fri-mon` = Friday through Monday). A days
field that is absent or empty means **every day**.

**Window semantics**:

- `start` is inclusive, `end` is exclusive. `24:00` is valid **only as an
  end**. `00:00`–`24:00` is a whole day.
- A zero-length window (`start == end`) is a **load error**, not "never".
- An overnight window (`start > end`, e.g. `fri 22:00-06:00`) belongs to its
  **start day**: the Friday 22:00–24:00 leg plus the early Saturday 00:00–06:00
  leg are both governed by Friday's mask.
- At most **64 windows** per policy; more is a load error.
- Overlapping and touching windows **merge into one continuous blocked band**.
  `reopensAt` (what refusal messages and status output report) is the end of
  the whole merged band, not of the individual window.
- A policy that covers the entire week (every minute blocked) reports **no
  invented reopen time** — there is none.
- DST follows the **real clock**: windows are wall-clock times in the policy
  timezone. A `14:00-18:00` window under `America/New_York` runs 14:00–18:00
  local on both DST-transition days, even though one is 1h shorter in UTC
  terms.
- The next transition reported in status output is bounded to **8 days**
  ahead; if no transition exists within that horizon, none is reported.

## 3. Legacy compatibility

The old single-window form still reads and writes unchanged:

```json
{ "start": "09:00", "end": "18:00", "message": "quota" }
```

This is one machine-local (`HH:MM`) window, no days mask, no timezone.

Compatibility rules:

- Old CLI flags (`rush providers set ID --peak-hours HH:MM-HH:MM` /
  `--peak-hours off`, `--peak-hours-message`) still work and never wipe
  unrelated fields of the provider entry.
- In layered configurations (global + workspace), **legacy** entries
  field-merge: a workspace fragment with only `start`/`end` keeps the global
  `message`.
- A **new-format policy overrides between scopes entirely**: a policy in the
  workspace scope replaces a global policy (or legacy entry) wholesale — no
  field-level mixing across the format boundary.
- `peak_hours: null` (or an explicitly empty window set) in the workspace
  scope **suppresses** an inherited global policy.
- Absence of the key inherits the outer scope, as usual.
- Mixing legacy `start`/`end` with `windows` — including an incomplete legacy
  fragment layered over a policy — is a **load error**. It is never guessed
  into one form or the other.
- Editing a legacy entry whose window is empty (`start`/`end` both empty or
  equal) normalizes to **off** on write; reading such an entry keeps the old
  semantics (feature off).

## 4. CLI reference

All commands are under `rush providers peak-hours` and accept `--global` (the
write default) or `--local` to pick the target scope. Read commands (`list`,
`show` without a scope flag) show the **effective merged** view.

```
rush providers peak-hours list                       # every provider with a policy
rush providers peak-hours list --json                # one NDJSON object per provider
rush providers peak-hours show zai                   # effective policy + live status
rush providers peak-hours show zai --json
rush providers peak-hours set zai mon-fri 14:00-18:00 sat,sun 10:00-12:00
rush providers peak-hours add zai sat,sun 10:00-12:00   # append; never duplicates identical windows
rush providers peak-hours rm zai 2                      # remove window #2 of the stored order
rush providers peak-hours tz zai Asia/Shanghai          # set/replace the timezone
rush providers peak-hours clear zai                     # remove peak_hours from the scope
rush providers peak-hours pause zai 2h
rush providers peak-hours resume zai
```

- `set` **replaces** the windows of the target-scope entry; `--tz` and
  `--message` optionally update those fields; other fields (paused_until,
  timezone, message) are preserved unless their flag is passed.
- `rm` numbers are 1-based positions in the stored window order, exactly the
  numbers `show` prints (use `--global`/`--local` to match the scope). `rm`
  on a **legacy single-window entry** first **migrates** it to the weekly
  form (the legacy window becomes the equivalent whole-week window) and then
  removes the numbered window.
- `clear` removes the scope's `peak_hours` entry entirely (the scope then
  **inherits** the outer scope again).
- `--json` on `show`/`list` emits machine-readable NDJSON with RFC3339
  timestamps.
- Explicit **off** in the local scope (web toggle-off, `rush providers set ID
  --peak-hours off --local`) or `{"windows":[]}` in the local scope persists
  as an explicit `peak_hours: null` override suppressing a global policy; off
  in the **global** scope removes the key.
- The legacy `rush providers set ID --peak-hours ...` /
  `--peak-hours-message ...` flags remain. Legacy entries keep their legacy
  form. On an existing weekly policy, `--peak-hours` replaces its windows
  with one daily window while preserving timezone, pause and any unchanged
  message; `--peak-hours-message` alone preserves the windows too.

## 5. Pause and resume

Pausing is an **explicit operator action** that suspends refusals for a while.
It is stored as `paused_until` (RFC3339) inside the same `peak_hours` policy
through the normal ConfigStore write path — there is no separate state file,
no cron, and no background writer.

- Durations: `2h`, `90m`, `7d`, `6d12h`, or an explicit RFC3339 timestamp.
  The pause may last at most **7 days** from now; longer is rejected.
- Without a duration, the pause runs **until the end of the currently active
  peak band**. This requires the provider to be inside an active peak window
  right now: it errors when the provider is **already paused** (resume first)
  or is **outside any peak window**, and when the band never ends.
- Passing a duration again **replaces** an earlier pause; it never extends or
  errors.
- Expiry needs no background job: the next status evaluation simply observes
  that `paused_until` has passed.
- The **settings lock applies** to these writes like any other config write —
  there is no hidden unlock path for pausing.

While paused, requests are not refused by peak hours; `resume` clears the
pause (also a normal config write). Pausing an already-paused provider
**without a duration** errors and tells you to resume first; pausing again
**with a duration** simply replaces the pause deadline.

A pause **suppresses** peak-hour refusals until `paused_until` — it never adds
prohibition. While paused inside a window, requests are allowed; outside any
window the pause changes nothing.

## 6. Runtime behaviour

The runtime gate is **pre-flight only**: it applies to **new provider
requests** (the `buildCall` pre-check plus the summarize/sub-agent pre-checks).
A window opening mid-turn **never aborts a running stream**; the old mid-turn
watcher was removed. Already-started streams are never interrupted by a
window boundary or a pause.

- New provider requests are refused pre-flight with a typed `PeakHoursError`;
  the refusal text always contains the stable marker `is in peak hours (` for
  tooling and fallback automation to match. A fully blocked week reports
  **"no scheduled reopen"** — never a `0001` date or an invented `00:00`.
- While a pause is active the provider is simply not refused (see §5); the
  runtime never emits a "pause refusal" error.
- Already-started streams are **never interrupted** by a window boundary or a
  pause.
- `rush run --allow-peak-hours` bypasses the check for **one invocation
  only**; nothing persists it and agents must never add it themselves.
- Config edits made by other processes (CLI, another server) are picked up via
  the existing config store's hot reload — no restart needed.
- There are **no paid retry loops**: a peak-hours refusal ends the attempt
  instead of silently re-dialling a blocked provider.
- A refusal that ends a `rush run` reports exit_reason **`peak_hours`** in
  the `--json` envelope (not the generic `error`), with a non-empty
  `final_text` naming the window and the exact `RESUME AT:` time, and sets
  the session's `ended_reason` to `peak_hours` (read by `rush sessions
  status` / `why`). The process exit code stays non-zero and the stable
  `is in peak hours (` marker stays in `.error`, so existing matchers keep
  working.
- Nothing is cleaned up on this path: no terminal assistant message is
  expected for a refused turn (so no "authoritative terminal message
  reconciliation failed" warning is emitted), the unfinished-todos
  reminder turn is skipped with a stderr note instead of failing, and
  completed work, pending notifications and the reaction debt stay
  untouched for the resumed run.

## 7. Web UI

The providers modal exposes the same feature to both **built-in and custom
providers** through one shared editor (`web/src/components/PeakHoursEditor.tsx`):
day chips, start/end inputs per window, timezone, refusal message, and the
pause/resume controls — pause/resume is exposed for both built-in and custom
providers. Pausing **without a duration** is
supported from the UI (the server picks the end of the currently active band,
as in §5); server-side errors (already paused, outside any band, invalid
duration) **surface in the editor** instead of failing silently.

- The **status line/badge is backend-authoritative**
  (`web/src/components/PeakHoursStatus.tsx`): everything — `checkedAt`,
  `inPeak`, `blocked`, `reopensAt`, `pausedUntil`, `timezone` — comes from the
  server's `get_peak_hours_status` reply. The browser **formats** the
  timestamps but never decides peak state itself; there are no
  browser-clock "active now" computations.
- Refresh strategy: a fetch on mount, a scheduled refetch just after the next
  boundary (reopensAt / pausedUntil / nextTransition, capped at 24h), plus one
  bounded 120s fallback interval while the modal is open — not a global poll.
- WS commands: `set_provider_peak_hours`, `pause_provider_peak_hours`,
  `resume_provider_peak_hours` (admin authority, same as other provider
  configuration) and `get_peak_hours_status` (**read** scope).

## Layered example

Global `rush.json`:

```json
{
  "providers": {
    "zai": { "peak_hours": { "timezone": "Asia/Shanghai", "windows": [{ "days": "mon-fri", "start": "14:00", "end": "18:00" }] } }
  }
}
```

Workspace `.rush/rush.json`:

```json
{
  "providers": {
    "zai": { "peak_hours": null },
    "ollama": { "peak_hours": { "windows": [{ "days": "fri-mon", "start": "22:00", "end": "06:00" }] } }
  }
}
```

Result: `zai` has no peak hours locally (explicit `null` suppresses the global
policy); `ollama` has one overnight window belonging to its start day
(Friday), evaluated in machine-local time.

## Where it lives

| Concern | Path |
| --- | --- |
| Weekly policy type, validation, evaluation | `internal/config/peak_hours_policy.go` |
| Legacy single-window form | `internal/config/peak_hours.go` |
| Layered merge / override rules | `internal/config/peak_hours_merge.go` |
| Timezone resolution | `internal/config/peak_hours_tz.go` |
| CLI command family | `internal/cmd/providers_peak_hours.go` |
| Refusal paths, `PeakHoursError`, `--allow-peak-hours` | `internal/agent/agent_turn_peakhours.go` |
| WS handlers and wire payloads | `internal/server/handlers_peak_hours.go`, `internal/server/protocol_peak_hours.go`, `internal/server/scope_table.go` |
| Shared editor / status components | `web/src/components/PeakHoursEditor.tsx`, `web/src/components/PeakHoursStatus.tsx` |
| E2E specs (CI-only) | `web/tests/peak-hours-weekly.spec.ts`, `web/tests/providers.spec.ts` |
