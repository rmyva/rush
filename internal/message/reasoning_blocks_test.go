package message

import (
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"github.com/stretchr/testify/require"
)

func TestLegacyReasoningPartsFixtureRemainsReadable(t *testing.T) {
	const fixture = `[{"type":"reasoning","data":{"thinking":"legacy thought","signature":"legacy-signature","thought_signature":"","tool_id":"","responses_data":null,"started_at":11,"finished_at":12}},{"type":"text","data":{"text":"answer"}}]`
	parts, err := unmarshalParts([]byte(fixture))
	require.NoError(t, err)
	m := Message{Role: Assistant, Parts: parts}
	require.Equal(t, ReasoningContent{Thinking: "legacy thought", Signature: "legacy-signature", StartedAt: 11, FinishedAt: 12}, m.ReasoningContent())
	wire := m.ToAIMessage()
	require.Len(t, wire, 1)
	require.Equal(t, []fantasy.MessagePart{
		fantasy.ReasoningPart{Text: "legacy thought", ProviderOptions: fantasy.ProviderOptions{anthropic.Name: &anthropic.ReasoningOptionMetadata{Signature: "legacy-signature"}}},
		fantasy.TextPart{Text: "answer"},
	}, wire[0].Content)
}

func TestLegacyReasoningFixtureWithoutTimestampsPreservesReplayFieldsAndOrder(t *testing.T) {
	// These literal rows predate reasoning timestamps. Replay must preserve the
	// fields and part arrival order that exist; timestamps cannot recover any
	// ordering information that an older row did not persist.
	const fixture = `[{"type":"reasoning","data":{"thinking":"legacy thought","signature":"legacy-signature","thought_signature":"","tool_id":"","responses_data":null}},{"type":"text","data":{"text":"answer"}},{"type":"reasoning","data":{"thinking":"second thought","signature":"","thought_signature":"legacy-google-signature","tool_id":"legacy-tool","responses_data":null}}]`
	parts, err := unmarshalParts([]byte(fixture))
	require.NoError(t, err)
	m := Message{Role: Assistant, Parts: parts}
	require.Equal(t, []ReasoningContent{
		{Thinking: "legacy thought", Signature: "legacy-signature"},
		{Thinking: "second thought", ThoughtSignature: "legacy-google-signature", ToolID: "legacy-tool"},
	}, m.ReasoningContents())

	wire := m.ToAIMessage()
	require.Len(t, wire, 1)
	require.Equal(t, []fantasy.MessagePart{
		fantasy.ReasoningPart{Text: "legacy thought", ProviderOptions: fantasy.ProviderOptions{anthropic.Name: &anthropic.ReasoningOptionMetadata{Signature: "legacy-signature"}}},
		fantasy.TextPart{Text: "answer"},
		fantasy.ReasoningPart{Text: "second thought", ProviderOptions: fantasy.ProviderOptions{google.Name: &google.ReasoningMetadata{Signature: "legacy-google-signature", ToolID: "legacy-tool"}}},
	}, wire[0].Content)
}

func TestReasoningBlocksRemainOrderedAcrossPersistenceAndReplay(t *testing.T) {
	m := Message{Role: Assistant, Parts: []ContentPart{}}
	m.StartReasoningContent("first")
	m.AppendReasoningSignature("sig-one")
	m.FinishThinking()
	m.AppendContent("visible")
	m.StartReasoningContent("second")
	m.AppendThoughtSignature("sig-two", "call-two")

	encoded, err := marshalParts(m.Parts)
	require.NoError(t, err)
	parts, err := unmarshalParts(encoded)
	require.NoError(t, err)
	m.Parts = parts

	reasoning := m.ReasoningContents()
	require.Len(t, reasoning, 2)
	require.Equal(t, "first", reasoning[0].Thinking)
	require.Equal(t, "sig-one", reasoning[0].Signature)
	require.NotZero(t, reasoning[0].FinishedAt)
	require.Equal(t, "second", reasoning[1].Thinking)
	require.Equal(t, "sig-two", reasoning[1].ThoughtSignature)
	require.Equal(t, "call-two", reasoning[1].ToolID)

	wire := m.ToAIMessage()
	require.Len(t, wire, 1)
	require.Equal(t, []fantasy.MessagePart{
		fantasy.ReasoningPart{Text: "first", ProviderOptions: fantasy.ProviderOptions{anthropic.Name: &anthropic.ReasoningOptionMetadata{Signature: "sig-one"}}},
		fantasy.TextPart{Text: "visible"},
		fantasy.ReasoningPart{Text: "second", ProviderOptions: fantasy.ProviderOptions{google.Name: &google.ReasoningMetadata{Signature: "sig-two", ToolID: "call-two"}}},
	}, wire[0].Content)
}

func TestReasoningMutatorsTargetLastUnfinishedBlockAcrossInterleaving(t *testing.T) {
	m := Message{Role: Assistant, Parts: []ContentPart{
		ReasoningContent{Thinking: "finished", StartedAt: 10, FinishedAt: 11},
		TextContent{Text: "visible"},
		ReasoningContent{Thinking: "unfinished", StartedAt: 12},
		ToolCall{ID: "call"},
	}}
	m.AppendReasoningContent(" continued")
	m.AppendReasoningSignature("signature")
	m.AppendThoughtSignature("thought-signature", "tool-id")
	data := &openai.ResponsesReasoningMetadata{ItemID: "item"}
	m.SetReasoningResponsesData(data)
	m.FinishThinking()

	reasoning := m.ReasoningContents()
	require.Len(t, reasoning, 2)
	require.Equal(t, "finished", reasoning[0].Thinking)
	require.Equal(t, "unfinished continued", reasoning[1].Thinking)
	require.Equal(t, "signature", reasoning[1].Signature)
	require.Equal(t, "thought-signature", reasoning[1].ThoughtSignature)
	require.Equal(t, "tool-id", reasoning[1].ToolID)
	require.Same(t, data, reasoning[1].ResponsesData)
	require.NotZero(t, reasoning[1].FinishedAt)
}

func TestThinkingDurationContinuesWhileLaterBlockUnfinished(t *testing.T) {
	now := time.Now().Unix()
	m := Message{Role: Assistant, Parts: []ContentPart{
		ReasoningContent{Thinking: "finished", StartedAt: now - 10, FinishedAt: now - 8},
		TextContent{Text: "interleaved"},
		ReasoningContent{Thinking: "still thinking", StartedAt: now - 7},
	}}

	require.GreaterOrEqual(t, m.ThinkingDuration(), 9*time.Second)
}

func TestThinkingDurationUsesFinishedEndWhenAllBlocksFinished(t *testing.T) {
	m := Message{Role: Assistant, Parts: []ContentPart{
		ReasoningContent{StartedAt: 10, FinishedAt: 12},
		TextContent{Text: "interleaved"},
		ReasoningContent{StartedAt: 13, FinishedAt: 15},
	}}
	require.Equal(t, 5*time.Second, m.ThinkingDuration())
}
