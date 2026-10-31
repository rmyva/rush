package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestHandleStreamFailureCreatesResultWhenOnlyEarlierReusedIDExists(t *testing.T) {
	const (
		sessionID = "failure-reused-tool-result"
		reusedID  = "reused-call"
	)
	f := newRetryFixture(t, sessionID)

	_, err := f.svc.Create(context.Background(), sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{ID: reusedID, Name: "prior-tool", Finished: true}},
	})
	require.NoError(t, err)
	_, err = f.svc.Create(context.Background(), sessionID, message.CreateMessageParams{
		Role:  message.Tool,
		Parts: []message.ContentPart{message.ToolResult{ToolCallID: reusedID, Content: "stale result"}},
	})
	require.NoError(t, err)

	current := f.beginStep(t)
	current.AddToolCall(message.ToolCall{ID: reusedID, Name: "current-tool", Finished: true})
	require.NoError(t, f.svc.Update(context.Background(), *current))
	f.ts.smartModel.ModelCfg.Provider = "test"

	_, _, _, err = f.ts.handleStreamFailure(nil, errors.New("stream failed"), false)
	require.Error(t, err)

	msgs, err := f.svc.List(context.Background(), sessionID)
	require.NoError(t, err)
	currentIndex := -1
	for i := range msgs {
		if msgs[i].ID == current.ID {
			currentIndex = i
			break
		}
	}
	require.NotEqual(t, -1, currentIndex, "current assistant message must be persisted")
	var currentResult *message.ToolResult
	for _, msg := range msgs[currentIndex+1:] {
		if msg.Role != message.Tool {
			continue
		}
		for _, result := range msg.ToolResults() {
			if result.ToolCallID == reusedID {
				currentResult = &result
			}
		}
	}
	require.NotNil(t, currentResult, "recovery must create a result for the current assistant call despite the earlier reused ID")
	require.True(t, currentResult.IsError)
	require.Equal(t, "There was an error while executing the tool", currentResult.Content)
}

func TestHasToolResultAfterAssistantPositionWithReusedID(t *testing.T) {
	const reusedID = "reused-call"
	msgs := []message.Message{
		{ID: "prior-assistant", Role: message.Assistant},
		{ID: "prior-tool", Role: message.Tool, Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: reusedID, Content: "stale result"},
		}},
		{ID: "issuing-assistant", Role: message.Assistant},
		{ID: "current-tool", Role: message.Tool, Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: reusedID, Content: "current result"},
		}},
	}

	require.True(t, hasToolResultAfterAssistantPosition(msgs, "issuing-assistant", reusedID))
	require.False(t, hasToolResultAfterAssistantPosition(msgs[:3], "issuing-assistant", reusedID))
}
