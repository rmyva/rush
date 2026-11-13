package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
)

// ErrFinalizationHoldsUnattributable reports a live physical hold whose
// owning session cannot be proven from the in-process registries, so the
// per-owner count is uncomputable and the provider fails closed.
var ErrFinalizationHoldsUnattributable = errors.New(
	"finalization physical hold is not attributable to the owner session " +
		"from the in-process registries")

// ErrFinalizationHoldsRegistryUnreadable reports provider wiring whose
// registries are nil, so the source cannot be observed at all.
var ErrFinalizationHoldsRegistryUnreadable = errors.New(
	"finalization physical holds registry is unreadable")

// diskOwnerKeysOfKind snapshots the manager's owner keys of the given kinds
// under one lock acquisition, releasing the lock before attribution runs.
func diskOwnerKeysOfKind(m *diskCapabilityManager, kinds ...diskOwnerKind) []diskOwnerKey {
	if m == nil {
		return nil
	}
	selected := make(map[diskOwnerKind]struct{}, len(kinds))
	for _, kind := range kinds {
		selected[kind] = struct{}{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]diskOwnerKey, 0, len(m.owners))
	for key := range m.owners {
		if _, ok := selected[key.kind]; ok {
			keys = append(keys, key)
		}
	}
	return keys
}

// batchRootLeaseRoots snapshots the lease ledger's root ids under one lock
// acquisition.
func batchRootLeaseRoots(s *batchRootLeaseStore) map[jobbatch.ID]struct{} {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	roots := make(map[jobbatch.ID]struct{}, len(s.entries))
	for root := range s.entries {
		roots[root] = struct{}{}
	}
	return roots
}

// NewFinalizationBatchRootHoldsProvider counts the batch-root physical holds
// owned by one session from the live capability owner set and the lease
// ledger. It returns a session.FinalizationHoldsProvider.
//
// Attribution evidence: production root-kind owners are created only via
// batchRootLeaseStore.acquire (batch_ingress.go and batch_model_control.go
// acquire call sites, plus the CLI submit path in batch_cli_runtime.go),
// and acquire now records the OWNING SESSION in the ledger entry itself.
// That ledger attribution replaced the old CLI-only runner-adapter index
// (batchRunnerAdapter.owners): ownerForRoot is the single attribution
// reader, so model-control and ingress roots attribute the same way CLI
// roots always did.
//
// Execution over-count: an execution-kind owner carries no session
// attribution (there is no execution-to-session index), so every live
// execution is counted as a hold of the QUERIED owner too — a deliberate
// conservative over-count. It is the safe side: the seal defers until the
// physical dispose completes, then the count drops. Execution lifecycle:
// the engine releases execution owners via the Disposer path
// (internal/jobbatch/runner_control.go maybeDispose after terminate plus
// giveUp plus zero pins -> batchBindingExecution.Dispose -> the once
// release), NOT immediately at node-terminal; a dispose TIMEOUT or panic
// intentionally retains the owner (batch_exec.go), so the hold count stays
// elevated until the process restarts. That is truthful — physical
// disposal did not complete — never a silent zero. Call/construction/
// continuation kinds are capability scopes, not hold sources: construction
// pins are pinned-once app-level records that would block every
// finalization for the process lifetime, call owners are intra-invocation,
// and continuation is reserved and unused.
//
// Window safety: after the owner binds, the S1 fence refuses new
// reservations so batch roots only decrease, and cross-process roots stay
// covered by the durable batch_nodes projection read by the descendants
// facts reader.
//
// Locking: the function takes two separate snapshot acquisitions (caps
// owners, ledger entries) plus one ownerForRoot read, never nests them,
// and never holds either lock across attribution.
func NewFinalizationBatchRootHoldsProvider(
	leases *BatchRootLeaseStore,
	caps *DiskProviderRegistry,
) session.FinalizationHoldsProvider {
	return func(ctx context.Context, ownerSessionID string) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("count finalization batch root holds: %w", err)
		}
		if caps == nil || leases == nil {
			return 0, fmt.Errorf("%w: batch root hold registries are nil",
				ErrFinalizationHoldsRegistryUnreadable)
		}
		if ownerSessionID == "" {
			// Empty owner deterministically means zero owned holds.
			return 0, nil
		}
		keys := diskOwnerKeysOfKind(caps, diskOwnerRoot, diskOwnerExecution)
		if len(keys) == 0 {
			// A sound empty observation for every owner: the live owner
			// set holds no root or execution keys at all.
			return 0, nil
		}
		leaseRoots := batchRootLeaseRoots(leases)
		count := 0
		for _, key := range keys {
			if key.kind == diskOwnerExecution {
				// CONSERVATIVE OVER-COUNT: a live execution of ANY owner
				// counts as a hold of the queried owner too; there is no
				// execution-to-session index, so this is the safe side —
				// the seal defers until physical dispose, then passes.
				count++
				continue
			}
			if key.kind != diskOwnerRoot {
				continue
			}
			root := jobbatch.ID(key.instance)
			if _, ok := leaseRoots[root]; !ok {
				return 0, fmt.Errorf("%w: root %s holds a capability owner "+
					"outside the lease ledger",
					ErrFinalizationHoldsUnattributable, root)
			}
			owner := leases.ownerForRoot(root)
			if owner == "" {
				// No owning session recorded — impossible in production
				// (acquire refuses an empty owner); the check stays as a
				// fail-closed guard against a forgotten entry.
				return 0, fmt.Errorf("%w: root %s has no owning session recorded",
					ErrFinalizationHoldsUnattributable, root)
			}
			if owner == ownerSessionID {
				count++
			}
		}
		return count, nil
	}
}

