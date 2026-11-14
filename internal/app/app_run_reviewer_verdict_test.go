// Design step 5 tests (docs/plans/2026-10-02-reviewer-pass-verification.md
// §5, §9): T10 (the verdict line is parsed, and a pass the reviewer never
// checked for itself is downgraded) and T13 (a review turn that fails for a
// reason of its own leaves the executor's answer as the run's outcome).
//
// They reuse the end-to-end harness of app_run_reviewer_pass_test.go — a REAL
// App whose provider is a stub openai-compat server keyed by model id, with
// the coordinator, message persistence and the two-phase event loop all
// running for real — so the downgrade is observed against the real
// transcript the review turn actually wrote.

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// T10a: `parseReviewVerdict` is pure, so the whole table runs without a
// database, a provider or a repository. Every case asserts the exact wire
// value RunResult.ReviewVerdict carries, including the empty string for a
// review that never answered the question.
//
// One row deliberately documents, rather than intends, the leading-space
// case: "   VERDICT: PASS" parses as "pass" because \W* swallows whitespace
// prefixes. Since the 2026-11-13 fix-audit the keyword's case is ignored
// (format noise), and a review with no verdict line at all is still ""
// (fail-closed).
func TestParseReviewVerdict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "plain pass",
			text: "VERDICT: PASS\n\n## Request vs result\nall done\n",
			want: "pass",
		},
		{
			// The alternation order is load-bearing: PASS_WITH_NOTES must
			// come first, or PASS swallows the prefix and a
			// "PASS_WITH_NOTES" review is recorded as a bare "pass".
			name: "pass with notes is not pass",
			text: "VERDICT: PASS_WITH_NOTES\n\n## Findings\nP2: stale comment\n",
			want: "pass_with_notes",
		},
		{
			// ^\W* tolerates a markdown marker in front of the keyword.
			name: "bold fail",
			text: "**VERDICT: FAIL**",
			want: "fail",
		},
		{
			name: "no verdict line at all",
			text: "no verdict line at all",
			want: "",
		},
		{
			// See the doc comment: leading whitespace IS \W*, so the
			// anchor still matches and the verdict is still read.
			name: "leading spaces are consumed by \\W*",
			text: "   VERDICT: PASS",
			want: "pass",
		},
		{
			// (?m) lets the verdict sit anywhere in the report, not only
			// on line 1 — the prompt demands line 1, the parser does not
			// require it.
			name: "second line of a multi-line report",
			text: "# Review of the run\nVERDICT: PASS_WITH_NOTES\n\n## Request vs result\n",
			want: "pass_with_notes",
		},
		{
			// The FIRST match wins: a report that repeats the token in its
			// body (quoting the prompt's rules, say) cannot change what the
			// run recorded.
			name: "first match wins",
			text: "VERDICT: FAIL\n\n(not \"VERDICT: PASS\", which the prompt forbids here)\n",
			want: "fail",
		},
		{
			// The keyword's case is format noise (2026-11-13 fix-audit):
			// "verdict: pass" is a pass. Fail-closed is untouched — a review
			// with no verdict line at all is still "".
			name: "lowercased verdict still parses",
			text: "verdict: pass\n\n## Findings\n",
			want: "pass",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, parseReviewVerdict(tt.text))
		})
	}

	// CONTROL: the verdict patterns agree on what they accept, so the table
	// above is not silently passing on a pattern that matches everything.
	require.Equal(t, "pass", parseReviewVerdict("VERDICT: PASS"))
	require.Equal(t, "pass_with_notes", parseReviewVerdict("VERDICT: PASS_WITH_NOTES"))
	require.Equal(t, "fail", parseReviewVerdict("VERDICT: FAIL"))
	require.Equal(t, "", parseReviewVerdict("VERDICT: UNKNOWN"))
}

