package sdk

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeSDKTerminalGate is an in-memory gate stub: it counts calls and can
// block AwaitSealed on a channel until the test releases it.
type fakeSDKTerminalGate struct {
	allow      bool
	sealed     <-chan struct{}
	awaitErr   error
	allowCalls atomic.Int32
	awaitCalls atomic.Int32
}

// AllowTerminal counts the call and returns the configured verdict.
func (g *fakeSDKTerminalGate) AllowTerminal(context.Context, string) bool {
	g.allowCalls.Add(1)
	return g.allow
}

// AwaitSealed counts the call, waits for the sealed channel or ctx, then
// returns the configured error (nil by default).
func (g *fakeSDKTerminalGate) AwaitSealed(ctx context.Context, _ string) error {
	g.awaitCalls.Add(1)
	if g.sealed != nil {
		select {
		case <-g.sealed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.awaitErr
}

// A nil gate must fall back to the inert default: the run result passes
// through immediately with no holding.
func TestRunAwaitingSealNilGateReturnsResultImmediately(t *testing.T) {
	t.Parallel()

	runs := 0
	run := func() (*RunResult, error) {
		runs++
		return &RunResult{SessionID: "s1"}, nil
	}

	res, err := RunAwaitingSeal(t.Context(), nil, "s1", run)

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "s1", res.SessionID)
	require.Equal(t, 1, runs)
}

// A refusing gate must hold the result until the seal opens; the result is
// delivered causally after AwaitSealed returns.
func TestRunAwaitingSealHoldsResultUntilSealed(t *testing.T) {
	t.Parallel()

	sealed := make(chan struct{})
	gate := &fakeSDKTerminalGate{allow: false, sealed: sealed}
	runDone := make(chan struct{})
	finished := make(chan error, 1)
	var res *RunResult
	run := func() (*RunResult, error) {
		defer close(runDone)
		return &RunResult{SessionID: "s2"}, nil
	}

	go func() {
		var runErr error
		res, runErr = RunAwaitingSeal(t.Context(), gate, "s2", run)
		finished <- runErr
	}()

	<-runDone
	// Deterministic: the result is causally after AwaitSealed, which is
	// still blocked, so the channel must still be empty.
	select {
	case runErr := <-finished:
		t.Fatalf("result delivered before seal: %v", runErr)
	default:
	}

	close(sealed)
	runErr := <-finished
	require.NoError(t, runErr)
	require.NotNil(t, res)
	require.Equal(t, "s2", res.SessionID)
	require.Equal(t, int32(1), gate.awaitCalls.Load())
}

// A gate failure must refuse the result: the host gets a typed
// FinalizationGateError wrapping the gate reason and no RunResult.
func TestRunAwaitingSealAwaitErrorRefusesResult(t *testing.T) {
	t.Parallel()

	errStore := errors.New("store read failed")
	gate := &fakeSDKTerminalGate{allow: false, awaitErr: errStore}
	run := func() (*RunResult, error) {
		return &RunResult{SessionID: "s3"}, nil
	}

	res, err := RunAwaitingSeal(t.Context(), gate, "s3", run)

	require.Error(t, err)
	var gateErr *FinalizationGateError
	require.ErrorAs(t, err, &gateErr)
	require.ErrorIs(t, err, errStore)
	require.Nil(t, res)
}

// A canceled context while waiting must surface as a refusal wrapping
// context.Canceled, with no RunResult.
func TestRunAwaitingSealCanceledContextRefusesResult(t *testing.T) {
	t.Parallel()

	sealed := make(chan struct{})
	gate := &fakeSDKTerminalGate{allow: false, sealed: sealed}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run := func() (*RunResult, error) {
		return &RunResult{SessionID: "s4"}, nil
	}

	res, err := RunAwaitingSeal(ctx, gate, "s4", run)

	require.Error(t, err)
	var gateErr *FinalizationGateError
	require.ErrorAs(t, err, &gateErr)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, res)
}

// Each call runs and delivers exactly once: repeated calls must not
// duplicate or lose results.
func TestRunAwaitingSealRepeatCallDoesNotDuplicate(t *testing.T) {
	t.Parallel()

	sealed := make(chan struct{})
	close(sealed)
	gate := &fakeSDKTerminalGate{allow: false, sealed: sealed}
	runs := 0
	run := func(id string) func() (*RunResult, error) {
		return func() (*RunResult, error) {
			runs++
			return &RunResult{SessionID: id}, nil
		}
	}

	res1, err1 := RunAwaitingSeal(t.Context(), gate, "s5", run("r1"))
	res2, err2 := RunAwaitingSeal(t.Context(), gate, "s5", run("r2"))

	require.NoError(t, err1)
	require.NoError(t, err2)
	require.NotNil(t, res1)
	require.NotNil(t, res2)
	require.Equal(t, "r1", res1.SessionID)
	require.Equal(t, "r2", res2.SessionID)
	require.Equal(t, 2, runs)
	require.Equal(t, int32(2), gate.awaitCalls.Load())
}

// An allowing gate must take the fast path: no AwaitSealed call, result
// delivered even though AwaitSealed would have failed.
func TestRunAwaitingSealFastPathSkipsAwait(t *testing.T) {
	t.Parallel()

	gate := &fakeSDKTerminalGate{
		allow:    true,
		awaitErr: errors.New("await must not run on the fast path"),
	}
	run := func() (*RunResult, error) {
		return &RunResult{SessionID: "s6"}, nil
	}

	res, err := RunAwaitingSeal(t.Context(), gate, "s6", run)

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "s6", res.SessionID)
	require.Equal(t, int32(0), gate.awaitCalls.Load())
}
