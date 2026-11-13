package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestFinalizationBatchRootHoldsProviderCountsExactlyByRegistryFacts proves
// the provider counts exactly the LIVE root-kind capability owners whose
// lease-ledger entry names the requested owner session, ignoring the stale
// ledger entry a release-without-forget leaves behind, and never mixing
// another session's holds.
//
// Revert-check: a provider that counts ledger entries instead of live
// capability owners resurrects released roots and blocks sealing forever;
// one that mixes owners seals or refuses on another session's holds.
func TestFinalizationBatchRootHoldsProviderCountsExactlyByRegistryFacts(t *testing.T) {
	caps := NewDiskProviderRegistry()
	binding := registerTestBinding(t, caps, "holds-root")
	store, _, releaseConstruction := newRootLeaseTestManager(t, caps, "holds-root-reg")
	defer releaseConstruction()
	provider := NewFinalizationBatchRootHoldsProvider(store, caps)

	// Empty registries: a sound empty observation for every owner.
	n, err := provider(t.Context(), "A")
	require.NoError(t, err)
	require.Equal(t, 0, n)
	n, err = provider(t.Context(), "B")
	require.NoError(t, err)
	require.Equal(t, 0, n)

	root1 := jobbatch.ID("holds-root-a")
	require.NoError(t, store.acquire(root1, "A", binding, "holds-root-reg"))
	n, err = provider(t.Context(), "A")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = provider(t.Context(), "B")
	require.NoError(t, err)
	require.Equal(t, 0, n) // Foreign owner not mixed.

	root2 := jobbatch.ID("holds-root-b")
	require.NoError(t, store.acquire(root2, "B", binding, "holds-root-reg"))
	n, err = provider(t.Context(), "A")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = provider(t.Context(), "B")
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// Release without forget leaves a stale ledger entry; the live caps
	// owner is gone, so A's count must fall to exactly zero.
	store.release(root1, binding, "holds-root-reg")
	n, err = provider(t.Context(), "A")
	require.NoError(t, err)
	require.Equal(t, 0, n)
	n, err = provider(t.Context(), "B")
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

// TestFinalizationBatchRootHoldsProviderFailsClosedOnRootOutsideLeaseLedger
// proves a root-kind capability owner that no lease ledger entry backs fails
// the provider closed.
//
// Revert-check: counting bare capability owners without the ledger
// cross-check would count holds the ledger never accepted, and silently
// skipping them would miss real holds; the refusal keeps both loud.
func TestFinalizationBatchRootHoldsProviderFailsClosedOnRootOutsideLeaseLedger(t *testing.T) {
	caps := NewDiskProviderRegistry()
	store, _, releaseConstruction := newRootLeaseTestManager(t, caps, "holds-root-reg")
	defer releaseConstruction()
	require.NoError(t, caps.acquire(diskOwnerKey{
		kind: diskOwnerRoot, instance: "holds-ghost", registryID: "holds-root-reg",
	}))

	provider := NewFinalizationBatchRootHoldsProvider(store, caps)
	n, err := provider(t.Context(), "A")
	require.ErrorIs(t, err, ErrFinalizationHoldsUnattributable)
	require.Equal(t, 0, n)
}

// TestFinalizationBatchRootHoldsProviderFailsClosedOnUnattributedRoot
// proves one live root-kind capability owner whose ledger entry was
// FORGOTTEN (a state the adopt watcher can reach by racing a settlement)
// poisons the whole count instead of being skipped — exactly the state the
// ledger-membership and owner checks exist for, since acquire now refuses
// an empty owner and a live root always has an entry otherwise.
//
// Revert-check: a provider that skips forgotten-entry roots returns a
// silent under-count and seals while the owner's root may still be live —
// exactly the silent zero the lane forbids.
func TestFinalizationBatchRootHoldsProviderFailsClosedOnUnattributedRoot(t *testing.T) {
	caps := NewDiskProviderRegistry()
	binding := registerTestBinding(t, caps, "holds-root")
	store, _, releaseConstruction := newRootLeaseTestManager(t, caps, "holds-root-reg")
	defer releaseConstruction()
	root := jobbatch.ID("holds-root-c")
	require.NoError(t, store.acquire(root, "A", binding, "holds-root-reg"))
	// The adopt watcher can forget the ledger entry while the caps owner is
	// still live; the provider must refuse, not skip.
	store.forget(root)

	provider := NewFinalizationBatchRootHoldsProvider(store, caps)
	n, err := provider(t.Context(), "A")
	require.ErrorIs(t, err, ErrFinalizationHoldsUnattributable)
	require.Equal(t, 0, n)
}

// TestFinalizationBatchRootHoldsProviderCountsLiveExecutionConservatively
// proves a live execution-kind owner (held until physical disposal, no
// session attribution) is counted CONSERVATIVELY for the queried owner —
// one live execution of any owner yields a count of 1, not an error — and
// drops back to zero once the execution owner releases.
//
// Revert-check: a provider that ignores execution owners misses the one
// accepted execution held until physical disposal and could seal under it;
// one that refuses instead of over-counting would deadlock every seal for
// the lifetime of any unrelated live execution.
func TestFinalizationBatchRootHoldsProviderCountsLiveExecutionConservatively(t *testing.T) {
	caps := NewDiskProviderRegistry()
	binding := registerTestBinding(t, caps, "holds-root")
	store, _, releaseConstruction := newRootLeaseTestManager(t, caps, "holds-root-reg")
	defer releaseConstruction()
	execKey := diskOwnerKey{
		kind: diskOwnerExecution, instance: "holds-exec-1",
		bindingID: binding, registryID: "holds-root-reg",
	}
	require.NoError(t, caps.acquire(execKey))

	provider := NewFinalizationBatchRootHoldsProvider(store, caps)
	n, err := provider(t.Context(), "A")
	require.NoError(t, err)
	require.Equal(t, 1, n) // Conservative over-count for any owner.

	caps.release(execKey)
	n, err = provider(t.Context(), "A")
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

// TestFinalizationBatchRootHoldsProviderForInstallWiresLeaseLedger proves
// the install wiring attributes from the LEASE LEDGER (the runner adapter's
// owner map is left empty to prove it is no longer consulted) and guards a
// nil install with the unreadable-registry sentinel instead of panicking.
//
// Revert-check: ForInstall wiring that reads any index other than the lease
// ledger, or that panics on a nil/dormant install, breaks W1's only
// production wiring path.
func TestFinalizationBatchRootHoldsProviderForInstallWiresLeaseLedger(t *testing.T) {
	caps := NewDiskProviderRegistry()
	binding := registerTestBinding(t, caps, "holds-root")
	store, _, releaseConstruction := newRootLeaseTestManager(t, caps, "holds-root-reg")
	defer releaseConstruction()
	root := jobbatch.ID("holds-root-install")
	require.NoError(t, store.acquire(root, "cli-owner", binding, "holds-root-reg"))

	// The adapter owners map stays empty: attribution must come from the
	// ledger, never from the runner adapter.
	install := &BatchInstall{RunnerHandle: &batchRunnerAdapter{}, RootLeases: store, DiskBindings: caps}
	provider := NewFinalizationBatchRootHoldsProviderForInstall(install)
	n, err := provider(t.Context(), "cli-owner")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = provider(t.Context(), "other")
	require.NoError(t, err)
	require.Equal(t, 0, n)

	// An owner-only CLI-style entry (no caps owner) adds nothing to the
	// count: the provider stays at 1.
	root2 := jobbatch.ID("holds-root-cli")
	require.NoError(t, store.acquire(root2, "cli-owner", "", ""))
	n, err = provider(t.Context(), "cli-owner")
	require.NoError(t, err)
	require.Equal(t, 1, n)

	nilProvider := NewFinalizationBatchRootHoldsProviderForInstall(nil)
	n, err = nilProvider(t.Context(), "x")
	require.ErrorIs(t, err, ErrFinalizationHoldsRegistryUnreadable)
	require.Equal(t, 0, n)
}

// TestFinalizationTrackedProcessHoldsProviderFailsClosed proves the tracked
// process provider returns zero for an empty owner and returns exactly the
// process-level tracked-tree count for a named owner (no spawning), and
// honours a cancelled context.
//
// Revert-check: a provider that refuses a named owner (or invents any other
// number than session.CountFinalizationTrackedProcessHolds) breaks the
// conservative process-level over-approximation W1 wires into the assembly.
func TestFinalizationTrackedProcessHoldsProviderFailsClosed(t *testing.T) {
	provider := NewFinalizationTrackedProcessHoldsProvider()
	n, err := provider(t.Context(), "")
	require.NoError(t, err)
	require.Equal(t, 0, n)

	baseline := session.CountFinalizationTrackedProcessHolds()
	n, err = provider(t.Context(), "owner")
	require.NoError(t, err)
	require.Equal(t, baseline, n)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	n, err = provider(ctx, "owner")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 0, n)
}
