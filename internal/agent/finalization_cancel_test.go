package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// finalizationCancelTestExecution builds a lightweight execution with its own
// gate, without touching the database: the cancel path needs only the gate
// and the registry.
func finalizationCancelTestExecution(sessionID string, epoch uint64) *finalizationExecution {
	return &finalizationExecution{
		sessionID: sessionID,
		logicalID: "logical",
		gate:      newFinalizationGate(epoch),
		read:      quietFinalizationTestFacts,
	}
}

// TestFinalizationOnCancelHookRefusesSealBeforePublication proves the lane
// criterion: a cancel issued before the seal refuses every later gate
// publication in-process, leaves no candidate and no durable trace, and a
// repeated seal attempt stays impossible.
//
// Revert-check: a hook that resolves the session but skips gate.cancel, or a
// gate whose trySeal/beginCandidate ignores the cancelled flag, publishes
// here or takes a later stamp and fails the error, counter and record
// assertions.
func TestFinalizationOnCancelHookRefusesSealBeforePublication(t *testing.T) {
	env := newFinalizationDriverSeamEnv(t)
	registry := newFinalizationCancelRegistry()
	registry.register(env.execution)
	hook := NewFinalizationOnCancelHook(registry)
	stamp, err := env.execution.gate.beginCandidate(env.ctx, env.execution.read)
	require.NoError(t, err)
	hook(env.call.SessionID)
	var published int
	publish := func(context.Context, finalizationCandidate) error {
		published++
		return nil
	}
	candidate := finalizationCandidate{messageID: "m", hash: "h"}
	verdict, err := env.execution.gate.trySeal(env.ctx, stamp, candidate, env.execution.read, publish)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, finalizationWaitWork, verdict)
	require.Zero(t, published, "a cancelled owner must not deliver anything")
	_, err = env.execution.gate.beginCandidate(env.ctx, env.execution.read)
	require.ErrorIs(t, err, context.Canceled, "no later stamp can be taken, so no later seal is possible")
	verdict, err = env.execution.gate.trySeal(env.ctx, stamp, candidate, env.execution.read, publish)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, published)
	record, err := env.scope.store.Snapshot(env.ctx, env.scope.identity)
	require.NoError(t, err)
	require.Equal(t, "executing", record.Phase, "the in-process cancel alone must not rewrite the durable record")
	require.False(t, record.Cancelled)
	require.Empty(t, record.CandidateMessageID)
	// The durable fence stays independent: once the interrupt flow sets the
	// session flag, the record reports it and every seal refuses on it too.
	require.NoError(t, env.sessions.RequestCancel(env.ctx, "session"))
	record, err = env.scope.store.Snapshot(env.ctx, env.scope.identity)
	require.NoError(t, err)
	require.True(t, record.Cancelled)
}

// TestFinalizationOnCancelHookWakesArmedWaitThroughRegistry proves the hook
// wakes an owner parked in waitState: after the cancel the captured changed
// channel is closed, so the wait/compose loop returns instead of hanging.
//
// Revert-check: a registry that resolves the session but skips gate.cancel,
// or a snapshot that misses registered executions, leaves the channel open
// and the select falls into the fatal branch.
func TestFinalizationOnCancelHookWakesArmedWaitThroughRegistry(t *testing.T) {
	t.Parallel()
	execution := finalizationCancelTestExecution("session", 21)
	registry := newFinalizationCancelRegistry()
	registry.register(execution)
	hook := NewFinalizationOnCancelHook(registry)
	release, err := execution.gate.admitMutation(true)
	require.NoError(t, err)
	defer release()
	_, changed, err := execution.gate.waitState(t.Context(), execution.read)
	require.NoError(t, err)
	require.NotNil(t, changed)
	hook("session")
	select {
	case <-changed:
	default:
		t.Fatal("the cancel hook did not wake the owner waiting in waitState")
	}
}

// TestFinalizationOnCancelHookDoubleCancelIsIdempotent proves repeated cancel
// deliveries are harmless: the gate stays cancelled, nothing panics and the
// state does not flap.
//
// Revert-check: a hook or registry that treats the second cancel as a fresh
// lifecycle event (for example resurrecting or unregistering on cancel)
// breaks the flag assertions.
func TestFinalizationOnCancelHookDoubleCancelIsIdempotent(t *testing.T) {
	t.Parallel()
	execution := finalizationCancelTestExecution("session", 22)
	registry := newFinalizationCancelRegistry()
	registry.register(execution)
	hook := NewFinalizationOnCancelHook(registry)
	hook("session")
	require.True(t, execution.gate.cancelled)
	hook("session")
	hook("session")
	require.True(t, execution.gate.cancelled)
	require.False(t, execution.gate.sealed)
}

// TestFinalizationOnCancelHookUnknownSessionIsInert proves an unknown or
// empty session id changes nothing: registered work for other sessions stays
// untouched and still cancellable afterwards.
//
// Revert-check: a registry that cancels on a partial or empty id match
// cancels "session" here and fails.
func TestFinalizationOnCancelHookUnknownSessionIsInert(t *testing.T) {
	t.Parallel()
	execution := finalizationCancelTestExecution("session", 23)
	registry := newFinalizationCancelRegistry()
	registry.register(execution)
	hook := NewFinalizationOnCancelHook(registry)
	hook("other")
	hook("")
	require.False(t, execution.gate.cancelled)
	hook("session")
	require.True(t, execution.gate.cancelled)
}

