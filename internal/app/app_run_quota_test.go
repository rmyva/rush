package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// Revert-check: treating ordinary 429 as IsHardQuotaLimit prevents the coordinator retry.
func TestCLIQuotaRetryable429Unchanged(t *testing.T) {
	var firstAt time.Time
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		if n == 1 {
			firstAt = time.Now()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "0.1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"server overloaded","type":"rate_limit","code":"rate_limit"}}`))
			return
		}
		loopText(w, "ok", "retry succeeded", 1, 1)
	})
	res, _, err := h.run(loopCtx(t), RunOverrides{})
	require.NoError(t, err)
	require.Equal(t, "end_turn", res.ExitReason)
	require.GreaterOrEqual(t, time.Since(firstAt), 100*time.Millisecond)
	require.Greater(t, h.requests.Load(), int32(1))
	require.Empty(t, res.ResumeCommand)
}

func quotaResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "3600")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":{"message":"hard usage limit quota exhausted","type":"quota","code":"quota"}}`))
}

// Revert-check: removing cliLoop.latchQuota from firstTurnPhase permits a completion Drain.
func TestCLIQuotaFirstWaitsPersistsAndResumes(t *testing.T) {
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 50*time.Millisecond, 0)()
	var resume bool
	var lastQuotaAt atomic.Int64
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, _ bool, n int) {
		if resume {
			require.Contains(t, string(body), "own-output")
			require.Contains(t, string(body), "child-output")
			loopText(w, "resumed", "resumed persisted results", 1, 1)
			return
		}
		require.LessOrEqual(t, n, agent.ProviderMaxRetries+1, "no own turn after quota")
		lastQuotaAt.Store(time.Now().UnixNano())
		quotaResponse(w)
	})
	ctx := loopCtx(t)
	child, err := h.app.Sessions.Create(ctx, "child")
	require.NoError(t, err)
	claimDelegation(t, ctx, h.app, h.sessionID, "child-job", child.ID)
	history, err := h.app.Sessions.Create(ctx, "historical child")
	require.NoError(t, err)
	claimDelegation(t, ctx, h.app, h.sessionID, "historical-child", history.ID)
	f6a2ClaimOwnCommandJob(t, ctx, h.app, h.sessionID, "own-bash")
	completeDelegation(t, ctx, h.app, h.sessionID, "historical-child", "historical-output", false)
	h.afterFirstTurn(func() {
		go func() {
			time.Sleep(150 * time.Millisecond)
			// Own command completion: the f6a2 root+task-child projection
			// with the wake notice on the task node. Error-returning for
			// this goroutine.
			e := e3CompleteOwnJobErr(ctx, h.app, h.sessionID, "own-bash", "own-output")
			require.NoError(t, e)
			time.Sleep(150 * time.Millisecond)
			completeDelegation(t, ctx, h.app, h.sessionID, "child-job", "child-output", true)
		}()
	})
	res, wire, runErr := h.run(ctx, RunOverrides{})
	require.Error(t, runErr)
	require.Equal(t, "provider_limit", res.ExitReason)
	require.EqualValues(t, agent.ProviderMaxRetries+1, h.requests.Load())
	// Identity-based: the raw key set is {own-bash, <ownRoot>.1, child-job,
	// <childRoot>.1} -- the root exports under its tool-call id AND each task
	// under its node id (app_batch_cutover_facts.go:151-202), so the alias
	// keys are validated then NORMALISED away before the exact-key check.
	_, ownAlias := f6FindDelegationNodes(t, h.app, h.sessionID, "own-bash")
	_, childAlias := f6FindDelegationNodes(t, h.app, h.sessionID, "child-job")
	results := map[string][]string{}
	for _, work := range res.FinishedWork {
		require.NotEqual(t, "historical-child", work.JobID)
		results[work.JobID] = append(results[work.JobID], work.Result)
	}
	for alias, want := range map[string]string{ownAlias: "own-output", childAlias: "child-output"} {
		entries, ok := results[alias]
		if !ok {
			continue
		}
		for _, aliasResult := range entries {
			require.Equal(t, want, aliasResult,
				"the task alias of %s must export the same result as its root", alias)
		}
		delete(results, alias)
	}
	require.Len(t, results, 2, "exactly the two logical jobs may be exported")
	require.Contains(t, results, "own-bash")
	require.Equal(t, []string{"own-output"}, results["own-bash"])
	require.Contains(t, results, "child-job")
	require.Equal(t, []string{"child-output"}, results["child-job"])
	require.Contains(t, wire, "own-output")
	require.Contains(t, wire, "child-output")
	require.Contains(t, res.ResumeCommand, "--role smart --session "+h.sessionID)
	reset, err := time.Parse(time.RFC3339, res.QuotaResetAt)
	require.NoError(t, err)
	require.WithinDuration(t, time.Unix(0, lastQuotaAt.Load()).Add(time.Hour), reset, 10*time.Second)
	deliveries := e3BatchNoticeDeliveries(t, h.app, h.sessionID)
	require.NotEmpty(t, deliveries)
	for _, delivery := range deliveries {
		require.Equal(t, "pending", delivery)
	}
	resume = true
	res, _, runErr = h.run(ctx, RunOverrides{})
	require.NoError(t, runErr)
	require.Equal(t, "resumed persisted results", res.FinalText)
	deliveries = e3BatchNoticeDeliveries(t, h.app, h.sessionID)
	require.NotEmpty(t, deliveries)
	for _, delivery := range deliveries {
		require.Equal(t, "done", delivery)
	}
}

