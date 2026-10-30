// turnStream.onRetry is fantasy's only hook between a cut stream attempt's
// deltas and the retried attempt's deltas: fantasy re-runs the whole step on
// the SAME callbacks (agent.go's retry wraps Stream+processStepStream;
// PrepareStep and OnStepStart are not re-run), so nothing else can clear
// ts.currentAssistant. These tests pin what the hook must discard, observed
// through a real message service.
package agent

import (
	"context"
	"io"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// retryFixture builds the thinnest turnStream that drives the streaming
// callbacks and onRetry against a real message service (the same connection
// helper the tool-suggest fixture uses), so what onRetry discards is
// observed both in memory and in what is actually persisted.
type retryFixture struct {
	svc message.Service
	ts  *turnStream
}

func newRetryFixture(t *testing.T, sessionID string) *retryFixture {
	t.Helper()
	conn := batchFixtureConnNoFK(t)
	svc := message.NewService(db.New(conn))
	return &retryFixture{
		svc: svc,
		ts: &turnStream{
			a:               &sessionAgent{messages: svc},
			ctx:             t.Context(),
			genCtx:          t.Context(),
			call:            SessionAgentCall{SessionID: sessionID},
			bumpActivity:    func() {},
			startCheckpoint: func() {},
			notifyUI:        func() error { return nil },
		},
	}
}

// beginStep creates the step's assistant row the way prepareStep does and
// installs it through the same per-step reset prepareStep uses.
func (f *retryFixture) beginStep(t *testing.T) *message.Message {
	t.Helper()
	msg, err := f.svc.Create(context.Background(), f.ts.call.SessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{},
	})
	require.NoError(t, err)
	f.ts.beginAssistantStep(&msg)
	return &msg
}

// codexCutError is the exact shape #1202 produces when a Codex body is cut
// mid-stream: a retryable ProviderError over io.ErrUnexpectedEOF.
func codexCutError() *fantasy.ProviderError {
	return &fantasy.ProviderError{
		Title:   "stream transport error",
		Message: io.ErrUnexpectedEOF.Error(),
		Cause:   io.ErrUnexpectedEOF,
	}
}

// TestStreamPersistsMultipleReasoningBlocksInArrivalOrder pins persistence order.
func TestStreamPersistsMultipleReasoningBlocksInArrivalOrder(t *testing.T) {
	f := newRetryFixture(t, "reasoning-block-order")
	stepMsg := f.beginStep(t)
	require.NoError(t, f.ts.onReasoningStart("r1", fantasy.ReasoningContent{Text: "first thought"}))
	require.NoError(t, f.ts.onReasoningDelta("r1", " continues"))
	require.NoError(t, f.ts.onReasoningEnd("r1", fantasy.ReasoningContent{ProviderMetadata: fantasy.ProviderMetadata{
		anthropic.Name: &anthropic.ReasoningOptionMetadata{Signature: "first-signature"},
	}}))
	require.NoError(t, f.ts.onTextDelta("t1", "visible"))
	require.NoError(t, f.ts.onReasoningStart("r2", fantasy.ReasoningContent{Text: "second thought"}))
	require.NoError(t, f.ts.onReasoningDelta("r2", " continues"))
	require.NoError(t, f.ts.onReasoningEnd("r2", fantasy.ReasoningContent{ProviderMetadata: fantasy.ProviderMetadata{
		anthropic.Name: &anthropic.ReasoningOptionMetadata{Signature: "second-signature"},
	}}))

	persisted, err := f.svc.Get(context.Background(), stepMsg.ID)
	require.NoError(t, err)
	var sequence []string
	for _, part := range persisted.Parts {
		switch value := part.(type) {
		case message.ReasoningContent:
			sequence = append(sequence, "reasoning:"+value.Thinking)
		case message.TextContent:
			sequence = append(sequence, "text:"+value.Text)
		}
	}
	require.Equal(t, []string{"reasoning:first thought continues", "text:visible", "reasoning:second thought continues"}, sequence)
	reasoning := persisted.ReasoningContents()
	require.Len(t, reasoning, 2)
	require.Equal(t, "first thought continues", reasoning[0].Thinking)
	require.Equal(t, "first-signature", reasoning[0].Signature)
	require.Equal(t, "second thought continues", reasoning[1].Thinking)
	require.Equal(t, "second-signature", reasoning[1].Signature)
}

