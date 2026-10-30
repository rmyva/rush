// Turn execution: SessionAgentCall assembly, the 401-retry run loop, and
// transient-failure retry classification. Extracted from coordinator.go —
// pure code move, bodies unchanged.

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/hyper"
	"github.com/PHPCraftdream/rush/internal/agent/notify"
	"github.com/PHPCraftdream/rush/internal/heartbeat"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/google/uuid"
)

const (
	// streamStallRetriesDefault is the default Options.StreamStallRetries
	// when the config key is absent or zero. We default to 2 (3 total
	// attempts per turn) rather than 0 because the user-visible failure
	// mode of a single transient provider error is a hard turn-error that
	// the orchestrator then has to handle — silently absorbing 1-2
	// provider hiccups is almost always what an operator wants. This bounds
	// retries for ALL transient turn failures (stream stall, empty stream,
	// overload, 5xx, network), not just stalls.
	streamStallRetriesDefault = 2
	// streamStallRetryBackoffMultiplier shapes exponential backoff: 10s →
	// 30s → 90s. Long enough to let a rate-limit window roll over, short
	// enough to keep one turn under ~5 min including the prior watchdog
	// timeout.
	streamStallRetryBackoffMultiplier = 3.0
	// streamStalledFinishTitle is the canonical Message field that
	// agent.Run writes on a watchdog stall. Match against this exact
	// string when deciding whether to retry.
	//
	// CROSS-LANGUAGE SYNC: a fifth copy of this literal lives in the
	// web UI, since TypeScript can't import this Go constant. See the
	// `part.Message === "Stream stalled"` check in
	// web/src/components/Message.tsx — it renders a stalled
	// turn-after-partial-work as a soft amber StreamPausedBlock
	// instead of a red failure block. If this literal ever changes,
	// update that TS check too. The two are intentionally not wired
	// through the WS/JSON protocol (LOW severity; a comment
	// cross-reference is the agreed sync mechanism).
	streamStalledFinishTitle = "Stream stalled"
)

// streamStallRetryBaseBackoff is the first retry's backoff (10s in
// production). A var, not a const, so tests can shrink it instead of
// sleeping through the real delay.
var streamStallRetryBaseBackoff = 10 * time.Second

// buildCall assembles the SessionAgentCall for the current agent + model
// state. Extracted so InterruptAndSend can queue a call shaped exactly like
// runInternal would.
//
// pinned is ALWAYS non-nil: every caller resolves from the session DB or
// config defaults before calling buildCall. This ensures session-scoped model
// overrides are respected even on cross-process paths (e.g., interrupt requeue).
func (c *coordinator) buildCall(ctx context.Context, sessionID, prompt string, pinned *resolvedOverrides, attachments []message.Attachment) (SessionAgentCall, error) {
	if pinned == nil {
		return SessionAgentCall{}, errors.New("buildCall: pinned is required; caller must resolve session models first")
	}

	model := pinned.smart

	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}

	if !model.CatwalkCfg.SupportsImages && attachments != nil {
		filteredAttachments := make([]message.Attachment, 0, len(attachments))
		for _, att := range attachments {
			if att.IsText() {
				filteredAttachments = append(filteredAttachments, att)
			}
		}
		attachments = filteredAttachments
	}

	// pinned.providerCfg was resolved from the SAME snapshot pinned.smart
	// came from (task #341/P1-1) -- a live c.cfg.Config() read here would
	// reintroduce the torn-read this whole `pinned` threading exists to
	// close, since buildCall may run well after resolveSessionModels did
	// (e.g. for a queued replacement call, per the comment below).
	providerCfg := pinned.providerCfg
	if providerCfg.ID == "" {
		return SessionAgentCall{}, errModelProviderNotConfigured
	}
	// A per-call --allow-peak-hours waives the window here too.
	if opts := callOptionsFrom(ctx); opts == nil || !opts.AllowPeakHours {
		if err := checkPeakHours(providerCfg); err != nil {
			return SessionAgentCall{}, err
		}
	}

	mergedOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(sessionID, model, providerCfg)
	sessionSystemPrompt := c.sessionPromptForCall(ctx, sessionID, pinned)

	pinnedSmart := model
	// R1-1: carry whatever per-call options the arming context holds so a
	// queued replacement (InterruptAndSend) keeps its own policy when it
	// eventually starts, however long the queue delay is.
	callOpts := callOptionsFrom(ctx)
	if callOpts != nil {
		EnsureCallBuildScope(callOpts)
	}
	autoResumed, backgroundJobNotice := noticeFlagsFrom(ctx)

	call := SessionAgentCall{
		SessionID:            sessionID,
		Prompt:               prompt,
		Attachments:          attachments,
		MaxOutputTokens:      maxTokens,
		ProviderOptions:      mergedOptions,
		Temperature:          temp,
		TopP:                 topP,
		TopK:                 topK,
		FrequencyPenalty:     freqPenalty,
		PresencePenalty:      presPenalty,
		SystemPromptOverride: sessionSystemPrompt,
		// Preserve the accepted call's policy when this run is queued as a
		// mailbox follow-up and later executed under the owner's context.
		CallOptions: callOpts,
		// R3-4: this call's restricted-run policy, armed at turn start; the
		// uncompiled spec travels too so the durable queue can persist it.
		RunAllowlist:        runAllowlistFrom(ctx),
		RunAllowlistSpec:    runAllowlistSpecFrom(ctx),
		FolderScopeSpec:     folderScopeSpecFrom(ctx),
		SmartModel:          &pinnedSmart,
		LogicalCallID:       uuid.New().String(), // P2-1: generate stable ID once
		AutoResumed:         autoResumed,
		BackgroundJobNotice: backgroundJobNotice,
		IsDrain:             isDrainCallFrom(ctx),
	}
	// Stamp the entry-channel origin at BUILD time, not message-creation
	// time: the call may be queued as an InterruptAndSend replacement and
	// drained much later under a DIFFERENT turn's context, so the sender's
	// origin must travel on the call struct itself.
	call.Origin = CallOriginFrom(ctx)
	// R12-H2b §2.2: a run armed with an execution requirement envelope
	// stamps it onto the built call so durable producers can persist and
	// carry it (diskRequirementForCall reads this field first).
	if callOpts != nil && callOpts.Requirement != nil {
		call.DiskRequirement = callOpts.Requirement
	}
	// Pinning matters more here than in runInternal: this call is QUEUED as a
	// replacement and may not start for a while, so the gap between "options
	// computed" and "turn starts" is unbounded rather than a few lines.
	pinned.pin(&call)
	return call, nil
}

