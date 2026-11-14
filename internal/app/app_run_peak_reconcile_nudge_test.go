package app

// FIX-PEAK (2026-11-13): the run-level faces of a peak-hours refusal. The
// 2026-11-13 07:00 deaths (sessions final-l2b/l3/l8) showed the refusal of
// the NEXT turn killing an otherwise healthy run as exit_reason "error"
// with an empty final_text, a false "authoritative terminal message
// reconciliation failed" warning and a bogus "todo reminder turn failed"
// warning. These tests pin the corrected, resumable shape.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// peakRefusingCoordinator stands in for a coordinator whose pre-flight
// peak-hours gate refuses the turn (the production 2026-11-13 shape).
type peakRefusingCoordinator struct {
	agent.Coordinator
	err error
}

func (c *peakRefusingCoordinator) Run(context.Context, string, string, ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, c.err
}

// REVERT-CHECK: reverting buildRunResult's peak_hours carve-out fails this
// on exit_reason ("error") and the empty final_text; reverting the finish()
// reconciliation guard fails on the false "authoritative terminal message
// reconciliation failed" warning — the refused turn has no committed
// terminal assistant message, the exact 2026-11-13 final-l2b shape.
func TestExecuteRun_PeakRefusalEnvelopeIsPeakHoursWithoutFalseWarnings(t *testing.T) {
	h := newCycle6RunApp(t)
	pe := &agent.PeakHoursError{
		ProviderID: "openaicompat", Start: "07:00", End: "11:00",
		ReopensAt: time.Now().Add(time.Hour),
	}
	h.app.AgentCoordinator = &peakRefusingCoordinator{Coordinator: h.app.AgentCoordinator, err: pe}

	result, err := h.app.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "answer immediately",
		Mode:              RunModeJSON,
		ContinueSessionID: h.sess.ID,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})
	require.Error(t, err)
	require.True(t, strings.HasPrefix(err.Error(), "run did not complete cleanly (peak_hours)"), "err=%q", err.Error())

	require.NotNil(t, result)
	require.Equal(t, "peak_hours", result.ExitReason)
	require.Contains(t, result.FinalText, "peak hours 07:00–11:00")
	require.Contains(t, result.FinalText, "RESUME AT:")
	for _, warn := range result.Warnings {
		require.NotContains(t, warn, "authoritative terminal message reconciliation failed")
		require.NotContains(t, warn, "final_text is empty")
	}

	fresh, gErr := h.service.Get(context.Background(), h.sess.ID)
	require.NoError(t, gErr)
	require.Equal(t, "peak_hours", fresh.EndedReason)
}

// REVERT-CHECK: pinning the drain-refusal give-up exit: reverting the
// incompleteExitReason wiring at drainPhase (or the buildRunResult
// carve-out) flips this back to exit_reason "error" / "run did not
// complete cleanly (error)"; letting the loop replace the answer breaks
// the FinalText assertion (progress loss). Debt staying open is the
// policy invariant: nothing is settled behind the orchestrator's back.
func TestRunLoop_PeakRefusalGiveUpEnvelopeIsPeakHours(t *testing.T) {
	defer agent.SetDrainPacingForTest(0, 100*time.Millisecond, 0)()
	origLimit := cliLockBusyRetryOverallLimit
	cliLockBusyRetryOverallLimit = time.Second
	t.Cleanup(func() { cliLockBusyRetryOverallLimit = origLimit })

	var stderr syncBuffer
	cliLoopStderr = &stderr
	t.Cleanup(func() { cliLoopStderr = nil })

	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
		if drain {
			h.t.Error("a refused Drain must never reach the provider")
		}
		loopText(w, "a", "first answer", 11, 3)
	})
	h.afterFirstTurn(func() {
		h.seedDebt()
		cfg, ok := h.app.config.Config().Providers.Get("openaicompat")
		require.True(t, ok)
		window := peakWindowCovering(time.Now())
		cfg.PeakHours = &config.ProviderPeakHours{Legacy: &window}
		h.app.config.Config().Providers.Set("openaicompat", cfg)
	})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	res, _, err := h.run(ctx, RunOverrides{})

	require.NoError(t, ctx.Err(), "the loop gives up on its own after the refusal budget")
	var peak *agent.PeakHoursError
	require.ErrorAs(t, err, &peak)
	require.NotNil(t, res)
	require.Equal(t, "peak_hours", res.ExitReason)
	require.Contains(t, res.Error, "run did not complete cleanly (peak_hours)")
	require.Contains(t, res.Error, "is in peak hours (")
	require.Equal(t, "first answer", res.FinalText, "the completed answer must survive the refusal")
	require.True(t, h.debtOpen(), "a refusal never closes the debt")
	for _, warn := range res.Warnings {
		require.NotContains(t, warn, "todo reminder turn failed")
		require.NotContains(t, warn, "authoritative terminal message reconciliation failed")
	}
	require.NotContains(t, stderr.String(), "todo reminder turn failed")

	fresh, gErr := h.app.Sessions.Get(context.Background(), h.sessionID)
	require.NoError(t, gErr)
	require.Equal(t, "peak_hours", fresh.EndedReason)
}

