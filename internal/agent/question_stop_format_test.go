package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestQuestionStopFormatRecoverDeadHost pins the retained halves of the
// question-stop contract in the batch world:
//
//  1. awaitingAnswerStoppedFinishText's shared framing (title + QUESTION
//     details) -- the producer/recovery divergence guard, unchanged.
//  2. a dead-host delegation root is recovered to 'interrupted' with
//     outcome host_lost (the batch recovery's own classification).
//
// The legacy assertion that the recovered ROW's outcome summary carries the
// "SUB-AGENT QUESTION (session X): ..." framing is an obsolete contract: in
// the batch world the question framing is delivered at PUBLISH time (the
// child_question session notice, batch_install_question.go), while recovery
// marks the node interrupted; the interruption notice text is
// buildBatchNoticeMessageParams's, pinned by
// TestBatchNoticeClassification_RecoveredNodeIsInterruption.
func TestQuestionStopFormatRecoverDeadHost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, dataDir := batchFixtureConn(t)
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	child, err := sessions.Create(ctx, "question child")
	require.NoError(t, err)

	const question = "Should I ship or hold?"
	title, details := awaitingAnswerStoppedFinishText(&AwaitingAnswerError{
		Question: question, Options: []string{"ship", "hold"}, SessionID: child.ID,
	}, "smart")
	require.Equal(t, "Stopped: agent asked a question and is awaiting an answer", title)
	require.Contains(t, details, "QUESTION: "+question)
	require.Contains(t, details, "\n\nThis is not a crash")
	// The framing round-trips through history verbatim (preamble preserved).
	_, err = message.NewService(q).Create(ctx, child.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Checked the output; one detail is unclear."},
			message.Finish{Reason: message.FinishReasonError, Message: title, Details: details},
		},
	})
	require.NoError(t, err)

	// A live delegation root on a host whose lock is gone: the recovery must
	// classify it interrupted/host_lost (the batch row half of the old
	// dead-host question scenario).
	const host = "question-format-dead-host"
	root, _ := b1SeedBatchRootOnHost(t, conn, "question-parent", "question-format-job", host, "running")
	_, _, err = session.NewBatchStore(conn).RecoverHostsOnce(ctx, func(string) (session.HostLockStatus, error) {
		return session.ProbeHostShared(dataDir, host)
	}, []string{host}, 8)
	require.NoError(t, err)
	var state, outcome string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT state, coalesce(outcome_code,'') FROM batch_nodes WHERE node_id=?`, root).Scan(&state, &outcome))
	require.Equal(t, "interrupted", state)
	require.Equal(t, "host_lost", outcome)
}
