package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/heartbeat"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// prepareStep, onStepFinish and stopConditions are runTurn's remaining
// step-lifecycle callbacks, moved onto turnStream mechanically (task
// #933/#937): identifiers renamed to ts.* fields, no behavioral change.
// onStepFinish is still the single ~230-line body it always was — splitting
// it into named phases is task #940's job, kept separate so this move's
// oracle stays a plain "identical modulo rename" comparison.
func (ts *turnStream) prepareStep(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
	// PrepareStep runs before the first token of the step and can
	// take non-trivial time (sliding-window trim, background
	// summarise kickoff, cache-control wiring). Bump first so a
	// slow prepare doesn't trip the watchdog before the stream
	// even starts.
	ts.bumpActivity()
	ts.stall.setPhase("provider", ts.smartModel.ModelCfg.Model, "")
	prepared.Messages = options.Messages
	for i := range prepared.Messages {
		prepared.Messages[i].ProviderOptions = nil
	}

	// R3-1: the call's PINNED tool slice when it carries one —
	// identical at every step, immune to a concurrent call's
	// SetTools landing between steps. Only legacy calls (nothing
	// pinned) re-read the shared slice (updated by SetTools when
	// MCP tools change). The cache-control marker is re-applied per
	// step through the same per-step wrapper the turn's initial
	// tool list uses; withProviderOptionsOnLast clones before
	// writing, so the pinned slice itself is never mutated.
	pinnedStepTools := ts.call.Tools
	if pinnedStepTools == nil {
		pinnedStepTools = ts.a.tools.Copy()
	}
	prepared.Tools = withProviderOptionsOnLast(pinnedStepTools, ts.a.getCacheControlOptions())

	// stepSplices collects every message THIS step's boundary inserts mid-
	// turn (mailbox injects, cross-process injects, phase-4 notice pulls) --
	// not appended to prepared.Messages directly. They land there together
	// with every earlier step's own splices (ts.carriedSplices) below, so
	// a step-3 splice is still visible verbatim at step 4, 5, … of this SAME
	// turn (doc sec.3.4's second bullet): fantasy's own options.Messages for
	// step N is initialPrompt + step responses, which does NOT carry
	// forward whatever a PREVIOUS step's PrepareStep spliced in.
	var stepSplices []fantasy.Message

	for _, inj := range ts.a.drainDueInjects(ts.call.SessionID, ts.genID, ts.historyIDs) {
		stepSplices = append(stepSplices, inj.ToAIMessage()...)
	}

	// Cross-process inject drain: rows written by another process
	// (`rush sessions inject`) into pending_injects. The message
	// row already exists in the DB (the CLI created it at inject
	// time for immediate web-UI visibility), so we only load it by
	// message_id and splice it in — no second Create, no dup row.
	// DrainPendingInjects deletes the consumed non-interrupt rows in
	// the same transaction (delete-after-read).
	pending, hasInterrupt, drainErr := ts.a.sessions.DrainPendingInjects(callContext, ts.call.SessionID)
	if drainErr != nil {
		return callContext, prepared, drainErr
	}
	if hasInterrupt {
		// Defensive: interrupt rows are meant to be consumed by the
		// interrupt ticker before PrepareStep runs. If one is still
		// here it is a race, not a normal path.
		slog.Warn("pending interrupt inject present during non-interrupt PrepareStep drain",
			"session_id", ts.call.SessionID)
	}
	for _, inj := range pending {
		// R8C-1: `sessions inject` saves the message before it queues the row, so
		// a row queued while nothing ran points at a message the turn's history
		// already holds; the row is consumed (DrainPendingInjects deleted it) but
		// the message is not spliced a second time -- the mailbox's rule.
		if _, inHistory := ts.historyIDs[inj.MessageID]; inHistory {
			continue
		}
		injMsg, getErr := ts.a.messages.Get(callContext, inj.MessageID)
		if getErr != nil {
			// The referenced message vanished (e.g. cascade delete):
			// skip it rather than aborting the whole step.
			slog.Warn("pending inject references missing message, skipping",
				"session_id", ts.call.SessionID, "message_id", inj.MessageID, "error", getErr)
			continue
		}
		stepSplices = append(stepSplices, injMsg.ToAIMessage()...)
		// The row was written by a foreign process (`rush sessions
		// inject`), so its Create() never published through THIS
		// process's message broker. If a web UI happens to be
		// attached to this process for the session, Notify pushes
		// the already-persisted message so it renders live instead
		// of waiting for a page reload.
		ts.a.messages.Notify(injMsg)
	}

	// Phase-4 step 3 (DUR-3, doc sec.3.3): the step-boundary half of the
	// driver's pull, alongside the inject drains above. Compaction steps
	// use their own separate PrepareStep closures (agent_compaction.go) and
	// never reach this method at all, so they never pull.
	for _, msg := range ts.a.pullPendingNoticesForStep(callContext, ts.call.SessionID) {
		stepSplices = append(stepSplices, msg.ToAIMessage()...)
	}

	// Re-insert every earlier step's mid-turn splice at the SAME position it
	// first landed, then this step's own new ones at the end, BEFORE window
	// trimming/cache marking below (doc sec.3.4).
	prepared.Messages = spliceCarried(prepared.Messages, ts.carriedSplices, stepSplices)
	if len(stepSplices) > 0 {
		ts.carriedSplices = append(ts.carriedSplices, carriedSplice{pos: len(options.Messages), msgs: stepSplices})
		// A step boundary that injected anything may have been the reason a
		// file changed (a worker finished, a notice landed), so every window
		// read earlier this turn may be stale by now (§1.2).
		ts.progress.seen.clear()
	}

	// Sliding-window context management: when the context is nearly
	// full, trim old messages so the agent can keep running without
	// blocking on a synchronous summarisation call.
	if !ts.a.disableAutoSummarize {
		cw := int64(ts.smartModel.CatwalkCfg.ContextWindow)
		if cw > 0 {
			usedTokens := ts.currentSession.CompletionTokens + ts.currentSession.PromptTokens
			remaining := cw - usedTokens
			var slideThreshold int64
			if cw > largeContextWindowThreshold {
				slideThreshold = largeContextWindowBuffer
			} else {
				slideThreshold = int64(float64(cw) * smallContextWindowRatio)
			}
			if remaining <= slideThreshold {
				targetTokens := int64(float64(cw) * contextSlideRatio)
				prepared.Messages = trimMessagesToWindow(prepared.Messages, targetTokens)

				// Trimmed away content is no longer in the prompt, so what was
				// read from it is no longer "already in your context" either
				// (§1.2).
				ts.progress.seen.clear()

				// Record that a silent compact is needed — it runs
				// synchronously AFTER the turn completes (under the
				// turn's mailbox ownership), not as a concurrent
				// goroutine. A goroutine deleting messages while the
				// turn is still streaming was the P0-4 data corruption
				// bug (#268).
				ts.silentCompactNeeded = true
			}
		}
	}

	prepared.Messages = ts.a.workaroundProviderMediaLimitations(prepared.Messages, ts.smartModel)

	// In-turn progress guard (§2.2 of the in-turn plan). This runs AFTER both
	// coverage resets above — the snapshot must reflect the coverage the
	// model will actually see in this step's prompt — and AFTER
	// withProviderOptionsOnLast, whose per-step cache-control wrapper the
	// guard wrapper sits around (it delegates ProviderOptions, so the marker
	// still reaches the provider). The snapshot is taken BEFORE the assistant
	// message is created so the refusal decision belongs to the step it was
	// computed for, never to the next one. The pinned slice itself is only
	// read, never mutated (R3-1); the wrapper slice is per-step.
	ts.armProgressGuard(callContext, pinnedStepTools, &prepared.Tools)

	lastSystemRoleInx := 0
	systemMessageUpdated := false
	for i, msg := range prepared.Messages {
		// Only add cache control to the last message.
		if msg.Role == fantasy.MessageRoleSystem {
			lastSystemRoleInx = i
		} else if !systemMessageUpdated {
			prepared.Messages[lastSystemRoleInx].ProviderOptions = ts.a.getCacheControlOptions()
			systemMessageUpdated = true
		}
		// Than add cache control to the last 2 messages.
		if i > len(prepared.Messages)-3 {
			prepared.Messages[i].ProviderOptions = ts.a.getCacheControlOptions()
		}
	}

	if ts.promptPrefix != "" {
		prepared.Messages = append([]fantasy.Message{fantasy.NewSystemMessage(ts.promptPrefix)}, prepared.Messages...)
	}

	ts.mu.Lock()
	ts.stepMessages = cloneFantasyMessages(prepared.Messages)
	ts.stepTools = append([]fantasy.AgentTool(nil), prepared.Tools...)
	ts.mu.Unlock()

	var assistantMsg message.Message
	// Provenance is recorded from the model that ACTUALLY produced
	// the message, not from the configuration that selected it.
	//
	// Both values feed `GROUP BY model, provider` in the usage and
	// cost reports (internal/db/stats.sql.go, messages.sql.go), and
	// the two summarize paths in agent_compaction.go have always
	// recorded Model.Model()/Provider(). While this line recorded
	// ModelCfg instead, a provider whose canonical id differs from
	// the configured one would split a single session into two
	// groups in `rush sessions cost` — with neither number looking
	// wrong enough to notice.
	assistantMsg, err = ts.a.messages.Create(callContext, ts.call.SessionID, message.CreateMessageParams{
		Role:            message.Assistant,
		Parts:           []message.ContentPart{},
		Model:           ts.smartModel.Model.Model(),
		Provider:        ts.smartModel.Model.Provider(),
		ReasoningEffort: turnSmartReasoningEffort(ts.smartModel),
	})
	if err != nil {
		return callContext, prepared, err
	}
	callContext = context.WithValue(callContext, tools.MessageIDContextKey, assistantMsg.ID)
	callContext = context.WithValue(callContext, tools.SupportsImagesContextKey, ts.smartModel.CatwalkCfg.SupportsImages)
	callContext = context.WithValue(callContext, tools.ModelNameContextKey, ts.smartModel.CatwalkCfg.Name)
	ts.beginAssistantStep(&assistantMsg)
	// R3-1 (round 5): report this row's ID to the caller (e.g. the
	// coordinator's transient-retry loop) so classification can act on
	// THIS attempt's own evidence instead of session-wide last-row state.
	if ts.call.OnAssistantMessageCreated != nil {
		ts.call.OnAssistantMessageCreated(assistantMsg.ID)
	}
	return callContext, prepared, err
}

