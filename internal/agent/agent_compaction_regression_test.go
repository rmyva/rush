package agent

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func compactionRetryServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			payload := `data: {"id":"cut","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"abandoned reasoning","content":"abandoned text"},"finish_reason":null}]}` + "\n\n"
			_, err = fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
			require.NoError(t, err)
			_, err = fmt.Fprintf(conn, "%x\r\n%s\r\n", len(payload), payload)
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"ok\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"probe\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"final reasoning\",\"content\":\"final summary\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"ok\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"probe\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func compactionFixture(t *testing.T, srv *httptest.Server) (*sessionAgent, string, Model, *sql.DB) {
	t.Helper()
	env := testEnv(t)
	lm := languageModelFromServer(t, srv)
	model := Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}}
	a := NewSessionAgent(SessionAgentOptions{SmartModel: model, FastModel: model, SystemPrompt: "probe", IsYolo: true, Sessions: env.sessions, Messages: env.messages, Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true}).(*sessionAgent)
	sess, err := env.sessions.Create(t.Context(), "compaction regression")
	require.NoError(t, err)
	return a, sess.ID, a.smartModel.Get(), env.conn
}

func TestSilentCompactionKeepsRecentTailVerbatim(t *testing.T) {
	// Keep recent exchanges verbatim; older history should be summarized.
	srv := summarizeSSEServer(10, 2, nil)
	t.Cleanup(srv.Close)
	a, sid, model, _ := compactionFixture(t, srv)
	var seeded []message.Message
	for i := 0; i < 16; i++ {
		role := message.User
		if i%2 == 1 {
			role = message.Assistant
		}
		m, err := a.messages.Create(t.Context(), sid, message.CreateMessageParams{Role: role, Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("history-%d", i)}}})
		require.NoError(t, err)
		if role == message.Assistant {
			m.AddFinish(message.FinishReasonEndTurn, "", "")
			require.NoError(t, a.messages.Update(t.Context(), m.Clone()))
		}
		seeded = append(seeded, m)
	}
	require.NoError(t, a.runSummarizeSilent(t.Context(), sid, fantasy.ProviderOptions{}, model, ""))
	sess, err := a.sessions.Get(t.Context(), sid)
	require.NoError(t, err)
	require.NotEmpty(t, sess.SummaryMessageID)
	modelHistory, err := a.getSessionMessages(t.Context(), sess)
	require.NoError(t, err)
	wantIDs := append([]string{sess.SummaryMessageID}, messageIDs(seeded[8:])...)
	require.Equal(t, wantIDs, messageIDs(modelHistory))
	require.Equal(t, message.User, modelHistory[0].Role)
	for i, got := range modelHistory[1:] {
		require.Equal(t, seeded[i+8].Role, got.Role)
		require.Equal(t, fmt.Sprintf("history-%d", i+8), got.Content().Text)
	}
	for i, m := range seeded {
		_, err = a.messages.Get(t.Context(), m.ID)
		if i >= 8 {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
}

func messageIDs(msgs []message.Message) []string {
	ids := make([]string, len(msgs))
	for i, msg := range msgs {
		ids[i] = msg.ID
	}
	return ids
}

func TestSilentCompactionToolBoundaryAndPinnedOlderMessage(t *testing.T) {
	var requestMu sync.Mutex
	var requestBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requestMu.Lock()
		requestBody = string(body)
		requestMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"probe\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"probe\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	a, sid, model, conn := compactionFixture(t, srv)
	var seeded []message.Message
	for i := 0; i < 12; i++ {
		role := message.User
		parts := []message.ContentPart{message.TextContent{Text: fmt.Sprintf("history-%d", i)}}
		switch i {
		case 1, 5:
			role = message.Assistant
			parts = []message.ContentPart{message.ToolCall{ID: fmt.Sprintf("call-%d", i), Name: "probe", Input: `{}`}}
		case 6:
			role = message.Tool
			parts = []message.ContentPart{message.ToolResult{ToolCallID: "call-5", Name: "probe", Content: "tool-result-6"}}
		}
		m, err := a.messages.Create(t.Context(), sid, message.CreateMessageParams{Role: role, Parts: parts})
		require.NoError(t, err)
		if role == message.Assistant {
			m.AddFinish(message.FinishReasonEndTurn, "", "")
			require.NoError(t, a.messages.Update(t.Context(), m.Clone()))
		}
		seeded = append(seeded, m)
	}
	require.NoError(t, a.messages.SetPinned(t.Context(), seeded[1].ID, true))
	// Set the pinned row well before the new summary and future kept messages,
	// avoiding same-second rowid ordering from making it appear after summary.
	ids := make([]any, 0, 6)
	for i := 0; i < 6; i++ {
		ids = append(ids, seeded[i].ID)
	}
	_, err := conn.ExecContext(t.Context(), `UPDATE messages SET created_at = created_at - 100 WHERE id IN (?, ?, ?, ?, ?, ?)`, ids...)
	require.NoError(t, err)
	require.NoError(t, a.runSummarizeSilent(t.Context(), sid, fantasy.ProviderOptions{}, model, ""))
	sess, err := a.sessions.Get(t.Context(), sid)
	require.NoError(t, err)
	got, err := a.getSessionMessages(t.Context(), sess)
	require.NoError(t, err)
	require.Equal(t, []string{seeded[1].ID, sess.SummaryMessageID, seeded[7].ID, seeded[8].ID, seeded[9].ID, seeded[10].ID, seeded[11].ID}, messageIDs(got))
	require.Equal(t, message.User, got[1].Role)
	for _, id := range []string{seeded[5].ID, seeded[6].ID} {
		_, getErr := a.messages.Get(t.Context(), id)
		require.Error(t, getErr)
	}
	requestMu.Lock()
	body := requestBody
	requestMu.Unlock()
	require.Contains(t, body, "call-5")
	require.Contains(t, body, "tool-result-6")
	require.Equal(t, message.User, got[2].Role)
	require.Equal(t, "history-7", got[2].Content().Text)
}

func TestSilentCompactionPinnedOlderMessageAtKeptTimestamp(t *testing.T) {
	srv := summarizeSSEServer(10, 2, nil)
	t.Cleanup(srv.Close)
	a, sid, model, conn := compactionFixture(t, srv)
	var seeded []message.Message
	for i := 0; i < 8; i++ {
		role := message.User
		if i%2 == 1 {
			role = message.Assistant
		}
		m, err := a.messages.Create(t.Context(), sid, message.CreateMessageParams{Role: role, Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("history-%d", i)}}})
		require.NoError(t, err)
		if role == message.Assistant {
			m.AddFinish(message.FinishReasonEndTurn, "", "")
			require.NoError(t, a.messages.Update(t.Context(), m.Clone()))
		}
		seeded = append(seeded, m)
	}
	_, err := conn.ExecContext(t.Context(), `UPDATE messages SET created_at = 100 WHERE session_id = ?`, sid)
	require.NoError(t, err)
	require.NoError(t, a.messages.SetPinned(t.Context(), seeded[1].ID, true))
	require.NoError(t, a.runSummarizeSilent(t.Context(), sid, fantasy.ProviderOptions{}, model, ""))
	sess, err := a.sessions.Get(t.Context(), sid)
	require.NoError(t, err)
	got, err := a.getSessionMessages(t.Context(), sess)
	require.NoError(t, err)
	require.Equal(t, []string{sess.SummaryMessageID, seeded[1].ID, seeded[4].ID, seeded[5].ID, seeded[6].ID, seeded[7].ID}, messageIDs(got))
}

