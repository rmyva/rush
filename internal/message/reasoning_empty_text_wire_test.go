package message

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"github.com/stretchr/testify/require"
)

func TestEmptyReasoningSurvivesPartsCodecAndToAIMessage(t *testing.T) {
	m := &Message{Role: Assistant}
	m.AppendReasoningContent("")
	m.AppendReasoningSignature("anthropic-signature")
	m.AppendThoughtSignature("google-signature", "google-tool-id")
	m.FinishThinking()

	parts, err := marshalParts(m.Parts)
	require.NoError(t, err)
	decoded, err := unmarshalParts(parts)
	require.NoError(t, err)
	m.Parts = decoded
	reasoningContent := m.ReasoningContent()
	require.Equal(t, "google-signature", reasoningContent.ThoughtSignature)
	require.Equal(t, "google-tool-id", reasoningContent.ToolID)

	ai := m.ToAIMessage()
	require.Len(t, ai, 1)
	require.Len(t, ai[0].Content, 1)
	reasoning, ok := ai[0].Content[0].(fantasy.ReasoningPart)
	require.True(t, ok)
	require.Empty(t, reasoning.Text)
	require.Equal(t, "anthropic-signature", reasoning.ProviderOptions[anthropic.Name].(*anthropic.ReasoningOptionMetadata).Signature)
	googleMetadata, ok := reasoning.ProviderOptions[google.Name].(*google.ReasoningMetadata)
	require.True(t, ok)
	require.Equal(t, "google-signature", googleMetadata.Signature)
	require.Equal(t, "google-tool-id", googleMetadata.ToolID)

	user := (&Message{Role: User, Parts: []ContentPart{TextContent{Text: "Continue."}}}).ToAIMessage()
	require.Len(t, user, 1)
	ai = append(ai, user...)
	require.Len(t, ai, 2)
	require.Equal(t, fantasy.MessageRoleUser, ai[1].Role)
}

func TestReasoningPartsRequireTextOrMetadata(t *testing.T) {
	responsesData := &openai.ResponsesReasoningMetadata{ItemID: "rs_metadata"}
	tests := []struct {
		name             string
		thinking         string
		setMetadata      func(*Message)
		expectedProvider string
	}{
		{
			name: "empty without metadata",
		},
		{
			name:             "anthropic signature",
			setMetadata:      func(m *Message) { m.AppendReasoningSignature("anthropic-signature") },
			expectedProvider: anthropic.Name,
		},
		{
			name:             "google thought signature",
			setMetadata:      func(m *Message) { m.AppendThoughtSignature("google-signature", "google-tool-id") },
			expectedProvider: google.Name,
		},
		{
			name:        "responses data only",
			setMetadata: func(m *Message) { m.SetReasoningResponsesData(responsesData) },
		},
		{
			name:     "nonempty text",
			thinking: "reasoning text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Message{Role: Assistant}
			m.AppendReasoningContent(tt.thinking)
			if tt.setMetadata != nil {
				tt.setMetadata(m)
			}

			messages := m.ToAIMessage()
			var reasoningParts []fantasy.ReasoningPart
			for _, message := range messages {
				for _, part := range message.Content {
					if reasoning, ok := part.(fantasy.ReasoningPart); ok {
						reasoningParts = append(reasoningParts, reasoning)
					}
				}
			}

			// Responses metadata without encrypted payload has no wire effect; it must not create a fantasy reasoning part.
			wantCount := 0
			if tt.expectedProvider != "" || tt.thinking != "" {
				wantCount = 1
			}
			require.Len(t, reasoningParts, wantCount)
			if wantCount == 0 {
				return
			}

			reasoning := reasoningParts[0]
			require.Equal(t, tt.thinking, reasoning.Text)
			switch tt.expectedProvider {
			case anthropic.Name:
				metadata, ok := reasoning.ProviderOptions[anthropic.Name].(*anthropic.ReasoningOptionMetadata)
				require.True(t, ok)
				require.Equal(t, "anthropic-signature", metadata.Signature)
				require.NotContains(t, reasoning.ProviderOptions, google.Name)
			case google.Name:
				metadata, ok := reasoning.ProviderOptions[google.Name].(*google.ReasoningMetadata)
				require.True(t, ok)
				require.Equal(t, "google-signature", metadata.Signature)
				require.Equal(t, "google-tool-id", metadata.ToolID)
				require.NotContains(t, reasoning.ProviderOptions, anthropic.Name)
			default:
				require.Empty(t, reasoning.ProviderOptions)
			}
		})
	}
}