// runInternal executes the agent, handling 401 retries.
//
// pinned carries what the caller's resolveSessionModels just resolved.
// pinned is ALWAYS non-nil: every Run path resolves from the session DB or
// config defaults before calling runInternal. Everything below —
// maxOutputTokens, providerCfg, the peak-hours check, mergeCallOptions —
// is derived from ONE model value that also travels with the call, so the
// turn cannot end up running a different model than the options were
// computed for (task #265).
func (c *coordinator) runInternal(ctx context.Context, sessionID string, prompt string, pinned *resolvedOverrides, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	if pinned == nil {
		return nil, errors.New("runInternal: pinned is required; caller must resolve session models first")
	}

	// Re-check trigger (iv): a run on this session has now returned. A
	// delegated sub-agent's completion is parked while its session is
	// mid-turn (see subAgentWorkTerminal's negative busy gate), so this is
	// where a deferred release is guaranteed to be re-evaluated. It covers
	// child auto-resume turns started by notifyAsyncCompletion /
	// notifyBackgroundJobDone, which no other trigger observes.
	//
	// MUST NOT hold anything: it only re-evaluates a release gate, and the
	// child's mailbox and OS lock are already released by this point (see
	// runOwned's deferred abandonOwnershipWithHandoff).
	defer c.noteSubAgentChildRunEnded(sessionID)

	// R8-3: capture the DIRECT caller's call-result recorder, then detach
	// it for everything downstream -- a nested runInternal reached through
	// this turn's tools (a sub-agent's child session shares ctx) must never
	// overwrite the top-level caller's own captured ID with a child
	// session's unrelated message.
	callResult := callResultRecorderFrom(ctx)
	callResult.BeginCall()
	callResultRecord := callResult.Capture()
	executionIdentityRecord := session.ExecutionAssistantIdentityRecorder(ctx)
	ctx = withoutCallResultRecorder(ctx)
	ctx = session.WithoutExecutionAssistantIdentity(ctx)

	model := pinned.smart

	// The heartbeat source labels are decided at resolve time, not here.
	ctx = withModelSource(ctx, pinned.smartSource, pinned.fastSource)

	// creds is non-nil for RunWithCredentials calls (see
	// resolvedOverrides.credentials): the 401 rebuild below must
	// re-resolve through the SAME per-call credentials, not the config.
	creds := pinned.credentials
	// callOpts is non-nil when the caller armed this run via
	// WithCallOptions (ExecuteRun, R1-1): every policy read below then
	// comes from THIS call's immutable options instead of the coordinator's
	// shared Set*-state, which a concurrent Run call can rewrite between
	// the Set and this read. nil = legacy caller — the fallback paths keep
	// the historical read-and-reset behavior byte-for-byte.
	callOpts := callOptionsFrom(ctx)
	// R12-H2b §2.4 ordinary/idle rebuilt calls — do not rely on the
	// request carrying options: a session with a persisted fence must not
	// run a turn that cannot name a live binding. Before any
	// provider-sensitive model/tool resolution.
	if err := c.guardTurnDiskFence(ctx, sessionID, callOpts); err != nil {
		return nil, c.drainRefused(ctx, sessionID, err)
	}
	// R12-H2a (P1-1): the NORMAL run path builds its tools into this call's
	// build-resource scope (resolveSessionModels → pinCallTools → buildTools
	// → newBatchWorkToolBuild registers construction pins in the scope), and
	// nothing below closes it. Release it on EVERY return from runInternal —
	// success, error and panic. Closing is idempotent, so a nested
	// runInternal that shares the same CallOptions pointer closes the same
	// scope harmlessly. Known accepted consequence: an InterruptAndSend
	// queued call copied these CallOptions (buildCall above) and inherits
	// this scope pointer; if it is drained after this run closed the scope,
	// its stale tools fail closed at the executor (RegistryID unknown). That
	// is the intended fail-closed direction — a drained queued call rebuilds
	// under RunSessionAgentCall's own fresh scope.
	if callOpts != nil {
		defer callOpts.buildScope.close()
	}
	// A human-initiated turn (web/SDK: no per-call options, not a Drain) spends
	// a stale `sessions cancel` request AT ADMISSION (#1101, R8A-2): runOwned
	// runs the callback once the mailbox and session lock are ours, so a turn
	// refused by another owner leaves the flag for that owner. The `rush run`
	// loop's own turns (per-call options) and Drains leave it to the loop.
	if callOpts == nil && !isDrainCallFrom(ctx) {
		sessions := c.sessions
		previous := admissionSetupFrom(ctx)
		ctx = WithAdmissionSetup(ctx, func(ctx context.Context) error {
			clearCancelRequest(ctx, sessions, sessionID)
			// Preserve any setup installed by the caller.
			if previous != nil {
				return previous(ctx)
			}
			return nil
		})
	}
	slog.Debug("Coordinator: running with model", "sessionID", sessionID, "model", model.ModelCfg.Model)

	maxOutputTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxOutputTokens = model.ModelCfg.MaxTokens
	}

	if !model.CatwalkCfg.SupportsImages && attachments != nil {
		filteredAttachments := make([]message.Attachment, 0, len(attachments))
		for _, att := range attachments {
			if att.IsText() {
				filteredAttachments = append(filteredAttachments, att)
			}
		}
		attachments = filteredAttachments
	}

	// pinned.providerCfg was resolved from the SAME snapshot pinned.smart
	// came from (task #341/P1-1) -- reuse it rather than a second, live
	// c.cfg.Config() read, which would reopen the exact torn-read gap the
	// 401-rebuild path below already fixed, on the far more common
	// (every-run, not just every-401) path.
	providerCfg := pinned.providerCfg
	if providerCfg.ID == "" {
		return nil, c.drainRefused(ctx, sessionID, errModelProviderNotConfigured)
	}
	// Fork patch (peak-hours bypass): consume the one-shot allow flag
	// armed by SetAllowPeakHours (`rush run --allow-peak-hours`). Reset
	// immediately so a subsequent Run on the same coordinator does not
	// inherit the bypass. R1-1: a per-call CallOptions value wins — the
	// shared flag is neither read nor consumed, so a concurrent run
	// arming/clearing it can no longer flip THIS run's bypass decision.
	var allowPeak bool
	if callOpts != nil {
		allowPeak = callOpts.AllowPeakHours
	} else {
		c.runLimitsMu.Lock()
		allowPeak = c.allowPeakHours
		c.allowPeakHours = false
		c.runLimitsMu.Unlock()
	}
	if !allowPeak {
		if err := checkPeakHours(providerCfg); err != nil {
			return nil, c.drainRefused(ctx, sessionID, err)
		}
	}

	mergedOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(sessionID, model, providerCfg)

	if refreshed, err := c.refreshExpiredToken(ctx, providerCfg); err != nil {
		// NOTE(@andreynering): We don't return here because the event handling to ask the user to reauthenticate
		// depends on the flow below. If refresh fails, proceed with the token we have.
		slog.Error("Failed to refresh OAuth2 token. Proceeding with existing token.", "error", err)
	} else if refreshed && creds == nil {
		// The model above was resolved BEFORE the refresh, so its client still
		// carries the old token: rebuild it on the new one (same model). A
		// Drain gets exactly one attempt, so it must not spend it on the
		// stale client.
		if fresh, rebuildErr := c.rebuildOnCurrentCredentials(ctx, model, false); rebuildErr != nil {
			slog.Warn("Could not rebuild the model client after a token refresh; proceeding with the old client", "provider", providerCfg.ID, "error", rebuildErr)
		} else {
			model = fresh
			pinned.smart = fresh
		}
	}

	sessionSystemPrompt := c.sessionPromptForCall(ctx, sessionID, pinned)

	// Fork patch: batch 30 — per-run limits, pass through to the agent.
	// R1-1: with a per-call CallOptions the caps are this call's own and
	// the shared SetRunLimits state is left untouched (no read, no reset);
	// otherwise the legacy atomic read-and-reset applies unchanged.
	var maxCost float64
	var maxTokensRunLimit int64
	if callOpts != nil {
		maxCost = callOpts.MaxCost
		maxTokensRunLimit = callOpts.MaxTokens
	} else {
		c.runLimitsMu.Lock()
		maxCost = c.maxCost
		c.maxCost = 0
		maxTokensRunLimit = c.maxTokens
		c.maxTokens = 0
		c.runLimitsMu.Unlock()
	}

	// Pin the model this call already resolved (task #265). Everything above
	// — maxOutputTokens, mergedOptions, temp/topP/topK/penalties — was
	// derived from THIS `model` value. Without pinning it, the agent
	// re-reads its shared smartModel when the turn actually starts, so a
	// concurrent session's applyModelOverrides landing in between makes the
	// turn run one model with another model's options. Passing it down keeps
	// the call internally consistent.
	pinnedSmart := model
	// createdUserMessageID captures the ID createUserMessage assigns the
	// first time this call actually creates a user message (nil
	// ExistingMessageID). The transient-retry loop below feeds it back in
	// as ExistingMessageID on each retry so a provider hiccup re-runs the
	// SAME turn instead of persisting the prompt as a fresh message every
	// attempt (previously: operator sees their own message duplicated once
	// per retry).
	var createdUserMessageID string
	// curAttempt is the CURRENT run() attempt's evidence capture target
	// (coordinator_attempt_evidence.go). R3-1 (round 6): every attempt
	// gets a FRESH instance via armAttempt below; classification acts
	// only on the row the classified attempt itself reported.
	curAttempt := newAttemptEvidence()
	var sealCurrentAttemptIdentity func()
	// attemptAdmission is the CURRENT attempt's ADMISSION outcome (R7-1):
	// did this run() invocation execute a turn, or merely queue behind a
	// current owner? Deliberately separate from attemptEvidence -- a
	// queued call owns no retry classification at all, whatever its
	// queued copy's callback recorded before this sender's resolve()
	// sealed the capture.
	attemptAdmission := newTurnAdmission()
	autoResumed, backgroundJobNotice := noticeFlagsFrom(ctx)
	agentCall := SessionAgentCall{
		SessionID:            sessionID,
		Prompt:               prompt,
		Attachments:          attachments,
		MaxOutputTokens:      maxOutputTokens,
		ProviderOptions:      mergedOptions,
		Temperature:          temp,
		TopP:                 topP,
		TopK:                 topK,
		FrequencyPenalty:     freqPenalty,
		PresencePenalty:      presPenalty,
		SystemPromptOverride: sessionSystemPrompt,
		MaxCost:              maxCost,
		MaxTokens:            maxTokensRunLimit,
		// R1-1/R1-4: per-call policy travels with the call — the caps the
		// turn enforces, this run's full options snapshot, and the
		// fail-fast busy contract enforced at the mailbox reservation.
		CallOptions: callOpts,
		// R3-4: this call's restricted-run policy, armed at turn start; the
		// uncompiled spec travels too so the durable queue can persist it.
		RunAllowlist:         runAllowlistFrom(ctx),
		RunAllowlistSpec:     runAllowlistSpecFrom(ctx),
		FolderScopeSpec:      folderScopeSpecFrom(ctx),
		FailIfSessionBusy:    callOpts != nil && callOpts.FailIfSessionBusy,
		SmartModel:           &pinnedSmart,
		Credentials:          creds,
		LogicalCallID:        uuid.New().String(), // P2-1: generate stable ID once
		AutoResumed:          autoResumed,
		BackgroundJobNotice:  backgroundJobNotice,
		IsDrain:              isDrainCallFrom(ctx),
		OnUserMessageCreated: func(id string) { createdUserMessageID = id },
		// R3-1: the CURRENT attempt's sink (for multi-step or
		// 401-retried runs the LAST prepared step wins). armAttempt
		// swaps a fresh one in before every run(), and each
		// SessionAgentCall copy keeps only the sink it was armed with --
		// a queued copy drained by another goroutine can never write a
		// later attempt's evidence.
		OnAssistantMessageCreated: curAttempt.record,
		// Stamp the entry-channel origin (see buildCall): createUserMessage
		// persists it on the user message this turn creates.
		Origin: CallOriginFrom(ctx),
	}
	// Overrides pin fast model / prefix / base prompt too; pin() leaves
	// SmartModel as set above when pinned is nil, and rewrites it to the same
	// value when it isn't (model was taken FROM pinned.smart).
	pinned.pin(&agentCall)

	// trackCall is a pointer to the current call so we can rebuild it after
	// a 401 credential refresh (task #341, P1-2).
	trackCall := &agentCall
	run := func() (*fantasy.AgentResult, error) {
		result, err := c.currentAgent.Run(withTurnAdmission(ctx, attemptAdmission), *trackCall)
		if sealCurrentAttemptIdentity != nil {
			sealCurrentAttemptIdentity()
		}
		return result, err
	}

	// armAttempt installs a FRESH per-attempt evidence capture into the
	// current call (R3-1, round 6); it must run immediately before EVERY
	// run() invocation -- the initial one and each retry below (the
	// runWithUnauthorizedRetry fn arms itself too, covering the 401
	// path, whose rebuildCall preserves the current attempt's sink).
	// resolve() the armed instance synchronously once run() returns:
	// later recordings are async dispatches of older queued copies.
	// It also re-arms the admission recorder so every attempt reports its
	// own admission outcome (R7-1).
	armAttempt := func() {
		attempt := newAttemptEvidence()
		curAttempt = attempt
		attemptAdmission = newTurnAdmission()
		admission := attemptAdmission
		var identityMu sync.Mutex
		identityOpen := true
		sealAttemptIdentity := func() {
			identityMu.Lock()
			identityOpen = false
			identityMu.Unlock()
		}
		trackCall.OnAssistantMessageCreated = func(id string) {
			attempt.record(id)
			callResultRecord(id)
			if admission.wasQueued() {
				return
			}
			identityMu.Lock()
			if identityOpen {
				executionIdentityRecord(id)
			}
			identityMu.Unlock()
		}
		sealCurrentAttemptIdentity = sealAttemptIdentity
	}

	// rebuildCall reconstructs the call after credential refresh, preserving
	// the logical request ID and other fields but using fresh models with new
	// credentials (task #341, P1-2).
	rebuildCall := func() error {
		// Resolve fresh models with updated credentials. A per-call
		// CredentialSet (RunWithCredentials) re-resolves through the SAME
		// credentials — resolveSessionModels here would rebuild the retry
		// on the config provider, sending a rejected tenant key's retry
		// to the operator's own provider.
		pinned, err := c.resolveCallModels(ctx, sessionID, creds)
		if err != nil {
			return fmt.Errorf("failed to resolve models after credential refresh: %w", err)
		}

		// Rebuild the call with the new models, preserving all logical fields.
		model := pinned.smart
		maxOutputTokens := model.CatwalkCfg.DefaultMaxTokens
		if model.ModelCfg.MaxTokens != 0 {
			maxOutputTokens = model.ModelCfg.MaxTokens
		}

		// Re-filter attachments for the new model's image support.
		if !model.CatwalkCfg.SupportsImages && attachments != nil {
			filteredAttachments := make([]message.Attachment, 0, len(attachments))
			for _, att := range attachments {
				if att.IsText() {
					filteredAttachments = append(filteredAttachments, att)
				}
			}
			attachments = filteredAttachments
		}

		// Use the provider config resolveSessionModels already resolved from
		// the SAME snapshot it built `model` from above (task #341, P1-1).
		// This used to take a SEPARATE, freshly-timed c.cfg.Snapshot() here
		// — despite the old comment claiming that was "consistent with
		// resolveSessionModels above", a reload landing in the gap between
		// resolveSessionModels returning and this second Snapshot() call
		// could hand back provider options/credentials from a DIFFERENT
		// generation than the model pinned.smart was built from, i.e.
		// exactly the torn-read this whole rebuildCall path exists to
		// avoid. pinned.providerCfg removes the second snapshot entirely.
		providerCfg := pinned.providerCfg
		if providerCfg.ID == "" {
			return errModelProviderNotConfigured
		}

		mergedOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(sessionID, model, providerCfg)

		pinnedSmart := model
		newCall := SessionAgentCall{
			SessionID:            trackCall.SessionID,
			Prompt:               trackCall.Prompt,
			Attachments:          attachments,
			MaxOutputTokens:      maxOutputTokens,
			ProviderOptions:      mergedOptions,
			Temperature:          temp,
			TopP:                 topP,
			TopK:                 topK,
			FrequencyPenalty:     freqPenalty,
			PresencePenalty:      presPenalty,
			SystemPromptOverride: trackCall.SystemPromptOverride,
			MaxCost:              trackCall.MaxCost,
			MaxTokens:            trackCall.MaxTokens,
			CallOptions:          trackCall.CallOptions,
			// Preserve the per-call restricted-run policy across the
			// credential-refresh rebuild: a retried call must stay judged
			// by its own declared policy, not fall back to the session or
			// process-wide gate.
			RunAllowlist:              trackCall.RunAllowlist,
			RunAllowlistSpec:          trackCall.RunAllowlistSpec,
			FolderScopeSpec:           trackCall.FolderScopeSpec,
			FailIfSessionBusy:         trackCall.FailIfSessionBusy,
			SmartModel:                &pinnedSmart,
			Credentials:               trackCall.Credentials,
			LogicalCallID:             trackCall.LogicalCallID, // Preserve logical ID
			ExistingMessageID:         trackCall.ExistingMessageID,
			OnUserMessageCreated:      trackCall.OnUserMessageCreated,
			OnAssistantMessageCreated: trackCall.OnAssistantMessageCreated,
			InjectID:                  trackCall.InjectID,
			FromDurableQueue:          trackCall.FromDurableQueue,
			Origin:                    trackCall.Origin,
			AutoResumed:               trackCall.AutoResumed,
			BackgroundJobNotice:       trackCall.BackgroundJobNotice,
		}
		pinned.pin(&newCall)
		*trackCall = newCall
		return nil
	}

	// Interrupt-inject ticker: watches pending_injects for interrupt=true rows
	// written by `rush sessions inject --interrupt` in another process, and
	// (on the first hit) cancels the running turn and requeues the referenced
	// message so it picks up immediately. Bound to this turn's lifetime via
	// tickerCtx — stopped by the defer as soon as run() returns, so no
	// idle-process polling. Runs for BOTH the initial turn and every retry
	// re-run below (each run() sees a fresh ticker via this closure). The
	// defer ensures the ticker goroutine has joined before runInternal returns.
	tickerCtx, stopTicker := context.WithCancel(ctx)
	tickerDone := c.startInterruptTicker(tickerCtx, sessionID)
	// defers run LIFO: stopTicker (cancel tickerCtx) must fire before we
	// wait on tickerDone, or the join blocks forever waiting for a
	// goroutine that's still parked on <-ctx.Done(). Combine both into one
	// deferred func so the order is explicit and can't be silently
	// reordered by a future defer inserted between the two.
	defer func() {
		stopTicker()
		<-tickerDone
	}()

	beforeLoaded := c.skillTracker.LoadedNames()
	var result *fantasy.AgentResult
	var originalErr error
	if agentCall.IsDrain {
		// A Drain is ONE provider attempt (attempts design 1.6): no 401
		// rebuild-and-retry leg here and no transient retry below -- the
		// attempt is accounted by the turn loop and retried by the launch
		// gate's pace. A 401 refreshes the credentials in that accounting
		// (refreshAfterUnauthorized), before it decides, not here after it.
		armAttempt()
		result, originalErr = run()
		if errors.Is(originalErr, ErrAgentShuttingDown) {
			originalErr = c.drainRefused(ctx, sessionID, originalErr)
		}
	} else {
		originalErr = c.runWithUnauthorizedRetry(ctx, providerCfg, func() error {
			var err error
			armAttempt()
			result, err = run()
			return err
		}, rebuildCall)
	}
	// Only what THIS attempt's own turn reported before run() returned
	// counts. A queued admission leaves it empty UNLESS the queued copy's
	// dispatch fired the callback before this resolve() sealed it (R7-1);
	// the retry loop's admission gate below refuses to classify any such
	// ID. resolve() itself is unconditional so late writes stay dropped
	// (round-6 invariant).
	attemptAssistantMsgID := curAttempt.resolve()
	// R8-3: report whatever this attempt resolved to the caller's
	// call-result recorder (if armed) on every path out of runInternal from
	// here on, including the ctx.Done() early return in the retry loop
	// below. The defer re-reads attemptAssistantMsgID at return time, not
	// now, so a later retry attempt's reassignment (below) is what actually
	// gets reported.
	if callResult != nil {
		defer func() { callResultRecord(attemptAssistantMsgID) }()
	}
	if !attemptAdmission.wasQueued() {
		defer func() { executionIdentityRecord(attemptAssistantMsgID) }()
	}
	logTurnSkillUsage(sessionID, prompt, c.activeSkills, c.skillTracker, beforeLoaded)

	// Notify only if still unauthorized after retry — a successful
	// retry means the user doesn't need to re-authenticate. A Drain's 401
	// was refreshed, or found terminal, by its own accounting, which also
	// publishes this notification when it stays terminal.
	if originalErr != nil && !agentCall.IsDrain && c.isUnauthorized(originalErr) && c.notify != nil && model.ModelCfg.Provider == hyper.Name {
		c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			Type:       notify.TypeReAuthenticate,
			ProviderID: model.ModelCfg.Provider,
		})
	}

	// Auto-retry on transient provider failures. The agent may have written
	// a FinishReasonError message (stream stall, empty stream) or returned a
	// transient error (429 overload, 5xx, GOAWAY/EOF, network drop). There
	// are two distinct recovery shapes, picked per-attempt by
	// shouldContinueTurn/shouldRetryTurn:
	//
	//   - NO content produced yet: blind resend of the SAME prompt, reusing
	//     the user message row (shouldRetryTurn / createdUserMessageID
	//     below) so nothing is duplicated.
	//   - Partial content already produced (the common real-world case: a
	//     watchdog stall or rate-limit hits mid-stream, after the model has
	//     already written text/reasoning/a tool call) — a blind resend would
	//     either duplicate that work or discard it, so shouldRetryTurn
	//     deliberately refuses to retry here. Instead we send a
	//     CONTINUATION prompt (see continuationPrompt) as a fresh turn: the
	//     partial assistant message stays in history untouched, and the
	//     model is asked to pick up where it left off.
	//
	// Both paths share the same transient/terminal classification
	// (classifyProviderError, isQuotaLimit) and the same backoff schedule.
	// "Solve it ourselves before bothering the user" — provider hiccups
	// (rate limits, HTTP/2 stalls, brief capacity drops) usually clear
	// within tens of seconds, so 2 retries after 10s + 30s of backoff
	// absorb the common cases without the orchestrator having to know.
	//
	// R3-1: a retry decision must never be driven by evidence this call
	// did not create. An admission refusal (ErrSessionBusy,
	// ErrAgentShuttingDown, OS session-lock busy) means no provider turn
	// ever started, and the session's last assistant row may have been
	// written by a concurrent caller whose turn interleaved after ours
	// returned — a newer ID proves nothing about ownership. So the
	// classifiers act ONLY on the assistant row THIS call's own attempt
	// wrote, identified by the OnAssistantMessageCreated callback
	// (attemptAssistantMsgID below); the session-wide last row is never
	// consulted.
	maxRetries := streamStallRetriesDefault
	if opts := c.cfg.Config().Options; opts != nil && opts.StreamStallRetries != nil {
		// Explicit override (including explicit 0 to disable entirely).
		maxRetries = *opts.StreamStallRetries
		if maxRetries < 0 {
			maxRetries = 0
		}
	}
	if agentCall.IsDrain {
		maxRetries = 0 // one attempt, see above
	}
	rateLimitWaits := 0
	var rateLimitSpent time.Duration
	for attempt := 1; ; {
		// R7-1: ADMISSION OUTCOME gates retry classification, checked
		// BEFORE any attemptEvidence consultation. A queued call never
		// executed a turn: originalErr's nil is the queueing return, not
		// an executed attempt's success, and the evidence ID resolved
		// above may belong to the queued copy's own dispatch -- a
		// different goroutine executed it under the then-current owner
		// and fired its OnAssistantMessageCreated before this sender
		// sealed the capture. A terminal failure on that dispatch (a
		// plain 400 with an error-finish no-progress row) must NOT be
		// retried from here: the classifiers would read "no error,
		// error-finish row" as retryable and re-issue a second provider
		// call after a genuine 400. Queued means nothing to retry from
		// this invocation; the queued turn's outcome belongs to whoever
		// executed it.
		if attemptAdmission.wasQueued() {
			break
		}
		partial, isContinuation := c.shouldContinueTurn(ctx, sessionID, originalErr, attemptAssistantMsgID)
		if isContinuation {
			// Fresh follow-up prompt, fresh user message: the partial
			// assistant message is left exactly as it is in history, and
			// createdUserMessageID is intentionally NOT reused here — the
			// continuation is a new logical turn, not an edit of the
			// original request.
			trackCall.Prompt = continuationPrompt(prompt, partial)
			trackCall.ExistingMessageID = ""
		} else {
			if !c.shouldRetryTurn(ctx, sessionID, originalErr, attemptAssistantMsgID) {
				break
			}
			// Reuse the user message the first attempt already created
			// instead of letting the retry's runTurn create a fresh one --
			// otherwise the operator sees their own prompt duplicated once
			// per retry. createdUserMessageID is empty only if
			// OnUserMessageCreated never fired (e.g. the call already
			// carried an ExistingMessageID of its own), in which case
			// there's nothing to overwrite.
			if createdUserMessageID != "" {
				trackCall.ExistingMessageID = createdUserMessageID
			}
		}
		if isRateLimitError(originalErr) {
			// A non-quota 429 retries on its OWN budget: Retry-After when
			// the provider sent one, else exponential, never counted
			// against maxRetries. Explicit stream_stall_retries=0 (and a
			// Drain) disables this wait too.
			if maxRetries == 0 {
				break
			}
			wait := rateLimitWaitFor(originalErr, rateLimitWaits)
			deadline := time.Now().Add(wait)
			slog.Warn(
				"coordinator: provider rate limit, waiting before next attempt",
				"session_id", sessionID,
				"attempt", rateLimitWaits+1,
				"wait", wait.String(),
				"wait_until", deadline.Format("15:04"),
			)
			c.notifyRateLimitWaitContext(ctx, sessionID, deadline, rateLimitWaits+1)
			heartbeat.SetRateLimitedUntilFor(sessionID, deadline)
			err := c.waitRateLimit(ctx, sessionID, wait, &rateLimitSpent)
			heartbeat.SetRateLimitedUntilFor(sessionID, time.Time{})
			if err != nil {
				if errors.Is(err, errRateLimitBudgetExhausted) {
					break
				}
				return result, ctx.Err()
			}
			rateLimitWaits++
		} else {
			if attempt > maxRetries {
				break
			}
			backoff := streamStallRetryBaseBackoff
			for i := 1; i < attempt; i++ {
				backoff = time.Duration(float64(backoff) * streamStallRetryBackoffMultiplier)
			}
			slog.Warn(
				"coordinator: retrying transient turn failure",
				"session_id", sessionID,
				"attempt", attempt+1,
				"max_attempts", maxRetries+1,
				"backoff", backoff.String(),
				"continuation", isContinuation,
			)
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(backoff):
			}
			attempt++
		}
		// Fresh capture target for the next attempt (R3-1, round 6);
		// this attempt's instance is sealed by the resolve() below.
		armAttempt()
		result, originalErr = run()
		attemptAssistantMsgID = curAttempt.resolve()
	}

	return result, originalErr
}

