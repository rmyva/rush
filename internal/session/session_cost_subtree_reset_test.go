package session

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
	"modernc.org/sqlite"
)

func TestResetCostBase_AtomicSubtreeSnapshot(t *testing.T) {
	ctx := context.Background()
	conn, q := newTestDB(t)
	svc := NewService(q, conn)

	root, err := svc.Create(ctx, "reset-root")
	require.NoError(t, err)
	child, err := svc.CreateTaskSession(ctx, "reset-child", root.ID, "child")
	require.NoError(t, err)
	grandchild, err := svc.CreateTaskSession(ctx, "reset-grandchild", child.ID, "grandchild")
	require.NoError(t, err)
	_, err = svc.IncrementCost(ctx, root.ID, 0.10)
	require.NoError(t, err)
	_, err = svc.IncrementCost(ctx, child.ID, 0.20)
	require.NoError(t, err)
	_, err = svc.IncrementCost(ctx, grandchild.ID, 0.30)
	require.NoError(t, err)

	require.NoError(t, svc.ResetCostBase(ctx, root.ID))
	var base, self float64
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT cost_base, cost_self FROM sessions WHERE id = ?`, root.ID).Scan(&base, &self))
	require.InDelta(t, 0.60, base, 1e-9)
	require.InDelta(t, 0.10, self, 1e-9, "reset must not change the monotonic own-cost ledger")
	budget, err := svc.SubtreeBudget(ctx, root.ID)
	require.NoError(t, err)
	require.InDelta(t, 0, budget, 1e-9)

	_, err = svc.IncrementCost(ctx, child.ID, 0.15)
	require.NoError(t, err)
	budget, err = svc.SubtreeBudget(ctx, root.ID)
	require.NoError(t, err)
	require.InDelta(t, 0.15, budget, 1e-9, "later subtree spend remains chargeable")

	previousBase := base
	require.NoError(t, svc.ResetCostBase(ctx, root.ID))
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT cost_base FROM sessions WHERE id = ?`, root.ID).Scan(&base))
	require.InDelta(t, 0.75, base, 1e-9)
	require.Greater(t, base, previousBase, "the second reset must include spending since the first reset")
	budget, err = svc.SubtreeBudget(ctx, root.ID)
	require.NoError(t, err)
	require.InDelta(t, 0, budget, 1e-9)
}

func TestResetCostBase_PerformsUpdateBeforePostResetRead(t *testing.T) {
	ctx := context.Background()
	recorder := &resetSQLRecorder{}
	path := filepath.Join(t.TempDir(), "reset-recording.db")
	conn := sql.OpenDB(resetRecordingConnector{inner: new(sqlite.Driver), dsn: path, recorder: recorder})
	t.Cleanup(func() { _ = conn.Close() })

	_, err := conn.ExecContext(ctx, `CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			parent_session_id TEXT,
			title TEXT NOT NULL,
			message_count INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			cost REAL NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT 0,
			summary_message_id TEXT,
			todos TEXT,
			smart_model_provider TEXT,
			smart_model_id TEXT,
			smart_model_reasoning_effort TEXT DEFAULT 'medium',
			fast_model_provider TEXT,
			fast_model_id TEXT,
			fast_model_reasoning_effort TEXT DEFAULT 'medium',
			worker_model_provider TEXT,
			worker_model_id TEXT,
			worker_model_reasoning_effort TEXT,
			reviewer_model_provider TEXT,
			reviewer_model_id TEXT,
			reviewer_model_reasoning_effort TEXT,
			system_prompt TEXT DEFAULT '',
			yolo_enabled INTEGER NOT NULL DEFAULT 0,
			deleted_todos TEXT NOT NULL DEFAULT '[]',
			cancel_requested INTEGER NOT NULL DEFAULT 0,
			ended_reason TEXT NOT NULL DEFAULT '',
			budget_max_cost REAL NOT NULL DEFAULT 0,
			budget_max_tokens INTEGER NOT NULL DEFAULT 0,
			budget_timeout_sec INTEGER NOT NULL DEFAULT 0,
			parent_cost_accounted REAL NOT NULL DEFAULT 0,
			origin TEXT DEFAULT '' NOT NULL,
			cost_self REAL NOT NULL DEFAULT 0,
			cost_base REAL NOT NULL DEFAULT 0,
			cost_parent_id TEXT NOT NULL DEFAULT '',
			workspace_root TEXT NOT NULL DEFAULT '',
			git_branch TEXT NOT NULL DEFAULT ''
		)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `INSERT INTO sessions (id, title, cost_self, cost_parent_id) VALUES ('root', 'root', 1, '')`)
	require.NoError(t, err)
	recorder.reset()

	q := db.New(conn)
	svc := NewService(q, conn)
	require.NoError(t, svc.ResetCostBase(ctx, "root"))

	recorder.mu.Lock()
	ops := append([]resetSQLOp(nil), recorder.ops...)
	recorder.mu.Unlock()
	require.Len(t, ops, 2)
	require.Equal(t, "write", ops[0].kind)
	require.True(t, strings.HasPrefix(ops[0].query, "WITH RECURSIVE"), "first database call must be the atomic update")
	require.Equal(t, "read", ops[1].kind, "only the post-reset event refresh may read")
	require.Contains(t, ops[1].query, "GETSESSION", "recorded read must be the post-reset event refresh")
}

type resetSQLOp struct {
	kind  string
	query string
}

type resetSQLRecorder struct {
	mu  sync.Mutex
	ops []resetSQLOp
}

func (r *resetSQLRecorder) record(kind, query string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, resetSQLOp{kind: kind, query: strings.ToUpper(strings.TrimSpace(query))[:min(20, len(strings.TrimSpace(query)))]})
}

func (r *resetSQLRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = nil
}

type resetRecordingConnector struct {
	inner    driver.Driver
	dsn      string
	recorder *resetSQLRecorder
}

func (c resetRecordingConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.inner.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return resetRecordingConn{Conn: conn, recorder: c.recorder}, nil
}

func (c resetRecordingConnector) Driver() driver.Driver { return c.inner }

type resetRecordingConn struct {
	driver.Conn
	recorder *resetSQLRecorder
}

func (c resetRecordingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.recorder.record("write", query)
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c resetRecordingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.recorder.record("read", query)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}