// Revert-check: removing cliLoop.quotaPhase from decidePhase waits for the future once schedule.
func TestCLIQuotaDrainIgnoresFutureAwaitSchedule(t *testing.T) {
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 50*time.Millisecond, 0)()
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		if n == 1 {
			h.seedDebt()
			loopText(w, "first", "keep answer", 1, 1)
			return
		}
		require.LessOrEqual(t, n, agent.ProviderMaxRetries+2)
		quotaResponse(w)
	})
	ctx := loopCtx(t)
	scheduler := h.app.AgentCoordinator.(tools.WakeControl)
	_, err := scheduler.ScheduleWakeOnceDelay(ctx, h.sessionID, 3600, "await_tasks max_wait elapsed")
	require.NoError(t, err)
	start := time.Now()
	res, wire, runErr := h.run(ctx, RunOverrides{})
	require.Error(t, runErr)
	require.Equal(t, "provider_limit", res.ExitReason)
	require.Equal(t, "keep answer", res.FinalText)
	require.EqualValues(t, agent.ProviderMaxRetries+2, h.requests.Load())
	// Pinned waits are 15s total; 60s leaves ample headroom for
	// scheduling even if the pin is later raised.
	require.Less(t, time.Since(start), 60*time.Second)
	require.Contains(t, wire, "once schedules")
	require.True(t, cliScopeOf(t, h.app, h.sessionID).OnceWakeOpen)
}

// Revert-check: removing quotaPhase's durable finish metadata read drops pending child questions.
func TestCLIQuotaPreservesPendingChildQuestion(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) { quotaResponse(w) })
	ctx := loopCtx(t)
	child, err := h.app.Sessions.Create(ctx, "question child")
	require.NoError(t, err)
	// The finish metadata alone never clears live work (cli_active_work.go
	// cliActiveWorkBatch); only the CURRENT committed held-question identity
	// makes the delegation dormant. Seed a token-carrying live delegation
	// (f6ReserveDelegation's nodes carry token 0, which BatchPublishQuestion
	// refuses) and publish through the real committed path.
	e3ReserveDelegationWithToken(t, h.app, h.sessionID, "held-child", child.ID)
	e3PublishHeldQuestion(t, ctx, h.app, h.sessionID, "held-child", child.ID, "which port?")
	_, err = h.app.Messages.Create(ctx, child.ID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{
		message.Finish{Reason: message.FinishReasonError, Message: "Stopped: agent asked a question and is awaiting an answer", Details: agent.AwaitingAnswerGuidance("which port?", []string{"8080"}, child.ID, "smart")},
	}})
	require.NoError(t, err)
	res, wire, runErr := h.run(ctx, RunOverrides{})
	require.Error(t, runErr)
	require.Equal(t, "provider_limit", res.ExitReason)
	require.Len(t, res.PendingChildQuestions, 1)
	require.Contains(t, wire, "which port?")
	require.EqualValues(t, agent.ProviderMaxRetries+1, h.requests.Load())
	var decoded RunResult
	require.NoError(t, json.Unmarshal([]byte(wire), &decoded))
	require.Contains(t, strings.Join([]string{decoded.Error, decoded.ResumeCommand}, " "), "--role smart")
}

// Revert-check: treating the finish metadata alone as a
// committed question (dropping batchCommittedQuestionChildren's marker/token
// fence) would classify the live delegation dormant and let the run exit with
// a PendingChildQuestions entry -- losing the live work the delegation still
// holds. The twin seeds ONLY the finish block: the run must keep waiting on
// the live delegation ("still has active work") until the run deadline and
// never report a pending question.
func TestCLIQuotaFinishMetadataAloneDoesNotClearLiveWork(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) { quotaResponse(w) })
	ctx := loopCtx(t)
	child, err := h.app.Sessions.Create(ctx, "twin child")
	require.NoError(t, err)
	claimDelegation(t, ctx, h.app, h.sessionID, "held-child-twin", child.ID)
	_, err = h.app.Messages.Create(ctx, child.ID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{
		message.Finish{Reason: message.FinishReasonError, Message: "Stopped: agent asked a question and is awaiting an answer", Details: agent.AwaitingAnswerGuidance("which port?", []string{"8080"}, child.ID, "smart")},
	}})
	require.NoError(t, err)
	runCtx, cancel := context.WithTimeout(t.Context(), quotaRetryWait()+6*time.Second)
	defer cancel()
	// Capture stderr exactly like app_run_quota_boundary_test.go:25-26:
	// the "still has active work" diagnostic goes to errOut()
	// (app_run_async.go:272-279 -> cliLoopStderr), never to the JSON wire.
	var stderrOut syncBuffer
	prevStderr := cliLoopStderr
	cliLoopStderr = &stderrOut
	t.Cleanup(func() { cliLoopStderr = prevStderr })
	res, _, runErr := h.run(runCtx, RunOverrides{})
	require.Error(t, runErr)
	require.NotNil(t, res)
	// Deadline expiry while quotaPhase waits on the live delegation keeps
	// ExitReason "canceled" (app_run_async_exit.go:91-101).
	require.Equal(t, "canceled", res.ExitReason)
	require.Zero(t, len(res.PendingChildQuestions),
		"finish metadata without a committed marker question is NOT a pending child question")
	require.Contains(t, stderrOut.String(), "still has active work",
		"the loop must keep waiting on the live delegation")
}
