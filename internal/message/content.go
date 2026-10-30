package message

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/stringext"
)

type MessageRole string

const (
	Assistant MessageRole = "assistant"
	User      MessageRole = "user"
	System    MessageRole = "system"
	Tool      MessageRole = "tool"
)

// Origin marks the entry channel a session or an individual user message
// arrived through. Zero value (empty string) means unspecified: legacy
// rows, tests, and internally-generated sessions/messages (title
// generation, agent-tool sub-sessions) carry no origin.
type Origin string

const (
	OriginUnspecified Origin = ""
	OriginCLI         Origin = "cli"
	OriginWeb         Origin = "web"
	OriginSDK         Origin = "sdk"
)

// mediaLoadFailedPlaceholder is the text substituted for image data that
// cannot be decoded during session replay.
const mediaLoadFailedPlaceholder = "[Image data could not be loaded]"

type FinishReason string

const (
	FinishReasonEndTurn   FinishReason = "end_turn"
	FinishReasonMaxTokens FinishReason = "max_tokens"
	FinishReasonToolUse   FinishReason = "tool_use"
	FinishReasonCanceled  FinishReason = "canceled"
	FinishReasonError     FinishReason = "error"

	// Should never happen
	FinishReasonUnknown FinishReason = "unknown"
)

type ContentPart interface {
	isPart()
}

type ReasoningContent struct {
	Thinking         string                             `json:"thinking"`
	Signature        string                             `json:"signature"`
	RedactedData     string                             `json:"redacted_data,omitempty"`
	ThoughtSignature string                             `json:"thought_signature"` // Used for google
	ToolID           string                             `json:"tool_id"`           // Used for openrouter google models
	ResponsesData    *openai.ResponsesReasoningMetadata `json:"responses_data"`
	StartedAt        int64                              `json:"started_at,omitempty"`
	FinishedAt       int64                              `json:"finished_at,omitempty"`
}

func (tc ReasoningContent) String() string {
	return tc.Thinking
}
func (ReasoningContent) isPart() {}

type TextContent struct {
	Text string `json:"text"`
}

func (tc TextContent) String() string {
	return tc.Text
}

func (TextContent) isPart() {}

type ImageURLContent struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

func (iuc ImageURLContent) String() string {
	return iuc.URL
}

func (ImageURLContent) isPart() {}

type BinaryContent struct {
	Path     string
	MIMEType string
	Data     []byte
}

func (bc BinaryContent) String(p catwalk.InferenceProvider) string {
	base64Encoded := base64.StdEncoding.EncodeToString(bc.Data)
	if p == catwalk.InferenceProviderOpenAI {
		return "data:" + bc.MIMEType + ";base64," + base64Encoded
	}
	return base64Encoded
}

func (BinaryContent) isPart() {}

type ToolCall struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Input            string `json:"input"`
	ProviderExecuted bool   `json:"provider_executed"`
	Finished         bool   `json:"finished"`
}

func (ToolCall) isPart() {}

type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Content    string `json:"content"`
	Data       string `json:"data"`
	MIMEType   string `json:"mime_type"`
	Metadata   string `json:"metadata"`
	IsError    bool   `json:"is_error"`
}

func (ToolResult) isPart() {}

type Finish struct {
	Reason  FinishReason `json:"reason"`
	Time    int64        `json:"time"`
	Message string       `json:"message,omitempty"`
	Details string       `json:"details,omitempty"`
	// Partial is set to true when this Finish was written by the
	// auto-checkpoint ticker (batch 8). A process that dies mid-stream
	// leaves an unfinished row where Finish has Partial=true and
	// Reason="" — recovery code uses this to distinguish "mid-stream
	// checkpoint" from "properly finished turn". A proper Finish
	// overwrite removes this marker.
	// Fork patch: batch 8 — see CHANGELOG.fork.md section 6.
	Partial bool `json:"partial,omitempty"`
}

