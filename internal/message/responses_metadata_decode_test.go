package message

import (
	"encoding/json"
	"testing"

	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestResponsesReasoningMetadataPersistenceFixtures(t *testing.T) {
	const wrapped = `[{"type":"reasoning","data":{"thinking":"private","responses_data":{"type":"openai.responses.reasoning_metadata","data":{"item_id":"rs_1","encrypted_content":"cipher","summary":["brief"]}}}}]`
	const flat = `[{"type":"reasoning","data":{"thinking":"private","responses_data":{"item_id":"rs_1","encrypted_content":"cipher","summary":["brief"]}}}]`
	const nullMetadata = `[{"type":"reasoning","data":{"thinking":"private","responses_data":null}}]`
	for _, tc := range []struct {
		name string
		data string
		want bool
	}{
		{name: "wrapped", data: wrapped, want: true},
		{name: "flat", data: flat, want: true},
		{name: "null", data: nullMetadata},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts, err := unmarshalParts([]byte(tc.data))
			require.NoError(t, err)
			require.Len(t, parts, 1)
			reasoning, ok := parts[0].(ReasoningContent)
			require.True(t, ok)
			require.Equal(t, "private", reasoning.Thinking)
			if !tc.want {
				require.Nil(t, reasoning.ResponsesData)
				return
			}
			require.Equal(t, &openai.ResponsesReasoningMetadata{
				ItemID: "rs_1", EncryptedContent: stringPointer("cipher"), Summary: []string{"brief"},
			}, reasoning.ResponsesData)
			encoded, err := marshalParts(parts)
			require.NoError(t, err)
			var roundTrip []json.RawMessage
			require.NoError(t, json.Unmarshal(encoded, &roundTrip))
			var part map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(roundTrip[0], &part))
			var data map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(part["data"], &data))
			var persisted json.RawMessage
			require.NoError(t, json.Unmarshal(data["responses_data"], &persisted))
			require.JSONEq(t, `{"type":"openai.responses.reasoning_metadata","data":{"item_id":"rs_1","encrypted_content":"cipher","summary":["brief"]}}`, string(persisted))
		})
	}
}

func TestResponsesReasoningMetadataConstructedPartsRoundTrip(t *testing.T) {
	original := []ContentPart{ReasoningContent{
		Thinking: "private",
		ResponsesData: &openai.ResponsesReasoningMetadata{
			ItemID: "rs_constructed", EncryptedContent: stringPointer("ciphertext"), Summary: []string{"summary"},
		},
	}}
	encoded, err := marshalParts(original)
	require.NoError(t, err)
	decoded, err := unmarshalParts(encoded)
	require.NoError(t, err)
	require.Len(t, decoded, 1)
	reasoning, ok := decoded[0].(ReasoningContent)
	require.True(t, ok)
	require.NotNil(t, reasoning.ResponsesData)
	require.Equal(t, "private", reasoning.Thinking)
	require.Equal(t, "rs_constructed", reasoning.ResponsesData.ItemID)
	require.NotNil(t, reasoning.ResponsesData.EncryptedContent)
	require.Equal(t, "ciphertext", *reasoning.ResponsesData.EncryptedContent)
	require.Equal(t, []string{"summary"}, reasoning.ResponsesData.Summary)
}

func TestResponsesReasoningMetadataPartsMarshalBytes(t *testing.T) {
	parts := []ContentPart{ReasoningContent{
		Thinking: "private",
		ResponsesData: &openai.ResponsesReasoningMetadata{
			ItemID: "rs_1", EncryptedContent: stringPointer("cipher"), Summary: []string{"brief"},
		},
	}}
	encoded, err := marshalParts(parts)
	require.NoError(t, err)
	require.Equal(t, `[{"type":"reasoning","data":{"thinking":"private","signature":"","thought_signature":"","tool_id":"","responses_data":{"type":"openai.responses.reasoning_metadata","data":{"item_id":"rs_1","encrypted_content":"cipher","summary":["brief"]}}}}]`, string(encoded))
}

func stringPointer(s string) *string { return &s }

func TestResponsesReasoningMetadataMessageJSONPersistence(t *testing.T) {
	const fixture = `[{"type":"reasoning","data":{"thinking":"private","responses_data":{"type":"openai.responses.reasoning_metadata","data":{"item_id":"rs_1","encrypted_content":"cipher","summary":["brief"]}}}}]`
	service := &service{}
	m, err := service.fromDBItem(db.Message{Parts: string(fixture)})
	require.NoError(t, err)
	require.Len(t, m.Parts, 1)
	reasoning, ok := m.Parts[0].(ReasoningContent)
	require.True(t, ok)
	require.NotNil(t, reasoning.ResponsesData)
	require.Equal(t, "rs_1", reasoning.ResponsesData.ItemID)
	require.NotNil(t, reasoning.ResponsesData.EncryptedContent)
	require.Equal(t, "cipher", *reasoning.ResponsesData.EncryptedContent)
	require.Equal(t, []string{"brief"}, reasoning.ResponsesData.Summary)
}
