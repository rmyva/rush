package apiclient

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	goldenSession    = `{"id":"session-fixed","title":"A fixed session","created_at":"2023-11-14T22:13:20Z","updated_at":"2023-11-14T22:15:23Z","message_count":7}`
	goldenMessage    = `{"id":"message-fixed","session_id":"session-fixed","role":"assistant","parts":[{"type":"text","text":"hello"},{"type":"tool_call","id":"call-1","name":"lookup","input":"{\"q\":\"x\"}"},{"type":"tool_result","name":"lookup","tool_call_id":"call-1","content":"found","is_error":true}],"created_at":"1970-01-01T00:00:01Z"}`
	goldenRun        = `{"id":"run-fixed","session_id":"session-fixed","state":"succeeded","created_at":"2023-11-14T22:13:20Z","started_at":"2023-11-14T22:13:21Z","finished_at":"2023-11-14T22:13:22Z","message_id":"message-fixed"}`
	goldenPermission = `{"id":"permission-fixed","session_id":"session-fixed","run_id":"run-fixed","tool":"shell","action":"execute","description":"list files","path":"/tmp","created_at":"2023-11-14T22:13:20Z"}`
)

func apiMethodsEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(name, t.TempDir())
	}
}

func apiMethodsClient(t *testing.T, handler http.Handler, options ...Options) (*Client, *httptest.Server) {
	t.Helper()
	var opts Options
	if len(options) > 0 {
		opts = options[0]
	}
	apiMethodsEnv(t)
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	opts.BaseURL = s.URL
	if opts.Token == "" {
		opts.Token = "test-token"
	}
	if opts.MaxResponseBytes == 0 {
		opts.MaxResponseBytes = 1 << 20
	}
	c, _, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.http.CloseIdleConnections)
	return c, s
}

func apiMethodsContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func assertAPIRequest(t *testing.T, r *http.Request, method, path, query, body string) {
	t.Helper()
	gotBody, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read request body: %v", err)
	}
	if r.Method != method || r.URL.EscapedPath() != "/api/v1"+path || r.URL.RawQuery != query {
		t.Errorf("request = %s %s?%s; want %s /api/v1%s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery, method, path, query)
	}
	if string(gotBody) != body {
		t.Errorf("body=%q want %q", gotBody, body)
	}
	if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("Accept") != "application/json, application/problem+json" || len(r.Header.Get("X-Request-Id")) != 16 {
		t.Errorf("headers=%v", r.Header)
	}
	if body != "" && r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type=%q", r.Header.Get("Content-Type"))
	}
	if body == "" && r.Header.Get("Content-Type") != "" {
		t.Errorf("unexpected Content-Type=%q", r.Header.Get("Content-Type"))
	}
}

