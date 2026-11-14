# rush sessions status

`rush sessions status <id> [--json]` is the single machine-callable snapshot
for the question "what is this session doing right now": is its process still
alive, what phase is it in, how long since real activity, are there live
children, and is the associated batch work terminal.

It is strictly **read-only**: it acquires no lock, writes no database row,
and prunes nothing. One invocation reads the session row (including the
recorded `ended_reason`), the session lock fact (a real OS liveness probe of
the recorded holder PID), run-driver markers plus host lock probes, live
batch roots through the shared batch live reader (the same read
`sessions why` and `sessions jobs` use), open wake schedules, messages and
tool calls, call-tree activity across sub-agent descendants, pending
child-question notices, and the sub-session tree.

## Honesty rule

Any state the evidence could not establish is reported as the literal value
`"unknown"` — never a guess. `sources` names the snapshots that actually
contributed evidence; `notes` explains every read that failed or degraded.

## Phase vocabulary

| Phase           | Meaning                                                                   |
| --------------- | ------------------------------------------------------------------------- |
| `turn`          | The session lock is held by a live holder (a turn is running).            |
| `between_turns` | No live lock or delegation, but durable work (driver, jobs, schedules).   |
| `waiting`       | A live sub-agent delegation is keeping the session open.                  |
| `idle`          | Nothing is known to be happening and no end is recorded.                  |
| `finished`      | The session row records an `ended_reason` and nothing is live.            |
| `paused`        | The session row records `ended_reason` `peak_hours` (a provider peak-hours pause) and nothing is live. |
| `crashed`       | A real dead-PID or dead-host probe result — never inferred.               |
| `unknown`       | The shared activity read failed; see `notes`.                             |

`crashed` is deliberately part of this command's vocabulary beyond the
base five phases (`turn`, `between_turns`, `waiting`, `idle`,
`finished`): it is only ever set from a real dead-PID or dead-host
probe, so a script can assert on it without re-deriving a crash from
absence of evidence.

`paused` is issued only for an `ended_reason` of `peak_hours`; it renders
textually as "paused: provider peak hours". No resume time is derivable
from the session facts (an open wake marker would have kept the session
live in `between_turns`, not `ended`), so none is printed.

## JSON reference (`--json`)

| Field                        | Type                | Meaning                                                                     |
| ---------------------------- | ------------------- | --------------------------------------------------------------------------- |
| `session_id`                 | string              | The resolved session id.                                                    |
| `phase`                      | string              | The phase vocabulary above.                                                 |
| `process.alive`              | bool \| `"unknown"` | `true`/`false` only from a real probe result; otherwise the string.         |
| `process.driver_pid`         | int \| null         | The run-driver marker's PID; null when there is no live driver marker.      |
| `process.evidence`           | string              | The actual probe behind the verdict, in plain English.                      |
| `last_tool`                  | object \| null      | Newest tool call: `name`, `age_seconds`. Null when none ever / read failed. |
| `seconds_since_activity`     | int \| null         | Freshest of call-tree activity and lock heartbeat; null without evidence.   |
| `children.live`              | int                 | Live delegation rows in the tree.                                           |
| `children.total`             | int \| null         | Distinct descendant sessions ever; null when the walk failed.               |
| `children.held_questions`    | int \| null         | Pending child-question notices; null when the notice read failed.           |
| `batch.terminal`             | bool \| `"unknown"` | An incomplete batch read is the string, never proof of completion.          |
| `batch.roots[]`              | array               | `root_id`, `state` (cancelling/disappeared included), `held`.               |
| `exit_reason`                | string              | The recorded `ended_reason`, rendered verbatim; the text output adds a human label for known reasons ("paused: provider peak hours" for `peak_hours`); unknown reasons are never turned into `done`. Omitted when none. |
| `sources`                    | array               | Snapshots that contributed evidence, fixed order.                           |
| `notes`                      | array               | Degraded/failed reads; omitted when none.                                   |

## What this does NOT prove

- A recorded `ended_reason` is a database fact, **not** proof that a process
  physically exited: `process.alive` stays `"unknown"` when that is the only
  end evidence.
- An absent or released session lock does not mean the process exited; it
  means there is nothing to probe.

## Ended reason rendering

- `peak_hours` renders as the `paused` phase and the label
  "paused: provider peak hours" — a pause, never a completion.
- The known vocabulary (`end_turn`, `stop`, `tool_use`, `max_tokens`,
  `max_cost`, `canceled`, `error`, `unknown`, `queued`, `awaiting_answer`,
  `stalled`, `timeout`, `done`, `batch_fault`) renders as `done` (the
  historical whole-column behavior).
- Any other reason — one a newer rush wrote that this binary does not
  know — renders verbatim, never as `done`.
- A live status with a recorded `ended_reason` is annotated
  "(exit <reason>)" in `sessions list` and `sessions why` instead of
  hiding the recorded end.

## Examples

```sh
# Human summary
rush sessions status pr-42

# Machine snapshot for an orchestrator script
rush sessions status pr-42 --json | jq '.phase, .process.alive, .batch.terminal'

# Assert nothing is pending before a retry
rush sessions status 8a3f0c --json | jq 'select(.batch.terminal == true)'
```
