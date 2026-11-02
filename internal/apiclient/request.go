package apiclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxRequestIDBytes = 8

type reqOpts struct {
	IdempotencyKey string
}

type requestFailure struct {
	method string
	path   string
	token  string
	err    error
}

func (e *requestFailure) Error() string {
	method := scrubSecrets(e.method, e.token)
	if !safePresentation(method) {
		method = "[redacted]"
	}
	path := scrubSecrets(e.path, e.token)
	if !safePresentation(path) || strings.ContainsAny(path, "?#") {
		path = "[redacted]"
	}
	return fmt.Sprintf("API request %s %s failed", method, path)
}
func (e *requestFailure) Unwrap() error { return e.err }

type safeCause struct{ err error }

func (e *safeCause) Error() string { return "request body encoding failed" }
func (e *safeCause) Unwrap() error { return e.err }

func safePresentation(value string) bool {
	return !strings.ContainsAny(value, "\r\n")
}

// do sends one API request, applying the method-specific bounded retry policy.
func (c *Client) do(ctx context.Context, method, path string, query, body any, opts reqOpts) (status int, header http.Header, responseBody []byte, err error) {
	if err = validateRequestPath(path); err != nil {
		return 0, nil, nil, err
	}
	method = strings.ToUpper(method)
	if !safePresentation(method) || strings.ContainsAny(method, " ?#") {
		return 0, nil, nil, errors.New("invalid API method")
	}
	if opts.IdempotencyKey != "" && !validIdempotencyKey(opts.IdempotencyKey) {
		return 0, nil, nil, errors.New("invalid Idempotency-Key")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("encode API request body: %w", &safeCause{err: err})
	}
	requestID, err := c.newRequestID()
	if err != nil {
		return 0, nil, nil, errors.New("generate API request id")
	}
	endpoint := *c.baseURL
	decodedPath, _ := url.PathUnescape(path)
	basePath := strings.TrimRight(endpoint.Path, "/") + "/api/v1"
	endpoint.Path = basePath + decodedPath
	endpoint.RawPath = (&url.URL{Path: basePath}).EscapedPath() + path
	values, err := queryValues(query)
	if err != nil {
		return 0, nil, nil, errors.New("invalid API query")
	}
	baseValues, err := url.ParseQuery(endpoint.RawQuery)
	if err != nil {
		return 0, nil, nil, errors.New("invalid API base query")
	}
	for key, entries := range values {
		for _, value := range entries {
			baseValues.Add(key, value)
		}
	}
	endpoint.RawQuery = baseValues.Encode()
	maxAttempts := 1
	if method == http.MethodGet || method == http.MethodHead {
		maxAttempts = 3
	}
	if (method == http.MethodPost || method == http.MethodPut || method == http.MethodDelete) && opts.IdempotencyKey != "" {
		maxAttempts = 2
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, nil, nil, &requestFailure{method: method, path: path, token: c.options.Token, err: err}
		}
		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(payload)
		}
		req, reqErr := http.NewRequestWithContext(ctx, method, endpoint.String(), bodyReader)
		if reqErr != nil {
			return 0, nil, nil, errors.New("invalid API request")
		}
		req.Header.Set("Authorization", "Bearer "+c.options.Token)
		req.Header.Set("Accept", "application/json, application/problem+json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.options.UserAgent != "" {
			req.Header.Set("User-Agent", c.options.UserAgent)
		}
		req.Header.Set("X-Request-Id", requestID)
		if opts.IdempotencyKey != "" {
			req.Header.Set("Idempotency-Key", opts.IdempotencyKey)
		}
		resp, doErr := c.http.Do(req)
		if doErr != nil {
			cause := safeRequestCause(doErr)
			wrapped := &requestFailure{method: method, path: path, token: c.options.Token, err: cause}
			if attempt+1 < maxAttempts && retryableNetworkError(doErr) && (method == http.MethodGet || method == http.MethodHead || resp == nil) {
				if waitErr := c.waitRetry(ctx, attempt, 0); waitErr != nil {
					return 0, nil, nil, &requestFailure{method: method, path: path, token: c.options.Token, err: waitErr}
				}
				continue
			}
			return 0, nil, nil, wrapped
		}
		status, header = resp.StatusCode, resp.Header.Clone()
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, c.options.MaxResponseBytes+1))
		closeErr := resp.Body.Close()
		if readErr != nil {
			return status, header, nil, &requestFailure{method: method, path: path, token: c.options.Token, err: readErr}
		}
		if int64(len(data)) > c.options.MaxResponseBytes {
			return status, header, nil, ErrResponseTooLarge
		}
		if closeErr != nil {
			return status, header, data, &requestFailure{method: method, path: path, token: c.options.Token, err: closeErr}
		}
		responseBody = data
		if status >= 200 && status < 300 {
			return status, header, responseBody, nil
		}
		apiErr := parseAPIError(status, header, responseBody, c.options.Token, c.options.Now())
		apiErr.method, apiErr.path, apiErr.token = scrubSecrets(method, c.options.Token), scrubSecrets(path, c.options.Token), c.options.Token
		if attempt+1 < maxAttempts && retryableStatus(status, method) {
			if waitErr := c.waitRetry(ctx, attempt, apiErr.RetryAfter); waitErr != nil {
				return status, header, responseBody, &requestFailure{method: method, path: path, token: c.options.Token, err: waitErr}
			}
			continue
		}
		return status, header, responseBody, apiErr
	}
	return status, header, responseBody, err
}

