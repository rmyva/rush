package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildRunResult_HappyPath(t *testing.T) {
	r := buildRunResult(
		"s1", "all done", "", "stop", nil, false,
		map[string]int{"bash": 3, "edit": 1},
		1234, 0.0021,
		2*time.Second+500*time.Millisecond,
		"", "",
		0, "", "",
		nil, "",
	)
	assert.Equal(t, "s1", r.SessionID)
	assert.Equal(t, "stop", r.ExitReason)
	assert.Equal(t, "all done", r.FinalText)
	assert.Empty(t, r.Error)
	require.Len(t, r.ToolCalls, 2)
	// Sorted alphabetically — bash before edit.
	assert.Equal(t, "bash", r.ToolCalls[0].Name)
	assert.Equal(t, 3, r.ToolCalls[0].Count)
	assert.Equal(t, "edit", r.ToolCalls[1].Name)
	assert.Equal(t, int64(1234), r.Usage.DeltaTokens)
	assert.InDelta(t, 0.0021, r.Usage.DeltaCostUSD, 1e-9)
	assert.Equal(t, int64(2500), r.DurationMs)
}

func TestBuildRunResult_FallbackExitReason(t *testing.T) {
	cases := []struct {
		name       string
		finishStr  string
		err        error
		canceled   bool
		wantReason string
		wantErrSet bool
	}{
		{"empty + canceled → canceled", "", context.Canceled, true, "canceled", false},
		{"empty + error → error", "", errors.New("boom"), false, "error", true},
		{"empty + no err → unknown", "", nil, false, "unknown", false},
		{"explicit reason wins over fallback", "max_tokens", nil, false, "max_tokens", false},
		{"explicit reason wins over canceled fallback", "stop", context.Canceled, true, "stop", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := buildRunResult("s", "", "", tc.finishStr, tc.err, tc.canceled, nil, 0, 0, 0, "", "", 0, "", "", nil, "")
			assert.Equal(t, tc.wantReason, r.ExitReason)
			if tc.wantErrSet {
				assert.NotEmpty(t, r.Error, "Error must surface non-cancel errors")
			} else {
				assert.Empty(t, r.Error, "Error must stay empty for cancel / success")
			}
		})
	}
}

func TestBuildRunResult_StableToolCallOrder(t *testing.T) {
	r := buildRunResult("s", "", "", "stop", nil, false,
		map[string]int{"zeta": 1, "alpha": 1, "mu": 2}, 0, 0, 0, "", "", 0, "", "", nil, "")
	names := make([]string, len(r.ToolCalls))
	for i, t := range r.ToolCalls {
		names[i] = t.Name
	}
	assert.Equal(t, []string{"alpha", "mu", "zeta"}, names)
}

func TestBuildRunResult_NoToolCalls(t *testing.T) {
	r := buildRunResult("s", "done", "", "stop", nil, false, nil, 0, 0, 0, "", "", 0, "", "", nil, "")
	require.NotNil(t, r.ToolCalls)
	assert.Empty(t, r.ToolCalls)
}

func TestBuildRunResult_AgentErrRequestCancelledIsHandledByCaller(t *testing.T) {
	// Sanity: we don't unwrap ErrRequestCancelled inside buildRunResult —
	// the call site classifies it as canceled and passes canceled=true.
	// Verify that with canceled=true the Error stays empty even though
	// the err itself is non-nil.
	r := buildRunResult("s", "", "", "", agent.ErrRequestCancelled, true, nil, 0, 0, 0, "", "", 0, "", "", nil, "")
	assert.Equal(t, "canceled", r.ExitReason)
	assert.Empty(t, r.Error)
}

// TestBuildRunResult_AwaitingAnswer verifies that an *agent.AwaitingAnswerError
// (the ask_question tool's turn-stop error, surfaced as runErr) maps to
// exit_reason "awaiting_answer" — NOT the generic "error" that
// message.FinishReasonError would otherwise produce — and that the Error
// field carries the already-recorded title+details (question + full
// AwaitingAnswerGuidance, including the resume command) verbatim rather
// than the terser err.Error().
func TestBuildRunResult_AwaitingAnswer(t *testing.T) {
	awaitingErr := &agent.AwaitingAnswerError{
		Question:  "Which environment should I deploy to?",
		Options:   []string{"staging", "production"},
		SessionID: "sess-1",
	}
	title := "Stopped: agent asked a question and is awaiting an answer"
	details := awaitingErr.Error() + "\n\nQUESTION: Which environment should I deploy to?\n\nrush run --role smart --session sess-1 \"<your answer>\""

	// finalReason mirrors what agent.go's AddFinish actually records for
	// this path: message.FinishReasonError, because there is no dedicated
	// FinishReason for "awaiting answer" — see AddFinish in the awaitingErr
	// branch of agent.go.
	r := buildRunResult(
		"sess-1", "", "", "error", awaitingErr, false,
		nil, 0, 0, 0,
		title, details,
		0, "", "",
		nil, "",
	)
	assert.Equal(t, "awaiting_answer", r.ExitReason, "must not fall through to the generic error reason")
	assert.Contains(t, r.Error, title)
	assert.Contains(t, r.Error, "Which environment should I deploy to?")
	assert.Contains(t, r.Error, "rush run --role smart --session sess-1")
}