// onStepFinish is fantasy's per-step callback, split into 9 named phases
// (task #940) after the mechanical move in task #939 kept it as one
// ~230-line body. Each phase is verified by a targeted revert-check (break
// the invariant, confirm the specific existing test fails with the specific
// expected symptom, restore) rather than a diff — a behavioral split has no
// diff oracle. The phase order below IS the invariant in four places:
// stopStepTicker must run before recordStepFinish touches currentAssistant
// (else the checkpoint ticker races the final write); recordStepHistory
// must run before recordStepFinish reads loopDetected (fantasy calls
// OnStepFinish before StopWhen for the same step, so a stale flag here
// never gets fixed by a later step) — and, for the same reason, before it
// reads the in-turn progress guard's stopped flag, which recordStepHistory
// sets for this very step; and enforceRunawayCaps/recheckPeakHours
// must call activeRequests' cancelFn(), not just return an error, because
// returning an error from OnStepFinish alone does not break fantasy's loop
// (BUG-4, pinned by TestActiveRequests_HoldsLiveCancelDuringTurn).
// persistStepFinish runs before both abort checks: the step that crossed a
// cap is still a recorded reaction.
func (ts *turnStream) onStepFinish(stepResult fantasy.StepResult) error {
	ts.bumpActivity()
	ts.stall.markProgress()
	ts.recordStepHistory(stepResult)
	// Surface provider CallWarnings (malformed tool-call sanitization,
	// unsupported settings, etc.) that fantasy otherwise discards
	// silently. Visible in logs only — does not interrupt the turn.
	logProviderWarnings(stepResult.Warnings)
	ts.stopStepTicker()

	finishReason := classifyStepFinishReason(stepResult)
	ts.recordStepFinish(finishReason)

	updatedSession, err := ts.applyStepUsage(stepResult)
	if err != nil {
		return err
	}
	// The step's reaction is recorded BEFORE the abort checks: a step the
	// provider already answered and billed is a reaction even when a cap or
	// the peak-hours window ends the turn right after it (else the notice
	// stays debt and a paid reaction turn repeats without bound).
	// The step's reaction is recorded BEFORE the abort checks: a step the
	// provider already answered and billed is a reaction even when a cap or
	// the peak-hours window ends the turn right after it (else the notice
	// stays debt and a paid reaction turn repeats without bound).
	ts.stall.setPhase("internal", "persist", "")
	persistErr := ts.persistStepFinish()
	if err := ts.enforceRunawayCaps(updatedSession); err != nil {
		if persistErr != nil {
			slog.Warn("agent: step finish not persisted before a cap abort", "session_id", ts.call.SessionID, "err", persistErr)
		}
		return err
	}
	if err := ts.recheckPeakHours(); err != nil {
		if persistErr != nil {
			slog.Warn("agent: step finish not persisted before a peak-hours abort", "session_id", ts.call.SessionID, "err", persistErr)
		}
		return err
	}
	return persistErr
}

