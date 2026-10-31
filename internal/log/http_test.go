package log

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPRoundTripLogger(t *testing.T) {
	// Create a test server that returns a 500 error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom-Header", "test-value")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "Internal server error", "code": 500}`))
	}))
	defer server.Close()

	client := NewHTTPClient()

	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		server.URL,
		strings.NewReader(`{"test": "data"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-token")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("Expected status code 500, got %d", resp.StatusCode)
	}
}

func TestFormatHeaders(t *testing.T) {
	headers := http.Header{
		"Content-Type":  []string{"application/json"},
		"Authorization": []string{"Bearer secret-token"},
		"X-API-Key":     []string{"api-key-123"},
		"User-Agent":    []string{"test-agent"},
	}

	formatted := formatHeaders(headers)

	if formatted["Authorization"][0] != "[REDACTED]" {
		t.Error("Authorization header should be redacted")
	}
	if formatted["X-API-Key"][0] != "[REDACTED]" {
		t.Error("X-API-Key header should be redacted")
	}
	if formatted["Content-Type"][0] != "application/json" {
		t.Error("Content-Type header should be preserved")
	}
	if formatted["User-Agent"][0] != "test-agent" {
		t.Error("User-Agent header should be preserved")
	}
}

// chunkedServer serves each chunk, flushing it, sleeping interChunk between
// chunks. It returns promptly when the client disconnects (request context
// canceled) so the test stays fast.
func chunkedServer(t *testing.T, chunks []string, interChunk time.Duration) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		for _, c := range chunks {
			if _, err := w.Write([]byte(c)); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-time.After(interChunk):
			case <-r.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestHTTPRoundTripLogger_StreamsWhenDebugDisabled proves the core fix: with
// slog above Debug, the response body is the live stream. With the old
// unconditional drainBody, RoundTrip blocked until the WHOLE body was
// buffered and the first chunk only arrived after the full ~1.2s stream.
func TestHTTPRoundTripLogger_StreamsWhenDebugDisabled(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})))

	const interChunk = 400 * time.Millisecond
	server := chunkedServer(t, []string{"chunk-0;", "chunk-1;", "chunk-2;"}, interChunk)

	client := NewHTTPClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	probe := make([]byte, 64)
	n, err := io.ReadAtLeast(resp.Body, probe, 1)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	elapsed := time.Since(start)

	if !strings.Contains(string(probe[:n]), "chunk-0") {
		t.Fatalf("first read missing chunk-0: %q", probe[:n])
	}
	// Full stream takes ~3*interChunk = 1.2s. The first chunk must arrive
	// far sooner, proving the body was not fully buffered before return.
	if elapsed > 300*time.Millisecond {
		t.Fatalf("first chunk took %v; response body was buffered, not streamed", elapsed)
	}
}

// TestHTTPRoundTripLogger_StreamsAndLogsWhenDebugEnabled proves that even
// when the debug wrapper IS installed (body wrapped in teeBody, which only
// happens when the LogHTTPBodies opt-in is also set — see P3.1), the first
// chunk still streams in immediately, and the response preview is logged
// once when the body is closed.
func TestHTTPRoundTripLogger_StreamsAndLogsWhenDebugEnabled(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	prevFlag := LogHTTPBodies
	LogHTTPBodies = true
	t.Cleanup(func() { LogHTTPBodies = prevFlag })

	const interChunk = 400 * time.Millisecond
	server := chunkedServer(t, []string{"chunk-0;", "chunk-1;", "chunk-2;"}, interChunk)

	client := NewHTTPClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	// Even with the debug wrapper installed, the first chunk must stream in
	// immediately rather than after the whole body is buffered.
	probe := make([]byte, 64)
	n, err := io.ReadAtLeast(resp.Body, probe, 1)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	firstRead := time.Since(start)
	if !strings.Contains(string(probe[:n]), "chunk-0") {
		t.Fatalf("first read missing chunk-0: %q", probe[:n])
	}
	if firstRead > 300*time.Millisecond {
		t.Fatalf("first chunk took %v under debug; body not streamed", firstRead)
	}

	// Drain the live stream so the deferred Close-time response log fires.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain: %v", err)
	}
	resp.Body.Close()

	logged := logBuf.String()
	if !strings.Contains(logged, "HTTP Response") {
		t.Errorf("expected 'HTTP Response' debug log; got:\n%s", logged)
	}
	if !strings.Contains(logged, "chunk-0") {
		t.Errorf("expected response preview to include chunk-0; got:\n%s", logged)
	}
}

// erroringReadCloser is an io.ReadCloser whose Read always fails, used to
// simulate a request body that breaks partway through — e.g. a pipe whose
// writer side errored, or a network-backed body reader that hit a transport
// error before RoundTrip's own preview read got to it.
type erroringReadCloser struct {
	err error
}

func (e *erroringReadCloser) Read(p []byte) (int, error) { return 0, e.err }
func (e *erroringReadCloser) Close() error               { return nil }

// unreachableRoundTripper fails the test if RoundTrip is ever called on it —
// used to prove the L-4 fix returns before reaching the real transport at
// all when the body preview read fails, rather than reconstructing a body
// from the broken reader and sending it anyway.
type unreachableRoundTripper struct{ t *testing.T }

func (u unreachableRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	u.t.Fatal("inner Transport.RoundTrip must not be called when the body preview read fails")
	return nil, nil
}

// TestHTTPRoundTripLogger_RequestBodyReadErrorFailsFast proves the L-4 fix:
// when LogHTTPBodies is on and reading the request body preview fails (the
// body reader itself is broken, not just truncated by maxBodyPreview — a
// LimitReader hitting its cap surfaces as a clean io.EOF, not an error),
// RoundTrip returns the error immediately instead of logging-and-continuing
// with a request body reconstructed from the same broken reader. Before this
// fix, RoundTrip would log an Error line and press on into the real
// transport with a corrupted/truncated body.
func TestHTTPRoundTripLogger_RequestBodyReadErrorFailsFast(t *testing.T) {
	prevFlag := LogHTTPBodies
	LogHTTPBodies = true
	t.Cleanup(func() { LogHTTPBodies = prevFlag })

	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})))

	wantErr := errors.New("simulated broken body reader")
	logger := &HTTPRoundTripLogger{Transport: unreachableRoundTripper{t: t}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.invalid/", &erroringReadCloser{err: wantErr})
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1 // unknown length, matches a streaming body

	resp, roundTripErr := logger.RoundTrip(req)

	if resp != nil {
		resp.Body.Close()
		t.Errorf("expected a nil response on body-read failure, got %+v", resp)
	}
	if roundTripErr == nil {
		t.Fatal("expected RoundTrip to return an error when the body preview read fails")
	}
	if !errors.Is(roundTripErr, wantErr) {
		t.Errorf("expected the returned error to wrap the underlying read error via errors.Is, got %v", roundTripErr)
	}
}

// TestHTTPRoundTripLogger_BodyNotLoggedByDefault proves the P3.1 fix: even
// with slog at Debug level (the "someone passed --debug" case), request and
// response bodies are NOT captured or logged unless the separate
// LogHTTPBodies opt-in is also set. Only method/url/status/content_length/
// duration should appear.
func TestHTTPRoundTripLogger_BodyNotLoggedByDefault(t *testing.T) {
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	prevFlag := LogHTTPBodies
	LogHTTPBodies = false
	t.Cleanup(func() { LogHTTPBodies = prevFlag })

	const secretReq = `{"api_key": "sk-super-secret-request", "prompt": "hello"}`
	const secretResp = `{"choices": [{"message": "hi"}], "api_key_echo": "sk-super-secret-response"}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(secretResp))
	}))
	t.Cleanup(server.Close)

	client := NewHTTPClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(secretReq))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer request-secret-token")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain: %v", err)
	}
	resp.Body.Close()

	logged := logBuf.String()

	if !strings.Contains(logged, "HTTP Request") || !strings.Contains(logged, "HTTP Response") {
		t.Fatalf("expected both request/response debug lines; got:\n%s", logged)
	}
	if !strings.Contains(logged, "status_code=200") && !strings.Contains(logged, `status_code=200`) {
		// status_code is logged as a plain attr; just check the value is present somewhere.
		if !strings.Contains(logged, "200") {
			t.Errorf("expected status code 200 to be logged; got:\n%s", logged)
		}
	}
	if !strings.Contains(logged, "duration_ms") {
		t.Errorf("expected duration_ms to be logged; got:\n%s", logged)
	}
	if !strings.Contains(logged, "content_length") {
		t.Errorf("expected content_length to be logged; got:\n%s", logged)
	}

	if strings.Contains(logged, "sk-super-secret-request") {
		t.Errorf("request body leaked into debug log when LogHTTPBodies=false:\n%s", logged)
	}
	if strings.Contains(logged, "sk-super-secret-response") {
		t.Errorf("response body leaked into debug log when LogHTTPBodies=false:\n%s", logged)
	}
	if strings.Contains(logged, "hello") || strings.Contains(logged, "\"hi\"") {
		t.Errorf("body content leaked into debug log when LogHTTPBodies=false:\n%s", logged)
	}
	if strings.Contains(logged, `"body"`) {
		t.Errorf("a \"body\" field should not be present at all when LogHTTPBodies=false:\n%s", logged)
	}
}

