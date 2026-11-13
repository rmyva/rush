package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
)

// finalizationSettleDebtPendingSQL is MarkSessionNoticesReactedForOwner
// (internal/db/sql/session_notices.sql) with delivery = 'pending': the
// generated op covers only delivered rows, but a pending wake notice stays
// debt until the pull delivers it, so a done-only settle would keep the
// seal refused across every recompose (each pull re-delivers the row as an
// unsettled debt and the wait loop never converges). The condition is the
// same strict debt predicate; nothing is deleted or voided, so every
// existing notice consumer (the pull, pending child questions, the debt
// accounting) keeps reading the row.
const finalizationSettleDebtPendingSQL = `UPDATE session_notices SET reacted = 1, updated_at = ?
WHERE owner = ? AND wake = 1 AND reacted = 0 AND delivery = 'pending'`

// finalizationSettleNodeDebtSQL is the batch_nodes half of the settle,
// written as the strict read predicate the debt count uses: the exact
// batchDebtNodesVoidAwareSQL terms (notice_selected = 1, root announced,
// wake = 1, reacted = 0, delivery IN ('pending', 'done'), root not void)
// plus the verbatim delivery != 'void' index term. batchOutboxReactNodes
// (internal/db/sql/batch_outbox.sql) is deliberately not reused: it covers
// only delivery = 'done', lacks the root-void exclusion (r.delivery !=
// 'void') and would mark rows the reader does not count. The pending half
// is required for convergence: a queued parent call's notice sits pending
// behind the current finalization turn, so a done-only nodes settle would
// leave it counted and the seal refused across every recompose, exactly
// like the pending session_notices half above. Nothing is deleted or
// voided: delivery, wake, state and the notice_* columns stay, so the
// parent's compose/next-turn pull still delivers the child's result
// content afterwards, and reacted_failed stays untouched, so the settle
// never fabricates a failure signal.
const finalizationSettleNodeDebtSQL = `UPDATE batch_nodes SET reacted = 1, updated_at = ?
WHERE batch_nodes.owner_session_id = ? AND batch_nodes.notice_selected = 1
  AND batch_nodes.delivery IN ('pending', 'done') AND batch_nodes.delivery != 'void'
  AND batch_nodes.wake = 1 AND batch_nodes.reacted = 0
  AND EXISTS (SELECT 1 FROM batch_nodes r WHERE r.node_id = batch_nodes.root_id
    AND r.announced = 1 AND r.delivery != 'void')`

// SettleFinalizationDebts idempotently settles the owner's outstanding
// reaction debts in both canonical debt tables by marking the rows reacted
// with the existing receipt mechanism (reacted = 1, the same mark the
// ordinary real-step reaction MarkSessionNoticesReactedForOwner and
// batchOutboxReactNodes write). All three halves run in one transaction,
// so the settle is all-or-nothing and a repeat call finds no matching rows
// and returns 0. It returns the number of rows this call settled; an
// unknown or empty owner settles nothing and returns 0.
func (s *FinalizationDebtStore) SettleFinalizationDebts(ctx context.Context, ownerSessionID string) (int, error) {
	if ownerSessionID == "" {
		return 0, nil
	}
	if s == nil || s.sqlDB == nil {
		return 0, errors.New("finalization debt settler requires a database connection")
	}
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin finalization debt settle: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	done, err := db.New(tx).MarkSessionNoticesReactedForOwner(ctx, db.MarkSessionNoticesReactedForOwnerParams{
		Owner: ownerSessionID, UpdatedAt: now,
	})
	if err != nil {
		return 0, fmt.Errorf("settle finalization reaction debt (delivered rows): %w", err)
	}
	pendingResult, err := tx.ExecContext(ctx, finalizationSettleDebtPendingSQL, now, ownerSessionID)
	if err != nil {
		return 0, fmt.Errorf("settle finalization reaction debt (pending rows): %w", err)
	}
	pending, err := pendingResult.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count settled finalization reaction debt: %w", err)
	}
	nodesResult, err := tx.ExecContext(ctx, finalizationSettleNodeDebtSQL, now, ownerSessionID)
	if err != nil {
		return 0, fmt.Errorf("settle finalization node debt: %w", err)
	}
	nodes, err := nodesResult.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count settled finalization node debt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit finalization debt settle: %w", err)
	}
	return int(done) + int(pending) + int(nodes), nil
}
