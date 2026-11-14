package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/require"
)

// The turn runner must wrap a peak-hours refusal with %w: the --json
// envelope (buildRunResult) and the plain stderr guidance both find the
// operator message via errors.As. A %v wrap silently drops it (the
// 2026-09-23 smoke failure mode).
func TestRunAgentTurnRecovered_PeakHoursRefusalReachesEnvelopeWithMessage(t *testing.T) {
	pe := &agent.PeakHoursError{
		ProviderID: "zai", Start: "20:00", End: "23:59",
		ReopensAt: time.Now().Add(time.Hour),
		Message:   "OPERATOR-NOTE: quota exhausted, resume after midnight",
	}
	done := make(chan agentTurnResponse, 1)
	runAgentTurnRecovered(t.Context(), "s1", "hi", func(context.Context, string, string) (*fantasy.AgentResult, error) {
		return nil, pe
	}, done)
	resp := <-done

	var got *agent.PeakHoursError
	require.True(t, errors.As(resp.err, &got), "turn runner must keep the *PeakHoursError reachable, got %v", resp.err)

	res := buildRunResult("s1", "", "", "", resp.err, false, nil, 0, 0, time.Second, "", "", 0, "", "", nil, "")
	require.Equal(t, "peak_hours", res.ExitReason)
	require.NotEmpty(t, strings.TrimSpace(res.FinalText), "the pause must leave a readable final_text")
	require.Contains(t, res.Error, "is in peak hours (")
	require.Contains(t, res.Error, "RESUME AT:")
	require.Contains(t, res.Error, pe.Message)
}
