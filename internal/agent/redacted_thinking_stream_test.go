package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/message"
)

func TestStreamCapturesRedactedMetadataAtStartOrEnd(t *testing.T) {
	f := newRetryFixture(t, "redacted-thinking-metadata-boundaries")
	f.beginStep(t)

	require.NoError(t, f.ts.onReasoningStart("start", fantasy.ReasoningContent{ProviderMetadata: fantasy.ProviderMetadata{
		anthropic.Name: &anthropic.ReasoningOptionMetadata{RedactedData: "from-start"},
	}}))
	require.NoError(t, f.ts.onReasoningEnd("start", fantasy.ReasoningContent{}))
	require.NoError(t, f.ts.onReasoningStart("end", fantasy.ReasoningContent{}))
	require.NoError(t, f.ts.onReasoningEnd("end", fantasy.ReasoningContent{ProviderMetadata: fantasy.ProviderMetadata{
		anthropic.Name: &anthropic.ReasoningOptionMetadata{RedactedData: "from-end"},
	}}))

	persisted, err := f.svc.Get(context.Background(), f.ts.currentAssistant.ID)
	require.NoError(t, err)
	contents := persisted.ReasoningContents()
	require.Len(t, contents, 2)
	require.Equal(t, "from-start", contents[0].RedactedData)
	require.Equal(t, "from-end", contents[1].RedactedData)
}

func TestStreamPersistsAndSendsInterleavedRedactedThinking(t *testing.T) {
	f := newRetryFixture(t, "redacted-thinking-order")
	stepMsg := f.beginStep(t)

	require.NoError(t, f.ts.onReasoningStart("r1", fantasy.ReasoningContent{Text: "first thought"}))
	require.NoError(t, f.ts.onReasoningEnd("r1", fantasy.ReasoningContent{ProviderMetadata: fantasy.ProviderMetadata{
		anthropic.Name: &anthropic.ReasoningOptionMetadata{Signature: "sig-one"},
	}}))
	require.NoError(t, f.ts.onReasoningStart("redacted", fantasy.ReasoningContent{ProviderMetadata: fantasy.ProviderMetadata{
		anthropic.Name: &anthropic.ReasoningOptionMetadata{RedactedData: "opaque-redacted-payload"},
	}}))
	require.NoError(t, f.ts.onReasoningEnd("redacted", fantasy.ReasoningContent{ProviderMetadata: fantasy.ProviderMetadata{
		anthropic.Name: &anthropic.ReasoningOptionMetadata{RedactedData: "opaque-redacted-payload"},
	}}))
	require.NoError(t, f.ts.onReasoningStart("r2", fantasy.ReasoningContent{Text: "second thought"}))
	require.NoError(t, f.ts.onReasoningEnd("r2", fantasy.ReasoningContent{ProviderMetadata: fantasy.ProviderMetadata{
		anthropic.Name: &anthropic.ReasoningOptionMetadata{Signature: "sig-two"},
	}}))
	require.NoError(t, f.ts.onTextDelta("text", "visible"))
	require.NoError(t, f.svc.Update(context.Background(), f.ts.currentAssistant.Clone()))

	persisted, err := f.svc.Get(context.Background(), stepMsg.ID)
	require.NoError(t, err)
	contents := persisted.ReasoningContents()
	require.Len(t, contents, 3)
	require.Equal(t, []string{"first thought", "", "second thought"}, []string{contents[0].Thinking, contents[1].Thinking, contents[2].Thinking})
	require.Equal(t, "sig-one", contents[0].Signature)
	require.Equal(t, "opaque-redacted-payload", contents[1].RedactedData)
	require.Equal(t, "sig-two", contents[2].Signature)

	capturedRequest := make(chan struct {
		body map[string]any
		err  error
	}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		err := json.NewDecoder(r.Body).Decode(&body)
		capturedRequest <- struct {
			body map[string]any
			err  error
		}{body: body, err: err}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-3-5-sonnet-20241022\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()
	provider, err := anthropic.New(anthropic.WithBaseURL(server.URL), anthropic.WithAPIKey("test"))
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "claude-3-5-sonnet-20241022")
	require.NoError(t, err)
	prompt := (&message.Message{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "Earlier prompt."}}}).ToAIMessage()
	prompt = append(prompt, persisted.ToAIMessage()...)
	prompt = append(prompt, (&message.Message{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "Continue."}}}).ToAIMessage()...)
	require.Len(t, prompt, 3)
	require.Equal(t, fantasy.MessageRoleUser, prompt[0].Role)
	require.Equal(t, fantasy.MessageRoleAssistant, prompt[1].Role)
	require.Equal(t, fantasy.MessageRoleUser, prompt[2].Role)
	stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: prompt})
	require.NoError(t, err)
	for part := range stream {
		require.NotEqual(t, fantasy.StreamPartTypeError, part.Type, "stream event error: %v", part.Error)
		require.NoError(t, part.Error)
	}
	request := <-capturedRequest
	require.NoError(t, request.err)
	body := request.body
	apiMessages, ok := body["messages"].([]any)
	require.True(t, ok, "Anthropic request did not include a messages array: %#v", body)
	var apiRoles []string
	for _, item := range apiMessages {
		apiMessage, ok := item.(map[string]any)
		require.True(t, ok, "Anthropic request message has unexpected type: %#v", item)
		role, ok := apiMessage["role"].(string)
		require.True(t, ok, "Anthropic request message has no role: %#v", apiMessage)
		apiRoles = append(apiRoles, role)
	}
	require.Equal(t, []string{"user", "assistant", "user"}, apiRoles)
	assistantMessage, ok := apiMessages[1].(map[string]any)
	require.True(t, ok, "Anthropic request message has unexpected type: %#v", apiMessages[1])
	require.Equal(t, "assistant", assistantMessage["role"])
	content, ok := assistantMessage["content"].([]any)
	require.True(t, ok, "Anthropic request message did not include content: %#v", assistantMessage)
	require.Len(t, content, 4)
	var contentTypes []string
	for _, item := range content {
		part, ok := item.(map[string]any)
		require.True(t, ok, "Anthropic content item has unexpected type: %#v", item)
		contentTypes = append(contentTypes, part["type"].(string))
	}
	require.Equal(t, []string{"thinking", "redacted_thinking", "thinking", "text"}, contentTypes)
	first, ok := content[0].(map[string]any)
	require.True(t, ok, "first content item has unexpected type: %#v", content[0])
	require.Equal(t, "sig-one", first["signature"])
	require.Equal(t, map[string]any{"type": "redacted_thinking", "data": "opaque-redacted-payload"}, content[1])
	third, ok := content[2].(map[string]any)
	require.True(t, ok, "third content item has unexpected type: %#v", content[2])
	require.Equal(t, "sig-two", third["signature"])
	fourth, ok := content[3].(map[string]any)
	require.True(t, ok, "fourth content item has unexpected type: %#v", content[3])
	require.Equal(t, "visible", fourth["text"])
}