// T10b: a PASS the reviewer never checked for itself is downgraded to
// "unverified". This is the exact failure mode the design exists for (#1165):
// a reviewer that only reads the orchestrator's claims and retells them is
// not an independent check, so the run must not record its word as a pass.
//
// Revert-check: removing the `reviewerToolCalls == 0` downgrade from
// attachReview turns the "unverified" assertion below back into "pass".
func TestAttachReview_PassWithoutToolsIsUnverified(t *testing.T) {
	h := newReviewerPassApp(t, true)
	sess := createModelOverrideSession(t, h.app, "reviewer-pass-unverified")

	// The real two-phase run: the stub reviewer answers with plain text and
	// NO tool call, which is exactly what the transcript has to show.
	result, err := runReviewerPassExecuteRun(t, h, sess.ID, RunOverrides{ModelRole: config.SelectedModelTypeSmart})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, []string{"smart-default", reviewerPassReviewerModel}, h.requestedModels(),
		"the clean smart run must be followed by exactly one review turn")
	require.Equal(t, reviewerPassPrimaryText, result.FinalText,
		"A10: the reviewer pass never replaces the executor's own answer")

	// The review turn produced a verdict line and not one read tool, so
	// folding it in must downgrade the pass.
	var stderr bytes.Buffer
	h.app.attachReview(context.Background(), result, sess.ID,
		"VERDICT: PASS\n\n## Checked myself\nI read the report above and agree with it.\n", &stderr)

	require.Equal(t, "unverified", result.ReviewVerdict,
		"a pass with no read-tool check of the reviewer's own must be recorded as unverified")
	require.Contains(t, result.Review, "VERDICT: PASS",
		"the reviewer's own words are kept verbatim; only the verdict is downgraded")
	require.Contains(t, stderr.String(), "unverified",
		"the operator gets the one line that says the pass is not to be trusted")
	require.Contains(t, result.Warnings, "reviewer passed the run without any read-tool check; treat the review as unverified",
		"and the orchestrator sees the same fact in the envelope's warnings")
}

// T10c: the other half of the downgrade — a single read-tool call by the
// review turn itself is enough to earn the verdict, so the same PASS is
// recorded as a plain "pass" and the operator sees nothing.
//
// APPROACH NOTE: this exercises the lower layer rather than scripting a tool
// call through the real ExecuteRun. The phase loop, terminal reconciliation
// and the transcript read behind the verdict are all the real ones (a real
// App with a real message store), but the review turn's assistant row is
// seeded directly through app.Messages.Create — one assistant row holding a
// message.ToolCall part, after the user row carrying reviewerPassMarker —
// which is what makes reviewerToolCalls return 1. The e2e direction
// (T10b) covers the same path from the other side, and the markdown-shaped
// verdict text is the only other input attachReview needs.
func TestAttachReview_PassWithAToolCallIsPass(t *testing.T) {
	h := newReviewerPassApp(t, true)
	sess := createModelOverrideSession(t, h.app, "reviewer-pass-verified")

	// The review turn: its prompt user row, then its own read.
	_, err := h.app.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: reviewerPassPrompt}},
	})
	require.NoError(t, err)
	_, err = h.app.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "reviewer-view-1", Name: "view", Input: `{"file_path":"internal/app/app_run_reviewer.go"}`, Finished: true},
			message.TextContent{Text: "VERDICT: PASS\n\n## Checked myself\nview internal/app/app_run_reviewer.go: the claim is confirmed.\n"},
		},
	})
	require.NoError(t, err)

	msgs, err := h.app.Messages.List(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, 1, reviewerToolCalls(msgs),
		"the seeded transcript must read back as exactly one call the reviewer itself made")

	result := &RunResult{FinalText: reviewerPassPrimaryText}
	var stderr bytes.Buffer
	h.app.attachReview(context.Background(), result, sess.ID,
		"VERDICT: PASS\n\n## Checked myself\nview internal/app/app_run_reviewer.go: the claim is confirmed.\n", &stderr)

	require.Equal(t, "pass", result.ReviewVerdict,
		"one read-tool call by the review turn itself earns the plain verdict")
	require.Empty(t, stderr.String(),
		"a pass backed by the reviewer's own check prints nothing at all")
	require.NotContains(t, result.Warnings, "reviewer passed the run without any read-tool check; treat the review as unverified")
}

