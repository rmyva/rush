package log

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxRetries     = 3
	retryDelayBase = 2 * time.Second
	// maxBodyPreview bounds how many bytes of each request/response body
	// are captured for debug logging. It keeps memory predictable for
	// arbitrarily large payloads while still capturing the request echo,
	// the response status line/headers, and the first several SSE events
	// of an LLM stream — enough to diagnose auth, format, and transport
	// problems. 16 KiB is deliberately small; full bodies are never held.
	maxBodyPreview = 16 << 10 // 16 KiB
)

// LogHTTPBodies gates whether request/response BODIES are ever included in
// debug logs, as opposed to just method/url/status/content_length/duration.
//
// Request/response bodies for LLM traffic routinely contain the system
// prompt, the full user message history, tool output (which can include the
// contents of files the agent read — including secrets from a .env the
// agent was asked to inspect), and — for some providers — the API key
// itself repeated inside a JSON field, not just the Authorization header.
// formatHeaders (below) redacts known sensitive HEADERS, but historically
// nothing redacted the BODY, so simply turning on --debug was enough to
// spray all of that into rush.log at slog.Debug level.
//
// This is intentionally a SEPARATE opt-in from --debug/slog.LevelDebug:
// --debug is routinely turned on to diagnose transport/timing issues and
// must not, by itself, imply "log secrets". Body logging additionally
// requires RUSH_LOG_HTTP_BODIES=1 (following the existing RUSH_* opt-in
// env var convention used elsewhere, e.g. RUSH_DISABLE_ANTHROPIC_CACHE,
// RUSH_CORE_UTILS — no new configuration mechanism introduced here).
// Even then, known secret-shaped JSON fields are redacted before logging
// (see redactBodySecrets).
var LogHTTPBodies = func() bool {
	v, _ := strconv.ParseBool(os.Getenv("RUSH_LOG_HTTP_BODIES"))
	return v
}()

// NewHTTPClient creates an HTTP client with debug logging and retry on 5xx errors.
func NewHTTPClient() *http.Client {
	return NewHTTPClientWithTransport(http.DefaultTransport)
}

// NewHTTPClientWithTransport creates an HTTP client with debug logging and
// retry on 5xx errors, layering both on top of the given base transport
// instead of http.DefaultTransport. Callers with a custom proxy/DNS
// transport pass it here so logging and retry compose on top of it.
func NewHTTPClientWithTransport(base http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: &RetryTransport{
			Transport: &HTTPRoundTripLogger{
				Transport: base,
			},
		},
	}
}

// RetryTransport is an http.RoundTripper that retries idempotent requests
// (or any request carrying an Idempotency-Key) on 5xx errors.
type RetryTransport struct {
	Transport http.RoundTripper
}

// RoundTrip implements http.RoundTripper with retry logic for 5xx errors.
//
// Only idempotent methods (GET, HEAD, OPTIONS, PUT, DELETE) or requests
// carrying an Idempotency-Key/X-Idempotency-Key header are retried. Other
// methods — notably POST to LLM completion endpoints — are returned after
// the first response, since retrying a request the server may already be
// processing could double-charge or double-generate. This transport is only
// wired up in debug mode, but the guard is cheap and strictly safer.
func (r *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	retryable := isRetryable(req)

	// Only buffer the request body when we might actually retry, so that
	// non-retryable requests stream straight through to the inner transport.
	var bodyBytes []byte
	if retryable && req.Body != nil && req.Body != http.NoBody {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if err = req.Body.Close(); err != nil {
			return nil, err
		}
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		if attempt > 0 {
			delay := retryDelayBase * time.Duration(1<<(attempt-1)) // exponential backoff
			slog.Warn("Retrying HTTP request due to server error",
				"attempt", attempt,
				"delay", delay,
				"method", req.Method,
				"url", redactRequestURL(req.URL),
			)
			select {
			case <-time.After(delay):
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}

		resp, err := r.Transport.RoundTrip(req)
		if err != nil {
			return nil, err
		}

		// Non-retryable requests, non-5xx responses, and the final attempt
		// are returned immediately.
		if !retryable || resp.StatusCode < 500 || resp.StatusCode >= 600 || attempt >= maxRetries {
			return resp, nil
		}

		// Close the response body before retrying.
		resp.Body.Close()
	}

	return nil, http.ErrHandlerTimeout
}

// CloseIdleConnections forwards to the wrapped transport chain so a
// caller closing idle connections through the wrapper client actually
// reaches the real keep-alive pool: http.Client only consults its
// Transport field's own CloseIdleConnections method, so a wrapper
// without one silently misses the base transport's pool.
func (r *RetryTransport) CloseIdleConnections() {
	if owned, ok := r.Transport.(interface{ CloseIdleConnections() }); ok {
		owned.CloseIdleConnections()
	}
}

// isRetryable reports whether req is safe to repeat.
func isRetryable(req *http.Request) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	}
	return req.Header.Get("Idempotency-Key") != "" || req.Header.Get("X-Idempotency-Key") != ""
}

