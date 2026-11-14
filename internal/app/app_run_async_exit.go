package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/agent"
)

// The loop's exit path: every way a `rush run` loop ends goes through finish
// exactly once (split out of app_run_async.go, which was at the 1000-line limit).

// exit computes the exit envelope's reason for err (reason, when set, is the
// exit_reason to record; otherwise a failed last Drain's own classification is
// carried over) and finishes the run.
func (l *cliLoop) exit(err error, reason string) (*RunResult, error) {
	final := l.final
	// Turn-stall abort: the turn died past its stall timeout with the
	// finish part already written — surface a dedicated "stalled"
	// exit_reason (non-zero exit, since err is non-nil).
	if err != nil && reason == "" && errors.Is(err, agent.ErrTurnStalled) {
		reason = "stalled"
	}
	if final != nil {
		switch {
		case err != nil && reason != "":
			final.ExitReason = reason
			final.Error = err.Error()
		case err != nil && l.lastFailed != nil && errors.Is(err, l.runErr):
			final.ExitReason = l.lastFailed.ExitReason
			final.Error = l.lastFailed.Error
		}
	}
	return l.finish(err)
}

// finish is the loop's ONE exit path: totals of every real turn are applied
// to the final envelope, the operator's honoured cancel request is cleared,
// the session row's ended_reason is made to match the envelope
// (persistEndedReason), and the envelope is flushed through the one common
// path; the caller gets (final, err). Every loop exit -- refusal give-up,
// ctx cancellation, a wait error, a stuck debt, or the ordinary scope-closed
// end -- goes through here exactly once.
func (l *cliLoop) finish(err error) (*RunResult, error) {
	l.final, err = classifyRunTimeout(l.ctx, l.final, err, l.sessionID, l.resumeRole())
	if l.lastBuffered == nil {
		l.lastBuffered = &bytes.Buffer{}
	}
	if l.final != nil {
		l.applyTotals(l.final)
		// A18: the budget for unfinished-todos reminders was spent without
		// the todos moving -- the orchestrator must see why the run ended
		// with the list still open.
		if l.todosGaveUp {
			if n, _, todoErr := l.openTodos(); todoErr == nil && n > 0 {
				l.final.Warnings = append(l.final.Warnings, fmt.Sprintf(
					"ended with unfinished todos: %d item(s) still pending/in_progress after %d reminder(s)", n, l.todoNudges))
			}
		}
	}
	l.clearHonouredCancel(l.final, err)
	l.persistEndedReason(l.final, err)
	if flushErr := flushLoopExit(l.output, l.mode, l.final, l.lastBuffered); flushErr != nil {
		return l.final, errors.Join(err, flushErr)
	}
	return l.final, err
}

func (l *cliLoop) exitCanceled() (*RunResult, error) {
	err := l.runErr
	if err == nil {
		err = l.ctx.Err()
	}
	if l.final == nil && l.ctx.Err() != nil && l.sessionID != "" {
		// The first turn was cut short before it produced an envelope: the
		// run still owes its caller one (session, reason, usage so far).
		l.final = &RunResult{SessionID: l.sessionID, ToolCalls: []ToolCallStat{}}
		if l.lastBuffered == nil {
			l.lastBuffered = &bytes.Buffer{}
		}
	}
	if l.final != nil && l.ctx.Err() != nil {
		l.final.ExitReason = "canceled"
		l.final.Error = l.ctx.Err().Error()
	}
	return l.finish(err)
}

func (l *cliLoop) exitWait(waitErr error) (*RunResult, error) {
	if l.final != nil {
		l.final.ExitReason = "canceled"
		if !errors.Is(waitErr, context.Canceled) && !(l.ctx.Err() != nil && errors.Is(waitErr, context.DeadlineExceeded)) {
			// A persistent DB-read failure is not a cancellation.
			l.final.ExitReason = "error"
		}
		l.final.Error = waitErr.Error()
	}
	return l.finish(waitErr)
}

func (l *cliLoop) exitPrecheck(err error) (*RunResult, error) {
	var inc *runIncompleteError
	reason := "error"
	if errors.As(err, &inc) {
		reason = inc.reason
	}
	return l.exit(err, reason)
}

// incompleteExitReason maps a run-terminating error onto the exit_reason of
// the give-up exits that used to hard-code "error". The error's own
// classification wins when it carries one (a peak-hours refusal carries
// "peak_hours"); everything else stays "error".
func incompleteExitReason(err error) string {
	var inc *runIncompleteError
	if errors.As(err, &inc) && inc.reason != "" {
		return inc.reason
	}
	return "error"
}
