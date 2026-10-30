package agent

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// Fixture: node facts bound, the batch store, and a real message service.
func batchQuestionFixture(t *testing.T) (*coordinator, *session.SessionNoticeStore, *sql.DB, string, context.Context) {
	t.Helper()
	ctx := context.Background()
	conn, dataDir := batchFixtureConn(t)
	q := db.New(conn)
	coord := newBatchRuntimeCoordinator()
	coord.messages = message.NewService(q)
	bindBatchNodeFacts(t, coord, conn, dataDir)
	return coord, session.NewSessionNoticeStore(conn), conn, dataDir, ctx
}

// seedDelegation reserves and announces a live delegation with a NON-ZERO
// token on both the root and child nodes (seedBatchDelegation hardcodes 0 and
// is out of scope). Mirrors seedBatchDelegation's live-state body.
func seedDelegation(t *testing.T, conn *sql.DB, owner, callID, childID string, token int64) (rootID, childNodeID string) {
	t.Helper()
	ctx := context.Background()
	ensureFixtureSession(t, conn, owner)
	bs := session.NewBatchStore(conn)
	res, err := bs.Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: "fixture-host", RunnerID: "fixture-runner", ToolCallID: callID,
		InputHash: "fixture-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none", Token: token,
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "agent", Valid: true}, ChildSessionID: sql.NullString{String: childID, Valid: true}, State: "running", ControlState: "none", Token: token,
		}}}},
	})
	require.NoError(t, err)
	rootID, childNodeID = res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
	start, err := bs.BatchAnnounceStarted(ctx, message.NewService(db.New(conn)), owner, rootID, batchFixtureMessage("fixture delegation started"))
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)
	return rootID, childNodeID
}

// seedQuestionStop makes childID's last finished assistant message a
// question stop, exactly as a paused sub-agent leaves it.
func seedQuestionStop(t *testing.T, coord *coordinator, childID string) {
	t.Helper()
	ctx := context.Background()
	qe := &AwaitingAnswerError{Question: "which one?", SessionID: childID}
	title, details := awaitingAnswerStoppedFinishText(qe, "smart")
	_, err := coord.messages.Create(ctx, childID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "checked the output"},
			message.Finish{Reason: message.FinishReasonError, Message: title, Details: details},
		},
	})
	require.NoError(t, err)
}

// commitMarkerNotice publishes the batch marker child_question notice.
func commitMarkerNotice(t *testing.T, conn *sql.DB, owner, nodeID, childID string, token int64) {
	t.Helper()
	publisher := batchCoordinatorQuestionPublisher{store: session.NewBatchStore(conn)}
	require.NoError(t, publisher.PublishChildQuestion(context.Background(), batchChildQuestionNotice{
		Owner: owner, ChildSessionID: childID, NodeID: nodeID, Token: uint64(token), Depth: 1, Sequence: 1, Question: "which one?",
	}))
}

// createSessionRow creates a bare session row with the given id.
func createSessionRow(t *testing.T, q *db.Queries, ctx context.Context, id string) {
	t.Helper()
	_, err := q.CreateSession(ctx, db.CreateSessionParams{ID: id, Title: id})
	require.NoError(t, err)
}

// TestCLIActiveWorkBatch_MarkerNoticeCommitGate: a question-paused
// delegation keeps the CLI active until its batch marker child_question
// notice is durably committed; after the commit it releases.
// Revert-check: excluding a paused child without a committed notice releases
// the CLI before the announcement and the first True assertion fails.
// Revert-check: deleting batchActiveAfterQuestionPause's committed-notice
// exclusion (answering true whenever live work exists) makes the post-commit
// False assertion fail.
func TestCLIActiveWorkBatch_MarkerNoticeCommitGate(t *testing.T) {
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const owner = "sess-batchq-owner"
	q := db.New(conn)
	createSessionRow(t, q, ctx, owner)
	createSessionRow(t, q, ctx, "sess-batchq-marker-child")
	_, nodeID := seedDelegation(t, conn, owner, "call-bq-marker", "sess-batchq-marker-child", 7)
	seedQuestionStop(t, coord, "sess-batchq-marker-child")

	active, err := coord.CLIActiveWork(ctx, owner)
	require.NoError(t, err)
	require.True(t, active, "paused child's uncommitted notice keeps the CLI active")

	commitMarkerNotice(t, conn, owner, nodeID, "sess-batchq-marker-child", 7)
	active, err = coord.CLIActiveWork(ctx, owner)
	require.NoError(t, err)
	require.False(t, active, "durably announced question alone is not active CLI work")
}

