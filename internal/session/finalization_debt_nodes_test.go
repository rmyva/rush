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

// finalizationSeedNodeDebt reserves one batch root for owner with two task
// children, projects the first child terminal with a selected wake notice
// (batch_store_test.go's literal shape), and announces the root so the debt
// goes live: exactly one pending node debt row. The root's row keeps
// delivery 'none' and notice_selected 0, so only the child is debt.
func finalizationSeedNodeDebt(t *testing.T, conn *sql.DB, ctx context.Context, owner, toolCallID string) (rootID, childID string) {
	t.Helper()
	s := NewBatchStore(conn)
	reserved, err := s.Reserve(ctx, BatchReservation{
		Owner: owner, Host: "host", ToolCallID: toolCallID, InputHash: "hash-" + toolCallID, Sequence: 1,
		Root: BatchReservationNode{
			Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: batchStoreTestString("parallel"), State: "pending", ControlState: "none"},
			Children: []BatchReservationNode{
				batchStoreTestTask(), batchStoreTestTask(),
			},
		},
	})
	require.NoError(t, err)
	snap := reserved.Snapshot
	rootID, childID = snap.Root.RootID, snap.Nodes[1].NodeID
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: 2, Roots: []BatchRootProjection{{
		Owner:   owner,
		Root:    rootID,
		Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(childID, "completed")},
		Notices: []db.SelectBatchNodeNoticeParams{batchStoreTestNotice(childID)},
	}}}))
	_, err = db.New(conn).BatchOutboxAnnounce(ctx, db.BatchOutboxAnnounceParams{
		Owner: owner, RootID: rootID,
		MessageID: sql.NullString{String: "announce-" + toolCallID, Valid: true},
		UpdatedAt: time.Now().Unix(),
	})
	require.NoError(t, err)
	return rootID, childID
}

// finalizationFlipNode forces one batch_nodes row's columns directly in the
// database, the deterministic lever for classifying node debt states (the
// same lever style as finalizationFlipNotice).
func finalizationFlipNode(t *testing.T, conn *sql.DB, ctx context.Context, nodeID string, sets string) {
	t.Helper()
	result, err := conn.ExecContext(ctx, `UPDATE batch_nodes SET `+sets+` WHERE node_id=?`, nodeID)
	require.NoError(t, err)
	changed, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, changed, "the flip lever must hit exactly the one node")
}