func (Finish) isPart() {}

type Message struct {
	ID               string
	Role             MessageRole
	SessionID        string
	Parts            []ContentPart
	Model            string
	Provider         string
	ReasoningEffort  string // "low", "medium", "high", or "max" - reasoning effort for Claude models
	CreatedAt        int64
	UpdatedAt        int64
	IsSummaryMessage bool
	Pinned           bool
	// Hidden messages are not rendered in the UI but are still passed to the
	// LLM as context (used for silent background summaries).
	Hidden bool
	// AutoResumed marks a user message created by Phase 4 autonomous
	// idle-resume; rendered as a badge in the web UI so operators can see
	// where the agent started a turn on its own.
	AutoResumed bool
	// BackgroundJobNotice marks a system-injected background-job-completion
	// notice so the web renders it as a notice, not a human message.
	BackgroundJobNotice bool
	// NoticeKind distinguishes the KIND of notice for messages the wake/async
	// job machinery persists (docs/plans/2026-09-27-wake-tools-contract.md
	// §5.2): "" for an ordinary finish/fail/cancel notice (today's
	// contract-undefined value, unchanged), "timeout_wake_only"/
	// "timeout_terminated" for the two timeout events phase 2 introduces,
	// "wake_failed" for the marker persisted when wakeSession's own Run
	// attempt fails after the notice was already saved (orchestrator
	// decision 2026-09-28). Other values are reserved for later phases.
	NoticeKind string
	// Origin marks the entry channel this message arrived through (see
	// the Origin type's doc). Zero value = unspecified.
	Origin Origin
	// CheckpointGeneration fences mid-stream checkpoint writes (P1-3 of the
	// 2026-08-18 release-readiness review). runTurn replaces its checkpoint
	// writer per step and stopCheckpoint waits only a bounded grace for the
	// old one, so a stale writer can still land a write after a newer one --
	// the in-memory generation check runs before the write and the write
	// happens after the session lock is released, which is the gap.
	//
	// Set ONLY by the checkpoint writer, on the partial snapshot it is about
	// to persist, and read only by the conditional update. Zero everywhere
	// else, which is correct: terminal writes take the unconditional path and
	// are already protected from later partials by finished_at.
	//
	// Not part of the message's content -- it is bookkeeping about which
	// writer produced this snapshot, not something the model or the UI sees.
	CheckpointGeneration int64
	// Usage is this message's own token accounting and prompt-cache
	// breakdown, or nil when none was recorded (every message written before
	// per-message tracking existed, and every non-assistant message).
	//
	// A POINTER on purpose: a zero-valued struct would be indistinguishable
	// from a turn measured at zero, and the UI must be able to say "not
	// measured" instead of rendering a confident 0% cache hit.
	Usage *TokenUsage
	// DeleteGeneration is the session's delete-generation counter value as
	// of this message's Delete/ForceDelete call (task #737, replacing the
	// task #731 rowid-based watermark below). Populated only where a caller
	// specifically asked for it (message.Service's Delete/ForceDelete);
	// zero everywhere else, including every row returned by the plain
	// Get/List/ListPaginated/etc. methods.
	//
	// This is the wire-level "delete watermark": DeletedEvent carries the
	// POST-INCREMENT generation value (message.service's deleteGen map,
	// bumped before the row is removed), and a messages_list snapshot reply
	// carries the session's CURRENT generation as of that read (see
	// message.Service.ListWithWatermark's doc comment for why that read
	// must happen before the List query, not after). The web client
	// compares the two to tell a snapshot that is PROVABLY older than a
	// delete it has already applied from one that merely looks that way
	// because of a reordered push -- see web/src/ws.ts's per-session delete
	// high-water mark and CLAUDE.md/docs for the resurrection risk this
	// closes.
	//
	// Task #731's original design used MAX(rowid) over a session's
	// surviving messages as the watermark. That is NOT a monotonic "highest
	// ever assigned" value: deleting a non-tail message (an older message
	// while a newer one survives) does not lower MAX(rowid), so the
	// watermark never moved for that class of delete and a stale
	// pre-delete snapshot could still compare as "fresh". The generation
	// counter increments on every delete regardless of which message was
	// removed, so it has no such blind spot.
	DeleteGeneration int64
}