func TestAPIHealthVersionModelsAndOpenAPI(t *testing.T) {
	calls := 0
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/api/v1/health":
			assertAPIRequest(t, r, "GET", "/health", "", "")
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		case "/api/v1/version":
			assertAPIRequest(t, r, "GET", "/version", "", "")
			_, _ = io.WriteString(w, `{"version":"1.2.3"}`)
		case "/api/v1/models":
			assertAPIRequest(t, r, "GET", "/models", "", "")
			_, _ = io.WriteString(w, `[{"id":"model-1","name":"Model","provider":"fixture"}]`)
		case "/api/v1/openapi.json":
			assertAPIRequest(t, r, "GET", "/openapi.json", "", "")
			_, _ = io.WriteString(w, `{"openapi":"3.1.0"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	ctx := apiMethodsContext(t)
	if err := c.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Version(ctx); err != nil || got != "1.2.3" {
		t.Fatalf("Version=%q,%v", got, err)
	}
	if got, err := c.Models(ctx); err != nil || !reflect.DeepEqual(got, []Model{{ID: "model-1", Name: "Model", Provider: "fixture"}}) {
		t.Fatalf("Models=%+v,%v", got, err)
	}
	if got, err := c.OpenAPI(ctx); err != nil || string(got) != `{"openapi":"3.1.0"}` {
		t.Fatalf("OpenAPI=%q,%v", got, err)
	}
	if calls != 4 {
		t.Fatalf("requests=%d", calls)
	}
}

func TestAPISessionMethodsAndPagination(t *testing.T) {
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/sessions":
			if r.Method == "GET" {
				assertAPIRequest(t, r, "GET", "/sessions", "after=cursor+1&limit=7", "")
				_, _ = io.WriteString(w, `{"items":[`+goldenSession+`],"next_after":"next"}`)
			} else {
				assertAPIRequest(t, r, "POST", "/sessions", "", `{"title":"A fixed session"}`)
				w.Header().Set("Location", "/api/v1/sessions/session-fixed")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, goldenSession)
			}
		case "/api/v1/sessions/session-fixed":
			if r.Method == "GET" {
				assertAPIRequest(t, r, "GET", "/sessions/session-fixed", "", "")
				_, _ = io.WriteString(w, goldenSession)
			} else {
				assertAPIRequest(t, r, "DELETE", "/sessions/session-fixed", "", "")
				w.WriteHeader(http.StatusNoContent)
			}
		case "/api/v1/sessions/session-fixed/fork":
			assertAPIRequest(t, r, "POST", "/sessions/session-fixed/fork", "", `{"title":"copy"}`)
			w.Header().Set("Location", "/api/v1/sessions/fork-fixed")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, strings.Replace(goldenSession, "session-fixed", "fork-fixed", 1))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	ctx := apiMethodsContext(t)
	page, err := c.ListSessions(ctx, SessionsOptions{After: "cursor 1", Limit: 7})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "session-fixed" || page.NextAfter == nil || *page.NextAfter != "next" {
		t.Fatalf("ListSessions=%+v,%v", page, err)
	}
	if got, err := c.GetSession(ctx, "session-fixed"); err != nil || got.Title != "A fixed session" {
		t.Fatalf("GetSession=%+v,%v", got, err)
	}
	if got, err := c.CreateSession(ctx, "A fixed session"); err != nil || got.MessageCount != 7 {
		t.Fatalf("CreateSession=%+v,%v", got, err)
	}
	if got, err := c.ForkSession(ctx, "session-fixed", "copy"); err != nil || got.ID != "fork-fixed" {
		t.Fatalf("ForkSession=%+v,%v", got, err)
	}
	if err := c.DeleteSession(ctx, "session-fixed"); err != nil {
		t.Fatal(err)
	}
}

func TestAPIMessageMethodsAndPagination(t *testing.T) {
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAPIRequest(t, r, "GET", "/sessions/session-fixed/messages", "after=message-0&limit=3", "")
		_, _ = io.WriteString(w, `{"items":[`+goldenMessage+`],"next_after":null}`)
	}))
	page, err := c.ListMessages(apiMethodsContext(t), "session-fixed", "message-0", 3)
	if err != nil || len(page.Items) != 1 || page.Items[0].Parts[0].Text != "hello" || page.NextAfter != nil {
		t.Fatalf("ListMessages=%+v,%v", page, err)
	}
}

func TestAPIRunMethodsAndInject(t *testing.T) {
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/sessions/session-fixed/runs":
			assertAPIRequest(t, r, "POST", "/sessions/session-fixed/runs", "", `{"prompt":"hello","role":"assistant","model":"m1"}`)
			if r.Header.Get("Idempotency-Key") != "run-key" {
				t.Errorf("key=%q", r.Header.Get("Idempotency-Key"))
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"run_id":"run-fixed","session_id":"session-fixed","state":"queued"}`)
		case "/api/v1/runs/run-fixed":
			assertAPIRequest(t, r, "GET", "/runs/run-fixed", "wait=25", "")
			_, _ = io.WriteString(w, goldenRun)
		case "/api/v1/runs/run-fixed/cancel":
			assertAPIRequest(t, r, "POST", "/runs/run-fixed/cancel", "", `{}`)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, goldenRun)
		case "/api/v1/sessions/session-fixed/inject":
			assertAPIRequest(t, r, "POST", "/sessions/session-fixed/inject", "", `{"message":"continue","mode":"interrupt"}`)
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"state":"accepted"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	ctx := apiMethodsContext(t)
	accepted, err := c.CreateRun(ctx, "session-fixed", RunRequest{Prompt: "hello", Role: "assistant", Model: "m1"}, "run-key")
	if err != nil || accepted.RunID != "run-fixed" || accepted.SessionID != "session-fixed" || accepted.State != "queued" {
		t.Fatalf("CreateRun=%+v,%v", accepted, err)
	}
	run, err := c.GetRun(ctx, "run-fixed", 25)
	if err != nil || run.State != "succeeded" {
		t.Fatalf("GetRun=%+v,%v", run, err)
	}
	if _, err := c.CancelRun(ctx, "run-fixed"); err != nil {
		t.Fatal(err)
	}
	if err := c.Inject(ctx, "session-fixed", "continue", true); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRunGeneratedIdempotencyKey(t *testing.T) {
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if !strings.HasPrefix(key, "rushapi-") || len(key) != len("rushapi-")+32 {
			t.Errorf("generated key=%q", key)
		}
		_, _ = io.WriteString(w, `{"run_id":"run-fixed","session_id":"session-fixed","state":"queued"}`)
	}))
	if _, err := c.CreateRun(apiMethodsContext(t), "session-fixed", RunRequest{Prompt: "hello"}, ""); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRunRetryKeepsIdempotencyKey(t *testing.T) {
	var calls atomic.Int32
	var keys []string
	var keysMu sync.Mutex
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		keysMu.Lock()
		keys = append(keys, key)
		keysMu.Unlock()
		if calls.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("server does not support hijacking")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = io.WriteString(w, `{"run_id":"run-fixed","session_id":"session-fixed","state":"queued"}`)
	}), Options{Sleeper: func(context.Context, time.Duration) error { return nil }})
	_, err := c.CreateRun(apiMethodsContext(t), "session-fixed", RunRequest{Prompt: "hello"}, "")
	if err != nil {
		t.Fatal(err)
	}
	keysMu.Lock()
	defer keysMu.Unlock()
	prefix := "rushapi-"
	if calls.Load() != 2 || len(keys) != 2 || !strings.HasPrefix(keys[0], prefix) || len(keys[0]) != len(prefix)+32 {
		t.Fatalf("calls=%d keys=%q", calls.Load(), keys)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(keys[0], prefix)); err != nil {
		t.Fatalf("generated key suffix is not hex: %q", keys[0])
	}
	if keys[0] != keys[1] {
		t.Fatalf("retry changed idempotency key: %q", keys)
	}
}

