package agent

// Retry/continuation attempt-evidence-ownership tests (R3-1 series): the
// classifiers must only act on evidence the CURRENT call's own attempt
// produced, never a foreign or pre-existing row. Split out of
// coordinator_retry_test.go to stay under the repo's 1000-line file limit
// (CLAUDE.md) — a pure move, no behavior change.

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRetryClassifiers_AttemptScopedEvidence pins the R3-1 fix: the retry
// classifiers must only act on evidence the CURRENT call's own attempt
// produced. Two independent guards enforce this — an admission-shaped
// refusal (ErrSessionBusy, ErrAgentShuttingDown, OS session-lock busy)
// short-circuits classification entirely, and a classifier only ever reads
// the assistant row the attempt itself wrote, identified by the
// OnAssistantMessageCreated callback ID; a foreign or pre-existing row,
// however new, never authorizes a retry or continuation. Without these
// guards, caller A refused at admission would observe caller B's stalled
// partial assistant message as the session's last row and "resume" it with
// a continuation prompt — an unauthorized retry execution quoting text A's
// call never produced.
func TestRetryClassifiers_AttemptScopedEvidence(t *testing.T) {
	t.Parallel()
	partialStalledParts := func(text string) []message.ContentPart {
		return []message.ContentPart{
			message.TextContent{Text: text},
			message.Finish{Reason: message.FinishReasonError, Message: streamStalledFinishTitle},
		}
	}

	t.Run("admission refusal with foreign stalled partial does not continue", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		// Caller B's stalled partial is the session's last assistant row.
		coord, sid, _ := appendAssistant(t, env, partialStalledParts("partial answer"))
		// Mirrors agent_run.go's pre-attempt refusal exactly (including
		// the %w wrap the fail-fast caller sees). The refusal guard must
		// fire regardless of owned evidence, hence "" — the refused
		// attempt wrote no assistant row of its own.
		busyErr := fmt.Errorf("session %q is already processing another request: %w", sid, ErrSessionBusy)
		_, ok := coord.shouldContinueTurn(t.Context(), sid, busyErr, "" /* the refused attempt wrote no assistant row */)
		assert.False(t, ok, "a refused call must not resume a foreign stalled message as a continuation")
		assert.False(t, coord.shouldRetryTurn(t.Context(), sid, busyErr, "" /* the refused attempt wrote no assistant row */), "a refused call must not be retried")
	})

	t.Run("shutdown refusal does not retry or continue", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		coord, sid, _ := appendAssistant(t, env, partialStalledParts("partial answer"))
		_, ok := coord.shouldContinueTurn(t.Context(), sid, ErrAgentShuttingDown, "" /* the refused attempt wrote no assistant row */)
		assert.False(t, ok, "a shutdown refusal means no attempt ran — nothing to resume")
		assert.False(t, coord.shouldRetryTurn(t.Context(), sid, ErrAgentShuttingDown, "" /* the refused attempt wrote no assistant row */), "a shutdown refusal must not be retried")
	})

	t.Run("own stalled attempt continues even when a newer foreign row exists", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions, messages: env.messages}
		sess, err := env.sessions.Create(t.Context(), "own-attempt-evidence")
		require.NoError(t, err)
		// This call's own attempt wrote a stalled partial row...
		own, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  message.Assistant,
			Parts: partialStalledParts("A's own partial answer"),
		})
		require.NoError(t, err)
		// ...and a concurrent caller B's stalled row landed AFTER it, so
		// B's row is the session's newest at classification time.
		// Classification must still read the attempt's OWN row.
		_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  message.Assistant,
			Parts: partialStalledParts("B's newer foreign answer"),
		})
		require.NoError(t, err)
		msg, ok := coord.shouldContinueTurn(t.Context(), sess.ID, context.Canceled, own.ID)
		assert.True(t, ok, "a stall of this call's own partial attempt still resumes via a continuation")
		assert.Equal(t, own.ID, msg.ID, "the continuation must be built from the attempt's own row, not the newer foreign row")
		assert.Equal(t, "A's own partial answer", msg.FullText())
	})

	t.Run("attempt that wrote no row does not act on a pre-existing error row", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		// A stalled no-progress row from an earlier turn is the session's
		// last assistant row. The attempt being classified produced no
		// assistant row of its own (attemptAssistantMsgID == ""), so the
		// classifiers have no owned evidence and must not resurrect the
		// earlier turn.
		coord, sid, _ := appendAssistant(t, env, []message.ContentPart{
			message.Finish{Reason: message.FinishReasonError, Message: streamStalledFinishTitle},
		})
		transientErr := providerErr(http.StatusTooManyRequests, "The service may be temporarily overloaded")
		assert.False(t, coord.shouldRetryTurn(t.Context(), sid, transientErr, "" /* the attempt wrote no assistant row */), "a transient error with no owned evidence must not resurrect an earlier turn as a retry")
		_, ok := coord.shouldContinueTurn(t.Context(), sid, transientErr, "" /* the attempt wrote no assistant row */)
		assert.False(t, ok, "a transient error with no owned evidence must not continue an earlier turn either")
	})

	t.Run("os-lock busy refusal does not retry", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		coord, sid, _ := appendAssistant(t, env, partialStalledParts("partial answer"))
		// Constructed per internal/session/lock.go's SessionLockBusyError
		// shape; runOwned wraps it as `session %q is already in use: %w`.
		lockBusy := &session.SessionLockBusyError{Path: "/data/locks/" + sid, HolderPID: 424242}
		lockErr := fmt.Errorf("session %q is already in use: %w", sid, lockBusy)
		_, ok := coord.shouldContinueTurn(t.Context(), sid, lockErr, "" /* the refused attempt wrote no assistant row */)
		assert.False(t, ok, "an OS session-lock refusal means no attempt ran — nothing to resume")
		assert.False(t, coord.shouldRetryTurn(t.Context(), sid, lockErr, "" /* the refused attempt wrote no assistant row */), "an OS session-lock refusal must not be retried")
	})
}

