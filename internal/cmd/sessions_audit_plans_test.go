package cmd

// The query-plan proof of `rush sessions audit`: --since and the chunked
// scans must rest on the production message indexes rather than full scans
// and sorts. EXPLAIN QUERY PLAN never executes a statement, so this is
// proved against the read-only handle the audit itself would use, over a
// fixture that carries the same indexes a real rush.db has.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db/storepath"
	"github.com/stretchr/testify/require"
)

// auditPlanFixtureSchema is the rules fixture schema plus the two production
// message indexes (internal/db/migrations/20261022000001_store_v1_initial.sql):
// the plan proof is only meaningful against the indexes a real rush.db has.
const auditPlanFixtureSchema = auditSchemaCore + `
CREATE INDEX idx_messages_session_id_created_at ON messages (session_id, created_at);
CREATE INDEX idx_messages_created_at ON messages (created_at);
` + auditBatchProjectionDDL

// seedAuditPlanDB builds the plan fixture: one session ('audit-plan-1') with
// messages both inside and far outside a one-day window, written with a
// short-lived connection that is fully closed before the read-only handle is
// opened.
func seedAuditPlanDB(t *testing.T, dataDir string) string {
	t.Helper()

	dbPath := storepath.ActivePath(dataDir)
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	_, err = db.ExecContext(ctx, auditPlanFixtureSchema)
	require.NoError(t, err)

	now := timeNowUnix()
	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions (id, title, created_at, updated_at, message_count) VALUES
			('audit-plan-1', 'audit-plan-1 title', ?, ?, 5)`, now, now)
	require.NoError(t, err)

	for i, createdAt := range []int64{
		now, now + 1, now - 72*3600, now - 72*3600 + 1, now + 2,
	} {
		_, err = db.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at)
				VALUES ('m%d', 'audit-plan-1', 'assistant', '[]', ?, ?)`, i),
			createdAt, createdAt)
		require.NoError(t, err)
	}

	require.NoError(t, db.Close())
	return dbPath
}

// auditExplainQueryPlan returns EXPLAIN QUERY PLAN's detail lines for one
// query. EXPLAIN never executes the statement, so the read-only handle the
// audit uses stays exactly that.
func auditExplainQueryPlan(t *testing.T, ro *sql.DB, query string, params []any) string {
	t.Helper()
	rows, err := ro.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, params...)
	require.NoError(t, err)
	defer rows.Close()
	var details []string
	for rows.Next() {
		var (
			id, parent, notused any
			detail              string
		)
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	return strings.Join(details, "\n")
}

// openAuditPlanRO seeds a plan fixture and hands back a read-only handle to
// it, fully closed after the writing connection is gone.
func openAuditPlanRO(t *testing.T) *sql.DB {
	t.Helper()
	isolateConfigEnvForTests(t)
	dbPath := seedAuditPlanDB(t, t.TempDir())
	ro, err := openAuditRO(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ro.Close() })
	return ro
}

// TestAuditQueryPlan_MessagesScanUsesIndexWithoutSort: the messages query the
// rules run searches by (session_id, created_at) and never sorts — a full
// scan of messages or a TEMP B-TREE for an ORDER BY is a regression.
//
// Revert-check: re-adding " ORDER BY created_at, rowid" to auditMessagesQuery
// brings the TEMP B-TREE back into the plan; dropping " AND created_at >= ?"
// degrades the window to a plain session_id search.
func TestAuditQueryPlan_MessagesScanUsesIndexWithoutSort(t *testing.T) {
	ro := openAuditPlanRO(t)

	query, params := auditMessagesQuery(auditSchema{MessagesNoticeKind: true},
		[]string{"audit-plan-1"}, time.Hour)
	require.NotEmpty(t, params, "a windowed query binds its window")
	plan := auditExplainQueryPlan(t, ro, query, params)
	require.Contains(t, plan,
		"SEARCH messages USING INDEX idx_messages_session_id_created_at",
		"the messages scan must seek the composite index")
	require.NotContains(t, plan, "SCAN messages",
		"a full scan of messages is forbidden")
	require.NotContains(t, plan, "TEMP B-TREE",
		"the rules are order-independent: no sort may be paid")
}

// TestAuditQueryPlan_SinceSelectionSeeksPerSession: with a window, --all's
// session selection proves each session's presence with an index seek into
// messages; without a window the selection does not touch messages at all.
//
// Revert-check: replacing auditRecentSessionsFilter's EXISTS subquery with a
// filter on sessions.updated_at (or deleting it) removes the message-index
// seek from the plan; removing the whole window returns the old behaviour.
func TestAuditQueryPlan_SinceSelectionSeeksPerSession(t *testing.T) {
	ro := openAuditPlanRO(t)

	query := "SELECT id, title, created_at, updated_at, message_count FROM sessions" +
		auditRecentSessionsFilter(time.Hour) + " ORDER BY updated_at DESC"
	plan := auditExplainQueryPlan(t, ro, query, auditSinceParams(time.Hour))
	require.Contains(t, plan, "idx_messages_session_id_created_at",
		"the EXISTS check must seek the message index, not scan messages")
	require.NotContains(t, plan, "SCAN messages")

	queryNoWindow := "SELECT id, title, created_at, updated_at, message_count FROM sessions" +
		auditRecentSessionsFilter(0) + " ORDER BY updated_at DESC"
	planNoWindow := auditExplainQueryPlan(t, ro, queryNoWindow, auditSinceParams(0))
	require.NotContains(t, planNoWindow, "messages",
		"without a window the session selection does not involve messages")
}

// TestAuditQueryPlan_BatchStateCountsChunkIsOneScan: the batch state counts
// of one chunk are one grouped statement over batch_nodes. There is no index
// on kind, so the plan documents exactly one scan per chunk — not one scan
// per session.
//
// Revert-check: returning to the per-owner loop over db.BatchHistoryStateCounts
// replaces the grouped statement with per-session lookups whose plans no
// longer carry GROUP BY owner_session_id, multiplying the scans by the
// session count.
func TestAuditQueryPlan_BatchStateCountsChunkIsOneScan(t *testing.T) {
	ro := openAuditPlanRO(t)

	query := "SELECT owner_session_id, state, count(*) AS count FROM batch_nodes" +
		" WHERE owner_session_id IN (?, ?) AND kind = 'task'" +
		" GROUP BY owner_session_id, state"
	plan := auditExplainQueryPlan(t, ro, query, []any{"a", "b"})
	require.Contains(t, plan, "SCAN batch_nodes",
		"without a kind index the chunk is one documented scan")
}