func TestWaitRunStateTableAndLongWaitLoop(t *testing.T) {
	for _, tc := range []struct {
		state   string
		wantErr string
	}{
		{"succeeded", ""}, {"failed", "run"}, {"cancelled", "run"}, {"awaiting_answer", "paused"}, {"running", ""},
	} {
		t.Run(tc.state, func(t *testing.T) {
			var n atomic.Int32
			c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertAPIRequest(t, r, "GET", "/runs/run-fixed", "wait=25", "")
				state := tc.state
				if tc.state == "running" && n.Add(1) == 1 {
					state = "running"
				} else if tc.state == "running" {
					state = "succeeded"
				}
				body := strings.Replace(goldenRun, `"state":"succeeded"`, `"state":"`+state+`"`, 1)
				if state == "failed" || state == "cancelled" {
					body = strings.Replace(body, `"message_id":"message-fixed"`, `"error":{"code":"worker_error","summary":"failed safely"}`, 1)
				}
				_, _ = io.WriteString(w, body)
			}))
			budget := time.Second
			run, err := c.WaitRun(apiMethodsContext(t), "run-fixed", budget)
			switch tc.wantErr {
			case "":
				if err != nil || run == nil || run.State != "succeeded" {
					t.Fatalf("WaitRun=%+v,%v", run, err)
				}
			case "run":
				var re *RunError
				if !errors.As(err, &re) || re.Run == nil || (tc.state == "failed" && re.Code != "worker_error") || (tc.state == "cancelled" && re.Code != "cancelled") {
					t.Fatalf("WaitRun=%+v,%v", run, err)
				}
			case "paused":
				if !errors.Is(err, ErrRunPaused) || run == nil || run.State != "awaiting_answer" {
					t.Fatalf("WaitRun=%+v,%v", run, err)
				}
			}
		})
	}
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request for zero timeout: %s %s", r.Method, r.URL.Path)
	}))
	_, err := c.WaitRun(apiMethodsContext(t), "run-fixed", 0)
	var timeoutErr *RunTimeoutError
	if !errors.As(err, &timeoutErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitRun with zero timeout error=%v", err)
	}
}

