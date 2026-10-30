// Retry classification and continuation prompts for coordinator_run.go's
// run loop — split out because coordinator_run.go sits just under the
// repo's 1000-line limit (CLAUDE.md). Pure code move, no behavior change.

package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// continuationPrompt builds the follow-up prompt sent when a turn already
// produced partial content (text/reasoning/a tool call) before a transient
// provider failure interrupted it. Unlike the blind-retry path, this never
// resends the original prompt verbatim -- that would ask the model to
// redo work it may have already done, or contradict a tool call it already
// made. Instead it names the interruption explicitly and hands back
// whatever text the model had produced, mirroring the shape of the
// shouldSummarize continuation call in agent_turn.go (same idea -- "the
// previous attempt was cut short, continue it" -- for a different cause).
func continuationPrompt(originalPrompt string, partial message.Message) string {
	partialText := strings.TrimSpace(partial.FullText())
	if partialText == "" {
		// Progress was reasoning/a tool call only, no visible text yet --
		// nothing to quote back, so just name the interruption.
		return fmt.Sprintf("Your previous response was interrupted by a transient provider error (e.g. a stream stall or rate limit) before any text was produced. Continue working on the original request: `%s`", originalPrompt)
	}
	return fmt.Sprintf(
		"Your previous response to the request `%s` was interrupted by a transient provider error (e.g. a stream stall or rate limit) partway through. Here is what you had written so far:\n\n%s\n\nContinue exactly where you left off. Do not repeat the text above and do not restart the task from scratch.",
		originalPrompt, partialText,
	)
}

// Continuation prompt openings, shared between continuationPrompt (the
// producer) and IsContinuationPrompt (the recognizer the run loop's
// terminal reconciliation uses to find continuation chain boundaries in
// the committed history).
const (
	continuationPromptPrefixPartialText = "Your previous response to the request `"
	continuationPromptPrefixNoText      = "Your previous response was interrupted"
)

// IsContinuationPrompt reports whether text is a coordinator-generated
// continuation prompt (see continuationPrompt) — the fresh user message
// the transient-retry loop sends after an attempt was interrupted
// mid-stream. The run loop's terminal reconciliation walks the committed
// rows backward across these boundaries to reassemble a continuation
// chain's full text; a genuine user turn must end that walk, so the
// recognizer has to match the producer exactly. It lives next to
// continuationPrompt so any wording change updates both in one commit.
func IsContinuationPrompt(text string) bool {
	return strings.HasPrefix(text, continuationPromptPrefixPartialText) ||
		strings.HasPrefix(text, continuationPromptPrefixNoText)
}

// retryClass partitions a turn-terminating failure into "surface it" vs
// "transparently re-run it". See shouldRetryTurn for the policy.
type retryClass int

const (
	// classTerminal is an operator-actionable failure (quota wall, auth,
	// context overflow, bad request, user cancel) that must surface.
	classTerminal retryClass = iota
	// classTransient is a provider/network hiccup worth a re-run.
	classTransient
	// classRateLimit is a 429 non-quota overload: retryable with its own
	// dedicated wait budget.
	classRateLimit
)

// classifyProviderError classifies a NON-NIL turn-terminating error.
// context cancellation is terminal here — watchdog stalls are matched
// separately by their persisted finish title in shouldRetryTurn, because
// a stall surfaces only as context.Canceled.
func classifyProviderError(err error) retryClass {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return classTerminal
	}
	// Peak-hours refusal is operator policy, not a transient hiccup: the
	// condition only clears when the wall clock leaves the window, so a
	// backoff retry would just burn the backoff and fail identically.
	if errors.Is(err, errProviderPeakHours) {
		return classTerminal
	}
	var providerErr *fantasy.ProviderError
	if errors.As(err, &providerErr) {
		if providerErr.IsContextTooLarge() {
			return classTerminal // the auto-summarize path owns this
		}
		switch providerErr.StatusCode {
		case http.StatusUnauthorized, http.StatusPaymentRequired:
			return classTerminal
		case http.StatusForbidden:
			// 403 is ambiguous: it can be a real auth/geo wall (retry pointless)
			// or a CDN/anti-abuse banner from a fronting balancer that clears in
			// tens of seconds (z.ai "Forbidden ZS", Cloudflare-fronted providers).
			// Treat as transient: the worst case is ~40s of bounded backoff on a
			// truly bad key vs. losing a long agent run on a momentary block.
			return classTransient
		case http.StatusTooManyRequests:
			if isQuotaLimit(providerErr) {
				return classTerminal // multi-hour usage wall — operator accepts a fast fail
			}
			return classRateLimit // momentary overload — dedicated wait budget
		case http.StatusRequestTimeout, http.StatusConflict:
			return classTransient
		}
		if providerErr.StatusCode >= 500 {
			// A billing wall some providers (MiniMax 1008) serve as a 5xx never clears.
			if classifyHardProviderLimit(err, "", time.Now()) == providerLimitHard {
				return classTerminal
			}
			return classTransient
		}
		if providerErr.StatusCode >= 400 {
			return classTerminal // genuine client error (400, 404, ...)
		}
		// No HTTP status (status 0): EOF / network wrapped as ProviderError.
		if providerErr.IsRetryable() {
			return classTransient
		}
		return classTerminal
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return classTransient
	}
	return classTerminal
}

