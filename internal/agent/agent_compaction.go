// Compaction and summarisation: the manual /compact path under mailbox
// ownership (Summarize/runSummarize), the inline bodies reused by
// auto-summarize and the silent sliding-window compact (runSummarizeBody,
// runSummarizeSilent), and the summarize-queue accessors.
package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"

	"github.com/PHPCraftdream/rush/internal/heartbeat"
	rushlog "github.com/PHPCraftdream/rush/internal/log"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// summaryCommitMaxDuration bounds the silent compaction's COMMIT phase — the
// SetSummaryAndUsage write plus the deletion of the messages it replaces
// (P0-4 review follow-up).
//
// That phase deliberately runs on context.WithoutCancel: interrupting it
// between "summary pointer written" and "old messages deleted" is how a
// session's history gets unrecoverably holed, so cancellation must not be
// able to land in the middle. Detaching from cancellation without a deadline
// would recreate the unbounded-operation-holds-Run shape P1-B removed, hence
// this bound. The provider stream is already finished by then; what remains
// is local single-writer DB work, so 30s is generous rather than tight.
const summaryCommitMaxDuration = 30 * time.Second

//go:embed templates/summary.md
var summaryPrompt []byte

// ErrSummarizeQueued is returned by Summarize when the session is busy and
// the request has been queued for execution after the current task finishes.
var ErrSummarizeQueued = errors.New("summarize queued")

func (a *sessionAgent) Summarize(ctx context.Context, sessionID string, snapshot *SummarizeSnapshot) error {
	// Track this Summarize() call in the agent-wide runWg so CancelAll can join
	// on manual compactions that might be writing to the DB (P0-4).
	// This defer fires on EVERY return from Summarize, including the queued
	// early-return path and all error paths. The check and Add are gated
	// under admitMu (tryAdmitRunWg) to close the P1-1 race: without the mutex,
	// a concurrent CancelAll could set shuttingDown and start its Wait goroutine
	// while the counter was still 0, letting Wait return before Add(1) landed
	// (panics or admits work after "shutdown complete").
	if !a.tryAdmitRunWg() {
		return ErrAgentShuttingDown
	}
	defer a.runWg.Done()

	// Atomic check-and-reserve (#268/P0-4, design §6): beginCompact makes
	// us the sole owner of sessionID's mailbox or returns false if a turn
	// or another compaction already owns it. This replaces the old
	// non-atomic IsSessionBusy + runSummarize pair that had a TOCTOU window
	// between the busy check and the compaction's DB writes — a concurrent
	// Run could start between the check and the first delete, corrupting
	// history.
	genCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	mb := a.getMailbox(sessionID)
	epoch, ok := mb.beginCompact(cancel)
	if !ok {
		cancel()
		a.summarizeQueue.Set(sessionID, snapshot)
		return ErrSummarizeQueued
	}
	return a.runSummarize(ctx, genCtx, sessionID, snapshot, mb, epoch, cancel)
}