// TestHTTPRoundTripLogger_BodyLoggedWithRedactionWhenOptedIn proves that
// when LogHTTPBodies=true, bodies ARE captured and logged, but known
// secret-shaped JSON fields (api_key, authorization, token, secret,
// password, etc. at any nesting depth) are redacted first.
func TestHTTPRoundTripLogger_BodyLoggedWithRedactionWhenOptedIn(t *testing.T) {
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	prevFlag := LogHTTPBodies
	LogHTTPBodies = true
	t.Cleanup(func() { LogHTTPBodies = prevFlag })

	const secretReq = `{"api_key": "sk-super-secret-request", "prompt": "hello-world", "nested": {"password": "hunter2"}}`
	const secretResp = `{"choices": [{"message": "hi-there"}], "auth": {"token": "resp-secret-tok"}}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(secretResp))
	}))
	t.Cleanup(server.Close)

	client := NewHTTPClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(secretReq))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain: %v", err)
	}
	resp.Body.Close()

	logged := logBuf.String()

	// Secrets must never appear verbatim.
	if strings.Contains(logged, "sk-super-secret-request") {
		t.Errorf("request api_key leaked unredacted:\n%s", logged)
	}
	if strings.Contains(logged, "hunter2") {
		t.Errorf("nested request password leaked unredacted:\n%s", logged)
	}
	if strings.Contains(logged, "resp-secret-tok") {
		t.Errorf("nested response token leaked unredacted:\n%s", logged)
	}

	// Non-sensitive content must still be present (proves body logging is
	// genuinely on, not just silently dropping everything).
	if !strings.Contains(logged, "hello-world") {
		t.Errorf("expected non-sensitive request field to be logged; got:\n%s", logged)
	}
	if !strings.Contains(logged, "hi-there") {
		t.Errorf("expected non-sensitive response field to be logged; got:\n%s", logged)
	}

	// The redaction marker must appear (proves redaction actually ran).
	if !strings.Contains(logged, "[REDACTED]") {
		t.Errorf("expected [REDACTED] marker in logged body; got:\n%s", logged)
	}
}

// TestRedactBodySecrets_TokenUsageFieldsNotRedacted proves the "token"
// substring in sensitiveBodyKeySubstrings doesn't over-match LLM
// token-COUNT fields, which all use the plural "tokens" (max_tokens,
// prompt_tokens, completion_tokens, input_tokens, output_tokens,
// cache_creation_input_tokens, max_completion_tokens, total_tokens) — every
// one of these used to be silently replaced with the string "[REDACTED]"
// (also corrupting its JSON type from number to string) whenever
// RUSH_LOG_HTTP_BODIES was on, defeating the exact usage-accounting
// inspection that flag exists for. Credential-shaped SINGULAR "*token*"
// fields (access_token, refresh_token, auth's nested token) must still
// redact normally — this is not a loosening of the "token" match, only a
// narrow exclusion for the plural/count shape.
func TestRedactBodySecrets_TokenUsageFieldsNotRedacted(t *testing.T) {
	body := `{
		"max_tokens": 4096,
		"access_token": "should-be-redacted",
		"usage": {
			"prompt_tokens": 10,
			"completion_tokens": 5,
			"total_tokens": 15,
			"input_tokens": 10,
			"output_tokens": 5,
			"cache_creation_input_tokens": 2,
			"max_completion_tokens": 8192
		},
		"auth": {
			"refresh_token": "also-should-be-redacted"
		}
	}`

	got := redactBodySecrets(body)

	for _, field := range []string{
		"\"max_tokens\": 4096",
		"\"prompt_tokens\": 10",
		"\"completion_tokens\": 5",
		"\"total_tokens\": 15",
		"\"input_tokens\": 10",
		"\"output_tokens\": 5",
		"\"cache_creation_input_tokens\": 2",
		"\"max_completion_tokens\": 8192",
	} {
		if !strings.Contains(got, field) {
			t.Errorf("token-count field was redacted or altered, want verbatim %q; got:\n%s", field, got)
		}
	}

	if strings.Contains(got, "should-be-redacted") {
		t.Errorf("credential-shaped singular *_token field leaked unredacted:\n%s", got)
	}
	if strings.Contains(got, "also-should-be-redacted") {
		t.Errorf("nested credential-shaped singular *_token field leaked unredacted:\n%s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected [REDACTED] marker for the credential fields; got:\n%s", got)
	}
}

// TestRedactBodySecrets_SSE proves per-line SSE redaction: each "data: {...}"
// line's JSON payload is redacted independently.
func TestRedactBodySecrets_SSE(t *testing.T) {
	body := "event: message\n" +
		`data: {"delta": "hello", "api_key": "leak-me"}` + "\n" +
		"\n" +
		`data: {"delta": "world"}` + "\n"

	got := redactBodySecrets(body)

	if strings.Contains(got, "leak-me") {
		t.Errorf("SSE data line api_key leaked unredacted:\n%s", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("SSE redaction dropped non-sensitive content:\n%s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected [REDACTED] marker:\n%s", got)
	}
}

func TestHTTPRoundTripLogger_RedactsRequestURL(t *testing.T) {
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	logger := &HTTPRoundTripLogger{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("request failed for " + req.URL.String())
	})}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://user-sentinel:pass-sentinel@127.0.0.1:1/path?KEY=key-sentinel&Api_Key=api-key-sentinel&Token=token-sentinel&ACCESS_TOKEN=access-sentinel&Signature=signature-sentinel&sig=sig-sentinel&Secret=secret-sentinel&Password=password-sentinel&public=visible-value&x=one%20two&x=three", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.URL.RawQuery += "&&bad=%zz"
	if resp, _ := logger.RoundTrip(req); resp != nil {
		_ = resp.Body.Close()
	}

	logged := logBuf.String()
	for _, secret := range []string{"user-sentinel", "pass-sentinel", "key-sentinel", "api-key-sentinel", "token-sentinel", "access-sentinel", "signature-sentinel", "sig-sentinel", "secret-sentinel", "password-sentinel"} {
		if strings.Contains(logged, secret) {
			t.Errorf("URL secret %q leaked in debug log:\n%s", secret, logged)
		}
	}
	for _, preserved := range []string{"public=visible-value", "x=one%20two&x=three&&bad=%zz"} {
		if !strings.Contains(logged, preserved) {
			t.Errorf("expected query semantics %q to remain visible:\n%s", preserved, logged)
		}
	}
	if !strings.Contains(logged, "error=\"request failed for http://%5BREDACTED%5D@127.0.0.1:1/path?") {
		t.Errorf("transport error did not contain redacted URL:\n%s", logged)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRedactRequestURL_PreservesQuerySemantics(t *testing.T) {
	raw, err := url.Parse("https://user:pass@example.com/p?x=one%20two&x=three&API_KEY=first&api_key=second&&bad=%zz&flag")
	if err != nil {
		t.Fatal(err)
	}
	got := redactRequestURL(raw)
	for _, want := range []string{"x=one%20two&x=three", "API_KEY=%5BREDACTED%5D&api_key=%5BREDACTED%5D", "&&bad=%zz&flag"} {
		if !strings.Contains(got, want) {
			t.Errorf("redacted URL %q missing preserved query fragment %q", got, want)
		}
	}
	if strings.Contains(got, "first") || strings.Contains(got, "second") || strings.Contains(got, "user:pass") {
		t.Errorf("redacted URL leaked credentials: %q", got)
	}
}

// TestRetryTransport_NoRetryForPost locks the idempotency guard: a POST that
// gets a 5xx hits the server exactly once with no backoff delay.
func TestRetryTransport_NoRetryForPost(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	client := NewHTTPClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected POST to hit server exactly once (no retry), got %d", got)
	}
	if elapsed > time.Second {
		t.Fatalf("non-retryable POST took %v; should return without backoff", elapsed)
	}
}
