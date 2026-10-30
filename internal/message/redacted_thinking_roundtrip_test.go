package message

import (
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/stretchr/testify/require"
)

func TestLegacyReasoningPartsReserializeByteIdentically(t *testing.T) {
	const fixture = `[{"type":"reasoning","data":{"thinking":"legacy thought","signature":"legacy-signature","thought_signature":"","tool_id":"","responses_data":null,"started_at":11,"finished_at":12}},{"type":"text","data":{"text":"answer"}}]`
	parts, err := unmarshalParts([]byte(fixture))
	require.NoError(t, err)
	encoded, err := marshalParts(parts)
	require.NoError(t, err)
	require.Equal(t, fixture, string(encoded))
}

func TestToAIMessageRedactedMetadataPrecedenceAndEmptyReasoning(t *testing.T) {
	m := &Message{Role: Assistant, Parts: []ContentPart{
		ReasoningContent{RedactedData: "opaque"},
		ReasoningContent{Thinking: "signed", Signature: "signed-token", RedactedData: "ignored"},
		ReasoningContent{Thinking: "visible"},
	}}
	ai := m.ToAIMessage()
	require.Len(t, ai, 1)
	require.Len(t, ai[0].Content, 3)
	redacted := ai[0].Content[0].(fantasy.ReasoningPart)
	require.Empty(t, redacted.Text)
	require.Equal(t, &anthropic.ReasoningOptionMetadata{RedactedData: "opaque"}, redacted.ProviderOptions[anthropic.Name])
	signed := ai[0].Content[1].(fantasy.ReasoningPart)
	require.Equal(t, &anthropic.ReasoningOptionMetadata{Signature: "signed-token"}, signed.ProviderOptions[anthropic.Name])
	visible := ai[0].Content[2].(fantasy.ReasoningPart)
	require.Equal(t, "visible", visible.Text)
	require.Empty(t, visible.ProviderOptions[anthropic.Name])
}

func TestRedactedDataMutatorTargetsCurrentReasoningAndCreatesBlock(t *testing.T) {
	m := &Message{}
	m.SetReasoningRedactedData("opaque")
	require.Equal(t, "opaque", m.ReasoningContent().RedactedData)

	m = &Message{Parts: []ContentPart{ReasoningContent{Thinking: "first", FinishedAt: 1}, ReasoningContent{Thinking: "current"}}}
	m.SetReasoningRedactedData("current block")
	require.Equal(t, "current block", m.ReasoningContents()[1].RedactedData)
}
