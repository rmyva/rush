// The await_tasks tool (#1270) end to end through the real `rush run` loop:
// the seeded delegation rows are the same shape app_run_question_open_work_test.go
// uses (reserved through the batch projection, owner = the loop's session),
// the provider is the httptest server, and the await_tasks call is
// scripted as an ordinary tool call in the first turn.
package app

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// claimDelegation seeds one live `agent` delegation owned by owner, announced
// so the loop treats it as open work from the first turn on. Batch-only:
// the f6 helper reserves a constraint-valid batch root + task node
// (Reserve-based, internal/db/migrations/20261012000001_batch_projection.sql).
func claimDelegation(t *testing.T, ctx context.Context, application *App, owner, toolCallID, childID string) {
	t.Helper()
	f6ClaimDelegation(t, ctx, application, owner, toolCallID, childID)
}

// completeDelegation completes one delegation through the batch projection;
// wake turns the completion into reaction debt.
func completeDelegation(t *testing.T, ctx context.Context, application *App, owner, toolCallID, summary string, wake bool) {
	t.Helper()
	f6CompleteDelegation(t, ctx, application, owner, toolCallID, summary, wake)
}

// cliScopeOf reads the coordinator's ONE between-turns decision.
func cliScopeOf(t *testing.T, application *App, sessionID string) agent.CLIScopeState {
	t.Helper()
	src, ok := application.AgentCoordinator.(agent.ReactionDebtSource)
	require.True(t, ok, "the coordinator must implement ReactionDebtSource")
	state, err := src.CLIScope(context.Background(), sessionID)
	require.NoError(t, err)
	return state
}

// hoistOnceWake pulls the session's once schedule forward to ~1s from now and
// tells the in-process scheduler to recompute, so a max_wait_seconds floor of
// 60 does not cost the test a real minute.
func hoistOnceWake(t *testing.T, application *App, sessionID string) {
	t.Helper()
	ctx := context.Background()
	var id string
	require.Eventually(t, func() bool {
		rows, err := application.WakeScheduleStore().ListSchedules(ctx, sessionID)
		if err != nil || len(rows) != 1 {
			return false
		}
		id = rows[0].ID
		return rows[0].State == "active"
	}, 10*time.Second, 20*time.Millisecond, "the max_wait once schedule is active")
	_, err := application.DB().ExecContext(ctx,
		`UPDATE wake_schedules SET next_run_at = ? WHERE id = ?`,
		time.Now().Add(time.Second).Unix(), id)
	require.NoError(t, err)
	if n, ok := application.AgentCoordinator.(interface{ WakeSchedulerNotify() }); ok {
		n.WakeSchedulerNotify()
	}
}

// delegationRunning reports whether toolCallID's delegation is still running
// through the selected authority.
func delegationRunning(t *testing.T, application *App, sessionID, toolCallID string) bool {
	t.Helper()
	return f6DelegationRunning(t, application, sessionID, toolCallID)
}