// TestRunInternal_NoRetryAfterAdmissionRefusal_WithForeignStalledMessage
// reproduces the R3-1 bug end-to-end at the coordinator level: caller A is
// refused at mailbox admission (FailIfSessionBusy, the exact error
// agent_run.go produces BEFORE any provider request), while caller B's
// stalled partial assistant message is the session's last row. Before the
// fix, the retry loop's shouldContinueTurn saw B's row, decided A's failed
// call had partial progress worth resuming, and re-ran A's call as a
// continuation quoting B's text — an unauthorized retry execution. The
// refusal must surface untouched: exactly one provider call, the original
// prompt, B's message preserved, no retry attempt after the backoff.
// Deliberate scope: a true ExecuteRun-level test needs internal/app files
// owned by a sibling task; runInternal + the mock agent IS the
// coordinator's own retry machinery, and the mailbox refusal itself is
// covered by existing fail-fast tests.
func TestRunInternal_NoRetryAfterAdmissionRefusal_WithForeignStalledMessage(t *testing.T) {
	const providerID = "test-busy-refusal"
	const prompt = "A's reviewer prompt"

	orig := streamStallRetryBaseBackoff
	streamStallRetryBaseBackoff = time.Millisecond
	t.Cleanup(func() { streamStallRetryBaseBackoff = orig })

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	providerCfg := config.ProviderConfig{
		ID:   providerID,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096},
		},
	}
	cfg.Config().Providers.Set(providerID, providerCfg)
	sel := config.SelectedModel{Provider: providerID, Model: "test-model"}
	cfg.Config().Models[config.SelectedModelTypeSmart] = sel
	cfg.Config().Models[config.SelectedModelTypeFast] = sel

	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}

	sess, err := env.sessions.Create(t.Context(), "busy-refusal-test")
	require.NoError(t, err)

	// Seed caller B's history: B's stalled partial is the session's last
	// assistant row before A's fail-fast call ever starts.
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "review this"}},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "B's partial answer, cut off"},
			message.Finish{Reason: message.FinishReasonError, Message: streamStalledFinishTitle},
		},
	})
	require.NoError(t, err)

	callCount := 0
	var recordedPrompt string
	agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		callCount++
		recordedPrompt = call.Prompt
		// Fail-fast caller A: exactly the pre-attempt refusal
		// agent_run.go produces for FailIfSessionBusy calls.
		return nil, fmt.Errorf("session %q is already processing another request: %w", call.SessionID, ErrSessionBusy)
	})
	coord.currentAgent = agent

	pinned, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)

	res, err := coord.runInternal(t.Context(), sess.ID, prompt, pinned)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSessionBusy, "the fail-fast refusal must surface, not be swallowed by the retry machinery")
	assert.Nil(t, res)
	assert.Equal(t, 1, callCount, "no second provider call may happen — not immediately, and no retry exists to run after B releases")
	assert.Equal(t, prompt, recordedPrompt, "the prompt must never be rewritten into a continuation quoting B's text")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var lastAssistantText string
	for _, m := range msgs {
		if m.Role == message.Assistant {
			lastAssistantText = m.FullText()
		}
	}
	assert.Contains(t, lastAssistantText, "B's partial answer", "caller B's stalled message must remain the last assistant row, untouched")
}

