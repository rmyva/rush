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

Status: behind the flag, off by default. In this sprint the switch is being
wired to the implementation in stages, so the flag alone does not change
behavior yet; this page documents the contract the switch stands for.