func (m *Message) Content() TextContent {
	for _, part := range m.Parts {
		if c, ok := part.(TextContent); ok {
			return c
		}
	}
	return TextContent{}
}

func (m *Message) FullText() string {
	var parts []string
	for _, part := range m.Parts {
		if c, ok := part.(TextContent); ok {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func (m *Message) FullReasoningText() string {
	var texts []string
	for _, part := range m.Parts {
		if content, ok := part.(ReasoningContent); ok && content.Thinking != "" {
			texts = append(texts, content.Thinking)
		}
	}
	return strings.Join(texts, "\n\n")
}

func (m *Message) ReasoningContent() ReasoningContent {
	for _, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			return c
		}
	}
	return ReasoningContent{}
}

// ReasoningContents returns every reasoning block in part arrival order.
func (m *Message) ReasoningContents() []ReasoningContent {
	var contents []ReasoningContent
	for _, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			contents = append(contents, c)
		}
	}
	return contents
}

func (m *Message) ImageURLContent() []ImageURLContent {
	imageURLContents := make([]ImageURLContent, 0)
	for _, part := range m.Parts {
		if c, ok := part.(ImageURLContent); ok {
			imageURLContents = append(imageURLContents, c)
		}
	}
	return imageURLContents
}

func (m *Message) BinaryContent() []BinaryContent {
	binaryContents := make([]BinaryContent, 0)
	for _, part := range m.Parts {
		if c, ok := part.(BinaryContent); ok {
			binaryContents = append(binaryContents, c)
		}
	}
	return binaryContents
}

func (m *Message) ToolCalls() []ToolCall {
	toolCalls := make([]ToolCall, 0)
	for _, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			toolCalls = append(toolCalls, c)
		}
	}
	return toolCalls
}

func (m *Message) ToolResults() []ToolResult {
	toolResults := make([]ToolResult, 0)
	for _, part := range m.Parts {
		if c, ok := part.(ToolResult); ok {
			toolResults = append(toolResults, c)
		}
	}
	return toolResults
}

// Fork patch: batch 8 — IsPartial returns true when the message has a
// Finish part with Partial=true. This means the row was written by the
// mid-stream auto-checkpoint ticker, NOT by a proper step-end or error
// handler. Recovery code uses this to distinguish "process died mid-composition"
// from "properly finished turn". A partial Finish does NOT count as finished
// for IsFinished().
func (m *Message) IsPartial() bool {
	for _, part := range m.Parts {
		if f, ok := part.(Finish); ok && f.Partial {
			return true
		}
	}
	return false
}

func (m *Message) IsFinished() bool {
	for _, part := range m.Parts {
		if f, ok := part.(Finish); ok && !f.Partial {
			return true
		}
	}
	return false
}

func (m *Message) FinishPart() *Finish {
	for _, part := range m.Parts {
		if c, ok := part.(Finish); ok {
			return &c
		}
	}
	return nil
}

func (m *Message) FinishReason() FinishReason {
	for _, part := range m.Parts {
		if c, ok := part.(Finish); ok {
			return c.Reason
		}
	}
	return ""
}

func (m *Message) IsThinking() bool {
	return m.FullReasoningText() != "" && m.FullText() == "" && !m.IsFinished()
}

func (m *Message) AppendContent(delta string) {
	// Only append to the last part if it's a TextContent; otherwise create a new
	// TextContent. This ensures text that arrives after a tool_call/tool_result
	// gets its own block rather than being merged into the pre-tool text.
	if n := len(m.Parts); n > 0 {
		if c, ok := m.Parts[n-1].(TextContent); ok {
			m.Parts[n-1] = TextContent{Text: c.Text + delta}
			return
		}
	}
	m.Parts = append(m.Parts, TextContent{Text: delta})
}

