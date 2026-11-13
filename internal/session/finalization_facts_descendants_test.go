package session

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// descendantsTestCount reads the counter in one fresh transaction and
// discards the transaction, the way every test observes committed fixtures.
func descendantsTestCount(t *testing.T, env *finalizationSeamEnv, owner string) int {
	t.Helper()
	reader := NewFinalizationDescendantsReader()
	tx, err := env.conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	count, err := reader.FinalizationActiveDescendantsTx(t.Context(), tx, owner)
	require.NoError(t, err)
	return count
}

// descendantsTestRoot inserts one batch root row in the given state,
// bypassing BatchStore: Reserve cannot express several simultaneous live
// roots for one owner, and the fixtures need exact node-level control.
func descendantsTestRoot(t *testing.T, env *finalizationSeamEnv, rootID, ownerID, state string) {
	t.Helper()
	_, err := env.conn.ExecContext(env.ctx, `INSERT INTO batch_nodes
		(node_id, root_id, owner_session_id, host_id, ordinal, kind,
			tool_call_id, input_hash, mode, state, created_at, updated_at)
		VALUES (?, ?, ?, 'host', 0, 'batch', ?, ?, 'parallel', ?, 1, 1)`,
		rootID, rootID, ownerID, "call-"+rootID, "hash-"+rootID, state)
	require.NoError(t, err)
}

// descendantsTestTask inserts one task node under rootID; an empty
// childSession leaves the child binding NULL.
func descendantsTestTask(t *testing.T, env *finalizationSeamEnv, rootID, nodeID, ownerID string, ordinal int64, state, childSession string) {
	t.Helper()
	_, err := env.conn.ExecContext(env.ctx, `INSERT INTO batch_nodes
		(node_id, root_id, owner_session_id, host_id, parent_id, ordinal, kind,
			task_kind, state, child_session_id, created_at, updated_at)
		VALUES (?, ?, ?, 'host', ?, ?, 'task', 'test', ?, NULLIF(?, ''), 1, 1)`,
		nodeID, rootID, ownerID, rootID, ordinal, state, childSession)
	require.NoError(t, err)
}

// descendantsTestSetNodeState flips one node's state, the fixture lever for
// walking a node through its lifecycle.
func descendantsTestSetNodeState(t *testing.T, env *finalizationSeamEnv, nodeID, state string) {
	t.Helper()
	_, err := env.conn.ExecContext(env.ctx, `UPDATE batch_nodes SET state=? WHERE node_id=?`, state, nodeID)
	require.NoError(t, err)
}

// Revert-check: a reader that errors on unknown owners, misses an active
// record, or counts terminal phases fails the zero/one/many assertions.
func TestFinalizationDescendantsReaderCountsActiveRecords(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	require.Zero(t, descendantsTestCount(t, env, "owner"), "no descendants means zero")
	require.Zero(t, descendantsTestCount(t, env, "nobody"), "an unknown owner owns nothing, not an error")
	finalizationSeamLiveChild(t, env, "owner", "one")
	require.Equal(t, 1, descendantsTestCount(t, env, "owner"))
	finalizationSeamLiveChild(t, env, "owner", "two")
	require.Equal(t, 2, descendantsTestCount(t, env, "owner"))
}

// Revert-check: counting accepted, failed, cancelled or interrupted records
// keeps dead descendants in the counter and blocks the seal forever.
func TestFinalizationDescendantsReaderIgnoresTerminalPhases(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	child := finalizationSeamLiveChild(t, env, "owner", "term-child")
	require.Equal(t, 1, descendantsTestCount(t, env, "owner"))
	for _, phase := range []string{"accepted", "failed", "cancelled", "interrupted"} {
		finalizationSeamSetPhase(t, env, child.ID, phase)
		require.Zero(t, descendantsTestCount(t, env, "owner"), phase)
	}
	finalizationSeamSetPhase(t, env, child.ID, "executing")
	require.Equal(t, 1, descendantsTestCount(t, env, "owner"))
}

