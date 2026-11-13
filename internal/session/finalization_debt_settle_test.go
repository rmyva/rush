package session

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// finalizationDebtScopeReader builds the composite production scope reader
// with the debt store wired as the reaction-debt source and zero-value stubs
// for the other three facts sources (they count 0).
func finalizationDebtScopeReader(owner string, debt *FinalizationDebtStore) AgentFinalizationScopeReader {
	return NewAgentFinalizationScopeReader(owner,
		&finalizationSeamScopeStub{}, &finalizationSeamScopeStub{},
		&finalizationSeamScopeStub{}, debt)
}

// finalizationDebtCountTx reads the owner's unsettled debt count through the
// reader inside a fresh transaction, then rolls the transaction back.
func finalizationDebtCountTx(t *testing.T, conn *sql.DB, ctx context.Context, reader *FinalizationDebtStore, owner string) int {
	t.Helper()
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	count, err := reader.FinalizationReactionDebtTx(ctx, tx, owner)
	require.NoError(t, err)
	return count
}

// finalizationDebtSealEnv replicates newCandidateEnv with the debt inserted
// BEFORE the bind: the S1 fence refuses notice inserts for an owner that
// already has a bound finalization scope, so the debt must exist first
// (newReservedScopeEnv's ordering, finalization_seal_test.go).
func finalizationDebtSealEnv(t *testing.T) candidateEnv {
	t.Helper()
	_, conn, ctx := batchStoreTestDB(t)
	// The S1 fence refuses notice inserts for an owner with a bound
	// finalization scope, so the debt must exist before the bind
	// (newReservedScopeEnv's ordering, finalization_seal_test.go).
	require.NoError(t, NewSessionNoticeStore(conn).InsertSessionNotice(ctx, "owner", NoticeKindWakeOnly, "finalization-debt", true, ""))
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

// finalizationDebtLateReader wraps the composite reader and produces one
// more wake debt while the seal is deciding, through the seal's own
// transaction; it records the debt counter before and after the injection.
type finalizationDebtLateReader struct {
	inner     AgentFinalizationScopeReader
	owner     string
	firstRead int
	lateRead  int
}

var _ AgentFinalizationScopeReader = (*finalizationDebtLateReader)(nil)

// FinalizationSealScopeFacts reads the wrapped facts, injects one pending
// wake debt row inside the passed transaction, and re-reads the facts so the
// seal decides on the late row too.
func (r *finalizationDebtLateReader) FinalizationSealScopeFacts(ctx context.Context, tx *sql.Tx) (AgentFinalizationScopeFacts, error) {
	facts, err := r.inner.FinalizationSealScopeFacts(ctx, tx)
	if err != nil {
		return AgentFinalizationScopeFacts{}, err
	}
	r.firstRead = facts.ReactionDebt
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_notices
		(owner, kind, text, wake, delivery, created_at, updated_at)
		VALUES (?, ?, ?, 1, 'pending', ?, ?)`,
		r.owner, NoticeKindWakeOnly, "late-debt", now, now); err != nil {
		return AgentFinalizationScopeFacts{}, err
	}
	late, err := r.inner.FinalizationSealScopeFacts(ctx, tx)
	if err != nil {
		return AgentFinalizationScopeFacts{}, err
	}
	r.lateRead = late.ReactionDebt
	return late, nil
}

// Revert-check: a non-idempotent settle, a deleting or voiding settle, or a
// lost owner scoping fails these assertions.
func TestSettleFinalizationDebtsSettlesExactlyOwnerDebtsIdempotently(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	settler := NewFinalizationDebtStore(env.conn)
	reader := NewFinalizationDebtStore(env.conn)
	finalizationSeamInsertDebt(t, env, "owner")
	finalizationSeamInsertDebt(t, env, "owner")
	// One delivered-but-unreacted debt: the settle must reach it too.
	delivered, err := NewSessionNoticeStore(env.conn).InsertSessionNoticeReturningID(env.ctx, "owner", NoticeKindWakeOnly, "finalization-debt", true, "")
	require.NoError(t, err)
	finalizationFlipNotice(t, env.conn, env.ctx, delivered, `delivery='done'`)
	// A second, unbound session keeps its own unsettled debt.
	_, err = env.svc.CreateTaskSession(env.ctx, "other", "owner", "unbound")
	require.NoError(t, err)
	finalizationSeamInsertDebt(t, env, "other")
	settled, err := settler.SettleFinalizationDebts(env.ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, 3, settled, "two pending plus one delivered debt settled")
	require.Equal(t, 0, finalizationDebtCountTx(t, env.conn, env.ctx, reader, "owner"),
		"after the settle the owner owes no reaction debt")
	var total, reacted, pending, done int64
	var delivery string
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT count(*) FROM session_notices WHERE owner=?`, "owner").Scan(&total))
	require.EqualValues(t, 3, total, "a settle marks, it never deletes rows")
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT count(*) FROM session_notices WHERE owner=? AND reacted=1`, "owner").Scan(&reacted))
	require.EqualValues(t, 3, reacted, "every settled row carries the reacted receipt")
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT count(*) FROM session_notices WHERE owner=? AND delivery='pending'`, "owner").Scan(&pending))
	require.EqualValues(t, 2, pending, "pending rows keep their delivery")
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT count(*) FROM session_notices WHERE owner=? AND delivery='done'`, "owner").Scan(&done))
	require.EqualValues(t, 1, done, "delivered rows keep their delivery")
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT reacted, delivery FROM session_notices WHERE owner='other'`).Scan(&reacted, &delivery))
	require.Zero(t, reacted, "another session's debt stays unsettled")
	require.Equal(t, "pending", delivery)
	require.Equal(t, 1, finalizationDebtCountTx(t, env.conn, env.ctx, reader, "other"),
		"the other session still owes its wake reaction")
	settled, err = settler.SettleFinalizationDebts(env.ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, 0, settled, "a repeat settle finds no matching rows")
	settled, err = settler.SettleFinalizationDebts(env.ctx, "other")
	require.NoError(t, err)
	require.Equal(t, 1, settled)
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT count(*) FROM session_notices WHERE owner='owner' AND reacted=1`).Scan(&reacted))
	require.EqualValues(t, 3, reacted, "settling other leaves the owner's receipts untouched")
}

// Revert-check: a settle that recounts or aborts on already-reacted rows
// fails the == 2 and repeat == 0 assertions.
func TestSettleFinalizationDebtsConvergesAfterPartialApplication(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	settler := NewFinalizationDebtStore(env.conn)
	reader := NewFinalizationDebtStore(env.conn)
	finalizationSeamInsertDebt(t, env, "owner")
	finalizationSeamInsertDebt(t, env, "owner")
	finalizationSeamInsertDebt(t, env, "owner")
	// An earlier interrupted settle marked exactly one row reacted.
	var first int64
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT id FROM session_notices WHERE owner=? ORDER BY id LIMIT 1`, "owner").Scan(&first))
	finalizationFlipNotice(t, env.conn, env.ctx, first, `reacted=1`)
	settled, err := settler.SettleFinalizationDebts(env.ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, 2, settled, "only the two unsettled rows settle this call")
	require.Equal(t, 0, finalizationDebtCountTx(t, env.conn, env.ctx, reader, "owner"))
	settled, err = settler.SettleFinalizationDebts(env.ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, 0, settled, "the settle converges instead of recounting")
}