func TestRunInternal_UnauthorizedRetryReusesUserMessage(t *testing.T) {
	const providerID = "test-401-reuse"
	const prompt = "retry the unauthorized request"
	t.Setenv("TEST_401_REUSE_KEY", "fresh-key")

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	providerCfg := config.ProviderConfig{
		ID: providerID, Type: "openai", APIKey: "stale-key", APIKeyTemplate: "$TEST_401_REUSE_KEY",
		Models: []catwalk.Model{{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096}},
	}
	cfg.Config().Providers.Set(providerID, providerCfg)
	sel := config.SelectedModel{Provider: providerID, Model: "test-model"}
	cfg.Config().Models[config.SelectedModelTypeSmart] = sel
	cfg.Config().Models[config.SelectedModelTypeFast] = sel
	coord := &coordinator{
		cfg: cfg, sessions: env.sessions, messages: env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}
	sess, err := env.sessions.Create(t.Context(), "401-reuse-test")
	require.NoError(t, err)

	var createdUserMsgID string
	callCount := 0
	agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		callCount++
		if callCount == 1 {
			require.Empty(t, call.ExistingMessageID)
			userMsg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: prompt}},
			})
			require.NoError(t, err)
			createdUserMsgID = userMsg.ID
			require.NotNil(t, call.OnUserMessageCreated)
			call.OnUserMessageCreated(userMsg.ID)
			return nil, &fantasy.ProviderError{StatusCode: http.StatusUnauthorized, Message: "stale credential"}
		}
		require.Equal(t, createdUserMsgID, call.ExistingMessageID, "401 retry must reuse the user message")
		return agentResultWithText("authorized after refresh"), nil
	})
	coord.currentAgent = agent
	pinned, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)
	result, err := coord.runInternal(t.Context(), sess.ID, prompt, pinned)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, callCount)
}

// TestRunInternal_RetryReusesUserMessage_NoDuplicate reproduces a real
// incident: a session where the same prompt appeared multiple times in
// history after transient provider failures. Each retry created a fresh
// user message instead of reusing the one the first attempt already
// created. Fix: capture the first attempt's created message ID via
// OnUserMessageCreated and feed it back as ExistingMessageID on retries.
func TestRunInternal_RetryReusesUserMessage_NoDuplicate(t *testing.T) {
	const providerID = "test-retry-dup"
	const prompt = "investigate the flaky provider"

	orig := streamStallRetryBaseBackoff
	streamStallRetryBaseBackoff = time.Millisecond
	t.Cleanup(func() { streamStallRetryBaseBackoff = orig })

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	providerCfg := config.ProviderConfig{
		ID:   providerID,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096},
		},
	}
	cfg.Config().Providers.Set(providerID, providerCfg)
	sel := config.SelectedModel{Provider: providerID, Model: "test-model"}
	cfg.Config().Models[config.SelectedModelTypeSmart] = sel
	cfg.Config().Models[config.SelectedModelTypeFast] = sel

	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}

	sess, err := env.sessions.Create(t.Context(), "retry-dup-test")
	require.NoError(t, err)

	var createdUserMsgID string
	callCount := 0
	agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		callCount++
		if callCount == 1 {
			require.Empty(t, call.ExistingMessageID, "first attempt must create its own user message")
			userMsg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role:  message.User,
				Parts: []message.ContentPart{message.TextContent{Text: prompt}},
			})
			require.NoError(t, err)
			createdUserMsgID = userMsg.ID
			require.NotNil(t, call.OnUserMessageCreated)
			call.OnUserMessageCreated(userMsg.ID)

			emptyRow, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.Finish{Reason: message.FinishReasonError, Message: "Empty response"},
				},
			})
			require.NoError(t, err)
			// Report the row exactly like the real PrepareStep does, so the
			// retry classifiers can identify it as this attempt's own
			// evidence.
			require.NotNil(t, call.OnAssistantMessageCreated)
			call.OnAssistantMessageCreated(emptyRow.ID)
			return nil, nil // "turn returned no error" -- shouldRetryTurn's empty-stream retry path
		}
		// Retry: must reuse the message the first attempt created, not a new one.
		assert.Equal(t, createdUserMsgID, call.ExistingMessageID, "retry must reuse the first attempt's user message")
		// A real runTurn would persist a clean assistant finish here; without
		// it, shouldRetryTurn keeps seeing call 1's FinishReasonError message
		// and retries again.
		recoveredRow, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "recovered"},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, call.OnAssistantMessageCreated)
		call.OnAssistantMessageCreated(recoveredRow.ID)
		return agentResultWithText("recovered"), nil
	})
	coord.currentAgent = agent

	pinned, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)

	res, err := coord.runInternal(t.Context(), sess.ID, prompt, pinned)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 2, callCount, "exactly one retry expected")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var userCount int
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == prompt {
			userCount++
		}
	}
	assert.Equal(t, 1, userCount, "the prompt must appear exactly once in history, not once per retry attempt")
}

