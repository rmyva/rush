package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testRequestClient(t *testing.T, server *httptest.Server, opts Options) *Client {
	t.Helper()
	for _, name := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(name, t.TempDir())
	}
	if opts.BaseURL == "" {
		opts.BaseURL = server.URL
	}
	opts.Token = "test-token"
	if opts.MaxResponseBytes == 0 {
		opts.MaxResponseBytes = 1024
	}
	c, _, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.http.CloseIdleConnections)
	return c
}

func requestContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func TestRequestKeepsEscapedPathSegments(t *testing.T) {
	var gotURI, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI, gotPath = r.RequestURI, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c := testRequestClient(t, server, Options{})
	ctx, cancel := requestContext(t)
	defer cancel()
	if _, _, _, err := c.do(ctx, http.MethodGet, "/sessions/"+url.PathEscape("a b/c"), nil, nil, reqOpts{}); err != nil {
		t.Fatal(err)
	}
	if gotURI != "/api/v1/sessions/a%20b%2Fc" || gotPath != "/api/v1/sessions/a b/c" {
		t.Errorf("request URI = %q, path = %q", gotURI, gotPath)
	}
}

func TestRequestShapeAndQueryRedaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var gotBody map[string]string
		if r.URL.Path != "/api/v1/items" || r.URL.Query().Get("page") != "2" || r.URL.Query().Get("base") != "kept" || strings.Contains(r.URL.String(), "test-token") {
			t.Errorf("request URL = %s", r.URL)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request JSON: %v", err)
		} else if gotBody["x"] != "y" {
			t.Errorf("request JSON = %#v", gotBody)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("Accept") != "application/json, application/problem+json" || r.Header.Get("User-Agent") != "test-agent" {
			t.Errorf("headers = %#v", r.Header)
		}
		if r.Header.Get("X-Request-Id") == "" || len(r.Header.Get("X-Request-Id")) != 16 || r.Header.Get("Idempotency-Key") != "key-1" {
			t.Errorf("request ID/key missing: %#v", r.Header)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		w.Header().Set("X-Request-Id", "server-id")
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	c := testRequestClient(t, server, Options{UserAgent: "test-agent", BaseURL: server.URL + "?base=kept"})
	ctx, cancel := requestContext(t)
	defer cancel()
	status, h, _, err := c.do(ctx, http.MethodPost, "/items", map[string]string{"page": "2"}, map[string]string{"x": "y"}, reqOpts{IdempotencyKey: "key-1"})
	if err != nil || status != http.StatusCreated || h.Get("X-Request-Id") != "server-id" {
		t.Fatalf("do = %d, %v", status, err)
	}
}

func TestRequestPathValidation(t *testing.T) {
	isolateClientEnv(t)
	for _, path := range []string{"", "items", "//host", "http://host/x", "/a/../b", "/a/%2e%2e/b", "/x?token=secret", "/x#frag", "/x\r\ny"} {
		t.Run(strings.ReplaceAll(path, "/", "_"), func(t *testing.T) {
			if err := validateRequestPath(path); err == nil {
				t.Errorf("accepted %q", path)
			}
		})
	}
	if err := validateRequestPath("/items/one"); err != nil {
		t.Fatal(err)
	}
}

func TestRequestIdempotencyKeyValidation(t *testing.T) {
	isolateClientEnv(t)
	for _, key := range []string{"a", strings.Repeat("x", 128), "A-._:9"} {
		if !validIdempotencyKey(key) {
			t.Errorf("rejected valid key %q", key)
		}
	}
	for _, key := range []string{"", strings.Repeat("x", 129), "bad key", "x\r\ny"} {
		if validIdempotencyKey(key) {
			t.Errorf("accepted invalid key %q", key)
		}
	}
}

func TestRequestRetryGetRetryAfter(t *testing.T) {
	var calls atomic.Int32
	var wait time.Duration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	c := testRequestClient(t, server, Options{Sleeper: func(_ context.Context, d time.Duration) error { wait = d; return nil }})
	ctx, cancel := requestContext(t)
	defer cancel()
	if _, _, _, err := c.do(ctx, "GET", "/x", nil, nil, reqOpts{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || wait != 3*time.Second {
		t.Fatalf("calls=%d wait=%v", calls.Load(), wait)
	}
}

func TestRequestPostRetriesConnectionResetWithSameKey(t *testing.T) {
	var calls atomic.Int32
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if calls.Add(1) == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("server does not support hijacking")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	c := testRequestClient(t, server, Options{Sleeper: func(context.Context, time.Duration) error { return nil }})
	ctx, cancel := requestContext(t)
	defer cancel()
	_, _, _, err := c.do(ctx, http.MethodPost, "/x", nil, map[string]string{"x": "y"}, reqOpts{IdempotencyKey: "same-key"})
	if err != nil || calls.Load() != 2 || len(keys) != 2 || keys[0] != "same-key" || keys[1] != keys[0] {
		t.Fatalf("err=%v calls=%d keys=%v", err, calls.Load(), keys)
	}
}

func TestRequestPostWithKeyGivesUpAfterOneRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()
	c := testRequestClient(t, server, Options{Sleeper: func(context.Context, time.Duration) error { return nil }})
	ctx, cancel := requestContext(t)
	defer cancel()
	if _, _, _, err := c.do(ctx, http.MethodPost, "/x", nil, map[string]string{"x": "y"}, reqOpts{IdempotencyKey: "same-key"}); err == nil {
		t.Fatal("persistent connection resets were reported as success")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("attempts = %d, want exactly 2 (one retry)", got)
	}
}

func TestRequestPostWithoutKeyNeverRetries(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer server.Close()
	c := testRequestClient(t, server, Options{Sleeper: func(context.Context, time.Duration) error { return nil }})
	ctx, cancel := requestContext(t)
	defer cancel()
	_, _, _, _ = c.do(ctx, http.MethodPost, "/x", nil, nil, reqOpts{})
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestRequestPostNeverRetriesResponse(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	c := testRequestClient(t, server, Options{Sleeper: func(context.Context, time.Duration) error { return nil }})
	ctx, cancel := requestContext(t)
	defer cancel()
	_, _, _, err := c.do(ctx, "POST", "/x", nil, nil, reqOpts{IdempotencyKey: "key"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestRequestTLSVerificationFailureNotRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer server.Close()
	var sleeps atomic.Int32
	c := testRequestClient(t, server, Options{Pin: "cert-sha256:" + strings.Repeat("0", 64), InsecureSkipVerify: true, Sleeper: func(context.Context, time.Duration) error { sleeps.Add(1); return nil }})
	ctx, cancel := requestContext(t)
	defer cancel()
	_, _, _, err := c.do(ctx, http.MethodGet, "/x", nil, nil, reqOpts{})
	if err == nil || calls.Load() != 0 || sleeps.Load() != 0 || !errors.Is(err, ErrTLSVerification) {
		t.Fatalf("err=%v calls=%d retry sleeps=%d", err, calls.Load(), sleeps.Load())
	}
}

func TestRequestCancelDuringBackoff(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer server.Close()
	started := make(chan struct{})
	c := testRequestClient(t, server, Options{Sleeper: func(ctx context.Context, _ time.Duration) error { close(started); <-ctx.Done(); return ctx.Err() }})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, _, err := c.do(ctx, "GET", "/x", nil, nil, reqOpts{}); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not return promptly")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestRequestFailurePresentationRedactsQueryToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer server.Close()
	c := testRequestClient(t, server, Options{Sleeper: func(context.Context, time.Duration) error { return nil }})
	ctx, cancel := requestContext(t)
	defer cancel()
	_, _, _, err := c.do(ctx, "GET", "/x", map[string]string{"secret": "distinctive-query"}, nil, reqOpts{})
	if strings.Contains(err.Error(), "distinctive-query") || strings.Contains(err.Error(), "test-token") {
		t.Fatalf("error leaked secret: %v", err)
	}
	if !errors.Is((&requestFailure{method: "GET", path: "/x", err: net.ErrClosed}), net.ErrClosed) {
		t.Fatal("failure does not preserve cause")
	}
}

func TestRequestResponseLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("12345")) }))
	defer server.Close()
	c := testRequestClient(t, server, Options{MaxResponseBytes: 4})
	ctx, cancel := requestContext(t)
	defer cancel()
	_, _, _, err := c.do(ctx, "GET", "/x", nil, nil, reqOpts{})
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v", err)
	}
}