// TestBuildRunResult_AwaitingAnswer_NoFanoutOrEmptyWarning verifies the
// awaiting_answer path is excluded from the "final_text is empty" warnings,
// same as "error" and "canceled" — asking a question and stopping the turn
// is a deliberate, successful stop, not a fan-out-without-composition bug.
func TestBuildRunResult_AwaitingAnswer_NoFanoutOrEmptyWarning(t *testing.T) {
	awaitingErr := &agent.AwaitingAnswerError{Question: "Proceed?", SessionID: "s"}
	r := buildRunResult(
		"s", "", "", "error", awaitingErr, false,
		map[string]int{"agent": 2},
		0, 0, 0,
		"Stopped: agent asked a question and is awaiting an answer", "details",
		0, "", "",
		nil, "",
	)
	assert.Equal(t, "awaiting_answer", r.ExitReason)
	for _, w := range r.Warnings {
		assert.NotContains(t, w, "final_text is empty")
	}
}

// TestBuildRunResult_AwaitingAnswer_FallsBackToErrErrorWhenFinishTextMissing
// covers the defensive default branch: if for some reason finalErrTitle and
// finalErrDetails are both empty (shouldn't happen given agent.go always
// populates them via awaitingAnswerStoppedFinishText, but buildRunResult
// must not silently produce an empty Error field either way).
func TestBuildRunResult_AwaitingAnswer_FallsBackToErrErrorWhenFinishTextMissing(t *testing.T) {
	awaitingErr := &agent.AwaitingAnswerError{Question: "Proceed?", SessionID: "s"}
	r := buildRunResult(
		"s", "", "", "error", awaitingErr, false,
		nil, 0, 0, 0,
		"", "",
		0, "", "",
		nil, "",
	)
	assert.Equal(t, "awaiting_answer", r.ExitReason)
	assert.Equal(t, awaitingErr.Error(), r.Error)
}

// --- batch-7 additions: reduction warning + sub_agent_outputs --------

// TestBuildRunResult_ReductionWarningAppended verifies the always-on
// "reduction-loss" warning that fires when parent summarises sub-agent
// fan-out too aggressively. The warning text is computed in
// RunNonInteractive (it needs DB access for sub-session sizes) and
// passed in as the last positional argument; buildRunResult's job is
// only to append it AFTER the other warnings.
func TestBuildRunResult_ReductionWarningAppended(t *testing.T) {
	warn := "reduction-loss: final_text is 200 chars (10% of 2000 combined sub-agent chars across 3 sub-session(s))."
	r := buildRunResult(
		"s", "summary text", "", "stop", nil, false,
		map[string]int{"agent": 3}, 0, 0, 0, "", "",
		0, "", "",
		nil, warn,
	)
	require.NotEmpty(t, r.Warnings)
	assert.Equal(t, warn, r.Warnings[len(r.Warnings)-1],
		"reduction-loss warning must land LAST in the array so existing fan-out warnings stay first")
}

// TestBuildRunResult_NoReductionWarning verifies that when caller
// passes "" for reductionWarning, nothing is appended.
func TestBuildRunResult_NoReductionWarning(t *testing.T) {
	r := buildRunResult(
		"s", "ok", "", "stop", nil, false, nil, 0, 0, 0, "", "",
		0, "", "",
		nil, "",
	)
	for _, w := range r.Warnings {
		assert.NotContains(t, w, "reduction-loss")
	}
}

