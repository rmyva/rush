package session

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// newHostHoldsSealEnv builds the L2c seal integration environment: a fully
// migrated temp database (db.Connect applies migrations), the "owner"
// session seeded, and the finalization store, message service, writer and
// reader bound to that single connection -- the same shape newCandidateEnv
// builds, plus the data dir the host-driver facts probe.
func newHostHoldsSealEnv(t *testing.T) (candidateEnv, string) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	require.NoError(t, seedSession(ctx, db.New(conn), "owner"))
	conn.SetMaxOpenConns(1)
	messages := message.NewService(db.New(conn))
	store := NewAgentFinalizationStore(conn)
	id := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "logical", Generation: 1}
	require.NoError(t, store.Bind(ctx, id))
	writer, ok := messages.(message.FinalizationWriter)
	require.True(t, ok)
	reader, ok := messages.(message.FinalizationReader)
	require.True(t, ok)
	return candidateEnv{
		conn: conn, store: store, messages: messages,
		writer: writer, reader: reader, id: id,
	}, dataDir
}

// TestTrySealHostDriverHoldsRefusalThenSeal wires the production holds
// Sum (providers + host-driver facts) into a real TrySeal. Leg 1: a
// genuinely foreign live host drives the owner session, so the seal must
// refuse with AgentFinalizationSealPhysicalHolds. Leg 2: the same env, the
// same presented candidate, the foreign host's lock gone (Close releases
// the OS lock and removes the file) -- the identical reader now observes
// zero holds and the seal commits.
//
// Revert-check: a Sum or host facts that silently return 0 while a foreign
// live driver row exists seals under a physical hold (leg 1 fails on
// Sealed == true); a Sum that never returns 0 refuses forever (leg 2 fails
// on Sealed == false).
func TestTrySealHostDriverHoldsRefusalThenSeal(t *testing.T) {
	e, dataDir := newHostHoldsSealEnv(t)
	ctx := context.Background()
	q := db.New(e.conn)

	holdsFacts := NewFinalizationPhysicalHoldsFactsSum(
		NewFinalizationPhysicalHoldsFactsFromProviders((&holdsProviderStub{count: 0}).provider),
		NewFinalizationHostDriverHoldsFacts(dataDir),
	)
	reader := NewAgentFinalizationScopeReader(
		"owner", &finalizationSeamScopeStub{}, NewFinalizationQueuedCallsFacts(),
		holdsFacts, &finalizationSeamScopeStub{})

	// A genuinely foreign live host drives the owner session: its lock is
	// really held by this test process, but the host is not marked as this
	// process's own, so the facts must take the shared-probe path.
	h, err := RegisterHost(ctx, dataDir, 4321, "foreign", nodeStore(q))
	require.NoError(t, err)
	unmarkOwnHostID(h.ID)
	t.Cleanup(func() { _ = h.Close(ctx, nodeStore(q)) })
	n, err := q.InsertSessionDriver(ctx, db.InsertSessionDriverParams{
		SessionID: e.id.SessionID, HostID: h.ID, Pid: 4321, ClaimedAt: 1,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	require.False(t, IsOwnHostID(h.ID),
		"precondition: the own-host exemption must not answer for this host")

	_, presented := sealStoredCandidate(t, e, 0, 1)

	// Leg 1: one foreign live driver is one physical hold, and one hold
	// refuses the seal.
	outcome, err := e.store.TrySeal(t.Context(), e.id, presented, e.sealWriter(t), reader)
	require.NoError(t, err)
	require.False(t, outcome.Sealed)
	require.False(t, outcome.Accepted)
	require.Equal(t, AgentFinalizationSealPhysicalHolds, outcome.Reason)

	// Leg 2: the foreign host closes, its lock file disappears, the same
	// marker row now reads dead through the same reader, and the seal
	// commits atomically.
	require.NoError(t, h.Close(ctx, nodeStore(q)))
	outcome, err = e.store.TrySeal(t.Context(), e.id, presented, e.sealWriter(t), reader)
	require.NoError(t, err)
	require.True(t, outcome.Sealed)
	require.Equal(t, AgentFinalizationSealNone, outcome.Reason)
	require.Equal(t, "accepted", outcome.Record.Phase)
	require.True(t, outcome.Message.IsFinished())
}
