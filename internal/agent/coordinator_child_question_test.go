// Delegated-child scope helpers and the retained first-turn question
// presentation test. The deleted ledger park/delivery tests
// (TestChildDrainQuestion_ReachesParent, TestChildScope_DeferredDebtReleases-
// Delegation) were obsolete contracts: the workLedger park/armDelegation/
// onWebDone delivery channel they exercised is gone -- in the batch world a
// delegation's outcome is delivered by the batch executor, and a child
// question reaches the parent through the batch question machinery
// (LiveHeldQuestions + commitMarkerNotice + deliverBatchHeldAnswer), pinned by
// cli_active_work_batch_question_test.go, batch_notice_presentation_test.go
// and coordinator_agent_inject_batch_test.go. The scope halves (deferred debt,
// rerun holds) live on coordinator_drain_policy/coordinator_await_tasks and
// their batch twins are pinned by batch_scope_recovery_test.go and
// TestChildScope_RerunHoldKeepsDelegationOpen below.
package agent

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// childDelegationBase is the batch-only delegation fixture: a parent session
// delegating to a child session whose driver is a real *sessionAgent, with
// the LIVE parent->child delegation link seeded in batch_nodes
// (seedBatchDelegation, state "running"). The deleted ledger half (Start +
// acknowledge + the onWebDone delivery channel) is gone: batch delegation
// state is durable, seeded only through the constraint-valid helpers.
func childDelegationBase(t *testing.T, o attemptFixtureOpts) (*attemptFixture, string, string) {
	t.Helper()
	f := newAttemptFixture(t, "child-question-root", o)
	f.coord.messages = f.env.messages
	f.coord.sessions = f.env.sessions

	parent, err := f.env.sessions.Create(context.Background(), "parent")
	require.NoError(t, err)
	child, err := f.env.sessions.CreateTaskSession(context.Background(), "task-q", parent.ID, "child")
	require.NoError(t, err)
	f.coord.subAgentDrivers.register(child.ID, subAgentDriver{
		agent: f.sa, call: SessionAgentCall{SessionID: child.ID}, parentSessionID: parent.ID,
	})
	seedBatchDelegation(t, f.env.conn, parent.ID, "delegate-1", child.ID, "agent", "running", false)
	return f, parent.ID, child.ID
}

// TestRunSubAgent_FirstTurnQuestionKeepsPreamble: the child's FIRST turn ends
// on a question it asked after writing a sentence: the parent's tool response
// is the question (not an error) and keeps the sentence as its preamble.
//
// Revert-check: building the response from subAgentQuestionText alone (no
// childQuestionPreamble) drops the sentence and this test goes red.
func TestRunSubAgent_FirstTurnQuestionKeepsPreamble(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	c6ConfigureProvider(t, coord, "smart-provider", "pinned-model", "key", nil)
	parent, err := env.sessions.Create(t.Context(), "question parent")
	require.NoError(t, err)

	agent := &mockSessionAgent{model: c6BuildModel(t, coord, true)}
	agent.runFunc = func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		qe := &AwaitingAnswerError{Question: "which one?", SessionID: call.SessionID}
		title, details := awaitingAnswerStoppedFinishText(qe, "smart")
		_, createErr := env.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "Checked the job output; one detail is unclear."},
				message.Finish{Reason: message.FinishReasonError, Message: title, Details: details},
			},
		})
		require.NoError(t, createErr)
		return nil, qe
	}
	coord.currentAgent = agent

	resp, err := coord.runSubAgent(t.Context(), subAgentParams{
		Agent: agent, SessionID: parent.ID, AgentMessageID: "message", ToolCallID: "tool",
		Prompt: "do the work", SessionTitle: "question child",
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "a question is not a failure")
	require.Contains(t, resp.Content, "Checked the job output; one detail is unclear.")
	require.Contains(t, resp.Content, "SUB-AGENT QUESTION")
	require.Contains(t, resp.Content, "which one?")
	require.Less(t, strings.Index(resp.Content, "Checked the job output"), strings.Index(resp.Content, "SUB-AGENT QUESTION"))
}

// TestChildScope_RerunHoldKeepsDelegationOpen (R3C-5): a rerun holds the
// child's automatic turns while it cancels, truncates and hands off. The hold
// is temporary, so the child's scope reads as paced (open), not deferred; once
// the hold is released, the child's owed debt reads as owed again. (The
// ledger-era "delegation is not released during the hold" delivery assertion
// rode the deleted armDelegation/onWebDone channel; the batch executor owns
// delegation delivery now.)
//
// Revert-check: answering "deferred" for a held session (the old verdict)
// reads the child as drained instead of paced: this test's DrainPaced
// assertion goes red.
func TestChildScope_RerunHoldKeepsDelegationOpen(t *testing.T) {
	ctx := context.Background()
	f, _, childID := childDelegationBase(t, attemptFixtureOpts{noIdle: true})
	// The child owes selected (wake-armed) debt, so its post-hold scope is
	// owed, not empty.
	seedFixtureJobDebt(t, f.env.conn, childID, "hold-call", false)
	release := f.coord.HoldAutomaticTurns(childID)
	t.Cleanup(release)

	st, err := f.coord.CLIScope(ctx, childID)
	require.NoError(t, err)
	require.Equal(t, DrainPaced, st.Drain, "a hold is a temporary pause, not a refusal")
	require.True(t, st.RetryAt.IsZero(), "no clock: the tick and the release retry it")

	release()
	st, err = f.coord.CLIScope(ctx, childID)
	require.NoError(t, err)
	require.Equal(t, DrainOwed, st.Drain, "the debt is owed again after the hold")
}