// TestRunLoop_AwaitTasksAny_WakesOnFirstCompletion: with two live delegations
// and `until: "any"`, the completion that wakes the loop is the FIRST one --
// the second provider request is issued only after that transition, and the
// run ends end_turn (never awaiting_answer).
//
// REVERT CHECK: dropping tools.NewAwaitTasksTool(c) from the tool list in
// internal/agent/coordinator_tools.go makes the turn-1 call an unknown-tool
// step error -- the run never reaches request 2 and this test goes red.
func TestRunLoop_AwaitTasksAny_WakesOnFirstCompletion(t *testing.T) {
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 50*time.Millisecond, 0)()
	var trans1At, req2At atomic.Int64
	stopCompleters := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopCompleters) }) }
	defer stop()
	var completers sync.WaitGroup
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		switch n {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("aw", "call_aw", "await_tasks", `{"until":"any"}`),
				admissionSSEStop("aw", "tool_calls"),
			})
			completers.Add(1)
			go func() {
				defer completers.Done()
				ctx := context.Background()
				firstTimer := time.NewTimer(300 * time.Millisecond)
				defer firstTimer.Stop()
				select {
				case <-firstTimer.C:
				case <-stopCompleters:
					return
				}
				completeDelegation(h.t, ctx, h.app, h.sessionID, "deleg-1", "done-one", true)
				trans1At.Store(time.Now().UnixNano())
				// The second delegation completes only after the any-wake
				// drain started, so its result needs a further drain.
				ticker := time.NewTicker(20 * time.Millisecond)
				defer ticker.Stop()
				for req2At.Load() == 0 {
					select {
					case <-ticker.C:
					case <-stopCompleters:
						return
					}
				}
				secondTimer := time.NewTimer(500 * time.Millisecond)
				defer secondTimer.Stop()
				select {
				case <-secondTimer.C:
				case <-stopCompleters:
					return
				}
				completeDelegation(h.t, ctx, h.app, h.sessionID, "deleg-2", "done-two", true)
			}()
		case 2:
			req2At.Store(time.Now().UnixNano())
			loopText(w, "a", "first result integrated", 11, 3)
		default:
			loopText(w, "f", "final", 11, 3)
		}
	})
	ctx := loopCtx(t)
	child1, err := h.app.Sessions.Create(ctx, "child-1")
	require.NoError(t, err)
	child2, err := h.app.Sessions.Create(ctx, "child-2")
	require.NoError(t, err)
	claimDelegation(t, ctx, h.app, h.sessionID, "deleg-1", child1.ID)
	claimDelegation(t, ctx, h.app, h.sessionID, "deleg-2", child2.ID)

	res, _, runErr := h.run(ctx, RunOverrides{})
	if runErr != nil {
		t.Errorf("run: %v", runErr)
	}
	stop()
	completers.Wait()
	if runErr != nil {
		return
	}

	require.NoError(t, ctx.Err(), "the run finishes well within the deadline")
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason)
	require.NotEqual(t, "awaiting_answer", res.ExitReason)
	require.Greater(t, req2At.Load(), trans1At.Load(),
		"the wake request is issued only after the FIRST delegation transitions")
	require.EqualValues(t, 3, h.requests.Load(), "the await turn, the any-wake drain, the final drain")
}

// TestRunLoop_AwaitTasksAll_DefersUntilEveryTaskFinishes: after the FIRST
// completion the scope is Deferred with the sleep reason and the request count
// stays put; only after the second completion does exactly one more request
// run, carrying both results.
//
// REVERT CHECK: removing rule 4b from internal/agent/turn_arbiter.go (the
// SleepAll/CompletionOnly defer) makes the loop drain after the FIRST
// transition -- the request count leaves 1 early and the Deferred assertion
// goes red.
func TestRunLoop_AwaitTasksAll_DefersUntilEveryTaskFinishes(t *testing.T) {
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 50*time.Millisecond, 0)()
	var body2 atomic.Value // string
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, _ bool, n int) {
		switch n {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("aw", "call_aw", "await_tasks", `{"until":"all"}`),
				admissionSSEStop("aw", "tool_calls"),
			})
			go func() {
				ctx := context.Background()
				time.Sleep(300 * time.Millisecond)
				completeDelegation(h.t, ctx, h.app, h.sessionID, "deleg-1", "done-one", true)
			}()
		case 2:
			body2.Store(string(body))
			loopText(w, "a", "all integrated", 11, 3)
		default:
			h.t.Errorf("unexpected provider request #%d: the sleep must hold until every task finishes", n)
			loopText(w, "x", "unexpected", 1, 1)
		}
	})
	ctx := loopCtx(t)
	child1, err := h.app.Sessions.Create(ctx, "child-1")
	require.NoError(t, err)
	child2, err := h.app.Sessions.Create(ctx, "child-2")
	require.NoError(t, err)
	claimDelegation(t, ctx, h.app, h.sessionID, "deleg-1", child1.ID)
	claimDelegation(t, ctx, h.app, h.sessionID, "deleg-2", child2.ID)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		h.run(ctx, RunOverrides{})
	}()

	require.Eventually(t, func() bool {
		state := cliScopeOf(t, h.app, h.sessionID)
		return state.Drain == agent.DrainDeferred &&
			state.Reason == "sleeping until all tasks finish"
	}, 10*time.Second, 20*time.Millisecond, "the sleep defers the first completion")
	require.EqualValues(t, 1, h.requests.Load(), "the Deferred sleep never reaches the provider")
	time.Sleep(500 * time.Millisecond)
	require.EqualValues(t, 1, h.requests.Load(), "the request count stays put while the sleep holds")

	completeDelegation(t, ctx, h.app, h.sessionID, "deleg-2", "done-two", true)
	select {
	case <-runDone:
	case <-ctx.Done():
		t.Fatal("the run hangs after the last task finished")
	}
	require.EqualValues(t, 2, h.requests.Load(), "exactly one more request after the last completion")
	require.Contains(t, body2.Load().(string), "done-one", "the wake turn carries the first result")
	require.Contains(t, body2.Load().(string), "done-two", "the wake turn carries the second result")
}

