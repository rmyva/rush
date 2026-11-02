package apiclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"
)

const problemNotFound = `{"type":"about:blank","title":"Not Found","status":404,"detail":"missing"}`

// everyMethod calls each typed method once and returns the error it produced.
func everyMethod(ctx context.Context, c *Client) map[string]error {
	out := map[string]error{}
	out["Health"] = c.Health(ctx)
	_, out["Version"] = c.Version(ctx)
	_, out["Models"] = c.Models(ctx)
	_, out["OpenAPI"] = c.OpenAPI(ctx)
	_, out["ListPermissions"] = c.ListPermissions(ctx, PermissionFilter{})
	out["AnswerPermission"] = c.AnswerPermission(ctx, "p", true)
	_, out["ListSessions"] = c.ListSessions(ctx, SessionsOptions{})
	_, out["GetSession"] = c.GetSession(ctx, "s")
	_, out["CreateSession"] = c.CreateSession(ctx, "t")
	_, out["ForkSession"] = c.ForkSession(ctx, "s", "t")
	out["DeleteSession"] = c.DeleteSession(ctx, "s")
	_, out["ListMessages"] = c.ListMessages(ctx, "s", "", 0)
	_, out["CreateRun"] = c.CreateRun(ctx, "s", RunRequest{Prompt: "p"}, "")
	_, out["GetRun"] = c.GetRun(ctx, "r", 0)
	_, out["CancelRun"] = c.CancelRun(ctx, "r")
	out["Inject"] = c.Inject(ctx, "s", "m", false)
	return out
}

func TestEveryMethodReturnsTypedAPIErrorOnProblemResponse(t *testing.T) {
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(problemNotFound))
	}))
	for name, err := range everyMethod(apiMethodsContext(t), c) {
		if err == nil || !IsNotFound(err) {
			t.Errorf("%s: err = %v, want a 404 API error", name, err)
		}
	}
}

func TestMethodsRejectMalformedSuccessBodies(t *testing.T) {
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{not json`))
	}))
	errs := everyMethod(apiMethodsContext(t), c)
	for _, name := range []string{"Models", "ListPermissions", "ListSessions", "GetSession", "CreateSession", "ForkSession", "ListMessages", "GetRun", "CancelRun", "Version"} {
		if errs[name] == nil {
			t.Errorf("%s accepted a malformed JSON body", name)
		}
	}
}

func TestMethodsHonourCancelledContext(t *testing.T) {
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, err := range everyMethod(ctx, c) {
		if err == nil {
			t.Errorf("%s ignored a cancelled context", name)
		}
	}
}

func TestQueryValuesAcceptedShapes(t *testing.T) {
	type typed struct {
		After string `json:"after"`
	}
	for name, tc := range map[string]struct {
		in      any
		want    string
		wantErr bool
	}{
		"nil":        {in: nil, want: ""},
		"url.Values": {in: url.Values{"a": {"1"}}, want: "a=1"},
		"map":        {in: map[string]string{"b": "2"}, want: "b=2"},
		"unsupported": {
			in: struct{ F func() }{}, wantErr: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			values, err := queryValues(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && values.Encode() != tc.want {
				t.Errorf("encoded = %q, want %q", values.Encode(), tc.want)
			}
		})
	}
	// A struct of strings is not an object of string arrays: it must be refused,
	// never silently dropped.
	if _, err := queryValues(typed{After: "x"}); err == nil {
		t.Error("a struct value was accepted as a query")
	}
}

func TestClientErrorStringsAreStableAndUnwrap(t *testing.T) {
	root := errors.New("root")
	for name, err := range map[string]error{
		"RunTimeoutError": &RunTimeoutError{Err: root},
		"RunPausedError":  &RunPausedError{Run: &Run{ID: "r"}},
	} {
		if err.Error() == "" {
			t.Errorf("%s has an empty message", name)
		}
	}
	if !errors.Is(&RunTimeoutError{Err: root}, root) {
		t.Error("RunTimeoutError does not unwrap")
	}
	if !errors.Is(&RunPausedError{Run: &Run{}}, ErrRunPaused) {
		t.Error("RunPausedError does not unwrap to ErrRunPaused")
	}
	if got := ExitCodeFor(&RunTimeoutError{Err: root}); got == 0 {
		t.Errorf("a run timeout maps to exit code %d", got)
	}
}

func TestWaitRunUsesOverallTimeout(t *testing.T) {
	c, _ := apiMethodsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","session_id":"s","state":"running","created_at":"2023-11-14T22:13:20Z"}`))
	}))
	_, err := c.WaitRun(apiMethodsContext(t), "r", time.Nanosecond)
	var timeout *RunTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("err = %v, want RunTimeoutError", err)
	}
}