// StartReasoningContent begins a distinct provider reasoning block.
func (m *Message) StartReasoningContent(text string) {
	m.Parts = append(m.Parts, ReasoningContent{Thinking: text, StartedAt: time.Now().Unix()})
}

func (m *Message) reasoningPartIndex() int {
	last := -1
	for i := len(m.Parts) - 1; i >= 0; i-- {
		if reasoning, ok := m.Parts[i].(ReasoningContent); ok {
			if last == -1 {
				last = i
			}
			if reasoning.FinishedAt == 0 {
				return i
			}
		}
	}
	return last
}

func (m *Message) AppendReasoningContent(delta string) {
	if i := m.reasoningPartIndex(); i >= 0 {
		reasoning := m.Parts[i].(ReasoningContent)
		reasoning.Thinking += delta
		m.Parts[i] = reasoning
		return
	}
	m.Parts = append(m.Parts, ReasoningContent{Thinking: delta, StartedAt: time.Now().Unix()})
}

// AppendThoughtSignature attaches provider metadata to the most recently
// received unfinished reasoning block, creating an empty block if metadata arrives first.
func (m *Message) AppendThoughtSignature(signature string, toolCallID string) {
	if i := m.reasoningPartIndex(); i >= 0 {
		reasoning := m.Parts[i].(ReasoningContent)
		reasoning.ThoughtSignature += signature
		reasoning.ToolID = toolCallID
		m.Parts[i] = reasoning
		return
	}
	m.Parts = append(m.Parts, ReasoningContent{ThoughtSignature: signature, ToolID: toolCallID})
}

func (m *Message) SetReasoningRedactedData(data string) {
	if i := m.reasoningPartIndex(); i >= 0 {
		reasoning := m.Parts[i].(ReasoningContent)
		reasoning.RedactedData = data
		m.Parts[i] = reasoning
		return
	}
	m.Parts = append(m.Parts, ReasoningContent{RedactedData: data})
}

func (m *Message) AppendReasoningSignature(signature string) {
	if i := m.reasoningPartIndex(); i >= 0 {
		reasoning := m.Parts[i].(ReasoningContent)
		reasoning.Signature += signature
		m.Parts[i] = reasoning
		return
	}
	m.Parts = append(m.Parts, ReasoningContent{Signature: signature})
}

func (m *Message) SetReasoningResponsesData(data *openai.ResponsesReasoningMetadata) {
	if i := m.reasoningPartIndex(); i >= 0 {
		reasoning := m.Parts[i].(ReasoningContent)
		reasoning.ResponsesData = data
		m.Parts[i] = reasoning
		return
	}
	m.Parts = append(m.Parts, ReasoningContent{ResponsesData: data})
}

func (m *Message) FinishThinking() {
	if i := m.reasoningPartIndex(); i >= 0 {
		reasoning := m.Parts[i].(ReasoningContent)
		if reasoning.FinishedAt == 0 {
			reasoning.FinishedAt = time.Now().Unix()
			m.Parts[i] = reasoning
		}
	}
}

func (m *Message) ThinkingDuration() time.Duration {
	var start, end int64
	unfinished := false
	for _, part := range m.Parts {
		if reasoning, ok := part.(ReasoningContent); ok {
			if start == 0 && reasoning.StartedAt != 0 {
				start = reasoning.StartedAt
			}
			if reasoning.FinishedAt == 0 {
				unfinished = true
			} else if reasoning.FinishedAt > end {
				end = reasoning.FinishedAt
			}
		}
	}
	if start == 0 {
		return 0
	}
	if unfinished || end == 0 {
		end = time.Now().Unix()
	}
	return time.Duration(end-start) * time.Second
}

