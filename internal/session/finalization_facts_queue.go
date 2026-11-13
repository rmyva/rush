package session

import (
	"context"
	"database/sql"
	"fmt"
)

// finalizationQueuedCallsFacts is the production queued-calls facts reader:
// it counts the owner's unfinished queued calls inside the seal transaction.
type finalizationQueuedCallsFacts struct{}

var _ FinalizationQueuedCallsFacts = finalizationQueuedCallsFacts{}

// NewFinalizationQueuedCallsFacts builds the queued-calls facts reader.
func NewFinalizationQueuedCallsFacts() FinalizationQueuedCallsFacts {
	return finalizationQueuedCallsFacts{}
}

// FinalizationQueuedCallsTx counts the owner's outstanding queued calls with
// one inline statement, binding both placeholders to ownerSessionID.
//
// Count-once criterion: a queued call is ONE fact. Its live run-queue row
// counts it, and the run-queue row only exists while the call is outstanding
// (status RunQueueStatusPending or RunQueueStatusLeased — the terminal
// outcomes AckRunQueueEntry and TerminalFailRunQueueEntry DELETE the row, and
// the sqlc reference predicate HasOutstandingRunQueueEntryForSession counts
// exactly pending|leased). Its unconsumed continuation row counts ONLY when
// no run-queue row with that row_id exists any more, so the enqueue pair
// never counts twice, while an orphaned unconsumed continuation — a durable
// obligation left behind, e.g. the queue row deleted without consumption —
// is never lost from the count. When in doubt the count errs HIGH:
// undercounting seals the owner early, overcounting only keeps it waiting.
//
// An empty ownerSessionID returns (0, nil): an unfamiliar owner is zero
// facts, never an error (seam contract §5.1). A nil tx is a broken reader
// and returns an error, never 0. The status literals in the SQL are the same
// hardcoded strings as the sqlc reference query; SQL string literals cannot
// reference the Go constants RunQueueStatusPending/RunQueueStatusLeased.
func (finalizationQueuedCallsFacts) FinalizationQueuedCallsTx(ctx context.Context, tx *sql.Tx, ownerSessionID string) (int, error) {
	if tx == nil {
		return 0, fmt.Errorf("finalization queued calls facts require a transaction")
	}
	if ownerSessionID == "" {
		return 0, nil
	}
	const query = `
SELECT
    (SELECT COUNT(*) FROM session_run_queue
     WHERE session_id = ? AND status IN ('pending', 'leased')) +
    (SELECT COUNT(*) FROM session_agent_finalization_continuations c
     WHERE c.session_id = ? AND c.consumed = 0
       AND NOT EXISTS (
           SELECT 1 FROM session_run_queue q WHERE q.id = c.row_id))`
	var count int
	if err := tx.QueryRowContext(ctx, query, ownerSessionID, ownerSessionID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count finalization queued calls facts: %w", err)
	}
	return count, nil
}
