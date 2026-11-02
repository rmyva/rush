package apiclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// firstWait runs one failed connection (503) followed by a clean close and returns
// the reconnect delay the client asked its sleeper for.
func firstWait(t *testing.T, opts EventsOptions) time.Duration {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeSSE(t, w, "event: closed\ndata: {}\n\n")
	}))
	defer server.Close()
	client := newEventsTestClient(t, server)
	var waits []time.Duration
	opts.MaxReconnects = 2
	opts.Sleeper = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	events, done := client.Events(context.Background(), "session", opts)
	if _, ok := receiveEvent(t, events); ok {
		t.Fatal("closed stream emitted an event")
	}
	if err := receiveEventsDone(t, done); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 {
		t.Fatalf("waits = %v, want exactly one", waits)
	}
	return waits[0]
}

func TestEventsBackoffCapAndJitterBounds(t *testing.T) {
	zeros := strings.Repeat("\x00", 16)
	maxed := strings.Repeat("\xff", 16)
	for name, tc := range map[string]struct {
		backoff time.Duration
		rand    string
		want    time.Duration
	}{
		"cap applies to a huge backoff":    {backoff: 10 * time.Minute, rand: zeros, want: maxEventBackoff},
		"jitter adds at most 25 percent":   {backoff: 4 * time.Second, rand: maxed, want: 5 * time.Second},
		"jitter never pushes past the cap": {backoff: maxEventBackoff, rand: maxed, want: maxEventBackoff},
		"no jitter keeps the base":         {backoff: 4 * time.Second, rand: zeros, want: 4 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			got := firstWait(t, EventsOptions{
				Backoff: func(int) time.Duration { return tc.backoff },
				Rand:    strings.NewReader(tc.rand),
			})
			if got != tc.want {
				t.Fatalf("wait = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEventsServerRetryHintRaisesBackoff(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			writeSSE(t, w, "retry: 5000\n\n")
			return
		}
		writeSSE(t, w, "event: closed\ndata: {}\n\n")
	}))
	defer server.Close()
	client := newEventsTestClient(t, server)
	var waits []time.Duration
	events, done := client.Events(context.Background(), "session", EventsOptions{
		MaxReconnects: 2,
		Backoff:       func(int) time.Duration { return time.Millisecond },
		Rand:          strings.NewReader(strings.Repeat("\x00", 8)),
		Sleeper:       func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
	})
	if _, ok := receiveEvent(t, events); ok {
		t.Fatal("closed stream emitted an event")
	}
	if err := receiveEventsDone(t, done); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0] != 5*time.Second {
		t.Fatalf("waits = %v, want [5s] from the server retry hint", waits)
	}
}

func TestEventsOversizeLineIsFatalWithoutReconnect(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		writeSSE(t, w, "data: "+strings.Repeat("x", sseMaxLineBytes+1024))
	}))
	defer server.Close()
	client := newEventsTestClient(t, server)
	events, done := client.Events(context.Background(), "session", EventsOptions{MaxReconnects: 3})
	if _, ok := receiveEvent(t, events); ok {
		t.Fatal("oversize line produced an event")
	}
	if err := receiveEventsDone(t, done); !errors.Is(err, errSSELineTooLong) {
		t.Fatalf("err = %v, want errSSELineTooLong", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("oversize line was retried: %d requests", got)
	}
}