func TestPaginationHelpersBoundsAndRepeatedCursor(t *testing.T) {
	apiMethodsEnv(t)
	ctx := context.Background()
	for _, max := range []int{-1, 0, 1} {
		calls := 0
		_, err := collectPages(ctx, max, func(_ context.Context, after string, limit int) (*Page[string], error) {
			calls++
			if calls > 20 {
				return nil, errors.New("runaway pagination")
			}
			if limit < 1 || limit > 200 {
				t.Errorf("limit=%d", limit)
			}
			if max == 0 {
				return &Page[string]{Items: []string{"a"}}, nil
			}
			if after == "" {
				return &Page[string]{Items: []string{"a"}, NextAfter: stringPointer("c")}, nil
			}
			return &Page[string]{Items: []string{}, NextAfter: stringPointer("c")}, nil
		})
		if max < 0 && err == nil {
			t.Fatal("negative max accepted")
		}
		if max == 0 && err == nil {
			t.Fatal("max overflow accepted")
		}
		if max == 1 && (err == nil || err.Error() != "pagination cursor repeated") {
			t.Fatalf("repeated cursor: err = %v", err)
		}
		if max == 0 && calls != 1 {
			t.Fatalf("calls=%d", calls)
		}
	}
	if got := paginationQuery(ListOptions{After: "a b", Limit: 200}).Encode(); got != "after=a+b&limit=200" {
		t.Fatalf("query=%q", got)
	}
	if got := paginationQuery(ListOptions{After: "", Limit: 0}).Encode(); got != "" {
		t.Fatalf("empty query=%q", got)
	}
}

func stringPointer(s string) *string { return &s }

func TestAllSessionsBoundAndPages(t *testing.T) {
	var calls int
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			assertAPIRequest(t, r, "GET", "/sessions", "limit=4", "")
			_, _ = io.WriteString(w, `{"items":[{"id":"one"}],"next_after":"one"}`)
			return
		}
		assertAPIRequest(t, r, "GET", "/sessions", "after=one&limit=3", "")
		_, _ = io.WriteString(w, `{"items":[{"id":"two"}],"next_after":null}`)
	}))
	items, err := c.AllSessions(apiMethodsContext(t), 3)
	if err != nil || len(items) != 2 || items[1].ID != "two" || calls != 2 {
		t.Fatalf("AllSessions=%+v calls=%d err=%v", items, calls, err)
	}
}

func TestAllSessionsRepeatedCursor(t *testing.T) {
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"items":[],"next_after":"same"}`)
	}))
	_, err := c.AllSessions(apiMethodsContext(t), 10)
	if err == nil || err.Error() != "pagination cursor repeated" {
		t.Fatalf("AllSessions repeated cursor error=%v", err)
	}
}

func TestAllMessagesBoundedPages(t *testing.T) {
	var calls int
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			assertAPIRequest(t, r, "GET", "/sessions/session-fixed/messages", "limit=3", "")
			_, _ = io.WriteString(w, `{"items":[{"id":"first","session_id":"session-fixed","role":"user","parts":[],"created_at":"1970-01-01T00:00:01Z"}],"next_after":"first"}`)
			return
		}
		assertAPIRequest(t, r, "GET", "/sessions/session-fixed/messages", "after=first&limit=2", "")
		_, _ = io.WriteString(w, `{"items":[{"id":"second","session_id":"session-fixed","role":"user","parts":[],"created_at":"1970-01-01T00:00:02Z"}],"next_after":null}`)
	}))
	items, err := c.AllMessages(apiMethodsContext(t), "session-fixed", 2)
	if err != nil || len(items) != 2 || items[1].ID != "second" || calls != 2 {
		t.Fatalf("AllMessages=%+v calls=%d err=%v", items, calls, err)
	}
}

func TestPermissionMethods(t *testing.T) {
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/permissions":
			assertAPIRequest(t, r, "GET", "/permissions", "run_id=run-fixed&session_id=session-fixed&state=pending", "")
			_, _ = io.WriteString(w, `{"items":[`+goldenPermission+`]}`)
		case "/api/v1/permissions/permission-fixed":
			assertAPIRequest(t, r, "POST", "/permissions/permission-fixed", "", `{"decision":"allow"}`)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	items, err := c.ListPermissions(apiMethodsContext(t), PermissionFilter{SessionID: "session-fixed", RunID: "run-fixed", State: "pending"})
	if err != nil || len(items) != 1 || items[0].Tool != "shell" || items[0].CreatedAt.IsZero() {
		t.Fatalf("ListPermissions=%+v,%v", items, err)
	}
	if err := c.AnswerPermission(apiMethodsContext(t), "permission-fixed", true); err != nil {
		t.Fatal(err)
	}
}

func TestRawMethodAndPathValidation(t *testing.T) {
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAPIRequest(t, r, "PUT", "/raw/item", "", `{"enabled":true}`)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"accepted":true}`)
	}))
	body, status, err := c.Raw(apiMethodsContext(t), "PUT", "/raw/item", []byte(`{"enabled":true}`))
	if err != nil || status != http.StatusAccepted || string(body) != `{"accepted":true}` {
		t.Fatalf("Raw=%q,%d,%v", body, status, err)
	}
	for _, path := range []string{"raw", "/a/../b", "/x?secret=y", "//host/path"} {
		if _, _, err := c.Raw(apiMethodsContext(t), "GET", path, nil); err == nil {
			t.Errorf("Raw accepted %q", path)
		}
	}
}

