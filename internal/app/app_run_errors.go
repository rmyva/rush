// Outcome classification helpers for a finished non-interactive run:
// runIncompleteError, the runFailed predicate, session-busy guidance,
// and cancelledRunError, which preserves the richer abort reason.

package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	sqlitedriver "modernc.org/sqlite"
)

// sqliteConstraintCode is SQLite's primary result code for ANY constraint
// violation (SQLITE_CONSTRAINT, https://sqlite.org/rescode.html) -- the
// low 8 bits of the driver's reported code regardless of which more
// specific extended code (UNIQUE, PRIMARYKEY, NOTNULL, ...) it actually
// carries. Named so isSessionsIDConstraintError's masking logic below reads
// as intent rather than a bare magic number -- mirrors
// isPermanentOrphanOutboxDrainError's identical `Code() & 0xff == 19` check
// in internal/session/run_queue_orphan_drain.go, which documents (having
// verified it against this project's actual observed driver behavior) that
// modernc.org/sqlite reports the EXTENDED code by default, so masking with
// 0xff is what recovers the primary code this comparison needs.
const sqliteConstraintCode = 19

// isSessionsIDConstraintError reports whether err is a SQLite PRIMARY
// KEY/UNIQUE constraint violation on sessions.id specifically — the shape
// produced when two `rush run --session <id>` processes race the very
// first INSERT for an id that has never existed before (task #605).
//
// Two layers, in order:
//
//  1. Typed fast path: modernc.org/sqlite's *sqlite.Error exposes a
//     numeric Code() (see isPermanentOrphanOutboxDrainError in
//     internal/session/run_queue_orphan_drain.go for the same &0xff
//     masking, applied there against this exact codebase's confirmed
//     driver behavior). A match narrows this to "some constraint was
//     violated", not specifically sessions.id — SQLite's code alone does
//     not carry the offending table/column, only sqlite3_errmsg's text
//     does — so the substring check below still runs to confirm the
//     column identity even when the typed check matches.
//  2. Textual fallback: err's message chain must contain "constraint
//     failed" (used verbatim by both drivers this project can be built
//     with — see modernc.org/sqlite's error.go ErrorCodeString table and
//     github.com/ncruces/go-sqlite3's internal/sqlite3_wrap.ErrorCodeString,
//     both confirmed by reading the vendored driver source at the versions
//     pinned in go.mod, v1.50.1 and v0.34.2 respectively) AND
//     "sessions.id". This is the ONLY layer active when the typed check
//     above cannot run (github.com/ncruces/go-sqlite3, the fallback driver
//     on platforms connect_ncruces.go's build tag selects, is not
//     type-asserted here to avoid a second build-tag-gated copy of this
//     function; its errors fall straight through to this textual layer)
//     and is what actually identifies WHICH column/table was violated on
//     every path.
//
// A previous version of this comment additionally claimed the driver could
// render this as "constraint violation" (not "failed"). That phrase does
// not appear ANYWHERE in either driver's own error-text-producing code
// (confirmed by reading both drivers' vendored source: modernc's
// error.go/conn.go always use SQLite's own sqlite3_errstr wording, which is
// "constraint failed", never "constraint violation"; ncruces's
// internal/sqlite3_wrap.ErrorCodeString hard-codes "sqlite3: constraint
// failed" for the same code) — it was never observed, only assumed, and is
// removed rather than carried forward unverified.
//
// Deliberately narrow: only sessions.id, and only a constraint failure.
// This must NOT match every database error — a genuinely broken DB (disk
// full, corruption, permission denied, a constraint violation on some
// OTHER table/column) has to keep surfacing as-is so the operator sees a
// real failure instead of a swallowed one. See resolveSession's caller for
// why this matters: an earlier pass in this project quarantined entries on
// ANY error instead of a proven-permanent one, which is exactly the
// category of bug this narrow match exists to avoid repeating -- and see
// TestResolveSession_CreationRace_OtherConstraintNotSwallowed /
// TestIsSessionsIDConstraintError_MessagesIDNotMatched for the regression
// coverage proving a constraint violation on a DIFFERENT column (e.g.
// messages.id) is never swallowed by either layer.
func isSessionsIDConstraintError(err error) bool {
	if err == nil {
		return false
	}

	// Typed fast path (layer 1): narrows "is this a constraint violation at
	// all" without depending on driver wording, but still cannot identify
	// WHICH column by code alone -- the substring check below is not
	// skipped, only reached with higher confidence.
	var sqliteErr *sqlitedriver.Error
	isTypedConstraint := errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqliteConstraintCode

	msg := err.Error()
	isTextualConstraint := strings.Contains(msg, "constraint failed")
	if !isTypedConstraint && !isTextualConstraint {
		return false
	}
	return strings.Contains(msg, "sessions.id")
}

// runIncompleteError marks a non-interactive run that finished but did not
// complete its work cleanly (in-band provider error / stall, cancellation,
// timeout, or a max_tokens truncation). The envelope / final text was already
// emitted to stdout; this error exists only to drive a non-zero process exit
// so orchestrators and CI can branch on success without parsing stdout.
type runIncompleteError struct {
	reason string
	detail string
	// cause, when non-nil, is the underlying run error this envelope
	// failure was built from (R1-4). Unwrapping it keeps errors.Is-based
	// classification working on the ENVELOPE path, not just the
	// early-return path: a fail-fast busy rejection that surfaces through
	// the finished turn (ExecuteRun's race loser) must still satisfy
	// errors.Is(err, agent.ErrSessionBusy) for SDK consumers and
	// orchestrators, the same way sessionBusyGuidance classifies it.
	// Error()'s text is unchanged.
	cause error
}

// RunIncompleteError is the exit-code error returned when a run did not
// complete its work cleanly.
type RunIncompleteError = runIncompleteError

