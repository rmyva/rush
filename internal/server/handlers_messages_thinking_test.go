package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/message"
)

// TestHandleUpdateMessageThinkingTargetsPartIndex verifies indexed thinking
// edits update the selected reasoning part while preserving provider metadata.
func TestHandleUpdateMessageThinkingTargetsPartIndex(t *testing.T) {
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	ctx := t.Context()
	sess, err := a.Sessions.Create(ctx, "update-thinking-index-test")
	require.NoError(t, err)
	m, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "first", Signature: "sig-1"},
			message.TextContent{Text: "answer"},
			message.ReasoningContent{Thinking: "second", Signature: "sig-2", ToolID: "tool-2"},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	})
	require.NoError(t, err)

	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 1)
	hub.register <- client

	payload, err := json.Marshal(struct {
		MessageID string `json:"messageID"`
		Thinking  string `json:"thinking"`
		PartIndex int    `json:"partIndex"`
	}{MessageID: m.ID, Thinking: "edited second", PartIndex: 2})
	require.NoError(t, err)
	handleUpdateMessageThinking(ctx, a, client, WSMessage{ID: "update-thinking", Payload: payload})

	var reply WSMessage
	require.NoError(t, json.Unmarshal(<-client.send, &reply))
	require.Equal(t, EventResponse, reply.Type)
	updated, err := a.Messages.Get(ctx, m.ID)
	require.NoError(t, err)
	first, ok := updated.Parts[0].(message.ReasoningContent)
	require.True(t, ok)
	require.Equal(t, "first", first.Thinking)
	require.Equal(t, "sig-1", first.Signature)
	second, ok := updated.Parts[2].(message.ReasoningContent)
	require.True(t, ok)
	require.Equal(t, "edited second", second.Thinking)
	require.Equal(t, "sig-2", second.Signature)
	require.Equal(t, "tool-2", second.ToolID)
}