// TestRunInternal_SuccessfulResultNotClobberedByConcurrentStalledMessage
// reproduces the remaining R3-1 mechanism (round 5): caller A's own
// attempt completes SUCCESSFULLY (clean end_turn row, non-empty result),
// and before A's retry-loop classification runs, a concurrent legitimate
// caller B on the SAME session commits its own stalled partial assistant
// message. Under the old baseline scheme the classifiers read the
// session's LAST assistant row -- B's -- treated it as A's new evidence
// (its ID merely differed from the pre-attempt baseline), and replaced
// A's successful result with a spurious continuation quoting B's text.
// Under the ownership fix the classifiers read only the assistant row
// A's own attempt wrote (OnAssistantMessageCreated), so A's result must
// come back unchanged and no second provider call may happen.
func TestRunInternal_SuccessfulResultNotClobberedByConcurrentStalledMessage(t *testing.T) {
	const providerID = "test-success-clobber"
	const prompt = "A's prompt that succeeds on the first attempt"

	orig := streamStallRetryBaseBackoff
	streamStallRetryBaseBackoff = time.Millisecond
	t.Cleanup(func() { streamStallRetryBaseBackoff = orig })

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	providerCfg := config.ProviderConfig{
		ID:   providerID,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096},
		},
	}
	cfg.Config().Providers.Set(providerID, providerCfg)
	sel := config.SelectedModel{Provider: providerID, Model: "test-model"}
	cfg.Config().Models[config.SelectedModelTypeSmart] = sel
	cfg.Config().Models[config.SelectedModelTypeFast] = sel

	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}

	sess, err := env.sessions.Create(t.Context(), "success-clobber-test")
	require.NoError(t, err)

	callCount := 0
	var seenPrompts []string
	ownResult := agentResultWithText("A's clean final answer")
	agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		callCount++
		seenPrompts = append(seenPrompts, call.Prompt)
		if callCount > 1 {
			// Any second provider call is itself the bug: A's attempt
			// already succeeded.
			return agentResultWithText("SPURIOUS RETRY MUST NOT RUN"), nil
		}
		// A's own attempt: persists its user message and a CLEAN
		// assistant row, reporting both exactly like the real runTurn
		// preamble and PrepareStep do.
		userMsg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: prompt}},
		})
		require.NoError(t, err)
		require.NotNil(t, call.OnUserMessageCreated)
		call.OnUserMessageCreated(userMsg.ID)
		ownRow, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "A's clean final answer"},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, call.OnAssistantMessageCreated)
		call.OnAssistantMessageCreated(ownRow.ID)
		// A's attempt is now fully done -- but before runInternal's
		// classification runs, caller B legitimately takes the freed
		// session and commits ITS OWN stalled partial turn.
		_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "B's stalled partial answer"},
				message.Finish{Reason: message.FinishReasonError, Message: streamStalledFinishTitle},
			},
		})
		require.NoError(t, err)
		return ownResult, nil
	})
	coord.currentAgent = agent

	pinned, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)

	res, err := coord.runInternal(t.Context(), sess.ID, prompt, pinned)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 1, callCount, "A's successful attempt must not trigger any retry or continuation provider call")
	require.Len(t, seenPrompts, 1)
	assert.Equal(t, prompt, seenPrompts[0], "the only prompt A sends is its own; no continuation quoting B's text may be issued")
	assert.Same(t, ownResult, res, "A's own successful result object must be returned unchanged, not replaced by a retry's result")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	for _, m := range msgs {
		if m.Role == message.User {
			assert.False(t, IsContinuationPrompt(m.Content().Text), "no continuation prompt may be persisted")
		}
	}
}