// TestRunLoop_AwaitTasksMaxWait_WakesHungTask: the max_wait once schedule (its
// floor of 60s pulled forward by the test) fires while the delegation row is
// STILL running, and the wake turn runs through the normal Drain path.
//
// REVERT CHECK: dropping the ScheduleWakeOnceDelay arm in
// internal/agent/coordinator_await_tasks.go leaves no schedule -- the hoisted
// wake never fires, the run never makes request 2 and the deadline kills it.
func TestRunLoop_AwaitTasksMaxWait_WakesHungTask(t *testing.T) {
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 50*time.Millisecond, 0)()
	var sawRunning atomic.Bool
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		switch n {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("aw", "call_aw", "await_tasks", `{"until":"any","max_wait_seconds":60}`),
				admissionSSEStop("aw", "tool_calls"),
			})
			go hoistOnceWake(h.t, h.app, h.sessionID)
		case 2:
			sawRunning.Store(delegationRunning(h.t, h.app, h.sessionID, "deleg-1"))
			loopText(w, "w", "woke up", 11, 3)
			go func() {
				time.Sleep(200 * time.Millisecond)
				completeDelegation(h.t, context.Background(), h.app, h.sessionID, "deleg-1", "done-late", true)
			}()
		default:
			loopText(w, "f", "final", 11, 3)
		}
	})
	ctx, cancel := context.WithTimeout(loopCtx(t), 30*time.Second)
	t.Cleanup(cancel)
	child, err := h.app.Sessions.Create(ctx, "child-1")
	require.NoError(t, err)
	claimDelegation(t, ctx, h.app, h.sessionID, "deleg-1", child.ID)

	res, _, runErr := h.run(ctx, RunOverrides{})

	require.NoError(t, runErr)
	require.NoError(t, ctx.Err(), "the max_wait wake fires long before the deadline")
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason)
	require.True(t, sawRunning.Load(), "the wake turn fires while the delegation row still runs")
	require.EqualValues(t, 3, h.requests.Load(), "the await turn, the max_wait wake, the completion drain")
}

// TestRunLoop_AwaitTasksMaxWait_TaskFinishesFirst_ScheduleCancelled (the
// variant): the task completes before the max_wait fire, the allowed drain
// launch clears the sleep and CANCELS the pending once schedule, so the run
// ends promptly with no once schedule left open.
//
// REVERT CHECK: dropping the CancelWakeSchedule call inside
// coordinator.clearSleepAll (internal/agent/coordinator_await_tasks.go) leaves
// the once schedule ACTIVE -- OnceWakeOpen keeps the run open for the full 60s
// and this test's deadline (and the cancelled-row assertion) go red.
func TestRunLoop_AwaitTasksMaxWait_TaskFinishesFirst_ScheduleCancelled(t *testing.T) {
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 50*time.Millisecond, 0)()
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		switch n {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("aw", "call_aw", "await_tasks", `{"until":"all","max_wait_seconds":60}`),
				admissionSSEStop("aw", "tool_calls"),
			})
		default:
			loopText(w, "f", "final", 11, 3)
		}
	})
	ctx, cancel := context.WithTimeout(loopCtx(t), 20*time.Second)
	t.Cleanup(cancel)
	child, err := h.app.Sessions.Create(ctx, "child-1")
	require.NoError(t, err)
	claimDelegation(t, ctx, h.app, h.sessionID, "deleg-1", child.ID)
	h.afterFirstTurn(func() {
		rows, err := h.app.WakeScheduleStore().ListSchedules(context.Background(), h.sessionID)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, "active", rows[0].State)
		completeDelegation(t, context.Background(), h.app, h.sessionID, "deleg-1", "done-fast", true)
	})

	start := time.Now()
	res, _, runErr := h.run(ctx, RunOverrides{})

	require.NoError(t, runErr)
	require.NoError(t, ctx.Err(), "the task finishing first must free the run immediately")
	require.Less(t, time.Since(start), 15*time.Second,
		"the cancelled max_wait schedule must not hold the run open")
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason)
	require.False(t, cliScopeOf(t, h.app, h.sessionID).OnceWakeOpen, "no once schedule is left open")
	rows, err := h.app.WakeScheduleStore().ListSchedules(context.Background(), h.sessionID)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	for _, row := range rows {
		require.Equal(t, "cancelled", row.State, "the max_wait schedule is cancelled by the clear")
	}
}

