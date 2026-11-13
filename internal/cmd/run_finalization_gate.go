package cmd

import (
	"context"
	"io"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
)

// runFinalizationHoldGate wraps the real terminal gate for one `rush run`.
// The owner's finalization record is created DURING the run, so a pre-run
// AllowTerminal on the real gate would always pass and the hold would never
// engage; this wrapper forces the buffered path and leaves the decision to
// the post-run AwaitSealed.
type runFinalizationHoldGate struct {
	inner session.FinalizationTerminalGate
}

// AllowTerminal always refuses so the wrapper's output is buffered; the
// inner gate is never consulted before the run.
func (g runFinalizationHoldGate) AllowTerminal(context.Context, string) bool { return false }

// AwaitSealed delegates to the real gate with the owner resolved after the
// run (res.SessionID). A nil inner gate has nothing to wait for.
func (g runFinalizationHoldGate) AwaitSealed(ctx context.Context, ownerSessionID string) error {
	if g.inner == nil {
		return nil
	}
	return g.inner.AwaitSealed(ctx, ownerSessionID)
}

// runGatedNonInteractive resolves the finalization hold gate for one `rush
// run` and delivers the run's terminal stdout through
// runNonInteractiveAwaitingSeal. With finalizationEnabled false, a nil
// appGate provider, or a provider that returns nil, the gate stays nil and
// the legacy byte-for-byte path runs (run(stdout) directly, no buffering).
// The owner is resolved after the run: the run result's session id when
// known, else the pre-run knownSessionID.
func runGatedNonInteractive(
	ctx context.Context,
	finalizationEnabled bool,
	appGate func() session.FinalizationTerminalGate,
	knownSessionID string,
	stdout io.Writer,
	run func(out io.Writer) (*app.RunResult, error),
) (*app.RunResult, error) {
	var gate session.FinalizationTerminalGate
	if finalizationEnabled && appGate != nil {
		if inner := appGate(); inner != nil {
			gate = runFinalizationHoldGate{inner: inner}
		}
	}
	return runNonInteractiveAwaitingSeal(ctx, gate, knownSessionID, stdout, run,
		func(res *app.RunResult) string {
			if res != nil && res.SessionID != "" {
				return res.SessionID
			}
			return knownSessionID
		})
}