// TestRunInternal_TransientContinuationUsesOwnPartialNotNewerForeignRow
// covers the transient-failure side of the same ownership mechanism:
// A's attempt fails transiently AFTER writing its own partial progress,
// while a concurrent caller B's unrelated stalled message lands later and
// is the session's newest assistant row at classification time. The
// continuation prompt must be built from A's OWN partial text, never B's.
func TestRunInternal_TransientContinuationUsesOwnPartialNotNewerForeignRow(t *testing.T) {
	const providerID = "test-own-partial"
	const prompt = "A's flaky prompt"

	orig := streamStallRetryBaseBackoff
	streamStallRetryBaseBackoff = time.Millisecond
	t.Cleanup(func() { streamStallRetryBaseBackoff = orig })

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	providerCfg := config.ProviderConfig{
		ID:   providerID,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096},
		},
	}
	cfg.Config().Providers.Set(providerID, providerCfg)
	sel := config.SelectedModel{Provider: providerID, Model: "test-model"}
	cfg.Config().Models[config.SelectedModelTypeSmart] = sel
	cfg.Config().Models[config.SelectedModelTypeFast] = sel

	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}

	sess, err := env.sessions.Create(t.Context(), "own-partial-test")
	require.NoError(t, err)

	var continuationPromptSeen string
	callCount := 0
	agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		callCount++
		if callCount == 1 {
			// A's own attempt: user message, then its OWN stalled partial
			// row with real progress, reported via the callbacks.
			userMsg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role:  message.User,
				Parts: []message.ContentPart{message.TextContent{Text: prompt}},
			})
			require.NoError(t, err)
			require.NotNil(t, call.OnUserMessageCreated)
			call.OnUserMessageCreated(userMsg.ID)
			ownRow, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "A's own partial progress"},
					message.Finish{Reason: message.FinishReasonError, Message: streamStalledFinishTitle},
				},
			})
			require.NoError(t, err)
			require.NotNil(t, call.OnAssistantMessageCreated)
			call.OnAssistantMessageCreated(ownRow.ID)
			// B's unrelated stalled turn commits AFTER A's attempt, so
			// B's row is the session's newest at classification time.
			_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "B's unrelated stalled text"},
					message.Finish{Reason: message.FinishReasonError, Message: streamStalledFinishTitle},
				},
			})
			require.NoError(t, err)
			return nil, context.Canceled // watchdog stalls surface as context.Canceled
		}
		// Continuation attempt: its prompt must quote A's OWN partial
		// text and never B's.
		continuationPromptSeen = call.Prompt
		assert.Empty(t, call.ExistingMessageID, "continuation must create a fresh user message")
		row2, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "recovered"},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, call.OnAssistantMessageCreated)
		call.OnAssistantMessageCreated(row2.ID)
		return agentResultWithText("recovered"), nil
	})
	coord.currentAgent = agent

	pinned, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)

	res, err := coord.runInternal(t.Context(), sess.ID, prompt, pinned)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 2, callCount, "exactly one continuation attempt expected")
	assert.Contains(t, continuationPromptSeen, "A's own partial progress", "the continuation must quote the attempt's OWN partial text")
	assert.NotContains(t, continuationPromptSeen, "B's unrelated stalled text", "the continuation must never quote a foreign caller's message")

	// B's unrelated row must survive untouched in history.
	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	foundB := false
	for _, m := range msgs {
		if m.Role == message.Assistant && m.FullText() == "B's unrelated stalled text" {
			foundB = true
		}
	}
	assert.True(t, foundB, "caller B's message must remain in history untouched")
}