// HTTPRoundTripLogger is an http.RoundTripper that logs requests and responses.
type HTTPRoundTripLogger struct {
	Transport http.RoundTripper
}

func redactRequestURL(raw *url.URL) string {
	if raw == nil {
		return ""
	}
	redacted := *raw
	if redacted.User != nil {
		redacted.User = url.User("[REDACTED]")
	}
	redacted.RawQuery = redactURLQuery(redacted.RawQuery)
	return redacted.String()
}

// redactURLQuery redacts sensitive values without parsing and re-encoding the
// entire query. Keeping each raw pair intact preserves duplicate parameters,
// ordering, escaping, and malformed-but-forwarded query text in the logged URL.
func redactURLQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	parts := strings.Split(rawQuery, "&")
	for i, part := range parts {
		key, _, _ := strings.Cut(part, "=")
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			continue
		}
		switch strings.ToLower(decodedKey) {
		case "key", "api_key", "token", "access_token", "signature", "sig", "secret", "password":
			parts[i] = key + "=%5BREDACTED%5D"
		}
	}
	return strings.Join(parts, "&")
}

func redactRequestError(err error, raw *url.URL) string {
	if err == nil {
		return ""
	}
	if raw == nil {
		return err.Error()
	}
	return strings.ReplaceAll(err.Error(), raw.String(), redactRequestURL(raw))
}

// RoundTrip implements http.RoundTripper with streaming-safe debug logging.
//
// When debug logging is not enabled at the current slog level, request and
// response bodies are passed through untouched: streaming is fully preserved
// and there is zero buffering overhead.
//
// When debug logging IS enabled, method/url/status/content_length/duration
// are always logged, but request/response BODIES are only captured and
// logged when the separate LogHTTPBodies opt-in is also set — see its doc
// comment for why this is a distinct, more dangerous switch than --debug
// alone. When LogHTTPBodies is on, a bounded preview (maxBodyPreview bytes)
// of each body is captured — the request preview up front, the response
// preview lazily as the caller reads — and known secret-shaped JSON fields
// are redacted before logging (see redactBodySecrets). The response is
// logged once when its body is closed, never before RoundTrip returns. The
// response body returned to the caller is always the live stream, never a
// fully materialized buffer.
func (h *HTTPRoundTripLogger) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	debugEnabled := slog.Default().Enabled(ctx, slog.LevelDebug)
	logBodies := debugEnabled && LogHTTPBodies

	if logBodies && req.Body != nil && req.Body != http.NoBody {
		preview, err := io.ReadAll(io.LimitReader(req.Body, maxBodyPreview))
		if err != nil {
			// The preview read failed on req.Body itself — the same reader
			// the transport is about to send over the wire — so it is now
			// broken or partially exhausted. Silently reconstructing a
			// body from it and proceeding (the previous behavior) risks
			// sending a truncated/corrupted request, or failing later with
			// a more confusing symptom than the real cause. Fail fast here
			// instead, with the actual read error attached, matching what
			// RoundTrip would have to report anyway once the transport hit
			// the same broken reader.
			return nil, fmt.Errorf("HTTP request body preview read failed: %w", err)
		}
		// Rebuild the full request body so nothing is lost: the captured
		// preview followed by whatever the original stream still holds.
		req.Body = concatBody(preview, req.Body)
		truncated := ""
		if len(preview) >= maxBodyPreview {
			truncated = " …(truncated)"
		}
		slog.Debug("HTTP Request",
			"method", req.Method,
			"url", redactRequestURL(req.URL),
			"body", redactBodySecrets(prettyBody(preview))+truncated,
		)
	} else if debugEnabled {
		slog.Debug("HTTP Request", "method", req.Method, "url", redactRequestURL(req.URL))
	}

	start := time.Now()
	resp, err := h.Transport.RoundTrip(req)
	duration := time.Since(start)
	if err != nil {
		slog.Error("HTTP request failed",
			"method", req.Method,
			"url", redactRequestURL(req.URL),
			"duration_ms", duration.Milliseconds(),
			"error", redactRequestError(err, req.URL),
		)
		return resp, err
	}

	if !debugEnabled || resp.Body == nil || resp.Body == http.NoBody {
		// Fast path: return the live streaming body untouched.
		return resp, nil
	}

	statusCode, status := resp.StatusCode, resp.Status
	headers := formatHeaders(resp.Header)
	contentLength := resp.ContentLength

	if !logBodies {
		// Debug is on but body logging is not: log metadata only, without
		// ever reading/buffering the body ourselves. The caller's stream is
		// untouched.
		slog.Debug("HTTP Response",
			"status_code", statusCode,
			"status", status,
			"headers", headers,
			"content_length", contentLength,
			"duration_ms", duration.Milliseconds(),
		)
		return resp, nil
	}

	// Wrap the response body so the preview is captured as the caller
	// reads, and the debug log is emitted once on Close (after the stream
	// completes), not before RoundTrip returns.
	resp.Body = newTeeBody(resp.Body, maxBodyPreview, func(preview string) {
		slog.Debug("HTTP Response",
			"status_code", statusCode,
			"status", status,
			"headers", headers,
			"body", redactBodySecrets(prettyBody([]byte(preview))),
			"content_length", contentLength,
			"duration_ms", duration.Milliseconds(),
		)
	})
	return resp, nil
}