// recordStepHistory accumulates this step and recomputes loop detection
// NOW, in this callback invocation, so recordStepFinish below can use the
// result for THIS step. Fantasy calls OnStepFinish BEFORE StopWhen for the
// same step, so relying on the StopWhen closure to set loopDetected would
// read a stale (still-false) flag for the very step that trips the
// detector — the loop would break with empty finish text and no later
// OnStepFinish to fix it. See turnStream's doc for the ordering rationale.
func (ts *turnStream) recordStepHistory(stepResult fantasy.StepResult) {
	ts.stepHistory = append(ts.stepHistory, stepResult)
	ts.loopDetected, ts.loopDetail = hasRepeatedToolCalls(ts.stepHistory, loopDetectionWindowSize, loopDetectionMaxRepeats)
	ts.recordChainEvidence(stepResult)
	ts.recordStepProgress(stepResult)
}

// armProgressGuard snapshots the guard state for the step being prepared and,
// when that step could refuse anything, replaces tools with a wrapper around
// each entry (§2.2). pinned is the call's pinned (or copied shared) slice, read
// only; out points at prepared.Tools, which is a fresh per-step slice already
// carrying the cache-control wrapper.
func (ts *turnStream) armProgressGuard(callContext context.Context, pinned []fantasy.AgentTool, out *[]fantasy.AgentTool) {
	async := func() bool {
		origin := CallOriginFrom(callContext)
		return origin == message.OriginCLI || origin == message.OriginWeb
	}()
	// ownWork's source is the bound batch cutover facts (ownWorkBatch), the
	// same seam CLIActiveWork reads. The helper is used only when the facts
	// are INSTALLED (the same predicate CLIActiveWork applies, so a run
	// without an installed runtime answers idle instead of hiding live work).
	var ownWork bool
	if async {
		if ts.a.coord != nil && batchOwnWorkFactsInstalled(ts.a.coord) {
			ownWork = ts.a.ownWorkBatch(callContext, ts.call.SessionID)
		}
	}

	guard := stepGuardFor(&ts.progress, ownWork, async)
	guard.canEdit, guard.canDelegate = stepToolCapabilities(pinned)
	ts.progress.guard = guard

	if refuseWait(guard.streak, guard.ownWork, guard.async) || guard.streak >= inTurnRefuseAfter {
		*out = wrapProgressGuard(*out, guard)
	}
}

// stepToolCapabilities reports what this step's tool set allows: editing files
// and delegating to a sub-agent. Read from the set's own names, since a stale
// session prompt cannot make a tool appear (§4 of the in-turn plan).
func stepToolCapabilities(set []fantasy.AgentTool) (canEdit, canDelegate bool) {
	for _, tool := range set {
		switch tool.Info().Name {
		case tools.EditToolName, tools.MultiEditToolName, tools.WriteToolName,
			tools.FSWriteToolName, tools.FSReplaceToolName, tools.FSWriteLinesToolName,
			tools.FSDeleteToolName:
			canEdit = true
		case AgentToolName:
			canDelegate = true
		}
	}
	return canEdit, canDelegate
}