// NewRunIncompleteError creates the non-zero-exit error used when work ran
// but did not complete cleanly.
func NewRunIncompleteError(reason, detail string) *RunIncompleteError {
	return &runIncompleteError{reason: reason, detail: detail}
}

// ErrRunQueued marks a legacy mailbox admission that was accepted for a
// different active owner. The prompt remains owned by that mailbox and this
// invocation must not claim a completed result.
var ErrRunQueued = errors.New("run queued behind an active session")

type runQueuedError struct {
	sessionID string
}

func (e *runQueuedError) Error() string {
	return fmt.Sprintf("run for session %q was queued behind an active run; this invocation did not execute it", e.sessionID)
}

func (e *runQueuedError) Unwrap() error {
	return errors.Join(ErrRunQueued, agent.ErrSessionBusy)
}

func (e *runIncompleteError) Error() string {
	if e.detail != "" {
		return fmt.Sprintf("run did not complete cleanly (%s): %s", e.reason, e.detail)
	}
	return fmt.Sprintf("run did not complete cleanly (%s)", e.reason)
}

func (e *runIncompleteError) Unwrap() error { return e.cause }

// runFailed reports whether a finished non-interactive turn should map to a
// non-zero exit code. A clean end_turn (or a bare finish with no captured
// reason) is success; a hard error, a cancellation/timeout, an in-band error
// finish (stall / provider error / empty stream), or a max_tokens truncation
// are all "did not finish the work".
func runFailed(finalReason string, runErr error, isCanceled bool) bool {
	if runErr != nil || isCanceled {
		return true
	}
	switch message.FinishReason(finalReason) {
	case message.FinishReasonError, message.FinishReasonCanceled, message.FinishReasonMaxTokens:
		return true
	default:
		return false
	}
}

// isPeakHoursStop reports that the run's error is a peak-hours refusal: a
// deliberate, resumable stop (docs/peak-hours.md §6) where no terminal
// assistant message was expected and an optional reminder turn must not
// fire. errors.As-based so every wrap shape (DrainNotAttemptedError,
// ErrCallAlreadyAttempted, "failed to start agent processing stream: %w")
// keeps it identifiable.
func isPeakHoursStop(err error) bool {
	var peak *agent.PeakHoursError
	return errors.As(err, &peak)
}

// sessionBusyGuidance turns a "session already in use" failure into the
// sentence an operator can act on: who holds it, why a second `rush run`
// cannot attach, and the inject command that can.
func sessionBusyGuidance(sessionID string, err error) string {
	var busyErr *session.SessionLockBusyError
	holder := ""
	switch {
	case errors.As(err, &busyErr):
		holder = "another live rush process"
		if busyErr.HolderPID > 0 {
			holder = fmt.Sprintf("rush process PID %d", busyErr.HolderPID)
		}
	case errors.Is(err, agent.ErrSessionBusy):
		holder = "this rush process"
	default:
		return ""
	}
	return fmt.Sprintf(
		"session %q is already running in %s. `rush run --session %s ...` starts a new turn and cannot attach to an active one. To push a message into the running turn, use: rush sessions inject %s -m <message>",
		sessionID, holder, sessionID, sessionID,
	)
}

// cancelledRunError builds the runIncompleteError for the terse/--stream
// (non-JSON) isCanceled path.
//
// A forced abort (peak-hours mid-turn stop, max-cost, max-tokens, a DB
// cancel-request) cancels the run's context to unblock the in-flight
// generation — which races the specific error each of those paths already
// persisted onto the assistant message via AddFinish (see agent.go's
// OnStepFinish). Depending on that race, the run's returned error can end up
// being the generic context.Canceled instead of the specific one, so this
// must not drop the rich detail on the floor: if the last assistant message
// finished with FinishReasonError and carries a message/details, surface
// THAT instead of a bare "cancelled" that gives the operator no clue why. A
// genuine, unrecorded Ctrl+C never runs AddFinish first, so finalErrTitle
// stays empty and this falls through to the original bare behavior
// unchanged.
func cancelledRunError(runErr error, finalReason, finalErrTitle, finalErrDetails string) *runIncompleteError {
	// 1. Check the raw runErr first: a forced mid-turn abort (peak-hours,
	//    max-cost, max-tokens) returns a specific error via OnStepFinish
	//    and THEN calls cancel(). Because cancel() races the event-loop
	//    that populates finalReason/finalErrTitle, the event-loop may
	//    never see the FinishReasonError message — but the specific error
	//    is always available in runErr. Use it when it's richer than a
	//    bare context.Canceled.
	if runErr != nil {
		errText := runErr.Error()
		if !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded) && errText != "" {
			return &runIncompleteError{reason: "cancelled", detail: errText}
		}
	}
	// 2. Fallback: check the finish detail from the event-loop (works when
	//    the event-loop DID catch the updated message before the context
	//    died). A genuine unrecorded Ctrl+C (no FinishReasonError persisted)
	//    still falls through to bare "cancelled".
	if finalReason == string(message.FinishReasonError) && (finalErrTitle != "" || finalErrDetails != "") {
		detail := finalErrTitle
		if finalErrDetails != "" {
			if detail != "" {
				detail += ": "
			}
			detail += finalErrDetails
		}
		return &runIncompleteError{reason: "cancelled", detail: detail}
	}
	return &runIncompleteError{reason: "cancelled"}
}

// turnRefusedByOwner reports that a turn never started because another owner
// holds the session: a lock held by another process (a web tab's human turn,
// `sessions inject`) or an in-process owner. Such a turn ran nothing.
func turnRefusedByOwner(err error) bool {
	var busy *session.SessionLockBusyError
	return errors.As(err, &busy) || errors.Is(err, agent.ErrSessionBusy)
}