// CloseIdleConnections forwards to the wrapped transport; see
// RetryTransport.CloseIdleConnections for why the forwarding must be
// explicit.
func (h *HTTPRoundTripLogger) CloseIdleConnections() {
	if h.Transport == nil {
		return
	}
	if owned, ok := h.Transport.(interface{ CloseIdleConnections() }); ok {
		owned.CloseIdleConnections()
	}
}

// concatBody returns a ReadCloser that yields prefix then the remaining
// bytes of rest, closing rest when closed itself.
func concatBody(prefix []byte, rest io.ReadCloser) io.ReadCloser {
	return &multiReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), rest), Closer: rest}
}

type multiReadCloser struct {
	io.Reader
	io.Closer
}

// teeBody streams reads through to an underlying body while copying a
// bounded prefix into a preview buffer. It is safe for concurrent
// Read/Close (e.g. context cancellation racing a pending read) and invokes
// onClose at most once, with the captured preview, when the body is closed.
type teeBody struct {
	body    io.ReadCloser
	limit   int
	onClose func(preview string)

	mu     sync.Mutex
	closed bool
	once   sync.Once
	buf    bytes.Buffer
}

func newTeeBody(body io.ReadCloser, limit int, onClose func(string)) *teeBody {
	return &teeBody{body: body, limit: limit, onClose: onClose}
}

func (b *teeBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 {
		b.mu.Lock()
		if b.buf.Len() < b.limit {
			room := b.limit - b.buf.Len()
			if n < room {
				room = n
			}
			b.buf.Write(p[:room])
		}
		b.mu.Unlock()
	}
	return n, err
}

func (b *teeBody) Close() error {
	b.mu.Lock()
	already := b.closed
	b.closed = true
	preview := b.buf.String()
	b.mu.Unlock()
	if already {
		return nil
	}
	err := b.body.Close()
	b.once.Do(func() {
		if b.onClose != nil {
			b.onClose(preview)
		}
	})
	return err
}

// prettyBody returns a best-effort indented rendering of src for logging;
// non-JSON payloads (such as SSE streams) are returned verbatim.
func prettyBody(src []byte) string {
	trimmed := bytes.TrimSpace(src)
	var b bytes.Buffer
	if json.Indent(&b, trimmed, "", "  ") != nil {
		return string(src)
	}
	return b.String()
}

// sensitiveBodyKeys are JSON object keys (matched case-insensitively, and
// regardless of nesting depth) whose values are redacted by
// redactBodySecrets before a body is ever logged. This list is deliberately
// broader than formatHeaders' header list because provider request bodies
// echo credentials in more shapes than headers do — e.g. Vertex/Bedrock
// style bodies can carry a nested "credentials" or "apiKey" field, and
// config.ProviderConfig's own field names (APIKey, ExtraHeaders values,
// ExtraParams) can end up serialized into a request body for some
// providers. Matching by substring (like formatHeaders does) keeps this
// resilient to the "apiKey" / "api_key" / "X-Api-Key" naming variance seen
// across providers without needing an exact enumeration.
var sensitiveBodyKeySubstrings = []string{
	"apikey",
	"api_key",
	"authorization",
	"token",
	"secret",
	"password",
	"credential",
	"client_secret",
	"clientsecret",
	"private_key",
	"privatekey",
}

