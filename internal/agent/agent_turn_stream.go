package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// compositionPhase replaces runTurn's original bare bool sawToolBoundary.
// The two states are exactly the two the bool encoded; this only makes the
// callback code that flips it self-documenting at each of its five call
// sites (OnTextDelta, OnToolInputStart, OnToolCall, OnToolResult,
// OnStepFinish — one more than an earlier pass over this function counted,
// since OnToolInputStart is also a boundary).
type compositionPhase int

const (
	// phaseToolBoundary means the stream just crossed a tool call/result
	// boundary, or a step just finished: the NEXT text delta starts a new
	// "final composition" span and should log its start.
	phaseToolBoundary compositionPhase = iota
	// phaseComposing means the current span's start was already logged;
	// further text deltas append silently.
	phaseComposing
)

// turnStreamConfig groups newTurnStream's inputs. Several are identically
// typed func() callbacks (bumpActivity/toolStarted/toolFinished/
// startCheckpoint/stopCheckpoint); a plain struct literal with field names
// prevents them from being silently transposed the way same-typed
// constructor positional arguments could be.
type turnStreamConfig struct {
	a      *sessionAgent
	ctx    context.Context
	genCtx context.Context
	cancel context.CancelFunc
	call   SessionAgentCall
	genID  uint64
	stall  *stallClock
	// att is the Drain leg being accounted (nil for any other call).
	att *drainAttempt
	// fatalTools records the turn-ending tool error (tool_fatal_error.go).
	fatalTools *fatalToolCollector

	smartModel     Model
	promptPrefix   string
	historyIDs     map[string]struct{}
	currentSession session.Session

	bumpActivity    func()
	toolStarted     func()
	toolFinished    func()
	startCheckpoint func()
	stopCheckpoint  func()
	notifyUI        func() error
	drainPendingUI  func()

	setPeakHoursAbortErr func(error) bool

	wd               streamWatchdog
	watchdogCauseVal *atomic.Int32
	toolMaxDuration  time.Duration
	timeoutHardCap   time.Duration
	idleTimeout      time.Duration
}

