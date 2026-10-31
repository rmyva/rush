package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestAddStepUsageTokensUsesLargestTotal(t *testing.T) {
	// Revert-check: removing TotalTokens from the per-step cap calculation
	// makes the first assertion return only 15 instead of 30.
	got := addStepUsageTokens(0, fantasy.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 30})
	require.EqualValues(t, 30, got, "provider-reported total includes tokens absent from the parts")

	got = addStepUsageTokens(0, fantasy.Usage{
		InputTokens: 20, CacheReadTokens: 4, CacheCreationTokens: 6, OutputTokens: 10, TotalTokens: 25,
	})
	require.EqualValues(t, 40, got, "parts must win when their sum exceeds TotalTokens")
}

func TestApplyStepUsageEnforcesTotalTokensIndependentOfCumulativeUsage(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "provider total token cap")
	require.NoError(t, err)

	assistant, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{Role: "assistant"})
	require.NoError(t, err)

	ts := &turnStream{
		a:                &sessionAgent{sessions: env.sessions, messages: env.messages, activeRequests: csync.NewMap[string, context.CancelFunc]()},
		ctx:              t.Context(),
		genCtx:           t.Context(),
		call:             SessionAgentCall{SessionID: sess.ID, MaxTokens: 500},
		smartModel:       Model{Model: &mockModel{}},
		currentAssistant: &assistant,
	}
	// A single step is under the cap by its parts (100) but over by the
	// provider's TotalTokens (750); this pins TotalTokens handling without
	// depending on multiple-step accumulation.
	step := fantasy.StepResult{Response: fantasy.Response{Usage: fantasy.Usage{
		InputTokens: 40, OutputTokens: 60, TotalTokens: 750,
	}}}
	_, err = ts.applyStepUsage(step)
	require.NoError(t, err)
	err = ts.enforceRunawayCaps(sess)
	require.Error(t, err)
	require.Contains(t, err.Error(), "750 tokens exceeds max 500")
}

func TestApplyStepUsageEnforcesCumulativeTokenCap(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "cumulative token cap")
	require.NoError(t, err)

	assistant, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{Role: "assistant"})
	require.NoError(t, err)

	ts := &turnStream{
		a:                &sessionAgent{sessions: env.sessions, messages: env.messages, activeRequests: csync.NewMap[string, context.CancelFunc]()},
		ctx:              t.Context(),
		genCtx:           t.Context(),
		call:             SessionAgentCall{SessionID: sess.ID, MaxTokens: 150},
		smartModel:       Model{Model: &mockModel{}},
		currentAssistant: &assistant,
	}
	// Two 100-token steps cross the cap cumulatively; TotalTokens is absent,
	// isolating cumulative step accounting from provider-reported totals.
	for i := 0; i < 2; i++ {
		step := fantasy.StepResult{Response: fantasy.Response{Usage: fantasy.Usage{
			InputTokens: 40, OutputTokens: 60,
		}}}
		_, err := ts.applyStepUsage(step)
		require.NoError(t, err)
		if i == 0 {
			require.NoError(t, ts.enforceRunawayCaps(sess), "one step stays under the 150-token cap")
			continue
		}
		err = ts.enforceRunawayCaps(sess)
		require.Error(t, err, "two steps total 200 tokens and must exceed the cap")
		require.Contains(t, err.Error(), "200 tokens exceeds max 150")
	}
}
