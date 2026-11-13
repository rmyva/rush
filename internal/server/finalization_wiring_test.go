package server

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// finalizationWiringGateStub wraps the package's signal-based gate stub with
// an AwaitSealed entry barrier and owner recording, so tests can prove a
// hold deterministically.
type finalizationWiringGateStub struct {
	*finalizationNotifyGateStub
	entered   chan struct{}
	enterOnce sync.Once

	mu     sync.Mutex
	owners []string
}

func newFinalizationWiringGateStub() *finalizationWiringGateStub {
	return &finalizationWiringGateStub{
		finalizationNotifyGateStub: newFinalizationNotifyGateStub(),
		entered:                    make(chan struct{}),
	}
}

func (s *finalizationWiringGateStub) awaitEntered() <-chan struct{} { return s.entered }

func (s *finalizationWiringGateStub) awaitedOwners() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.owners...)
}

// AwaitSealed signals entry and records the owner, then delegates to the
// signal-based stub.
func (s *finalizationWiringGateStub) AwaitSealed(ctx context.Context, owner string) error {
	s.enterOnce.Do(func() { close(s.entered) })
	s.mu.Lock()
	s.owners = append(s.owners, owner)
	s.mu.Unlock()
	return s.finalizationNotifyGateStub.AwaitSealed(ctx, owner)
}

// finalizationWaitForSubscriberCount waits until count() >= want, using the
// package's bounded-deadline idiom (the forwarders subscribe inside their
// goroutines, so registration is inherently asynchronous).
func finalizationWaitForSubscriberCount(t *testing.T, what string, count func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for count() < want {
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached %d subscribers (got %d)", what, want, count())
		}
		time.Sleep(time.Millisecond)
	}
}

// finalizationWaitForEvent waits (bounded) for a predicate over WS frames
// read from a hub client's send channel; returns the matching frames seen.
func finalizationWaitForEvent(t *testing.T, send <-chan []byte, match func(WSMessage) bool, what string) WSMessage {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case raw := <-send:
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env), "hub frame %q", raw)
			if match(env) {
				return env
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// finalizationLogHasMessageEvent reports whether the session's log contains
// a message event whose data mentions id.
func finalizationLogHasMessageEvent(log *APIEventLog, sessionID, id string) bool {
	events, _, _ := log.Replay(sessionID, 0)
	for _, event := range events {
		if event.Type == "message" && strings.Contains(string(event.Data), `"`+id+`"`) {
			return true
		}
	}
	return false
}

// finalizationCountMessageEvents counts the session's log message events
// whose data mentions id.
func finalizationCountMessageEvents(log *APIEventLog, sessionID, id string) int {
	events, _, _ := log.Replay(sessionID, 0)
	count := 0
	for _, event := range events {
		if event.Type == "message" && strings.Contains(string(event.Data), `"`+id+`"`) {
			count++
		}
	}
	return count
}

// finalizationWaitForMessageEventCount waits (bounded) until the count of
// the session's log message events mentioning id reaches want.
func finalizationWaitForMessageEventCount(t *testing.T, log *APIEventLog, sessionID, id string, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for finalizationCountMessageEvents(log, sessionID, id) < want {
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached %d log entries (got %d)", what, want,
				finalizationCountMessageEvents(log, sessionID, id))
		}
		time.Sleep(time.Millisecond)
	}
}

// finalizationWaitForEventLogEntry waits (bounded) until the session's log
// contains a message event carrying id.
func finalizationWaitForEventLogEntry(t *testing.T, log *APIEventLog, sessionID, id, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !finalizationLogHasMessageEvent(log, sessionID, id) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached the event log", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// Flag-off regression: with the fixture app (finalization disabled) the gate
// lookup is nil and the feeder publishes a terminal assistant message into
// the API event log immediately, exactly as before the wiring existed.
func TestAPIEventsFeederNilGateFeedsTerminalMessageImmediately(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	require.Nil(t, finalizationTerminalGateFor(a),
		"the flag-off fixture app must resolve to the inert nil gate")
	log := NewAPIEventLog(nil)
	feeder := startAPIEventFeeder(a, log, nil)
	t.Cleanup(func() { _ = feeder.Stop(context.Background()) })

	messages := a.Messages.(apiEventsSubscriberCounter)
	finalizationWaitForSubscriberCount(t, "message feed", messages.GetSubscriberCount, messages.GetSubscriberCount()+1)

	ctx := t.Context()
	sess, err := a.Sessions.Create(ctx, "feeder inert")
	require.NoError(t, err)
	reply, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "final"}},
	})
	require.NoError(t, err)
	reply.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, reply))

	finalizationWaitForEventLogEntry(t, log, sess.ID, reply.ID, "terminal message with the flag off")
}

