# Agent finalization (batch agents)

Agent finalization is an opt-in safety brake for batch runs: a batch agent's
final answer is drafted first as a provisional candidate, and it is sealed only
once the agent's children have finished, its reaction debts have settled, and
there is no cancellation and no open mutation. Clients see "done" only after
the seal.

## Enabling

```
options: {
    finalization: {
        enabled: true
    }
}
```

The legacy `rush.json` spelling works the same: `{"options":{"finalization":{"enabled":true}}}`.
Project scope overrides global scope per the normal deep-merge rules (later document wins per key).

## What it changes

When the flag is on, a batch agent's final message is first a provisional
DRAFT (candidate). It becomes final ("sealed") only when the agent's children
have finished, its reaction debts have settled, and there is no cancellation
and no open mutation at that moment; until then the agent waits and recomposes.
Clients do not see "done" before the seal.

## Rollback

Set `enabled: false` or remove the `finalization` block. Sealed messages
remain valid accepted messages; the legacy path ignores leftover bookkeeping.

## Status

Behind the flag, off by default. With the flag on, a root run binds a durable
finalization record, the end of its turn waits (polling durable facts every two
seconds) until its children have finished, its reaction debts are settled and
nothing is open, and only then seals; `rush run` buffers its output and the
HTTP API and WebSocket hold terminal events until the seal. With the flag off
nothing in this document applies and the runtime is unchanged.

### Known limitations (do not enable by default yet)

- **No model turn on recomposition.** The recomposition re-marks the provisional
  answer without a provider request and the reaction debts are settled by
  marking, so the child's result does not reach the model in that run: the
  sealed text stays the provisional one. The result is not lost (the completion
  stays pullable and the session's next turn shows it), but the ordinary
  `rush run` loop, which already waits for the child and runs one more model
  turn with its result, produces a better final answer than the flag-on path.
  A real compose turn is the next piece of work.
- **Cancel during the wait.** A run cancelled while it waits marks its record
  cancelled; the batch commit admission then refuses the terminal commits of the
  owner's still-live children, so their roots stay non-terminal until the
  process exits or the host is recovered, and the shutdown drain reports a
  fault.
- **Queued and replacement calls** inside one run do not pass through the root
  bind (they run the legacy path); `InterruptAndSend` during the wait leaves the
  record active until the next root run of the session retires it.
- **App-level `RunResult`** is not gated separately; the turn blocks until the
  seal, so the result is produced after it.