func TestGoldenDTOContract(t *testing.T) {
	apiMethodsEnv(t)
	var session Session
	if err := json.Unmarshal([]byte(goldenSession), &session); err != nil || session.ID != "session-fixed" || session.MessageCount != 7 {
		t.Fatalf("session=%+v err=%v", session, err)
	}
	var message Message
	if err := json.Unmarshal([]byte(goldenMessage), &message); err != nil || len(message.Parts) != 3 || message.Parts[1].Type != "tool_call" || message.Parts[2].IsError != true {
		t.Fatalf("message=%+v err=%v", message, err)
	}
	var model Model
	if err := json.Unmarshal([]byte(`{"id":"m","name":"Model","provider":"p"}`), &model); err != nil || model.Provider != "p" {
		t.Fatalf("model=%+v err=%v", model, err)
	}
	var run Run
	if err := json.Unmarshal([]byte(goldenRun), &run); err != nil || run.State != "succeeded" || run.StartedAt == nil || run.FinishedAt == nil {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	var unknown Run
	if err := json.Unmarshal([]byte(`{"id":"u","session_id":"s","state":"future_state","created_at":"2023-11-14T22:13:20Z","extra":true}`), &unknown); err != nil || unknown.State != "future_state" {
		t.Fatalf("unknown=%+v err=%v", unknown, err)
	}
	var accepted RunAccepted
	if err := json.Unmarshal([]byte(`{"run_id":"run-fixed","session_id":"session-fixed","state":"queued"}`), &accepted); err != nil || accepted.RunID != "run-fixed" {
		t.Fatalf("accepted=%+v err=%v", accepted, err)
	}
	var permission Permission
	if err := json.Unmarshal([]byte(goldenPermission), &permission); err != nil || permission.ID != "permission-fixed" {
		t.Fatalf("permission=%+v err=%v", permission, err)
	}
	var page Page[Session]
	if err := json.Unmarshal([]byte(`{"items":[`+goldenSession+`],"next_after":"next"}`), &page); err != nil || len(page.Items) != 1 || *page.NextAfter != "next" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}

func TestErrorsAndCapturedOutputRedactDistinctiveToken(t *testing.T) {
	const token = "rush_deadbeef.DISTINCTIVE_TEST_TOKEN_0123456789"
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, `{"title":"failure","detail":%q}`, token)
	}), Options{Token: token})
	_, err := c.Version(apiMethodsContext(t))
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(fmt.Sprintf("%+v", err), token) {
		t.Fatalf("unsafe error: %v", err)
	}
	var captured strings.Builder
	_, _, responseErr := c.Raw(apiMethodsContext(t), "POST", "/hidden", []byte(`{"token":"`+token+`"}`))
	fmt.Fprintf(&captured, "%v", responseErr)
	if strings.Contains(captured.String(), token) {
		t.Fatalf("captured output leaked token: %s", captured.String())
	}
}
