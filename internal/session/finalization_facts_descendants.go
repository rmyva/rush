package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// finalizationDescendantTreeSQL walks the owner's hierarchy descendants:
// every session reachable through sessions.parent_session_id, transitively.
// The depth cap is baked in from finalizationAncestorLimit, the same bound
// readFinalizationAncestry enforces, and UNION drops repeated (id, depth)
// pairs, so a corrupt parent cycle terminates instead of recursing forever.
var finalizationDescendantTreeSQL = fmt.Sprintf(`WITH RECURSIVE descendants(id, depth) AS (
    SELECT id, 1 FROM sessions WHERE parent_session_id = ?
    UNION
    SELECT s.id, d.depth + 1 FROM sessions s, descendants d
    WHERE s.parent_session_id = d.id AND d.depth < %d
)`, finalizationAncestorLimit)

// finalizationDescendantRecordSQL counts hierarchy descendants holding an
// active finalization incarnation. The partial unique index
// session_agent_finalizations_active_owner keeps one active row per session,
// but the CTE can hold the same session at several depths, hence
// count(DISTINCT).
var finalizationDescendantRecordSQL = finalizationDescendantTreeSQL + `
SELECT count(DISTINCT f.session_id)
FROM session_agent_finalizations f, descendants d
WHERE f.session_id = d.id
    AND f.phase IN ('executing', 'waiting_work', 'composing')`

// finalizationDescendantTruncatedSQL detects a hierarchy the depth cap cut
// off: any child under a cap-depth descendant means real ancestry beyond
// the guard. Silently truncating could undercount live work and seal early,
// so the reader refuses instead, matching readFinalizationAncestry.
var finalizationDescendantTruncatedSQL = finalizationDescendantTreeSQL + fmt.Sprintf(`
SELECT EXISTS (
    SELECT 1
    FROM sessions s, descendants d
    WHERE d.depth = %d AND s.parent_session_id = d.id
)`, finalizationAncestorLimit)

// finalizationDescendantNodeSQL counts the owner's live batch nodes, root
// or task, in any non-terminal state. Same state list as the seal
// reference reader in finalization_seal_test.go.
var finalizationDescendantNodeSQL = `SELECT count(*) FROM batch_nodes
WHERE owner_session_id = ?
    AND state IN ('pending', 'starting', 'running', 'cancelling')`

// finalizationDescendantOverlapSQL counts the sessions the record source
// already counted that a live batch node also drives: distinct live-node
// child sessions that are hierarchy descendants with an active record.
var finalizationDescendantOverlapSQL = finalizationDescendantTreeSQL + `
SELECT count(DISTINCT n.child_session_id)
FROM batch_nodes n, descendants d, session_agent_finalizations f
WHERE n.owner_session_id = ?
    AND n.state IN ('pending', 'starting', 'running', 'cancelling')
    AND n.child_session_id = d.id
    AND f.session_id = d.id
    AND f.phase IN ('executing', 'waiting_work', 'composing')`

// FinalizationDescendantsReader counts the owner's live descendant work
// inside the seal's writer transaction: active finalization records of
// hierarchy descendants plus live batch nodes. The zero value is ready to
// use; the reader is stateless and reads only through the transaction it
// is handed.
type FinalizationDescendantsReader struct{}

var _ FinalizationDescendantsFacts = FinalizationDescendantsReader{}

// NewFinalizationDescendantsReader returns the production descendants
// source for NewAgentFinalizationScopeReader.
func NewFinalizationDescendantsReader() FinalizationDescendantsReader {
	return FinalizationDescendantsReader{}
}

// FinalizationActiveDescendantsTx returns the number of the owner's active
// descendants observed strictly inside tx, read-only. Deduplication is by
// session identity: a descendant already counted through its active
// finalization record is not counted again when a live batch node drives
// the same session, and only one node per such session folds away, so
// anomalous extra nodes keep counting and the counter errs high. A missing
// descendant would seal the owner's answer too early, an extra one only
// delays the seal. An unknown owner owns nothing and yields 0, not an
// error; a database failure is an error, and so is a hierarchy deeper
// than finalizationAncestorLimit, which truncation would undercount.
func (FinalizationDescendantsReader) FinalizationActiveDescendantsTx(ctx context.Context, tx *sql.Tx, ownerSessionID string) (int, error) {
	if tx == nil {
		return 0, errors.New("finalization descendants facts require a transaction")
	}
	if ownerSessionID == "" {
		return 0, nil
	}
	truncated, err := finalizationDescendantScalar(ctx, tx, finalizationDescendantTruncatedSQL, ownerSessionID)
	if err != nil {
		return 0, err
	}
	if truncated != 0 {
		return 0, fmt.Errorf("finalization descendant hierarchy deeper than %d", finalizationAncestorLimit)
	}
	recorded, err := finalizationDescendantScalar(ctx, tx, finalizationDescendantRecordSQL, ownerSessionID)
	if err != nil {
		return 0, err
	}
	nodes, err := finalizationDescendantScalar(ctx, tx, finalizationDescendantNodeSQL, ownerSessionID)
	if err != nil {
		return 0, err
	}
	// The CTE anchor placeholder comes first in the SQL text, then the
	// node-side owner filter, so the owner binds twice here.
	overlapped, err := finalizationDescendantScalar(ctx, tx, finalizationDescendantOverlapSQL, ownerSessionID, ownerSessionID)
	if err != nil {
		return 0, err
	}
	return recorded + nodes - overlapped, nil
}

// finalizationDescendantScalar runs one read-only count inside tx.
func finalizationDescendantScalar(ctx context.Context, tx *sql.Tx, query string, args ...any) (int, error) {
	var count int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("read finalization descendant facts: %w", err)
	}
	return count, nil
}