// Flag-on (stub gate): the feeder's message feed holds the terminal
// assistant message while the owner's record is active — the log must not
// contain it — while non-terminal messages still flow; after the seal the
// held message is published exactly once.
func TestAPIEventsFeederGatesTerminalMessageUntilSeal(t *testing.T) {
	stub := newFinalizationWiringGateStub()
	orig := finalizationTerminalGateFor
	finalizationTerminalGateFor = func(*appPkg.App) session.FinalizationTerminalGate { return stub }
	t.Cleanup(func() { finalizationTerminalGateFor = orig })

	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	log := NewAPIEventLog(nil)
	feeder := startAPIEventFeeder(a, log, nil)
	t.Cleanup(func() { _ = feeder.Stop(context.Background()) })

	messages := a.Messages.(apiEventsSubscriberCounter)
	finalizationWaitForSubscriberCount(t, "gated message feed", messages.GetSubscriberCount, messages.GetSubscriberCount()+1)

	ctx := t.Context()
	sess, err := a.Sessions.Create(ctx, "feeder gated")
	require.NoError(t, err)
	stub.activate(sess.ID)
	// Non-terminal assistant message flows while the record is active.
	partial, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "partial"}},
	})
	require.NoError(t, err)
	finalizationWaitForEventLogEntry(t, log, sess.ID, partial.ID, "non-terminal message")

	// Terminal message: held.
	final, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "final"}},
	})
	require.NoError(t, err)
	final.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, final))
	<-stub.awaitEntered()
	require.Equal(t, []string{sess.ID}, stub.awaitedOwners())

	// The created event of the terminal message passed the gate (it is
	// non-terminal at creation); the finishing update is held, so exactly
	// one entry mentioning the message exists while the record is active.
	finalizationWaitForEventLogEntry(t, log, sess.ID, final.ID, "created entry of the terminal message")
	require.Equal(t, 1, finalizationCountMessageEvents(log, sess.ID, final.ID),
		"the held terminal message update must not be published while the record is active")

	stub.seal(sess.ID)
	finalizationWaitForMessageEventCount(t, log, sess.ID, final.ID, 2,
		"held terminal message after seal")
}

// Flag-on (stub gate): subscribeAndBroadcast holds the terminal assistant
// message's WS broadcast until the seal; a session rename (non-terminal)
// still broadcasts while the hold is active, so the hub is not blocked.
func TestSubscribeAndBroadcastGatesTerminalMessageUntilSeal(t *testing.T) {
	stub := newFinalizationWiringGateStub()
	orig := finalizationTerminalGateFor
	finalizationTerminalGateFor = func(*appPkg.App) session.FinalizationTerminalGate { return stub }
	t.Cleanup(func() { finalizationTerminalGateFor = orig })

	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()
	sess, err := a.Sessions.Create(ctx, "ws gated")
	require.NoError(t, err)
	stub.activate(sess.ID)

	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 100)
	hub.register <- client

	subscribeAndBroadcast(ctx, a, hub)
	messages := a.Messages.(apiEventsSubscriberCounter)
	finalizationWaitForSubscriberCount(t, "gated ws message feed", messages.GetSubscriberCount, messages.GetSubscriberCount()+1)

	// Non-terminal assistant message broadcasts immediately.
	partial, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "partial"}},
	})
	require.NoError(t, err)
	finalizationWaitForEvent(t, client.send, func(env WSMessage) bool {
		return env.Type == EventMessageCreated && strings.Contains(string(env.Payload), partial.ID)
	}, "created broadcast of the non-terminal message")

	// Terminal assistant message: created broadcasts, the finishing update
	// is held.
	final, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "final"}},
	})
	require.NoError(t, err)
	final.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, final))
	<-stub.awaitEntered()
	require.Equal(t, []string{sess.ID}, stub.awaitedOwners())

	// While the hold is active the hub still broadcasts other events.
	require.NoError(t, a.Sessions.Rename(ctx, sess.ID, "renamed under hold"))
	finalizationWaitForEvent(t, client.send, func(env WSMessage) bool {
		return env.Type == EventSessionUpdated
	}, "session rename broadcast while the hold is active")

	stub.seal(sess.ID)
	finalizationWaitForEvent(t, client.send, func(env WSMessage) bool {
		return env.Type == EventMessageUpdated && strings.Contains(string(env.Payload), final.ID)
	}, "held terminal message broadcast after seal")
}