// Revert-check: removing the S1 existence check in validateBatchFinalizationTx
// lets unadmitted producers mutate scope mid-seal, and this fence proof fails.
func TestFinalizationDebtInsertIsFencedForBoundOwner(t *testing.T) {
	e := finalizationDebtSealEnv(t)
	reader := NewFinalizationDebtStore(e.conn)
	// The pre-bind debt insert succeeded (finalizationDebtSealEnv's order).
	require.Equal(t, 1, finalizationDebtCountTx(t, e.conn, t.Context(), reader, "owner"))
	// After the bind, a plain-context producer is refused.
	err := NewSessionNoticeStore(e.conn).InsertSessionNotice(t.Context(), "owner", NoticeKindWakeOnly, "late", true, "")
	require.ErrorIs(t, err, ErrAgentFinalizationChanged)
}

// Revert-check: a reader reading outside the seal's transaction or a settle
// that does not clear the counted predicate lets the seal proceed with an
// open debt, or never seals after the settle.
func TestFinalizationSealRefusesReactionDebtUntilSettled(t *testing.T) {
	e := finalizationDebtSealEnv(t)
	store := NewFinalizationDebtStore(e.conn)
	_, presented := sealStoredCandidate(t, e, 0, 1)
	reader := finalizationDebtScopeReader(e.id.SessionID, store)
	outcome, err := e.store.TrySeal(t.Context(), e.id, presented, e.sealWriter(t), reader)
	require.NoError(t, err)
	require.False(t, outcome.Sealed, "an open reaction debt must refuse the seal")
	require.False(t, outcome.Accepted)
	require.Equal(t, AgentFinalizationSealReactionDebt, outcome.Reason)
	settled, err := store.SettleFinalizationDebts(t.Context(), e.id.SessionID)
	require.NoError(t, err)
	require.Equal(t, 1, settled)
	require.Equal(t, 0, finalizationDebtCountTx(t, e.conn, t.Context(), store, e.id.SessionID),
		"the settle must clear exactly what the reader counts")
	outcome, err = e.store.TrySeal(t.Context(), e.id, presented, e.sealWriter(t), reader)
	require.NoError(t, err)
	require.True(t, outcome.Sealed, "a settled debt lets the seal proceed")
}