// runSummarize runs a manual /compact under mailbox ownership acquired by
// Summarize's beginCompact call. After the compaction body completes, it
// releases ownership via abandonOwnership (handing back any Run that queued
// behind it via mailbox.submit during the stream) and starts a fresh Run for
// the first queued call — the same drain shape the old runSummarizeCore tail
// had, now operating on mailbox ownership instead of a synthetic
// activeRequests key.
//
// #312: Cross-process compaction serialization
//
// Manual compaction (this path) acquires the OS session lock before calling
// runSummarizeBody and releases it after. This prevents another process from
// starting a Run() for the same session while the compaction's commit phase
// (set SummaryMessageID + delete messages) is in flight.
//
// Inline compaction paths (runTurn's shouldSummarize and silentCompactNeeded)
// do NOT go through this function — they call runSummarizeBody or
// runSummarizeSilent directly while already holding the OS lock via the parent
// Run() call. No deadlock is possible because those paths never acquire a
// second lock.
//
// OS lock acquisition happens AFTER beginCompact succeeds and mb.mu is
// released, avoiding #296's I/O-under-mu anti-pattern. The lock is held for
// the entire runSummarizeBody call to protect both the LLM stream and the
// commit phase, matching Run()'s lock-hold semantics.
//
// The snapshot parameter contains the model, provider options, and prompt
// prefix resolved from the target session (or shared state for sessions without
// overrides), ensuring the entire summarize operation uses consistent
// configuration regardless of concurrent SetModels calls (task #341, P1-1).
func (a *sessionAgent) runSummarize(ctx context.Context, genCtx context.Context, sessionID string, snapshot *SummarizeSnapshot, mb *mailbox, epoch uint64, cancel context.CancelFunc) error {
	// Determine idle timeout for summarization stream watchdog
	idleTimeout := streamIdleTimeoutDefault
	if a.streamIdleTimeout > 0 {
		idleTimeout = a.streamIdleTimeout
	}
	stallSetPhase(sessionID, "internal", "compaction")
	// No tools in manual compaction, so use 0 for tool bounds
	toolMaxDuration := time.Duration(0)
	toolCleanupGrace := time.Duration(0)
	// Start stream watchdog to detect idle stalls in the summarization stream
	var watchdogCauseVal atomic.Int32
	wd := startStreamWatchdog(
		genCtx, cancel, idleTimeout, a.effectiveStreamWatchdogTick(),
		// Use a simple callback - just log and cancel (no tools to report)
		func(elapsed time.Duration, cause watchdogCause) {
			watchdogCauseVal.Store(int32(cause))
			stackDump := rushlog.CaptureGoroutineStack("summarize watchdog fired")
			go func() {
				if dumpPath, dumpErr := rushlog.WriteGoroutineDump(stackDump); dumpErr != nil {
					slog.Warn("agent.runSummarize: failed to write goroutine dump", "err", dumpErr)
				} else {
					slog.Warn("agent.runSummarize: wrote goroutine dump", "path", dumpPath)
				}
			}()
			switch cause {
			case causeToolTimeout:
				slog.Warn(
					"agent.runSummarize: watchdog firing — tool execution exceeded cap, force-cancelling",
					"session_id", sessionID,
					"elapsed", elapsed.String(),
					"cap", toolMaxDuration.String(),
				)
			case causeHardCap:
				slog.Warn(
					"agent.runSummarize: watchdog firing — summarization exceeded hard cap, force-cancelling",
					"session_id", sessionID,
					"elapsed", elapsed.String(),
					"hard_cap", a.timeoutHardCap.String(),
				)
			default:
				slog.Warn(
					"agent.runSummarize: watchdog firing — no provider activity, force-cancelling",
					"session_id", sessionID,
					"idle_duration", elapsed.String(),
					"threshold", idleTimeout.String(),
				)
			}
		},
		a.timeoutExtendsOnProgress,
		a.timeoutHardCap,
		toolMaxDuration,
		toolCleanupGrace,
		func() { notifyActivity(genCtx) },
	)
	// Wrap genCtx so notifyWatchdog calls reach this watchdog
	genCtx = withWatchdogBump(genCtx, wd.bump)
	// Defer order matters: <-wd.done first so it runs after cancel()
	defer func() { <-wd.done }()
	defer cancel()

	// #312: Acquire OS session lock for cross-process serialization.
	// Manual compaction runs outside any Run() call, so the OS lock is not
	// already held. Acquiring it here prevents another process from starting
	// a concurrent Run() that would race with the compaction's DB writes.
	var lk *session.SessionLock
	if a.dataDir != "" {
		var lockErr error
		lk, lockErr = session.TryAcquireSessionLockWithOptions(a.dataDir, sessionID, a.lockOptions...)
		if lockErr != nil {
			var busyErr *session.SessionLockBusyError
			if errors.As(lockErr, &busyErr) {
				slog.Warn(
					"agent.runSummarize: rejected — session locked by another process",
					"session_id", sessionID,
					"holder_pid", busyErr.HolderPID,
					"lock_path", busyErr.Path,
				)
				// Release mailbox ownership with handoff so queued work gets a runner.
				a.abandonOwnershipWithHandoff(sessionID, epoch)
				a.requeueSummarizeSnapshot(sessionID, snapshot)
				return fmt.Errorf("%w: session %q is already in use: %w", ErrSummarizeQueued, sessionID, lockErr)
			}
			slog.Error("agent.runSummarize: failed to acquire inter-process session lock, refusing to compact unprotected",
				"session_id", sessionID, "err", lockErr)
			// Release mailbox ownership with handoff so queued work gets a runner.
			a.abandonOwnershipWithHandoff(sessionID, epoch)
			a.requeueSummarizeSnapshot(sessionID, snapshot)
			return fmt.Errorf("%w: session %q: could not acquire session lock: %w", ErrSummarizeQueued, sessionID, lockErr)
		}
	}

	// Wire this lock into genCtx's activity-notify chain (task #222's
	// pattern, mirrored from runTurn's own withActivityNotify call) so
	// runSummarizeBody's notifyActivity(ctx) calls on its stream callbacks
	// (#310) actually reach THIS lock. Without this, manual /compact
	// acquires its own lk above but never records activity on it — the
	// lock's mtime is frozen from acquisition until release, up to the full
	// 10-minute genCtx timeout, so `sessions locks`/`watch` can misreport a
	// healthy long-running /compact as heartbeat-stale. nil-receiver-safe
	// when lk is nil (a.dataDir == "").
	genCtx = withActivityNotify(genCtx, lk)

	// Test-only seam: fires strictly after the snapshot above was captured
	// and strictly before it is consumed below, letting a test land a
	// concurrent shared-state mutation deterministically inside the window a
	// pre-#341 regression would have re-read from. See
	// mailbox.testPreSnapshotConsumeSeam's own doc.
	if mb.testPreSnapshotConsumeSeam != nil {
		mb.testPreSnapshotConsumeSeam()
	}

	// Use the snapshot provided by the caller. The snapshot contains the
	// model, provider options, and prompt prefix resolved from the target
	// session (or shared state for sessions without overrides), ensuring the
	// entire summarize operation uses consistent configuration regardless of
	// concurrent SetModels calls (task #341, P1-1).
	err := a.runSummarizeBody(genCtx, sessionID, snapshot.providerOptions, snapshot.model, snapshot.promptPrefix)

	// P1-2 fix (2026-08-09): Transition mailbox to mbReleasing before releasing
	// the OS lock. This provides the same visibility that normal turns get
	// via drainOrReleaseFinal, allowing diagnostic tools to distinguish "releasing
	// OS lock" from "still actively streaming". If the filesystem/AV/SMB hangs
	// during lk.Release(), mbReleasing is still observable as "busy but not
	// streaming", which is semantically correct - the session is technically busy
	// (ownership not yet relinquished to another caller) but not making progress.
	releaseStarted := mb.beginRelease(epoch)

	// Release the OS lock. Even if beginRelease failed (epoch mismatch or state
	// already transitioned), we still need to release the lock to avoid leaking it.
	// The worst case is we release without mbReleasing visibility, which is the
	// old (pre-P1-2) behavior for that particular edge case.
	if lk != nil {
		if relErr := lk.Release(); relErr != nil {
			slog.Debug("agent.runSummarize: release session lock failed", "session_id", sessionID, "err", relErr)
		}
	}

	// Test-only seam: fires strictly after the OS lock above is released and
	// strictly before abandonOwnership below makes that visible as mbIdle to
	// same-process callers. See mailbox.testPreAbandonSeam's own doc.
	if mb.testPreAbandonSeam != nil {
		mb.testPreAbandonSeam()
	}

	// Complete the mbReleasing -> mbIdle transition if we successfully began it.
	// If beginRelease failed (epoch changed, wrong state), we skip this - the
	// actual state transition already happened elsewhere (e.g., a concurrent
	// abandonOwnership from a Cancel or shutdown).
	if releaseStarted {
		mb.finishRelease(epoch)
	}

	if err != nil {
		a.abandonOwnershipWithHandoff(sessionID, epoch)
		return err
	}

	// Success path: release ownership, drain the mailbox queue, and coalesce
	// any pending summarize requests. If a second /compact was queued while
	// this one was running, we discard it (coalesce) because the history has
	// already been compressed by this successful compaction.
	//
	// Uses the atomic abandonOwnershipAndPopFirstSubmitted (P2-5 follow-up)
	// instead of a separate abandonOwnership() + popFirstSubmitted() pair —
	// two independent lock acquisitions here left the same era-boundary
	// reordering window abandonOwnershipWithHandoff's own P2-5 fix closed
	// for the "pop all" case: a new owner's submit()+queue() landing in the
	// gap between them could get scooped into firstQueued below instead of
	// staying queued for that owner's own turn.
	firstQueued, hasNext := mb.abandonOwnershipAndPopFirstSubmitted(epoch)

	// Drain any queued summarize request - if a second /compact was queued
	// while this one was running, the history is already compressed, so we
	// coalesce by discarding the stale queued entry rather than executing it.
	// This is semantically correct: the queued request's goal (compress history)
	// is already satisfied by this compaction, and avoids redundant LLM calls.
	if a.summarizeQueue != nil {
		if _, queued := a.TakeSummarizeQueue(sessionID); queued {
			slog.Debug("agent: queued summarize coalesced with successful compaction, discarded stale entry",
				"session_id", sessionID)
		}
	}

	if !hasNext {
		return nil
	}
	_, runErr := a.Run(ctx, firstQueued)
	return runErr
}

