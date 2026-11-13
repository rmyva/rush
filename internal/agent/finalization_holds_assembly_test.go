package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// TestFinalizationHoldsAssemblyFailsClosedOnNilInputs proves the assembly
// point returns an error and nil facts for a nil install and for nil host
// facts, never a silent zero and never a panic.
//
// Revert-check: an assembly that passes a nil member through (or panics)
// hands the seal transaction an incomplete composite — the Sum combinator
// would then fail at read time anyway, so the early error is the honest
// construction-time verdict.
func TestFinalizationHoldsAssemblyFailsClosedOnNilInputs(t *testing.T) {
	facts, err := newFinalizationPhysicalHoldsFacts(nil, &finalizationSeamScopeStub{})
	require.Error(t, err)
	require.Nil(t, facts)
	facts, err = newFinalizationPhysicalHoldsFacts(&BatchInstall{}, nil)
	require.Error(t, err)
	require.Nil(t, facts)
}

// TestFinalizationHoldsAssemblySumsRootTrackedAndHostFacts proves the
// assembly sums all three sources per owner: one ledger-attributed batch
// root, the process-level tracked-tree count (zero here; nothing spawns in
// tests), and the host facts stub — and that an empty owner reads zero
// without invoking any member.
//
// Revert-check: an assembly that drops any source (or double-counts the
// host reader) lies to the seal gate in either direction; per-owner sums
// must be exactly root + tracked + host.
func TestFinalizationHoldsAssemblySumsRootTrackedAndHostFacts(t *testing.T) {
	caps := NewDiskProviderRegistry()
	binding := registerTestBinding(t, caps, "holds-assembly")
	store, _, releaseConstruction := newRootLeaseTestManager(t, caps, "holds-assembly-reg")
	defer releaseConstruction()
	require.NoError(t, store.acquire(jobbatch.ID("holds-assembly-root"), "owner", binding, "holds-assembly-reg"))
	install := &BatchInstall{RootLeases: store, DiskBindings: caps}
	hostStub := &finalizationSeamScopeStub{holds: 2}

	facts, err := newFinalizationPhysicalHoldsFacts(install, hostStub)
	require.NoError(t, err)
	require.NotNil(t, facts)

	n, err := facts.FinalizationPhysicalHoldsTx(t.Context(), nil, "owner")
	require.NoError(t, err)
	require.Equal(t, 3, n) // 1 root + 0 tracked + 2 host stub.

	n, err = facts.FinalizationPhysicalHoldsTx(t.Context(), nil, "other")
	require.NoError(t, err)
	require.Equal(t, 2, n) // Foreign owner: only the host stub holds.

	n, err = facts.FinalizationPhysicalHoldsTx(t.Context(), nil, "")
	require.NoError(t, err)
	require.Equal(t, 0, n)
}