// recordStepProgress folds this step's calls into the in-turn guard's state
// (§1.2): each call is classified against the coverage captured at the STEP'S
// START (the guard snapshot), never against the live map, and the step's own
// windows are added only afterwards. Called from recordStepHistory, where
// every result of the step is already on hand and before recordStepFinish
// reads stopped for this same step.
func (ts *turnStream) recordStepProgress(stepResult fantasy.StepResult) {
	calls := make(map[string]fantasy.ToolCallContent, len(stepResult.Content))
	for _, call := range stepResult.Content.ToolCalls() {
		calls[call.ToolCallID] = call
	}
	var (
		classes []stepCallClass
		spans   [][]namedSpan
	)
	for _, result := range stepResult.Content.ToolResults() {
		call, ok := calls[result.ToolCallID]
		if !ok {
			continue
		}
		classes = append(classes, classifyStepCall(call, result, stepClassifyCtx{seen: ts.progress.guard.seen}))
		spans = append(spans, readSpans(call))
	}

	wasStopped := ts.progress.stopped
	refused := ts.progress.applyStep(classes, spans)
	if refused > 0 {
		// The wrapper is external, so loggedTool never sees a refusal: the
		// guard reports it itself (§2.2).
		slog.Warn("agent: in-turn progress guard refused tool calls",
			"session_id", ts.call.SessionID,
			"refused", refused,
			"streak", ts.progress.streak,
			"waits", ts.progress.waits,
			"rereads", ts.progress.rereads)
	}
	if !wasStopped && ts.progress.stopped {
		slog.Warn("agent: in-turn progress guard stopping the turn",
			"session_id", ts.call.SessionID,
			"streak", ts.progress.streak,
			"waits", ts.progress.waits,
			"rereads", ts.progress.rereads)
	}
}

// chainNeutralTools are tool calls that neither advance the session's work
// nor launch anything: reading a background job's buffered output and
// managing the todo list. Every other tool (edit/write/view/grep and
// friends, a bash/run_command call with substance, a delegation, a stop
// control) is progress for the reaction chain guard (#1113).
// batch_status/batch_output are the detached-output polling reads -- neutral
// the same way the deleted job_output polling tool was.
var chainNeutralTools = map[string]struct{}{
	tools.TodosToolName: {},
	"batch_status":      {},
	"batch_output":      {},
}

// recordChainEvidence feeds the reaction chain guard (#1113) from this
// step's tool activity, classified by classifyStepCall: every async launch
// that is a pure wait command (bash sleep/echo, run_command sleep/timeout) has
// its claim_id (from the started response's ClientMetadata -- the same tag the
// ack gate reads) recorded on the leg, any real action (act, read, reread)
// marks the leg as progress. A guard refusal is neither -- the tool never
// ran, so it started nothing -- and a text-only or neutral-only step is
// neither.
func (ts *turnStream) recordChainEvidence(stepResult fantasy.StepResult) {
	if ts.att == nil || ts.att.closed {
		return
	}
	calls := make(map[string]fantasy.ToolCallContent, len(stepResult.Content))
	for _, call := range stepResult.Content.ToolCalls() {
		calls[call.ToolCallID] = call
	}
	for _, result := range stepResult.Content.ToolResults() {
		call, ok := calls[result.ToolCallID]
		if !ok {
			continue
		}
		// Batch started tool responses name their launched root in their own
		// text ("batch root <id> started; ...") and carry no ClientMetadata,
		// so the launched root's identity IS the call id. Classify exactly
		// like waits via classifyStepCall: a pure-wait batch launch records
		// the root as chain identity, a real batch action is progress; a
		// refusal or a neutral result started nothing this leg may claim.
		if isBatchStartedToolResult(result) {
			// A batch launch is async by construction (the started response
			// IS the detached boundary and carries no ClientMetadata), so
			// stamp the async half of the legacy started tag and classify
			// EXACTLY like a legacy wait: a pure-wait batch launch records
			// the root's call id as chain identity; a real batch action is
			// progress; a refusal/neutral result started nothing this leg
			// may claim.
			// Repeated-command policy (legacy parity): compute the launch
			// command and consult chainLastLaunch like the legacy branch
			// below (a non-refused launch records its command; an immediate
			// byte-identical repeat is an idle claim, never progress) -- the
			// root's call id is the batch identity appended instead of the
			// legacy ClaimID. A FIRST substantial (non-wait) launch stays
			// progress; a pure-wait launch is unchanged; refusals/neutral
			// stay inert.
			asyncResult := result
			asyncResult.ClientMetadata = `{"async":true}`
			class := classifyStepCall(call, asyncResult, stepClassifyCtx{})
			command, isCommand := asyncLaunchCommand(call)
			repeated := false
			if isCommand && class != stepCallRefused && ts.a != nil && ts.a.coord != nil {
				repeated = ts.a.coord.arb.chainLastLaunch(ts.call.SessionID, command)
			}
			switch class {
			case stepCallWait:
				ts.att.chainBatchRoots = append(ts.att.chainBatchRoots, call.ToolCallID)
			case stepCallRefused, stepCallNeutral:
				// started nothing this leg may claim
			default:
				if repeated {
					// an idle claim: this leg launched nothing new it may
					// count as progress
					ts.att.chainBatchRoots = append(ts.att.chainBatchRoots, call.ToolCallID)
				} else {
					ts.att.chainProgress = true
				}
			}
			continue
		}
		class := classifyStepCall(call, result, stepClassifyCtx{})
		var meta asyncToolMetadata
		_ = json.Unmarshal([]byte(result.ClientMetadata), &meta)
		command, isCommand := asyncLaunchCommand(call)
		repeated := false
		if meta.Async && meta.ClaimID != "" && isCommand && class != stepCallRefused && ts.a != nil && ts.a.coord != nil {
			repeated = ts.a.coord.arb.chainLastLaunch(ts.call.SessionID, command)
		}
		if meta.Async && meta.ClaimID != "" && isCommand && class != stepCallRefused && !repeated {
			if class != stepCallWait {
				ts.att.chainProgress = true
				continue
			}
		}
		if repeated {
			ts.att.chainIdleClaims = append(ts.att.chainIdleClaims, meta.ClaimID)
			continue
		}
		switch class {
		case stepCallWait:
			if meta.Async && meta.ClaimID != "" {
				ts.att.chainIdleClaims = append(ts.att.chainIdleClaims, meta.ClaimID)
			}
		case stepCallRefused, stepCallNeutral:
			// a guard refusal is neither progress nor a claim: it started nothing
		default: // act, read, reread: progress, as #1113 counts reread as progress
			if ts.a != nil && ts.a.coord != nil {
				ts.a.coord.arb.chainLastLaunch(ts.call.SessionID, "")
			}
			ts.att.chainProgress = true
		}
	}
}

