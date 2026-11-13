package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// finalizationNotifyGateStub is a deterministic in-memory terminal gate: no
// goroutines, no tickers, no polling.
type finalizationNotifyGateStub struct {
	mu        sync.Mutex
	active    map[string]bool
	seals     map[string]chan struct{} // Created lazily, buffered cap 1.
	awaitErr  error                    // Deterministic AwaitSealed failure.
	failAllow bool                     // Deterministic AllowTerminal=false.
}

func newFinalizationNotifyGateStub() *finalizationNotifyGateStub {
	return &finalizationNotifyGateStub{
		active: map[string]bool{},
		seals:  map[string]chan struct{}{},
	}
}

func (s *finalizationNotifyGateStub) sealChanLocked(owner string) chan struct{} {
	ch, ok := s.seals[owner]
	if !ok {
		ch = make(chan struct{}, 1)
		s.seals[owner] = ch
	}
	return ch
}

// activate marks the owner's finalization record active.
func (s *finalizationNotifyGateStub) activate(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[owner] = true
}

// seal releases the owner's hold: the record leaves the active phase and the
// AwaitSealed watcher is woken by a buffered signal.
func (s *finalizationNotifyGateStub) seal(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[owner] = false
	select {
	case s.sealChanLocked(owner) <- struct{}{}:
	default:
	}
}

func (s *finalizationNotifyGateStub) AllowTerminal(_ context.Context, owner string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAllow {
		return false
	}
	return !s.active[owner]
}

func (s *finalizationNotifyGateStub) AwaitSealed(ctx context.Context, owner string) error {
	for {
		s.mu.Lock()
		if s.awaitErr != nil {
			err := s.awaitErr
			s.mu.Unlock()
			return err
		}
		active := s.active[owner]
		seal := s.sealChanLocked(owner)
		s.mu.Unlock()
		if !active {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-seal:
		}
	}
}

// finalizationMessageEvent builds a message broker event for the given role
// and parts.
func finalizationMessageEvent(
	id, sessionID string, role message.MessageRole, parts ...message.ContentPart,
) pubsub.Event[message.Message] {
	return pubsub.Event[message.Message]{
		Type:    pubsub.UpdatedEvent,
		Payload: message.Message{ID: id, Role: role, SessionID: sessionID, Parts: parts},
	}
}

func finalizationTerminalEvent(id, sessionID string) pubsub.Event[message.Message] {
	return finalizationMessageEvent(id, sessionID, message.Assistant,
		message.Finish{Reason: message.FinishReasonEndTurn})
}

func finalizationNonTerminalEvent(id, sessionID string) pubsub.Event[message.Message] {
	return finalizationMessageEvent(id, sessionID, message.Assistant,
		message.TextContent{Text: "partial answer"})
}

func finalizationPublishAll[T any](t *testing.T, in chan T, events []T) {
	t.Helper()
	for _, ev := range events {
		in <- ev
	}
	close(in)
}

func finalizationCollect[T any](out <-chan T) []T {
	var collected []T
	for ev := range out {
		collected = append(collected, ev)
	}
	return collected
}

func TestFinalizationGatedMessageStreamNilGateReturnsInputUnchanged(t *testing.T) {
	t.Parallel()
	in := make(chan pubsub.Event[message.Message], 1)
	out := FinalizationGatedMessageStream(t.Context(), nil, in)
	event := finalizationNonTerminalEvent("m1", "s")
	in <- event
	close(in)
	require.Equal(t, []pubsub.Event[message.Message]{event}, finalizationCollect(out))
}

func TestFinalizationGatedMessageStreamInertDefaultGateForwardsInOrder(t *testing.T) {
	t.Parallel()
	in := make(chan pubsub.Event[message.Message], 3)
	first := finalizationNonTerminalEvent("a", "s")
	second := finalizationTerminalEvent("b", "s")
	third := finalizationNonTerminalEvent("c", "s")
	out := FinalizationGatedMessageStream(
		t.Context(), session.DefaultFinalizationTerminalGate(), in)
	finalizationPublishAll(t, in, []pubsub.Event[message.Message]{first, second, third})
	require.Equal(t, []pubsub.Event[message.Message]{first, second, third},
		finalizationCollect(out))
}

