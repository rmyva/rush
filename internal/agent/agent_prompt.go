// Message shaping around the provider stream: history/prompt assembly
// (preparePrompt and its orphaned-tool-call filters), user-message creation,
// cache-control options and session headers, tool-result conversion and
// provider media workarounds, context-window trimming, and input sanitisation.
package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/stringext"
)

func (a *sessionAgent) getCacheControlOptions() fantasy.ProviderOptions {
	if t, _ := strconv.ParseBool(os.Getenv("RUSH_DISABLE_ANTHROPIC_CACHE")); t {
		return fantasy.ProviderOptions{}
	}
	opts := fantasy.ProviderOptions{}
	for provider, profile := range cacheProfiles {
		if !profile.explicitMarkers {
			continue
		}
		opts[provider] = &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		}
	}
	return opts
}

// sessionHeaders returns the HTTP headers we use for cache affinity on
// every LLM request for a given session.
//
// We use the session hash instead of the raw UUID so the header value
// is deterministic and opaque.
func sessionHeaders(sessionID string) map[string]string {
	hash := session.HashID(sessionID)
	return map[string]string{
		"x-session-id":       hash,
		"x-session-affinity": hash,
	}
}

// autoResumedCtxKey tags a completion turn so its call retains the flag
// across mailbox and durable-queue handoffs.
type autoResumedCtxKey struct{}

// backgroundJobNoticeCtxKey tags a completion turn so its call retains
// the notice flag across mailbox and durable-queue handoffs.
type backgroundJobNoticeCtxKey struct{}

// WithBackgroundJobNotice marks a completion turn as an agent-visible notice.
func WithBackgroundJobNotice(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, autoResumedCtxKey{}, true)
	return context.WithValue(ctx, backgroundJobNoticeCtxKey{}, true)
}

func noticeFlagsFrom(ctx context.Context) (autoResumed, backgroundJobNotice bool) {
	autoResumed, _ = ctx.Value(autoResumedCtxKey{}).(bool)
	backgroundJobNotice, _ = ctx.Value(backgroundJobNoticeCtxKey{}).(bool)
	return
}

// drainCallCtxKey tags a turn built through the prompt-string entry points
// (coordinator.Run/buildCall) as a Drain call (phase-4 step 3, doc
// sec.3.4), for callers that cannot build a SessionAgentCall{IsDrain: true}
// literal directly -- today, only the CLI root loop's wake iteration
// (internal/app/app_run_async.go), which drives AgentCoordinator.Run with a
// prompt string through ExecuteRun rather than calling a SessionAgent
// directly the way wakeSession/drainCallFor do. Mirrors WithBackgroundJobNotice's
// pattern exactly: set here, read by buildCall/runInternal, carried on the
// resulting SessionAgentCall from then on like any other field.
type drainCallCtxKey struct{}

// WithDrainCall marks the next prompt-string-driven turn as a Drain call: an
// empty prompt (the caller MUST pass "" -- this alone does not blank an
// existing prompt), no user message persisted, reacting only to whatever
// the turn-start pull moves into history.
func WithDrainCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, drainCallCtxKey{}, true)
}

func isDrainCallFrom(ctx context.Context) bool {
	v, _ := ctx.Value(drainCallCtxKey{}).(bool)
	return v
}

// callOriginCtxKey tags a context with the entry-channel origin
// (message.OriginCLI/Web/SDK) of the request that started the turn.
// Set by ExecuteRun (internal/app) and the web server's turn handlers;
// buildCall stamps it onto the SessionAgentCall so the origin is
// persisted on the user message createUserMessage creates.
type callOriginCtxKey struct{}

// WithCallOrigin returns ctx tagged with the entry-channel origin of
// the request. Zero/unspecified origin preserves the historical
// behaviour (no origin recorded).
func WithCallOrigin(ctx context.Context, origin message.Origin) context.Context {
	return context.WithValue(ctx, callOriginCtxKey{}, origin)
}

// CallOriginFrom reads the entry-channel origin tagged by WithCallOrigin
// (message.OriginUnspecified when absent).
func CallOriginFrom(ctx context.Context) message.Origin {
	if ctx == nil {
		return message.OriginUnspecified
	}
	origin, _ := ctx.Value(callOriginCtxKey{}).(message.Origin)
	return origin
}