func asyncLaunchCommand(call fantasy.ToolCallContent) (string, bool) {
	var command string
	switch call.ToolName {
	case tools.BashToolName:
		var params tools.BashParams
		if json.Unmarshal([]byte(call.Input), &params) != nil {
			return "", false
		}
		command = params.Command
	case tools.RunCommandToolName:
		var params tools.RunCommandParams
		if json.Unmarshal([]byte(call.Input), &params) != nil {
			return "", false
		}
		command = (tools.RunCommandPermissionsParams{Program: params.Program, Args: params.Args}).RunAllowlistCommand()
	default:
		return "", false
	}
	return strings.TrimSpace(command), true
}

// chainIdleClaim reports whether call is an async launch of a pure wait
// command, and returns its claim id when it is. An INLINE result (A14,
// meta.Async false but meta.Inline true) of a pure wait command is idle too
// -- but with NO claim: it never produced a "started" result, so there is no
// launch for the guard to exclude (an inline `sleep 2` must not read as
// progress, nor as an outstanding launch).
func chainIdleClaim(call fantasy.ToolCallContent, meta asyncToolMetadata) (string, bool) {
	isAsyncLaunch := meta.Async && meta.ClaimID != ""
	isInlineResult := !meta.Async && meta.Inline
	if !isAsyncLaunch && !isInlineResult {
		return "", false
	}
	if !isWaitOnlyCall(call) {
		return "", false
	}
	claim := meta.ClaimID
	if isInlineResult {
		claim = "" // inline results carry no outstanding launch to exclude
	}
	return claim, true
}

// stopStepTicker stops the checkpoint ticker BEFORE the final write below so
// it doesn't race with OnStepFinish (Fork patch: batch 8), and resets the
// tool-boundary phase tracker for the next step.
func (ts *turnStream) stopStepTicker() {
	ts.stopCheckpoint()
	ts.phase = phaseToolBoundary
}

// classifyStepFinishReason maps fantasy's step finish reason to this
// package's, then upgrades FinishReasonToolUse to FinishReasonEndTurn when a
// tool result halted the turn (e.g. a hook halt or a permission denial): the
// step ends on FinishReasonToolCalls but the model will not be called
// again, so the UI must still render the assistant footer.
func classifyStepFinishReason(stepResult fantasy.StepResult) message.FinishReason {
	finishReason := message.FinishReasonUnknown
	switch stepResult.FinishReason {
	case fantasy.FinishReasonLength:
		finishReason = message.FinishReasonMaxTokens
	case fantasy.FinishReasonStop:
		finishReason = message.FinishReasonEndTurn
	case fantasy.FinishReasonToolCalls:
		finishReason = message.FinishReasonToolUse
	}
	if finishReason == message.FinishReasonToolUse {
		for _, tr := range stepResult.Content.ToolResults() {
			if tr.StopTurn {
				finishReason = message.FinishReasonEndTurn
				break
			}
		}
	}
	return finishReason
}