// turnStream is runTurn's fantasy.AgentStreamCall callback state, all in one
// type because the callbacks do not divide along topic (streaming vs. tools
// vs. step-lifecycle) the way their names suggest — they divide along
// OWNERSHIP instead. An earlier plan gave each theme its own type
// (streamRouter/toolsRouter/stepRouter); it was dropped because the theme
// boundary does not match the ownership boundary — e.g. compositionPhase
// above is written from five callbacks spanning all three proposed themes.
//
// Fields fall into three groups:
//
//   - callback-sequence only, no lock: fantasy invokes every callback for
//     one agent.Stream call sequentially from a single loop, never
//     concurrently with each other, so state touched only from inside
//     these methods needs no lock. currentSession belongs here too — it
//     looks turn-wide-shared, but nothing outside this callback sequence
//     ever reads or writes it (the ticker goroutines below only touch
//     currentAssistant), so the original code never locked it either.
//   - shared with this turn's ticker goroutines (turnCheckpointWriter,
//     turnUINotifier, peakHoursWatcher) and so always touched under mu:
//     currentAssistant.
//   - stepMessages/stepTools: written under mu in prepareStep, read
//     without mu in onStepFinish's applyStepUsage. Investigated as task
//     #941: not a real race despite the asymmetry — prepareStep and
//     onStepFinish for a given step (and across steps) never run
//     concurrently with EACH OTHER (same "fantasy invokes every callback
//     sequentially from one loop" guarantee documented on recordStepFinish
//     below), and no ticker goroutine reads either field directly; every
//     value they hand out (scheduleCacheKeepAlive's arguments) is a plain
//     slice-header copy, not a pointer back into these fields. The mu.Lock
//     around the write is therefore vestigial, not load-bearing; left as
//     mu.Lock rather than removed, since removing it would be a second,
//     unrelated behavior change riding on a decomposition task.
type turnStream struct {
	a      *sessionAgent
	ctx    context.Context // outer ctx: survives genCtx's cancellation, used where a write must land even mid-cancel
	genCtx context.Context // this turn's cancelable ctx
	cancel context.CancelFunc
	call   SessionAgentCall
	genID  uint64
	stall  *stallClock
	att    *drainAttempt

	fatalTools *fatalToolCollector

	smartModel   Model
	promptPrefix string

	historyIDs map[string]struct{}

	bumpActivity    func()
	toolStarted     func()
	toolFinished    func()
	startCheckpoint func()
	stopCheckpoint  func()
	notifyUI        func() error
	drainPendingUI  func()

	setPeakHoursAbortErr func(error) bool

	wd               streamWatchdog
	watchdogCauseVal *atomic.Int32
	toolMaxDuration  time.Duration
	timeoutHardCap   time.Duration
	idleTimeout      time.Duration

	// callback-sequence-only state — see doc above.
	phase               compositionPhase
	stepHistory         []fantasy.StepResult
	loopDetected        bool
	loopDetail          loopDetail
	sanitizedToolCalls  map[string]bool
	shouldSummarize     bool
	silentCompactNeeded bool
	currentSession      session.Session

	// progress is the in-turn progress guard's state (turn_progress_guard.go).
	// Scoped to ONE turn — a new turn starts from zero, cross-turn chains are
	// #1113's business. Written only by prepareStep (coverage resets, the
	// guard snapshot) and by onStepFinish's recordStepHistory (the step's class,
	// the streak, stopped), read by StopWhen: the same sequential callback
	// sequence as stepHistory/loopDetected above, so it needs no lock either.
	// The guard snapshot itself (progress.guard) is a plain value handed to the
	// step's tool wrappers, which may run concurrently for parallel tools.
	progress turnProgress

	// carriedSplices is every mid-turn insertion (notice pulls AND mailbox
	// injects) this turn's PrepareStep has spliced into a step's prompt,
	// each with the position it landed at (phase-4 step 3, doc sec.3.4's
	// second bullet). fantasy builds each step's input as initialPrompt +
	// step responses, so a splice made at one step's boundary is otherwise
	// visible to that ONE step only; spliceCarried re-inserts every one at
	// its original position at every later step's boundary (BEFORE window
	// trimming and cache marking). Callback-sequence-only state, like
	// stepHistory above.
	carriedSplices []carriedSplice

	// currentAssistant is shared with the ticker goroutines; every touch,
	// from ANY goroutine including these callbacks, holds mu.
	mu               sync.Mutex
	currentAssistant *message.Message
	// retryStale arms the lazy truncation of the cut attempt's streamed
	// content after a provider retry: onRetry sets it, the retried
	// attempt's first content callback consumes it, and the per-step
	// reset (beginAssistantStep) clears it so it can never leak into a
	// later step. Kept under mu alongside the currentAssistant it acts
	// on, though the callbacks themselves are sequential.
	retryStale bool

	// See the struct doc's third bullet: asymmetric locking preserved as-is.
	stepMessages []fantasy.Message
	stepTools    []fantasy.AgentTool
	// toolReceipts is the per-step receipt sink installed by
	// prepareStepWithToolReceipts and consumed by call ID in onToolResult;
	// toolResultErr latches the first fused-persistence failure so the NEXT
	// PrepareStep surfaces it (fantasy ignores OnToolResult errors).
	toolResultMu  sync.Mutex
	toolReceipts  *toolResultReceiptSink
	toolResultErr error
}

func newTurnStream(cfg turnStreamConfig) *turnStream {
	return &turnStream{
		a:                    cfg.a,
		ctx:                  cfg.ctx,
		genCtx:               cfg.genCtx,
		cancel:               cfg.cancel,
		call:                 cfg.call,
		att:                  cfg.att,
		fatalTools:           cfg.fatalTools,
		genID:                cfg.genID,
		stall:                cfg.stall,
		smartModel:           cfg.smartModel,
		promptPrefix:         cfg.promptPrefix,
		historyIDs:           cfg.historyIDs,
		currentSession:       cfg.currentSession,
		bumpActivity:         cfg.bumpActivity,
		toolStarted:          cfg.toolStarted,
		toolFinished:         cfg.toolFinished,
		startCheckpoint:      cfg.startCheckpoint,
		stopCheckpoint:       cfg.stopCheckpoint,
		notifyUI:             cfg.notifyUI,
		drainPendingUI:       cfg.drainPendingUI,
		setPeakHoursAbortErr: cfg.setPeakHoursAbortErr,
		wd:                   cfg.wd,
		watchdogCauseVal:     cfg.watchdogCauseVal,
		toolMaxDuration:      cfg.toolMaxDuration,
		timeoutHardCap:       cfg.timeoutHardCap,
		idleTimeout:          cfg.idleTimeout,
		phase:                phaseToolBoundary,
		sanitizedToolCalls:   make(map[string]bool),
	}
}

