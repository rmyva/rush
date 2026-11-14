package server

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
)

const (
	// finalizationNotifyOutBuffer is the output channel buffer of a gated
	// stream pump.
	finalizationNotifyOutBuffer = 256

	// finalizationNotifyReleaseBuffer is the buffer of the pump's
	// release-notice channel.
	finalizationNotifyReleaseBuffer = 64
)

// finalizationRelease reports the outcome of one releaser goroutine: sealed
// true flushes the owner's held queue, sealed false drops it.
type finalizationRelease struct {
	owner  string
	sealed bool
}

// FinalizationGatedStream is a generic, order-preserving hold pump over any
// event stream. Terminal events (terminal(ev) true) of an owner whose
// finalization record is still active are held per owner until the record
// leaves the active phase; every other event forwards immediately. The gate
// is read at admission time, so once the record is sealed later terminal
// events pass immediately while an already-held queue flushes at its
// release. Non-held events keep their relative arrival order and held events
// keep theirs; each event is delivered at most once (held queues flush
// exactly once, on the single release notice).
//
// Gate calls (AllowTerminal on the pump, AwaitSealed on per-owner releaser
// goroutines) never run under the hub, the APIEventLog, or the registry
// mutex, and AwaitSealed never blocks the stream or other owners' holds.
//
// A nil gate, a nil input channel, or a nil owner/terminal selector returns
// in unchanged: the inert wiring, no goroutine at all.
func FinalizationGatedStream[T any](
	ctx context.Context,
	gate session.FinalizationTerminalGate,
	in <-chan T,
	owner func(T) string,
	terminal func(T) bool,
) <-chan T {
	if gate == nil || in == nil || owner == nil || terminal == nil {
		return in
	}
	out := make(chan T, finalizationNotifyOutBuffer)
	done := make(chan struct{})
	var wg sync.WaitGroup
	go func() {
		// Deferred close after wg.Wait: every releaser has reported, so a
		// consumer draining via range terminates deterministically.
		defer close(out)
		held := make(map[string][]T)
		waiting := make(map[string]bool)
		releases := make(chan finalizationRelease, finalizationNotifyReleaseBuffer)
		closed := false
		ctxDone := ctx.Done()
		inCh := in
		for !closed || len(waiting) > 0 {
			select {
			case ev, ok := <-inCh:
				if !ok {
					closed = true
					// Stop selecting on the closed input.
					inCh = nil
					continue
				}
				o := owner(ev)
				// Forward on admission: empty owner, non-terminal events,
				// and terminal events of a sealed owner never wait.
				if o == "" || !terminal(ev) || gate.AllowTerminal(ctx, o) {
					sendFinalizationEvent(ctx, out, ev)
					continue
				}
				held[o] = append(held[o], ev)
				if !waiting[o] {
					waiting[o] = true
					wg.Add(1)
					go func(o string) {
						defer wg.Done()
						err := gate.AwaitSealed(ctx, o)
						select {
						case releases <- finalizationRelease{owner: o, sealed: err == nil}:
						case <-done:
						}
					}(o)
				}
			case notice := <-releases:
				delete(waiting, notice.owner)
				queue := held[notice.owner]
				delete(held, notice.owner)
				// sealed=false (cancel or gate failure) drops the queue:
				// a held terminal is never delivered as a success.
				if notice.sealed {
					for _, ev := range queue {
						sendFinalizationEvent(ctx, out, ev)
					}
				}
			case <-ctxDone:
				// Cancellation releases all holds WITHOUT delivery and
				// stops reading input; pending releasers report sealed
				// false and are drained normally before shutdown.
				closed = true
				held = make(map[string][]T)
				ctxDone = nil
				inCh = nil
			}
		}
		close(done)
		wg.Wait()
	}()
	return out
}

// sendFinalizationEvent delivers one event, abandoning the send on context
// cancellation. Blocking sends preserve order and nothing is dropped while
// ctx is live; a normal consumer always drains, so the abandon path only
// fires during shutdown.
func sendFinalizationEvent[T any](ctx context.Context, out chan<- T, ev T) {
	select {
	case out <- ev:
	case <-ctx.Done():
	}
}

// FinalizationGatedMessageStream decorates the app message broker stream:
// terminal assistant messages of a session with an active finalization record
// are held until the record is sealed; every other event forwards without
// delay and in arrival order. A nil gate returns in unchanged.
func FinalizationGatedMessageStream(
	ctx context.Context,
	gate session.FinalizationTerminalGate,
	in <-chan pubsub.Event[message.Message],
) <-chan pubsub.Event[message.Message] {
	return FinalizationGatedStream(ctx, gate, in,
		func(ev pubsub.Event[message.Message]) string {
			return ev.Payload.SessionID
		},
		func(ev pubsub.Event[message.Message]) bool {
			return session.FinalizationTerminalMessage(ev.Payload)
		})
}

// FinalizationGatedSessionStream decorates the app session broker stream:
// session-ended updates (non-empty EndedReason on an UpdatedEvent) of a
// session with an active finalization record are held until sealed; created
// and deleted events and ordinary updates forward without delay.
func FinalizationGatedSessionStream(
	ctx context.Context,
	gate session.FinalizationTerminalGate,
	in <-chan pubsub.Event[session.Session],
) <-chan pubsub.Event[session.Session] {
	return FinalizationGatedStream(ctx, gate, in,
		func(ev pubsub.Event[session.Session]) string {
			return ev.Payload.ID
		},
		func(ev pubsub.Event[session.Session]) bool {
			// Only updated rows carry the ended marker; created and
			// deleted notifications are structural, never terminal.
			return ev.Type == pubsub.UpdatedEvent &&
				session.FinalizationSessionEnded(ev.Payload)
		})
}

// FinalizationGatedAPIEventStream decorates ONE session's APIEventLog
// subscription: terminal run results (the HTTP API's completion events, type
// "run", state succeeded|failed|cancelled — matching terminalAPIRunState) of
// ownerSessionID are held until sealed. Message and session entries of the
// API log carry no finish or ended marker (the API projection strips them),
// so they are never held here; the feeder-level stream wrappers cover them
// upstream. An empty ownerSessionID or nil gate returns in unchanged.
func FinalizationGatedAPIEventStream(
	ctx context.Context,
	gate session.FinalizationTerminalGate,
	ownerSessionID string,
	in <-chan APIEvent,
) <-chan APIEvent {
	if ownerSessionID == "" {
		return in
	}
	return FinalizationGatedStream(ctx, gate, in,
		func(APIEvent) string {
			return ownerSessionID
		},
		func(ev APIEvent) bool {
			return finalizationAPIEventTerminal(ev)
		})
}

// finalizationAPIRunEvent is the published shape of a "run" API event
// (api_events_feeder.go:29: log.Publish(run.SessionID, "run", data)).
type finalizationAPIRunEvent struct {
	State string `json:"state"`
}

// finalizationAPIEventTerminal classifies one API event stream entry as a
// terminal run result; unreadable payloads are never treated as terminal.
func finalizationAPIEventTerminal(event APIEvent) bool {
	// The literal matches the feeder's "run" publish (api_events_feeder.go:29).
	if event.Type != "run" {
		return false
	}
	var payload finalizationAPIRunEvent
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		return false
	}
	return terminalAPIRunState(APIRunState(payload.State))
}