// recordStepFinish writes this step's Finish part onto currentAssistant.
// Fork patch: surface empty-stream as a visible error. Some providers (e.g.
// z.ai) sometimes close the stream without sending any content (no text, no
// tool_call, no reasoning) and without an explicit finish reason. The
// upstream code records this as FinishReasonUnknown with empty parts, which
// the WUI renders as a blank assistant block — looking like a session
// lockup. Convert this case to an error so both the WUI fallback and the
// user see an actionable message. See CHANGELOG.fork.md section 4.D.
//
// currentAssistant reads/mutations below are under mu: OnStepFinish never
// runs concurrently with the other streaming callbacks (fantasy invokes
// them sequentially from one loop), but it DOES run concurrently with the
// checkpoint ticker and the peak-hours watcher goroutines, which also touch
// currentAssistant.
func (ts *turnStream) recordStepFinish(finishReason message.FinishReason) {
	ts.mu.Lock()
	if finishReason == message.FinishReasonUnknown &&
		ts.currentAssistant.FullText() == "" &&
		ts.currentAssistant.FullReasoningText() == "" &&
		len(ts.currentAssistant.ToolCalls()) == 0 {
		slog.Warn(
			"agent: empty stream from provider — recording as error",
			"sessionID", ts.call.SessionID,
			"provider", ts.smartModel.ModelCfg.Provider,
			"model", ts.smartModel.ModelCfg.Model,
		)
		ts.currentAssistant.AddFinish(
			message.FinishReasonError,
			"Empty response",
			fmt.Sprintf(
				"Provider %q closed the stream for model %q without returning any content. This is usually a transient provider/network issue — please retry.",
				ts.smartModel.ModelCfg.Provider, ts.smartModel.ModelCfg.Model,
			),
		)
	} else if ts.loopDetected {
		// Loop detection force-stopped the turn. The reason stays
		// FinishReasonEndTurn (NOT a new distinct enum value) so
		// reclassifyCrashedAsDone / sessions-why keep treating this as
		// "done" — but the message/details are non-empty so an operator
		// or orchestrator can distinguish "model finished voluntarily"
		// from "we truncated a likely loop (possibly a legitimate poll)".
		loopMsg, loopDetails := loopDetectedFinishText(ts.loopDetail)
		ts.currentAssistant.AddFinish(finishReason, loopMsg, loopDetails)
	} else if ts.progress.stopped {
		// In-turn progress guard (§2.3): the model made no progress for
		// inTurnStopAfter consecutive steps. The reason stays
		// FinishReasonEndTurn for the same reason loopDetected above
		// does — reclassifyCrashedAsDone / sessions-why must keep
		// treating a guard stop as "done", while the non-empty
		// message/details let an operator tell it apart from a
		// voluntary end.
		ts.currentAssistant.AddFinish(message.FinishReasonEndTurn, InTurnGuardStopTitle, guardStopDetails(ts.progress))
	} else {
		ts.currentAssistant.AddFinish(finishReason, "", "")
	}
	ts.mu.Unlock()
	// Drain any pending UI snapshot so the ticker goroutine does not
	// publish a stale state after messages.Update writes the final one.
	ts.drainPendingUI()
}

// applyStepUsage fetches the session row this step just updated, folds in
// this step's usage/cost, and records both the session- and message-level
// breakdown. Any error here short-circuits OnStepFinish exactly as it did
// before this split — cap enforcement and the peak-hours recheck never run
// against a session snapshot that failed to load or persist.
func (ts *turnStream) applyStepUsage(stepResult fantasy.StepResult) (session.Session, error) {
	updatedSession, getSessionErr := ts.a.sessions.Get(ts.ctx, ts.call.SessionID)
	if getSessionErr != nil {
		return session.Session{}, getSessionErr
	}
	// Fork merge note (origin/main 6ed8852b "fix(agent): estimate
	// missing streamed usage"): if the provider omits the final
	// usage chunk, use upstream's token estimator so our sliding
	// context window stays accurate. We drop the "estimated" flag
	// (TUI marker — see CHANGELOG.fork.md Section 2).
	usage, estimated := fallbackStepUsage(ts.stepMessages, stepResult)
	// Normalize once, upstream of both updateSessionUsage and
	// recordMessageUsage, so InputTokens is exclusive-of-cache for
	// every provider before either consumer sees it.
	usage = normalizeProviderUsage(ts.smartModel.Model.Provider(), usage)
	costDelta := ts.a.updateSessionUsage(ts.smartModel, &updatedSession, usage, ts.a.openrouterCost(stepResult.ProviderMetadata))
	// Heartbeat usage: one add per accounted step, executing model identity (design pt. 4).
	addHeartbeatUsage(ts.genCtx, ts.smartModel.Model.Provider(), ts.smartModel.Model.Model(), usage, costDelta)
	if costDelta != 0 {
		if _, costErr := ts.a.sessions.IncrementCost(ts.ctx, updatedSession.ID, costDelta); costErr != nil {
			return session.Session{}, costErr
		}
	}
	if sessionErr := ts.a.sessions.SetUsage(ts.ctx, updatedSession.ID, updatedSession.PromptTokens, updatedSession.CompletionTokens); sessionErr != nil {
		return session.Session{}, sessionErr
	}
	// Per-message breakdown (task #469). The session-level figures
	// above are a last-snapshot overwrite plus a running cost, which
	// cannot answer "how well is the cache working" for a message, a
	// model, or a day. currentAssistant.ID is read under mu
	// because the checkpoint ticker and peak-hours watcher also touch
	// currentAssistant (same reason recordStepFinish locks).
	ts.mu.Lock()
	assistantID := ts.currentAssistant.ID
	ts.mu.Unlock()
	ts.a.recordMessageUsage(ts.ctx, assistantID, ts.smartModel, usage, costDelta, estimated)
	if usage.CacheCreationTokens > 0 {
		ts.a.scheduleCacheKeepAliveFromCtx(heartbeat.FromContext(ts.genCtx), ts.call.SessionID, ts.smartModel, ts.stepMessages, ts.stepTools, ts.call.ProviderOptions, ts.call.MaxCost)
	}
	ts.currentSession = updatedSession
	return updatedSession, nil
}

