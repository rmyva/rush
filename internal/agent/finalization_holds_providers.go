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
// acquire call sites); the ledger records the root's capability identity but
// NOT the owning session, so attribution is injected through ownerFor — the
// only place recording root-to-owner today is the CLI submit path
// (batch_cli_runtime.go, batchRunnerAdapter.owners). Execution-kind owners
// cover one accepted execution until physical disposal and carry no session
// attribution at all, so ANY live execution fails the provider closed.
// Call/construction/continuation kinds are capability scopes, not hold
// sources: construction pins are pinned-once app-level records that would
// block every finalization for the process lifetime, call owners are
// intra-invocation, and continuation is reserved and unused.
//
// Window safety: after the owner binds, the S1 fence refuses new
// reservations so batch roots only decrease, and cross-process roots stay
// covered by the durable batch_nodes projection read by the descendants
// facts reader.
//
// Locking: the function takes two separate snapshot acquisitions (caps
// owners, ledger entries), never nests them, and never holds either lock
// across attribution.
func NewFinalizationBatchRootHoldsProvider(
	leases *BatchRootLeaseStore,
	caps *DiskProviderRegistry,
	ownerFor func(jobbatch.ID) string,
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
		executions := 0
		for _, key := range keys {
			if key.kind == diskOwnerExecution {
				executions++
			}
		}
		if executions > 0 {
			return 0, fmt.Errorf("%w: %d execution owner(s) hold disk providers "+
				"with no execution-to-session index",
				ErrFinalizationHoldsUnattributable, executions)
		}
		leaseRoots := batchRootLeaseRoots(leases)
		count := 0
		for _, key := range keys {
			if key.kind != diskOwnerRoot {
				continue
			}
			root := jobbatch.ID(key.instance)
			if _, ok := leaseRoots[root]; !ok {
				return 0, fmt.Errorf("%w: root %s holds a capability owner "+
					"outside the lease ledger",
					ErrFinalizationHoldsUnattributable, root)
			}
			owner := ""
			if ownerFor != nil {
				owner = ownerFor(root)
			}
			if owner == "" {
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
// install.RootLeases, attribution from the runner adapter's in-process
// root-to-owner index. All fields are nil-guarded: a nil install or a
// dormant install (nil registries) fails closed at call time, never panics.
func NewFinalizationBatchRootHoldsProviderForInstall(install *BatchInstall) session.FinalizationHoldsProvider {
	var ownerFor func(jobbatch.ID) string
	if install != nil && install.RunnerHandle != nil {
		ownerFor = install.RunnerHandle.ownerFor
	}
	var leases *BatchRootLeaseStore
	var caps *DiskProviderRegistry
	if install != nil {
		leases, caps = install.RootLeases, install.DiskBindings
	}
	return NewFinalizationBatchRootHoldsProvider(leases, caps, ownerFor)
}

// NewFinalizationTrackedProcessHoldsProvider counts the tracked process
// trees owned by one session. It returns a session.FinalizationHoldsProvider.
//
// Attribution evidence: the tracked-process registries (trackedJobs on
// Windows, trackedGroups on Unix, both in package session) are
// package-level pid-keyed maps with NO owning session; the cliprovider
// registration sites (procgroup_windows.go and procgroup_unix.go) do not
// know the owning session either. Package agent cannot read package
// session's unexported state, so any count returned today would be
// session-blind and would mix every session's holds — the provider fails
// closed instead.
//
// W1 note: record the owning session at the cliprovider spawn sites into a
// session-keyed registry — a NEW file in package session, e.g. a
// map[sessionID]pids maintained by a session-aware wrapper around
// TrackProcessTree/RegisterSpawnedGroup plus an exported reader — then wire
// this constructor to that reader.
func NewFinalizationTrackedProcessHoldsProvider() session.FinalizationHoldsProvider {
	return func(ctx context.Context, ownerSessionID string) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("count finalization tracked process holds: %w", err)
		}
		if ownerSessionID == "" {
			return 0, nil
		}
		return 0, fmt.Errorf("%w: tracked process trees are pid-keyed in "+
			"package session with no owning-session attribution",
			ErrFinalizationHoldsUnattributable)
	}
}

// NewFinalizationHostLockHoldsProvider counts the host locks owned by one
// session. It returns a session.FinalizationHoldsProvider.
//
// Attribution evidence: the hold is the exclusive OS file lock on
// <dataDir>/hosts/<host_id>.lock (host_lock.go); locks belong to hosts, not
// sessions. The only session-to-host index is the durable session_drivers
// marker (session_driver.go), whose liveness verdict requires an OS file
// probe no database row attests. Counting this process's own host lock
// would deadlock every finalization, because the owner's driver holds its
// own lock and marker while TrySeal runs. The provider seam carries no
// database handle by design — the composite must not read the database — so
// the provider fails closed today.
//
// W1 note: either extend this constructor with a driver-probe delegate that
// reads the owner's marker row through TrySeal's own transaction (inline SQL
// on session_drivers — the sanctioned pattern for lane files) plus the
// non-blocking shared probe session.ProbeHostShared, excluding this
// process's own host ids via session.IsOwnHostID with unknown probe
// outcomes failing closed; or build a new package-session provider on
// HostStore with the same own-host exclusion.
func NewFinalizationHostLockHoldsProvider() session.FinalizationHoldsProvider {
	return func(ctx context.Context, ownerSessionID string) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("count finalization host lock holds: %w", err)
		}
		if ownerSessionID == "" {
			return 0, nil
		}
		return 0, fmt.Errorf("%w: host locks belong to hosts, not sessions, "+
			"and no session-to-host index with a non-blocking liveness probe "+
			"is reachable from this seam",
			ErrFinalizationHoldsUnattributable)
	}
}
