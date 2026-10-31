package agent

// Session-title generation trigger + lifetime tests.
//
// The bug these pin: after the FIRST user message of a NEW session, the fast
// model should generate a concise title, persist it on the session row, and
// (via the session service's UpdatedEvent) reach the browser. Reported as not
// occurring at all. Two independent defects produced that:
//
//  1. The title goroutine's context was derived from the turn's genCtx, so
//     the instant runTurn's bounded join gave up (titleJoinGrace) and fell
//     through to cancel(), the still-in-flight title request was killed
//     mid-flight — "Error generating title with fast model; trying next
//     err=context canceled" for BOTH models — and for a web session (born
//     titled "New Session") the "Untitled Session" fallback then declined to
//     write. No title, ever, on every session whose title provider was
//     slower than the grace.
//  2. session.Service.Rename published no pubsub event, so even a title that
//     DID land never reached the tab as session_updated.
//
// (1) is fixed here and in agent_turn_title.go; (2) in
// internal/session/session_update.go, with its own test there and the
// broadcast-level one in internal/server.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// titleSSEServer answers every request with a single-step, text-only
// completion carrying content, counting each request in calls. When delay is
// non-zero the handler waits that long BEFORE writing the stream, standing
// in for a title provider that is merely slow (connection setup +
// time-to-first-token + a ~96-token title), which is the ordinary case the
// old 5s join grace used to cut off.
//
// The wait also honours the request context so a cancelled request cannot
// leave a handler goroutine behind for httptest.Server.Close to trip over.
func titleSSEServer(calls *atomic.Int64, content string, delay time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}]}`, content),
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
}

func titleTestModel(t *testing.T, srv *httptest.Server) Model {
	t.Helper()
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)
	return Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}
}

// newTitleTestAgent builds an agent whose SMART model (the main turn) and
// FAST model (title generation) are two distinct servers, so a test can
// count title requests without the main turn's traffic polluting the count
// and vice versa. Registers the agent's runWg wait as a cleanup so a title
// goroutine that outlives its turn is joined before the test DB is
// released (the Windows TempDir-removal race fakeEnv.cleanup exists for).
func newTitleTestAgent(t *testing.T, env fakeEnv, titleCalls *atomic.Int64, titleContent string, titleDelay time.Duration, opts ...func(*SessionAgentOptions)) (SessionAgent, *httptest.Server) {
	t.Helper()

	mainSrv := singleTurnSSEServer(nil)
	t.Cleanup(mainSrv.Close)
	titleSrv := titleSSEServer(titleCalls, titleContent, titleDelay)
	t.Cleanup(titleSrv.Close)

	agentOpts := SessionAgentOptions{
		SmartModel:           titleTestModel(t, mainSrv),
		FastModel:            titleTestModel(t, titleSrv),
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	}
	for _, opt := range opts {
		opt(&agentOpts)
	}
	a := NewSessionAgent(agentOpts)
	if sa, ok := a.(*sessionAgent); ok && env.cleanup != nil {
		env.cleanup(sa.runWg.Wait)
	}
	return a, titleSrv
}

// TestRun_FirstUserMessageGeneratesTitleViaFastModel is the headline
// requirement: exactly one title request goes to the FAST slot on the first
// user message of a new session, and the result is persisted on the session
// row. The session is born with the web's placeholder title ("New Session")
// on purpose — that is the shape the reported bug always came in, and it is
// why the first-message signal (len(msgs) == 0) has to be OR'd with the
// title test rather than AND'd.
func TestRun_FirstUserMessageGeneratesTitleViaFastModel(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	var titleCalls atomic.Int64
	a, _ := newTitleTestAgent(t, env, &titleCalls, "A Generated Title", 0)

	res, err := a.Run(context.Background(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "how do I reverse a linked list in place",
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	require.Equal(t, int64(1), titleCalls.Load(),
		"the first user message of a new session must generate exactly one title, via the fast-model slot")

	updated, err := env.sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "A Generated Title", updated.Title,
		"the generated title must be persisted on the session row")
}

// TestRun_SecondUserMessageDoesNotRegenerateTitle pins the once-per-session
// guard: a session that already carries a generated title never triggers a
// second title call. The persisted title IS the guard.
func TestRun_SecondUserMessageDoesNotRegenerateTitle(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	var titleCalls atomic.Int64
	a, _ := newTitleTestAgent(t, env, &titleCalls, "A Generated Title", 0)

	for i, prompt := range []string{"first message", "second message", "third message"} {
		res, err := a.Run(context.Background(), SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          prompt,
			MaxOutputTokens: 1000,
		})
		require.NoError(t, err, "turn %d", i)
		require.NotNil(t, res, "turn %d", i)
	}

	require.Equal(t, int64(1), titleCalls.Load(),
		"only the FIRST user message may generate a title; the persisted title guards every later turn")

	updated, err := env.sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "A Generated Title", updated.Title,
		"the already-generated title must survive every later turn untouched")
}

// TestRun_ContinuedSessionWithTitleDoesNotGenerateTitle covers the other
// half of the guard: an existing session resumed with a real title and a
// real message history owes no title.
func TestRun_ContinuedSessionWithTitleDoesNotGenerateTitle(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "Already Named Session")
	require.NoError(t, err)
	_, err = env.messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier user turn"}},
	})
	require.NoError(t, err)

	var titleCalls atomic.Int64
	a, _ := newTitleTestAgent(t, env, &titleCalls, "Should Never Be Generated", 0)

	res, err := a.Run(context.Background(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "continue please",
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	require.Equal(t, int64(0), titleCalls.Load(),
		"a continued session that already has a title must not generate one")

	updated, err := env.sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "Already Named Session", updated.Title)
}

// TestRun_NoticeTurnDoesNotGenerateTitle pins the exclusion for
// system-authored turns: an auto-resume / background-job completion notice
// is not the user's first message, so it must never title a session.
func TestRun_NoticeTurnDoesNotGenerateTitle(t *testing.T) {
	env := testEnv(t)

	// A genuinely new, nameless session: the len(msgs) == 0 clause alone
	// would fire here, so this exercises nothing but the notice guard.
	sess, err := env.sessions.Create(t.Context(), "")
	require.NoError(t, err)

	var titleCalls atomic.Int64
	a, _ := newTitleTestAgent(t, env, &titleCalls, "Named After A Notice", 0)

	res, err := a.Run(context.Background(), SessionAgentCall{
		SessionID:           sess.ID,
		Prompt:              "[Background job completed] build finished",
		MaxOutputTokens:     1000,
		BackgroundJobNotice: true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	require.Equal(t, int64(0), titleCalls.Load(),
		"a background-job notice turn must not generate a session title")

	updated, err := env.sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Empty(t, updated.Title, "the notice turn must leave the session nameless")
}

// TestRun_TitleSlowerThanTheGraceStillLandsAfterTheTurn is the regression
// test for the reported bug: a title provider slower than the join grace
// must NOT lose the title. The turn gives up waiting (it is never held open
// for a title) but the goroutine finishes on its own budget, persists the
// title, and publishes it.
//
// Before the fix this was red: the join expired, runTurn's cancel() killed
// the in-flight request, both model attempts failed with "context
// canceled", and the session kept its pre-generation name forever.
func TestRun_TitleSlowerThanTheGraceStillLandsAfterTheTurn(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	var titleCalls atomic.Int64
	const titleDelay = 10 * time.Second
	a, _ := newTitleTestAgent(t, env, &titleCalls, "A Slow Title", titleDelay,
		func(o *SessionAgentOptions) { o.TitleJoinGrace = 300 * time.Millisecond })

	start := time.Now()
	res, err := a.Run(context.Background(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "summarise this repository for me",
		MaxOutputTokens: 1000,
	})
	runElapsed := time.Since(start)
	require.NoError(t, err, "a slow title must never fail the user's turn")
	require.NotNil(t, res)
	require.Less(t, runElapsed, 3*time.Second,
		"the turn must not be held open past the grace for a slow title (got %v)", runElapsed)

	// The title is no longer tied to the turn: it lands on its own, on the
	// goroutine's own budget, shortly after the turn returns.
	deadline := time.Now().Add(60 * time.Second)
	for {
		updated, getErr := env.sessions.Get(context.Background(), sess.ID)
		require.NoError(t, getErr)
		if updated.Title == "A Slow Title" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("title never landed after the turn gave up waiting: last title %q", updated.Title)
		}
		time.Sleep(25 * time.Millisecond)
	}

	require.Equal(t, int64(1), titleCalls.Load(),
		"the slow title must be attempted exactly once, not retried per turn")
}

// TestRun_TitleFastModelFailureFallsBackToSmartAndNeverFailsTheTurn keeps
// the existing fast -> smart fallback honest AND pins the isolation half: a
// failing fast model must neither fail nor delay the user's turn. The title
// simply comes from the smart slot instead (which is the same server as the
// main turn here, hence its "ok" completion).
func TestRun_TitleFastModelFailureFallsBackToSmartAndNeverFailsTheTurn(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	var fastTitleCalls atomic.Int64
	failTitleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fastTitleCalls.Add(1)
		http.Error(w, "boom", http.StatusBadRequest)
	}))
	t.Cleanup(failTitleSrv.Close)
	mainSrv := singleTurnSSEServer(nil)
	t.Cleanup(mainSrv.Close)

	agent := NewSessionAgent(SessionAgentOptions{
		SmartModel:           titleTestModel(t, mainSrv),
		FastModel:            titleTestModel(t, failTitleSrv),
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})
	if sa, ok := agent.(*sessionAgent); ok && env.cleanup != nil {
		env.cleanup(sa.runWg.Wait)
	}

	start := time.Now()
	res, err := agent.Run(context.Background(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "do the thing",
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err, "a failing fast title model must never fail the user's turn")
	require.NotNil(t, res)
	require.Less(t, time.Since(start), 5*time.Second, "a failing title must not delay the turn")

	require.Equal(t, int64(1), fastTitleCalls.Load(), "the fast slot must be tried exactly once")

	updated, err := env.sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "ok", updated.Title,
		"with the fast model failing, the title must come from the smart-model fallback, not be abandoned")
}

// TestTurnTitleJoinerWaitPaths directly pins both bounded join outcomes. A
// blocked done channel forces the grace timeout; a subsequently closed done
// channel is observed immediately on the second joiner. The seam provides an
// ordering barrier proving disarm has happened before either wait starts.
func TestTurnTitleJoinerWaitPaths(t *testing.T) {
	const grace = 100 * time.Millisecond

	t.Run("timeout with held-open done", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			disarmed := make(chan struct{})
			turnTitleJoinAfterDisarmSeam = func() { close(disarmed) }
			done := make(chan struct{})
			returned := make(chan struct{})
			var closeDone sync.Once
			t.Cleanup(func() {
				closeDone.Do(func() { close(done) })
				<-returned
				turnTitleJoinAfterDisarmSeam = nil
			})

			joiner := newTurnTitleJoiner(streamWatchdog{disarm: func() {}}, done, grace, "timeout")
			go func() {
				joiner.join()
				close(returned)
			}()

			// The seam is a deterministic ordering barrier; quiescence confirms
			// the joiner has begun waiting on its grace timer.
			synctest.Wait()
			select {
			case <-disarmed:
			default:
				t.Fatal("join did not reach the post-disarm barrier")
			}
			select {
			case <-returned:
				t.Fatal("join returned before the grace timeout")
			default:
			}
			time.Sleep(grace)
			synctest.Wait()
			select {
			case <-returned:
			default:
				t.Fatal("join did not return after the grace timeout")
			}
		})
	})

	t.Run("done closes after disarm", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			disarmed := make(chan struct{})
			turnTitleJoinAfterDisarmSeam = func() { close(disarmed) }
			done := make(chan struct{})
			returned := make(chan struct{})
			var closeDone sync.Once
			t.Cleanup(func() {
				closeDone.Do(func() { close(done) })
				<-returned
				turnTitleJoinAfterDisarmSeam = nil
			})

			joiner := newTurnTitleJoiner(streamWatchdog{disarm: func() {}}, done, time.Minute, "done")
			go func() {
				joiner.join()
				close(returned)
			}()
			synctest.Wait()
			select {
			case <-disarmed:
			default:
				t.Fatal("join did not reach the post-disarm barrier")
			}
			closeDone.Do(func() { close(done) })
			synctest.Wait()
			select {
			case <-returned:
			default:
				t.Fatal("join did not return after done closed")
			}
		})
	})
}