func (m *Message) FinishToolCall(toolCallID string) {
	for i, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			if c.ID == toolCallID {
				m.Parts[i] = ToolCall{
					ID:               c.ID,
					Name:             c.Name,
					Input:            c.Input,
					ProviderExecuted: c.ProviderExecuted,
					Finished:         true,
				}
				return
			}
		}
	}
}

func (m *Message) AppendToolCallInput(toolCallID string, inputDelta string) {
	for i, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			if c.ID == toolCallID {
				m.Parts[i] = ToolCall{
					ID:       c.ID,
					Name:     c.Name,
					Input:    c.Input + inputDelta,
					Finished: c.Finished,
				}
				return
			}
		}
	}
}

func (m *Message) AddToolCall(tc ToolCall) {
	for i, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			if c.ID == tc.ID {
				m.Parts[i] = tc
				return
			}
		}
	}
	m.Parts = append(m.Parts, tc)
}

func (m *Message) SetToolCalls(tc []ToolCall) {
	// remove any existing tool call part it could have multiple
	parts := make([]ContentPart, 0)
	for _, part := range m.Parts {
		if _, ok := part.(ToolCall); ok {
			continue
		}
		parts = append(parts, part)
	}
	m.Parts = parts
	for _, toolCall := range tc {
		m.Parts = append(m.Parts, toolCall)
	}
}

func (m *Message) AddToolResult(tr ToolResult) {
	m.Parts = append(m.Parts, tr)
}

func (m *Message) SetToolResults(tr []ToolResult) {
	for _, toolResult := range tr {
		m.Parts = append(m.Parts, toolResult)
	}
}

// Clone returns a deep copy of the message with an independent Parts slice.
// This prevents race conditions when the message is modified concurrently.
func (m *Message) Clone() Message {
	clone := *m
	clone.Parts = make([]ContentPart, len(m.Parts))
	copy(clone.Parts, m.Parts)
	// Usage is a pointer, so the struct copy above would alias it. Nothing
	// mutates a TokenUsage today, but Clone exists precisely to keep
	// concurrent readers off shared state — leaving one aliased field would
	// make the next mutation a race that looks unrelated to this line.
	if m.Usage != nil {
		u := *m.Usage
		clone.Usage = &u
	}
	return clone
}

func (m *Message) AddFinish(reason FinishReason, message, details string) {
	// remove any existing finish part
	for i, part := range m.Parts {
		if _, ok := part.(Finish); ok {
			m.Parts = slices.Delete(m.Parts, i, i+1)
			break
		}
	}
	m.Parts = append(m.Parts, Finish{Reason: reason, Time: time.Now().Unix(), Message: message, Details: details})
}

func (m *Message) AddImageURL(url, detail string) {
	m.Parts = append(m.Parts, ImageURLContent{URL: url, Detail: detail})
}

func (m *Message) AddBinary(mimeType string, data []byte) {
	m.Parts = append(m.Parts, BinaryContent{MIMEType: mimeType, Data: data})
}

func PromptWithTextAttachments(prompt string, attachments []Attachment) string {
	var sb strings.Builder
	sb.WriteString(prompt)
	addedAttachments := false
	for _, content := range attachments {
		if !content.IsText() {
			continue
		}
		if !addedAttachments {
			sb.WriteString("\n<system_info>The files below have been attached by the user, consider them in your response</system_info>\n")
			addedAttachments = true
		}
		if content.FilePath != "" {
			fmt.Fprintf(&sb, "<file path='%s'>\n", content.FilePath)
		} else {
			sb.WriteString("<file>\n")
		}
		sb.WriteString("\n")
		sb.Write(content.Content)
		sb.WriteString("\n</file>\n")
	}
	return sb.String()
}

