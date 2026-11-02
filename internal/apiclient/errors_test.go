package apiclient

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseAPIErrorScrubsAndBounds(t *testing.T) {
	isolateClientEnv(t)
	token := "rush_1234abcd.ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi0123456789"
	shaped := "rush_abcdef12.ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijk0123456"
	h := make(http.Header)
	h.Set("Content-Type", "application/problem+json")
	h.Set("X-Request-Id", "req-"+token+"\x01-"+shaped)
	h.Set("Retry-After", "7200")
	body := []byte(`{"title":"Oops","detail":"secret ` + token + ` ` + shaped + `\u0001"}`)
	err := ParseAPIError(429, h, body, token)
	if err.Title != "Oops" || strings.Contains(err.RequestID, token) || strings.Contains(err.RequestID, shaped) || strings.ContainsRune(err.RequestID, '\x01') || err.RetryAfter != time.Hour {
		t.Fatalf("unexpected parsed error: %#v", err)
	}
	if strings.Contains(err.Detail, token) || strings.Contains(err.Detail, shaped) || strings.ContainsRune(err.Detail, '\x01') {
		t.Fatalf("detail not scrubbed: %q", err.Detail)
	}
	if len([]rune(err.Detail)) > 500 || len(err.body) > 1024 {
		t.Fatalf("limits exceeded")
	}
}

func TestParseAPIErrorLimitsAndInvalidJSON(t *testing.T) {
	isolateClientEnv(t)
	h := make(http.Header)
	h.Set("Content-Type", "application/problem+json")
	longDetail := strings.Repeat("x", 600)
	err := ParseAPIError(400, h, []byte(`{"detail":"`+longDetail+`"}`), "")
	if len([]rune(err.Detail)) != 500 {
		t.Fatalf("detail length = %d, want 500", len([]rune(err.Detail)))
	}
	invalid := ParseAPIError(400, h, []byte(`{"detail":"partial",`), "")
	if invalid.Detail != "" || invalid.Title != "Bad Request" {
		t.Fatalf("invalid JSON leaked fields: %#v", invalid)
	}
	largeBody := []byte(strings.Repeat("b", 2048))
	bounded := ParseAPIError(400, make(http.Header), largeBody, "")
	if len(bounded.body) != 1024 {
		t.Fatalf("debug body length = %d, want 1024", len(bounded.body))
	}
}

func TestParseAPIErrorUsesSuppliedClock(t *testing.T) {
	isolateClientEnv(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	h := make(http.Header)
	h.Set("Retry-After", now.Add(5*time.Minute).Format(http.TimeFormat))
	if got := parseAPIError(429, h, nil, "", now).RetryAfter; got != 5*time.Minute {
		t.Fatalf("RetryAfter = %v, want 5m", got)
	}
}

func TestParseAPIErrorFallback(t *testing.T) {
	isolateClientEnv(t)
	err := ParseAPIError(418, make(http.Header), []byte("not json"), "")
	if err.Title != "I'm a teapot" || err.Detail != "" {
		t.Fatalf("unexpected fallback: %#v", err)
	}
}

func TestAPIErrorStatusHelpers(t *testing.T) {
	isolateClientEnv(t)
	for _, tc := range []struct {
		status                                        int
		auth, missing, conflict, limited, unavailable bool
	}{
		{401, true, false, false, false, false},
		{403, true, false, false, false, false},
		{404, false, true, false, false, false},
		{409, false, false, true, false, false},
		{429, false, false, false, true, false},
		{502, false, false, false, false, true},
		{503, false, false, false, false, true},
		{504, false, false, false, false, true},
	} {
		err := &APIError{Status: tc.status}
		if IsAuth(err) != tc.auth || IsNotFound(err) != tc.missing || IsConflict(err) != tc.conflict || IsRateLimited(err) != tc.limited || IsUnavailable(err) != tc.unavailable {
			t.Errorf("status %d helper mismatch", tc.status)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	isolateClientEnv(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{{"12", 12 * time.Second}, {"7200", time.Hour}, {"999999999999999999999999999999999999999", time.Hour}, {"1.5", 0}, {"NaN", 0}, {"bad", 0}, {now.Add(5 * time.Minute).Format(http.TimeFormat), 5 * time.Minute}} {
		if got := parseRetryAfter(tc.value, now); got != tc.want {
			t.Errorf("%q: got %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestExitCodeFor(t *testing.T) {
	isolateClientEnv(t)
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"usage", &UsageError{Err: errors.New("bad config")}, 2},
		{"auth", &APIError{Status: 401}, 3},
		{"not found", &APIError{Status: 404}, 4},
		{"timeout", context.DeadlineExceeded, 5},
		{"paused", ErrRunPaused, 6},
		{"interrupted", errors.Join(context.Canceled, ErrInterrupted), 130},
		{"network", errors.New("network"), 1},
		{"server", &APIError{Status: 503}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCodeFor(tc.err); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}

func TestIsTimeout(t *testing.T) {
	isolateClientEnv(t)
	if !IsTimeout(context.DeadlineExceeded) || IsTimeout(errors.New("ordinary")) {
		t.Fatal("timeout classification mismatch")
	}
}
