package app

import (
	"context"
	"errors"
	"log/slog"
)

// endedReasonWriteAttempts bounds the best-effort ended_reason write: each
// attempt gets a fresh cleanupTimeout budget detached from the run's context.
// A single attempt was observed dying on "context deadline exceeded" while a
// burst of concurrent loop exits contended for the same SQLite write lock --
// the exit envelope had already been flushed, nothing retried, and the row
// kept an empty ended_reason forever, which the session classifier then
// reports as a bare "idle: no live work and no recorded end". Three fresh
// budgets ride out a seconds-long contention window while staying far below
// the 30s busy_timeout a caller could observe as a hang.
const endedReasonWriteAttempts = 3

// writeEndedReasonWithRetry runs write up to endedReasonWriteAttempts times,
// each attempt on a fresh cleanupTimeout budget derived from parent (its
// cancellation and deadline never leak into the attempt). Returns nil once
// write succeeds, otherwise the last error.
func writeEndedReasonWithRetry(parent context.Context, write func(ctx context.Context) error) error {
	var err error
	for attempt := 0; attempt < endedReasonWriteAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), cleanupTimeout)
		err = write(ctx)
		cancel()
		if err == nil {
			return nil
		}
	}
	return err
}

// persistEndedReason is the ONLY writer of the session row's ended_reason for a
// `rush run` loop (R8A-1): the column is empty while the run is in progress
// (ExecuteRun's per-turn write is suppressed for the loop's turns, loopTurn)
// and holds the run's exit reason afterwards -- the envelope's exit_reason the
// loop is about to flush. Every loop exit (Ctrl-C or --timeout while waiting,
// a stuck debt, a cap or cancel caught by the precheck or while waiting, an
// unreadable DB, the ordinary close) goes through exit or flushed, which call
// it. The write is retried on fresh cleanupTimeout budgets (see
// writeEndedReasonWithRetry): a one-shot budget was observed dropping the
// write under concurrent loop-exit bursts. A run that crashed (kill -9) never
// reaches it and keeps the empty value.
//
// Without an envelope (the launched first turn returned none) the reason is
// derived from the error; a first turn that never launched (refused, lock
// busy) wrote nothing and records nothing.
func (l *cliLoop) persistEndedReason(final *RunResult, err error) {
	reason := l.endedReasonFor(final, err)
	if reason == "" || l.sessionID == "" {
		return
	}
	if writeErr := writeEndedReasonWithRetry(l.ctx, func(ctx context.Context) error {
		return l.recordEndedReason(ctx, reason)
	}); writeErr != nil {
		slog.Warn("Failed to persist ended_reason at loop exit", "session_id", l.sessionID, "reason", reason, "err", writeErr)
	}
}

// recordEndedReason writes reason unless the row already holds it. The
// per-attempt read keeps the idempotent skip when a retry follows a write
// that failed on the caller's side after landing.
func (l *cliLoop) recordEndedReason(ctx context.Context, reason string) error {
	if l.app.DB != nil {
		var current string
		qErr := l.app.DB().QueryRowContext(ctx,
			`SELECT COALESCE(ended_reason, '') FROM sessions WHERE id = ?`, l.sessionID).Scan(&current)
		if qErr == nil && current == reason {
			return nil
		}
	}
	return l.app.Sessions.SetEndedReason(ctx, l.sessionID, reason)
}

// endedReasonFor is the value persistEndedReason records ("" = record nothing).
func (l *cliLoop) endedReasonFor(final *RunResult, err error) string {
	switch {
	case l.refusedByOwner:
		return ""
	case final != nil && final.ExitReason != "":
		return final.ExitReason
	case final != nil:
		return "done"
	case !l.firstSubmitted || err == nil:
		return ""
	case exitedCanceled(nil, err) || l.ctx.Err() != nil:
		return "canceled"
	}
	var inc *runIncompleteError
	if errors.As(err, &inc) && inc.reason != "" {
		return inc.reason
	}
	return "error"
}