func (m *Message) ToAIMessage() []fantasy.Message {
	var messages []fantasy.Message
	switch m.Role {
	case User:
		var parts []fantasy.MessagePart
		text := strings.TrimSpace(m.Content().Text)
		var textAttachments []Attachment
		for _, content := range m.BinaryContent() {
			if !strings.HasPrefix(content.MIMEType, "text/") {
				continue
			}
			textAttachments = append(textAttachments, Attachment{
				FilePath: content.Path,
				MimeType: content.MIMEType,
				Content:  content.Data,
			})
		}
		text = PromptWithTextAttachments(text, textAttachments)
		if text != "" {
			parts = append(parts, fantasy.TextPart{Text: text})
		}
		for _, content := range m.BinaryContent() {
			// skip text attachements
			if strings.HasPrefix(content.MIMEType, "text/") {
				continue
			}
			parts = append(parts, fantasy.FilePart{
				Filename:  content.Path,
				Data:      content.Data,
				MediaType: content.MIMEType,
			})
		}
		messages = append(messages, fantasy.Message{
			Role:    fantasy.MessageRoleUser,
			Content: parts,
		})
	case Assistant:
		var parts []fantasy.MessagePart
		for _, part := range m.Parts {
			switch content := part.(type) {
			case ReasoningContent:
				// OpenRouter replays encrypted Responses data; direct Responses skips reasoning parts.
				if content.Thinking == "" && content.Signature == "" && content.RedactedData == "" && content.ThoughtSignature == "" && (content.ResponsesData == nil || content.ResponsesData.EncryptedContent == nil || *content.ResponsesData.EncryptedContent == "") {
					continue
				}
				reasoningPart := fantasy.ReasoningPart{Text: content.Thinking, ProviderOptions: fantasy.ProviderOptions{}}
				if content.Signature != "" {
					reasoningPart.ProviderOptions[anthropic.Name] = &anthropic.ReasoningOptionMetadata{Signature: content.Signature}
				} else if content.RedactedData != "" {
					reasoningPart.ProviderOptions[anthropic.Name] = &anthropic.ReasoningOptionMetadata{RedactedData: content.RedactedData}
				}
				if content.ResponsesData != nil {
					reasoningPart.ProviderOptions[openai.Name] = content.ResponsesData
				}
				if content.ThoughtSignature != "" {
					reasoningPart.ProviderOptions[google.Name] = &google.ReasoningMetadata{Signature: content.ThoughtSignature, ToolID: content.ToolID}
				}
				parts = append(parts, reasoningPart)
			case TextContent:
				if text := strings.TrimSpace(content.Text); text != "" {
					parts = append(parts, fantasy.TextPart{Text: text})
				}
			case ToolCall:
				parts = append(parts, fantasy.ToolCallPart{
					ToolCallID:       content.ID,
					ToolName:         content.Name,
					Input:            content.Input,
					ProviderExecuted: content.ProviderExecuted,
				})
			}
		}
		messages = append(messages, fantasy.Message{
			Role:    fantasy.MessageRoleAssistant,
			Content: parts,
		})
	case Tool:
		var parts []fantasy.MessagePart
		for _, result := range m.ToolResults() {
			var content fantasy.ToolResultOutputContent
			if result.IsError {
				content = fantasy.ToolResultOutputContentError{
					Error: errors.New(result.Content),
				}
			} else if result.Data != "" {
				if stringext.IsValidBase64(result.Data) {
					content = fantasy.ToolResultOutputContentMedia{
						Data:      result.Data,
						MediaType: result.MIMEType,
					}
				} else {
					content = fantasy.ToolResultOutputContentText{
						Text: mediaLoadFailedPlaceholder,
					}
				}
			} else {
				content = fantasy.ToolResultOutputContentText{
					Text: result.Content,
				}
			}
			parts = append(parts, fantasy.ToolResultPart{
				ToolCallID: result.ToolCallID,
				Output:     content,
			})
		}
		messages = append(messages, fantasy.Message{
			Role:    fantasy.MessageRoleTool,
			Content: parts,
		})
	}
	return messages
}