// streamCall builds the fantasy.AgentStreamCall for this turn's agent.Stream
// invocation. Once every callback is a turnStream method value instead of a
// closure, this is the whole "router": one flat struct literal, no branching.
func (ts *turnStream) streamCall(history []fantasy.Message, files []fantasy.FilePart, maxOutputTokens *int64) fantasy.AgentStreamCall {
	return fantasy.AgentStreamCall{
		MaxRetries:       &ProviderMaxRetries,
		Prompt:           message.PromptWithTextAttachments(ts.call.Prompt, ts.call.Attachments),
		Files:            files,
		Messages:         history,
		Headers:          sessionHeaders(ts.call.SessionID),
		ProviderOptions:  ts.call.ProviderOptions,
		MaxOutputTokens:  maxOutputTokens,
		TopP:             ts.call.TopP,
		Temperature:      ts.call.Temperature,
		PresencePenalty:  ts.call.PresencePenalty,
		TopK:             ts.call.TopK,
		FrequencyPenalty: ts.call.FrequencyPenalty,
		PrepareStep:      ts.prepareStepWithToolReceipts,
		OnReasoningStart: ts.onReasoningStart,
		OnReasoningDelta: ts.onReasoningDelta,
		OnReasoningEnd:   ts.onReasoningEnd,
		OnTextDelta:      ts.onTextDelta,
		OnToolInputStart: ts.onToolInputStart,
		OnToolInputDelta: ts.onToolInputDelta,
		OnToolInputEnd:   ts.onToolInputEnd,
		OnRetry:          ts.onRetry,
		OnWarnings:       ts.onWarnings,
		OnToolCall:       ts.onToolCall,
		OnToolResult:     ts.onToolResult,
		OnStepFinish:     ts.onStepFinish,
		StopWhen:         ts.stopConditions(),
	}
}

type toolReceiptContextKey struct{}

type toolResultReceipt struct {
	identity session.BatchToolResultIdentity
	// persist fuses the actual tool-result message with the guarded root
	// consumption CAS in ONE transaction (session.BatchConsumeToolResult).
	persist func(context.Context, message.CreateMessageParams) (message.Message, bool, error)
}

type toolResultReceiptSink struct {
	mu       sync.Mutex
	receipts map[string]toolResultReceipt
}

func (s *toolResultReceiptSink) add(receipt toolResultReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.receipts == nil {
		receipts := make(map[string]toolResultReceipt)
		s.receipts = receipts
	}
	id := receipt.identity.ToolCallID
	if _, exists := s.receipts[id]; exists {
		return errors.New("batch tool result: duplicate call receipt")
	}
	s.receipts[id] = receipt
	return nil
}

func (s *toolResultReceiptSink) take(callID string) (toolResultReceipt, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.receipts[callID]
	delete(s.receipts, callID)
	return r, ok
}

func (ts *turnStream) prepareStepWithToolReceipts(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
	ts.toolResultMu.Lock()
	priorErr := ts.toolResultErr
	ts.toolResultMu.Unlock()
	if priorErr != nil {
		return ctx, fantasy.PrepareStepResult{}, priorErr
	}
	sink := &toolResultReceiptSink{}
	ts.toolResultMu.Lock()
	ts.toolReceipts = sink
	ts.toolResultMu.Unlock()
	updated, prepared, err := ts.prepareStep(ctx, opts)
	if err != nil {
		return updated, prepared, err
	}
	prepared.Tools = withFatalToolErrors(prepared.Tools, ts.fatalTools)
	return context.WithValue(updated, toolReceiptContextKey{}, sink), prepared, nil
}