// requeueSummarizeSnapshot puts a compaction back in summarizeQueue after a
// lock-acquisition failure so the session's next owner drains and runs it.
// GetOrSet (not Set) never clobbers a newer queued request, and no compaction
// is spawned here, so a long-held lock cannot cause a busy retry loop.
func (a *sessionAgent) requeueSummarizeSnapshot(sessionID string, snapshot *SummarizeSnapshot) {
	a.summarizeQueue.GetOrSet(sessionID, func() *SummarizeSnapshot { return snapshot })
	slog.Info("agent.runSummarize: re-queued compaction after session lock failure",
		"session_id", sessionID)
}

func (a *sessionAgent) SummarizeQueued(sessionID string) bool {
	_, ok := a.summarizeQueue.Get(sessionID)
	return ok
}

// testBuildSummarizeSnapshot is a test helper that creates a SummarizeSnapshot
// from the agent's current shared state. Used by tests that call Summarize directly.
func (a *sessionAgent) testBuildSummarizeSnapshot() *SummarizeSnapshot {
	model := a.smartModel.Get()
	prefix := a.systemPromptPrefix.Get()

	// Build minimal provider options (same as getProviderOptions but for tests).
	opts := fantasy.ProviderOptions{}

	return &SummarizeSnapshot{
		model:           model,
		providerOptions: opts,
		promptPrefix:    prefix,
	}
}

