package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/hyper"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/stringext"
)

// handleStreamFailure is runTurn's error path once agent.Stream returns a
// non-nil err (already passed through normalizeTurnError by the caller).
// Moved out mechanically (task #933/#938) — this was the largest cluster
// runTurn's callback set had that an earlier pass over this function missed
// entirely, since it isn't a fantasy callback at all, just the block of
// runTurn's own body that runs right after the Stream call returns. It is a
// pure function of ts's settled callback state plus the two locals runTurn
// still owns at the call site (result, userMessageCreated): no defer, no
// goroutine, nothing outlives this call.
func (ts *turnStream) handleStreamFailure(
	result *fantasy.AgentResult,
	err error,
	userMessageCreated bool,
) (*fantasy.AgentResult, SessionAgentCall, bool, error) {
	isHyper := ts.smartModel.ModelCfg.Provider == hyper.Name
	isCancelErr := errors.Is(err, context.Canceled)
	isWatchdogStall := isCancelErr && ts.wd.stalled.Load()
	// Turn-stall abort (causeStall): join ErrTurnStalled BEFORE the
	// ErrCallAlreadyAttempted wrap so the CLI's exit mapping can classify
	// the run as "stalled" through the Unwrap chain.
	if isWatchdogStall && ts.watchdogCauseVal != nil && watchdogCause(ts.watchdogCauseVal.Load()) == causeStall {
		err = errors.Join(err, ErrTurnStalled)
	}
	// The TURN's own context decides what a deadline error means: a net/http
	// timeout satisfies errors.Is(err, context.DeadlineExceeded) with a live
	// turn context and is a provider failure, not an operator stop.
	turnCtxDone := ts.genCtx.Err() != nil
	if ts.att != nil {
		// A watchdog stall surfaces as context.Canceled but is a real,
		// paid attempt (accountDrainAttempt does not exempt it).
		ts.att.stalled.Store(isWatchdogStall)
		ts.att.turnCtxDone.Store(turnCtxDone)
		ts.att.streamErr = err
	}
	// `rush run --timeout` bounds the whole invocation via
	// context.WithTimeout on the root ctx (run.go); when it fires
	// mid-turn, ctx.Err() is context.DeadlineExceeded, NOT
	// context.Canceled, so isCancelErr above never catches it. Without
	// this branch it fell into the generic `else` below as "Provider
	// Error" with a bare "context deadline exceeded" — indistinguishable
	// from a real provider failure and useless to `sessions why`. Only a
	// deadline of the turn's own context counts: a transport timeout
	// (net/http Client.Timeout) is a provider failure and takes the generic
	// branch, not this "--timeout" text.
	isRunTimeout := errors.Is(err, context.DeadlineExceeded) && turnCtxDone
	// If userMessageCreated is true (either we just created it or
	// call.ExistingMessageID was set), the call has already left a
	// persistent trace. Wrap the error to prevent duplicate execution
	// on retry (task #339). This handles ALL errors after user message
	// creation, not just those after currentAssistant is set.
	//
	// The wrapping must happen BEFORE we check nilAssistant because
	// errors in PrepareStep (before currentAssistant is set) also need
	// to be wrapped. See call_attempted_error.go for the design rationale.
	if userMessageCreated {
		err = &ErrCallAlreadyAttempted{Err: err}
	}
	// currentAssistant is only ever reassigned (never set back to nil)
	// by PrepareStep, under mu. agent.Stream has already
	// returned by this point so no streaming callback can race this
	// read, but the peak-hours watcher goroutine may still be alive
	// (it only stops when genCtx is cancelled by the deferred cancel()
	// at the end of Run) and touches currentAssistant under the same
	// lock, so guard the read too.
	ts.mu.Lock()
	nilAssistant := ts.currentAssistant == nil
	ts.mu.Unlock()
	if nilAssistant {
		return result, SessionAgentCall{}, false, err
	}
	// All DB writes in the error path use a detached context. The outer
	// ctx may itself be cancelled — in `rush run` it's the
	// signal.NotifyContext from fang, so Ctrl-C cancels it too; in the
	// web UI a request abort cancels it; the stream watchdog above
	// cancels genCtx (whose parent is ctx, so it doesn't cancel ctx,
	// but defensively we still detach). Without a detached ctx the
	// finish part Update fails with context.Canceled and the assistant
	// ends up half-saved in the DB — the "silent dying" pattern
	// observed in 162-promise-all. Codec must surface control: the
	// finish part MUST land on disk before we return.
	flushCtx, flushCancel := context.WithTimeout(context.WithoutCancel(ts.ctx), 15*time.Second)
	defer flushCancel()
	// Turn-stall abort threshold for finish/tool-result wording (nil-safe).
	var stallTimeout time.Duration
	if ts.call.CallOptions != nil {
		stallTimeout = ts.call.CallOptions.TurnStallTimeout
	}
	// Ensure we finish thinking on error to close the reasoning state.
	// From here to the final flush below, currentAssistant's Parts are
	// mutated in place; every touch (including the plain reads used to
	// build msgs/toolCalls) takes mu to stay consistent with
	// the peak-hours watcher goroutine that may still be running.
	ts.mu.Lock()
	ts.currentAssistant.FinishThinking()
	toolCalls := ts.currentAssistant.ToolCalls()
	assistantID := ts.currentAssistant.ID
	sessionID := ts.currentAssistant.SessionID
	ts.mu.Unlock()
	msgs, createErr := ts.a.messages.List(flushCtx, sessionID)
	if createErr != nil {
		return nil, SessionAgentCall{}, false, &ErrCallAlreadyAttempted{Err: createErr}
	}
	for _, tc := range toolCalls {
		if !tc.Finished {
			tc.Finished = true
			tc.Input = "{}"
			ts.mu.Lock()
			ts.currentAssistant.AddToolCall(tc)
			snap := ts.currentAssistant.Clone()
			ts.mu.Unlock()
			updateErr := ts.a.messages.Update(flushCtx, snap)
			if updateErr != nil {
				return nil, SessionAgentCall{}, false, &ErrCallAlreadyAttempted{Err: updateErr}
			}
		}

		if hasToolResultAfterAssistantPosition(msgs, assistantID, tc.ID) {
			continue
		}
		content := "There was an error while executing the tool"
		if isWatchdogStall {
			content = watchdogToolResultMessage(
				watchdogCause(ts.watchdogCauseVal.Load()),
				ts.toolMaxDuration,
				ts.timeoutHardCap,
				ts.idleTimeout,
				stallTimeout,
				ts.smartModel.ModelCfg.Provider,
			)
		} else if isCancelErr {
			content = "Error: user cancelled assistant tool calling"
		}
		toolResult := message.ToolResult{
			ToolCallID: tc.ID,
			Name:       tc.Name,
			Content:    content,
			IsError:    true,
		}
		_, createErr = ts.a.messages.Create(flushCtx, sessionID, message.CreateMessageParams{
			Role: message.Tool,
			Parts: []message.ContentPart{
				toolResult,
			},
		})
		if createErr != nil {
			return nil, SessionAgentCall{}, false, &ErrCallAlreadyAttempted{Err: createErr}
		}
	}
	var fantasyErr *fantasy.Error
	var providerErr *fantasy.ProviderError
	var peakErr *PeakHoursError
	var awaitingErr *AwaitingAnswerError
	const defaultTitle = "Provider Error"
	// None of the branches below perform I/O — they only decide which
	// AddFinish to record based on err/isWatchdogStall/etc. — so the
	// whole chain can run under a single lock/unlock pair guarding the
	// currentAssistant mutation, matching the pattern used everywhere
	// else in Run().
	ts.mu.Lock()
	if isWatchdogStall {
		// Close the observability loop: the watchdog goroutine already
		// emitted its slog.Warn at fire-time, but a log reader
		// chasing the trail needs to see that the stall actually
		// made it into the user-visible finish part on this session.
		slog.Info(
			"agent: watchdog stall surfaced as FinishReasonError",
			"session_id", ts.call.SessionID,
			"provider", ts.smartModel.ModelCfg.Provider,
		)
		cause := watchdogCause(ts.watchdogCauseVal.Load())
		title, _ := watchdogFinishMessage(
			cause,
			ts.toolMaxDuration,
			ts.timeoutHardCap,
			ts.idleTimeout,
			stallTimeout,
			ts.smartModel.ModelCfg.Provider,
		)
		body := composeWatchdogFinishBody(ts.call.SessionID, cause, ts.toolMaxDuration, ts.timeoutHardCap, ts.idleTimeout, stallTimeout, ts.smartModel.ModelCfg.Provider)
		ts.currentAssistant.AddFinish(message.FinishReasonError, title, body)
	} else if isCancelErr {
		ts.currentAssistant.AddFinish(message.FinishReasonCanceled, "User canceled request", "")
	} else if isRunTimeout {
		title, details := runTimeoutFinishText(context.Cause(ts.genCtx), ts.call.SessionID)
		ts.currentAssistant.AddFinish(message.FinishReasonError, title, details)
	} else if isHyper && errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusUnauthorized {
		ts.currentAssistant.AddFinish(message.FinishReasonError, "Unauthorized", `Please re-authenticate with Hyper. You can also run "rush auth" to re-authenticate.`)
	} else if isHyper && errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusPaymentRequired {
		url := hyper.BaseURL()
		ts.currentAssistant.AddFinish(message.FinishReasonError, "No credits", "You're out of credits. Add more at "+url)
	} else if errors.As(err, &providerErr) {
		if providerErr.Message == "The requested model is not supported." {
			url := "https://github.com/settings/copilot/features"
			ts.currentAssistant.AddFinish(
				message.FinishReasonError,
				"Copilot model not enabled",
				fmt.Sprintf("%q is not enabled in Copilot. Go to the following page to enable it. Then, wait 5 minutes before trying again. %s", ts.smartModel.CatwalkCfg.Name, url),
			)
		} else {
			// R979: a quota-classified 429 (isQuotaLimit) embeds the
			// provider's OWN reset timestamp, in whatever timezone the
			// remote server used -- rush ping already solved converting
			// that to local time (QuotaLimitGuidance, extracted from its
			// former pingRateLimitReset); reuse it here so `rush run`
			// stops showing the reset time in remote time. The raw
			// provider message is kept as-is and the local-time line is
			// appended, matching PeakHoursGuidance's own "add a precise
			// line, don't hide the underlying text" pattern. ok=false
			// (not a quota wall, or no parseable reset hint) leaves the
			// message exactly as before -- graceful degradation, no panic.
			// An inline stamp is rewritten in place (one reset, one time);
			// the extra line is only for resets that came from headers.
			details := LocalizeResetHint(providerErr.Message, time.Now())
			if details == providerErr.Message {
				if guidance, ok := QuotaLimitGuidance(err); ok {
					details = fmt.Sprintf("%s\n\n%s", providerErr.Message, guidance)
				}
			}
			ts.currentAssistant.AddFinish(message.FinishReasonError, cmp.Or(stringext.Capitalize(providerErr.Title), defaultTitle), details)
		}
	} else if errors.As(err, &fantasyErr) {
		ts.currentAssistant.AddFinish(message.FinishReasonError, cmp.Or(stringext.Capitalize(fantasyErr.Title), defaultTitle), fantasyErr.Message)
	} else if errors.As(err, &peakErr) {
		// Re-derive the same (msg, details) OnStepFinish's peak-hours
		// check already wrote (with the RESUME AT guidance) — this
		// path (err forced to peakHoursAbortErr) ALSO reaches here,
		// and AddFinish always replaces the prior finish part, so
		// without this branch the generic `else` below would
		// overwrite the useful message with a bare "Provider Error:
		// <terse text>" that drops the resume-time guidance entirely.
		peakMsg, peakDetails := peakHoursStoppedFinishText(err)
		ts.currentAssistant.AddFinish(message.FinishReasonError, peakMsg, peakDetails)
	} else if errors.As(err, &awaitingErr) {
		// Same rationale as the peakErr branch above: without this,
		// the generic `else` below would overwrite the question/options/
		// resume-command guidance with a bare "Provider Error: <text>".
		role := ""
		if ts.call.CallOptions != nil {
			role = string(ts.call.CallOptions.ModelRole)
		}
		awaitingMsg, awaitingDetails := awaitingAnswerStoppedFinishText(err, role)
		ts.currentAssistant.AddFinish(message.FinishReasonError, awaitingMsg, awaitingDetails)
	} else {
		// R2-5: this generic finish is the diagnostic path that persists
		// raw error text into the transcript's finish details; redact
		// URL secrets (userinfo/query) before it lands on disk.
		ts.currentAssistant.AddFinish(message.FinishReasonError, defaultTitle, redactNetworkURLs(LocalizeResetHint(err.Error(), time.Now())))
	}
	snap := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	// Detached flush (flushCtx is context.WithoutCancel + 15s timeout,
	// created at the top of this error block). This is the call that
	// MUST land on disk — without it the assistant message has tool
	// calls but no finish part, and the WUI/recovery sees it as still
	// in-flight forever.
	updateErr := ts.persistFailureFinish(flushCtx, snap, awaitingErr != nil)
	if updateErr != nil {
		slog.Error(
			"agent: failed to persist final finish part",
			"session_id", ts.call.SessionID,
			"err", updateErr,
		)
		return nil, SessionAgentCall{}, false, &ErrCallAlreadyAttempted{Err: updateErr}
	}

	// Drain on cancel via the mailbox's generation-aware drain (design
	// §4): an interrupt-and-replace payload (mb.replacement) takes
	// precedence over a plain queued follow-up (mb.submitted). The
	// busy reservation itself stays claimed (Run's loop is about to
	// run another turn for the same sessionID) and is only released
	// by Run() once the loop has no more queued work.
	if isCancelErr {
		if next, ok := ts.a.getMailbox(ts.call.SessionID).drainAfterCancel(); ok {
			ts.cancel()
			return nil, next, true, nil
		}
	}
	// err was already wrapped in ErrCallAlreadyAttempted above (if
	// userMessageCreated), so return it directly rather than wrapping
	// it a second time.
	return nil, SessionAgentCall{}, false, err
}

