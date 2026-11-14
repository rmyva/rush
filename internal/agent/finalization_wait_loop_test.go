package agent

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// finalizationWaitFactsStub is a mutex-guarded, mutable replacement for the
// execution's read closure, so tests can move the facts while the loop runs.
type finalizationWaitFactsStub struct {
	mu    sync.Mutex
	facts finalizationFacts
}

func newFinalizationWaitFactsStub() *finalizationWaitFactsStub {
	return &finalizationWaitFactsStub{facts: finalizationFacts{complete: true}}
}

func (s *finalizationWaitFactsStub) read(context.Context) (finalizationFacts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.facts, nil
}

func (s *finalizationWaitFactsStub) set(fn func(*finalizationFacts)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.facts)
}

// finalizationSettlerStub records every SettleFinalizationDebts call.
type finalizationSettlerStub struct {
	mu     sync.Mutex
	calls  int
	owners []string
	err    error
	onCall func()
}

func (s *finalizationSettlerStub) SettleFinalizationDebts(_ context.Context, ownerSessionID string) (int, error) {
	s.mu.Lock()
	s.calls++
	s.owners = append(s.owners, ownerSessionID)
	err, onCall := s.err, s.onCall
	s.mu.Unlock()
	if onCall != nil {
		onCall()
	}
	return 0, err
}

func (s *finalizationSettlerStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// finalizationWaitFixture wires a seam env with a mutable facts stub, a real
// provisional candidate recorded as if a provider step had just committed it.
type finalizationWaitFixture struct {
	env      finalizationDriverSeamEnv
	facts    *finalizationWaitFactsStub
	ts       *turnStream
	mu       sync.Mutex
	composes int
}

func newFinalizationWaitFixture(t *testing.T) *finalizationWaitFixture {
	t.Helper()
	env := newFinalizationDriverSeamEnv(t)
	facts := newFinalizationWaitFactsStub()
	env.execution.read = facts.read
	created, err := env.messages.Create(t.Context(), "session", message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "provisional"}},
	})
	require.NoError(t, err)
	ts := &turnStream{
		a:                &sessionAgent{sessions: env.sessions, messages: env.messages},
		ctx:              context.Background(),
		genCtx:           context.Background(),
		call:             env.call,
		finalization:     env.execution,
		currentAssistant: &created,
	}
	require.NoError(t, ts.beginFinalizationStep(t.Context()))
	stamp := ts.finalizationStamp
	identity := message.FinalizationIdentity{
		Key: env.execution.logicalID, Generation: stamp.epoch,
		Revision: stamp.durableRevision, Serial: stamp.serial,
	}
	require.NoError(t, created.MarkFinalizationCandidate(identity))
	created.CheckpointGeneration = 1
	persisted, err := persistFinalizationCandidate(t.Context(), env.sessions, env.messages, env.scope, identity, created)
	require.NoError(t, err)
	ts.finalizationCandidate = persisted
	return &finalizationWaitFixture{env: env, facts: facts, ts: ts}
}

// composeStub counts recompositions and records a real persisted candidate
// under the fresh stamp, exactly as the default compose would.
func (f *finalizationWaitFixture) composeStub(t *testing.T) FinalizationComposeFunc {
	return func(ctx context.Context, in FinalizationComposeInput) (persistedFinalizationCandidate, error) {
		f.mu.Lock()
		f.composes++
		f.mu.Unlock()
		identity := message.FinalizationIdentity{
			Key: in.Execution.logicalID, Generation: in.Stamp.epoch,
			Revision: in.Stamp.durableRevision, Serial: in.Stamp.serial,
		}
		created, err := f.env.messages.Create(ctx, "session", message.CreateMessageParams{
			Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "recomposed"}},
		})
		require.NoError(t, err)
		require.NoError(t, created.MarkFinalizationCandidate(identity))
		created.CheckpointGeneration = 1
		return persistFinalizationCandidate(ctx, f.env.sessions, f.env.messages, in.Execution.scope, identity, created)
	}
}

func (f *finalizationWaitFixture) composeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.composes
}

