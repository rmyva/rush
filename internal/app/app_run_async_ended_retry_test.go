package app

// FIX-STATUS (b) regression tests: the loop's ended_reason write used to be
// ONE best-effort attempt on a 2s budget; a concurrent-exit burst contending
// for the SQLite write lock outlasted it ("context deadline exceeded"), the
// envelope had already been flushed, and nothing retried -- the row kept an
// empty ended_reason forever.

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// flakyEndedReasonSet fails the first N SetEndedReason calls, then passes
// through to the wrapped service.
type flakyEndedReasonSet struct {
	session.Service
	calls atomic.Int32
	fail  atomic.Int32
}

func (f *flakyEndedReasonSet) SetEndedReason(ctx context.Context, id, reason string) error {
	if f.calls.Add(1) <= f.fail.Load() {
		return context.DeadlineExceeded
	}
	return f.Service.SetEndedReason(ctx, id, reason)
}

// TestPersistEndedReason_RetriesAfterATransientWriteFailure: a first failed
// write must not be the last word -- the second attempt records the reason.
//
// Revert-check: removing the retry (one attempt per budget) leaves the row
// empty and fails the ended_reason assertion.
func TestPersistEndedReason_RetriesAfterATransientWriteFailure(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "R8-X", 1, 1)
	})
	svc := &flakyEndedReasonSet{Service: h.app.Sessions}
	svc.fail.Store(1)
	h.app.Sessions = svc

	l := &cliLoop{app: h.app, ctx: t.Context(), sessionID: h.sessionID, firstSubmitted: true}
	l.persistEndedReason(&RunResult{SessionID: h.sessionID, ExitReason: "end_turn"}, nil)

	require.EqualValues(t, 2, svc.calls.Load(), "the failed first write must be retried exactly once more")
	require.Equal(t, "end_turn", sessionEndedReason(t, h.app, h.sessionID))
}

// TestPersistEndedReason_GivesUpAfterBoundedAttempts: an always-failing
// store stops after exactly endedReasonWriteAttempts calls -- bounded work,
// not an endless loop, and the final Warn keeps the last error.
//
// Revert-check: an unbounded retry never returns (the package would hang
// until the go test deadline); an attempt-count regression fails the
// equality.
func TestPersistEndedReason_GivesUpAfterBoundedAttempts(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "R8-X", 1, 1)
	})
	svc := &flakyEndedReasonSet{Service: h.app.Sessions}
	svc.fail.Store(1 << 30)
	h.app.Sessions = svc

	l := &cliLoop{app: h.app, ctx: t.Context(), sessionID: h.sessionID, firstSubmitted: true}
	l.persistEndedReason(&RunResult{SessionID: h.sessionID, ExitReason: "end_turn"}, nil)

	require.EqualValues(t, endedReasonWriteAttempts, svc.calls.Load())
	require.Empty(t, sessionEndedReason(t, h.app, h.sessionID),
		"an always-failing store leaves the recorded reason empty; the Warn is the only trace")
}

// TestWriteEndedReasonWithRetry_StopsOnFirstSuccess: the retry loop returns
// nil on the first successful write instead of exhausting all attempts.
//
// Revert-check: retrying past a success fails the calls assertion; dropping
// the retry fails the error assertion instead.
func TestWriteEndedReasonWithRetry_StopsOnFirstSuccess(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	err := writeEndedReasonWithRetry(context.Background(), func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			return context.DeadlineExceeded
		}
		return nil
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, calls.Load())
}

// TestWriteEndedReasonWithRetry_FreshBudgetPerAttempt: each attempt's
// context carries its own live deadline; a parent whose budget is already
// spent must not hand an exhausted context to the write.
//
// Revert-check: deriving the attempt context from the parent without
// WithoutCancel plus a fresh WithTimeout (the one-shared-budget regression)
// fails the Err/Deadline assertions on the first attempt.
func TestWriteEndedReasonWithRetry_FreshBudgetPerAttempt(t *testing.T) {
	spent, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-spent.Done() // the parent budget is already exhausted

	var attempts atomic.Int32
	err := writeEndedReasonWithRetry(spent, func(ctx context.Context) error {
		attempts.Add(1)
		require.Nil(t, ctx.Err(), "an attempt context must not start exhausted")
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "each attempt must carry a deadline")
		require.Greater(t, time.Until(deadline), time.Duration(0), "the fresh budget must be in the future")
		return errors.New("boom")
	})
	require.Error(t, err)
	require.EqualValues(t, endedReasonWriteAttempts, attempts.Load())
}