// Flag-off regression through the production store derivation: the handler
// built over apiAppWriteStore (the real route construction shape) resolves
// the nil gate from the flag-off fixture app and delivers a terminal "run"
// event immediately.
func TestAPIEventsHandlerAppStoreFlagOffDeliversRunImmediately(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	sess, err := a.Sessions.Create(t.Context(), "handler flag off")
	require.NoError(t, err)
	log := NewAPIEventLog(nil)
	h := newAPIEventsHandler(apiAppWriteStore{app: a}, log, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, base := apiEventsRequest(ctx, "")
	r.SetPathValue("id", sess.ID)
	var held APIEvent
	w := &publishOnFirstWrite{apiEventsFlushRecorder: base, publish: func() {
		held, _ = log.Publish(sess.ID, "run", json.RawMessage(`{"state":"succeeded"}`))
	}}
	done := make(chan struct{})
	go func() { defer close(done); h(w, r) }()
	awaitAPIEventsSignal(t, base.flushed, "stream start")
	awaitAPIEventsSignal(t, base.flushed, "run event flush")
	cancel()
	awaitAPIEventsSignal(t, done, "handler exit")
	require.Contains(t, base.Body.String(),
		"id: "+finalizationRunEventID(held)+"\nevent: run\n",
		"with the flag off the terminal run event must be delivered immediately")
}

// Flag-on (stub gate): a terminal "run" event of the subscribed session is
// held until the seal; a later non-terminal event still streams while the
// hold is active, so the SSE stream is not blocked.
func TestAPIEventsHandlerGateHoldsTerminalRunUntilSealed(t *testing.T) {
	stub := newFinalizationWiringGateStub()
	stub.activate("s")
	log := NewAPIEventLog(nil)
	h := newAPIEventsHandler(apiEventsTestStore{item: session.Session{ID: "s"}}, log, nil, nil, stub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, base := apiEventsRequest(ctx, "")
	var held APIEvent
	w := &publishOnFirstWrite{apiEventsFlushRecorder: base, publish: func() {
		held, _ = log.Publish("s", "run", json.RawMessage(`{"state":"succeeded"}`))
	}}
	done := make(chan struct{})
	go func() { defer close(done); h(w, r) }()
	awaitAPIEventsSignal(t, base.flushed, "stream start")
	<-stub.awaitEntered()
	require.Equal(t, []string{"s"}, stub.awaitedOwners(),
		"the hold must await the subscribed session id")

	// A later non-terminal event streams while the run result is held.
	benign, _ := log.Publish("s", "message", json.RawMessage(`{"id":"m1"}`))
	awaitAPIEventsSignal(t, base.flushed, "non-terminal event flush")
	require.Contains(t, base.Body.String(), "id: "+finalizationRunEventID(benign)+"\nevent: message\n")
	require.NotContains(t, base.Body.String(), "id: "+finalizationRunEventID(held)+"\nevent: run\n",
		"the terminal run event must stay held while the record is active")

	stub.seal("s")
	awaitAPIEventsSignal(t, base.flushed, "held run event flush")
	cancel()
	awaitAPIEventsSignal(t, done, "handler exit")
	body := base.Body.String()
	runFrame := "id: " + finalizationRunEventID(held) + "\nevent: run\n"
	require.Equal(t, 1, strings.Count(body, runFrame), "each event is delivered exactly once: %q", body)
	require.Less(t, strings.Index(body, "event: message"), strings.Index(body, runFrame),
		"the run result, though published first, must be delivered after the seal")
}

// finalizationRunEventID formats an event id the way apiSSEWrite does.
func finalizationRunEventID(event APIEvent) string {
	return strconv.FormatUint(event.ID, 10)
}