func (a *sessionAgent) createUserMessage(ctx context.Context, call SessionAgentCall) (message.Message, error) {
	parts := []message.ContentPart{message.TextContent{Text: call.Prompt}}
	var attachmentParts []message.ContentPart
	for _, attachment := range call.Attachments {
		attachmentParts = append(attachmentParts, message.BinaryContent{Path: attachment.FilePath, MIMEType: attachment.MimeType, Data: attachment.Content})
	}
	parts = append(parts, attachmentParts...)
	msg, err := a.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
		Role:                message.User,
		Parts:               parts,
		AutoResumed:         call.AutoResumed,
		BackgroundJobNotice: call.BackgroundJobNotice,
		NoticeKind:          call.NoticeKind,
		Origin:              call.Origin,
	})
	if err != nil {
		return message.Message{}, fmt.Errorf("failed to create user message: %w", err)
	}
	return msg, nil
}

func (a *sessionAgent) preparePrompt(msgs []message.Message, todos []session.Todo, attachments ...message.Attachment) ([]fantasy.Message, []fantasy.FilePart) {
	var history []fantasy.Message
	var reminderMsg fantasy.Message
	hasReminder := false
	if !a.isSubAgent {
		// Fork merge note: we already extended this block to also surface the
		// CURRENT todo list when non-empty (originally only handled empty).
		// Upstream's small reword to the empty-case text is not worth the churn.
		var reminderText string
		if len(todos) == 0 {
			reminderText = `This is a reminder that your todo list is currently empty — all previous tasks have been completed or deleted. DO NOT recreate any old tasks from memory. DO NOT mention this to the user explicitly because they are already aware.
If you are working on tasks that would benefit from a todo list please use the "todos" tool to create one.
If not, please feel free to ignore. Again do not mention this message to the user.`
		} else {
			var sb strings.Builder
			sb.WriteString("This is a reminder of your CURRENT todo list. This is the authoritative ground truth — it overrides anything in your conversation history:\n\n")
			for _, t := range todos {
				fmt.Fprintf(&sb, "- [%s] %s\n", t.Status, t.Content)
			}
			sb.WriteString("\nIMPORTANT: Tasks NOT in this list have been DELETED (by the user or by you). Do NOT add them back. Only manage the tasks listed above, plus any new ones the user explicitly requests. DO NOT mention this reminder to the user.")
			reminderText = sb.String()
		}
		reminderMsg = fantasy.NewUserMessage(
			fmt.Sprintf("<system_reminder>%s</system_reminder>", reminderText),
		)
		hasReminder = true
	}
	// A user message created while a tool ran must not split tool_use from
	// its tool_result (#1066).
	msgs = deferUserMessagesPastToolRound(msgs)
	// Collect all tool call IDs present in assistant messages and all tool
	// result IDs present in tool messages. This lets us detect both orphaned
	// tool results (result without a call) and orphaned tool calls (call
	// without a result).
	knownToolCallIDs := make(map[string]struct{})
	knownToolResultIDs := make(map[string]struct{})
	for _, m := range msgs {
		switch m.Role {
		case message.Assistant:
			for _, tc := range m.ToolCalls() {
				knownToolCallIDs[tc.ID] = struct{}{}
			}
		case message.Tool:
			for _, tr := range m.ToolResults() {
				knownToolResultIDs[tr.ToolCallID] = struct{}{}
			}
		}
	}

	for _, m := range msgs {
		if len(m.Parts) == 0 {
			continue
		}
		// Assistant message without content or tool calls (cancelled before it returned anything).
		if m.Role == message.Assistant && len(m.ToolCalls()) == 0 && m.Content().Text == "" && m.FullReasoningText() == "" {
			continue
		}
		if m.Role == message.Tool {
			if msg, ok := filterOrphanedToolResults(m, knownToolCallIDs); ok {
				history = append(history, msg)
			}
			continue
		}
		aiMsgs := m.ToAIMessage()
		// Fork merge note (origin/main 6d95ecc5 "skip image attachments in
		// history when model doesn't support them"): we intentionally skip
		// upstream's per-message filter here — the same scrub happens in
		// workaroundProviderMediaLimitations() which runs once per Stream
		// call inside PrepareStep, so doing it twice would just walk the
		// history twice.
		history = append(history, aiMsgs...)

		if m.Role == message.Assistant {
			if msg, ok := syntheticToolResultsForOrphanedCalls(m, knownToolResultIDs); ok {
				history = append(history, msg)
			}
		}
	}

	// Reminder goes last, not first: keeping it at the tail preserves the
	// longest-common-prefix cache match across turns (PrepareStep caches the
	// last 2 messages; a volatile message near the front busts the whole
	// history's cache on every todo mutation).
	if hasReminder {
		history = append(history, reminderMsg)
	}

	var files []fantasy.FilePart
	for _, attachment := range attachments {
		if attachment.IsText() {
			continue
		}
		files = append(files, fantasy.FilePart{
			Filename:  attachment.FileName,
			Data:      attachment.Content,
			MediaType: attachment.MimeType,
		})
	}

	return history, files
}