// TestStreamRetryDiscardsCutAttemptContent pins the retry duplication fix.
// Revert check (red): with the truncation calls removed this fails — the
// assistant text reads "partial the full answer" (the cut attempt's prefix
// riding under the retry's answer).
func TestStreamRetryDiscardsCutAttemptContent(t *testing.T) {
	f := newRetryFixture(t, "retry-cut")
	stepMsg := f.beginStep(t)

	require.NoError(t, f.ts.onReasoningStart("r1", fantasy.ReasoningContent{Text: "cut-attempt thinking"}))
	require.NoError(t, f.ts.onReasoningDelta("r1", "more thinking"))
	require.NoError(t, f.ts.onTextDelta("t1", "partial "))
	require.NoError(t, f.ts.onReasoningStart("r1-second", fantasy.ReasoningContent{Text: "cut-attempt second block"}))
	require.NoError(t, f.ts.onReasoningDelta("r1-second", " deeper"))

	f.ts.onRetry(codexCutError(), 2*time.Second)

	require.NoError(t, f.ts.onReasoningStart("r2", fantasy.ReasoningContent{Text: "retry thinking"}))
	// The truncation was persisted by that same callback's Update: the row
	// no longer carries any cut-attempt text.
	persisted, err := f.svc.Get(context.Background(), stepMsg.ID)
	require.NoError(t, err)
	require.Empty(t, persisted.Content().Text)
	require.Equal(t, "retry thinking", persisted.ReasoningContent().Thinking)
	require.NoError(t, f.ts.onReasoningDelta("r2", " deeper"))
	require.NoError(t, f.ts.onTextDelta("t2", "the full answer"))
	// Deltas alone are not persisted (unchanged behavior) — the final
	// step-boundary write lands them; mimic it and require BOTH stores to
	// end with exactly the retry's content.
	require.NoError(t, f.svc.Update(context.Background(), f.ts.currentAssistant.Clone()))
	persisted, err = f.svc.Get(context.Background(), stepMsg.ID)
	require.NoError(t, err)
	require.Equal(t, "the full answer", persisted.Content().Text)
	require.Equal(t, "the full answer", f.ts.currentAssistant.FullText())
	require.Equal(t, []message.ReasoningContent{{Thinking: "retry thinking deeper", StartedAt: persisted.ReasoningContents()[0].StartedAt}}, persisted.ReasoningContents())
	require.Equal(t, "retry thinking deeper", f.ts.currentAssistant.FullReasoningText())
}

// TestStreamRetryWithoutFollowupDeltaKeepsPartialForContinuation pins the
// partial text the coordinator's continuation reads when the retries fail;
// mimic the checkpoint persist before retrying. Revert check (red): with
// onRetry truncating eagerly this fails — the partial text is already gone
// from the message and the row.
func TestStreamRetryWithoutFollowupDeltaKeepsPartialForContinuation(t *testing.T) {
	f := newRetryFixture(t, "retry-no-delta")
	stepMsg := f.beginStep(t)
	require.NoError(t, f.ts.onTextDelta("t1", "Section 1: Introduction"))
	// Mimic the running system's mid-step persistence (checkpoint ticker /
	// step-boundary write): the partial is on disk before the retry fires.
	require.NoError(t, f.svc.Update(context.Background(), f.ts.currentAssistant.Clone()))
	f.ts.onRetry(codexCutError(), 2*time.Second)
	require.Equal(t, "Section 1: Introduction", f.ts.currentAssistant.FullText())
	persisted, err := f.svc.Get(context.Background(), stepMsg.ID)
	require.NoError(t, err)
	require.Equal(t, "Section 1: Introduction", persisted.Content().Text)
}

