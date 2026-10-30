package message

import (
	"bytes"
	"encoding/json"

	"charm.land/fantasy/providers/openai"
)

// decodeResponsesReasoningMetadata accepts the legacy flat representation and
// fantasy's provider-type wrapper without changing its normal JSON encoding.
func decodeResponsesReasoningMetadata(data json.RawMessage) (*openai.ResponsesReasoningMetadata, error) {
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil, nil
	}
	var wrapped struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}
	if wrapped.Type != "" && len(wrapped.Data) != 0 {
		data = wrapped.Data
	}
	var metadata openai.ResponsesReasoningMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}
	return &metadata, nil
}