func TestEmptyReasoningPreservedOnAnthropicWire(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-3-5-sonnet-20241022\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	m := &Message{Role: Assistant}
	m.AppendReasoningContent("")
	m.AppendReasoningSignature("signed-empty-thinking")
	m.FinishThinking()
	m.AppendContent("ok")
	parts, err := marshalParts(m.Parts)
	require.NoError(t, err)
	m.Parts, err = unmarshalParts(parts)
	require.NoError(t, err)
	messages := m.ToAIMessage()
	messages = append(messages, (&Message{Role: User, Parts: []ContentPart{TextContent{Text: "Continue."}}}).ToAIMessage()...)

	provider, err := anthropic.New(anthropic.WithBaseURL(server.URL), anthropic.WithAPIKey("test"))
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "claude-3-5-sonnet-20241022")
	require.NoError(t, err)
	stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: messages})
	require.NoError(t, err)
	for range stream {
	}

	apiMessages, ok := body["messages"].([]any)
	require.True(t, ok)
	require.Len(t, apiMessages, 2)
	content, ok := apiMessages[0].(map[string]any)["content"].([]any)
	require.True(t, ok)
	var thinkingFound, textFound bool
	for _, item := range content {
		part := item.(map[string]any)
		switch part["type"] {
		case "thinking":
			thinkingFound = true
			require.Contains(t, part, "thinking")
			require.Equal(t, "", part["thinking"])
			require.Equal(t, "signed-empty-thinking", part["signature"])
		case "text":
			textFound = true
			require.Equal(t, "ok", part["text"])
		}
	}
	require.True(t, thinkingFound)
	require.True(t, textFound)
	require.Equal(t, "user", apiMessages[1].(map[string]any)["role"])
}

func TestMultipleSignedReasoningBlocksPreservedOnAnthropicWire(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-3-5-sonnet-20241022\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	m := &Message{Role: Assistant}
	m.StartReasoningContent("")
	m.AppendReasoningContent("")
	m.AppendReasoningSignature("signature-")
	m.AppendReasoningSignature("one")
	m.FinishThinking()
	m.StartReasoningContent("")
	m.AppendReasoningContent("")
	m.AppendReasoningSignature("signature-")
	m.AppendReasoningSignature("two")
	m.FinishThinking()
	m.AppendContent("visible ")
	m.AppendContent("text")
	m.AddToolCall(ToolCall{ID: "lookup-call", Name: "lookup", Input: `{"q":"`})
	m.AppendToolCallInput("lookup-call", `x"}`)
	m.FinishToolCall("lookup-call")
	parts, err := marshalParts(m.Parts)
	require.NoError(t, err)
	m.Parts, err = unmarshalParts(parts)
	require.NoError(t, err)
	prompt := m.ToAIMessage()
	prompt = append(prompt, (&Message{Role: User, Parts: []ContentPart{TextContent{Text: "Continue."}}}).ToAIMessage()...)

	provider, err := anthropic.New(anthropic.WithBaseURL(server.URL), anthropic.WithAPIKey("test"))
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "claude-3-5-sonnet-20241022")
	require.NoError(t, err)
	stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: prompt})
	require.NoError(t, err)
	for range stream {
	}

	apiMessages := body["messages"].([]any)
	content := apiMessages[0].(map[string]any)["content"].([]any)
	var types []string
	for _, item := range content {
		part := item.(map[string]any)
		types = append(types, part["type"].(string))
	}
	require.Equal(t, []string{"thinking", "thinking", "text", "tool_use"}, types)
	first := content[0].(map[string]any)
	require.Equal(t, "", first["thinking"])
	require.Equal(t, "signature-one", first["signature"])
	second := content[1].(map[string]any)
	require.Equal(t, "", second["thinking"])
	require.Equal(t, "signature-two", second["signature"])
	text := content[2].(map[string]any)
	require.Equal(t, "visible text", text["text"])
	tool := content[3].(map[string]any)
	require.Equal(t, "lookup-call", tool["id"])
	require.Equal(t, "lookup", tool["name"])
	require.Equal(t, map[string]any{"q": "x"}, tool["input"])
}