// TestCLIActiveWorkBatch_SecondLiveChildKeepsActive: a committed paused
// child must not release the CLI while a second live delegation child has
// no question stop. This test covers TWO SEPARATE roots (each Reserve
// creates its own root under the owner); the SHARED-root sibling case is
// covered by TestCLIActiveWorkBatch_SharedRootCommandKeepsActive below.
// Revert-check: excluding each root once its own child is committed would
// still release the second root here and the True assertion fails.
func TestCLIActiveWorkBatch_SecondLiveChildKeepsActive(t *testing.T) {
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const owner = "sess-batchq-owner"
	q := db.New(conn)
	createSessionRow(t, q, ctx, owner)
	createSessionRow(t, q, ctx, "sess-batchq-paused-child")
	createSessionRow(t, q, ctx, "sess-batchq-live-child")
	_, nodeID := seedDelegation(t, conn, owner, "call-bq-1", "sess-batchq-paused-child", 11)
	seedQuestionStop(t, coord, "sess-batchq-paused-child")
	commitMarkerNotice(t, conn, owner, nodeID, "sess-batchq-paused-child", 11)
	seedDelegation(t, conn, owner, "call-bq-2", "sess-batchq-live-child", 12)

	active, err := coord.CLIActiveWork(ctx, owner)
	require.NoError(t, err)
	require.True(t, active, "the second live non-question child keeps the CLI active")
}

// TestCLIActiveWorkBatch_OwnRunningCommandKeepsActive: a committed paused
// delegation plus a running own command stays active. The two seeds are
// TWO SEPARATE roots (each Reserve creates its own root); the shared-root
// sibling case is TestCLIActiveWorkBatch_SharedRootCommandKeepsActive below.
// Revert-check: excluding all live work once any child is committed makes
// the True assertion fail.
func TestCLIActiveWorkBatch_OwnRunningCommandKeepsActive(t *testing.T) {
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const owner = "sess-batchq-owner"
	q := db.New(conn)
	createSessionRow(t, q, ctx, owner)
	createSessionRow(t, q, ctx, "sess-batchq-paused-own-child")
	_, nodeID := seedDelegation(t, conn, owner, "call-bq-paused", "sess-batchq-paused-own-child", 13)
	seedQuestionStop(t, coord, "sess-batchq-paused-own-child")
	commitMarkerNotice(t, conn, owner, nodeID, "sess-batchq-paused-own-child", 13)
	seedBatchDelegation(t, conn, owner, "call-bq-own", "", "command", "running", false)

	active, err := coord.CLIActiveWork(ctx, owner)
	require.NoError(t, err)
	require.True(t, active, "a running own command keeps the CLI active")
}

// seedMixedRoot reserves ONE root with TWO live task children under it: the
// first a delegation child (Kind "task", TaskKind "agent", ChildSessionID,
// running, non-zero token on both nodes), the second a plain command
// (Kind "task", TaskKind "command", running, no child). Announces the root
// started like seedDelegation. Returns the root and the agent child's node.
func seedMixedRoot(t *testing.T, conn *sql.DB, owner, callID, childID string, token int64, commandState string) (rootID, agentNodeID string) {
	t.Helper()
	ctx := context.Background()
	bs := session.NewBatchStore(conn)
	res, err := bs.Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: "fixture-host", RunnerID: "fixture-runner", ToolCallID: callID,
		InputHash: "fixture-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none", Token: token,
		}, Children: []session.BatchReservationNode{
			{Values: db.ReserveBatchNodeParams{
				Kind: "task", TaskKind: sql.NullString{String: "agent", Valid: true}, ChildSessionID: sql.NullString{String: childID, Valid: true}, State: "running", ControlState: "none", Token: token,
			}},
			{Values: db.ReserveBatchNodeParams{
				Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: commandState, ControlState: "none", Token: token,
			}},
		}},
	})
	require.NoError(t, err)
	rootID, agentNodeID = res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
	start, err := bs.BatchAnnounceStarted(ctx, message.NewService(db.New(conn)), owner, rootID, batchFixtureMessage("fixture mixed delegation started"))
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)
	return rootID, agentNodeID
}

// TestCLIActiveWorkBatch_SharedRootCommandKeepsActive: a committed paused
// delegation child does NOT release ITS ROOT while a running command task
// sibling shares that same root.
// Revert-check: excluding the WHOLE root once its delegation child is
// committed (the round-1 root-granular bug) returns false and this test
// fails.
func TestCLIActiveWorkBatch_SharedRootCommandKeepsActive(t *testing.T) {
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const owner = "sess-bq-mixed-owner"
	const child = "sess-bq-mixed-agent"
	q := db.New(conn)
	createSessionRow(t, q, ctx, owner)
	createSessionRow(t, q, ctx, child)
	createSessionRow(t, q, ctx, "sess-bq-mixed-command")
	_, agentNode := seedMixedRoot(t, conn, owner, "call-bq-mixed", child, 41, "running")
	seedQuestionStop(t, coord, child)
	commitMarkerNotice(t, conn, owner, agentNode, child, 41)

	active, err := coord.CLIActiveWork(ctx, owner)
	require.NoError(t, err)
	require.True(t, active, "the running command sibling keeps the shared root active")
}