func (c *Client) newRequestID() (string, error) {
	var raw [maxRequestIDBytes]byte
	if _, err := io.ReadFull(c.options.Rand, raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func validateRequestPath(path string) error {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#\r\n") || strings.Contains(path, "..") {
		return errors.New("invalid API path")
	}
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid API path")
	}
	decoded, err := url.PathUnescape(path)
	if err != nil || strings.Contains(decoded, "..") || strings.ContainsAny(decoded, "\r\n") {
		return errors.New("invalid API path")
	}
	return nil
}

func validIdempotencyKey(key string) bool {
	if len(key) < 1 || len(key) > 128 {
		return false
	}
	for _, r := range key {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r)) {
			return false
		}
	}
	return true
}

func queryValues(query any) (url.Values, error) {
	if query == nil {
		return make(url.Values), nil
	}
	switch q := query.(type) {
	case url.Values:
		return q, nil
	case map[string]string:
		values := make(url.Values, len(q))
		for k, v := range q {
			values.Set(k, v)
		}
		return values, nil
	default:
		encoded, err := json.Marshal(query)
		if err != nil {
			return nil, err
		}
		var values map[string][]string
		if err := json.Unmarshal(encoded, &values); err != nil {
			return nil, err
		}
		return url.Values(values), nil
	}
}

func retryableStatus(status int, method string) bool {
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	return status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func retryDelay(attempt int, random io.Reader) time.Duration {
	base := 200 * time.Millisecond * time.Duration(1<<attempt)
	var b [1]byte
	if _, err := io.ReadFull(random, b[:]); err != nil {
		_, _ = rand.Read(b[:])
	}
	return base + time.Duration(uint64(base)*uint64(b[0])*25/(255*100))
}

func (c *Client) waitRetry(ctx context.Context, attempt int, retryAfter time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delay := retryAfter
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	if delay <= 0 {
		delay = retryDelay(attempt, c.options.Rand)
	}
	if err := c.options.Sleeper(ctx, delay); err != nil {
		return err
	}
	return ctx.Err()
}

func retryableNetworkError(err error) bool {
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) || errors.Is(err, ErrRedirect) || errors.Is(err, ErrTLSVerification) {
		return false
	}
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

func safeRequestCause(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}