// filterFileParts removes fantasy.FilePart entries from a slice of message
// parts. Used to strip image attachments from historical user messages when
// the current model does not support them.
func filterFileParts(parts []fantasy.MessagePart) []fantasy.MessagePart {
	filtered := make([]fantasy.MessagePart, 0, len(parts))
	for _, part := range parts {
		if _, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
			continue
		}
		filtered = append(filtered, part)
	}
	return filtered
}

// filterOrphanedToolResults converts a tool message to a fantasy.Message,
// dropping any tool result parts whose tool_call_id has no matching tool call
// in the known set. An orphaned result causes API validation to fail on every
// subsequent turn, permanently locking the session. Returns the filtered
// message and true if at least one valid part remains.
func filterOrphanedToolResults(m message.Message, knownToolCallIDs map[string]struct{}) (fantasy.Message, bool) {
	aiMsgs := m.ToAIMessage()
	if len(aiMsgs) == 0 {
		return fantasy.Message{}, false
	}
	var validParts []fantasy.MessagePart
	for _, part := range aiMsgs[0].Content {
		tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
		if !ok {
			validParts = append(validParts, part)
			continue
		}
		if _, known := knownToolCallIDs[tr.ToolCallID]; known {
			validParts = append(validParts, part)
		} else {
			slog.Warn(
				"Dropping orphaned tool result with no matching tool call",
				"tool_call_id", tr.ToolCallID,
			)
		}
	}
	if len(validParts) == 0 {
		return fantasy.Message{}, false
	}
	msg := aiMsgs[0]
	msg.Content = validParts
	return msg, true
}

// syntheticToolResultsForOrphanedCalls returns a tool message containing
// synthetic tool results for any tool calls in the assistant message that
// have no matching result in knownToolResultIDs. LLM APIs require every
// tool_use to be immediately followed by a tool_result; an interrupted
// session can leave orphaned tool_use blocks that permanently lock the
// conversation. Returns the message and true if any synthetic results were
// produced.
func syntheticToolResultsForOrphanedCalls(m message.Message, knownToolResultIDs map[string]struct{}) (fantasy.Message, bool) {
	var syntheticParts []fantasy.MessagePart
	for _, tc := range m.ToolCalls() {
		if _, hasResult := knownToolResultIDs[tc.ID]; hasResult {
			continue
		}
		slog.Warn(
			"Injecting synthetic tool result for orphaned tool call",
			"tool_call_id", tc.ID,
			"tool_name", tc.Name,
		)
		syntheticParts = append(syntheticParts, fantasy.ToolResultPart{
			ToolCallID: tc.ID,
			Output: fantasy.ToolResultOutputContentError{
				Error: errors.New("tool call was interrupted and did not produce a result, you may retry this call if the result is still needed"),
			},
		})
	}
	if len(syntheticParts) == 0 {
		return fantasy.Message{}, false
	}
	return fantasy.Message{
		Role:    fantasy.MessageRoleTool,
		Content: syntheticParts,
	}, true
}

func (a *sessionAgent) getSessionMessages(ctx context.Context, session session.Session) ([]message.Message, error) {
	msgs, err := a.messages.List(ctx, session.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to list messages: %w", err)
	}

	if session.SummaryMessageID != "" {
		summaryMsgIndex := -1
		for i, msg := range msgs {
			if msg.ID == session.SummaryMessageID {
				summaryMsgIndex = i
				break
			}
		}
		if summaryMsgIndex != -1 {
			// Collect pinned messages that appear before the summary
			var pinned []message.Message
			for _, msg := range msgs[:summaryMsgIndex] {
				if msg.Pinned {
					pinned = append(pinned, msg)
				}
			}
			msgs = msgs[summaryMsgIndex:]
			msgs[0].Role = message.User
			if len(pinned) > 0 {
				msgs = append(pinned, msgs...)
			}
		}
	}
	return dropSupersededSupervisionNotices(msgs), nil
}