// bumpRevision moves the gate revision without an open mutation, so the next
// trySeal sees the current stamp as stale.
func bumpGateRevision(t *testing.T, gate *finalizationGate) {
	t.Helper()
	release, err := gate.admitMutation(false)
	require.NoError(t, err)
	release()
}

// recordAccepted asserts the durable and message receipts of a sealed owner.
func recordAccepted(t *testing.T, f *finalizationWaitFixture) {
	t.Helper()
	record, err := f.env.scope.store.Snapshot(t.Context(), f.env.scope.identity)
	require.NoError(t, err)
	require.Equal(t, "accepted", record.Phase)
}

// hook builds a FinalizeTurn hook with the given overrides over quiet facts.
func (f *finalizationWaitFixture) hook(override func(*FinalizationWaitDeps)) func(context.Context, *turnStream) error {
	// Default settler so wait entries do not fail closed unless a test
	// explicitly overrides Settler back to nil.
	deps := FinalizationWaitDeps{PollInterval: 2 * time.Millisecond, MaxRecompositions: -1, Settler: &finalizationSettlerStub{}}
	if override != nil {
		override(&deps)
	}
	return NewFinalizationFinalizeTurnHook(deps)
}

func TestFinalizationWaitLoopQuietFactsSealOnce(t *testing.T) {
	f := newFinalizationWaitFixture(t)
	hook := f.hook(func(d *FinalizationWaitDeps) {
		d.Compose = func(context.Context, FinalizationComposeInput) (persistedFinalizationCandidate, error) {
			return persistedFinalizationCandidate{}, errors.New("compose must not run on quiet facts")
		}
	})
	require.NoError(t, hook(t.Context(), f.ts))
	recordAccepted(t, f)
	require.True(t, f.ts.currentAssistant.IsFinished(), "publication replaces the provisional answer")
	require.Equal(t, 0, f.composeCount())
}

func TestFinalizationWaitLoopChildActiveWakesOnChangeAndSeals(t *testing.T) {
	f := newFinalizationWaitFixture(t)
	f.facts.set(func(x *finalizationFacts) { x.descendants = 1 })
	var entered sync.Once
	enteredWait := make(chan struct{})
	hook := f.hook(func(d *FinalizationWaitDeps) {
		d.Compose = f.composeStub(t)
		d.OnState = func(st FinalizationWaitStatus) {
			if st.Phase == FinalizationPhaseWaiting {
				entered.Do(func() { close(enteredWait) })
			}
		}
	})
	go func() {
		<-enteredWait
		// Bump the gate revision BEFORE the facts go quiet. The reverse order
		// is racy: the 2ms poll can sample the quiet facts, recompose, and
		// only then see the bump land between the new stamp and its seal,
		// which stales that stamp and recomposes a second time. With the bump
		// first, the facts are quiet only after the revision reached its
		// final value, so exactly one recomposition remains.
		release, err := f.env.execution.gate.admitMutation(false)
		if err == nil {
			release()
		}
		f.facts.set(func(x *finalizationFacts) { x.descendants = 0 })
	}()
	require.NoError(t, hook(t.Context(), f.ts))
	recordAccepted(t, f)
	require.Equal(t, 1, f.composeCount())
}

func TestFinalizationWaitLoopPollSeesExternalFactChange(t *testing.T) {
	f := newFinalizationWaitFixture(t)
	f.facts.set(func(x *finalizationFacts) { x.descendants = 1 })
	var entered sync.Once
	enteredWait := make(chan struct{})
	hook := f.hook(func(d *FinalizationWaitDeps) {
		d.Compose = f.composeStub(t)
		d.PollInterval = time.Millisecond
		d.OnState = func(st FinalizationWaitStatus) {
			if st.Phase == FinalizationPhaseWaiting {
				entered.Do(func() { close(enteredWait) })
			}
		}
	})
	// The fact move happens without any gate signal, so only the poll timer
	// can wake the loop.
	go func() {
		<-enteredWait
		f.facts.set(func(x *finalizationFacts) { x.descendants = 0 })
	}()
	require.NoError(t, hook(t.Context(), f.ts))
	recordAccepted(t, f)
	require.Equal(t, 1, f.composeCount())
}