// TestTurnStreamDeltasAccumulateWithoutRetry is the control: with NO retry,
// text and reasoning deltas accumulate exactly as before the fix.
func TestTurnStreamDeltasAccumulateWithoutRetry(t *testing.T) {
	f := newRetryFixture(t, "retry-control")
	f.beginStep(t)

	require.NoError(t, f.ts.onReasoningStart("r1", fantasy.ReasoningContent{Text: "thinking"}))
	require.NoError(t, f.ts.onReasoningDelta("r1", " harder"))
	require.NoError(t, f.ts.onTextDelta("t1", "hello "))
	require.NoError(t, f.ts.onTextDelta("t1", "world"))

	require.Equal(t, "hello world", f.ts.currentAssistant.FullText())
	require.Equal(t, "thinking harder", f.ts.currentAssistant.ReasoningContent().Thinking)
}

// TestStreamRetryFlagClearedAtStepStart pins that a flag left armed by a
// step whose retried attempt streamed nothing does not affect the next step.
// Under lazy consumption it would be consumed by step 2's first content
// callback on its fresh row anyway. This test's red shows against the reverted
// eager implementation (which destroys and persists step 1's row) and against
// any future change that truncates at non-content callbacks.
func TestStreamRetryFlagClearedAtStepStart(t *testing.T) {
	f := newRetryFixture(t, "retry-step-reset")
	step1 := f.beginStep(t)
	require.NoError(t, f.ts.onTextDelta("t1", "step 1 answer"))
	// Mimic the running system's mid-step persistence (checkpoint ticker /
	// step-boundary write): step 1's partial is on disk before the retry.
	require.NoError(t, f.svc.Update(context.Background(), f.ts.currentAssistant.Clone()))
	f.ts.onRetry(codexCutError(), 2*time.Second) // armed; nothing follows in step 1
	_ = f.beginStep(t)                           // step 2: fresh row, per-step reset
	require.NoError(t, f.ts.onReasoningStart("r2", fantasy.ReasoningContent{Text: "step 2 thinking"}))
	require.NoError(t, f.ts.onTextDelta("t2", "step 2 answer"))
	require.Equal(t, "step 2 thinking", f.ts.currentAssistant.ReasoningContent().Thinking)
	require.Equal(t, "step 2 answer", f.ts.currentAssistant.FullText())
	saved1, err := f.svc.Get(context.Background(), step1.ID)
	require.NoError(t, err)
	require.Equal(t, "step 1 answer", saved1.Content().Text)
}

// TestStreamRetrySecondStepKeepsEarlierStepContent: a retry inside the
// SECOND step must not touch the first step's already-persisted message.
func TestStreamRetrySecondStepKeepsEarlierStepContent(t *testing.T) {
	f := newRetryFixture(t, "retry-step2")

	// Step 1 composes and is persisted at its step boundary.
	step1 := f.beginStep(t)
	require.NoError(t, f.ts.onTextDelta("t1", "first step answer"))
	require.NoError(t, f.svc.Update(context.Background(), step1.Clone()))

	// Step 2 gets a fresh assistant row (prepareStep), is cut mid-body,
	// and the retried attempt answers on its own.
	step2 := f.beginStep(t)
	require.NoError(t, f.ts.onTextDelta("t2", "second step partial"))
	f.ts.onRetry(codexCutError(), 2*time.Second)
	require.NoError(t, f.ts.onTextDelta("t3", "second step final"))

	saved1, err := f.svc.Get(context.Background(), step1.ID)
	require.NoError(t, err)
	require.Equal(t, "first step answer", saved1.FullText())

	require.Equal(t, "second step final", f.ts.currentAssistant.FullText())
	saved2, err := f.svc.Get(context.Background(), step2.ID)
	require.NoError(t, err)
	require.Empty(t, saved2.Content().Text)
}