// TestCLIActiveWorkBatch_SharedRootPendingCommandKeepsActive: a PENDING
// command sibling (sequential / concurrency-limited, counted in
// Counts.Pending, not Counts.Active) is live work too.
// Revert-check: counting only Counts.Active excludes the root once its
// delegation child is committed and this test fails.
func TestCLIActiveWorkBatch_SharedRootPendingCommandKeepsActive(t *testing.T) {
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const owner = "sess-bq-pending-owner"
	const child = "sess-bq-pending-agent"
	q := db.New(conn)
	createSessionRow(t, q, ctx, owner)
	createSessionRow(t, q, ctx, child)
	_, agentNode := seedMixedRoot(t, conn, owner, "call-bq-pending", child, 43, "pending")
	seedQuestionStop(t, coord, child)
	commitMarkerNotice(t, conn, owner, agentNode, child, 43)

	active, err := coord.CLIActiveWork(ctx, owner)
	require.NoError(t, err)
	require.True(t, active, "the pending command sibling keeps the shared root active")
}

// TestCLIActiveWorkBatch_StaleIncarnationNoticeKeepsActive: a committed
// marker notice naming a RETIRED (terminal) v1 incarnation of a delegation
// must not commit the child's CURRENT v2 incarnation pause.
// Revert-check: text-matching historical marker notices without the
// incarnation fence (the round-1 bug) commits the v1 row and this test
// fails.
func TestCLIActiveWorkBatch_StaleIncarnationNoticeKeepsActive(t *testing.T) {
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const owner = "sess-bq-stale-owner"
	const child = "sess-bq-stale-child"
	q := db.New(conn)
	createSessionRow(t, q, ctx, owner)
	createSessionRow(t, q, ctx, child)
	rootV1, nodeV1 := seedDelegation(t, conn, owner, "call-bq-v1", child, 5)
	commitMarkerNotice(t, conn, owner, nodeV1, child, 5)
	require.NoError(t, session.NewBatchStore(conn).Project(ctx, session.BatchProjection{
		Host: "fixture-host", RunnerID: "fixture-runner", Sequence: 2,
		Roots: []session.BatchRootProjection{{Owner: owner, Root: rootV1, Updates: []db.ProjectBatchNodeParams{
			{NodeID: rootV1, State: "completed", OutcomeState: "completed", OutcomeSummary: "v1 retired", ControlState: "none"},
			{NodeID: nodeV1, State: "completed", OutcomeState: "completed", OutcomeSummary: "v1 retired", ControlState: "none"},
		}}},
	}))
	seedDelegation(t, conn, owner, "call-bq-v2", child, 6)
	seedQuestionStop(t, coord, child)

	active, err := coord.CLIActiveWork(ctx, owner)
	require.NoError(t, err)
	require.True(t, active, "the current incarnation's uncommitted pause keeps the CLI active despite the stale committed marker")
}

// TestCLIActiveWorkBatch_NestedOwnerCommitReleases: a question paused three
// levels down commits under its IMMEDIATE owner (the mid-chain delegation
// session), and the fully dormant nested scope releases the top-level CLI.
// Revert-check: reading every descendant's notices under the top-level CLI
// root instead of the delegation's immediate owner (the round-1 bug) finds
// nothing and this test fails.
func TestCLIActiveWorkBatch_NestedOwnerCommitReleases(t *testing.T) {
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const ownerA = "sess-bq-chain-a"
	const ownerB = "sess-bq-chain-b"
	const childC = "sess-bq-chain-c"
	q := db.New(conn)
	createSessionRow(t, q, ctx, ownerA)
	createSessionRow(t, q, ctx, ownerB)
	createSessionRow(t, q, ctx, childC)
	seedDelegation(t, conn, ownerA, "call-bq-chain-1", ownerB, 31)
	_, nodeC := seedDelegation(t, conn, ownerB, "call-bq-chain-2", childC, 32)
	seedQuestionStop(t, coord, childC)
	commitMarkerNotice(t, conn, ownerB, nodeC, childC, 32)

	active, err := coord.CLIActiveWork(ctx, ownerA)
	require.NoError(t, err)
	require.False(t, active, "the fully paused nested scope releases the CLI")
}

// TestCLIActiveWorkBatch_NestedUncommittedKeepsActive: same nested chain,
// but the deepest question is NOT committed; the conduit must stay active.
// Revert-check: marking a conduit dormant from its nested child's PAUSE
// alone, without the committed-notice requirement, releases the CLI and this
// test fails.
func TestCLIActiveWorkBatch_NestedUncommittedKeepsActive(t *testing.T) {
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const ownerA = "sess-bq-chain-a"
	const ownerB = "sess-bq-chain-b"
	const childC = "sess-bq-chain-c"
	q := db.New(conn)
	createSessionRow(t, q, ctx, ownerA)
	createSessionRow(t, q, ctx, ownerB)
	createSessionRow(t, q, ctx, childC)
	seedDelegation(t, conn, ownerA, "call-bq-chain-1", ownerB, 31)
	seedDelegation(t, conn, ownerB, "call-bq-chain-2", childC, 32)
	seedQuestionStop(t, coord, childC)

	active, err := coord.CLIActiveWork(ctx, ownerA)
	require.NoError(t, err)
	require.True(t, active, "the nested paused child without a committed notice keeps the CLI active")
}