// TestBuildRunResult_SubAgentOutputsAttached verifies the envelope
// field that --aggregation=attach populates.
func TestBuildRunResult_SubAgentOutputsAttached(t *testing.T) {
	subs := []SubAgentOutput{
		{SessionID: "sub-1", Title: "Topic A", FinalText: "verbatim A", CharCount: 10},
		{SessionID: "sub-2", Title: "Topic B", FinalText: "verbatim B much longer text", CharCount: 27},
	}
	r := buildRunResult(
		"parent", "wrap-up", "", "stop", nil, false,
		map[string]int{"agent": 2}, 0, 0, 0, "", "",
		0, "", "",
		subs, "",
	)
	require.Len(t, r.SubAgentOutputs, 2)
	assert.Equal(t, "sub-1", r.SubAgentOutputs[0].SessionID)
	assert.Equal(t, "verbatim A", r.SubAgentOutputs[0].FinalText)
	assert.Equal(t, 27, r.SubAgentOutputs[1].CharCount)
}

func TestBuildRunResult_NoSubAgentOutputsByDefault(t *testing.T) {
	// summary-mode (the default) must NOT spuriously emit an empty
	// array — `omitempty` on the struct tag should drop the field.
	r := buildRunResult(
		"s", "ok", "", "stop", nil, false, nil, 0, 0, 0, "", "",
		0, "", "",
		nil, "",
	)
	assert.Nil(t, r.SubAgentOutputs, "default (summary) must yield nil so omitempty drops the JSON key")
}

// TestBuildRunResult_EmptyFinalText_NoFanout verifies the generic
// empty-final_text warning that fires when the model ended the turn on a
// tool_call (no assistant text composed), regardless of fan-out. Without
// this warning the orchestrator gets `final_text: ""` and a silent
// success — they have no way to know "this run did things but never told
// me what". The warning enumerates what tools were called so the
// operator can decide whether to re-prompt or to inspect git directly.
func TestBuildRunResult_EmptyFinalText_NoFanout(t *testing.T) {
	r := buildRunResult(
		"s", "", "", "stop", nil, false,
		map[string]int{"edit": 2, "bash": 1, "view": 4},
		0, 0, 0, "", "",
		0, "", "",
		nil, "",
	)
	require.NotEmpty(t, r.Warnings, "empty final_text without fan-out must still emit a warning")
	found := false
	for _, w := range r.Warnings {
		if assertContainsAll(w, "final_text is empty", "2 file edit(s)", "1 bash call(s)", "4 other tool call(s)") {
			found = true
			break
		}
	}
	assert.True(t, found, "warning should name the file-edit / bash / other tool counts; got: %v", r.Warnings)
}

func TestBuildRunResult_EmptyFinalText_NoTools(t *testing.T) {
	r := buildRunResult("s", "", "", "stop", nil, false, nil, 0, 0, 0, "", "", 0, "", "", nil, "")
	require.NotEmpty(t, r.Warnings)
	assert.Contains(t, r.Warnings[0], "no tools were called", "no tools + empty final_text should produce its own warning")
}

func TestBuildRunResult_EmptyFinalText_FanoutWarningTakesPriority(t *testing.T) {
	// fan-out warning is the original path and must keep firing — the new
	// generic path only triggers when fanoutCalls == 0.
	r := buildRunResult(
		"s", "", "", "stop", nil, false,
		map[string]int{"agent": 2, "edit": 1},
		0, 0, 0, "", "",
		0, "", "",
		nil, "",
	)
	require.NotEmpty(t, r.Warnings)
	assert.Contains(t, r.Warnings[0], "sub-agent fan-out call(s)")
	for _, w := range r.Warnings {
		assert.NotContains(t, w, "model ended on a tool_call without composing", "should not double-emit the generic warning on top of the fan-out one")
	}
}

func TestBuildRunResult_NonEmptyFinalText_NoEmptyWarning(t *testing.T) {
	r := buildRunResult(
		"s", "I edited foo.go", "", "stop", nil, false,
		map[string]int{"edit": 1},
		0, 0, 0, "", "",
		0, "", "",
		nil, "",
	)
	for _, w := range r.Warnings {
		assert.NotContains(t, w, "final_text is empty")
	}
}