// dropSupersededSupervisionNotices keeps only the newest
// NoticeKind==noticeKindSupervision message in full (design doc §7: only the
// latest summary matters). Older ones stay in place with their text replaced
// by a short marker instead of being removed: removing a user-role message
// can leave two assistant messages adjacent, which some providers reject.
func dropSupersededSupervisionNotices(msgs []message.Message) []message.Message {
	lastIdx := -1
	for i, m := range msgs {
		if m.NoticeKind == noticeKindSupervision {
			lastIdx = i
		}
	}
	if lastIdx == -1 {
		return msgs
	}
	out := make([]message.Message, len(msgs))
	copy(out, msgs)
	for i, m := range out {
		if m.NoticeKind == noticeKindSupervision && i != lastIdx {
			m.Parts = []message.ContentPart{message.TextContent{Text: supersededSupervisionText}}
			out[i] = m
		}
	}
	return out
}

// supersededSupervisionText replaces an older supervision summary in the
// model's context.
const supersededSupervisionText = "[Earlier supervision check-in; superseded by a newer one.]"

// convertToToolResult converts a fantasy tool result to a message tool result.
func (a *sessionAgent) convertToToolResult(result fantasy.ToolResultContent) message.ToolResult {
	baseResult := message.ToolResult{
		ToolCallID: result.ToolCallID,
		Name:       result.ToolName,
		Metadata:   result.ClientMetadata,
	}

	switch result.Result.GetType() {
	case fantasy.ToolResultContentTypeText:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result.Result); ok {
			baseResult.Content = r.Text
		}
	case fantasy.ToolResultContentTypeError:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result.Result); ok {
			baseResult.Content = r.Error.Error()
			baseResult.IsError = true
		}
	case fantasy.ToolResultContentTypeMedia:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](result.Result); ok {
			if !stringext.IsValidBase64(r.Data) {
				slog.Warn(
					"Tool returned media with invalid base64 data, discarding image",
					"tool", result.ToolName,
					"tool_call_id", result.ToolCallID,
				)
				baseResult.Content = "Tool returned image data with invalid encoding"
				baseResult.IsError = true
			} else {
				content := r.Text
				if content == "" {
					content = fmt.Sprintf("Loaded %s content", r.MediaType)
				}
				baseResult.Content = content
				baseResult.Data = r.Data
				baseResult.MIMEType = r.MediaType
			}
		}
	}

	return baseResult
}

// workaroundProviderMediaLimitations converts media content in tool results to
// user messages for providers that don't natively support images in tool results.
//
// Problem: OpenAI, Google, OpenRouter, and other OpenAI-compatible providers
// don't support sending images/media in tool result messages - they only accept
// text in tool results. However, they DO support images in user messages.
//
// If we send media in tool results to these providers, the API returns an error.
//
// Solution: For these providers, we:
//  1. Replace the media in the tool result with a text placeholder
//  2. Inject a user message immediately after with the image as a file attachment
//  3. This maintains the tool execution flow while working around API limitations
//
// Anthropic and Bedrock support images natively in tool results, so we skip
// this workaround for them.
//
// Example transformation:
//
//	BEFORE: [tool result: image data]
//	AFTER:  [tool result: "Image loaded - see attached"], [user: image attachment]
func (a *sessionAgent) workaroundProviderMediaLimitations(messages []fantasy.Message, smartModel Model) []fantasy.Message {
	providerSupportsMedia := smartModel.ModelCfg.Provider == string(catwalk.InferenceProviderAnthropic) ||
		smartModel.ModelCfg.Provider == string(catwalk.InferenceProviderBedrock)

	if providerSupportsMedia {
		return messages
	}

	supportsImages := smartModel.CatwalkCfg.SupportsImages

	convertedMessages := make([]fantasy.Message, 0, len(messages))

	for _, msg := range messages {
		if msg.Role != fantasy.MessageRoleTool {
			convertedMessages = append(convertedMessages, msg)
			continue
		}

		textParts := make([]fantasy.MessagePart, 0, len(msg.Content))
		var mediaFiles []fantasy.FilePart

		for _, part := range msg.Content {
			toolResult, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				textParts = append(textParts, part)
				continue
			}

			if media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](toolResult.Output); ok {
				if !supportsImages {
					// Model cannot process images. Replace with a text
					// placeholder and skip creating a synthetic user
					// message with FilePart, which would brick the
					// session on text-only models.
					textParts = append(textParts, fantasy.ToolResultPart{
						ToolCallID: toolResult.ToolCallID,
						Output: fantasy.ToolResultOutputContentText{
							Text: "[Image/media content not supported by this model]",
						},
						ProviderOptions: toolResult.ProviderOptions,
					})
					continue
				}

				decoded, err := base64.StdEncoding.DecodeString(media.Data)
				if err != nil {
					slog.Warn("Failed to decode media data", "error", err)
					textParts = append(textParts, part)
					continue
				}

				mediaFiles = append(mediaFiles, fantasy.FilePart{
					Data:      decoded,
					MediaType: media.MediaType,
					Filename:  fmt.Sprintf("tool-result-%s", toolResult.ToolCallID),
				})

				textParts = append(textParts, fantasy.ToolResultPart{
					ToolCallID: toolResult.ToolCallID,
					Output: fantasy.ToolResultOutputContentText{
						Text: "[Image/media content loaded - see attached file]",
					},
					ProviderOptions: toolResult.ProviderOptions,
				})
			} else {
				textParts = append(textParts, part)
			}
		}

		convertedMessages = append(convertedMessages, fantasy.Message{
			Role:    fantasy.MessageRoleTool,
			Content: textParts,
		})

		if len(mediaFiles) > 0 {
			convertedMessages = append(convertedMessages, fantasy.NewUserMessage(
				"Here is the media content from the tool result:",
				mediaFiles...,
			))
		}
	}

	return convertedMessages
}