func TestFinalizationGatedMessageStreamHoldsTerminalWhileRecordActive(t *testing.T) {
	t.Parallel()
	gate := newFinalizationNotifyGateStub()
	gate.activate("owner")
	in := make(chan pubsub.Event[message.Message], 2)
	out := FinalizationGatedMessageStream(t.Context(), gate, in)
	held := finalizationTerminalEvent("t", "owner")
	passes := finalizationNonTerminalEvent("n", "owner")
	in <- held
	in <- passes
	// The first delivery must be the non-terminal event: the hold must not
	// block the subscriber (the single pump goroutine decides the order).
	require.Equal(t, passes, <-out)
	gate.seal("owner")
	require.Equal(t, held, <-out)
	close(in)
	require.Empty(t, finalizationCollect(out), "each event is delivered exactly once")
}

func TestFinalizationGatedMessageStreamDeliversHeldEventsInArrivalOrder(t *testing.T) {
	t.Parallel()
	gate := newFinalizationNotifyGateStub()
	gate.activate("owner")
	in := make(chan pubsub.Event[message.Message], 2)
	out := FinalizationGatedMessageStream(t.Context(), gate, in)
	first := finalizationTerminalEvent("t1", "owner")
	second := finalizationTerminalEvent("t2", "owner")
	finalizationPublishAll(t, in, []pubsub.Event[message.Message]{first, second})
	gate.seal("owner")
	require.Equal(t, []pubsub.Event[message.Message]{first, second},
		finalizationCollect(out))
}

func TestFinalizationGatedMessageStreamFailClosedOnAwaitSealedError(t *testing.T) {
	t.Parallel()
	gate := newFinalizationNotifyGateStub()
	gate.failAllow = true
	gate.awaitErr = errors.New("stub store failure")
	in := make(chan pubsub.Event[message.Message], 1)
	out := FinalizationGatedMessageStream(t.Context(), gate, in)
	held := finalizationTerminalEvent("t", "owner")
	finalizationPublishAll(t, in, []pubsub.Event[message.Message]{held})
	// The held event is dropped, never delivered as a success, and out
	// closes cleanly once the releaser has reported.
	require.Empty(t, finalizationCollect(out))
}

func TestFinalizationGatedMessageStreamCancelReleasesHoldWithoutDelivery(t *testing.T) {
	t.Parallel()
	gate := newFinalizationNotifyGateStub()
	gate.activate("owner")
	ctx, cancel := context.WithCancel(t.Context())
	in := make(chan pubsub.Event[message.Message], 1)
	out := FinalizationGatedMessageStream(ctx, gate, in)
	held := finalizationTerminalEvent("t", "owner")
	in <- held
	cancel()
	close(in)
	// The drain terminates only after the pump's wg.Wait, i.e. after the
	// releaser goroutine exited; no held event may arrive.
	require.Empty(t, finalizationCollect(out))
}

func TestFinalizationGatedMessageStreamOwnersAreIndependent(t *testing.T) {
	t.Parallel()
	gate := newFinalizationNotifyGateStub()
	gate.activate("a")
	in := make(chan pubsub.Event[message.Message], 3)
	out := FinalizationGatedMessageStream(t.Context(), gate, in)
	heldA := finalizationTerminalEvent("ta", "a")
	passB := finalizationTerminalEvent("tb", "b")
	passN := finalizationNonTerminalEvent("n", "a")
	in <- heldA
	in <- passB
	in <- passN
	// Owner b is unaffected by a's hold, and a's own non-terminal event
	// passes while the terminal one is held.
	require.Equal(t, passB, <-out)
	require.Equal(t, passN, <-out)
	gate.seal("a")
	require.Equal(t, heldA, <-out)
	close(in)
	require.Empty(t, finalizationCollect(out), "heldA is delivered exactly once")
}