// finalizationDebtNodeSealEnv is finalizationDebtSealEnv with the node debt
// seeded before the bind: the S1 ingress fence refuses batch reservations
// for an owner that already has a bound finalization scope, so the root
// must exist first (newReservedScopeEnv's ordering, finalization_seal_test.go).
func finalizationDebtNodeSealEnv(t *testing.T) candidateEnv {
	t.Helper()
	_, conn, ctx := batchStoreTestDB(t)
	finalizationSeedNodeDebt(t, conn, ctx, "owner", "call")
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

// Revert-check: a reader missing the batch_nodes half or drifting from the
// join predicate (notice_selected, announced root, wake, reacted, delivery)
// fails one of these counts.
func TestFinalizationReactionDebtTxCountsNodeDebts(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	reader := NewFinalizationDebtStore(env.conn)
	tx, err := env.conn.BeginTx(env.ctx, nil)
	require.NoError(t, err)
	count, err := reader.FinalizationReactionDebtTx(env.ctx, tx, "owner")
	require.NoError(t, err)
	require.Equal(t, 0, count, "a fresh owner owes no node debt")
	require.NoError(t, tx.Rollback())
	_, child := finalizationSeedNodeDebt(t, env.conn, env.ctx, "owner", "call")
	tx, err = env.conn.BeginTx(env.ctx, nil)
	require.NoError(t, err)
	count, err = reader.FinalizationReactionDebtTx(env.ctx, tx, "owner")
	require.NoError(t, err)
	require.Equal(t, 1, count, "one pending selected notice is one node debt")
	require.NoError(t, tx.Rollback())
	// The pulled half stays debt: delivered into history, reaction owed.
	finalizationFlipNode(t, env.conn, env.ctx, child, `delivery='done'`)
	tx, err = env.conn.BeginTx(env.ctx, nil)
	require.NoError(t, err)
	count, err = reader.FinalizationReactionDebtTx(env.ctx, tx, "owner")
	require.NoError(t, err)
	require.Equal(t, 1, count, "a delivered-unreacted node stays debt")
	require.NoError(t, tx.Rollback())
	finalizationSeedNodeDebt(t, env.conn, env.ctx, "owner", "call-2")
	tx, err = env.conn.BeginTx(env.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	count, err = reader.FinalizationReactionDebtTx(env.ctx, tx, "owner")
	require.NoError(t, err)
	require.Equal(t, 2, count, "each root's live node debt sums into the owner's total")
}

// Revert-check: dropping any predicate term — announced, root-void, wake,
// reacted, delivery — counts one of the excluded roots; a lost owner scope
// bleeds the foreign session's debt into the owner's count.
func TestFinalizationReactionDebtTxNodeDebtClassification(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	reader := NewFinalizationDebtStore(env.conn)
	// The one live debt the owner keeps: a pending selected notice under an
	// announced, non-void root.
	finalizationSeedNodeDebt(t, env.conn, env.ctx, "owner", "call")
	// A consumed row: the ordinary real-step reaction already marked it.
	_, reactedChild := finalizationSeedNodeDebt(t, env.conn, env.ctx, "owner", "call-reacted")
	finalizationFlipNode(t, env.conn, env.ctx, reactedChild, `reacted=1`)
	// A non-wake selected notice: describes an outcome, never wakes.
	_, quietChild := finalizationSeedNodeDebt(t, env.conn, env.ctx, "owner", "call-quiet")
	finalizationFlipNode(t, env.conn, env.ctx, quietChild, `wake=0`)
	// A voided row: the pull's void condition retired it.
	_, voidChild := finalizationSeedNodeDebt(t, env.conn, env.ctx, "owner", "call-void")
	finalizationFlipNode(t, env.conn, env.ctx, voidChild, `delivery='void'`)
	// An unannounced root: the debt is not live before the announcement.
	unannouncedRoot, _ := finalizationSeedNodeDebt(t, env.conn, env.ctx, "owner", "call-unannounced")
	finalizationFlipNode(t, env.conn, env.ctx, unannouncedRoot, `announced=0`)
	// A root voided by a rerun: its subtree is never debt (R12-F), and the
	// lever hits the root row itself (node_id == root_id).
	voidRoot, _ := finalizationSeedNodeDebt(t, env.conn, env.ctx, "owner", "call-rootvoid")
	// The batch_nodes CHECK lets only a selected row take delivery='void'.
	finalizationFlipNode(t, env.conn, env.ctx, voidRoot,
		`notice_selected=1, notice_kind='root', notice_state='completed', notice_cause='',
		 notice_sequence=99, notice_ordinal=0, delivery='void', wake=0`)
	// A second, unbound session owes its own debt and must not bleed into
	// the owner's count.
	_, err := env.svc.CreateTaskSession(env.ctx, "other", "owner", "unbound")
	require.NoError(t, err)
	finalizationSeedNodeDebt(t, env.conn, env.ctx, "other", "call-other")
	tx, err := env.conn.BeginTx(env.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	count, err := reader.FinalizationReactionDebtTx(env.ctx, tx, "owner")
	require.NoError(t, err)
	require.Equal(t, 1, count, "only the live pending node debt counts")
	other, err := reader.FinalizationReactionDebtTx(env.ctx, tx, "other")
	require.NoError(t, err)
	require.Equal(t, 1, other, "the other session's debt stays scoped to it")
	var delivery, state string
	require.NoError(t, tx.QueryRowContext(env.ctx,
		`SELECT delivery, state FROM batch_nodes WHERE node_id=?`, reactedChild).Scan(&delivery, &state))
	require.Equal(t, "pending", delivery, "the reacted flip must keep the delivery")
	require.Equal(t, "completed", state, "the flip lever must not disturb state")
	require.NoError(t, tx.QueryRowContext(env.ctx,
		`SELECT delivery FROM batch_nodes WHERE node_id=?`, voidChild).Scan(&delivery))
	require.Equal(t, "void", delivery)
	require.NoError(t, tx.QueryRowContext(env.ctx,
		`SELECT delivery FROM batch_nodes WHERE node_id=?`, voidRoot).Scan(&delivery))
	require.Equal(t, "void", delivery, "the root-void lever hits the root row itself")
}

// Revert-check: a settle missing the batch_nodes half, a deleting or
// voiding settle, a fabricated reacted_failed receipt, or a lost owner
// scoping fails one of these assertions.
func TestSettleFinalizationDebtsSettlesNodeAndNoticeDebtsTogether(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	settler := NewFinalizationDebtStore(env.conn)
	reader := NewFinalizationDebtStore(env.conn)
	finalizationSeamInsertDebt(t, env, "owner")
	finalizationSeamInsertDebt(t, env, "owner")
	_, pendingChild := finalizationSeedNodeDebt(t, env.conn, env.ctx, "owner", "call")
	_, doneChild := finalizationSeedNodeDebt(t, env.conn, env.ctx, "owner", "call-done")
	finalizationFlipNode(t, env.conn, env.ctx, doneChild, `delivery='done'`)
	// A second, unbound session keeps its own unsettled debts.
	_, err := env.svc.CreateTaskSession(env.ctx, "other", "owner", "unbound")
	require.NoError(t, err)
	finalizationSeamInsertDebt(t, env, "other")
	finalizationSeedNodeDebt(t, env.conn, env.ctx, "other", "call-other")
	settled, err := settler.SettleFinalizationDebts(env.ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, 4, settled, "two notice debts plus one pending and one done node debt")
	require.Equal(t, 0, finalizationDebtCountTx(t, env.conn, env.ctx, reader, "owner"),
		"after the settle the owner owes no debt in either table")
	// The pending node row stays alive and receipted, not deleted or voided:
	// the parent's later pull still delivers the child's result content.
	var delivery, state string
	var selected, wake, reacted, reactedFailed int64
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT delivery, state, notice_selected, wake, reacted, reacted_failed
		 FROM batch_nodes WHERE node_id=?`, pendingChild).
		Scan(&delivery, &state, &selected, &wake, &reacted, &reactedFailed))
	require.Equal(t, "pending", delivery, "a settle marks, it never voids the pending delivery")
	require.Equal(t, "completed", state, "a settle marks, it never disturbs the node state")
	require.EqualValues(t, 1, selected, "the selected notice identity stays")
	require.EqualValues(t, 1, wake, "the wake column stays")
	require.EqualValues(t, 1, reacted, "the settled row carries the reacted receipt")
	require.Zero(t, reactedFailed, "a settle never writes the failure signal")
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT delivery, reacted FROM batch_nodes WHERE node_id=?`, doneChild).Scan(&delivery, &reacted))
	require.Equal(t, "done", delivery, "delivered rows keep their delivery")
	require.EqualValues(t, 1, reacted)
	settled, err = settler.SettleFinalizationDebts(env.ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, 0, settled, "a repeat settle finds no matching rows")
	require.Equal(t, 2, finalizationDebtCountTx(t, env.conn, env.ctx, reader, "other"),
		"the other session still owes its own notice and node debt")
	var otherSettled int64
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT count(*) FROM batch_nodes WHERE owner_session_id='other' AND reacted=1`).
		Scan(&otherSettled))
	require.Zero(t, otherSettled, "another session's node debt stays unsettled")
}

// Revert-check: a reader and settle that disagree on the node half let the
// seal proceed with an open node debt, or never seal after the settle.
func TestFinalizationSealRefusesNodeDebtUntilSettled(t *testing.T) {
	e := finalizationDebtNodeSealEnv(t)
	store := NewFinalizationDebtStore(e.conn)
	_, presented := sealStoredCandidate(t, e, 0, 1)
	reader := finalizationDebtScopeReader(e.id.SessionID, store)
	outcome, err := e.store.TrySeal(t.Context(), e.id, presented, e.sealWriter(t), reader)
	require.NoError(t, err)
	require.False(t, outcome.Sealed, "an open node debt must refuse the seal")
	require.False(t, outcome.Accepted)
	require.Equal(t, AgentFinalizationSealReactionDebt, outcome.Reason)
	settled, err := store.SettleFinalizationDebts(t.Context(), e.id.SessionID)
	require.NoError(t, err)
	require.Equal(t, 1, settled, "exactly the one seeded node debt settles")
	require.Equal(t, 0, finalizationDebtCountTx(t, e.conn, t.Context(), store, e.id.SessionID),
		"the settle must clear exactly what the reader counts")
	outcome, err = e.store.TrySeal(t.Context(), e.id, presented, e.sealWriter(t), reader)
	require.NoError(t, err)
	require.True(t, outcome.Sealed, "a settled node debt lets the seal proceed")
}

// finalizationNodeDebtLateReader wraps the composite reader and selects a
// real node notice while the seal is deciding, through the seal's own
// transaction; it records the debt counter before and after the injection
// and the rows the injection touched.
type finalizationNodeDebtLateReader struct {
	inner     AgentFinalizationScopeReader
	owner     string
	root      string
	child     string
	firstRead int
	lateRead  int
	selected  int64
}

var _ AgentFinalizationScopeReader = (*finalizationNodeDebtLateReader)(nil)

// FinalizationSealScopeFacts reads the wrapped facts, selects the prepared
// terminal child's notice inside the passed transaction with the field
// wiring batchProjectRoot uses (RootID/HostID/OwnerSessionID/RunnerID, the
// projection sequence as notice_sequence), and re-reads the facts so the
// seal decides on the late node debt too.
func (r *finalizationNodeDebtLateReader) FinalizationSealScopeFacts(ctx context.Context, tx *sql.Tx) (AgentFinalizationScopeFacts, error) {
	facts, err := r.inner.FinalizationSealScopeFacts(ctx, tx)
	if err != nil {
		return AgentFinalizationScopeFacts{}, err
	}
	r.firstRead = facts.ReactionDebt
	notice := batchStoreTestNotice(r.child)
	notice.RootID, notice.HostID, notice.OwnerSessionID, notice.RunnerID = r.root, "host", r.owner, ""
	notice.NoticeSequence = sql.NullInt64{Int64: 3, Valid: true}
	notice.UpdatedAt = time.Now().Unix()
	selected, err := db.New(tx).SelectBatchNodeNotice(ctx, notice)
	if err != nil {
		return AgentFinalizationScopeFacts{}, err
	}
	r.selected = selected
	late, err := r.inner.FinalizationSealScopeFacts(ctx, tx)
	if err != nil {
		return AgentFinalizationScopeFacts{}, err
	}
	r.lateRead = late.ReactionDebt
	return late, nil
}

// Revert-check: a reader blind to debt produced mid-seal reads a stale
// count and seals prematurely; the firstRead, lateRead, selection-count and
// rollback assertions catch it.
func TestFinalizationSealSeesNodeDebtProducedWhileSealing(t *testing.T) {
	_, conn, ctx := batchStoreTestDB(t)
	// The S1 fence refuses batch reservations and projections for a bound
	// owner, so the whole batch shape must exist before the bind: child1
	// selected as the seeded debt, child2 terminal but unselected.
	root, child1 := finalizationSeedNodeDebt(t, conn, ctx, "owner", "call")
	var child2 string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT node_id FROM batch_nodes
		 WHERE root_id=? AND node_id != root_id AND node_id != ?`, root, child1).Scan(&child2))
	require.NoError(t, NewBatchStore(conn).Project(ctx, BatchProjection{
		Host: "host", Sequence: 3,
		Roots: []BatchRootProjection{{
			Owner:   "owner",
			Root:    root,
			Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(child2, "completed")},
		}},
	}))
	messages := message.NewService(db.New(conn))
	store := NewAgentFinalizationStore(conn)
	id := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "logical", Generation: 1}
	require.NoError(t, store.Bind(ctx, id))
	writer, ok := messages.(message.FinalizationWriter)
	require.True(t, ok)
	reader, ok := messages.(message.FinalizationReader)
	require.True(t, ok)
	e := candidateEnv{conn: conn, store: store, messages: messages, writer: writer, reader: reader, id: id}
	debt := NewFinalizationDebtStore(conn)
	late := &finalizationNodeDebtLateReader{
		inner: finalizationDebtScopeReader(e.id.SessionID, debt),
		owner: e.id.SessionID,
		root:  root,
		child: child2,
	}
	_, presented := sealStoredCandidate(t, e, 0, 1)
	outcome, err := e.store.TrySeal(t.Context(), e.id, presented, e.sealWriter(t), late)
	require.NoError(t, err)
	require.False(t, outcome.Sealed, "a node debt produced while sealing must refuse the seal")
	require.Equal(t, AgentFinalizationSealReactionDebt, outcome.Reason)
	require.Equal(t, 1, late.firstRead, "one seeded node debt before the injection")
	require.Equal(t, 2, late.lateRead, "the reader must observe the notice it selected")
	require.EqualValues(t, 1, late.selected, "the injection must select exactly the prepared child")
	// The injection rolled back with the refused seal: child2 is unselected
	// again and the debt count is back to the seeded row alone.
	require.Equal(t, 1, finalizationDebtCountTx(t, e.conn, t.Context(), debt, e.id.SessionID))
	var selected int64
	require.NoError(t, e.conn.QueryRowContext(t.Context(),
		`SELECT notice_selected FROM batch_nodes WHERE node_id=?`, child2).Scan(&selected))
	require.Zero(t, selected, "the injected notice must not survive the rollback")
	settled, err := debt.SettleFinalizationDebts(t.Context(), e.id.SessionID)
	require.NoError(t, err)
	require.Equal(t, 1, settled)
	require.Equal(t, 0, finalizationDebtCountTx(t, e.conn, t.Context(), debt, e.id.SessionID))
	outcome, err = e.store.TrySeal(t.Context(), e.id, presented, e.sealWriter(t),
		finalizationDebtScopeReader(e.id.SessionID, debt))
	require.NoError(t, err)
	require.True(t, outcome.Sealed, "a settled debt lets the seal proceed")
}