func (a *sessionAgent) TakeSummarizeQueue(sessionID string) (*SummarizeSnapshot, bool) {
	snapshot, ok := a.summarizeQueue.Take(sessionID)
	return snapshot, ok
}

func (a *sessionAgent) CancelQueuedSummarize(sessionID string) {
	a.summarizeQueue.Del(sessionID)
}

// runSummarizeBody performs the actual summarisation: load messages, snapshot
// non-pinned IDs for deletion, create the summary message, stream the
// summary, persist it, wire up SummaryMessageID, and delete the old messages.
// It performs NO busy-check and NO ownership management — the caller must
// already hold sessionID's mailbox ownership (via beginCompact for
// standalone /compact, or via the turn loop's submit for inline
// auto-summarize). The caller's cancel is already registered in the mailbox
// (current.cancel), so Cancel(sessionID) naturally targets this compaction
// — no synthetic "sessionID-summarize" key.
//
// genCtx is the caller's already-cancel-scoped context (beginCompact's
// cancel for manual /compact, or the turn's genCtx for inline
// auto-summarize).
func (a *sessionAgent) runSummarizeBody(ctx context.Context, sessionID string, opts fantasy.ProviderOptions, smartModel Model, systemPromptPrefix string) error {
	// Heartbeat attribution: summary runs on the smart slot (design pt. 1).
	role, source := hbInherited(ctx)
	ctx = withHeartbeat(ctx, sessionID, heartbeat.PurposeSummary, role, source)
	currentSession, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	msgs, err := a.getSessionMessages(ctx, currentSession)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		// Nothing to summarize.
		return nil
	}

	// Snapshot non-pinned message IDs for deletion AFTER the summary stream
	// completes. The summary message itself is created BELOW, after this
	// snapshot, so it is never in toDelete. Pinned messages stay.
	var toDelete []message.Message
	for _, m := range msgs {
		if !m.Pinned {
			toDelete = append(toDelete, m)
		}
	}

	aiMsgs, _ := a.preparePrompt(msgs, nil)

	agent := fantasy.NewAgent(
		smartModel.Model,
		fantasy.WithSystemPrompt(string(summaryPrompt)),
		fantasy.WithUserAgent(userAgent),
	)
	summaryMessage, err := a.messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:             message.Assistant,
		Model:            smartModel.Model.Model(),
		Provider:         smartModel.Model.Provider(),
		ReasoningEffort:  turnSmartReasoningEffort(smartModel),
		IsSummaryMessage: true,
	})
	if err != nil {
		return err
	}

	summaryPromptText := buildSummaryPrompt(currentSession.Todos)

	resp, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		MaxRetries:      &ProviderMaxRetries,
		Prompt:          summaryPromptText,
		Messages:        aiMsgs,
		Headers:         sessionHeaders(sessionID),
		ProviderOptions: opts,
		PrepareStep: func(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = options.Messages
			if systemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{fantasy.NewSystemMessage(systemPromptPrefix)}, prepared.Messages...)
			}
			return callContext, prepared, nil
		},
		OnReasoningDelta: func(id string, text string) error {
			notifyActivity(ctx)
			notifyWatchdog(ctx)
			summaryMessage.AppendReasoningContent(text)
			return a.messages.Update(ctx, summaryMessage)
		},
		OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
			notifyActivity(ctx)
			notifyWatchdog(ctx)
			// Handle anthropic signature.
			if anthropicData, ok := reasoning.ProviderMetadata["anthropic"]; ok {
				if signature, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok && signature.Signature != "" {
					summaryMessage.AppendReasoningSignature(signature.Signature)
				}
			}
			summaryMessage.FinishThinking()
			return a.messages.Update(ctx, summaryMessage)
		},
		OnTextDelta: func(id, text string) error {
			notifyActivity(ctx)
			notifyWatchdog(ctx)
			summaryMessage.AppendContent(text)
			return a.messages.Update(ctx, summaryMessage)
		},
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// User cancelled summarize; remove the summary message.
			// Use a bounded cancel-immune context for cleanup (P1-4): the
			// stream's context is already canceled, so Delete would silently
			// fail and leave an orphaned summary message in the DB.
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), summaryCommitMaxDuration)
			defer cleanupCancel()
			deleteErr := a.messages.Delete(cleanupCtx, summaryMessage.ID)
			if deleteErr != nil {
				slog.Error("Failed to delete orphaned summary message after cancel", "session_id", sessionID, "err", deleteErr)
			}
			return err
		}
		// Mark the summary message as finished with an error so the UI
		// stops spinning. Use a bounded cancel-immune context for cleanup (P1-4).
		summaryMessage.AddFinish(message.FinishReasonError, "Summarization Error", err.Error())
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), summaryCommitMaxDuration)
		defer cleanupCancel()
		if updateErr := a.messages.Update(cleanupCtx, summaryMessage); updateErr != nil {
			slog.Error("Failed to mark summary message as error", "session_id", sessionID, "err", updateErr)
			return updateErr
		}
		return err
	}

	summaryMessage.AddFinish(message.FinishReasonEndTurn, "", "")
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), summaryCommitMaxDuration)
	defer cleanupCancel()
	if err = a.messages.Update(cleanupCtx, summaryMessage); err != nil {
		return err
	}

	// COMMIT PHASE — the commit must not be tearable by cancellation.
	//
	// Without WithoutCancel, a cancellation (Stop, CancelAll, --timeout, stream
	// watchdog) could land between SetSummaryAndUsage and the deletes, leaving
	// the session in a holed state: summary pointer is set but old messages
	// still exist, getSessionMessages sees neither, history is unrecoverably
	// corrupt.
	//
	// Bounded rather than unbounded: the provider stream is already finished,
	// so what remains is local DB work. Uses the same timeout as
	// runSummarizeSilent.
	commitCtx, commitCancel := context.WithTimeout(context.WithoutCancel(ctx), summaryCommitMaxDuration)
	defer commitCancel()

	var openrouterCost *float64
	for _, step := range resp.Steps {
		stepCost := a.openrouterCost(step.ProviderMetadata)
		if stepCost != nil {
			newCost := *stepCost
			if openrouterCost != nil {
				newCost += *openrouterCost
			}
			openrouterCost = &newCost
		}
	}

	// Re-fetch the session to pick up any user edits that happened while the
	// summary was streaming, then overlay our own fields.
	freshSession, err := a.sessions.Get(commitCtx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to re-fetch session before save: %w", err)
	}
	// Normalize once, upstream of both sinks (see agent_turn.go's OnStepFinish).
	totalUsage := normalizeProviderUsage(smartModel.Model.Provider(), resp.TotalUsage)
	costDelta := a.updateSessionUsage(smartModel, &freshSession, totalUsage, openrouterCost)
	// Heartbeat usage: one add per accounted compaction (design pt. 4).
	addHeartbeatUsage(ctx, smartModel.Model.Provider(), smartModel.Model.Model(), totalUsage, costDelta)
	if costDelta != 0 {
		if _, costErr := a.sessions.IncrementCost(commitCtx, freshSession.ID, costDelta); costErr != nil {
			return costErr
		}
	}

	// Per-message usage for the summary turn itself (task #469). Recorded
	// against totalUsage — the same figure updateSessionUsage just billed —
	// so the summarization's own token cost is visible in analytics instead
	// of appearing as an unattributed jump in the session total.
	a.recordMessageUsage(commitCtx, summaryMessage.ID, smartModel, totalUsage, costDelta, false)

	usage := resp.Response.Usage
	if err := a.sessions.SetSummaryAndUsage(commitCtx, freshSession.ID, summaryMessage.ID, 0, summaryCompletionTokens(usage, summaryMessage)); err != nil {
		// Nothing has been deleted yet, so the session is still whole: the
		// compaction simply did not happen, and the next turn retries it.
		return err
	}

	// Now that the summary is persisted and SummaryMessageID is wired up,
	// drop the historical non-pinned messages. The summary message itself was
	// created AFTER the snapshot above so it is not in toDelete.
	for _, m := range toDelete {
		if delErr := a.messages.Delete(commitCtx, m.ID); delErr != nil {
			slog.Warn("summarise: failed to delete old message", "id", m.ID, "err", delErr)
		}
	}

	return nil
}

