package apiclient

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestNewValidatesAndNormalizesBaseURL(t *testing.T) {
	isolateClientEnv(t)
	tests := []struct {
		name          string
		url           string
		allowClear    bool
		wantURL       string
		wantWarnings  int
		wantErrorPart string
	}{
		{name: "https root", url: "https://example.test", wantURL: "https://example.test/"},
		{name: "path prefix", url: "https://example.test/api/v1///", wantURL: "https://example.test/api/v1"},
		{name: "loopback cleartext", url: "http://127.3.4.5:8000/prefix/", wantURL: "http://127.3.4.5:8000/prefix"},
		{name: "ipv6 loopback", url: "http://[::1]/", wantURL: "http://[::1]/"},
		{name: "localhost", url: "http://LOCALHOST:8080", wantURL: "http://LOCALHOST:8080/"},
		{name: "remote cleartext refused", url: "http://example.test", wantErrorPart: "cleartext HTTP"},
		{name: "remote cleartext allowed warns", url: "http://example.test", allowClear: true, wantURL: "http://example.test/", wantWarnings: 1},
		{name: "scheme refused", url: "ftp://example.test", wantErrorPart: "https or http"},
		{name: "host required", url: "https:///path", wantErrorPart: "host"},
		{name: "userinfo refused", url: "https://user:pass@example.test", wantErrorPart: "userinfo"},
		{name: "token query refused", url: "https://example.test/?Token=x", wantErrorPart: "token query"},
		{name: "access token query refused", url: "https://example.test/?access_token=x", wantErrorPart: "token query"},
		{name: "ordinary query retained", url: "https://example.test/api?limit=10", wantURL: "https://example.test/api?limit=10"},
		{name: "fragment refused", url: "https://example.test/#fragment", wantErrorPart: "fragment"},
		{name: "empty fragment refused", url: "https://example.test/#", wantErrorPart: "fragment"},
		{name: "malformed query refused", url: "https://example.test/?x=%zz", wantErrorPart: "query"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, warnings, err := New(Options{BaseURL: tt.url, Token: "secret", AllowCleartext: tt.allowClear})
			if tt.wantErrorPart != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrorPart) {
					t.Fatalf("New() error = %v, want substring %q", err, tt.wantErrorPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if got := client.baseURL.String(); got != tt.wantURL {
				t.Errorf("normalized URL = %q, want %q", got, tt.wantURL)
			}
			if len(warnings) != tt.wantWarnings {
				t.Errorf("warnings = %q, want %d", warnings, tt.wantWarnings)
			}
		})
	}
}

func TestNewRequiresToken(t *testing.T) {
	isolateClientEnv(t)
	if _, _, err := New(Options{BaseURL: "https://example.test"}); err == nil {
		t.Fatal("New() accepted an empty token")
	}
}

func TestNewInsecureSkipVerifyIsLoopbackOnly(t *testing.T) {
	isolateClientEnv(t)
	for _, tc := range []struct {
		url          string
		wantWarnings int
		wantError    bool
	}{
		{url: "https://localhost", wantWarnings: 1},
		{url: "http://127.0.0.1", wantWarnings: 1},
		{url: "https://example.test", wantError: true},
	} {
		_, warnings, err := New(Options{BaseURL: tc.url, Token: "secret", InsecureSkipVerify: true})
		if (err != nil) != tc.wantError {
			t.Errorf("New(%q) error = %v, wantError %v", tc.url, err, tc.wantError)
		}
		if err == nil && len(warnings) != tc.wantWarnings {
			t.Errorf("New(%q) warnings = %q, want %d", tc.url, warnings, tc.wantWarnings)
		}
	}
}

func TestNewAppliesDefaultsAndPreservesOverrides(t *testing.T) {
	isolateClientEnv(t)
	client, _, err := New(Options{BaseURL: "https://example.test", Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	got := client.options
	if got.Timeouts != (Timeouts{Dial: 10 * time.Second, TLSHandshake: 10 * time.Second, ResponseHeader: 30 * time.Second, Idle: 90 * time.Second}) {
		t.Errorf("timeouts = %+v", got.Timeouts)
	}
	if got.MaxResponseBytes != defaultMaxResponseBytes {
		t.Errorf("max response bytes = %d, want %d", got.MaxResponseBytes, defaultMaxResponseBytes)
	}
	if got.Sleeper == nil || got.Rand == nil || got.Now == nil {
		t.Error("default seams are not initialized")
	}
	customSleeper := func(context.Context, time.Duration) error { return nil }
	now := time.Unix(123, 0)
	custom, _, err := New(Options{
		BaseURL: "https://example.test", Token: "secret",
		Timeouts: Timeouts{Dial: time.Second}, MaxResponseBytes: 1024,
		Sleeper: customSleeper, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if custom.options.Timeouts.Dial != time.Second || custom.options.Timeouts.TLSHandshake != 10*time.Second || custom.options.MaxResponseBytes != 1024 {
		t.Errorf("custom options were not preserved/defaulted: %+v", custom.options)
	}
	if custom.options.Sleeper == nil || custom.options.Now() != now {
		t.Error("custom seams were not preserved")
	}
}

func TestNewRejectsOutOfRangeResponseLimit(t *testing.T) {
	isolateClientEnv(t)
	for _, limit := range []int64{-1, maxResponseBytesLimit + 1, math.MaxInt64} {
		_, _, err := New(Options{BaseURL: "https://example.test", Token: "secret", MaxResponseBytes: limit})
		var usage *UsageError
		if !errors.As(err, &usage) {
			t.Errorf("limit %d: err = %v, want UsageError", limit, err)
		}
	}
	if _, _, err := New(Options{BaseURL: "https://example.test", Token: "secret", MaxResponseBytes: maxResponseBytesLimit}); err != nil {
		t.Errorf("limit at the cap rejected: %v", err)
	}
}

func TestDefaultSleeperHonorsContext(t *testing.T) {
	isolateClientEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := defaultSleeper(ctx, time.Hour); err != context.Canceled {
		t.Errorf("defaultSleeper() error = %v, want context.Canceled", err)
	}
}