// assertContainsAll returns true if every part appears in s. testify has
// `assert.Contains` for a single substring; we use this when we need to
// verify a warning string carries multiple expected fragments at once.
func assertContainsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// TestRunFailed pins the success/failure classification that drives the
// process exit code: only a clean end_turn (or an empty captured reason)
// exits 0; in-band error/canceled/max_tokens finishes, a hard error, or a
// cancellation/timeout all exit non-zero.
func TestRunFailed(t *testing.T) {
	tests := []struct {
		name        string
		finalReason string
		runErr      error
		isCanceled  bool
		want        bool
	}{
		{"clean end_turn", "end_turn", nil, false, false},
		{"empty reason, no error", "", nil, false, false},
		{"unknown reason is not a failure", "unknown", nil, false, false},
		{"in-band error finish", "error", nil, false, true},
		{"in-band canceled finish", "canceled", nil, false, true},
		{"max_tokens truncation", "max_tokens", nil, false, true},
		{"hard runErr", "end_turn", errors.New("boom"), false, true},
		{"isCanceled (timeout/watchdog/user)", "end_turn", nil, true, true},
		{"runErr context.Canceled", "", context.Canceled, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, runFailed(tc.finalReason, tc.runErr, tc.isCanceled))
		})
	}
}

func TestBuildRunResult_QueuedAlwaysOverridesCapturedOwnerFinish(t *testing.T) {
	result := buildRunResult(
		"s1", "owner text", "", "stop", &runQueuedError{sessionID: "s1"}, false,
		nil, 0, 0, 0, "", "", 0, "", "", nil, "",
	)

	assert.Equal(t, "queued", result.ExitReason)
	assert.Contains(t, result.Warnings, "prompt was queued behind an active session run; this invocation did not execute it")
}

// TestRunIncompleteError_Message ensures the sentinel renders both with and
// without detail (it drives stderr diagnostics on a non-zero exit).
func TestRunIncompleteError_Message(t *testing.T) {
	assert.Equal(t, "run did not complete cleanly (error): Stream stalled: provider X",
		(&runIncompleteError{reason: "error", detail: "Stream stalled: provider X"}).Error())
	assert.Equal(t, "run did not complete cleanly (cancelled)",
		(&runIncompleteError{reason: "cancelled"}).Error())
}

func TestSessionBusyGuidance(t *testing.T) {
	queued := sessionBusyGuidance("s1", fmt.Errorf("outer: %w", agent.ErrSessionBusy))
	assert.Contains(t, queued, "this rush process")
	assert.Contains(t, queued, "rush sessions inject s1 -m <message>")

	locked := sessionBusyGuidance("s2", fmt.Errorf("outer: %w", &session.SessionLockBusyError{HolderPID: 1234, Path: "lock"}))
	assert.Contains(t, locked, "rush process PID 1234")
	assert.Contains(t, locked, "rush run --session s2")

	assert.Empty(t, sessionBusyGuidance("s3", errors.New("other")))
}

// TestCancelledRunError pins the terse/--stream isCanceled path: a forced
// abort (peak-hours mid-turn stop, max-cost, max-tokens) cancels the run's
// context, which can race the specific error into a generic
// context.Canceled - this must not throw away the FinishReasonError detail
// that was already persisted on the assistant message. A genuine,
// unrecorded Ctrl+C (no FinishReasonError message) must still fall back to
// the bare "cancelled" text.
func TestCancelledRunError(t *testing.T) {
	peakErr := fmt.Errorf("provider zai is in peak hours (08:00–12:00), refusing until 12:00")

	t.Run("specific runErr is surfaced even when event-loop missed the finish detail", func(t *testing.T) {
		// The typical race: OnStepFinish returns a specific error AND
		// calls cancel(). runErr carries the specific error, but the
		// event-loop (which populates finalReason/finalErrTitle) never
		// sees the updated message because the context died first.
		err := cancelledRunError(peakErr, "", "", "")
		require.NotNil(t, err)
		assert.Equal(t, "cancelled", err.reason)
		assert.Contains(t, err.detail, "peak hours")
		assert.Contains(t, err.detail, "refusing until 12:00")
	})

	t.Run("context.Canceled runErr falls through to event-loop detail", func(t *testing.T) {
		// When runErr is bare context.Canceled, use the event-loop's
		// recorded FinishReasonError detail instead.
		err := cancelledRunError(
			context.Canceled,
			string(message.FinishReasonError),
			"Stopped: budget exceeded",
			"cost $5.00 exceeds max $2.00",
		)
		assert.Contains(t, err.detail, "budget exceeded")
	})

	t.Run("genuine unrecorded cancel falls back to bare message", func(t *testing.T) {
		err := cancelledRunError(context.Canceled, "", "", "")
		assert.Empty(t, err.detail)
		assert.Equal(t, "run did not complete cleanly (cancelled)", err.Error())
	})

	t.Run("non-error finish reason does not leak unrelated finish text", func(t *testing.T) {
		err := cancelledRunError(context.Canceled, string(message.FinishReasonEndTurn), "some unrelated title", "some unrelated detail")
		assert.Empty(t, err.detail)
	})
}
