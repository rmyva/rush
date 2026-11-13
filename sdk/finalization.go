package sdk

import (
	"context"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/session"
)

// FinalizationTerminalGate is a re-export alias of the internal/session
// seam, following the sdk.go pattern: aliases, not new types.
type FinalizationTerminalGate = session.FinalizationTerminalGate

// FinalizationGateError is the public typed error a host receives instead
// of a *RunResult when the owner's seal state could not be confirmed. The
// delivery is fail-closed: no RunResult is published for that call.
type FinalizationGateError struct {
	// OwnerSessionID is the owner whose seal state was not confirmed.
	OwnerSessionID string

	// GateErr is why the seal was not confirmed: ctx.Err() or a store
	// read error surfaced by the gate.
	GateErr error

	// RunErr is the wrapped run's own error, if any; it may be nil.
	RunErr error
}

// Error mentions the owner and the gate reason, and appends the run error
// when one is present.
func (e *FinalizationGateError) Error() string {
	msg := fmt.Sprintf("run result for session %q held: seal not confirmed: %v",
		e.OwnerSessionID, e.GateErr)
	if e.RunErr != nil {
		msg += fmt.Sprintf("; run error: %v", e.RunErr)
	}
	return msg
}

// Unwrap exposes both the gate reason and the run error so errors.Is and
// errors.As can find either cause.
func (e *FinalizationGateError) Unwrap() []error {
	errs := []error{e.GateErr}
	if e.RunErr != nil {
		errs = append(errs, e.RunErr)
	}
	return errs
}

// RunAwaitingSeal wraps a single SDK run so the host receives the
// *RunResult only after the gate confirms the owner's seal. Calling it
// again runs and delivers exactly once per call: each invocation performs
// its own run and its own await. The wrapper prints nothing — handing the
// RunResult to the host is the terminal notification itself.
func RunAwaitingSeal(
	ctx context.Context,
	gate FinalizationTerminalGate,
	ownerSessionID string,
	run func() (*RunResult, error),
) (*RunResult, error) {
	if gate == nil {
		gate = session.DefaultFinalizationTerminalGate()
	}
	// Fast path: deliver immediately, byte-for-byte the plain run call.
	if gate.AllowTerminal(ctx, ownerSessionID) {
		return run()
	}
	res, runErr := run()
	if err := gate.AwaitSealed(ctx, ownerSessionID); err != nil {
		// Fail-closed: the run result is withheld, never delivered.
		return nil, &FinalizationGateError{
			OwnerSessionID: ownerSessionID,
			GateErr:        err,
			RunErr:         runErr,
		}
	}
	return res, runErr
}