// enforceRunawayCaps implements Fork patch: batch 30 — cancel + runaway
// protection: the DB cancel flag (cross-process signal) and the cost/token
// caps. BUG-4 (full-project reviewer audit, 2026-08-11): these abort paths
// (max-cost, max-tokens; peak-hours is recheckPeakHours below) stop the
// turn ONLY via the cancelFunc looked up from activeRequests — returning an
// error from OnStepFinish alone does NOT break fantasy's loop. This is safe
// today ONLY because runTurn stores the turn's genCtx cancel via
// activeRequests.Set before the agent.Stream call whose OnStepFinish looks
// it up, and nothing ever calls activeRequests.Del for this key (entries
// live forever — see IsBusy's doc). Any future change that reclaims an
// activeRequests entry before the turn ends silently turns these aborts
// into no-ops: the error is returned but the turn keeps running. Pinned by
// TestActiveRequests_HoldsLiveCancelDuringTurn.
func (ts *turnStream) enforceRunawayCaps(updatedSession session.Session) error {
	canc, cancErr := ts.a.sessions.IsCancelRequested(ts.ctx, ts.call.SessionID)
	if cancErr != nil {
		// A failed read is NOT treated as a cancellation: a transient
		// DB error is no evidence the operator asked for one, and
		// aborting on it would turn every hiccup into what looks like
		// a user abort. But it must not be silent either — if the
		// operator did request a cancel and this is the read that
		// failed, the turn runs on with nothing in the log to say why
		// the request appeared to be ignored.
		slog.Warn("could not read the cancel-requested flag; continuing the turn",
			"session_id", ts.call.SessionID, "err", cancErr)
	}
	if cancErr == nil && canc {
		// Honoured: the request is spent, unless the `rush run` loop of this process
		// drives the session -- it reads the flag after the turn, ends the run
		// canceled and clears it itself (R8A-2).
		if ts.a.coord == nil || !ts.a.coord.driverHints.isExternalDriver(ts.call.SessionID) {
			clearCancelRequest(ts.ctx, ts.a.sessions, ts.call.SessionID)
		}
		if cancelFn, ok := ts.a.activeRequests.Get(ts.call.SessionID); ok {
			cancelFn()
		}
		return fmt.Errorf("session %s cancelled by user", ts.call.SessionID)
	}
	if ts.call.MaxCost > 0 {
		// #1130: the cap is the SUBTREE budget (own + delegation children,
		// minus the reset base) read from the DB — the same query the rush
		// run loop's stopError/capError run — not the in-memory row value,
		// which only ever covers this node.
		budget, budgetErr := ts.a.sessions.SubtreeBudget(ts.ctx, ts.call.SessionID)
		if budgetErr == nil && budget > ts.call.MaxCost {
			slog.Warn(
				"agent: aborting — max-cost exceeded",
				"session_id", ts.call.SessionID,
				"cost", budget,
				"max", ts.call.MaxCost,
			)
			if ts.att != nil {
				ts.att.capAbort.Store(true)
			}
			if cancelFn, ok := ts.a.activeRequests.Get(ts.call.SessionID); ok {
				cancelFn()
			}
			return fmt.Errorf("session %s aborted: cost $%.4f exceeds max $%.4f",
				ts.call.SessionID, budget, ts.call.MaxCost)
		}
		// A failed budget read is NOT treated as an abort: like the cancel
		// flag above, a transient DB error is no evidence the budget is
		// spent. The rush run loop re-checks on its own cadence.
	}
	totalTokens := updatedSession.PromptTokens + updatedSession.CompletionTokens
	if ts.call.MaxTokens > 0 && totalTokens > ts.call.MaxTokens {
		slog.Warn(
			"agent: aborting — max-tokens exceeded",
			"session_id", ts.call.SessionID,
			"tokens", totalTokens,
			"max", ts.call.MaxTokens,
		)
		if ts.att != nil {
			ts.att.capAbort.Store(true)
		}
		if cancelFn, ok := ts.a.activeRequests.Get(ts.call.SessionID); ok {
			cancelFn()
		}
		return fmt.Errorf("session %s aborted: %d tokens exceeds max %d",
			ts.call.SessionID, totalTokens, ts.call.MaxTokens)
	}
	return nil
}

// recheckPeakHours re-checks the provider's peak-hours window once per step
// (Fork patch). Peak-hours is normally only checked once, at the START of a
// turn (coordinator.buildCall/runInternal) — an already-in-flight turn was
// never re-checked, so a long turn that started before the window opened
// ran straight through it.
func (ts *turnStream) recheckPeakHours() error {
	if ts.a.peakHoursCheck == nil || peakHoursBypassed(ts.call) {
		return nil
	}
	pErr := ts.a.peakHoursCheck(ts.smartModel.ModelCfg.Provider)
	if pErr == nil {
		return nil
	}
	if ts.setPeakHoursAbortErr(pErr) {
		slog.Warn("agent: aborting — provider entered peak-hours mid-turn",
			"session_id", ts.call.SessionID, "error", pErr)
		peakMsg, peakDetails := peakHoursStoppedFinishText(pErr)
		ts.mu.Lock()
		ts.currentAssistant.AddFinish(message.FinishReasonError, peakMsg, peakDetails)
		snap := ts.currentAssistant.Clone()
		ts.mu.Unlock()
		// Use the parent ctx (not genCtx) for the DB write —
		// genCtx dies as soon as we cancel below.
		if uErr := ts.a.messages.Update(ts.ctx, snap); uErr != nil {
			slog.Warn("agent: failed to persist peak-hours finish message", "error", uErr)
		}
		if cancelFn, ok := ts.a.activeRequests.Get(ts.call.SessionID); ok {
			cancelFn()
		}
	}
	// Stash the specific error so Run() can return it AFTER fantasy's
	// agent.Stream exits. We must call cancelFn() to break fantasy's loop
	// (returning an error from OnStepFinish alone doesn't stop it), but
	// cancel() makes fantasy return context.Canceled — swallowing our
	// pErr. The stash lets Run() replace that generic error with the real
	// one.
	return pErr
}

