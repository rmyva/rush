package cmd

import (
	"errors"
	"io"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// The hold gate must force buffering even when the inner gate would allow,
// and must delegate AwaitSealed verbatim; a nil inner gate releases.
func TestRunFinalizationHoldGateAlwaysRefusesAndDelegatesAwait(t *testing.T) {
	t.Parallel()

	sealed := make(chan struct{})
	close(sealed)
	inner := &fakeRunTerminalGate{allow: true, sealed: sealed}
	hold := runFinalizationHoldGate{inner: inner}
	require.False(t, hold.AllowTerminal(t.Context(), "owner"),
		"the hold gate must force the buffered path even when the inner gate allows")
	require.NoError(t, hold.AwaitSealed(t.Context(), "owner"))
	require.Equal(t, int32(1), inner.awaitCalls.Load())
	require.Equal(t, int32(0), inner.allowCalls.Load(),
		"the inner gate must not be consulted on the pre-run AllowTerminal")
	require.NoError(t, (runFinalizationHoldGate{}).AwaitSealed(t.Context(), "owner"),
		"a nil inner gate has nothing to wait for")
}

// Flag off must keep the legacy path: the gate provider is never consulted
// and the run writes straight to the caller's writer.
func TestRunGatedNonInteractiveFlagOffKeepsLegacyPathWithoutGate(t *testing.T) {
	t.Parallel()

	providerCalls := 0
	provider := func() session.FinalizationTerminalGate {
		providerCalls++
		return &fakeRunTerminalGate{allow: true, awaitErr: errors.New("must not run")}
	}
	rec := &recordingWriter{}
	var gotWriter io.Writer
	run := func(out io.Writer) (*app.RunResult, error) {
		gotWriter = out
		_, err := out.Write([]byte("FINAL\n"))
		require.NoError(t, err)
		return &app.RunResult{SessionID: "s1"}, nil
	}

	res, err := runGatedNonInteractive(t.Context(), false, provider, "", rec, run)

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Same(t, rec, gotWriter.(*recordingWriter))
	require.Equal(t, "FINAL\n", rec.buf.String())
	require.Zero(t, providerCalls, "flag off must not build any gate")
}

// Flag on with a nil app gate keeps the legacy path: there is no store to
// consult, so buffering would hold output for a record that cannot exist.
func TestRunGatedNonInteractiveFlagOnNilAppGateKeepsLegacyPath(t *testing.T) {
	t.Parallel()

	rec := &recordingWriter{}
	var gotWriter io.Writer
	run := func(out io.Writer) (*app.RunResult, error) {
		gotWriter = out
		_, err := out.Write([]byte("FINAL\n"))
		require.NoError(t, err)
		return &app.RunResult{SessionID: "s2"}, nil
	}

	res, err := runGatedNonInteractive(t.Context(), true, func() session.FinalizationTerminalGate { return nil }, "", rec, run)

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Same(t, rec, gotWriter.(*recordingWriter))
	require.Equal(t, "FINAL\n", rec.buf.String())
}

// Flag on buffers the output until the inner gate's AwaitSealed releases,
// flushes exactly once, and awaits the owner resolved from the run result —
// not the (empty) pre-run session id.
func TestRunGatedNonInteractiveFlagOnBuffersUntilSealedAndUsesRunResultOwner(t *testing.T) {
	t.Parallel()

	sealed := make(chan struct{})
	inner := &fakeRunTerminalGate{allow: true, sealed: sealed, entered: make(chan struct{})}
	providerCalls := 0
	provider := func() session.FinalizationTerminalGate {
		providerCalls++
		return inner
	}
	rec := &recordingWriter{}
	runDone := make(chan struct{})
	run := func(out io.Writer) (*app.RunResult, error) {
		_, err := out.Write([]byte("FINAL"))
		require.NoError(t, err)
		close(runDone)
		return &app.RunResult{SessionID: "created-owner"}, nil
	}

	type outcome struct {
		res *app.RunResult
		err error
	}
	finished := make(chan outcome, 1)
	go func() {
		res, err := runGatedNonInteractive(t.Context(), true, provider, "", rec, run)
		finished <- outcome{res: res, err: err}
	}()

	<-runDone
	<-inner.awaitEntered()
	// Deterministic: the run finished and AwaitSealed started (and is still
	// blocked), so nothing can have been flushed yet.
	require.Equal(t, "", rec.buf.String(), "output must stay buffered while the record is active")
	require.Equal(t, int32(1), inner.awaitCalls.Load())
	require.Equal(t, []string{"created-owner"}, inner.awaitedOwners(),
		"the awaited owner must come from the run result, not the empty pre-run id")
	require.Equal(t, 1, providerCalls)

	close(sealed)
	got := <-finished
	require.NoError(t, got.err)
	require.NotNil(t, got.res)
	require.Equal(t, "created-owner", got.res.SessionID)
	require.Equal(t, "FINAL", rec.buf.String())
	require.Equal(t, 1, rec.writes, "the held output must flush exactly once")
}

// Flag on with a gate error after a successful run must print nothing:
// fail-closed discards the buffered output and returns a refusal.
func TestRunGatedNonInteractiveFlagOnGateErrorPrintsNothing(t *testing.T) {
	t.Parallel()

	errStore := errors.New("store read failed")
	sealed := make(chan struct{})
	close(sealed)
	inner := &fakeRunTerminalGate{allow: true, sealed: sealed, awaitErr: errStore}
	rec := &recordingWriter{}
	run := func(out io.Writer) (*app.RunResult, error) {
		_, err := out.Write([]byte("FINAL"))
		require.NoError(t, err)
		return &app.RunResult{SessionID: "own"}, nil
	}

	res, err := runGatedNonInteractive(t.Context(), true, func() session.FinalizationTerminalGate { return inner }, "", rec, run)

	var refusal *finalizationGateRefusal
	require.ErrorAs(t, err, &refusal)
	require.ErrorIs(t, err, errStore)
	require.Equal(t, "", rec.buf.String(), "fail-closed discards the held output")
	require.NotNil(t, res)
	require.Equal(t, "own", res.SessionID)
}

// The wrapper resolves the owner from the run result even when a pre-run id
// was known: the record is bound to the session the run actually created or
// continued.
func TestRunNonInteractiveAwaitingSealResolvesOwnerFromRunResult(t *testing.T) {
	t.Parallel()

	sealed := make(chan struct{})
	close(sealed)
	gate := &fakeRunTerminalGate{allow: false, sealed: sealed}
	rec := &recordingWriter{}
	run := func(out io.Writer) (*app.RunResult, error) {
		_, err := out.Write([]byte("FINAL"))
		require.NoError(t, err)
		return &app.RunResult{SessionID: "real-owner"}, nil
	}

	res, err := runNonInteractiveAwaitingSeal(
		t.Context(), gate, "pre-known", rec, run,
		func(res *app.RunResult) string { return res.SessionID },
	)

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, []string{"real-owner"}, gate.awaitedOwners(),
		"the real session id must be awaited, not the pre-run value")
	require.Equal(t, "FINAL", rec.buf.String())
}

// An unresolvable owner behind a failed run needs no fail-closed refusal:
// nothing can be waited for, so the run's own error is returned and the
// partial output is flushed once.
func TestRunNonInteractiveAwaitingSealUnknownOwnerFailedRunReturnsRunError(t *testing.T) {
	t.Parallel()

	errRun := errors.New("run failed before creating a session")
	gate := &fakeRunTerminalGate{allow: false, awaitErr: errors.New("must not await an unknown owner")}
	rec := &recordingWriter{}
	run := func(out io.Writer) (*app.RunResult, error) {
		_, err := out.Write([]byte("PARTIAL"))
		require.NoError(t, err)
		return nil, errRun
	}

	res, err := runNonInteractiveAwaitingSeal(t.Context(), gate, "", rec, run, nil)

	require.Nil(t, res)
	require.Equal(t, errRun, err)
	require.Equal(t, int32(0), gate.awaitCalls.Load(), "an empty owner must short-circuit the await")
	require.Equal(t, "PARTIAL", rec.buf.String())
	require.Equal(t, 1, rec.writes)
}
