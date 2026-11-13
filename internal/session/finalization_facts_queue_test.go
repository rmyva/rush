package session

import (
	"database/sql"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// finalizationFactsInsertQueuePair stages the enqueue pair with plain SQL,
// mirroring the two migrated schemas; provenance is not this reader's
// concern, it only needs the rows present.
func finalizationFactsInsertQueuePair(t *testing.T, conn *sql.DB, sessionID, rowID string) {
	t.Helper()
	now := time.Now().Unix()
	_, err := conn.ExecContext(t.Context(), `INSERT INTO session_run_queue
		(id, session_id, call_data, status, created_at, updated_at) VALUES (?, ?, '{}', 'pending', ?, ?)`,
		rowID, sessionID, now, now)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO session_agent_finalization_continuations
		(row_id, session_id, logical_call_id, payload_hash, identities, ancestry) VALUES (?, ?, 'facts-call', 'facts-hash', '[]', '[]')`,
		rowID, sessionID)
	require.NoError(t, err)
}

// finalizationFactsInsertOrphanContinuation stages a continuation row with no
// matching run-queue row.
func finalizationFactsInsertOrphanContinuation(t *testing.T, conn *sql.DB, sessionID, rowID string) {
	t.Helper()
	_, err := conn.ExecContext(t.Context(), `INSERT INTO session_agent_finalization_continuations
		(row_id, session_id, logical_call_id, payload_hash, identities, ancestry) VALUES (?, ?, 'facts-call', 'facts-hash', '[]', '[]')`,
		rowID, sessionID)
	require.NoError(t, err)
}

// finalizationFactsQueuedCount runs the production queued-calls reader inside
// a transaction on any connection and rolls the transaction back.
func finalizationFactsQueuedCount(t *testing.T, conn *sql.DB, owner string) int {
	t.Helper()
	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	count, err := NewFinalizationQueuedCallsFacts().FinalizationQueuedCallsTx(t.Context(), tx, owner)
	require.NoError(t, err)
	return count
}

// newQueuedScopeSealEnv builds a seal environment with an optional
// pre-binding queue pair, mirroring newReservedScopeEnv: the S1 fence
// refuses scope work for an already-bound owner, so the rows go in BEFORE
// store.Bind.
func newQueuedScopeSealEnv(t *testing.T, queued bool) candidateEnv {
	t.Helper()
	_, conn, ctx := batchStoreTestDB(t)
	if queued {
		finalizationFactsInsertQueuePair(t, conn, "owner", "queued-seal")
	}
	messages := message.NewService(db.New(conn))
	store := NewAgentFinalizationStore(conn)
	id := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "logical", Generation: 1}
	require.NoError(t, store.Bind(ctx, id))
	writer, ok := messages.(message.FinalizationWriter)
	require.True(t, ok)
	reader, ok := messages.(message.FinalizationReader)
	require.True(t, ok)
	return candidateEnv{conn: conn, store: store, messages: messages, writer: writer, reader: reader, id: id}
}

// Revert-check: a reader that errors on an unfamiliar owner, or that silently
// returns zero facts for a broken (nil) transaction, breaks the seam
// contract — the first seals nothing that should wait, the second lies about
// coverage.
func TestFinalizationQueuedCallsTxEmptyAndBrokenInputs(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	require.Equal(t, 0, finalizationFactsQueuedCount(t, env.conn, "owner"))
	tx, err := env.conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	count, err := NewFinalizationQueuedCallsFacts().FinalizationQueuedCallsTx(t.Context(), tx, "")
	require.NoError(t, err)
	require.Equal(t, 0, count)
	count, err = NewFinalizationQueuedCallsFacts().FinalizationQueuedCallsTx(t.Context(), nil, "owner")
	require.Error(t, err)
	require.Equal(t, 0, count)
}

// Revert-check: a reader that counts the enqueue pair's two rows separately
// (or neither) misreports one queued call as zero or two facts — the owner
// seals early or waits forever.
func TestFinalizationQueuedCallsTxCountsQueuedCallOnce(t *testing.T) {
	fixture := finalizationSeamQueuedCallFixture(t)
	finalizationSeamEnqueueQueuedCall(t, fixture, "queued-once")
	require.EqualValues(t, 1, commitHookCount(t, fixture.env.conn,
		`SELECT count(*) FROM session_run_queue WHERE id='queued-once'`))
	require.EqualValues(t, 1, commitHookCount(t, fixture.env.conn,
		`SELECT count(*) FROM session_agent_finalization_continuations WHERE row_id='queued-once'`))
	require.Equal(t, 1, finalizationFactsQueuedCount(t, fixture.env.conn, fixture.data.SessionID))
}

// Revert-check: a reader that only counts pending entries misses the leased
// window — the owner could seal while a queued call is actively in flight.
func TestFinalizationQueuedCallsTxCountsLeasedEntry(t *testing.T) {
	fixture := finalizationSeamQueuedCallFixture(t)
	finalizationSeamEnqueueQueuedCall(t, fixture, "queued-lease")
	entry := fixture.lease(t, "facts-pump")
	require.NotNil(t, entry)
	require.Equal(t, 1, finalizationFactsQueuedCount(t, fixture.env.conn, fixture.data.SessionID))
}

// Revert-check: a reader that counts consumed continuations forever, or that
// still counts after the ack deleted the queue row, keeps the owner waiting
// after its queued call fully completed.
func TestFinalizationQueuedCallsTxIgnoresConsumedAndAcked(t *testing.T) {
	fixture := finalizationSeamQueuedCallFixture(t)
	finalizationSeamEnqueueQueuedCall(t, fixture, "queued-done")
	entry := fixture.lease(t, "facts-pump")
	leaseCtx := withFinalizationQueueLease(fixture.ctx, entry)
	restored := NewService(db.New(fixture.env.conn), fixture.env.conn).(AgentFinalizationQueueService)
	_, err := restored.RestoreAgentFinalizationQueue(leaseCtx, entry.ID, fixture.data)
	require.NoError(t, err)
	require.Equal(t, 1, finalizationFactsQueuedCount(t, fixture.env.conn, fixture.data.SessionID))
	_, err = fixture.service.AckRunQueueEntry(fixture.ctx, entry.ID, "facts-pump")
	require.NoError(t, err)
	require.Equal(t, 0, finalizationFactsQueuedCount(t, fixture.env.conn, fixture.data.SessionID))
}

// Revert-check: a reader that counts only live run-queue rows loses the
// orphaned unconsumed continuation — a durable obligation left behind (the
// queue row deleted without consumption) must still keep the owner waiting.
func TestFinalizationQueuedCallsTxCountsOrphanedContinuation(t *testing.T) {
	fixture := finalizationSeamQueuedCallFixture(t)
	finalizationSeamEnqueueQueuedCall(t, fixture, "queued-orphan")
	result, err := fixture.env.conn.ExecContext(t.Context(), `DELETE FROM session_run_queue WHERE id='queued-orphan'`)
	require.NoError(t, err)
	affected, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, affected)
	require.Equal(t, 1, finalizationFactsQueuedCount(t, fixture.env.conn, fixture.data.SessionID))
	result, err = fixture.env.conn.ExecContext(t.Context(),
		`UPDATE session_agent_finalization_continuations SET consumed=1 WHERE row_id='queued-orphan'`)
	require.NoError(t, err)
	affected, err = result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, affected)
	require.Equal(t, 0, finalizationFactsQueuedCount(t, fixture.env.conn, fixture.data.SessionID))
}

// Revert-check: a reader that counts by session without filtering, or that
// errors on an unfamiliar owner in a shared database, cross-charges one
// session's queued calls to another.
func TestFinalizationQueuedCallsTxSeparatesOwners(t *testing.T) {
	fixture := finalizationSeamQueuedCallFixture(t)
	finalizationSeamEnqueueQueuedCall(t, fixture, "queued-facts")
	require.NoError(t, seedSession(fixture.ctx, db.New(fixture.env.conn), "other"))
	finalizationFactsInsertQueuePair(t, fixture.env.conn, "other", "other-rq")
	finalizationFactsInsertOrphanContinuation(t, fixture.env.conn, "other", "other-cont")
	require.Equal(t, 1, finalizationFactsQueuedCount(t, fixture.env.conn, fixture.data.SessionID))
	require.Equal(t, 2, finalizationFactsQueuedCount(t, fixture.env.conn, "other"))
	require.Equal(t, 0, finalizationFactsQueuedCount(t, fixture.env.conn, "child"))
}

// Revert-check: a TrySeal that ignores the queued-calls fact seals the owner
// while a queued call is still outstanding, instead of refusing with
// queued_calls_present.
func TestTrySealRefusesQueuedCallsFact(t *testing.T) {
	e := newQueuedScopeSealEnv(t, true)
	_, presented := sealStoredCandidate(t, e, 0, 1)
	zero := &finalizationSeamScopeStub{}
	reader := NewAgentFinalizationScopeReader("owner", zero, NewFinalizationQueuedCallsFacts(), &finalizationSeamScopeStub{}, zero)
	outcome, err := e.store.TrySeal(t.Context(), e.id, presented, e.sealWriter(t), reader)
	require.NoError(t, err)
	require.False(t, outcome.Sealed)
	require.False(t, outcome.Accepted)
	require.Equal(t, AgentFinalizationSealQueuedCalls, outcome.Reason)
}

// Revert-check: a TrySeal that refuses even with zero queued facts — or one
// that seals without driving the record to the accepted phase — breaks the
// clean-path seal.
func TestTrySealSealsWithoutQueuedCallsFact(t *testing.T) {
	e := newQueuedScopeSealEnv(t, false)
	_, presented := sealStoredCandidate(t, e, 0, 1)
	zero := &finalizationSeamScopeStub{}
	reader := NewAgentFinalizationScopeReader("owner", zero, NewFinalizationQueuedCallsFacts(), &finalizationSeamScopeStub{}, zero)
	outcome, err := e.store.TrySeal(t.Context(), e.id, presented, e.sealWriter(t), reader)
	require.NoError(t, err)
	require.True(t, outcome.Sealed)
	require.Equal(t, AgentFinalizationSealNone, outcome.Reason)
	record, err := e.store.Snapshot(t.Context(), e.id)
	require.NoError(t, err)
	require.Equal(t, "accepted", record.Phase)
}
