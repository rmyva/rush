package session

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// finalizationFlipNotice forces one session_notices row's columns directly
// in the database, the deterministic lever for classifying debt states (the
// same lever style as finalizationSeamSetPhase).
func finalizationFlipNotice(t *testing.T, conn *sql.DB, ctx context.Context, id int64, sets string) {
	t.Helper()
	result, err := conn.ExecContext(ctx, `UPDATE session_notices SET `+sets+` WHERE id=?`, id)
	require.NoError(t, err)
	changed, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, changed, "the flip lever must hit exactly the one notice")
}

// Revert-check: a reader that hardcodes zero or drifts from the debt
// predicate (wake, reacted, delivery) fails this count.
func TestFinalizationReactionDebtTxCountsUnsettledWakeDebts(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	reader := NewFinalizationDebtStore(env.conn)
	tx, err := env.conn.BeginTx(env.ctx, nil)
	require.NoError(t, err)
	count, err := reader.FinalizationReactionDebtTx(env.ctx, tx, "owner")
	require.NoError(t, err)
	require.Equal(t, 0, count, "a fresh owner owes no reaction debt")
	require.NoError(t, tx.Rollback())
	finalizationSeamInsertDebt(t, env, "owner")
	finalizationSeamInsertDebt(t, env, "owner")
	tx, err = env.conn.BeginTx(env.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	count, err = reader.FinalizationReactionDebtTx(env.ctx, tx, "owner")
	require.NoError(t, err)
	require.Equal(t, 2, count, "each pending wake notice is one unsettled debt")
}

// Revert-check: dropping the reacted = 0 / wake = 1 / delivery filter, or
// adding a kind filter, changes the count; the classification is
// kind-agnostic.
func TestFinalizationReactionDebtTxClassifiesDebtStates(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	reader := NewFinalizationDebtStore(env.conn)
	notices := NewSessionNoticeStore(env.conn)
	// Two unsettled pending wake debts (the plain testkit fixture).
	finalizationSeamInsertDebt(t, env, "owner")
	finalizationSeamInsertDebt(t, env, "owner")
	// One delivered-but-unreacted debt: pulled into history, still owed.
	delivered, err := notices.InsertSessionNoticeReturningID(env.ctx, "owner", NoticeKindWakeOnly, "finalization-debt", true, "")
	require.NoError(t, err)
	finalizationFlipNotice(t, env.conn, env.ctx, delivered, `delivery='done'`)
	// One consumed row: the ordinary real-step reaction already marked it.
	consumed, err := notices.InsertSessionNoticeReturningID(env.ctx, "owner", NoticeKindWakeOnly, "finalization-debt", true, "")
	require.NoError(t, err)
	finalizationFlipNotice(t, env.conn, env.ctx, consumed, `delivery='done', reacted=1`)
	// One voided row: the pull's void condition retired it.
	voided, err := notices.InsertSessionNoticeReturningID(env.ctx, "owner", NoticeKindWakeOnly, "finalization-debt", true, "")
	require.NoError(t, err)
	finalizationFlipNotice(t, env.conn, env.ctx, voided, `delivery='void'`)
	// One non-wake marker: describes an outcome, never re-pended on rerun.
	require.NoError(t, notices.InsertSessionNotice(env.ctx, "owner", NoticeKindReactionChain, "marker", false, ""))
	var delivery string
	var reacted int64
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT delivery, reacted FROM session_notices WHERE id=?`, delivered).Scan(&delivery, &reacted))
	require.Equal(t, "done", delivery)
	require.Zero(t, reacted, "the delivered row stays unreacted")
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT delivery, reacted FROM session_notices WHERE id=?`, voided).Scan(&delivery, &reacted))
	require.Equal(t, "void", delivery)
	// A second, unbound session owes its own debt and must not bleed into
	// the owner's count.
	_, err = env.svc.CreateTaskSession(env.ctx, "other", "owner", "unbound")
	require.NoError(t, err)
	finalizationSeamInsertDebt(t, env, "other")
	tx, err := env.conn.BeginTx(env.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	count, err := reader.FinalizationReactionDebtTx(env.ctx, tx, "owner")
	require.NoError(t, err)
	require.Equal(t, 3, count, "two pending debts plus one delivered-unreacted")
	other, err := reader.FinalizationReactionDebtTx(env.ctx, tx, "other")
	require.NoError(t, err)
	require.Equal(t, 1, other, "the other session's debt stays scoped to it")
}

// Revert-check: a fail-open reader returning (0, nil) for a nil or dead
// transaction hides a broken reader behind complete facts.
func TestFinalizationReactionDebtTxUnknownOwnerAndBrokenTx(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	reader := NewFinalizationDebtStore(env.conn)
	tx, err := env.conn.BeginTx(env.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	count, err := reader.FinalizationReactionDebtTx(env.ctx, tx, "nobody")
	require.NoError(t, err)
	require.Equal(t, 0, count, "an unknown owner owns no rows, so zero debt")
	count, err = reader.FinalizationReactionDebtTx(env.ctx, tx, "")
	require.NoError(t, err)
	require.Equal(t, 0, count, "an empty owner owns no rows, so zero debt")
	_, err = reader.FinalizationReactionDebtTx(env.ctx, nil, "owner")
	require.Error(t, err, "a missing transaction is a caller bug, not zero")
	// The test DB has one connection: release it before the next BeginTx.
	require.NoError(t, tx.Rollback())
	dead, err := env.conn.BeginTx(env.ctx, nil)
	require.NoError(t, err)
	require.NoError(t, dead.Rollback())
	_, err = reader.FinalizationReactionDebtTx(env.ctx, dead, "owner")
	require.Error(t, err, "a rolled-back transaction cannot serve the read")
}

// Revert-check: a reader that ignores the passed transaction and uses its
// own connection misses the uncommitted row.
func TestFinalizationReactionDebtTxReadsInsideGivenTransaction(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	reader := NewFinalizationDebtStore(env.conn)
	tx, err := env.conn.BeginTx(env.ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(env.ctx,
		`INSERT INTO session_notices (owner, kind, text, wake, created_at, updated_at)
		 VALUES (?, ?, ?, 1, 0, 0)`, "owner", NoticeKindWakeOnly, "finalization-debt")
	require.NoError(t, err)
	count, err := reader.FinalizationReactionDebtTx(env.ctx, tx, "owner")
	require.NoError(t, err)
	require.Equal(t, 1, count, "the reader must see its own transaction's row")
	require.NoError(t, tx.Rollback())
	tx, err = env.conn.BeginTx(env.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	count, err = reader.FinalizationReactionDebtTx(env.ctx, tx, "owner")
	require.NoError(t, err)
	require.Equal(t, 0, count, "the rolled-back row never became durable")
}