// takeToolReceipt removes THIS step's receipt for the call, if the delivery
// registered one. The sink is per-step and installed only by
// prepareStepWithToolReceipts, so it never leaks across steps or turns.
func (ts *turnStream) takeToolReceipt(callID string) (toolResultReceipt, bool) {
	ts.toolResultMu.Lock()
	sink := ts.toolReceipts
	ts.toolResultMu.Unlock()
	if sink == nil {
		return toolResultReceipt{}, false
	}
	return sink.take(callID)
}

func (ts *turnStream) recordToolReceiptError(err error) {
	if err == nil {
		return
	}
	ts.toolResultMu.Lock()
	if ts.toolResultErr == nil {
		ts.toolResultErr = err
	}
	ts.toolResultMu.Unlock()
}

// beginAssistantStep installs a freshly created step assistant row as the
// stream target and resets per-step callback state: a retry flag armed by
// an earlier step's failed attempt must never truncate this step's content.
func (ts *turnStream) beginAssistantStep(msg *message.Message) {
	ts.mu.Lock()
	ts.currentAssistant = msg
	ts.retryStale = false
	ts.mu.Unlock()
}

func (ts *turnStream) onReasoningStart(id string, reasoning fantasy.ReasoningContent) error {
	ts.bumpActivity()
	slog.Debug("agent: OnReasoningStart called", "id", id)
	ts.mu.Lock()
	// ReasoningStart carries the retried attempt's first reasoning text
	// and persists immediately, so the lazy truncation fires here too —
	// before that text lands. Its own Update below then persists the
	// already-truncated state; no extra write is needed.
	ts.truncateStaleRetryContentLocked()
	ts.currentAssistant.StartReasoningContent(reasoning.Text)
	if anthropicData, ok := reasoning.ProviderMetadata[anthropic.Name]; ok {
		if metadata, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok && metadata.RedactedData != "" {
			ts.currentAssistant.SetReasoningRedactedData(metadata.RedactedData)
		}
	}
	snap := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	return ts.a.messages.Update(ts.genCtx, snap)
}

func (ts *turnStream) onReasoningDelta(id string, text string) error {
	ts.bumpActivity()
	if ts.stall != nil {
		ts.stall.reasoningChars.Add(int64(len(text))) // diagnostics only: never moves lastProgress
	}
	slog.Debug("agent: OnReasoningDelta called", "len", len(text))
	ts.mu.Lock()
	truncSnap := ts.truncateStaleRetryContentLocked()
	ts.currentAssistant.AppendReasoningContent(text)
	ts.mu.Unlock()
	ts.persistTruncatedRetrySnap(truncSnap)
	return ts.notifyUI()
}

func (ts *turnStream) onReasoningEnd(id string, reasoning fantasy.ReasoningContent) error {
	ts.bumpActivity()
	ts.mu.Lock()
	// handle anthropic signature
	if anthropicData, ok := reasoning.ProviderMetadata[anthropic.Name]; ok {
		if reasoning, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok {
			ts.currentAssistant.AppendReasoningSignature(reasoning.Signature)
			if reasoning.RedactedData != "" {
				ts.currentAssistant.SetReasoningRedactedData(reasoning.RedactedData)
			}
		}
	}
	if googleData, ok := reasoning.ProviderMetadata[google.Name]; ok {
		if reasoning, ok := googleData.(*google.ReasoningMetadata); ok {
			ts.currentAssistant.AppendThoughtSignature(reasoning.Signature, reasoning.ToolID)
		}
	}
	if openaiData, ok := reasoning.ProviderMetadata[openai.Name]; ok {
		if reasoning, ok := openaiData.(*openai.ResponsesReasoningMetadata); ok {
			ts.currentAssistant.SetReasoningResponsesData(reasoning)
		}
	}
	ts.currentAssistant.FinishThinking()
	snap := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	return ts.a.messages.Update(ts.genCtx, snap)
}

