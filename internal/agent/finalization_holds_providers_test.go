package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// TestFinalizationBatchRootHoldsProviderCountsExactlyByRegistryFacts proves
// the provider counts exactly the LIVE root-kind capability owners of the
// requested owner session, ignoring the stale ledger entry a
// release-without-forget leaves behind, and never mixing another session's
// holds.
//
// Revert-check: a provider that counts ledger entries instead of live
// capability owners resurrects released roots and blocks sealing forever;
// one that mixes owners seals or refuses on another session's holds.
func TestFinalizationBatchRootHoldsProviderCountsExactlyByRegistryFacts(t *testing.T) {
	caps := NewDiskProviderRegistry()
	binding := registerTestBinding(t, caps, "holds-root")
	store, _, releaseConstruction := newRootLeaseTestManager(t, caps, "holds-root-reg")
	defer releaseConstruction()
	index := map[jobbatch.ID]string{}
	provider := NewFinalizationBatchRootHoldsProvider(store, caps, func(root jobbatch.ID) string {
		return index[root]
	})

	// Empty registries: a sound empty observation for every owner.
	n, err := provider(t.Context(), "A")
	require.NoError(t, err)
	require.Equal(t, 0, n)
	n, err = provider(t.Context(), "B")
	require.NoError(t, err)
	require.Equal(t, 0, n)

	root1 := jobbatch.ID("holds-root-a")
	require.NoError(t, store.acquire(root1, binding, "holds-root-reg"))
	index[root1] = "A"
	n, err = provider(t.Context(), "A")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = provider(t.Context(), "B")
	require.NoError(t, err)
	require.Equal(t, 0, n) // Foreign owner not mixed.

	root2 := jobbatch.ID("holds-root-b")
	require.NoError(t, store.acquire(root2, binding, "holds-root-reg"))
	index[root2] = "B"
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

// TestFinalizationBatchRootHoldsProviderFailsClosedOnUnattributedRoot proves
// one live root without a recorded owner poisons the whole count instead of
// being skipped, even for an owner with its own attributed root, and that a
// nil ownerFor behaves identically.
//
// Revert-check: a provider that skips unattributed roots returns a silent
// under-count and seals while the owner's root may still be live — exactly
// the silent zero the lane forbids.
func TestFinalizationBatchRootHoldsProviderFailsClosedOnUnattributedRoot(t *testing.T) {
	caps := NewDiskProviderRegistry()
	binding := registerTestBinding(t, caps, "holds-root")
	store, _, releaseConstruction := newRootLeaseTestManager(t, caps, "holds-root-reg")
	defer releaseConstruction()
	root := jobbatch.ID("holds-root-c")
	require.NoError(t, store.acquire(root, binding, "holds-root-reg"))

	index := map[jobbatch.ID]string{}
	provider := NewFinalizationBatchRootHoldsProvider(store, caps, func(r jobbatch.ID) string {
		return index[r]
	})
	n, err := provider(t.Context(), "A")
	require.ErrorIs(t, err, ErrFinalizationHoldsUnattributable)
	require.Equal(t, 0, n)

	// Nil ownerFor must fail closed the same way.
	providerNil := NewFinalizationBatchRootHoldsProvider(store, caps, nil)
	n, err = providerNil(t.Context(), "A")
	require.ErrorIs(t, err, ErrFinalizationHoldsUnattributable)
	require.Equal(t, 0, n)
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

	provider := NewFinalizationBatchRootHoldsProvider(store, caps, nil)
	n, err := provider(t.Context(), "A")
	require.ErrorIs(t, err, ErrFinalizationHoldsUnattributable)
	require.Equal(t, 0, n)
}

// TestFinalizationBatchRootHoldsProviderFailsClosedOnLiveExecution proves a
// live execution-kind owner (held until physical disposal, no session
// attribution) fails the provider closed even with no roots present.
//
// Revert-check: a provider that ignores execution owners misses the one
// accepted execution held until physical disposal and could seal under it.
func TestFinalizationBatchRootHoldsProviderFailsClosedOnLiveExecution(t *testing.T) {
	caps := NewDiskProviderRegistry()
	binding := registerTestBinding(t, caps, "holds-root")
	store, _, releaseConstruction := newRootLeaseTestManager(t, caps, "holds-root-reg")
	defer releaseConstruction()
	require.NoError(t, caps.acquire(diskOwnerKey{
		kind: diskOwnerExecution, instance: "holds-exec-1",
		bindingID: binding, registryID: "holds-root-reg",
	}))

	provider := NewFinalizationBatchRootHoldsProvider(store, caps, nil)
	n, err := provider(t.Context(), "A")
	require.ErrorIs(t, err, ErrFinalizationHoldsUnattributable)
	require.Equal(t, 0, n)
}

// TestFinalizationBatchRootHoldsProviderForInstallWiresAdapterIndex proves
// the install wiring reads attribution from the runner adapter's
// root-to-owner map and guards a nil install with the unreadable-registry
// sentinel instead of panicking.
//
// Revert-check: ForInstall wiring that reads any index other than the
// adapter's root-to-owner map, or that panics on a nil/dormant install,
// breaks W1's only production wiring path.
func TestFinalizationBatchRootHoldsProviderForInstallWiresAdapterIndex(t *testing.T) {
	caps := NewDiskProviderRegistry()
	binding := registerTestBinding(t, caps, "holds-root")
	store, _, releaseConstruction := newRootLeaseTestManager(t, caps, "holds-root-reg")
	defer releaseConstruction()
	root := jobbatch.ID("holds-root-install")
	require.NoError(t, store.acquire(root, binding, "holds-root-reg"))

	handle := &batchRunnerAdapter{owners: map[jobbatch.ID]string{root: "cli-owner"}}
	install := &BatchInstall{RunnerHandle: handle, RootLeases: store, DiskBindings: caps}
	provider := NewFinalizationBatchRootHoldsProviderForInstall(install)
	n, err := provider(t.Context(), "cli-owner")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = provider(t.Context(), "other")
	require.NoError(t, err)
	require.Equal(t, 0, n)

	nilProvider := NewFinalizationBatchRootHoldsProviderForInstall(nil)
	n, err = nilProvider(t.Context(), "x")
	require.ErrorIs(t, err, ErrFinalizationHoldsRegistryUnreadable)
	require.Equal(t, 0, n)
}

// TestFinalizationTrackedProcessHoldsProviderFailsClosed proves the tracked
// process provider returns zero for an empty owner and refuses any named
// owner, and honours a cancelled context.
//
// Revert-check: any future change that makes this provider return a global
// session-blind count of trackedJobs/trackedGroups mixes every session's
// holds; the refusal must stay until session attribution exists (W1).
func TestFinalizationTrackedProcessHoldsProviderFailsClosed(t *testing.T) {
	provider := NewFinalizationTrackedProcessHoldsProvider()
	n, err := provider(t.Context(), "")
	require.NoError(t, err)
	require.Equal(t, 0, n)
	n, err = provider(t.Context(), "owner")
	require.ErrorIs(t, err, ErrFinalizationHoldsUnattributable)
	require.Equal(t, 0, n)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	n, err = provider(ctx, "owner")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 0, n)
}

// TestFinalizationHostLockHoldsProviderFailsClosed proves the host lock
// provider returns zero for an empty owner and refuses any named owner, and
// honours a cancelled context.
//
// Revert-check: counting async_hosts rows or this process's own host lock
// would either lie about liveness or deadlock the owner's own driver (its
// lock and marker are present during its own TrySeal); the refusal must
// stay until a marker-plus-TryLock-probe attribution is wired (W1).
func TestFinalizationHostLockHoldsProviderFailsClosed(t *testing.T) {
	provider := NewFinalizationHostLockHoldsProvider()
	n, err := provider(t.Context(), "")
	require.NoError(t, err)
	require.Equal(t, 0, n)
	n, err = provider(t.Context(), "owner")
	require.ErrorIs(t, err, ErrFinalizationHoldsUnattributable)
	require.Equal(t, 0, n)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	n, err = provider(ctx, "owner")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 0, n)
}
