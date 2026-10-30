// wakeSession: the single wake primitive (docs/plans/2026-09-28-async-
// phase4-durable-core.md sec.3.4, attempts design 1.6). It persists nothing
// itself: by the time a caller reaches it the fact is durably committed (a
// batch completion row or a session_notices row). Its job
// is to decide -- through the ONE launch predicate drainDecision -- whether a
// Drain call is submitted to the session's owning driver: idle becomes owner
// like any call, busy queues (mailbox_queue.go's mergeQueuedCall). The
// Drain's own turn-start pull moves the notice into history. Nothing runs
// after Run returns: the attempt is accounted by the turn loop that ran it
// (drain_attempt.go).
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
)

// wakeSession launches (at most) one Drain for sessionID. fact=true means the
// caller just committed a wake=1 fact: the session's hint is bumped first so
// every hint-driven waiter sees it whatever happens next. fact=false is a
// release or tick re-check: it bumps nothing (a hint fed by non-facts would
// reopen the launch gate on its own). A session driven by a live `rush run`
// loop in this process gets the hint only.
func (c *coordinator) wakeSession(ctx context.Context, sessionID string, fact bool) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("wakeSession panic", "session_id", sessionID, "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("wakeSession panic: %v", r)
		}
	}()
	if fact {
		c.driverHints.bumpHint(sessionID)
	}
	if c.driverHints.isExternalDriver(sessionID) {
		return nil // the loop re-evaluates its own scope from the DB
	}

	// Only the decision reads are bounded; the Drain itself is not.
	decideCtx, cancel := context.WithTimeout(ctx, recheckDebtCheckBudget)
	debt, v, decErr := c.drainDecision(decideCtx, sessionID, fact)
	cancel()
	if decErr != nil {
		c.addToRecheckSet(sessionID)
		return fmt.Errorf("wakeSession: launch decision: %w", decErr)
	}
	if !debt {
		return nil
	}
	switch v.kind {
	case drainPaced:
		c.addToRecheckSet(sessionID)
		return nil
	case drainDeferred:
		if v.recheck {
			c.addToRecheckSet(sessionID)
		}
		return nil
	case drainStuck:
		return nil
	}

	call, prep, buildErr := c.drainCallFor(ctx, sessionID)
	if buildErr != nil {
		return c.handleDrainBuildError(sessionID, buildErr)
	}
	// The Continuation owner lives exactly across the dispatch: released
	// on every exit from here, including the Run-error path.
	if prep != nil {
		defer prep.release()
	}
	if _, runErr := c.agentFor(sessionID).Run(ctx, call); runErr != nil {
		slog.Warn("drain call failed after its underlying notice was committed",
			"session_id", sessionID, "err", runErr)
		return runErr
	}
	return nil
}

// handleDrainBuildError routes one drainCallFor failure (gh#11a): an
// unavailable process-bound binding is TERMINAL FOR THIS EPOCH — it cannot
// come back on a timer, so the refusal must not re-arm the timed recheck
// pass. The durable debt is preserved untouched (never counted, never
// settled); a newly admitted explicit epoch re-evaluates on its own. Any
// other refusal keeps the historical paced retry (noteDrainRefused).
func (c *coordinator) handleDrainBuildError(sessionID string, buildErr error) error {
	if errors.Is(buildErr, ErrDiskProviderUnavailable) || errors.Is(buildErr, ErrCredentialBindingUnavailable) {
		c.clearFromRecheckSet(sessionID)
		slog.Warn("drain launch refused: a process-bound drain dependency is unavailable; deferred until explicit re-supply",
			"session_id", sessionID, "err", buildErr)
		return fmt.Errorf("wakeSession: build drain call: %w", buildErr)
	}
	c.noteDrainRefused(sessionID, buildErr)
	return fmt.Errorf("wakeSession: build drain call: %w", buildErr)
}