func (ts *turnStream) onTextDelta(id string, text string) error {
	ts.bumpActivity()
	ts.stall.markProgress()
	// Fork patch: batch 8 — start the checkpoint ticker on the
	// first text delta of this step (lazily, once only).
	ts.startCheckpoint()
	ts.mu.Lock()
	// First content delta after a retry: drop the cut attempt's
	// streamed text/reasoning before the retry's own delta lands.
	// Revert check: without this the saved answer reads as the cut
	// attempt's partial followed by the retry's full answer.
	truncSnap := ts.truncateStaleRetryContentLocked()
	// Fork patch: batch 8 — emit final-composition log at most
	// once per step, on the first text delta after a tool boundary.
	if ts.phase == phaseToolBoundary && ts.currentAssistant != nil {
		ts.phase = phaseComposing
		slog.Info(
			"agent: final composition started",
			"session_id", ts.call.SessionID,
			"message_id", ts.currentAssistant.ID,
			"chars_in_message_so_far", len(ts.currentAssistant.FullText()),
		)
	}
	// Strip leading newline from initial text content. This is is
	// particularly important in non-interactive mode where leading
	// newlines are very visible.
	if len(ts.currentAssistant.Parts) == 0 {
		text = strings.TrimPrefix(text, "\n")
	}

	ts.currentAssistant.AppendContent(text)
	ts.mu.Unlock()
	ts.persistTruncatedRetrySnap(truncSnap)
	return ts.notifyUI()
}

func (ts *turnStream) onToolInputStart(id string, toolName string) error {
	ts.bumpActivity()
	ts.phase = phaseToolBoundary // Fork patch: batch 8
	toolCall := message.ToolCall{
		ID:               id,
		Name:             toolName,
		ProviderExecuted: false,
		Finished:         false,
	}
	ts.mu.Lock()
	ts.currentAssistant.AddToolCall(toolCall)
	snap := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	// Use parent ctx instead of genCtx to ensure the update succeeds
	// even if the request is canceled mid-stream
	return ts.a.messages.Update(ts.ctx, snap)
}

func (ts *turnStream) onToolInputDelta(id string, delta string) error {
	ts.bumpActivity()
	ts.mu.Lock()
	ts.currentAssistant.AppendToolCallInput(id, delta)
	ts.mu.Unlock()
	return nil // don't spam DB on every delta; ToolInputEnd will persist
}

func (ts *turnStream) onToolInputEnd(id string) error {
	ts.bumpActivity()
	ts.mu.Lock()
	ts.currentAssistant.FinishToolCall(id)
	snap := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	return ts.a.messages.Update(ts.genCtx, snap)
}

func (ts *turnStream) onRetry(err *fantasy.ProviderError, delay time.Duration) {
	ts.bumpActivity()
	slog.Warn("Provider request failed, retrying", providerRetryLogFields(err, delay)...)
	// fantasy re-runs the whole step on the SAME callbacks (its retry
	// wraps Stream+processStepStream; PrepareStep is not re-run), so the
	// cut attempt's partial text/reasoning would ride under the retry's
	// deltas. Arming only — truncation happens lazily on the retried
	// attempt's first content callback; if the retries fail and no delta
	// ever follows, the partial stays for the coordinator's continuation.
	ts.mu.Lock()
	ts.retryStale = true
	ts.mu.Unlock()
}

// truncateStaleRetryContentLocked drops the cut attempt's streamed
// text/reasoning on the first content callback of a retried attempt. Lazy
// by design: if no delta ever follows the retry, the partial stays in the
// message for the coordinator's continuation. Tool-call parts are kept —
// orphaned ones are filtered later by preparePrompt's orphaned-tool-call
// filters. Returns a snapshot of the truncated state to persist, or nil
// when nothing was armed or nothing was removed. Call with ts.mu held.
func (ts *turnStream) truncateStaleRetryContentLocked() *message.Message {
	if !ts.retryStale || ts.currentAssistant == nil {
		return nil
	}
	ts.retryStale = false
	if !ts.currentAssistant.TruncateStreamedContent() {
		return nil
	}
	snap := ts.currentAssistant.Clone()
	return &snap
}