func TestFinalizationWaitLoopHeldQuestionPausesWithoutPublishing(t *testing.T) {
	f := newFinalizationWaitFixture(t)
	f.facts.set(func(x *finalizationFacts) { x.heldQuestion = true })
	hook := f.hook(nil)
	err := hook(t.Context(), f.ts)
	var awaited *AwaitingAnswerError
	require.ErrorAs(t, err, &awaited)
	require.Equal(t, "session", awaited.SessionID)
	require.ErrorIs(t, err, errAwaitingAnswer)
	record, err := f.env.scope.store.Snapshot(t.Context(), f.env.scope.identity)
	require.NoError(t, err)
	require.Equal(t, "executing", record.Phase, "a pause must not publish")
	require.False(t, f.ts.currentAssistant.IsFinished())
}

func TestFinalizationWaitLoopDebtSettledOnceThenSeals(t *testing.T) {
	f := newFinalizationWaitFixture(t)
	f.facts.set(func(x *finalizationFacts) { x.debt = 1 })
	settler := &finalizationSettlerStub{onCall: func() {
		f.facts.set(func(x *finalizationFacts) { x.debt = 0 })
	}}
	hook := f.hook(func(d *FinalizationWaitDeps) {
		d.Compose = f.composeStub(t)
		d.Settler = settler
	})
	require.NoError(t, hook(t.Context(), f.ts))
	recordAccepted(t, f)
	require.Equal(t, 1, settler.count(), "settle runs exactly once per wait entry")
}

func TestFinalizationWaitLoopCancelDuringWaitReturnsContextError(t *testing.T) {
	f := newFinalizationWaitFixture(t)
	f.facts.set(func(x *finalizationFacts) { x.descendants = 1 })
	ctx, cancel := context.WithCancel(t.Context())
	var entered sync.Once
	enteredWait := make(chan struct{})
	hook := f.hook(func(d *FinalizationWaitDeps) {
		d.Compose = f.composeStub(t)
		d.OnState = func(st FinalizationWaitStatus) {
			if st.Phase == FinalizationPhaseWaiting {
				entered.Do(func() {
					close(enteredWait)
					cancel()
				})
			}
		}
	})
	err := hook(ctx, f.ts)
	require.ErrorIs(t, err, context.Canceled)
	record, err := f.env.scope.store.Snapshot(t.Context(), f.env.scope.identity)
	require.NoError(t, err)
	require.Equal(t, "executing", record.Phase, "cancellation must not publish")
	require.Equal(t, 0, f.composeCount())
}

func TestFinalizationWaitLoopRecomposeLimitIsHonest(t *testing.T) {
	f := newFinalizationWaitFixture(t)
	// A stale entry stamp forces the first recomposition, so the budget is
	// actually exercised.
	f.ts.finalizationStamp.revision++
	hook := f.hook(func(d *FinalizationWaitDeps) {
		d.MaxRecompositions = 2
		d.Compose = func(ctx context.Context, in FinalizationComposeInput) (persistedFinalizationCandidate, error) {
			persisted, err := f.composeStub(t)(ctx, in)
			if err != nil {
				return persisted, err
			}
			// Every recomposition is immediately invalidated again, so the
			// loop would keep composing forever without a budget.
			bumpGateRevision(t, f.env.execution.gate)
			return persisted, nil
		}
	})
	err := hook(t.Context(), f.ts)
	require.ErrorIs(t, err, ErrFinalizationRecomposeLimit)
	require.Equal(t, 2, f.composeCount())
}

func TestFinalizationWaitLoopStaleStampComposesExactlyOnce(t *testing.T) {
	f := newFinalizationWaitFixture(t)
	f.ts.finalizationStamp.revision++
	hook := f.hook(func(d *FinalizationWaitDeps) { d.Compose = f.composeStub(t) })
	require.NoError(t, hook(t.Context(), f.ts))
	recordAccepted(t, f)
	require.Equal(t, 1, f.composeCount())
	require.Greater(t, f.ts.finalizationStamp.serial, uint64(1), "recomposition takes a fresh stamp")
}