// T13: a review turn that FAILS for a reason of its own must not replace the
// run's own answer. The executor already completed its turn cleanly, so a
// review that died on a provider error leaves that answer as the run's
// outcome with ReviewVerdict="error", a warning and one stderr line (§7).
//
// Revert-check: restoring the old "a failed review replaces the run's
// result" behaviour turns the "error"/FinalText assertions below red.
//
// The reviewer model answers HTTP 400 — a 4xx, deliberately NOT a 5xx: a
// genuine client error is classified terminal (coordinator_retry_classify.go
// returns classTerminal for >= 400), so the turn fails once instead of
// burning the provider's transient-retry backoff first.
func TestReviewerPass_FailureKeepsPrimaryAnswer(t *testing.T) {
	app := newReviewerPassFailureApp(t)
	sess := createModelOverrideSession(t, app, "reviewer-pass-failure")

	var stderr bytes.Buffer
	res, err := app.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "do the thing",
		ContinueSessionID: sess.ID,
		Overrides:         RunOverrides{ModelRole: config.SelectedModelTypeSmart},
		Mode:              RunModeJSON,
		Stdout:            io.Discard,
		Stderr:            &stderr,
		HideSpinner:       true,
	})
	require.NoError(t, err, "a failed review must not fail the run")
	require.NotNil(t, res)

	// A10: the executor's answer survives the review turn's death.
	require.Equal(t, reviewerPassPrimaryText, res.FinalText,
		"the primary answer must stay the run's final_text")
	require.Equal(t, "error", res.ReviewVerdict,
		"a review turn that failed records the error verdict")

	warned := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "reviewer pass failed") {
			warned = true
		}
	}
	require.True(t, warned, "the run must carry a warning naming the reviewer failure, got %v", res.Warnings)
	require.Contains(t, stderr.String(), "rush run: reviewer pass failed",
		"the operator gets the one stderr line naming the reviewer failure")
}

// newReviewerPassFailureApp is newReviewerPassApp's harness with one change:
// requests for the REVIEWER model are answered with an HTTP 400 and a JSON
// error body, and everything else gets the normal SSE. The reviewer turn is
// the only turn that must fail, so the primary turn's stub behaviour is kept
// byte-for-byte the same as the shared harness's.
func newReviewerPassFailureApp(t *testing.T) *App {
	t.Helper()
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	configDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.Unmarshal(body, &req))
		if req.Model == reviewerPassReviewerModel {
			// A 4xx: terminal, so the turn fails without the provider's
			// transient-retry backoff a 5xx would trigger first.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"message":"reviewer model rejected the request","type":"invalid_request_error"}}`)
			return
		}
		content := reviewerPassPrimaryText
		contentJSON, mErr := json.Marshal(content)
		require.NoError(t, mErr)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"rpf","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":`+string(contentJSON)+`},"finish_reason":null}]}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"rpf","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
	}))
	t.Cleanup(srv.Close)

	dataDir := t.TempDir()
	rushJSON := fmt.Sprintf(`{
  "disable_default_providers": true,
  "providers": {
    "openaicompat": {
      "id": "openaicompat", "type": "openai-compat", "base_url": %q,
      "api_key": "probe", "discover_models": false,
      "models": [
        {"id":"smart-default","context_window":200000,"default_max_tokens":1000},
        {"id":"fast-default","context_window":200000,"default_max_tokens":1000},
        {"id":%q,"context_window":200000,"default_max_tokens":1000}
      ]
    }
  },
  "models": {
    "smart": {"provider":"openaicompat","model":"smart-default"},
    "fast": {"provider":"openaicompat","model":"fast-default"},
    "reviewer": {"provider":"openaicompat","model":%q}
  }
}`, srv.URL, reviewerPassReviewerModel, reviewerPassReviewerModel)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), []byte(rushJSON), 0o644))

	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "smart-default"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "fast-default"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: reviewerPassReviewerModel})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	application, err := New(context.Background(), conn, store)
	if err != nil {
		err = errors.Join(err, db.ReleaseConn(conn))
	}
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)
	return application
}
