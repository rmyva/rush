package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

type stallRetryToolModel struct {
	mu       sync.Mutex
	requests []fantasy.Prompt
	second   chan struct{}
	third    chan struct{}
}

func (m *stallRetryToolModel) Provider() string { return "stall-retry-test" }
func (m *stallRetryToolModel) Model() string    { return "stall-retry-test" }
func (m *stallRetryToolModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	panic("unexpected Generate")
}
func (m *stallRetryToolModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	panic("unexpected GenerateObject")
}
func (m *stallRetryToolModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	panic("unexpected StreamObject")
}
func (m *stallRetryToolModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleRequest(call.Prompt) {
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "title"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "title", Delta: "Stall retry"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}
	m.mu.Lock()
	m.requests = append(m.requests, call.Prompt)
	n := len(m.requests)
	m.mu.Unlock()
	return func(yield func(fantasy.StreamPart) bool) {
		switch n {
		case 1:
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "side-effect-1", ToolCallName: "side_effect", ToolCallInput: `{}`})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
		case 2:
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: "reasoning-2"})
			close(m.second)
			<-ctx.Done()
		case 3:
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text-3"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text-3", Delta: "recovered"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			close(m.third)
		default:
			panic("unexpected provider request")
		}
	}, nil
}

// TestRunTurnStallRetryKeepsCompletedToolResult is a red/green regression
// for retry history reconstruction: request 1 runs a side effect, request 2
// sees its result then stalls while reasoning, and request 3 must retain that
// result without replaying the tool. Channels and the stall-clock seam drive
// the retry deterministically without sleep-based correctness windows.
func TestRunTurnStallRetryKeepsCompletedToolResult(t *testing.T) {
	env := testEnv(t)
	t.Cleanup(stallDumpWrites.Wait)
	model := &stallRetryToolModel{second: make(chan struct{}), third: make(chan struct{})}
	var toolCalls int
	tool := fantasy.NewAgentTool("side_effect", "count side effect", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		toolCalls++
		return fantasy.NewTextResponse("side-effect-result"), nil
	})
	sa := testSessionAgent(env, model, model, "test", tool).(*sessionAgent)
	sa.SetTimeoutOptions(false, time.Hour)
	sa.streamWatchdogTick = time.Hour
	session, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	// The fake clock stays at t0, so every progress mark is t0 and the
	// elapsed time the sampler sees is exactly what the test sets.
	t0 := time.Now()
	setNow := installFakeStallNow(t)
	setNow(t0)
	done := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: session.ID, Prompt: "run it", MaxOutputTokens: 100, CallOptions: &CallOptions{TurnStallTimeout: time.Second}})
		done <- runErr
	}()
	<-model.second
	clockValue, ok := armedStallClocks.Load(session.ID)
	require.True(t, ok)
	setNow(t0.Add(11 * time.Minute))
	stallSamplerCheck(session.ID, clockValue.(*stallClock), stallNow())
	<-model.third
	require.NoError(t, <-done)

	model.mu.Lock()
	requests := append([]fantasy.Prompt(nil), model.requests...)
	model.mu.Unlock()
	require.Len(t, requests, 3)
	require.True(t, promptHasToolResultText(requests[1], "side-effect-result"), "request 2 must observe the completed side-effect result")
	require.True(t, promptHasToolResultText(requests[2], "side-effect-result"), "retry request 3 must include the completed side-effect result")
	require.Equal(t, 1, toolCalls, "stall retry must not replay the side-effect tool")
}

// isTitleRequest recognises the first-message title request, which the agent
// sends to a configured model and which must not consume a script step.
func isTitleRequest(prompt fantasy.Prompt) bool {
	for _, msg := range prompt {
		if msg.Role != fantasy.MessageRoleSystem {
			continue
		}
		for _, part := range msg.Content {
			if text, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && strings.Contains(text.Text, "short title") {
				return true
			}
		}
	}
	return false
}

func promptHasToolResultText(prompt fantasy.Prompt, text string) bool {
	for _, msg := range prompt {
		if msg.Role != fantasy.MessageRoleTool {
			continue
		}
		for _, part := range msg.Content {
			if result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if output, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result.Output); ok && strings.Contains(output.Text, text) {
					return true
				}
			}
		}
	}
	return false
}

var _ fantasy.LanguageModel = (*stallRetryToolModel)(nil)
