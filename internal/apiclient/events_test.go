package apiclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const sseTestTimeout = 10 * time.Second

func newEventsTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	isolateSSETestEnvironment(t)
	randBytes := strings.NewReader(strings.Repeat("r", 4096))
	client, _, err := New(Options{
		BaseURL: server.URL, Token: "events-test-token", AllowCleartext: true,
		Rand: randBytes,
		Sleeper: func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				return nil
			}
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client
}

func receiveEvent(t *testing.T, events <-chan Event) (Event, bool) {
	t.Helper()
	select {
	case event, ok := <-events:
		return event, ok
	case <-time.After(sseTestTimeout):
		t.Fatal("timed out waiting for event")
		return Event{}, false
	}
}

func receiveEventsDone(t *testing.T, done func() error) error {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- done() }()
	select {
	case err := <-result:
		return err
	case <-time.After(sseTestTimeout):
		t.Fatal("timed out waiting for event stream closure")
		return nil
	}
}

func assertEventRequest(t *testing.T, r *http.Request, lastID string) {
	t.Helper()
	if r.Method != http.MethodGet || r.URL.Path != "/api/v1/sessions/session/events" || r.URL.RawQuery != "" {
		t.Errorf("request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
	}
	for key, want := range map[string]string{
		"Authorization": "Bearer events-test-token",
		"Accept":        "text/event-stream",
		"Cache-Control": "no-cache",
	} {
		if got := r.Header.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if got := r.Header.Get("Last-Event-ID"); got != lastID {
		t.Errorf("Last-Event-ID = %q, want %q", got, lastID)
	}
}

func writeSSE(t *testing.T, w http.ResponseWriter, value string) {
	t.Helper()
	if _, err := fmt.Fprint(w, value); err != nil {
		t.Errorf("write SSE: %v", err)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func TestEventsReconnectResumesAfterTruncatedFrames(t *testing.T) {
	for _, tc := range []struct {
		name string
		cut  string
	}{
		{name: "mid-frame", cut: "id: 2\ndata: {\"value\":"},
		{name: "mid-line", cut: "id: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests++
				n := requests
				mu.Unlock()
				wantID := ""
				if n > 1 {
					wantID = "1"
				}
				assertEventRequest(t, r, wantID)
				if n == 1 {
					writeSSE(t, w, "id: 1\ndata: {\"value\":1}\n\n"+tc.cut)
					return
				}
				writeSSE(t, w, "id: 1\ndata: {\"value\":1}\n\nid: 2\ndata: {\"value\":2}\n\nevent: closed\ndata: {}\n\n")
			}))
			defer server.Close()
			client := newEventsTestClient(t, server)
			events, done := client.Events(context.Background(), "session", EventsOptions{
				MaxReconnects: 2,
				Backoff:       func(int) time.Duration { return 0 },
				Sleeper:       func(context.Context, time.Duration) error { return nil },
			})
			first, ok := receiveEvent(t, events)
			if !ok || first.ID != 1 || first.Type != "message" || string(first.Data) != `{"value":1}` {
				t.Fatalf("first event = %+v, open=%v", first, ok)
			}
			second, ok := receiveEvent(t, events)
			if !ok || second.ID != 2 || string(second.Data) != `{"value":2}` {
				t.Fatalf("second event = %+v, open=%v", second, ok)
			}
			if _, ok := receiveEvent(t, events); ok {
				t.Fatal("stream emitted a duplicate or unexpected event")
			}
			if err := receiveEventsDone(t, done); err != nil {
				t.Fatalf("stream error = %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if requests != 2 {
				t.Fatalf("requests = %d, want 2", requests)
			}
		})
	}
}

func TestEventsResetGapAndOverflowReconnect(t *testing.T) {
	t.Run("reset gap", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assertEventRequest(t, r, "7")
			writeSSE(t, w, "id: 9\nevent: reset\ndata: {\"from\":7,\"to\":9}\n\nevent: closed\ndata: {}\n\n")
		}))
		defer server.Close()
		client := newEventsTestClient(t, server)
		events, done := client.Events(context.Background(), "session", EventsOptions{LastEventID: 7})
		got, ok := receiveEvent(t, events)
		if !ok || got.ID != 9 || got.Type != "reset" || !got.Gap || string(got.Data) != `{"from":7,"to":9}` {
			t.Fatalf("reset event = %+v, open=%v", got, ok)
		}
		if _, ok := receiveEvent(t, events); ok {
			t.Fatal("closed marker was emitted")
		}
		if err := receiveEventsDone(t, done); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("overflow resumes immediately", func(t *testing.T) {
		var mu sync.Mutex
		requests := 0
		var waits []time.Duration
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			requests++
			n := requests
			mu.Unlock()
			if n == 1 {
				assertEventRequest(t, r, "")
				writeSSE(t, w, "id: 1\nevent: overflow\ndata: {}\n\n")
				return
			}
			assertEventRequest(t, r, "")
			writeSSE(t, w, "id: 1\ndata: {\"ok\":true}\n\nevent: closed\ndata: {}\n\n")
		}))
		defer server.Close()
		client := newEventsTestClient(t, server)
		events, done := client.Events(context.Background(), "session", EventsOptions{
			MaxReconnects: 1,
			Backoff:       func(int) time.Duration { return time.Hour },
			Sleeper:       func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
		})
		got, ok := receiveEvent(t, events)
		if !ok || got.ID != 1 || string(got.Data) != `{"ok":true}` {
			t.Fatalf("event = %+v, open=%v", got, ok)
		}
		if _, ok := receiveEvent(t, events); ok {
			t.Fatal("unexpected event")
		}
		if err := receiveEventsDone(t, done); err != nil {
			t.Fatal(err)
		}
		if len(waits) != 0 {
			t.Fatalf("overflow waits = %v, want none", waits)
		}
	})
}

