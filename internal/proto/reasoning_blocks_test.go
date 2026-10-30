package proto

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReasoningMutatorsTargetLastUnfinishedBlockAcrossInterleaving(t *testing.T) {
	m := Message{Role: Assistant, Parts: []ContentPart{
		ReasoningContent{Thinking: "finished", StartedAt: 10, FinishedAt: 11},
		TextContent{Text: "visible"},
		ReasoningContent{Thinking: "unfinished", StartedAt: 12},
		ToolCall{ID: "call"},
	}}
	m.AppendReasoningContent(" continued")
	m.AppendReasoningSignature(" signature")
	m.FinishThinking()

	reasoning := m.ReasoningContents()
	require.Len(t, reasoning, 2)
	require.Equal(t, "finished", reasoning[0].Thinking)
	require.Equal(t, "unfinished continued", reasoning[1].Thinking)
	require.Equal(t, " signature", reasoning[1].Signature)
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

func TestIsThinkingChecksAllTextBlocks(t *testing.T) {
	m := Message{Role: Assistant, Parts: []ContentPart{
		ReasoningContent{Thinking: "reasoning"},
		TextContent{Text: "first"},
		TextContent{Text: "second"},
	}}

	require.Equal(t, "first\n\nsecond", m.FullText())
	require.False(t, m.IsThinking())
}

func TestIsThinkingWithoutTextBlocks(t *testing.T) {
	m := Message{Role: Assistant, Parts: []ContentPart{
		ReasoningContent{Thinking: "first reasoning"},
		ReasoningContent{Thinking: "second reasoning"},
	}}

	require.True(t, m.IsThinking())
}

func TestThinkingDurationUsesFinishedEndWhenAllBlocksFinished(t *testing.T) {
	m := Message{Role: Assistant, Parts: []ContentPart{
		ReasoningContent{StartedAt: 10, FinishedAt: 12},
		TextContent{Text: "interleaved"},
		ReasoningContent{StartedAt: 13, FinishedAt: 15},
	}}
	require.Equal(t, 5*time.Second, m.ThinkingDuration())
}