// isProviderClassifiable reports whether err is a shape classifyProviderError
// can reason about as an ACTUAL provider-turn outcome (a provider error, a
// network error, or the peak-hours refusal) -- as opposed to some other
// error a Drain's turn can return before ever reaching the provider (e.g. a
// DB read failure in the turn-start preamble). Used by
// settleOrRetryDrainFailure (coordinator_drain_policy.go) as its fallback
// classifier when no per-attempt assistant-message evidence is available
// (docs/reviews/2026-09-29-async-phase4-round1.md B5/C3, W-DRAIN item 1):
// treating every unrecognized error as classifyProviderError's terminal
// default would close debt and write a visible marker for a failure that
// never touched the provider at all.
func isProviderClassifiable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errProviderPeakHours) {
		return true
	}
	var providerErr *fantasy.ProviderError
	if errors.As(err, &providerErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// turnAttemptRefused reports whether err is an admission-shaped refusal:
// the call never started a provider turn, so there is no attempt outcome
// for the retry classifiers to act on. ErrSessionBusy (mailbox busy,
// agent_run.go) and ErrAgentShuttingDown (shutdown admission gate) are
// returned before any provider request begins; the OS session-lock busy
// error (runOwned) refuses after mailbox admission but before the first
// turn. R3-1: a refusal must never be reclassified as a transient turn
// failure — the stale/foreign message rows a concurrent caller left in
// the session are not this call's evidence.
func turnAttemptRefused(err error) bool {
	if errors.Is(err, ErrSessionBusy) || errors.Is(err, ErrAgentShuttingDown) {
		return true
	}
	var lockBusy *session.SessionLockBusyError
	return errors.As(err, &lockBusy)
}

// isQuotaLimit reports whether a 429 is a hard usage/quota wall (resets on
// the order of hours) rather than a momentary overload. The two share
// status 429, so we discriminate on the provider's message text.
func isQuotaLimit(providerErr *fantasy.ProviderError) bool {
	return classifyHardProviderLimit(providerErr, "", time.Now()) == providerLimitHard
}

// turnMadeProgress reports whether the assistant message carries any real
// output — text, reasoning, or a tool call (even a partial one). A turn
// that made progress must never be re-run: it would duplicate work the
// user already has.
func turnMadeProgress(msg message.Message) bool {
	return strings.TrimSpace(msg.FullText()) != "" ||
		strings.TrimSpace(msg.FullReasoningText()) != "" ||
		len(msg.ToolCalls()) > 0
}

// shouldRetryStalledMessage decides whether a watchdog-stalled assistant
// message warrants re-running the turn. The retry exists to recover from
// turns where the provider never delivered anything; ANY content reaching
// the assistant — text, reasoning, even a half-emitted tool call — proves
// the server received and processed the prompt, and re-running would just
// duplicate the user message in the DB and burn tokens redoing work the
// user already (partially) has.
//
// Returns false for any non-stalled finish reason (including nil), so the
// caller can pass the last assistant message unconditionally.
func shouldRetryStalledMessage(msg message.Message) bool {
	fp := msg.FinishPart()
	if fp == nil {
		return false
	}
	if fp.Reason != message.FinishReasonError || fp.Message != streamStalledFinishTitle {
		return false
	}
	return !turnMadeProgress(msg)
}

// ownAttemptAssistantMessage returns the assistant message row THIS
// call's attempt wrote, identified by the ID its turn reported through
// SessionAgentCall.OnAssistantMessageCreated. The lookup is scoped to the
// session, so an ID belonging to any other session never qualifies. ok is
// false on a DB error or when no assistant row with that ID exists here
// (deleted by a concurrent compaction, or the attempt never wrote one) --
// callers treat that as "no owned evidence, nothing to retry".
func (c *coordinator) ownAttemptAssistantMessage(ctx context.Context, sessionID, msgID string) (message.Message, bool) {
	if c.messages == nil {
		return message.Message{}, false
	}
	msgs, err := c.messages.List(ctx, sessionID)
	if err != nil {
		return message.Message{}, false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].ID == msgID {
			return msgs[i], msgs[i].Role == message.Assistant
		}
	}
	return message.Message{}, false
}