// Revert-check: a depth-one-only walk misses grandchildren and seals while
// a depth-two descendant still runs; counting a terminal middle record
// keeps dead work in the counter.
func TestFinalizationDescendantsReaderCountsNestedDescendants(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	depth1 := finalizationSeamLiveChild(t, env, "owner", "n1")
	finalizationSeamLiveChild(t, env, depth1.ID, "n2")
	require.Equal(t, 2, descendantsTestCount(t, env, "owner"))
	finalizationSeamLiveChild(t, env, "n2", "n3")
	require.Equal(t, 3, descendantsTestCount(t, env, "owner"), "depth-three descendants count")
	finalizationSeamSetPhase(t, env, "n2", "cancelled")
	require.Equal(t, 2, descendantsTestCount(t, env, "owner"),
		"a terminal middle record drops out without hiding the deeper one")
}

// Revert-check: a reader that treats pending or cancelling nodes as dead,
// or counts terminal node states, seals too early or never seals.
func TestFinalizationDescendantsReaderCountsLiveBatchNodes(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	descendantsTestRoot(t, env, "bn-a", "owner", "pending")
	require.Equal(t, 1, descendantsTestCount(t, env, "owner"))
	descendantsTestRoot(t, env, "bn-b", "owner", "starting")
	require.Equal(t, 2, descendantsTestCount(t, env, "owner"))
	for _, state := range []string{"pending", "starting", "running", "cancelling"} {
		descendantsTestSetNodeState(t, env, "bn-a", state)
		descendantsTestSetNodeState(t, env, "bn-b", state)
		require.Equal(t, 2, descendantsTestCount(t, env, "owner"), state)
	}
	for _, state := range []string{"completed", "failed", "cancelled", "timed_out", "interrupted", "skipped"} {
		descendantsTestSetNodeState(t, env, "bn-a", state)
		require.Equal(t, 1, descendantsTestCount(t, env, "owner"), state+" node is not live work")
	}
	descendantsTestSetNodeState(t, env, "bn-b", "completed")
	require.Zero(t, descendantsTestCount(t, env, "owner"))
}

// Revert-check: dropping the session-identity dedup counts a child twice
// (record + its driving node) and inflates the counter; subtracting nodes
// whose child has no active record would lose live work instead.
func TestFinalizationDescendantsReaderDeduplicatesRecordAndNode(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	child := finalizationSeamLiveChild(t, env, "owner", "dup")
	descendantsTestRoot(t, env, "dup-root", "owner", "pending")
	descendantsTestTask(t, env, "dup-root", "dup-root.1", "owner", 0, "running", child.ID)
	require.Equal(t, 2, descendantsTestCount(t, env, "owner"),
		"the record and its driving node are one descendant, not two")
	// A node whose child session carries no active record is live work of
	// its own and must not fold away.
	_, err := env.svc.CreateTaskSession(env.ctx, "lone", "owner", "child")
	require.NoError(t, err)
	descendantsTestTask(t, env, "dup-root", "dup-root.2", "owner", 1, "running", "lone")
	require.Equal(t, 3, descendantsTestCount(t, env, "owner"))
	finalizationSeamSetPhase(t, env, child.ID, "accepted")
	require.Equal(t, 3, descendantsTestCount(t, env, "owner"),
		"a terminal record leaves both nodes counted")
}

// Revert-check: an owner-agnostic counter mixes two owners' descendants in
// one shared database and refuses (or seals) for the wrong reason.
func TestFinalizationDescendantsReaderSeparatesOwners(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	_, err := env.svc.CreateWithID(env.ctx, "owner2", "second")
	require.NoError(t, err)
	finalizationSeamLiveChild(t, env, "owner", "own-a")
	finalizationSeamLiveChild(t, env, "owner2", "own-b")
	descendantsTestRoot(t, env, "root-a", "owner", "running")
	descendantsTestRoot(t, env, "root-b", "owner2", "running")
	require.Equal(t, 2, descendantsTestCount(t, env, "owner"))
	require.Equal(t, 2, descendantsTestCount(t, env, "owner2"))
}