// persistStepFinish is OnStepFinish's normal-path final write. Doc sec.3.4
// (DUR-4): when this step's finish carries real content (stepIsReaction),
// the write and the reaction-debt marker (reacted=1 on every currently
// wake=1/reacted=0/delivery='done' row of this session, across async_jobs
// AND session_notices) happen in ONE transaction via the store -- not two
// separate commits, and not derived from clock/created_at order. A pull
// only ever moves a row to delivery='done' during PrepareStep, before this
// step's own provider call, so every such row was already visible in the
// prompt that produced THIS step's content (see async_job_reaction.go's
// MarkReactedWithMessageUpdate doc for the argument in full) -- no separate
// "pulled before this step began" id set needs to be captured here.
func (ts *turnStream) persistStepFinish() error {
	ts.mu.Lock()
	snap := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	if stepIsReaction(snap) {
		// The batch one-transaction write marks the BATCH debt reacted
		// (batch_nodes + session_notices, reacted_failed cleared on both).
		// Nothing bound falls back to the plain update so the finish lands.
		if store := batchCutoverReactionStore(ts.a.coord); store != nil {
			return store.BatchReactionMarkReactedWithMessageUpdate(ts.genCtx, ts.a.messages, ts.call.SessionID, snap)
		}
	}
	return ts.a.messages.Update(ts.genCtx, snap)
}

// stepIsReaction reports whether a step's persisted finish counts as a
// reaction to any notice visible in its own prompt (doc sec.3.4): real
// content (text, a tool call, or reasoning) with a finish that is not
// error/canceled. An empty assistant stub, a failed step, or (by
// construction -- compaction never reaches persistStepFinish at all, see
// this file's own doc) a compaction summary never qualify.
func stepIsReaction(m message.Message) bool {
	reason := m.FinishReason()
	if reason == message.FinishReasonError || reason == message.FinishReasonCanceled {
		return false
	}
	return hasReactionContent(m)
}

// hasReactionContent reports whether m carries real content (text, a tool
// call or reasoning), whatever its finish.
func hasReactionContent(m message.Message) bool {
	return m.FullText() != "" || len(m.ToolCalls()) > 0 || m.FullReasoningText() != ""
}

func (ts *turnStream) stopConditions() []fantasy.StopCondition {
	return []fantasy.StopCondition{
		func(_ []fantasy.StepResult) bool {
			cw := int64(ts.smartModel.CatwalkCfg.ContextWindow)
			// If context window is unknown (0), skip auto-summarize
			// to avoid immediately truncating custom/local models.
			if cw == 0 {
				return false
			}
			tokens := ts.currentSession.CompletionTokens + ts.currentSession.PromptTokens
			remaining := cw - tokens
			var threshold int64
			if cw > largeContextWindowThreshold {
				threshold = largeContextWindowBuffer
			} else {
				threshold = int64(float64(cw) * smallContextWindowRatio)
			}
			if (remaining <= threshold) && !ts.a.disableAutoSummarize {
				ts.shouldSummarize = true
				return true
			}
			return false
		},
		func(steps []fantasy.StepResult) bool {
			// StopWhen runs AFTER OnStepFinish for the same step, so by the
			// time this executes, OnStepFinish has already appended to
			// stepHistory and recomputed loopDetected/loopDetail. We only
			// need to return the boolean here to tell fantasy to break the
			// loop — do NOT mutate loopDetected/loopDetail here, OnStepFinish
			// owns them (mutating here would race for the last step's
			// finish text and re-introduce the stale-flag bug).
			detected, _ := hasRepeatedToolCalls(steps, loopDetectionWindowSize, loopDetectionMaxRepeats)
			return detected
		},
		func(_ []fantasy.StepResult) bool {
			// In-turn progress guard (turn_progress_guard.go). Same ordering
			// as loopDetected above: recordStepHistory — not StopWhen — owns
			// stopped, so this only reads the flag for the step just
			// finished, never writes it.
			return ts.progress.stopped
		},
	}
}

// carriedSplice is one step boundary's mid-turn insertion: msgs landed at
// index pos of that step's options.Messages (initialPrompt + the step
// responses so far).
type carriedSplice struct {
	pos  int
	msgs []fantasy.Message
}

// spliceCarried builds a step's messages: base (fantasy's own
// initialPrompt + step responses) with every earlier carried splice
// re-inserted at its original position, then this step's own new splices
// at the end. base grows only by appending between steps, so an earlier
// pos still names the same boundary in a later step's base. Returns a
// fresh slice; base is never appended to in place.
func spliceCarried(base []fantasy.Message, carried []carriedSplice, stepSplices []fantasy.Message) []fantasy.Message {
	if len(carried) == 0 && len(stepSplices) == 0 {
		return base
	}
	n := len(base) + len(stepSplices)
	for _, cs := range carried {
		n += len(cs.msgs)
	}
	out := make([]fantasy.Message, 0, n)
	prev := 0
	for _, cs := range carried {
		pos := min(max(cs.pos, prev), len(base))
		out = append(out, base[prev:pos]...)
		out = append(out, cs.msgs...)
		prev = pos
	}
	out = append(out, base[prev:]...)
	return append(out, stepSplices...)
}