// shouldRetryTurn decides whether a finished turn should be transparently
// re-run. A turn qualifies ONLY if it ended in error WITHOUT producing any
// content (so a re-run cannot clobber a partial answer) AND the failure is
// a transient provider/network hiccup rather than an operator-actionable
// condition. This generalizes the original stall-only retry: a watchdog
// stall is one transient class among several.
//
// Decision order:
//   - admission refusal (ErrSessionBusy/ErrAgentShuttingDown/OS session-lock
//     busy — turnAttemptRefused) → no attempt ran, don't retry
//   - no owned evidence (the attempt wrote no assistant row) → don't retry
//   - the attempt's own row: clean finish / user-cancel finish → don't retry
//   - turn produced any content                                 → don't retry
//   - persisted "Stream stalled" title (err is context.Canceled) → retry
//   - turn returned no error (empty-stream close)                → retry
//   - otherwise classify the returned error                      → transient/rate limit?
func (c *coordinator) shouldRetryTurn(ctx context.Context, sessionID string, err error, attemptAssistantMsgID string) bool {
	// R3-1: an admission refusal means this call never started a provider
	// request — classify nothing, not even the message lookup.
	if turnAttemptRefused(err) {
		return false
	}
	// R3-1 (round 5): evidence ownership. The only assistant message this
	// call may classify is the row its own attempt wrote, reported via
	// OnAssistantMessageCreated. An empty ID means the attempt produced no
	// assistant row (refused before the turn, or it failed before the
	// first provider step) — nothing to act on. The session's last
	// assistant row at classification time may belong to a concurrent
	// caller whose turn interleaved after ours returned: a newer ID is
	// not ownership, so the session-wide last row is never consulted.
	if attemptAssistantMsgID == "" {
		return false
	}
	msg, ok := c.ownAttemptAssistantMessage(ctx, sessionID, attemptAssistantMsgID)
	if !ok {
		return false
	}
	fp := msg.FinishPart()
	if fp == nil || fp.Reason != message.FinishReasonError {
		return false
	}
	if turnMadeProgress(msg) {
		return false
	}
	if fp.Message == streamStalledFinishTitle {
		// A call armed with a positive CallOptions.IdleTimeout (`rush run
		// --idle-timeout`) wants a stall to end the run outright, not
		// retry — see IdleTimeout's doc in call_options.go.
		if callOpts := callOptionsFrom(ctx); callOpts != nil && callOpts.IdleTimeout > 0 {
			return false
		}
		return true
	}
	if err == nil {
		return true
	}
	// rate limit are both retryable; everything terminal is not.
	return classifyProviderError(err) != classTerminal
}

// shouldContinueTurn decides whether a turn that already produced partial
// content (text/reasoning/a tool call) before failing should be resumed via
// a CONTINUATION prompt, as opposed to a blind resend (shouldRetryTurn) or
// a terminal failure. This is the path shouldRetryTurn deliberately never
// takes: turnMadeProgress(msg) == true makes shouldRetryTurn bail out, on
// the theory that a blind resend of the same prompt would either duplicate
// or discard that partial work. shouldContinueTurn covers exactly that
// case for a transient failure -- the real-world shape a watchdog stall
// or rate limit actually takes, since either can fire well after the model
// started streaming a reply.
//
// Returns the partial assistant message and true only when: no admission
// refusal is present (turnAttemptRefused — no attempt ran, R3-1), the
// attempt DID write an assistant row of its own and that row (not the
// session's last row, which a concurrent caller's newer turn can own) has
// FinishReasonError, it DID make progress, and the failure reads as
// transient -- either the persisted "Stream stalled" finish title
// (matching shouldRetryTurn's stall check), or a retryable
// classifyProviderError class (transient or rate limit). A nil err paired with
// progress is left alone (returns false): that combination doesn't
// correspond to any known transient signal (the nil-err retry path exists
// only for the empty-stream-close case, which by definition has no
// content), so it's surfaced rather than guessed at.
func (c *coordinator) shouldContinueTurn(ctx context.Context, sessionID string, err error, attemptAssistantMsgID string) (message.Message, bool) {
	// R3-1: an admission refusal means this call never started a provider
	// request — classify nothing, not even the message lookup.
	if turnAttemptRefused(err) {
		return message.Message{}, false
	}
	// R3-1 (round 5): evidence ownership — see shouldRetryTurn. The
	// continuation, if any, is built from the attempt's OWN partial row
	// even when a concurrent caller's newer message is the session's last
	// assistant row by the time classification runs.
	if attemptAssistantMsgID == "" {
		return message.Message{}, false
	}
	msg, ok := c.ownAttemptAssistantMessage(ctx, sessionID, attemptAssistantMsgID)
	if !ok {
		return message.Message{}, false
	}
	fp := msg.FinishPart()
	if fp == nil || fp.Reason != message.FinishReasonError {
		return message.Message{}, false
	}
	if !turnMadeProgress(msg) {
		return message.Message{}, false
	}
	if fp.Message == streamStalledFinishTitle {
		// See the matching check in shouldRetryTurn.
		if callOpts := callOptionsFrom(ctx); callOpts != nil && callOpts.IdleTimeout > 0 {
			return message.Message{}, false
		}
		return msg, true
	}
	if err == nil {
		return message.Message{}, false
	}
	if classifyProviderError(err) != classTerminal {
		return msg, true
	}
	return message.Message{}, false
}