// runSummarizeSilent compacts the oldest half of the session's messages
// synchronously under the caller's mailbox ownership without any visible
// change in the UI. It:
//  1. Loads all current messages, splits them at the midpoint.
//  2. Sends the older half to the LLM for summarisation.
//  3. Creates a hidden summary message (not rendered in the UI).
//  4. Deletes all non-pinned messages that were summarised.
//  5. Updates session.SummaryMessageID so future runs start from the summary.
//
// Pinned messages are never deleted.
func (a *sessionAgent) runSummarizeSilent(ctx context.Context, sessionID string, opts fantasy.ProviderOptions, smartModel Model, systemPromptPrefix string) error {
	// Heartbeat attribution: same summary purpose, silent path (design pt. 1).
	role, source := hbInherited(ctx)
	ctx = withHeartbeat(ctx, sessionID, heartbeat.PurposeSummary, role, source)
	currentSession, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	msgs, err := a.getSessionMessages(ctx, currentSession)
	if err != nil {
		return err
	}
	if len(msgs) < 4 {
		// Too few messages to bother summarising.
		return nil
	}

	// Split at midpoint: summarise the older half.
	mid := len(msgs) / 2
	oldMsgs := msgs[:mid]
	// Separate pinned from non-pinned in the old half.
	var toSummarise, pinnedOld []message.Message
	for _, m := range oldMsgs {
		if m.Pinned {
			pinnedOld = append(pinnedOld, m)
		} else {
			toSummarise = append(toSummarise, m)
		}
	}
	if len(toSummarise) == 0 {
		return nil
	}

	aiMsgs, _ := a.preparePrompt(toSummarise, nil)

	agent := fantasy.NewAgent(
		smartModel.Model,
		fantasy.WithSystemPrompt(string(summaryPrompt)),
	)
	// Create the summary message as hidden so it is invisible in the UI.
	summaryMessage, err := a.messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:             message.Assistant,
		Model:            smartModel.Model.Model(),
		Provider:         smartModel.Model.Provider(),
		ReasoningEffort:  turnSmartReasoningEffort(smartModel),
		IsSummaryMessage: true,
		Hidden:           true,
	})
	if err != nil {
		return err
	}

	summaryPromptText := buildSummaryPrompt(currentSession.Todos)
	resp, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		MaxRetries:      &ProviderMaxRetries,
		Prompt:          summaryPromptText,
		Messages:        aiMsgs,
		Headers:         sessionHeaders(sessionID),
		ProviderOptions: opts,
		PrepareStep: func(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = options.Messages
			if systemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{fantasy.NewSystemMessage(systemPromptPrefix)}, prepared.Messages...)
			}
			return callContext, prepared, nil
		},
		OnReasoningDelta: func(id string, text string) error {
			notifyActivity(ctx)
			notifyWatchdog(ctx)
			summaryMessage.AppendReasoningContent(text)
			return a.messages.Update(ctx, summaryMessage)
		},
		OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
			notifyActivity(ctx)
			notifyWatchdog(ctx)
			if anthropicData, ok := reasoning.ProviderMetadata["anthropic"]; ok {
				if signature, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok && signature.Signature != "" {
					summaryMessage.AppendReasoningSignature(signature.Signature)
				}
			}
			summaryMessage.FinishThinking()
			return a.messages.Update(ctx, summaryMessage)
		},
		OnTextDelta: func(id, text string) error {
			notifyActivity(ctx)
			notifyWatchdog(ctx)
			summaryMessage.AppendContent(text)
			return a.messages.Update(ctx, summaryMessage)
		},
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// User cancelled summarize; remove the summary message.
			// Use a bounded cancel-immune context for cleanup (P1-4): the
			// stream's context is already canceled, so Delete would silently
			// fail and leave an orphaned summary message in the DB.
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), summaryCommitMaxDuration)
			defer cleanupCancel()
			deleteErr := a.messages.Delete(cleanupCtx, summaryMessage.ID)
			if deleteErr != nil {
				slog.Warn("Silent summarize: failed to delete orphaned summary message after cancel", "session_id", sessionID, "err", deleteErr)
			}
		}
		return err
	}

	summaryMessage.AddFinish(message.FinishReasonEndTurn, "", "")
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), summaryCommitMaxDuration)
	defer cleanupCancel()
	if err = a.messages.Update(cleanupCtx, summaryMessage); err != nil {
		return err
	}

	// COMMIT PHASE — the order and the context were both wrong here, and
	// P0-4's own change is what made that reachable.
	//
	// Order: wire up SummaryMessageID BEFORE deleting the messages it
	// replaces, exactly as runSummarizeBody does (see its comment at the
	// equivalent point). This function did the opposite. Interrupted between
	// the two, that leaves the old half deleted with no summary pointer
	// standing in for it — getSessionMessages then finds neither, and the
	// history is unrecoverably holed.
	//
	// Context: the commit must not be tearable by cancellation. While the
	// silent compaction was a detached goroutine on context.WithoutCancel,
	// this window was unreachable. Making it synchronous (P0-4) put it on the
	// turn's cancellable genCtx and turned a long-standing latent ordering
	// bug into a live one, reachable via Stop in the web UI, CancelAll on
	// shutdown, --timeout, or the turn's own stream watchdog. Bounded rather
	// than unbounded: the provider stream is already finished, so what
	// remains is local DB work.
	commitCtx, commitCancel := context.WithTimeout(context.WithoutCancel(ctx), summaryCommitMaxDuration)
	defer commitCancel()

	// Point SummaryMessageID at the new hidden summary and reset token
	// counters so the next call gets an accurate remaining-context estimate.
	freshSession, err := a.sessions.Get(commitCtx, sessionID)
	if err != nil {
		return fmt.Errorf("silent summarise: failed to re-fetch session: %w", err)
	}
	var openrouterCost *float64
	for _, step := range resp.Steps {
		if stepCost := a.openrouterCost(step.ProviderMetadata); stepCost != nil {
			if openrouterCost == nil {
				openrouterCost = new(float64)
			}
			*openrouterCost += *stepCost
		}
	}
	// Normalize once, upstream of both sinks (see agent_turn.go's OnStepFinish).
	totalUsage := normalizeProviderUsage(smartModel.Model.Provider(), resp.TotalUsage)
	costDelta := a.updateSessionUsage(smartModel, &freshSession, totalUsage, openrouterCost)
	// Heartbeat usage: one add per accounted silent compaction (design pt. 4).
	addHeartbeatUsage(ctx, smartModel.Model.Provider(), smartModel.Model.Model(), totalUsage, costDelta)
	if costDelta != 0 {
		if _, costErr := a.sessions.IncrementCost(commitCtx, freshSession.ID, costDelta); costErr != nil {
			return costErr
		}
	}
	// Per-message usage for the silent-summarise turn (task #469), same
	// rationale as the manual /compact path above.
	a.recordMessageUsage(commitCtx, summaryMessage.ID, smartModel, totalUsage, costDelta, false)

	if err := a.sessions.SetSummaryAndUsage(commitCtx, freshSession.ID, summaryMessage.ID, 0, resp.Response.Usage.OutputTokens); err != nil {
		// Nothing has been deleted yet, so the session is still whole: the
		// compaction simply did not happen, and the next turn retries it.
		return err
	}

	// Only now, with the summary persisted AND pointed at, drop what it
	// replaces. A failure here is survivable — the summary already stands in
	// for these rows, so the worst case is redundant history, not a hole.
	for _, m := range toSummarise {
		if delErr := a.messages.Delete(commitCtx, m.ID); delErr != nil {
			slog.Warn("silent summarise: failed to delete old message", "id", m.ID, "err", delErr)
		}
	}
	_ = pinnedOld // pinned messages stay in the DB untouched
	return nil
}

// summaryCompletionTokens returns OutputTokens when the provider reported
// them, otherwise falls back to an approximate count from the rendered
// summary message. Fork merge note: from origin/main 6ed8852b
// "fix(agent): estimate missing streamed usage" — used in Summarize when
// the provider omits final usage on the summary stream.
func summaryCompletionTokens(usage fantasy.Usage, summaryMessage message.Message) int64 {
	if usage.OutputTokens != 0 {
		return usage.OutputTokens
	}
	return approxTokenCount(summaryMessage.Content().Text) + approxTokenCount(summaryMessage.FullReasoningText())
}

// buildSummaryPrompt constructs the prompt text for session summarization.
func buildSummaryPrompt(todos []session.Todo) string {
	var sb strings.Builder
	sb.WriteString("Provide a detailed summary of our conversation above.")
	if len(todos) > 0 {
		sb.WriteString("\n\n## Current Todo List\n\n")
		for _, t := range todos {
			fmt.Fprintf(&sb, "- [%s] %s\n", t.Status, t.Content)
		}
		sb.WriteString("\nInclude these tasks and their statuses in your summary. ")
		sb.WriteString("Instruct the resuming assistant to use the `todos` tool to continue tracking progress on these tasks.")
	}
	return sb.String()
}