// trimMessagesToWindow returns a suffix of msgs whose estimated token count
// fits within targetTokens (1 token ≈ 4 characters).  It always starts on a
// user-role message so the conversation stays well-formed.
func trimMessagesToWindow(msgs []fantasy.Message, targetTokens int64) []fantasy.Message {
	if len(msgs) == 0 || targetTokens <= 0 {
		return msgs
	}
	const charsPerToken = 4
	budget := int(targetTokens) * charsPerToken

	var accumulated int
	cutIdx := 0 // by default keep everything
	for i := len(msgs) - 1; i >= 0; i-- {
		accumulated += estimateMsgChars(msgs[i])
		if accumulated >= budget {
			cutIdx = i + 1
			break
		}
	}
	if cutIdx == 0 {
		return msgs // all messages fit
	}
	// Advance to the next user-role message to keep the history well-formed.
	for cutIdx < len(msgs) && msgs[cutIdx].Role != fantasy.MessageRoleUser {
		cutIdx++
	}
	if cutIdx >= len(msgs) {
		return msgs // can't trim without losing all context
	}
	return msgs[cutIdx:]
}

// estimateMsgChars returns a rough character count for a fantasy.Message,
// used to estimate its token footprint for window trimming.
func estimateMsgChars(msg fantasy.Message) int {
	total := 0
	for _, part := range msg.Content {
		switch p := part.(type) {
		case fantasy.TextPart:
			total += len(p.Text)
		case fantasy.ToolCallPart:
			total += len(p.ToolName) + len(p.Input)
		case fantasy.ToolResultPart:
			switch o := p.Output.(type) {
			case fantasy.ToolResultOutputContentText:
				total += len(o.Text)
			case fantasy.ToolResultOutputContentError:
				total += len(fmt.Sprintf("%v", o.Error))
			}
		}
	}
	if total == 0 {
		total = 64 // minimum for empty / binary messages
	}
	return total
}

func providerRetryLogFields(err *fantasy.ProviderError, delay time.Duration) []any {
	fields := []any{
		"retry_delay", delay.String(),
	}
	if err == nil {
		return fields
	}
	fields = append(fields, "status_code", err.StatusCode)
	if err.Title != "" {
		fields = append(fields, "title", err.Title)
	}
	if err.Message != "" {
		fields = append(fields, "message", err.Message)
	}
	return fields
}

// sanitizeToolInput validates tool call JSON from the provider.
// Malformed input — and a bare null payload, which is valid JSON but
// carries no arguments — is replaced with an empty object to prevent
// stuck conversations from truncated or malformed model output.
// The second return value indicates whether sanitization occurred.
func sanitizeToolInput(toolName, toolCallID, input string) (string, bool) {
	if !json.Valid([]byte(input)) {
		slog.Warn("Malformed tool call JSON from provider, replacing with empty object",
			"tool", toolName,
			"id", toolCallID,
			"input_len", len(input),
		)
		return "{}", true
	}
	// A bare null payload is valid JSON but carries no arguments; every
	// consumer treats it the same as an empty object, so normalize it at
	// the source instead of letting a null literal reach the transcript,
	// where non-defensive readers (the web formatter) dereference it.
	if trimmed := strings.TrimSpace(input); trimmed == "null" {
		slog.Warn("Null tool call JSON from provider, replacing with empty object",
			"tool", toolName,
			"id", toolCallID,
		)
		return "{}", true
	}
	return input, false
}