// persistTruncatedRetrySnap lands the truncated snapshot on the outer ctx —
// it must survive a mid-stream cancel, like onToolInputStart's write.
func (ts *turnStream) persistTruncatedRetrySnap(snap *message.Message) {
	if snap == nil {
		return
	}
	if updateErr := ts.a.messages.Update(ts.ctx, *snap); updateErr != nil {
		slog.Warn("agent: failed to persist truncated assistant after retry",
			"session_id", ts.call.SessionID, "message_id", snap.ID, "err", updateErr)
	}
}

func (ts *turnStream) onWarnings(warnings []fantasy.CallWarning) error {
	for _, w := range warnings {
		slog.Warn("Provider warning", "type", w.Type, "message", w.Message)
	}
	return nil
}

func (ts *turnStream) onToolCall(tc fantasy.ToolCallContent) error {
	ts.bumpActivity()
	// A tool is about to execute — pause the stall watchdog until its
	// result arrives (OnToolResult). fantasy fires every OnToolCall
	// for a step before executing any tool, so the counter brackets
	// the whole executeTools window. The same toolMaxDuration cap
	// bounds every tool, including a sub-agent delegation (the
	// `agent` tool) — see toolExecutionMaxDefault's doc in agent.go.
	ts.toolStarted()
	ts.stall.toolEnter(tc.ToolName, tc.ToolCallID)
	ts.stall.markProgress()
	ts.phase = phaseToolBoundary // Fork patch: batch 8
	input, wasSanitized := sanitizeToolInput(tc.ToolName, tc.ToolCallID, tc.Input)
	if wasSanitized {
		ts.sanitizedToolCalls[tc.ToolCallID] = true
	}
	toolCall := message.ToolCall{
		ID:               tc.ToolCallID,
		Name:             tc.ToolName,
		Input:            input,
		ProviderExecuted: false,
		Finished:         true,
	}
	ts.mu.Lock()
	ts.currentAssistant.AddToolCall(toolCall)
	snap := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	// Use parent ctx instead of genCtx to ensure the update succeeds
	// even if the request is canceled mid-stream
	return ts.a.messages.Update(ts.ctx, snap)
}

func (ts *turnStream) onToolResult(result fantasy.ToolResultContent) error {
	ts.bumpActivity()
	// Tool finished — resume the stall watchdog (and restart its idle
	// window so the tool's runtime isn't counted against the provider).
	ts.toolFinished()
	ts.stall.toolExit()
	ts.stall.markProgress()
	ts.phase = phaseToolBoundary // Fork patch: batch 8
	toolResult := ts.a.convertToToolResult(result)
	// Fork patch (#1147): hallucinated tool names get a self-correctable
	// answer instead of a bare "tool not found".
	toolResult = ts.a.augmentUnknownToolResult(toolResult)
	if ts.sanitizedToolCalls[result.ToolCallID] {
		toolResult.Content = "Tool call failed: arguments were not valid JSON. Please check your tool call format and try again."
		toolResult.IsError = true
	}
	ts.mu.Lock()
	sessionID := ts.currentAssistant.SessionID
	ts.mu.Unlock()
	params := message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			toolResult,
		},
	}
	receipt, hasReceipt := ts.takeToolReceipt(result.ToolCallID)
	if hasReceipt {
		if receipt.identity.Owner != sessionID || receipt.identity.ToolCallID != result.ToolCallID {
			err := errors.New("batch tool result: receipt identity mismatch")
			ts.recordToolReceiptError(err)
			return err
		}
		// Fused delivery identity: the actual tool message IS the consumed
		// delivery. Ordinary persistence is skipped for this call.
		if _, _, err := receipt.persist(ts.ctx, params); err != nil {
			ts.recordToolReceiptError(err)
			return err
		}
		return nil
	}
	// Batch-only: a batch delivery's result was persisted above through its
	// fused receipt (the tool message IS the consumed delivery); every other
	// result is a plain Create. The outer ctx is used so the message is
	// created even if the request is canceled mid-stream.
	_, createMsgErr := ts.a.messages.Create(ts.ctx, sessionID, params)
	return createMsgErr
}
