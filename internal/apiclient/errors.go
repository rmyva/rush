package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrResponseTooLarge indicates that an API response exceeded the configured limit.
var ErrResponseTooLarge = errors.New("API response too large")

// APIError describes an unsuccessful API response. Body is bounded and scrubbed.
type APIError struct {
	Status                   int
	Title, Detail, RequestID string
	RetryAfter               time.Duration
	body                     []byte
	method, path, token      string
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	where := ""
	if e.method != "" {
		method := scrubSecrets(e.method, e.token)
		if safePresentation(method) {
			where = " " + method
		}
	}
	if e.path != "" {
		path := scrubSecrets(e.path, e.token)
		if safePresentation(path) && !strings.ContainsAny(path, "?#") {
			where += " " + path
		}
	}
	if e.Detail != "" {
		return fmt.Sprintf("API request%s failed (%d %s): %s", where, e.Status, e.Title, e.Detail)
	}
	return fmt.Sprintf("API request%s failed (%d %s)", where, e.Status, e.Title)
}

func ParseAPIError(status int, header http.Header, body []byte, token string) *APIError {
	return parseAPIError(status, header, body, token, time.Now())
}

func parseAPIError(status int, header http.Header, body []byte, token string, now time.Time) *APIError {
	title := http.StatusText(status)
	if title == "" {
		title = "API error"
	}
	var problem struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(header.Get("Content-Type"), ";")[0]))
	parsed := mediaType == "application/problem+json" && json.Unmarshal(body, &problem) == nil
	if parsed && strings.TrimSpace(problem.Title) != "" {
		title = problem.Title
	}
	title = cleanText(scrubSecrets(title, token))
	detail := ""
	if parsed {
		detail = cleanText(scrubSecrets(problem.Detail, token))
		if len([]rune(detail)) > 500 {
			detail = string([]rune(detail)[:500])
		}
	}
	debug := cleanText(scrubSecrets(string(body), token))
	if len(debug) > 1024 {
		debug = debug[:1024]
	}
	return &APIError{Status: status, Title: title, Detail: detail, RequestID: cleanText(scrubSecrets(header.Get("X-Request-Id"), token)), RetryAfter: parseRetryAfter(header.Get("Retry-After"), now), body: []byte(debug)}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value != "" {
		digits := true
		for _, r := range value {
			if r < '0' || r > '9' {
				digits = false
				break
			}
		}
		if digits {
			seconds, err := strconv.ParseUint(value, 10, 64)
			if err != nil || seconds > 3600 {
				return time.Hour
			}
			return time.Duration(seconds) * time.Second
		}
	}
	if t, err := http.ParseTime(value); err == nil {
		d := t.Sub(now)
		if d <= 0 {
			return 0
		}
		if d > time.Hour {
			return time.Hour
		}
		return d
	}
	return 0
}
func IsAuth(err error) bool        { return hasStatus(err, 401, 403) }
func IsNotFound(err error) bool    { return hasStatus(err, 404) }
func IsConflict(err error) bool    { return hasStatus(err, 409) }
func IsRateLimited(err error) bool { return hasStatus(err, 429) }
func IsUnavailable(err error) bool { return hasStatus(err, 502, 503, 504) }
func hasStatus(err error, statuses ...int) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	for _, s := range statuses {
		if apiErr.Status == s {
			return true
		}
	}
	return false
}

func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var n net.Error
	return errors.As(err, &n) && n.Timeout()
}