// TestFinalizationOnCancelHookNilDependenciesDoNotPanic proves the hook and
// the registry stay safe with missing dependencies: a nil lookup, a nil
// registry receiver and malformed executions are all no-ops.
//
// Revert-check: removing any nil guard in the hook factory or the registry
// methods panics here instead of no-op'ing.
func TestFinalizationOnCancelHookNilDependenciesDoNotPanic(t *testing.T) {
	t.Parallel()
	hook := NewFinalizationOnCancelHook(nil)
	hook("session")
	hook("")
	var registry *finalizationCancelRegistry
	registry.register(nil)
	registry.register(finalizationCancelTestExecution("", 24))
	registry.register(&finalizationExecution{sessionID: "session"})
	registry.register(&finalizationExecution{sessionID: "session", logicalID: "logical"})
	registry.unregister(nil)
	registry.cancelSession("session")
	registry.cancelSession("")
}

// TestFinalizationConcurrentCancelAndSealAgreeOnExactlyOneOutcome proves the
// cancel-versus-seal race has no double outcome: either the seal publishes
// once and the cancel becomes a no-op, or the cancel wins and nothing is
// ever published.
//
// Revert-check: a gate that publishes outside the admission lock, or a
// cancel that unseals an accepted publication, breaks the exclusive-outcome
// assertions (double publish, or published and cancelled at once).
func TestFinalizationConcurrentCancelAndSealAgreeOnExactlyOneOutcome(t *testing.T) {
	t.Parallel()
	execution := finalizationCancelTestExecution("session", 25)
	registry := newFinalizationCancelRegistry()
	registry.register(execution)
	hook := NewFinalizationOnCancelHook(registry)
	stamp, err := execution.gate.beginCandidate(t.Context(), execution.read)
	require.NoError(t, err)
	var published atomic.Int32
	sealErr := make(chan error, 1)
	go func() {
		_, err := execution.gate.trySeal(t.Context(), stamp, finalizationCandidate{messageID: "m", hash: "h"}, execution.read, func(context.Context, finalizationCandidate) error {
			published.Add(1)
			return nil
		})
		sealErr <- err
	}()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hook("session")
		}()
	}
	wg.Wait()
	err = <-sealErr
	count := published.Load()
	if count == 0 {
		require.ErrorIs(t, err, context.Canceled)
		require.True(t, execution.gate.cancelled)
		require.False(t, execution.gate.sealed)
	} else {
		require.EqualValues(t, 1, count, "a second publication is always a defect")
		require.True(t, execution.gate.sealed)
		require.False(t, execution.gate.cancelled, "cancel must become a no-op once the seal committed")
	}
}

// TestFinalizationOnCancelHookSurvivesConcurrentRegistryTraffic drives the
// hook from many goroutines while another execution registers and
// unregisters around it; the race detector (orchestrator run) is the real
// oracle, the assertions pin the observable end state.
//
// Revert-check: unsynchronized map access in the registry fails the
// orchestrator's -race run; a lost cancel leaves the main gate uncancelled.
func TestFinalizationOnCancelHookSurvivesConcurrentRegistryTraffic(t *testing.T) {
	t.Parallel()
	execution := finalizationCancelTestExecution("session", 26)
	other := finalizationCancelTestExecution("session", 27)
	registry := newFinalizationCancelRegistry()
	registry.register(execution)
	registry.register(other)
	hook := NewFinalizationOnCancelHook(registry)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hook("session")
			hook("unknown")
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			registry.unregister(other)
			registry.register(other)
			registry.cancelSession("session")
		}()
	}
	wg.Wait()
	require.True(t, execution.gate.cancelled)
	registry.unregister(execution)
	registry.unregister(other)
	registry.mu.Lock()
	remaining := len(registry.byKey)
	registry.mu.Unlock()
	require.Zero(t, remaining, "every unregister must leave the registry empty")
}

// TestFinalizationCancelRegistryLifecycleDoesNotLeak pins register,
// unregister and re-register semantics: set semantics per bucket,
// pointer-identity removal, session isolation, re-registration as one fresh
// entry and no growth from malformed executions.
//
// Revert-check: an unregister that drops the whole session bucket (instead
// of just the execution) breaks the other-session assertions; a register
// that leaks duplicate or empty-session entries breaks the length asserts.
func TestFinalizationCancelRegistryLifecycleDoesNotLeak(t *testing.T) {
	t.Parallel()
	first := finalizationCancelTestExecution("first", 28)
	second := finalizationCancelTestExecution("second", 29)
	registry := newFinalizationCancelRegistry()
	registry.register(first)
	registry.register(first)
	registry.mu.Lock()
	buckets, firstBucket := len(registry.byKey), len(registry.byKey["first"])
	registry.mu.Unlock()
	require.Equal(t, 1, buckets)
	require.Equal(t, 1, firstBucket, "set semantics keep one entry per execution")
	registry.register(second)
	registry.cancelSession("first")
	require.True(t, first.gate.cancelled)
	require.False(t, second.gate.cancelled, "sessions are isolated")
	registry.unregister(first)
	registry.cancelSession("first")
	registry.mu.Lock()
	buckets = len(registry.byKey)
	registry.mu.Unlock()
	require.Equal(t, 1, buckets, "only the second session remains")
	registry.register(second)
	registry.mu.Lock()
	secondBucket := len(registry.byKey["second"])
	registry.mu.Unlock()
	require.Equal(t, 1, secondBucket, "re-registration is one fresh entry, not a duplicate")
	registry.register(finalizationCancelTestExecution("", 30))
	registry.mu.Lock()
	buckets = len(registry.byKey)
	registry.mu.Unlock()
	require.Equal(t, 1, buckets, "an execution without a session is never indexed")
	registry.unregister(second)
	registry.unregister(second)
	registry.mu.Lock()
	buckets = len(registry.byKey)
	registry.mu.Unlock()
	require.Zero(t, buckets, "draining the last bucket removes the session")
}
