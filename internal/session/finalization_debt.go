package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// FinalizationDebtStore reads and settles the owner's reaction debts across
// the two canonical debt tables: wake session_notices rows and selected
// batch_nodes notice rows. It implements both finalization seams for those
// sources: FinalizationReactionDebtFacts for the seal's scope reader and
// AgentFinalizationDebtSettler for the wait/compose loop.
type FinalizationDebtStore struct {
	sqlDB *sql.DB
}

var (
	_ FinalizationReactionDebtFacts = (*FinalizationDebtStore)(nil)
	_ AgentFinalizationDebtSettler  = (*FinalizationDebtStore)(nil)
)

// NewFinalizationDebtStore builds the debt store over sqlDB.
func NewFinalizationDebtStore(sqlDB *sql.DB) *FinalizationDebtStore {
	return &FinalizationDebtStore{sqlDB: sqlDB}
}

// finalizationReactionDebtSQL pins the session_notices half of the debt: the
// same predicate BatchOutboxDebtNotices (internal/db/sql/batch_outbox.sql)
// uses, plus the verbatim delivery != 'void' term repeating
// idx_session_notices_debt's partial-index predicate so the planner serves
// the read from that index. The extra term cannot change the row set: the
// IN list already fixes delivery to ('pending', 'done').
const finalizationReactionDebtSQL = `SELECT count(*) FROM session_notices
WHERE owner = ? AND wake = 1 AND reacted = 0 AND delivery IN ('pending', 'done')
  AND delivery != 'void'`

// finalizationNodeDebtSQL pins the batch_nodes half of the debt: the same
// count over the exact predicate batchDebtNodesVoidAwareSQL
// (internal/session/batch_reaction.go) executes — the canonical
// BatchOutboxDebtNodes terms (notice_selected = 1, root announced, wake = 1,
// reacted = 0, delivery IN ('pending', 'done')) plus the R12-F root-void
// exclusion (r.delivery != 'void'). node_id is the primary key and the root
// join is 1:1 (one root row per root_id), so the join cannot multiply rows
// and count(*) counts debts exactly. The verbatim n.delivery != 'void' term
// repeats idx_batch_nodes_debt's partial-index predicate (notice_selected =
// 1 AND wake = 1 AND reacted = 0 AND delivery != 'void') so the planner
// serves the read from that index; it cannot change the row set, which the
// IN list already fixes to ('pending', 'done').
const finalizationNodeDebtSQL = `SELECT count(*) FROM batch_nodes n JOIN batch_nodes r ON r.node_id = n.root_id
WHERE n.owner_session_id = ? AND n.notice_selected = 1 AND r.announced = 1
  AND r.delivery != 'void'
  AND n.wake = 1 AND n.reacted = 0 AND n.delivery IN ('pending', 'done')
  AND n.delivery != 'void'`

// FinalizationReactionDebtTx counts the owner's unsettled reaction debts
// inside the seal's writer transaction. The debt is the sum of the two
// canonical halves SessionNoticeStore.BatchPendingInclusiveDebtRows reads
// (internal/session/session_notice_store.go): wake session_notices rows and
// selected batch_nodes notice rows, with no dedup by construction — batch
// results select their notices onto batch_nodes rows (SelectBatchNodeNotice
// in internal/db/sql/batch_nodes.sql) while every other producer writes only
// session_notices, so no row belongs to both tables. Within each half a debt
// is a wake row the owner still owes a reaction to: wake = 1, not yet
// reacted, and delivery pending (not pulled yet) or done (pulled into
// history, reaction still owed). Consumed (reacted = 1), voided, non-wake
// and root-void rows are noise, not debt. An unknown or empty owner owns no
// rows, so the count is 0; a database error is an error, and a missing
// transaction is a caller bug surfaced as an error instead of a silent zero.
func (s *FinalizationDebtStore) FinalizationReactionDebtTx(ctx context.Context, tx *sql.Tx, ownerSessionID string) (int, error) {
	if tx == nil {
		return 0, errors.New("finalization reaction debt facts require a transaction")
	}
	var notices int
	if err := tx.QueryRowContext(ctx, finalizationReactionDebtSQL, ownerSessionID).Scan(&notices); err != nil {
		return 0, fmt.Errorf("count finalization reaction debt: %w", err)
	}
	var nodes int
	if err := tx.QueryRowContext(ctx, finalizationNodeDebtSQL, ownerSessionID).Scan(&nodes); err != nil {
		return 0, fmt.Errorf("count finalization node debt: %w", err)
	}
	return notices + nodes, nil
}