// Revert-check: a reader that cannot see a debt produced while the seal is
// deciding reads a stale count and seals prematurely; the firstRead and
// lateRead assertions plus the refusal catch it.
func TestFinalizationSealSeesDebtProducedWhileSealing(t *testing.T) {
	e := finalizationDebtSealEnv(t)
	store := NewFinalizationDebtStore(e.conn)
	late := &finalizationDebtLateReader{
		inner: finalizationDebtScopeReader(e.id.SessionID, store),
		owner: e.id.SessionID,
	}
	_, presented := sealStoredCandidate(t, e, 0, 1)
	outcome, err := e.store.TrySeal(t.Context(), e.id, presented, e.sealWriter(t), late)
	require.NoError(t, err)
	require.False(t, outcome.Sealed, "a debt produced while sealing must refuse the seal")
	require.Equal(t, AgentFinalizationSealReactionDebt, outcome.Reason)
	require.Equal(t, 1, late.firstRead, "one settled-stage debt before the injection")
	require.Equal(t, 2, late.lateRead, "the reader must observe the row it injected")
	// The injected row rolled back with the refused seal: exactly the
	// original debt remains, unsettled.
	require.Equal(t, 1, finalizationDebtCountTx(t, e.conn, t.Context(), store, e.id.SessionID))
	var total int64
	require.NoError(t, e.conn.QueryRowContext(t.Context(),
		`SELECT count(*) FROM session_notices WHERE owner=?`, e.id.SessionID).Scan(&total))
	require.EqualValues(t, 1, total, "the injected row must not survive the rollback")
	settled, err := store.SettleFinalizationDebts(t.Context(), e.id.SessionID)
	require.NoError(t, err)
	require.Equal(t, 1, settled)
	require.Equal(t, 0, finalizationDebtCountTx(t, e.conn, t.Context(), store, e.id.SessionID))
	outcome, err = e.store.TrySeal(t.Context(), e.id, presented, e.sealWriter(t),
		finalizationDebtScopeReader(e.id.SessionID, store))
	require.NoError(t, err)
	require.True(t, outcome.Sealed, "a settled debt lets the seal proceed")
}
