package app

// FIX-PEAK (2026-11-13): a peak-hours refusal that ends a `rush run` turn
// must surface as exit_reason "peak_hours" with a readable final_text —
// not the generic "error" with an empty answer. Pure buildRunResult level.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/assert"
)

func peakPauseFixture() *agent.PeakHoursError {
	return &agent.PeakHoursError{
		ProviderID: "zai",
		Start:      "07:00",
		End:        "11:00",
		ReopensAt:  time.Date(2026, 11, 13, 11, 0, 0, 0, time.Local),
		Message:    "OPERATOR-NOTE: quota exhausted",
	}
}

// REVERT-CHECK: removing buildRunResult's peak_hours carve-out turns both
// exit_reason assertions red (the refusal falls back to the generic
// "error"); dropping the final_text synthesis turns the non-empty
// final_text / RESUME AT assertions red.
func TestBuildRunResult_PeakRefusalIsPeakHoursPause(t *testing.T) {
	pe := peakPauseFixture()
	cases := map[string]error{
		"pre-flight refusal": fmt.Errorf("failed to start agent processing stream: %w", pe),
		"bare error":         pe,
	}
	for name, runErr := range cases {
		t.Run(name, func(t *testing.T) {
			res := buildRunResult("s1", "", "", "", runErr, false, nil, 0, 0, time.Second, "", "", 0, "", "", nil, "")
			assert.Equal(t, "peak_hours", res.ExitReason)
			assert.Contains(t, res.FinalText, "peak hours 07:00–11:00")
			assert.Contains(t, res.FinalText, "RESUME AT:")
			assert.Contains(t, res.FinalText, "rush run --session s1")
			assert.Contains(t, res.Error, "is in peak hours (")
			assert.Contains(t, res.Error, "RESUME AT:")
			assert.Contains(t, res.Error, pe.Message)
			assert.Empty(t, res.Warnings, "a refused turn must not add warnings, got %v", res.Warnings)
		})
	}
}

// REVERT-CHECK: an over-broad carve-out (overriding canceled/awaiting or a
// model-level finish) fails the canceled subtest; a carve-out that misses
// the committed-error-finish shape (finalReason "error" replayed from the
// DB) fails the explicit-error subtest; non-peak errors must stay "error".
func TestBuildRunResult_PeakRefusalEdges(t *testing.T) {
	t.Run("zero reopen names the blocked-around-the-clock shape", func(t *testing.T) {
		pe := peakPauseFixture()
		pe.ReopensAt = time.Time{}
		res := buildRunResult("s1", "", "", "", pe, false, nil, 0, 0, time.Second, "", "", 0, "", "", nil, "")
		assert.Equal(t, "peak_hours", res.ExitReason)
		assert.Contains(t, res.FinalText, "no scheduled reopen")
		assert.NotContains(t, res.FinalText, "RFC3339")
	})

	t.Run("explicit error finish from a committed row is still peak_hours", func(t *testing.T) {
		res := buildRunResult("s1", "", "", "error", peakPauseFixture(), false, nil, 0, 0, time.Second, "Stopped", "", 0, "", "", nil, "")
		assert.Equal(t, "peak_hours", res.ExitReason)
	})

	t.Run("canceled keeps its own reason and answer", func(t *testing.T) {
		pe := peakPauseFixture()
		runErr := fmt.Errorf("failed to start agent processing stream: %w", pe)
		res := buildRunResult("s1", "owner answer", "", "end_turn", runErr, true, nil, 0, 0, time.Second, "", "", 0, "", "", nil, "")
		assert.Equal(t, "canceled", res.ExitReason)
		assert.Equal(t, "owner answer", res.FinalText)
	})

	t.Run("non-peak errors stay error", func(t *testing.T) {
		res := buildRunResult("s1", "", "", "", context.DeadlineExceeded, false, nil, 0, 0, time.Second, "", "", 0, "", "", nil, "")
		assert.Equal(t, "error", res.ExitReason)
		assert.Empty(t, res.FinalText)
	})
}