// NewFinalizationBatchRootHoldsProviderForInstall wires the provider to one
// installed batch runtime: live holds come from install.DiskBindings and
// attribution from install.RootLeases — the lease ledger the install's
// acquire call sites record owners into. RunnerHandle and any ownerFor
// delegate are no longer consulted. All fields are nil-guarded: a nil
// install or a dormant install (nil registries) fails closed at call time,
// never panics.
func NewFinalizationBatchRootHoldsProviderForInstall(install *BatchInstall) session.FinalizationHoldsProvider {
	var leases *BatchRootLeaseStore
	var caps *DiskProviderRegistry
	if install != nil {
		leases, caps = install.RootLeases, install.DiskBindings
	}
	return NewFinalizationBatchRootHoldsProvider(leases, caps)
}

// NewFinalizationTrackedProcessHoldsProvider counts the tracked process
// trees as a PROCESS-level conservative over-approximation: the pid-keyed
// registries in package session (trackedJobs on Windows, trackedGroups
// elsewhere) carry no owning session, and the cliprovider spawn sites have
// no session index, so ANY live tracked tree defers every owner's seal
// until physical teardown, then the count returns to baseline. It returns
// a session.FinalizationHoldsProvider.
//
// No-self-deadlock guarantee for the owner's own current CLI stream, with
// code evidence: the spawn sites are
// internal/agent/cliprovider/provider_stream.go:424 (PTY) and :626 (pipe);
// unregistration is provider_stream.go:663 session.UntrackProcessTree
// (childPid) inside visibleCleanup, deferred in the stream generator at
// :697, so it runs when the stream generator returns — before the turn
// pipeline observes stream completion. TrySeal only runs when a candidate
// is recorded (internal/session/finalization_seal.go refuses
// AgentFinalizationSealNoCandidate otherwise), and the candidate is
// persisted only after the provider step fully consumed the stream
// (internal/agent/agent_turn_step.go persistFinalizationCandidate ~:827).
// During wait no stream runs; during compose the seal follows that compose
// turn's own completed stream. Therefore the owner's own current CLI
// stream is always unregistered by seal time — no permanent self-block.
func NewFinalizationTrackedProcessHoldsProvider() session.FinalizationHoldsProvider {
	return func(ctx context.Context, ownerSessionID string) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("count finalization tracked process holds: %w", err)
		}
		if ownerSessionID == "" {
			// Empty owner deterministically means zero owned holds.
			return 0, nil
		}
		return session.CountFinalizationTrackedProcessHolds(), nil
	}
}