// hasToolResultAfterAssistantPosition only accepts a matching result from the
// issuing assistant message onward. Tool-call IDs can be reused across turns,
// so an older result with the same ID must not suppress recovery for this call.
func hasToolResultAfterAssistantPosition(msgs []message.Message, assistantID, toolCallID string) bool {
	start := -1
	for i := range msgs {
		if msgs[i].ID == assistantID {
			start = i
			break
		}
	}
	if start < 0 {
		return false
	}
	for _, msg := range msgs[start:] {
		if msg.Role != message.Tool {
			continue
		}
		for _, result := range msg.ToolResults() {
			if result.ToolCallID == toolCallID {
				return true
			}
		}
	}
	return false
}

// persistFailureFinish writes the error-path final finish. A question the
// agent asked is a reaction (it answered the notices visible in its prompt
// with real content), so its finish goes through the same one-transaction
// write as a normal step and marks the owner's delivered debt reacted; a
// failed mark falls back to the plain update so the finish still lands.
func (ts *turnStream) persistFailureFinish(ctx context.Context, snap message.Message, awaiting bool) error {
	if awaiting && hasReactionContent(snap) {
		var err error
		// The batch twin marks the batch debt (and clears its reacted_failed);
		// a failed mark still falls back to the plain update.
		if store := batchCutoverReactionStore(ts.a.coord); store != nil {
			err = store.BatchReactionMarkReactedWithMessageUpdate(ctx, ts.a.messages, ts.call.SessionID, snap)
		} else {
			err = ts.a.messages.Update(ctx, snap)
		}
		if err == nil {
			return nil
		}
		slog.Warn("agent: recording the question as a reaction failed; writing the finish alone",
			"session_id", ts.call.SessionID, "err", err)
	}
	return ts.a.messages.Update(ctx, snap)
}