// Revert-check: a reader that queries the pool instead of the passed
// transaction cannot see the uncommitted descendant and reads 0 here.
// SetMaxOpenConns(1) holds the only connection for the open transaction, so
// a neighbouring-connection probe cannot run concurrently; visibility of
// this transaction's own uncommitted writes is the discriminative check.
func TestFinalizationDescendantsReaderReadsInsideTransaction(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	reader := NewFinalizationDescendantsReader()
	_, err := reader.FinalizationActiveDescendantsTx(t.Context(), nil, "owner")
	require.Error(t, err, "a missing transaction is a broken caller, not an empty scope")
	tx, err := env.conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO sessions
		(id, title, parent_session_id, updated_at, created_at)
		VALUES ('tx-child', 'tx-child', 'owner', 1, 1)`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(),
		`INSERT INTO session_agent_finalizations (session_id, logical_call_id, generation)
		VALUES ('tx-child', 'tx-child-call', 1)`)
	require.NoError(t, err)
	count, err := reader.FinalizationActiveDescendantsTx(t.Context(), tx, "owner")
	require.NoError(t, err)
	require.Equal(t, 1, count, "the uncommitted descendant is visible inside tx only")
	require.NoError(t, tx.Rollback())
	require.Zero(t, descendantsTestCount(t, env, "owner"), "rolled-back work is gone")
}

// compositeScopeFixture pairs a candidateEnv with the composite scope
// reader wired to the real descendants source; the other three sources are
// zero stubs from the test kit.
type compositeScopeFixture struct {
	env    candidateEnv
	reader AgentFinalizationScopeReader
}

// newCompositeScopeFixture binds the reader machinery onto a seam env.
func newCompositeScopeFixture(t *testing.T, env *finalizationSeamEnv) compositeScopeFixture {
	t.Helper()
	messages := message.NewService(db.New(env.conn))
	writer, ok := messages.(message.FinalizationWriter)
	require.True(t, ok)
	finishReader, ok := messages.(message.FinalizationReader)
	require.True(t, ok)
	candidate := candidateEnv{
		conn: env.conn, store: env.store, messages: messages,
		writer: writer, reader: finishReader,
		id: AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "logical", Generation: 1},
	}
	scope := NewAgentFinalizationScopeReader(
		candidate.id.SessionID,
		NewFinalizationDescendantsReader(),
		&finalizationSeamScopeStub{}, &finalizationSeamScopeStub{}, &finalizationSeamScopeStub{},
	)
	return compositeScopeFixture{env: candidate, reader: scope}
}

// Revert-check: a composite wired to a descendants source that misses live
// work seals while a child still runs; one that invents live work never
// seals a quiet owner. Built like newReservedScopeEnv: the live root and
// the child record exist BEFORE the owner bind, which the S1 ingress fence
// requires (Reserve refuses for an already-bound scope).
func TestTrySealCompositeScopeReaderRefusesLiveDescendant(t *testing.T) {
	busy := newFinalizationSeamEnv(t)
	batchStoreTestReserve(t, NewBatchStore(busy.conn), busy.ctx)
	finalizationSeamLiveChild(t, busy, "owner", "busy-child")
	finalizationSeamBindOwner(t, busy, "owner", "logical")
	busyScope := newCompositeScopeFixture(t, busy)
	require.Equal(t, 4, descendantsTestCount(t, busy, "owner"),
		"one child record plus three live reserve nodes, deduplicated by session")
	_, presented := sealStoredCandidate(t, busyScope.env, 0, 1)
	before := sealStateDump(t, busy.conn)
	outcome, err := busyScope.env.store.TrySeal(t.Context(), busyScope.env.id, presented,
		busyScope.env.sealWriter(t), busyScope.reader)
	require.NoError(t, err)
	require.False(t, outcome.Sealed)
	require.False(t, outcome.Accepted)
	require.Equal(t, AgentFinalizationSealDescendantsActive, outcome.Reason)
	require.Equal(t, before, sealStateDump(t, busy.conn), "the refusal must not mutate durable state")

	quiet := newFinalizationSeamEnv(t)
	finalizationSeamBindOwner(t, quiet, "owner", "logical")
	quietScope := newCompositeScopeFixture(t, quiet)
	stored, presented := sealStoredCandidate(t, quietScope.env, 0, 1)
	outcome, err = quietScope.env.store.TrySeal(t.Context(), quietScope.env.id, presented,
		quietScope.env.sealWriter(t), quietScope.reader)
	require.NoError(t, err)
	require.True(t, outcome.Sealed, "a quiet owner with complete facts must seal")
	require.False(t, outcome.Accepted)
	require.Equal(t, "accepted", outcome.Record.Phase)
	require.True(t, outcome.Message.IsFinished())
	require.Equal(t, stored.ID, outcome.Message.ID)
}