func TestCompactionRetriesDiscardAbandonedSummaryText(t *testing.T) {
	for _, silent := range []bool{false, true} {
		name := "manual"
		if silent {
			name = "silent"
		}
		t.Run(name, func(t *testing.T) {
			srv, calls := compactionRetryServer(t)
			a, sid, model, _ := compactionFixture(t, srv)
			for i := 0; i < 8; i++ {
				role := message.User
				if i%2 == 1 {
					role = message.Assistant
				}
				_, err := a.messages.Create(t.Context(), sid, message.CreateMessageParams{Role: role, Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("old-%d", i)}}})
				require.NoError(t, err)
			}
			var err error
			if silent {
				err = a.runSummarizeSilent(t.Context(), sid, fantasy.ProviderOptions{}, model, "")
			} else {
				err = a.runSummarizeBody(t.Context(), sid, fantasy.ProviderOptions{}, model, "")
			}
			require.NoError(t, err)
			require.GreaterOrEqual(t, calls.Load(), int64(2))
			sess, err := a.sessions.Get(t.Context(), sid)
			require.NoError(t, err)
			summary, err := a.messages.Get(t.Context(), sess.SummaryMessageID)
			require.NoError(t, err)
			require.Equal(t, "final summary", summary.Content().Text)
			require.NotContains(t, summary.Content().Text, "abandoned")
			require.Equal(t, "final reasoning", summary.FullReasoningText())
			require.NotContains(t, summary.FullReasoningText(), "abandoned")
		})
	}
}
