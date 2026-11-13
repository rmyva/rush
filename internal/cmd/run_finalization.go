package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
)

// finalizationGateRefusal is the typed error for "terminal delivery was
// refused because the owner's seal state could not be confirmed". It is
// fail-closed: the held output is discarded, never printed.
type finalizationGateRefusal struct {
	// OwnerSessionID is the owner whose seal state was not confirmed.
	OwnerSessionID string

	// GateErr is why the seal was not confirmed: ctx.Err() or a store
	// read error surfaced by the gate's AwaitSealed.
	GateErr error

	// RunErr is the run's own error, if the run itself failed; it may be
	// nil when the run succeeded but the delivery was still refused.
	RunErr error
}

// Error mentions the owner and the gate reason, and appends the run error
// when one is present.
func (e *finalizationGateRefusal) Error() string {
	msg := fmt.Sprintf("terminal output for session %q held: seal not confirmed: %v",
		e.OwnerSessionID, e.GateErr)
	if e.RunErr != nil {
		msg += fmt.Sprintf("; run error: %v", e.RunErr)
	}
	return msg
}

// Unwrap exposes both the gate reason and the run error so errors.Is and
// errors.As can find either cause.
func (e *finalizationGateRefusal) Unwrap() []error {
	errs := []error{e.GateErr}
	if e.RunErr != nil {
		errs = append(errs, e.RunErr)
	}
	return errs
}

// defaultRunFinalizationGate returns the inert default used before wiring:
// everything is allowed and output is never held.
func defaultRunFinalizationGate() session.FinalizationTerminalGate {
	return session.DefaultFinalizationTerminalGate()
}

// runNonInteractiveAwaitingSeal performs one `rush run` and delivers its
// terminal stdout only after the gate confirms the owner's record has left
// the active finalization phases.
//
// The ctx is passed through as-is: it already carries the `rush run`
// deadline installed by installRunDeadline, so a cancel or deadline while
// waiting yields a finalizationGateRefusal wrapping ctx.Err(). While the
// owner is actively running, stream mode loses token liveness — the output
// arrives only after the seal — which is exactly the requested behavior.
func runNonInteractiveAwaitingSeal(
	ctx context.Context,
	gate session.FinalizationTerminalGate,
	ownerSessionID string,
	stdout io.Writer,
	run func(out io.Writer) (*app.RunResult, error),
) (*app.RunResult, error) {
	if gate == nil {
		gate = session.DefaultFinalizationTerminalGate()
	}
	// Fast path: deliver byte-for-byte as before, with the exact writer.
	if gate.AllowTerminal(ctx, ownerSessionID) {
		return run(stdout)
	}
	var buf bytes.Buffer
	res, runErr := run(&buf)
	if err := gate.AwaitSealed(ctx, ownerSessionID); err != nil {
		// Fail-closed: discard the held output, do not print it.
		return res, &finalizationGateRefusal{
			OwnerSessionID: ownerSessionID,
			GateErr:        err,
			RunErr:         runErr,
		}
	}
	if _, copyErr := io.Copy(stdout, &buf); copyErr != nil {
		if runErr != nil {
			return res, runErr
		}
		return res, fmt.Errorf("flush held run output: %w", copyErr)
	}
	return res, runErr
}