// RunWithOverrides implements Coordinator. It is like Run but uses the given
// smart/fast model overrides instead of the global config defaults.
func (c *coordinator) RunWithOverrides(ctx context.Context, sessionID, prompt string, smart, fast *ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	// R12-H2a round 2: resolution builds this call's toolset into the
	// trusted call state's build-resource scope before runInternal runs, so
	// every return from here — resolution failure included — must release
	// it. Close is idempotent; runInternal's own defer double-closes
	// harmlessly. Captured from the ORIGINAL ctx: the later
	// WithModelOverrides re-arm carries the same CallOptions pointer.
	if callOpts := callOptionsFrom(ctx); callOpts != nil {
		defer callOpts.buildScope.close()
	}
	if err := c.readyWg.Wait(); err != nil {
		return nil, c.drainRefused(ctx, sessionID, err)
	}

	// Carry session-level reasoning effort into the overrides so that
	// applyModelOverrides restores it after resetting the model config.
	if WebModelPolicy(ctx) {
		if sess, err := c.sessions.Get(ctx, sessionID); err == nil {
			// An omitted slot inherits the target session's durable override. This
			// keeps a one-slot ExecuteRun override from silently switching the other
			// slot back to the process default.
			if smart == nil && sess.SmartModelID != "" {
				smart = &ModelOverride{Provider: sess.SmartModelProvider, Model: sess.SmartModelID}
			}
			if fast == nil && sess.FastModelID != "" {
				fast = &ModelOverride{Provider: sess.FastModelProvider, Model: sess.FastModelID}
			}
			inheritSessionEffort(smart, fast, sess)
		}
	}

	// R9-2 (round-9 audit): the session-inheritance fill-in above lives on
	// this function's own LOCAL smart/fast variables. Without re-arming
	// ctx, resolveCallModels' 401-rebuild path (credentials.go) reads
	// modelOverridesFrom(ctx) -- whatever the CALLER originally armed via
	// WithModelOverrides, e.g. the reviewer pass's (reviewerOverride, nil)
	// -- and never sees this inheritance step at all. A single omitted
	// slot then silently reset from the session's durable choice to the
	// global config default on every credential-refresh rebuild, even
	// though the FIRST attempt (built from the augmented pair right below)
	// used the correct one. Re-arming with the augmented pair makes both
	// resolutions agree.
	ctx = WithModelOverrides(ctx, smart, fast)

	pinned, err := c.applyModelOverrides(ctx, sessionID, smart, fast)
	if err != nil {
		return nil, c.drainRefused(ctx, sessionID, err)
	}

	return c.runInternal(ctx, sessionID, prompt, pinned, attachments...)
}