func TestFinalizationWaitLoopSealAfterSealIsIdempotent(t *testing.T) {
	f := newFinalizationWaitFixture(t)
	hook := f.hook(func(d *FinalizationWaitDeps) {
		d.Compose = func(context.Context, FinalizationComposeInput) (persistedFinalizationCandidate, error) {
			return persistedFinalizationCandidate{}, errors.New("compose must not run on an accepted gate")
		}
	})
	require.NoError(t, hook(t.Context(), f.ts))
	before, err := f.env.scope.store.Snapshot(t.Context(), f.env.scope.identity)
	require.NoError(t, err)
	require.NoError(t, hook(t.Context(), f.ts))
	after, err := f.env.scope.store.Snapshot(t.Context(), f.env.scope.identity)
	require.NoError(t, err)
	require.Equal(t, before, after, "an already sealed gate must not publish again")
	require.Equal(t, 0, f.composeCount())
}

func TestFinalizationWaitLoopZeroBudgetRefusesWithoutComposing(t *testing.T) {
	f := newFinalizationWaitFixture(t)
	f.ts.finalizationStamp.revision++
	hook := f.hook(func(d *FinalizationWaitDeps) {
		d.MaxRecompositions = 0
		d.Compose = f.composeStub(t)
	})
	err := hook(t.Context(), f.ts)
	require.ErrorIs(t, err, ErrFinalizationRecomposeLimit)
	require.Equal(t, 0, f.composeCount())
}

// finalizationScopeChangedOnceProbe makes the first candidate read fail with
// the message-level "changed" error, which the publisher maps to a durable
// ErrFinalizationScopeChanged refusal. The test pins that a durable not-yet
// round is settle + poll-wait + exactly one recomposition (the gate stales the
// stamp on refusal), not a hot loop burning recomposition budget.
type finalizationScopeChangedOnceProbe struct {
	message.Service
	message.FinalizationReader
	attempts atomic.Int32
}

func (r *finalizationScopeChangedOnceProbe) GetFinalizationCandidateTx(ctx context.Context, tx *sql.Tx, id string) (message.Message, error) {
	if r.attempts.Add(1) == 1 {
		return message.Message{}, message.ErrFinalizationCandidateChanged
	}
	return r.FinalizationReader.GetFinalizationCandidateTx(ctx, tx, id)
}

func TestFinalizationWaitLoopDurableNotYetWaitsInsteadOfRecomposing(t *testing.T) {
	f := newFinalizationWaitFixture(t)
	probe := &finalizationScopeChangedOnceProbe{Service: f.env.messages}
	probe.FinalizationReader = f.env.messages.(message.FinalizationReader)
	f.ts.a.messages = probe
	settler := &finalizationSettlerStub{}
	seenWaiting := false
	hook := f.hook(func(d *FinalizationWaitDeps) {
		d.Compose = f.composeStub(t)
		d.PollInterval = time.Millisecond
		d.Settler = settler
		d.OnState = func(st FinalizationWaitStatus) {
			if st.Phase == FinalizationPhaseWaiting {
				seenWaiting = true
			}
		}
	})
	require.NoError(t, hook(t.Context(), f.ts))
	recordAccepted(t, f)
	require.Equal(t, 1, f.composeCount(), "one recomposition per durable not-yet round: the gate's scope-changed branch stales the stamp, so the next attempt must recompose once")
	require.Equal(t, 1, settler.count(), "the not-yet round settles debts once before waiting")
	require.True(t, seenWaiting, "the not-yet round waits on the poll timer instead of hot-looping a recompose")
	require.GreaterOrEqual(t, probe.attempts.Load(), int32(2), "publication is retried after the wait")
}

func TestFinalizationWaitLoopWithoutExecutionFailsClosed(t *testing.T) {
	env := testEnv(t)
	ts := &turnStream{a: &sessionAgent{sessions: env.sessions, messages: env.messages}}
	hook := NewFinalizationFinalizeTurnHook(FinalizationWaitDeps{})
	err := hook(t.Context(), ts)
	require.ErrorIs(t, err, ErrFinalizationFactsUnavailable)
}
