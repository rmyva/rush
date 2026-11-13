package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// fakeRunTerminalGate is an in-memory gate stub: it counts calls and can
// block AwaitSealed on a channel until the test releases it.
type fakeRunTerminalGate struct {
	allow    bool
	sealed   <-chan struct{}
	awaitErr error

	allowCalls atomic.Int32
	awaitCalls atomic.Int32

	mu          sync.Mutex
	awaitOwners []string

	entered   chan struct{}
	enterOnce sync.Once
}

// awaitEntered returns a channel closed the first time AwaitSealed starts.
func (g *fakeRunTerminalGate) awaitEntered() <-chan struct{} {
	if g.entered == nil {
		g.entered = make(chan struct{})
	}
	return g.entered
}

// awaitedOwners returns every owner passed to AwaitSealed, in order.
func (g *fakeRunTerminalGate) awaitedOwners() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.awaitOwners...)
}

// AllowTerminal counts the call and returns the configured verdict.
func (g *fakeRunTerminalGate) AllowTerminal(context.Context, string) bool {
	g.allowCalls.Add(1)
	return g.allow
}

// AwaitSealed counts the call, records the owner, signals entry, waits for
// the sealed channel or ctx, then returns the configured error (nil by
// default).
func (g *fakeRunTerminalGate) AwaitSealed(ctx context.Context, owner string) error {
	g.awaitCalls.Add(1)
	g.mu.Lock()
	g.awaitOwners = append(g.awaitOwners, owner)
	g.mu.Unlock()
	g.enterOnce.Do(func() {
		if g.entered != nil {
			close(g.entered)
		}
	})
	if g.sealed != nil {
		select {
		case <-g.sealed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.awaitErr
}

// recordingWriter counts writes into an in-memory buffer.
type recordingWriter struct {
	buf    bytes.Buffer
	writes int
}

// Write forwards to the buffer and counts the call.
func (w *recordingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.buf.Write(p)
}

// The inert default gate must keep the legacy path intact: the exact writer
// is forwarded, output is delivered immediately, no await happens.
func TestRunNonInteractiveAwaitingSealInertGateDeliversLegacyOutput(t *testing.T) {
	t.Parallel()

	gate := session.DefaultFinalizationTerminalGate()
	rec := &recordingWriter{}
	var gotWriter io.Writer
	runs := 0
	run := func(out io.Writer) (*app.RunResult, error) {
		runs++
		gotWriter = out
		_, err := out.Write([]byte("FINAL\n"))
		require.NoError(t, err)
		return &app.RunResult{SessionID: "s1", FinalText: "FINAL\n"}, nil
	}

	res, err := runNonInteractiveAwaitingSeal(
		t.Context(), gate, "s1", rec, run, nil,
	)

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "s1", res.SessionID)
	require.Same(t, rec, gotWriter.(*recordingWriter))
	require.Equal(t, "FINAL\n", rec.buf.String())
	require.Equal(t, 1, runs)
}

// An allowing stub must take the fast path: no AwaitSealed call, output
// delivered even if AwaitSealed would have failed.
func TestRunNonInteractiveAwaitingSealAllowingStubSkipsAwait(t *testing.T) {
	t.Parallel()

	gate := &fakeRunTerminalGate{
		allow:    true,
		awaitErr: errors.New("await must not run on the fast path"),
	}
	rec := &recordingWriter{}
	run := func(out io.Writer) (*app.RunResult, error) {
		_, err := out.Write([]byte("FINAL\n"))
		require.NoError(t, err)
		return &app.RunResult{SessionID: "s2"}, nil
	}

	res, err := runNonInteractiveAwaitingSeal(
		t.Context(), gate, "s2", rec, run, nil,
	)

	require.NoError(t, err)
	require.Equal(t, "s2", res.SessionID)
	require.Equal(t, "FINAL\n", rec.buf.String())
	require.Equal(t, int32(0), gate.awaitCalls.Load())
}

// When the gate refuses, output is held until the seal opens and then is
// delivered exactly once, causally after AwaitSealed returns.
func TestRunNonInteractiveAwaitingSealHoldsOutputUntilSealed(t *testing.T) {
	t.Parallel()

	sealed := make(chan struct{})
	gate := &fakeRunTerminalGate{allow: false, sealed: sealed}
	rec := &recordingWriter{}
	runDone := make(chan struct{})
	finished := make(chan error, 1)
	var res *app.RunResult
	run := func(out io.Writer) (*app.RunResult, error) {
		_, err := out.Write([]byte("FINAL"))
		require.NoError(t, err)
		close(runDone)
		return &app.RunResult{SessionID: "s3"}, nil
	}

	go func() {
		var runErr error
		res, runErr = runNonInteractiveAwaitingSeal(
			t.Context(), gate, "s3", rec, run, nil,
		)
		finished <- runErr
	}()

	<-runDone
	// Deterministic: the flush is causally after AwaitSealed, which is
	// still blocked, so no concurrent write is possible here.
	require.Equal(t, "", rec.buf.String())
	require.Equal(t, 0, rec.writes)

	close(sealed)
	runErr := <-finished
	require.NoError(t, runErr)
	require.NotNil(t, res)
	require.Equal(t, "s3", res.SessionID)
	require.Equal(t, "FINAL", rec.buf.String())
	require.Equal(t, 1, rec.writes)
	require.Equal(t, int32(1), gate.awaitCalls.Load())
	require.Equal(t, int32(1), gate.allowCalls.Load())
}

// A store failure during the await must refuse delivery fail-closed: the
// successful run's output is discarded, never printed.
func TestRunNonInteractiveAwaitingSealRefusesDeliveryWhenAwaitFails(t *testing.T) {
	t.Parallel()

	errStore := errors.New("store read failed")
	gate := &fakeRunTerminalGate{allow: false, awaitErr: errStore}
	rec := &recordingWriter{}
	run := func(out io.Writer) (*app.RunResult, error) {
		_, err := out.Write([]byte("FINAL"))
		require.NoError(t, err)
		return &app.RunResult{SessionID: "s4"}, nil
	}

	res, err := runNonInteractiveAwaitingSeal(
		t.Context(), gate, "s4", rec, run, nil,
	)

	require.Error(t, err)
	var refusal *finalizationGateRefusal
	require.ErrorAs(t, err, &refusal)
	require.ErrorIs(t, err, errStore)
	require.Equal(t, "s4", refusal.OwnerSessionID)
	require.Equal(t, "", rec.buf.String())
	require.NotNil(t, res)
	require.Equal(t, "s4", res.SessionID)
}

// A context canceled while waiting must surface as a refusal wrapping
// context.Canceled, with nothing printed.
func TestRunNonInteractiveAwaitingSealCanceledContextWhileWaiting(t *testing.T) {
	t.Parallel()

	sealed := make(chan struct{})
	gate := &fakeRunTerminalGate{allow: false, sealed: sealed}
	rec := &recordingWriter{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run := func(out io.Writer) (*app.RunResult, error) {
		_, err := out.Write([]byte("FINAL"))
		require.NoError(t, err)
		return &app.RunResult{SessionID: "s5"}, nil
	}

	res, err := runNonInteractiveAwaitingSeal(
		ctx, gate, "s5", rec, run, nil,
	)

	require.Error(t, err)
	var refusal *finalizationGateRefusal
	require.ErrorAs(t, err, &refusal)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, "", rec.buf.String())
	require.NotNil(t, res)
}

// A failed run behind a passed gate must return the run error unchanged
// while the partial output is delivered exactly once.
func TestRunNonInteractiveAwaitingSealRunErrorPassthroughAfterSeal(t *testing.T) {
	t.Parallel()

	errRun := errors.New("run failed")
	sealed := make(chan struct{})
	close(sealed)
	gate := &fakeRunTerminalGate{allow: false, sealed: sealed}
	rec := &recordingWriter{}
	run := func(out io.Writer) (*app.RunResult, error) {
		_, err := out.Write([]byte("PARTIAL"))
		require.NoError(t, err)
		return &app.RunResult{SessionID: "s6"}, errRun
	}

	res, err := runNonInteractiveAwaitingSeal(
		t.Context(), gate, "s6", rec, run, nil,
	)

	require.ErrorIs(t, err, errRun)
	require.Equal(t, errRun, err)
	require.Equal(t, "PARTIAL", rec.buf.String())
	require.Equal(t, 1, rec.writes)
	require.NotNil(t, res)
}