// inheritSessionEffort fills an override slot's empty reasoning effort from
// the session's durable effort, but only when the slot names the session's
// own model: the effort was recorded for that model, and applyModelOverrides
// deliberately clears the effort whenever the model differs.
func inheritSessionEffort(smart, fast *ModelOverride, sess session.Session) {
	if smart != nil && smart.ReasoningEffort == "" && sess.SmartModelReasoningEffort != "" &&
		smart.Model == sess.SmartModelID && smart.Provider == sess.SmartModelProvider {
		smart.ReasoningEffort = sess.SmartModelReasoningEffort
	}
	if fast != nil && fast.ReasoningEffort == "" && sess.FastModelReasoningEffort != "" &&
		fast.Model == sess.FastModelID && fast.Provider == sess.FastModelProvider {
		fast.ReasoningEffort = sess.FastModelReasoningEffort
	}
}

// ReserveExclusive implements Coordinator — see the interface doc and
// SessionAgent.ReserveExclusive for the full contract. Thin passthrough:
// the reservation itself is pure mailbox state, nothing model/provider
// related needs resolving to claim it.
func (c *coordinator) ReserveExclusive(ctx context.Context, sessionID string) (holdCtx context.Context, epoch uint64, cancel context.CancelFunc, ok bool) {
	return c.currentAgent.ReserveExclusive(ctx, sessionID)
}