// tokenUsageKeySuffix distinguishes LLM token-COUNT fields — e.g.
// "max_tokens", "prompt_tokens", "total_tokens", "input_tokens",
// "cache_creation_input_tokens", "max_completion_tokens" — from
// credential-shaped "*token*" fields like "access_token"/"refresh_token"/
// "id_token"/"session_token". Every LLM provider request/response shape
// this codebase talks to (Anthropic, OpenAI, Gemini, Bedrock, Vertex) uses
// the PLURAL "tokens" exclusively for usage counts and the SINGULAR
// "token" exclusively for credentials — none was found using a plural
// "tokens" key to hold an actual secret value. This lets the bare "token"
// substring below stay broad enough to catch any "*_token"/"token_*"
// credential shape without an exact enumeration, while not redacting the
// exact usage-accounting fields RUSH_LOG_HTTP_BODIES is most often turned
// on to inspect (previously every one of the examples above was silently
// replaced with the string "[REDACTED]", corrupting both the value and its
// JSON type).
const tokenUsageKeySuffix = "tokens"

func isSensitiveBodyKey(key string) bool {
	lower := strings.ToLower(key)
	for _, sub := range sensitiveBodyKeySubstrings {
		if !strings.Contains(lower, sub) {
			continue
		}
		if sub == "token" && strings.HasSuffix(lower, tokenUsageKeySuffix) {
			// Plural "tokens" — a usage count, not a credential. See
			// tokenUsageKeySuffix's doc comment. Other substrings (secret,
			// password, credential, ...) still redact normally even when
			// this one doesn't fire.
			continue
		}
		return true
	}
	return false
}

// redactBodySecrets best-effort redacts known secret-shaped JSON fields
// (see sensitiveBodyKeySubstrings) from a request/response body before it
// is logged. It only activates when the body parses as JSON (an object,
// array, or a value containing either); non-JSON bodies — plain text,
// SSE streams, binary previews — are returned verbatim, since prettyBody
// already falls back to the raw bytes for those and there is no reliable
// generic way to redact secrets embedded in arbitrary non-JSON text
// without both false positives and false negatives. This mirrors
// prettyBody's own "best effort, JSON only" contract.
//
// SSE bodies (the common case for LLM streaming responses) are handled
// line-by-line: each "data: {...}" line's JSON payload is redacted
// independently, since the overall SSE body is not itself valid JSON.
func redactBodySecrets(body string) string {
	if body == "" {
		return body
	}

	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var v any
		if err := json.Unmarshal([]byte(body), &v); err == nil {
			redactJSONValue(v)
			var out bytes.Buffer
			enc := json.NewEncoder(&out)
			enc.SetIndent("", "  ")
			enc.SetEscapeHTML(false)
			if err := enc.Encode(v); err == nil {
				return strings.TrimRight(out.String(), "\n")
			}
		}
		return body
	}

	// SSE stream: redact each "data: {json}" line independently, leave
	// everything else (event:/id:/comments/blank lines) untouched.
	lines := strings.Split(body, "\n")
	changed := false
	for i, line := range lines {
		const prefix = "data: "
		rest, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(rest), &v); err != nil {
			continue
		}
		redactJSONValue(v)
		redacted, err := json.Marshal(v)
		if err != nil {
			continue
		}
		lines[i] = prefix + string(redacted)
		changed = true
	}
	if !changed {
		return body
	}
	return strings.Join(lines, "\n")
}

// redactJSONValue walks an arbitrary decoded JSON value in place, replacing
// the value of any object key matched by isSensitiveBodyKey with
// "[REDACTED]", at any nesting depth.
func redactJSONValue(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if isSensitiveBodyKey(k) {
				t[k] = "[REDACTED]"
				continue
			}
			redactJSONValue(val)
		}
	case []any:
		for _, item := range t {
			redactJSONValue(item)
		}
	}
}

// formatHeaders formats HTTP headers for logging, redacting sensitive ones.
func formatHeaders(headers http.Header) map[string][]string {
	filtered := make(map[string][]string)
	for key, values := range headers {
		lowerKey := strings.ToLower(key)
		if strings.Contains(lowerKey, "authorization") ||
			strings.Contains(lowerKey, "api-key") ||
			strings.Contains(lowerKey, "token") ||
			strings.Contains(lowerKey, "secret") {
			filtered[key] = []string{"[REDACTED]"}
		} else {
			filtered[key] = values
		}
	}
	return filtered
}