// REVERT-CHECK: reverting the carve-out flips this to exit_reason "error"
// with an empty final_text (the misleading 07:00 shape); the resume
// command must be present so an orchestrator can schedule the wake-up.
// The provider must never see a request: the gate is pre-flight only.
func TestRunLoop_PeakRefusedFirstTurnEnvelopeIsPeakHours(t *testing.T) {
	defer agent.SetDrainPacingForTest(0, 100*time.Millisecond, 0)()
	origLimit := cliLockBusyRetryOverallLimit
	cliLockBusyRetryOverallLimit = time.Second
	t.Cleanup(func() { cliLockBusyRetryOverallLimit = origLimit })

	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, drain bool, n int) {
		h.t.Errorf("the refused first turn must not reach the provider (request #%d)", n)
	})
	cfg, ok := h.app.config.Config().Providers.Get("openaicompat")
	require.True(t, ok)
	window := peakWindowCovering(time.Now())
	cfg.PeakHours = &config.ProviderPeakHours{Legacy: &window}
	h.app.config.Config().Providers.Set("openaicompat", cfg)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	res, _, err := h.run(ctx, RunOverrides{})

	require.NoError(t, ctx.Err())
	var peak *agent.PeakHoursError
	require.ErrorAs(t, err, &peak)
	require.NotNil(t, res)
	require.Equal(t, "peak_hours", res.ExitReason)
	require.NotEqual(t, "", strings.TrimSpace(res.FinalText))
	require.Contains(t, res.FinalText, "RESUME AT:")
	require.Contains(t, res.FinalText, "rush run --session")
	require.Contains(t, res.Error, "is in peak hours (")
	require.Zero(t, h.requests.Load())

	fresh, gErr := h.app.Sessions.Get(context.Background(), h.sessionID)
	require.NoError(t, gErr)
	require.Equal(t, "peak_hours", fresh.EndedReason)
}

// REVERT-CHECK: removing todoNudgeDue's peak exclusion makes the first
// assertion red (the reminder would fire and die with the same refusal —
// the 2026-11-13 "todo reminder turn failed" warning); the control case
// proves the guard stays peak-specific and does not swallow ordinary
// provider failures.
func TestCLILoopTodoNudgeDue_SkipsDuringPeakHours(t *testing.T) {
	h := newCycle6RunApp(t)
	ctx := context.Background()
	require.NoError(t, h.app.Sessions.SetTodos(ctx, h.sess.ID, []session.Todo{
		{Content: "ship it", Status: session.TodoStatusPending},
	}, nil))

	pe := &agent.PeakHoursError{ProviderID: "openaicompat", Start: "07:00", End: "11:00", ReopensAt: time.Now().Add(time.Hour)}
	peakRunErr := fmt.Errorf("run did not complete cleanly (peak_hours): failed to start agent processing stream: %w", &agent.DrainNotAttemptedError{Err: pe})

	var stderr bytes.Buffer
	l := &cliLoop{app: h.app, ctx: ctx, sessionID: h.sess.ID, stderr: &stderr, runErr: peakRunErr}
	require.False(t, l.todoNudgeDue(), "a peak pause must not fire a reminder turn")
	require.Contains(t, stderr.String(), "peak-hours window")

	var plain bytes.Buffer
	l2 := &cliLoop{app: h.app, ctx: ctx, sessionID: h.sess.ID, stderr: &plain, runErr: fmt.Errorf("provider 500")}
	require.True(t, l2.todoNudgeDue(), "an ordinary provider failure still fires the reminder")
}

func TestIncompleteExitReason(t *testing.T) {
	require.Equal(t, "peak_hours", incompleteExitReason(&runIncompleteError{reason: "peak_hours", detail: "paused"}))
	require.Equal(t, "error", incompleteExitReason(&runIncompleteError{reason: "error", detail: "x"}))
	require.Equal(t, "error", incompleteExitReason(fmt.Errorf("boom")))
	require.Equal(t, "error", incompleteExitReason(nil))
}