// ReleaseExclusive implements Coordinator — thin passthrough, see
// SessionAgent.ReleaseExclusive.
func (c *coordinator) ReleaseExclusive(sessionID string, epoch uint64, cancel context.CancelFunc) {
	c.currentAgent.ReleaseExclusive(sessionID, epoch, cancel)
}

// RunWithReservedOwnership implements Coordinator. It resolves models
// exactly like Run/RunWithOverrides (buildCall, the same shared path every
// other entry point uses) and then hands the call to
// SessionAgent.RunWithReservedOwnership instead of SessionAgent.Run, so the
// caller's already-held reservation (holdCtx, epoch, cancel from a prior
// ReserveExclusive) is CONTINUED rather than dropped and re-claimed. See
// SessionAgent.RunWithReservedOwnership's doc for why that distinction is
// the entire point: re-claiming would race a concurrent caller for the
// instant the mailbox is idle in between.
//
// THE SINGLE HANDOFF POINT at this layer is the same shape as
// SessionAgent.RunWithReservedOwnership's: exactly one line —
// `return c.currentAgent.RunWithReservedOwnership(...)` at the bottom —
// transfers release-responsibility down into the sessionAgent layer (whose
// own single handoff line then transfers it into runOwned's defer). Every
// return ABOVE that line in THIS function is this function's own
// responsibility to release via c.currentAgent.ReleaseExclusive before
// returning (findings review, task #614 defect 1: readyWg.Wait, model
// resolution, and buildCall failures previously fell through with no
// release at all — resolveSessionModels in particular fails on an ordinary
// misconfigured/unresolvable model, not just shutdown, so this was reachable
// on a routine error and would wedge the session at mbOwned forever, worse
// than the F5 race it was meant to close). There is no epoch-mismatch
// exception at this layer the way there is one line down in
// SessionAgent.RunWithReservedOwnership: nothing between ReserveExclusive
// returning to the caller and this function running can move the epoch on
// its own (only Cancel/CancelAll/a concurrent ReserveExclusive attempt that
// FAILS can touch it, and none of those end this era) — so every early
// return here really is this function's reservation to release.
//
// onHandoff, if non-nil, is passed through to the agent layer and invoked
// immediately before the handoff to runOwned. It is used by callers (e.g.
// handleRerunMessage) to transfer release responsibility exactly when the
// handoff occurs.
//
// R12-H2a round 2: model resolution here builds the call's toolset into
// the trusted call state's build-resource scope (via buildCall and
// resolveSessionModels/applyModelOverrides -> pinCallTools). The defer
// added below the readyWg.Wait closes that scope on every return — early
// errors included — and on the final handoff return, which is safe because
// this function returns only after the whole reserved run completes.
func (c *coordinator) RunWithReservedOwnership(ctx context.Context, sessionID, prompt string, epoch uint64, cancel context.CancelFunc, onHandoff func(), smart, fast *ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	// R8-3 residual (round-9 audit): this entry point never went through
	// runInternal, so it never reported into the caller's CallResultRecorder
	// -- an empty recorder made internal/app's terminal reconciliation fall
	// back to "any newer row is mine", the exact R8-3 race the recorder
	// exists to close. Capture and detach here too, mirroring runInternal's
	// own top -- detach so a nested Run reached through this turn's tools
	// (sub-agent delegation) cannot report into this call's recorder.
	callResult := callResultRecorderFrom(ctx)
	callResult.BeginCall()
	callResultRecord := callResult.Capture()
	executionIdentityRecord := session.ExecutionAssistantIdentityRecorder(ctx)
	ctx = withoutCallResultRecorder(ctx)
	ctx = session.WithoutExecutionAssistantIdentity(ctx)

	// R12-H2a round 2: resolution builds this call's toolset into the
	// trusted call state's build-resource scope before the reserved run
	// starts, so every return from here — resolution failure included —
	// must release it. This function returns only after the whole reserved
	// run completes, so the defer fires at true call end. Close is
	// idempotent.
	if callOpts := callOptionsFrom(ctx); callOpts != nil {
		defer callOpts.buildScope.close()
	}
	if err := c.readyWg.Wait(); err != nil {
		c.currentAgent.ReleaseExclusive(sessionID, epoch, cancel)
		return nil, err
	}

	// Invocation-only model selections must not inherit durable session pins.
	if WebModelPolicy(ctx) {
		if sess, getErr := c.sessions.Get(ctx, sessionID); getErr == nil {
			if smart == nil && sess.SmartModelID != "" {
				smart = &ModelOverride{Provider: sess.SmartModelProvider, Model: sess.SmartModelID}
			}
			if fast == nil && sess.FastModelID != "" {
				fast = &ModelOverride{Provider: sess.FastModelProvider, Model: sess.FastModelID}
			}
			inheritSessionEffort(smart, fast, sess)
		}
	}

	var pinned *resolvedOverrides
	var err error
	if smart != nil || fast != nil {
		pinned, err = c.applyModelOverrides(ctx, sessionID, smart, fast)
	} else {
		pinned, err = c.resolveSessionModels(ctx, sessionID)
	}
	if err != nil {
		c.currentAgent.ReleaseExclusive(sessionID, epoch, cancel)
		return nil, fmt.Errorf("failed to resolve session models: %w", err)
	}

	call, err := c.buildCall(ctx, sessionID, prompt, pinned, attachments)
	if err != nil {
		c.currentAgent.ReleaseExclusive(sessionID, epoch, cancel)
		return nil, err
	}
	// No retry loop exists in this entry point (unlike runInternal), so the
	// recorder can be wired directly onto the one call instead of going
	// through a per-attempt evidence capture: runOwned's own carry-over
	// (R9-1) propagates this to any accepted in-process replacement.
	if callResult != nil {
		call.OnAssistantMessageCreated = func(id string) {
			callResultRecord(id)
			executionIdentityRecord(id)
		}
	} else {
		call.OnAssistantMessageCreated = executionIdentityRecord
	}
	// HANDOFF LINE: from here on, SessionAgent.RunWithReservedOwnership (and,
	// past ITS OWN handoff line, runOwned's defer) owns releasing this
	// reservation exactly once. Nothing after this line may call
	// ReleaseExclusive again for this epoch.
	return c.currentAgent.RunWithReservedOwnership(ctx, call, epoch, cancel, onHandoff)
}
