package message

import (
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"github.com/stretchr/testify/require"
)

func TestNilEncryptedResponsesReasoningIsDroppedWithToolCall(t *testing.T) {
	m := &Message{Role: Assistant, Parts: []ContentPart{
		ReasoningContent{ResponsesData: &openai.ResponsesReasoningMetadata{
			ItemID:  "rs_1",
			Summary: []string{""},
		}},
		ToolCall{ID: "lookup-call", Name: "lookup", Input: `{"q":"value"}`},
	}}

	content := m.ToAIMessage()[0].Content
	require.Len(t, content, 1)
	toolCall, ok := content[0].(fantasy.ToolCallPart)
	require.True(t, ok)
	require.Equal(t, "lookup-call", toolCall.ToolCallID)
}

func TestEmptyEncryptedResponsesReasoningIsDroppedWithToolCall(t *testing.T) {
	m := &Message{Role: Assistant, Parts: []ContentPart{
		ReasoningContent{ResponsesData: &openai.ResponsesReasoningMetadata{
			ItemID:           "rs_1",
			EncryptedContent: stringPointer(""),
			Summary:          []string{""},
		}},
		ToolCall{ID: "lookup-call", Name: "lookup", Input: `{"q":"value"}`},
	}}

	content := m.ToAIMessage()[0].Content
	require.Len(t, content, 1)
	toolCall, ok := content[0].(fantasy.ToolCallPart)
	require.True(t, ok)
	require.Equal(t, "lookup-call", toolCall.ToolCallID)
}

func TestEncryptedResponsesReasoningRoundTripWithToolCall(t *testing.T) {
	m := &Message{Role: Assistant}
	m.StartReasoningContent("")
	m.AppendReasoningContent("")
	m.SetReasoningResponsesData(&openai.ResponsesReasoningMetadata{
		ItemID:           "rs_1",
		EncryptedContent: stringPointer("cipher"),
		Summary:          []string{""},
	})
	m.FinishThinking()
	m.StartReasoningContent("")
	m.AppendReasoningContent("ordered text")
	m.AppendReasoningSignature("anthropic-signature")
	m.FinishThinking()
	m.AddToolCall(ToolCall{ID: "lookup-call", Name: "lookup", Input: `{"q":"value"}`})
	m.FinishToolCall("lookup-call")

	encoded, err := marshalParts(m.Parts)
	require.NoError(t, err)
	decoded, err := unmarshalParts(encoded)
	require.NoError(t, err)
	m.Parts = decoded

	prompt := m.ToAIMessage()
	require.Len(t, prompt, 1)
	require.Len(t, prompt[0].Content, 3)

	encrypted, ok := prompt[0].Content[0].(fantasy.ReasoningPart)
	require.True(t, ok)
	require.Empty(t, encrypted.Text)
	require.Equal(t, &openai.ResponsesReasoningMetadata{
		ItemID:           "rs_1",
		EncryptedContent: stringPointer("cipher"),
		Summary:          []string{""},
	}, encrypted.ProviderOptions[openai.Name])
	require.NotContains(t, encrypted.ProviderOptions, anthropic.Name)
	require.NotContains(t, encrypted.ProviderOptions, google.Name)

	ordered, ok := prompt[0].Content[1].(fantasy.ReasoningPart)
	require.True(t, ok)
	require.Equal(t, "ordered text", ordered.Text)
	require.Equal(t, &anthropic.ReasoningOptionMetadata{Signature: "anthropic-signature"}, ordered.ProviderOptions[anthropic.Name])
	require.NotContains(t, ordered.ProviderOptions, openai.Name)
	require.NotContains(t, ordered.ProviderOptions, google.Name)

	toolCall, ok := prompt[0].Content[2].(fantasy.ToolCallPart)
	require.True(t, ok)
	require.Equal(t, "lookup-call", toolCall.ToolCallID)
	require.Equal(t, "lookup", toolCall.ToolName)
	require.JSONEq(t, `{"q":"value"}`, toolCall.Input)
}