func TestEventsTerminalHTTPStatusesAndRetryAfter(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				assertEventRequest(t, r, "")
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, `{"error":"terminal"}`)
			}))
			defer server.Close()
			client := newEventsTestClient(t, server)
			events, done := client.Events(context.Background(), "session", EventsOptions{MaxReconnects: 3})
			if _, ok := receiveEvent(t, events); ok {
				t.Fatal("terminal response emitted event")
			}
			err := receiveEventsDone(t, done)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != status {
				t.Fatalf("error = %v, want API status %d", err, status)
			}
			if strings.Contains(err.Error(), "events-test-token") {
				t.Fatalf("error exposed token: %v", err)
			}
			if got := hits.Load(); got != 1 {
				t.Fatalf("terminal status %d was retried: %d requests", status, got)
			}
		})
	}

	t.Run("503 honors retry after then reconnects", func(t *testing.T) {
		var requests int
		var waits []time.Duration
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			assertEventRequest(t, r, "")
			if requests == 1 {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprint(w, `{"error":"busy"}`)
				return
			}
			writeSSE(t, w, "event: closed\ndata: {}\n\n")
		}))
		defer server.Close()
		client := newEventsTestClient(t, server)
		events, done := client.Events(context.Background(), "session", EventsOptions{
			MaxReconnects: 1,
			Backoff:       func(int) time.Duration { return time.Millisecond },
			Sleeper:       func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
		})
		if _, ok := receiveEvent(t, events); ok {
			t.Fatal("closed stream emitted event")
		}
		if err := receiveEventsDone(t, done); err != nil {
			t.Fatal(err)
		}
		if requests != 2 || len(waits) != 1 || waits[0] != 2*time.Second {
			t.Fatalf("requests=%d waits=%v, want 2 and [2s]", requests, waits)
		}
	})
}

type manualEventsTimer struct {
	mu      sync.Mutex
	ch      chan time.Time
	stopped bool
}

func newManualEventsTimer() *manualEventsTimer {
	return &manualEventsTimer{ch: make(chan time.Time, 1)}
}
func (t *manualEventsTimer) C() <-chan time.Time { return t.ch }
func (t *manualEventsTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasActive := !t.stopped
	t.stopped = true
	return wasActive
}

func (t *manualEventsTimer) Reset(time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasActive := !t.stopped
	t.stopped = false
	return wasActive
}

func (t *manualEventsTimer) Fire() {
	select {
	case t.ch <- time.Now():
	default:
	}
}

func TestEventsWatchdogAndCancellation(t *testing.T) {
	t.Run("watchdog closes stalled stream", func(t *testing.T) {
		started := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		}))
		defer server.Close()
		timer := newManualEventsTimer()
		timerReady := make(chan struct{})
		client := newEventsTestClient(t, server)
		streamCtx, stopStream := context.WithCancel(context.Background())
		defer stopStream() // runs before server.Close so a stalled stream cannot hang the test
		events, done := client.Events(streamCtx, "session", EventsOptions{
			MaxReconnects:    0,
			HeartbeatTimeout: time.Hour,
			WatchdogTimer: func(d time.Duration) EventsTimer {
				if d != time.Hour {
					t.Errorf("watchdog duration = %v", d)
				}
				close(timerReady)
				return timer
			},
		})
		select {
		case <-started:
		case <-time.After(sseTestTimeout):
			t.Fatal("server did not start stream")
		}
		select {
		case <-timerReady:
		case <-time.After(sseTestTimeout):
			t.Fatal("watchdog timer was not created")
		}
		timer.Fire()
		if _, ok := receiveEvent(t, events); ok {
			t.Fatal("watchdog stream emitted event")
		}
		if err := receiveEventsDone(t, done); !errors.Is(err, io.EOF) {
			t.Fatalf("watchdog terminal error = %v, want EOF", err)
		}
	})

	t.Run("cancel stops reconnect wait", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		waiting := make(chan struct{})
		client := newEventsTestClient(t, server)
		events, done := client.Events(ctx, "session", EventsOptions{
			MaxReconnects: 2,
			Sleeper: func(ctx context.Context, _ time.Duration) error {
				close(waiting)
				<-ctx.Done()
				return ctx.Err()
			},
		})
		select {
		case <-waiting:
		case <-time.After(sseTestTimeout):
			cancel()
			t.Fatal("reconnect sleeper did not start")
		}
		cancel()
		if _, ok := receiveEvent(t, events); ok {
			t.Fatal("canceled stream emitted event")
		}
		if err := receiveEventsDone(t, done); err != nil {
			t.Fatalf("canceled stream error = %v", err)
		}
	})
}

func TestEventsBackoffSequence(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	var waits []time.Duration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if n == 2 {
			writeSSE(t, w, "id: 1\ndata: {}\n\n")
			return
		}
		writeSSE(t, w, "event: closed\ndata: {}\n\n")
	}))
	defer server.Close()
	client := newEventsTestClient(t, server)
	events, done := client.Events(context.Background(), "session", EventsOptions{
		MaxReconnects: 3,
		Backoff:       func(n int) time.Duration { return time.Duration(n+1) * time.Second },
		Rand:          strings.NewReader(strings.Repeat("\x00", 8)),
		Sleeper:       func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
	})
	if got, ok := receiveEvent(t, events); !ok || got.ID != 1 {
		t.Fatalf("event = %+v, open=%v", got, ok)
	}
	if _, ok := receiveEvent(t, events); ok {
		t.Fatal("closed stream emitted event")
	}
	if err := receiveEventsDone(t, done); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != time.Second {
		t.Fatalf("backoff waits = %v, want [1s 1s]", waits)
	}
}
