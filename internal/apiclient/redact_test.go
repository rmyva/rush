package apiclient

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const distinctiveToken = "rush_deadbeef.DISTINCTIVE_TEST_TOKEN_0123456789"

func isolateClientEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(key, t.TempDir())
	}
}

func TestNoForbiddenInternalImports(t *testing.T) {
	isolateClientEnv(t)
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate package source")
	}
	entries, err := os.ReadDir(filepath.Dir(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(source), entry.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"/internal/server", "/internal/app", "/internal/db", "/internal/config"} {
				if strings.Contains(path, forbidden) {
					t.Errorf("%s imports forbidden path %q", entry.Name(), path)
				}
			}
		}
	}
}

type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (h *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h *logCapture) Handle(_ context.Context, record slog.Record) error {
	var line strings.Builder
	line.WriteString(record.Message)
	record.Attrs(func(attr slog.Attr) bool {
		line.WriteByte(' ')
		fmt.Fprint(&line, attr.Value.Any())
		return true
	})
	h.mu.Lock()
	h.lines = append(h.lines, line.String())
	h.mu.Unlock()
	return nil
}

func (h *logCapture) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.lines...)
}

func (h *logCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logCapture) WithGroup(string) slog.Handler      { return h }

func TestRedirectDoesNotForwardAuthorization(t *testing.T) {
	isolateClientEnv(t)
	targetRequests := make(chan string, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	client, _, err := New(Options{BaseURL: redirect.URL, Token: distinctiveToken})
	if err != nil {
		t.Fatal(err)
	}
	defer client.http.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, _, err = client.do(ctx, http.MethodGet, "/redirect", nil, nil, reqOpts{})
	if !errors.Is(err, ErrRedirect) {
		t.Fatal("redirect sentinel not preserved")
	}
	select {
	case <-targetRequests:
		t.Fatal("redirect target received a request")
	default:
	}
}

func TestCapturedWarningsAndErrorsDoNotContainDistinctiveToken(t *testing.T) {
	isolateClientEnv(t)
	capture := &logCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(previous) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"title":"failure","detail":"` + distinctiveToken + `"}`))
	}))
	defer server.Close()
	client, warnings, err := New(Options{BaseURL: server.URL, Token: distinctiveToken})
	if err != nil {
		t.Fatal(err)
	}
	defer client.http.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, _, requestErr := client.do(ctx, http.MethodGet, "/failure", nil, nil, reqOpts{})
	if requestErr == nil {
		t.Fatal("expected API failure")
	}
	// Emit the real returned failure through the configured process logger.
	slog.Error("API request failed", "error", requestErr)
	outputs := append([]string(nil), warnings...)
	outputs = append(outputs, requestErr.Error())
	outputs = append(outputs, capture.snapshot()...)
	for _, output := range outputs {
		if strings.Contains(output, distinctiveToken) {
			t.Fatal("captured warnings, errors, or logs contain an unredacted token")
		}
	}
}

func TestConfiguredTokenScrubbedFromFailureLocation(t *testing.T) {
	isolateClientEnv(t)
	failure := &requestFailure{method: "GET" + distinctiveToken, path: "/items/" + distinctiveToken, token: distinctiveToken, err: context.DeadlineExceeded}
	if strings.Contains(failure.Error(), distinctiveToken) {
		t.Fatal("requestFailure leaked configured token")
	}
	apiErr := &APIError{Status: 500, Title: "failure", method: "GET" + distinctiveToken, path: "/items/" + distinctiveToken, token: distinctiveToken}
	if strings.Contains(apiErr.Error(), distinctiveToken) {
		t.Fatal("APIError leaked configured token")
	}
}

func TestRawCustomTokenScrubbedFromProblem(t *testing.T) {
	isolateClientEnv(t)
	capture := &logCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(previous) })
	token := "custom-sensitive-token"
	body := []byte(`{"title":"failure","detail":"` + token + `"}`)
	apiErr := parseAPIError(http.StatusBadRequest, http.Header{"Content-Type": []string{"application/problem+json"}}, body, token, time.Now())
	if strings.Contains(apiErr.Detail, token) || strings.Contains(apiErr.Error(), token) || strings.Contains(string(apiErr.body), token) {
		t.Fatal("custom token leaked from parsed problem")
	}
	slog.Error("API problem", "error", apiErr)
	if len(capture.snapshot()) == 0 || strings.Contains(strings.Join(capture.snapshot(), " "), token) {
		t.Fatal("custom token leaked into structured log")
	}
}

func TestRequestShapeIsHex(t *testing.T) {
	isolateClientEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-Id")
		decoded, err := hex.DecodeString(requestID)
		if err != nil || len(decoded) != 8 {
			t.Error("request ID is not 16 hexadecimal characters")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, _, err := New(Options{BaseURL: server.URL, Token: distinctiveToken})
	if err != nil {
		t.Fatal(err)
	}
	defer client.http.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, _, err := client.do(ctx, http.MethodGet, "/shape", nil, nil, reqOpts{}); err != nil {
		t.Fatal("request shape failed")
	}
}