func TestFinalizationGatedSessionStreamHoldsOnlyEndedUpdatedEvent(t *testing.T) {
	t.Parallel()
	gate := newFinalizationNotifyGateStub()
	gate.activate("s")
	in := make(chan pubsub.Event[session.Session], 4)
	out := FinalizationGatedSessionStream(t.Context(), gate, in)
	held := pubsub.Event[session.Session]{
		Type:    pubsub.UpdatedEvent,
		Payload: session.Session{ID: "s", EndedReason: "end_turn"},
	}
	update := pubsub.Event[session.Session]{
		Type:    pubsub.UpdatedEvent,
		Payload: session.Session{ID: "s"},
	}
	created := pubsub.Event[session.Session]{
		Type:    pubsub.CreatedEvent,
		Payload: session.Session{ID: "s", EndedReason: "end_turn"},
	}
	deleted := pubsub.Event[session.Session]{
		Type:    pubsub.DeletedEvent,
		Payload: session.Session{ID: "s"},
	}
	finalizationPublishAll(t, in, []pubsub.Event[session.Session]{
		held, update, created, deleted,
	})
	got := []pubsub.Event[session.Session]{<-out, <-out, <-out}
	require.Equal(t, []pubsub.Event[session.Session]{update, created, deleted}, got)
	gate.seal("s")
	require.Equal(t, []pubsub.Event[session.Session]{held},
		finalizationCollect(out))
}

func TestFinalizationAPIEventTerminalClassifiesOnlyTerminalRunStates(t *testing.T) {
	t.Parallel()
	states := map[string]bool{
		"succeeded": true, "failed": true, "cancelled": true,
		"queued": false, "running": false, "awaiting_answer": false,
	}
	for state, want := range states {
		event := APIEvent{
			Type: "run",
			Data: json.RawMessage(`{"state":"` + state + `"}`),
		}
		require.Equal(t, want, finalizationAPIEventTerminal(event), "state %q", state)
	}
	require.False(t, finalizationAPIEventTerminal(APIEvent{
		Type: "run",
		Data: json.RawMessage(`{not json`),
	}))
	require.False(t, finalizationAPIEventTerminal(APIEvent{
		Type: "message",
		Data: json.RawMessage(`{"state":"succeeded"}`),
	}))
	require.False(t, finalizationAPIEventTerminal(APIEvent{
		Type: "session",
		Data: json.RawMessage(`{"state":"succeeded"}`),
	}))
}

func TestFinalizationGatedAPIEventStreamHoldsTerminalRunUntilSealed(t *testing.T) {
	t.Parallel()
	gate := newFinalizationNotifyGateStub()
	gate.activate("s")
	ctx := t.Context()
	in := make(chan APIEvent, 4)
	out := FinalizationGatedAPIEventStream(ctx, gate, "s", in)
	held := APIEvent{Type: "run", Data: json.RawMessage(`{"state":"succeeded"}`)}
	passing := APIEvent{Type: "message", Data: json.RawMessage(`{"id":"m1"}`)}
	in <- held
	in <- passing
	// A second stream for a different owner is unaffected by s's hold.
	in2 := make(chan APIEvent, 1)
	out2 := FinalizationGatedAPIEventStream(ctx, gate, "other", in2)
	other := APIEvent{Type: "run", Data: json.RawMessage(`{"state":"failed"}`)}
	finalizationPublishAll(t, in2, []APIEvent{other})
	require.Equal(t, other, <-out2)
	// The non-terminal message entry passes while the run result is held.
	require.Equal(t, passing, <-out)
	gate.seal("s")
	// The pump closes out only after its input closes and the hold releases.
	close(in)
	require.Equal(t, []APIEvent{held}, finalizationCollect(out))
	require.Empty(t, finalizationCollect(out2))
}

func TestFinalizationGatedAPIEventStreamEmptyOwnerReturnsInputUnchanged(t *testing.T) {
	t.Parallel()
	in := make(chan APIEvent, 1)
	out := FinalizationGatedAPIEventStream(
		t.Context(), newFinalizationNotifyGateStub(), "", in)
	event := APIEvent{Type: "run", Data: json.RawMessage(`{"state":"succeeded"}`)}
	in <- event
	close(in)
	require.Equal(t, []APIEvent{event}, finalizationCollect(out))
}