func TestMultipleGeminiThoughtSignaturesPreservedOnWire(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
	}))
	defer server.Close()

	m := &Message{Role: Assistant}
	m.StartReasoningContent("")
	m.AppendReasoningContent("")
	m.AppendThoughtSignature("google-signature-", "call-one")
	m.AppendThoughtSignature("one", "call-one")
	m.FinishThinking()
	m.AddToolCall(ToolCall{ID: "call-one", Name: "lookup_one", Input: `{"q":"`})
	m.AppendToolCallInput("call-one", `one"}`)
	m.FinishToolCall("call-one")
	m.AppendContent("visible ")
	m.AppendContent("text")
	m.StartReasoningContent("")
	m.AppendReasoningContent("")
	m.AppendThoughtSignature("google-signature-", "call-two")
	m.AppendThoughtSignature("two", "call-two")
	m.FinishThinking()
	m.AddToolCall(ToolCall{ID: "call-two", Name: "lookup_two", Input: `{"q":"`})
	m.AppendToolCallInput("call-two", `two"}`)
	m.FinishToolCall("call-two")
	parts, err := marshalParts(m.Parts)
	require.NoError(t, err)
	m.Parts, err = unmarshalParts(parts)
	require.NoError(t, err)
	prompt := m.ToAIMessage()
	prompt = append(prompt, (&Message{Role: User, Parts: []ContentPart{TextContent{Text: "Continue."}}}).ToAIMessage()...)

	provider, err := google.New(google.WithBaseURL(server.URL), google.WithGeminiAPIKey("test"))
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gemini-2.5-flash")
	require.NoError(t, err)
	_, err = model.Generate(t.Context(), fantasy.Call{Prompt: prompt})
	require.NoError(t, err)

	contents := body["contents"].([]any)
	assistantParts := contents[0].(map[string]any)["parts"].([]any)
	// fantasy's Google mapper emits only text and function-call parts here; it
	// attaches each thought signature to the following tool call rather than
	// serializing the reasoning text as a Gemini thought part.
	require.Len(t, assistantParts, 3)
	var textFound bool
	calls := map[string]string{}
	callIDs := map[string]string{}
	for _, item := range assistantParts {
		part := item.(map[string]any)
		if text, ok := part["text"].(string); ok {
			textFound = true
			require.Equal(t, "visible text", text)
		}
		call, ok := part["functionCall"].(map[string]any)
		if !ok {
			continue
		}
		encoded, ok := part["thoughtSignature"].(string)
		require.True(t, ok)
		signature, err := base64.StdEncoding.DecodeString(encoded)
		require.NoError(t, err)
		name := call["name"].(string)
		calls[name] = string(signature)
		callIDs[name] = call["id"].(string)
		q := map[string]string{"lookup_one": "one", "lookup_two": "two"}[name]
		require.Equal(t, map[string]any{"q": q}, call["args"])
	}
	require.True(t, textFound)
	require.Equal(t, map[string]string{
		"lookup_one": "google-signature-one",
		"lookup_two": "google-signature-two",
	}, calls)
	require.Equal(t, map[string]string{"lookup_one": "call-one", "lookup_two": "call-two"}, callIDs)
}

func TestEmptyReasoningSignaturePreservedOnGoogleGenerateWire(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
	}))
	defer server.Close()

	m := &Message{Role: Assistant}
	m.AppendReasoningContent("")
	m.AppendThoughtSignature("google-empty-thought-signature", "call-1")
	m.FinishThinking()
	m.AddToolCall(ToolCall{ID: "call-1", Name: "lookup", Input: `{"q":"x"}`})
	parts, err := marshalParts(m.Parts)
	require.NoError(t, err)
	m.Parts, err = unmarshalParts(parts)
	require.NoError(t, err)
	prompt := m.ToAIMessage()
	prompt = append(prompt, (&Message{Role: User, Parts: []ContentPart{TextContent{Text: "Continue."}}}).ToAIMessage()...)

	provider, err := google.New(google.WithBaseURL(server.URL), google.WithGeminiAPIKey("test"))
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gemini-2.5-flash")
	require.NoError(t, err)
	_, err = model.Generate(t.Context(), fantasy.Call{Prompt: prompt})
	require.NoError(t, err)

	contents, ok := body["contents"].([]any)
	require.True(t, ok)
	require.Len(t, contents, 2)
	require.Equal(t, "model", contents[0].(map[string]any)["role"])
	assistantParts := contents[0].(map[string]any)["parts"].([]any)
	require.Len(t, assistantParts, 1)
	var functionCall map[string]any
	for _, item := range assistantParts {
		part := item.(map[string]any)
		if call, ok := part["functionCall"].(map[string]any); ok {
			functionCall = part
			require.Equal(t, "lookup", call["name"])
		}
	}
	require.NotNil(t, functionCall)
	encodedSignature, ok := functionCall["thoughtSignature"].(string)
	require.True(t, ok)
	signature, err := base64.StdEncoding.DecodeString(encodedSignature)
	require.NoError(t, err)
	// fantasy keeps the signature on the function call but does not emit an
	// empty reasoning-text part; this verifies signature replay, not thought text.
	require.Equal(t, "google-empty-thought-signature", string(signature))
	userParts := contents[1].(map[string]any)["parts"].([]any)
	require.Equal(t, "Continue.", userParts[0].(map[string]any)["text"])
}