// TestRunLoop_AwaitTasksAll_ChildQuestionBreaksSleep: a child question is
// never completion-class, so it wakes the sleeping root AT ONCE even though a
// delegation row is still running.
//
// REVERT CHECK: treating NoticeKindChildQuestion as completion-class in
// internal/agent/turn_arbiter.go's CompletionOnly loop (or dropping the
// CompletionOnly conjunction from rule 4b) keeps the root asleep -- request 2
// never fires while deleg-2 runs and the deadline kills the run.
func TestRunLoop_AwaitTasksAll_ChildQuestionBreaksSleep(t *testing.T) {
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 50*time.Millisecond, 0)()
	var questionWhileRunning atomic.Bool
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		switch n {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("aw", "call_aw", "await_tasks", `{"until":"all"}`),
				admissionSSEStop("aw", "tool_calls"),
			})
			go func() {
				ctx := context.Background()
				time.Sleep(300 * time.Millisecond)
				completeDelegation(h.t, ctx, h.app, h.sessionID, "deleg-1", "done-one", true)
				time.Sleep(300 * time.Millisecond)
				require.NoError(h.t, h.app.BatchNoticeStore().InsertSessionNotice(ctx, h.sessionID,
					session.NoticeKindChildQuestion, "child asks: which port?", true, "deleg-2"))
			}()
		case 2:
			questionWhileRunning.Store(delegationRunning(h.t, h.app, h.sessionID, "deleg-2"))
			loopText(w, "q", "port 9090", 11, 3)
			go func() {
				time.Sleep(200 * time.Millisecond)
				completeDelegation(h.t, context.Background(), h.app, h.sessionID, "deleg-2", "done-two", true)
			}()
		default:
			loopText(w, "f", "final", 11, 3)
		}
	})
	ctx, cancel := context.WithTimeout(loopCtx(t), 30*time.Second)
	t.Cleanup(cancel)
	child1, err := h.app.Sessions.Create(ctx, "child-1")
	require.NoError(t, err)
	child2, err := h.app.Sessions.Create(ctx, "child-2")
	require.NoError(t, err)
	claimDelegation(t, ctx, h.app, h.sessionID, "deleg-1", child1.ID)
	claimDelegation(t, ctx, h.app, h.sessionID, "deleg-2", child2.ID)

	res, _, runErr := h.run(ctx, RunOverrides{})

	require.NoError(t, runErr)
	require.NoError(t, ctx.Err(), "the child question wakes the root long before the deadline")
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason)
	require.True(t, questionWhileRunning.Load(),
		"the question wakes the root while the second delegation still runs")
	require.EqualValues(t, 3, h.requests.Load(),
		"the await turn, the question wake, the final completion drain")
}

// TestRunLoop_AwaitTasksNothingRunning_ContinuesTurn: await_tasks with no live
// work returns a normal (model-safe) error result, StopTurn is NOT honored and
// the turn continues.
//
// REVERT CHECK: making the empty-work error path StopTurn in
// internal/agent/tools/await_tasks.go ends the run right after request 1 -- no
// second request, the run exits without the final answer and this test goes red.
func TestRunLoop_AwaitTasksNothingRunning_ContinuesTurn(t *testing.T) {
	var body2 atomic.Value // string
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, _ bool, n int) {
		switch n {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("aw", "call_aw", "await_tasks", `{"until":"any"}`),
				admissionSSEStop("aw", "tool_calls"),
			})
		case 2:
			body2.Store(string(body))
			loopText(w, "f", "nothing to wait for", 11, 3)
		default:
			h.t.Errorf("unexpected provider request #%d", n)
			loopText(w, "x", "unexpected", 1, 1)
		}
	})
	ctx := loopCtx(t)

	res, _, runErr := h.run(ctx, RunOverrides{})

	require.NoError(t, runErr)
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason)
	require.NotEqual(t, "awaiting_answer", res.ExitReason)
	require.Equal(t, "nothing to wait for", res.FinalText)
	require.EqualValues(t, 2, h.requests.Load(), "StopTurn is not honored: the turn continues")
	require.Contains(t, body2.Load().(string), "nothing is running",
		"the tool's model-safe error reaches the model as the tool result")
}