// TestRunInternal_QueuedAttemptNotRetriedOnStaleEvidence reproduces the
// R3-1 round-6 residual (mechanism A) end-to-end: attempt 1 stalls after
// real partial progress (a legitimate continuation retry); attempt 2 is
// the exact shape agent_run.go's Run produces for a QUEUED admission
// (mailbox busy, !FailIfSessionBusy): it returns (nil, nil) immediately
// WITHOUT running PrepareStep, so OnAssistantMessageCreated never fires
// for it. Before the fix, the shared attemptAssistantMsgID still held
// attempt 1's row ID, so the next classification treated attempt 1's
// stalled partial as attempt 2's evidence, rewrote the prompt into
// another continuation, and enqueued a SECOND queued copy behind the
// first -- an unauthorized duplicate continuation execution. With
// per-attempt evidence the queued attempt has none (its capture target
// stays empty), both classifiers refuse, and the loop ends returning
// (nil, nil) -- a queued-and-abandoned call has nothing to report: the
// caller that got queued is not the one who sees the eventual dispatch
// result.
func TestRunInternal_QueuedAttemptNotRetriedOnStaleEvidence(t *testing.T) {
	const providerID = "test-queued-stale"
	const prompt = "A's prompt that gets queued"

	orig := streamStallRetryBaseBackoff
	streamStallRetryBaseBackoff = time.Millisecond
	t.Cleanup(func() { streamStallRetryBaseBackoff = orig })

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	providerCfg := config.ProviderConfig{
		ID:   providerID,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096},
		},
	}
	cfg.Config().Providers.Set(providerID, providerCfg)
	sel := config.SelectedModel{Provider: providerID, Model: "test-model"}
	cfg.Config().Models[config.SelectedModelTypeSmart] = sel
	cfg.Config().Models[config.SelectedModelTypeFast] = sel

	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}

	sess, err := env.sessions.Create(t.Context(), "queued-stale-evidence")
	require.NoError(t, err)

	callCount := 0
	var seenPrompts []string
	agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		callCount++
		seenPrompts = append(seenPrompts, call.Prompt)
		if callCount == 1 {
			// Attempt 1: a real watchdog stall after real progress.
			userMsg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role:  message.User,
				Parts: []message.ContentPart{message.TextContent{Text: prompt}},
			})
			require.NoError(t, err)
			require.NotNil(t, call.OnUserMessageCreated)
			call.OnUserMessageCreated(userMsg.ID)
			partialRow, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "A's stalled partial answer"},
					message.Finish{Reason: message.FinishReasonError, Message: streamStalledFinishTitle},
				},
			})
			require.NoError(t, err)
			require.NotNil(t, call.OnAssistantMessageCreated)
			call.OnAssistantMessageCreated(partialRow.ID)
			return nil, context.Canceled // watchdog stalls surface as context.Canceled
		}
		// Attempt 2: the queued-admission shape -- Run returned (nil,
		// nil) without ever starting the turn: no rows, no callbacks.
		// (The prompt is the legitimate continuation of attempt 1's OWN
		// partial; a spurious attempt 3 would show up here as callCount
		// >= 3 with another continuation prompt.)
		require.Contains(t, call.Prompt, "A's stalled partial answer")
		return nil, nil
	})
	coord.currentAgent = agent

	pinned, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)

	res, err := coord.runInternal(t.Context(), sess.ID, prompt, pinned)
	require.NoError(t, err, "a queued-and-abandoned call has nothing to report")
	assert.Nil(t, res)
	assert.Equal(t, 2, callCount, "a queued attempt must not be re-enqueued off attempt 1's stale evidence -- no attempt 3")
	require.Len(t, seenPrompts, 2)
	assert.Equal(t, prompt, seenPrompts[0], "attempt 1 sends the original prompt")

	// Attempt 1's partial row must be untouched by the abandoned retry.
	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var lastAssistantText string
	for _, m := range msgs {
		if m.Role == message.Assistant {
			lastAssistantText = m.FullText()
		}
	}
	assert.Equal(t, "A's stalled partial answer", lastAssistantText, "the partial row attempt 1 wrote must remain the last assistant row, untouched")
}
