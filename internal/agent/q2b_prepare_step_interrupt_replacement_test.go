package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type blockingFirstDrainPendingInjects struct {
	session.Service
	entered chan struct{}
	fired   atomic.Bool
}

func (b *blockingFirstDrainPendingInjects) DrainPendingInjects(ctx context.Context, sessionID string) ([]session.PendingInject, bool, error) {
	if !b.fired.Swap(true) {
		close(b.entered)
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	return b.Service.DrainPendingInjects(ctx, sessionID)
}

func TestInterruptReplacementDuringFirstPrepareStepDrainSurvives(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "replacement during PrepareStep inject drain")
	require.NoError(t, err)

	var calls atomic.Int64
	requestStarted := make(chan struct{}, 1)
	proceed := make(chan struct{})
	srv := requestStartedSSEServer(&calls, requestStarted, proceed)
	t.Cleanup(srv.Close)
	provider, err := openaicompat.New(openaicompat.WithBaseURL(srv.URL), openaicompat.WithAPIKey("probe"))
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)
	titleSrv := singleTurnSSEServer(nil)
	t.Cleanup(titleSrv.Close)
	titleProvider, err := openaicompat.New(openaicompat.WithBaseURL(titleSrv.URL), openaicompat.WithAPIKey("probe"))
	require.NoError(t, err)
	titleLM, err := titleProvider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	agentIface := NewSessionAgent(SessionAgentOptions{
		SmartModel:   Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
		FastModel:    Model{Model: titleLM, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
		SystemPrompt: "you are a probe", IsYolo: true, Sessions: env.sessions, Messages: env.messages,
		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, DataDirectory: t.TempDir(),
	})
	sa := agentIface.(*sessionAgent)
	blocking := &blockingFirstDrainPendingInjects{Service: sa.sessions, entered: make(chan struct{})}
	sa.sessions = blocking

	const replacementPrompt = "replacement survives failed first PrepareStep"
	resolved := make(chan error, 1)
	runDone := make(chan error, 1)
	go func() {
		_, runErr := agentIface.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "initial request", MaxOutputTokens: 1000})
		runDone <- runErr
	}()
	select {
	case <-blocking.entered:
	case runErr := <-runDone:
		t.Fatalf("Run returned before first PrepareStep drain blocked: %v", runErr)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for first PrepareStep drain")
	}

	// The production entry point records the replacement and cancels the
	// generation blocked in the first PrepareStep drain.
	require.True(t, sa.InterruptAndReplace(sess.ID, SessionAgentCall{
		SessionID: sess.ID, Prompt: replacementPrompt, MaxOutputTokens: 1000,
		onQueueResolved: func(_ *fantasy.AgentResult, err error) { resolved <- err },
	}))
	select {
	case <-requestStarted:
	case err := <-resolved:
		t.Fatalf("replacement was resolved without running: %v", err)
	case runErr := <-runDone:
		t.Fatalf("Run exited instead of running replacement: %v", runErr)
	case <-time.After(15 * time.Second):
		t.Fatal("replacement never reached provider")
	}
	close(proceed)
	select {
	case runErr := <-runDone:
		require.NoError(t, runErr)
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not complete")
	}
	require.Equal(t, int64(1), calls.Load())
	select {
	case resolveErr := <-resolved:
		require.NoError(t, resolveErr)
	default:
		t.Fatal("replacement waiter was not resolved by its executed turn")
	}
	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	found := false
	for _, msg := range msgs {
		if msg.Role == message.User && msg.Content().Text == replacementPrompt {
			found = true
		}
	}
	require.True(t, found, "replacement prompt must be persisted by its turn")
	require.Equal(t, 0, sa.QueuedPrompts(sess.ID))
}
