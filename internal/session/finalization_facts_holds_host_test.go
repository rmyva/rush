package session

import (
	"context"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// registerForeignLiveHostHandle mirrors registerForeignLiveHost but returns
// the *HostIdentity handle, so a leg can close the host mid-test and flip
// its probe verdict from alive to dead. The host is dropped from the
// process-wide own-id registry, so every probe of it takes the
// cross-process shared-probe path exactly as another process's host would.
func registerForeignLiveHostHandle(t *testing.T, ctx context.Context, q *db.Queries, dataDir string) *HostIdentity {
	t.Helper()
	h, err := RegisterHost(ctx, dataDir, 4321, "foreign", nodeStore(q))
	require.NoError(t, err)
	unmarkOwnHostID(h.ID)
	t.Cleanup(func() { _ = h.Close(ctx, nodeStore(q)) })
	return h
}

// readHostHoldsInTx opens a real writer transaction on one store's
// single-connection pool and reads the facts through it, rolling back so
// the pooled connection is always freed. Transactions on one store are
// never held concurrently.
func readHostHoldsInTx(t *testing.T, st *HostStore, facts FinalizationPhysicalHoldsFacts, owner string) (int, error) {
	t.Helper()
	tx, err := st.sqlDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	return facts.FinalizationPhysicalHoldsTx(t.Context(), tx, owner)
}

// TestFinalizationHostDriverHoldsFactsMatrix covers the host-driver hold
// definition per durable session_drivers marker row: no marker / own host /
// genuinely foreign live / the same row after its host closed / a host
// whose lock path is a directory (undecidable liveness fails closed).
//
// Revert-check: (a) a host facts reader that counts a marker row without
// probing liveness reports a hold for a dead or absent host's row (the
// closed-host leg fails on 1 != 0); (b) one that drops the own-host
// exemption reports the owner's own driver as a hold and deadlocks every
// seal (the own-host leg fails on 1 != 0); (c) one that folds an
// undecidable probe into dead or alive seals a session whose driver it
// cannot attest (the directory leg fails on err == nil).
func TestFinalizationHostDriverHoldsFactsMatrix(t *testing.T) {
	t.Parallel()
	stores, q, ctx, dataDir := nStores(t, 2, "none", "own", "foreign-live", "unknown")
	reader := NewFinalizationHostDriverHoldsFacts(dataDir)

	n, err := readHostHoldsInTx(t, stores[0], reader, "none")
	require.NoError(t, err)
	require.Zero(t, n, "no marker row is no hold")

	// A marker naming a same-process sibling store's host is the owner's
	// own process: IsOwnHostID excludes it without any probe.
	require.NoError(t, stores[1].ClaimSessionDriver(ctx, "own"))
	n, err = readHostHoldsInTx(t, stores[0], reader, "own")
	require.NoError(t, err)
	require.Zero(t, n, "the own host driving its own session is not a hold")

	// A genuinely foreign live host: the shared probe attests its really
	// held lock, so exactly one hold.
	h := registerForeignLiveHostHandle(t, ctx, q, dataDir)
	_, err = q.InsertSessionDriver(ctx, db.InsertSessionDriverParams{
		SessionID: "foreign-live", HostID: h.ID, Pid: 4321, ClaimedAt: 1,
	})
	require.NoError(t, err)
	n, err = readHostHoldsInTx(t, stores[0], reader, "foreign-live")
	require.NoError(t, err)
	require.Equal(t, 1, n, "one foreign live driver row is exactly one hold")

	// The same row stops being a hold once the foreign host's lock is
	// released and the file removed: the probe reads dead.
	require.NoError(t, h.Close(ctx, nodeStore(q)))
	n, err = readHostHoldsInTx(t, stores[0], reader, "foreign-live")
	require.NoError(t, err)
	require.Zero(t, n, "the row of a provably dead host is no hold")

	// A directory at the lock path: the probe cannot decide, the facts
	// must refuse instead of guessing.
	require.NoError(t, os.MkdirAll(HostLockPath(dataDir, "unknown-host"), 0o755))
	_, err = q.InsertSessionDriver(ctx, db.InsertSessionDriverParams{
		SessionID: "unknown", HostID: "unknown-host", Pid: 4242, ClaimedAt: 1,
	})
	require.NoError(t, err)
	n, err = readHostHoldsInTx(t, stores[0], reader, "unknown")
	require.ErrorIs(t, err, ErrFinalizationPhysicalHoldsUnreadable)
	require.Zero(t, n)
}

// TestFinalizationHostDriverHoldsFactsRequiresTransaction: the facts read
// the marker through the passed seal transaction, so a nil tx is a broken
// reader and an error, never a silent 0.
//
// Revert-check: a reader that silently returns (0, nil) on a nil tx would
// seal a session whose driver row it never observed.
func TestFinalizationHostDriverHoldsFactsRequiresTransaction(t *testing.T) {
	t.Parallel()
	_, _, _, dataDir := nStores(t, 1, "owner")
	reader := NewFinalizationHostDriverHoldsFacts(dataDir)
	n, err := reader.FinalizationPhysicalHoldsTx(t.Context(), nil, "owner")
	require.Error(t, err)
	require.Contains(t, err.Error(), "require a transaction")
	require.Zero(t, n)
}

// TestFinalizationHostDriverHoldsFactsEmptyOwnerIsZero: an unfamiliar or
// empty owner is zero facts, never an error (seam contract).
//
// Revert-check: a reader that errors on the empty owner turns every
// scope-less read into a spurious reader failure.
func TestFinalizationHostDriverHoldsFactsEmptyOwnerIsZero(t *testing.T) {
	t.Parallel()
	stores, _, _, dataDir := nStores(t, 1, "owner")
	reader := NewFinalizationHostDriverHoldsFacts(dataDir)
	tx, err := stores[0].sqlDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	n, err := reader.FinalizationPhysicalHoldsTx(t.Context(), tx, "")
	require.NoError(t, err)
	require.Zero(t, n)
}

// TestFinalizationHostDriverHoldsFactsRespectsCanceledContext: a cancelled
// context stops the read before any query, with the context error wrapped.
//
// Revert-check: a reader that ignores ctx runs queries inside a seal
// transaction whose owner already cancelled.
func TestFinalizationHostDriverHoldsFactsRespectsCanceledContext(t *testing.T) {
	t.Parallel()
	stores, _, _, dataDir := nStores(t, 1, "owner")
	reader := NewFinalizationHostDriverHoldsFacts(dataDir)
	tx, err := stores[0].sqlDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	n, err := reader.FinalizationPhysicalHoldsTx(canceled, tx, "owner")
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, n)
}
