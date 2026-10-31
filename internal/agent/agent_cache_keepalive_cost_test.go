package agent

// Cache keep-alive replays under cost and cancellation pressure: the max-cost
// checks, the charge accounting, the cancel-versus-rearm races, and the
// guarantees that a replay never executes a tool. Split out of
// agent_cache_keepalive_test.go when the 1000-line file limit landed; the
// scheduling and timer tests stay there.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestFireCacheKeepAlive_CancelDuringMaxCostCheckIsHonored proves the fix for
// a real race (2026-08-26 review): before the fix, fireCacheKeepAlive deleted
// the pending entry and released cacheKeepAliveMu, then ran the maxCost
// check + tryAdmitRunWg + ctx construction, and only THEN registered the
// in-flight cancel — a cancelCacheKeepAlive call landing in that gap found
// neither the pending entry (already deleted) nor the in-flight cancel (not
// yet registered) and was silently lost, letting the stale replay run to
// completion unopposed and potentially rearm afterward. cacheKeepAliveFireSeam
// lands the cancel deterministically inside that exact span (now closed: the
// in-flight cancel is registered atomically with the pending-entry removal,
// before the seam fires) and asserts the cancel is actually honored.
func TestFireCacheKeepAlive_CancelDuringMaxCostCheckIsHonored(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 3)
	a, _ := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	lm := &blockingKeepAliveModel{provider: anthropic.Name, started: make(chan struct{})}
	model := testKeepAliveModel(Model{Model: lm})

	seamHit := make(chan struct{})
	proceed := make(chan struct{})
	cacheKeepAliveFireSeam = func() { close(seamHit); <-proceed }
	t.Cleanup(func() { cacheKeepAliveFireSeam = nil })

	sessionID := "sess-cancel-race"
	a.scheduleCacheKeepAlive(sessionID, model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)

	select {
	case <-seamHit:
	case <-time.After(2 * time.Second):
		t.Fatal("fire never reached the seam")
	}

	// Land the cancel while the fire is PAUSED at the seam — the exact span
	// that used to be an unregistered gap before this fix — then let it
	// proceed. Blocking (not fire-and-forget) is required: without it, the
	// production goroutine's own scheduling could complete its registration
	// before this goroutine is even scheduled, closing the window before the
	// cancel has any chance to land inside it.
	a.cancelCacheKeepAlive(sessionID)
	close(proceed)

	select {
	case <-lm.started:
	case <-time.After(2 * time.Second):
		t.Fatal("replay never reached Stream")
	}

	// The in-flight entry is cleared only by fireCacheKeepAlive's deferred
	// cleanup, which runs once Stream has actually RETURNED — not merely been
	// entered. This must happen promptly (the ctx was already cancelled)
	// rather than after the full 30s cacheKeepAliveCallTimeout: a test that
	// only checked "Stream was entered" (lm.calls>=1, true the instant Stream
	// starts regardless of whether its ctx is ever cancelled) would pass even
	// when the call is still hanging in the background well past this test's
	// own return — this check instead demands the round trip completed.
	require.Eventually(t, func() bool {
		_, stillInFlight := a.cacheKeepAliveInFlight.Get(sessionID)
		return !stillInFlight
	}, 2*time.Second, 5*time.Millisecond,
		"cancelled replay must return promptly, not run out the 30s call timeout")

	// Give any incorrect rearm a chance to fire, then confirm none did.
	time.Sleep(100 * time.Millisecond)
	require.EqualValues(t, 1, lm.calls.Load(), "a cancelled fire must not rearm")
	_, stillScheduled := a.cacheKeepAlive.Get(sessionID)
	require.False(t, stillScheduled, "a cancelled fire must not leave a pending entry behind")
}

// TestFireCacheKeepAlive_RecordsReplayCost proves a successful replay's usage
// is billed to the session via IncrementCost, using the same formula
// agent_title.go's generateTitle uses (CostPer1M* x usage fields).
func TestFireCacheKeepAlive_RecordsReplayCost(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 1)
	a, env := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	sess, err := env.sessions.Create(t.Context(), "keepalive cost test")
	require.NoError(t, err)

	lm := &usageKeepAliveModel{
		provider: anthropic.Name,
		usage:    fantasy.Usage{InputTokens: 1000, OutputTokens: 1, CacheReadTokens: 2000, CacheCreationTokens: 500},
	}
	model := testKeepAliveModel(Model{Model: lm})
	model.CatwalkCfg.CostPer1MIn = 3.0
	model.CatwalkCfg.CostPer1MOut = 15.0
	model.CatwalkCfg.CostPer1MInCached = 6.0
	model.CatwalkCfg.CostPer1MOutCached = 0.30

	expectedCost := model.CatwalkCfg.CostPer1MInCached/1e6*float64(lm.usage.CacheCreationTokens) +
		model.CatwalkCfg.CostPer1MOutCached/1e6*float64(lm.usage.CacheReadTokens) +
		model.CatwalkCfg.CostPer1MIn/1e6*float64(lm.usage.InputTokens) +
		model.CatwalkCfg.CostPer1MOut/1e6*float64(lm.usage.OutputTokens)
	require.Greater(t, expectedCost, 0.0, "test setup sanity: expected cost must be nonzero")

	a.scheduleCacheKeepAlive(sess.ID, model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)

	require.Eventually(t, func() bool {
		return lm.calls.Load() >= 1
	}, 2*time.Second, 5*time.Millisecond, "expected the replay to fire")

	require.Eventually(t, func() bool {
		updated, err := env.sessions.Get(t.Context(), sess.ID)
		require.NoError(t, err)
		return updated.OwnCost > 0
	}, 2*time.Second, 5*time.Millisecond, "expected replay cost to be recorded on the session")

	updated, err := env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.InDelta(t, expectedCost, updated.OwnCost, 1e-9, "recorded cost must match the usage-derived formula")

	// PromptTokens/CompletionTokens are a session-level SNAPSHOT the main
	// turn owns (see recordCacheKeepAliveCost's doc) — a replay must never
	// touch them.
	require.Zero(t, updated.PromptTokens, "replay must not touch the PromptTokens snapshot")
	require.Zero(t, updated.CompletionTokens, "replay must not touch the CompletionTokens snapshot")
}

func TestFireCacheKeepAlive_SkipsDuringProviderPeakHours(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 3)
	a, env := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	const providerID = anthropic.Name
	w := peakWindowAroundNow()
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(providerID, config.ProviderConfig{ID: providerID, PeakHours: w})
	a.coord = &coordinator{cfg: cfg}

	lm := &keepAliveCountingModel{provider: providerID}
	model := testKeepAliveModel(Model{Model: lm})
	sess, err := env.sessions.Create(t.Context(), "keepalive peak-hours test")
	require.NoError(t, err)
	a.scheduleCacheKeepAlive(sess.ID, model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)

	// The short timer deterministically reaches checkLivePeakHours; an
	// in-window configured provider must be refused before Stream is called.
	require.Eventually(t, func() bool {
		_, pending := a.cacheKeepAlive.Get(sess.ID)
		_, inFlight := a.cacheKeepAliveInFlight.Get(sess.ID)
		return !pending && !inFlight
	}, 2*time.Second, 5*time.Millisecond, "peak-hours refusal should consume the scheduled replay")
	require.Zero(t, lm.calls.Load(), "keepalive request must be skipped inside provider peak hours")
}

// TestFireCacheKeepAlive_SkipsWhenSessionAtMaxCost proves a session already
// at or over its MaxCost cap gets no replay call at all, and is not
// rescheduled — mirroring agent_turn.go's own max-cost abort being terminal.
func TestFireCacheKeepAlive_SkipsWhenSessionAtMaxCost(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 3)
	a, env := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	sess, err := env.sessions.Create(t.Context(), "keepalive max-cost test")
	require.NoError(t, err)
	_, err = env.sessions.IncrementCost(t.Context(), sess.ID, 5.0)
	require.NoError(t, err)

	lm := &keepAliveCountingModel{provider: anthropic.Name}
	model := testKeepAliveModel(Model{Model: lm})

	a.scheduleCacheKeepAlive(sess.ID, model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 5.0)

	// Give the shrunk interval plenty of time to fire and settle.
	time.Sleep(200 * time.Millisecond)
	require.Zero(t, lm.calls.Load(), "session at max cost must never get a replay Stream call")

	_, stillScheduled := a.cacheKeepAlive.Get(sess.ID)
	require.False(t, stillScheduled, "a cost-capped skip must not reschedule")

	updated, err := env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.InDelta(t, 5.0, updated.OwnCost, 1e-9, "cost must remain unchanged, no replay spend")
}

// TestFireCacheKeepAlive_RecordsReplayCostEvenWhenItCrossesMaxCost pins the
// accounting rule: once a paid response succeeds, a concurrent spend crossing
// maxCost does not erase the replay's incurred cost; the cap only prevents
// subsequent replays. Revert-check: restoring IncrementCostIfUnderMax/return
// on !charged leaves OwnCost at maxCost and fails this test.
func TestFireCacheKeepAlive_RecordsReplayCostEvenWhenItCrossesMaxCost(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 3)
	a, env := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	sess, err := env.sessions.Create(t.Context(), "keepalive max-cost mid-flight test")
	require.NoError(t, err)

	lm := &blockingUsageKeepAliveModel{
		provider: anthropic.Name,
		usage:    fantasy.Usage{InputTokens: 1000, OutputTokens: 1},
		started:  make(chan struct{}),
		proceed:  make(chan struct{}),
	}
	model := testKeepAliveModel(Model{Model: lm})
	model.CatwalkCfg.CostPer1MIn = 1.0

	const maxCost = 5.0
	a.scheduleCacheKeepAlive(sess.ID, model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, maxCost)

	select {
	case <-lm.started:
	case <-time.After(2 * time.Second):
		t.Fatal("replay never reached Stream")
	}

	// The replay is now blocked in Stream, past the up-front maxCost check
	// (which passed: the session started at 0 cost). Simulate a concurrent
	// real turn pushing the session over the cap WHILE the replay is in
	// flight.
	_, err = env.sessions.IncrementCost(t.Context(), sess.ID, maxCost)
	require.NoError(t, err)

	// A rearmed entry must outlive the assertion below: with the short test
	// interval it would fire, hit the up-front cap check and delete itself
	// before the check, hiding a missing post-charge cap guard. The write is
	// ordered before the replay goroutine's read by close(lm.proceed).
	cacheKeepAliveInterval = time.Hour

	close(lm.proceed)

	// The successful response was already paid for, so its replay cost is
	// recorded even though the concurrent charge pushed the session over cap.
	// It must still not rearm: the max-cost guard prevents future requests.
	require.Eventually(t, func() bool {
		_, stillInFlight := a.cacheKeepAliveInFlight.Get(sess.ID)
		return !stillInFlight
	}, 2*time.Second, 5*time.Millisecond, "replay must complete promptly")

	time.Sleep(100 * time.Millisecond)
	_, stillScheduled := a.cacheKeepAlive.Get(sess.ID)
	require.False(t, stillScheduled, "a mid-flight max-cost crossing must not rearm")

	updated, err := env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	// A response that crosses the cap is still already-paid provider usage;
	// record it in the ledger even though the cap blocks future replays.
	require.InDelta(t, maxCost+0.001, updated.OwnCost, 1e-9,
		"a paid replay response must be accounted even when it crosses max cost")
}

// TestCancelCacheKeepAlive_CancelsInFlightReplay proves an in-flight replay
// (already past admission, blocked inside Stream) is cut off promptly by
// cancelCacheKeepAlive instead of running out the full 30s
// cacheKeepAliveCallTimeout. Run with -race -count=10+.
func TestCancelCacheKeepAlive_CancelsInFlightReplay(t *testing.T) {
	restoreKeepAliveVars(t, 15*time.Millisecond, 3)
	a, _ := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	lm := &blockingKeepAliveModel{provider: anthropic.Name, started: make(chan struct{})}
	model := testKeepAliveModel(Model{Model: lm})

	sessionID := "sess-inflight-cancel"
	a.scheduleCacheKeepAlive(sessionID, model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)

	select {
	case <-lm.started:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the replay's Stream call to start")
	}

	// The replay is now blocked inside Stream, in flight. cancelCacheKeepAlive
	// must cut it off promptly rather than waiting out the 30s call timeout.
	start := time.Now()
	a.cancelCacheKeepAlive(sessionID)

	require.Eventually(t, func() bool {
		return lm.calls.Load() == 1
	}, 5*time.Second, 5*time.Millisecond)
	elapsed := time.Since(start)
	require.Less(t, elapsed, 5*time.Second,
		"in-flight replay must be cancelled promptly, not run out cacheKeepAliveCallTimeout (30s)")

	_, stillInFlight := a.cacheKeepAliveInFlight.Get(sessionID)
	require.False(t, stillInFlight, "in-flight entry must be cleared after the call returns")
}

// TestCancelAll_CancelsInFlightReplay is TestCancelCacheKeepAlive_CancelsInFlightReplay's
// CancelAll counterpart: CancelAll's own shutdown sweep must also reach and
// cancel a replay call already in flight, within its existing grace period.
func TestCancelAll_CancelsInFlightReplay(t *testing.T) {
	restoreKeepAliveVars(t, 15*time.Millisecond, 3)
	a, _ := newKeepAliveAgent(t)

	lm := &blockingKeepAliveModel{provider: anthropic.Name, started: make(chan struct{})}
	model := testKeepAliveModel(Model{Model: lm})

	sessionID := "sess-inflight-cancelall"
	a.scheduleCacheKeepAlive(sessionID, model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)

	select {
	case <-lm.started:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the replay's Stream call to start")
	}

	start := time.Now()
	stillBusy := a.CancelAll()
	elapsed := time.Since(start)

	require.False(t, stillBusy, "CancelAll must observe the in-flight replay exit within its grace period")
	require.Equal(t, int64(1), lm.calls.Load())
	require.Less(t, elapsed, 5*time.Second,
		"CancelAll must cancel the in-flight replay promptly rather than waiting out its own grace period")
}

// toolCallingKeepAliveModel is a mockModel whose Stream always returns a
// single StreamPartTypeToolCall (for the tool named by toolName, with a
// trivial "{}" input) followed by a finish part with FinishReasonToolCalls —
// simulating a provider that ignores ToolChoiceNone and selects a tool
// anyway. Used to prove the replay cannot get that tool call to actually
// execute (task #776).
type toolCallingKeepAliveModel struct {
	mockModel
	provider string
	toolName string
	calls    atomic.Int64

	mu        sync.Mutex
	callsSeen []fantasy.Call
}

func (m *toolCallingKeepAliveModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.callsSeen = append(m.callsSeen, call)
	m.mu.Unlock()
	toolName := m.toolName
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{
			Type:          fantasy.StreamPartTypeToolCall,
			ID:            "call-1",
			ToolCallName:  toolName,
			ToolCallInput: "{}",
		}) {
			return
		}
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonToolCalls,
			Usage:        fantasy.Usage{InputTokens: 1, OutputTokens: 1},
		})
	}, nil
}

func (m *toolCallingKeepAliveModel) Provider() string { return m.provider }

func (m *toolCallingKeepAliveModel) lastCall() fantasy.Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.callsSeen) == 0 {
		return fantasy.Call{}
	}
	return m.callsSeen[len(m.callsSeen)-1]
}

// TestFireCacheKeepAlive_SetsToolChoiceNone proves the replay's outgoing
// fantasy.Call carries ToolChoice == ToolChoiceNone — the request-level
// guard task #776 requires. Pins the parameter directly, independent of
// whether the model actually honors it.
func TestFireCacheKeepAlive_SetsToolChoiceNone(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 1)
	a, _ := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	echoTool := fantasy.NewAgentTool("echo", "echoes input",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.ToolResponse{}, nil
		})

	lm := &keepAliveCountingModel{provider: anthropic.Name}
	model := testKeepAliveModel(Model{Model: lm})

	a.scheduleCacheKeepAlive("sess-toolchoice", model,
		[]fantasy.Message{fantasy.NewUserMessage("hi")},
		[]fantasy.AgentTool{echoTool}, nil, 0)

	require.Eventually(t, func() bool {
		return lm.calls.Load() >= 1
	}, 2*time.Second, 5*time.Millisecond, "expected the replay to fire")

	replayCall := lm.lastCall()
	require.NotNil(t, replayCall.ToolChoice, "replay call must set an explicit ToolChoice")
	require.Equal(t, fantasy.ToolChoiceNone, *replayCall.ToolChoice,
		"replay call must set ToolChoice=none so the provider is told not to select a tool")

	// The tool DEFINITION must still be present — dropping it would itself
	// invalidate the prompt-cache prefix the keep-alive exists to preserve.
	require.Equal(t, []string{"echo"}, keepAliveToolNames(replayCall),
		"ToolChoiceNone must not come at the cost of dropping the tool definitions")
}

// TestFireCacheKeepAlive_ToolCallNeverExecutes is the defect-pinning test:
// even when the mock model RETURNS a tool call (simulating a provider that
// ignores ToolChoiceNone), the replay's belt-and-braces noExecuteTools
// wrapper must stop it from actually running. Before the fix, this tool's
// real Run function executed for real inside a detached background replay —
// a P1 (Bash/edit/write/MCP could all fire unattended).
func TestFireCacheKeepAlive_ToolCallNeverExecutes(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 1)
	a, _ := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	var executed atomic.Bool
	spyTool := fantasy.NewAgentTool("dangerous", "flips a flag if it actually runs",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			executed.Store(true)
			return fantasy.NewTextResponse("ran"), nil
		})

	lm := &toolCallingKeepAliveModel{provider: anthropic.Name, toolName: "dangerous"}
	model := testKeepAliveModel(Model{Model: lm})

	a.scheduleCacheKeepAlive("sess-toolexec", model,
		[]fantasy.Message{fantasy.NewUserMessage("hi")},
		[]fantasy.AgentTool{spyTool}, nil, 0)

	require.Eventually(t, func() bool {
		return lm.calls.Load() >= 1
	}, 2*time.Second, 5*time.Millisecond, "expected the replay to fire")

	// Give the agent's internal step/tool-execution loop time to run if it
	// were going to — Stream having been called is not enough; the step loop
	// processes the yielded tool_call part asynchronously to this goroutine.
	time.Sleep(150 * time.Millisecond)

	require.False(t, executed.Load(),
		"the spy tool must NEVER execute from a cache keep-alive replay, even when the model returns a tool call")
}

// TestFireCacheKeepAlive_CancelBetweenErrCheckAndRearmDoesNotRearm pins the K-1
// re-arm race (2026-08-26 review): between fireCacheKeepAlive's `if ctx.Err() != nil`
// early-out and its rearm critical section, a cancelCacheKeepAlive landing in that
// window must still stop the rearm. The production fix added a BLOCKING test seam
// `cacheKeepAliveRearmSeam func()` called exactly in that window (after the
// ctx.Err() check, before the lock). This test uses the documented `close(signal);
// <-proceed` blocking pattern — a fire-and-forget seam produces a vacuous test here.
//
// The ctx.Err() check runs before the lock, so a cancelCacheKeepAlive landing
// between that check and the rearm critical section used to be invisible — the
// pending map was empty at that point (the fire Del'd its entry at start), the
// generation guard only rejects when an entry EXISTS with a different generation,
// and the stale fire re-armed a timer for a just-cancelled session, letting a stale
// replay later run on top of a NEW turn. The fix makes the in-flight registration
// the tombstone: cancelCacheKeepAlive consumes it under cacheKeepAliveMu, and the
// rearm critical section requires the registration to still be present AND still
// owned by this fire's generation.
func TestFireCacheKeepAlive_CancelBetweenErrCheckAndRearmDoesNotRearm(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 3)
	a, _ := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	lm := &usageKeepAliveModel{
		provider: anthropic.Name,
		usage:    fantasy.Usage{InputTokens: 1, OutputTokens: 1},
	}
	model := testKeepAliveModel(Model{Model: lm})

	rearmHit := make(chan struct{})
	rearmProceed := make(chan struct{})
	var closeOnce sync.Once
	cacheKeepAliveRearmSeam = func() { closeOnce.Do(func() { close(rearmHit) }); <-rearmProceed }
	t.Cleanup(func() { cacheKeepAliveRearmSeam = nil })

	sessionID := "sess-k1-rearm-race"
	a.scheduleCacheKeepAlive(sessionID, model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)

	select {
	case <-rearmHit:
	case <-time.After(2 * time.Second):
		t.Fatal("fire never reached the rearm seam")
	}

	// While the fire is BLOCKED at the seam (i.e. after the ctx.Err() check
	// already passed with ctx not yet cancelled), land the cancel — this takes
	// the in-flight registration (the K-1 tombstone) and cancels the ctx,
	// exactly the interleaving the old code could not see.
	a.cancelCacheKeepAlive(sessionID)
	close(rearmProceed)

	// Assert the fire finished: the deferred release must have cleared the
	// in-flight entry.
	require.Eventually(t, func() bool {
		_, stillInFlight := a.cacheKeepAliveInFlight.Get(sessionID)
		return !stillInFlight
	}, 2*time.Second, 5*time.Millisecond,
		"fire's deferred release must have cleared the in-flight entry")

	// Sleep well past the 20ms rearm interval, giving an incorrect rearm every
	// chance to fire a second replay.
	time.Sleep(150 * time.Millisecond)
	require.EqualValues(t, 1, lm.calls.Load(),
		"a cancel landing between the ctx.Err() check and the rearm critical section must stop the rearm")

	_, scheduled := a.cacheKeepAlive.Get(sessionID)
	require.False(t, scheduled,
		"the stale fire must not leave a re-armed pending entry behind")
}

// TestFireCacheKeepAlive_ChargeRecordedWhenDeltaCrossesMaxCost pins the K-2
// accounting rule at the wiring level: the replay's preflight check may pass
// while the session is below maxCost, but once the provider has returned a paid
// response, that cost must be recorded even when it crosses the cap. The cap
// prevents future replays; it cannot erase the cost already incurred.
func TestFireCacheKeepAlive_ChargeRecordedWhenDeltaCrossesMaxCost(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 1)
	a, env := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	sess, err := env.sessions.Create(t.Context(), "keepalive k2 overshoot test")
	require.NoError(t, err)

	// Pre-charge to leave headroom smaller than the replay's own cost.
	_, err = env.sessions.IncrementCost(t.Context(), sess.ID, 0.09)
	require.NoError(t, err)

	lm := &usageKeepAliveModel{
		provider: anthropic.Name,
		usage:    fantasy.Usage{InputTokens: 500, OutputTokens: 0},
	}
	model := testKeepAliveModel(Model{Model: lm})
	model.CatwalkCfg.CostPer1MIn = 100.0 // replay cost = 100/1e6*500 = 0.05

	const maxCost = 0.10 // up-front check passes (0.09 < 0.10); the paid response still gets charged across the cap

	a.scheduleCacheKeepAlive(sess.ID, model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, maxCost)

	require.Eventually(t, func() bool {
		return lm.calls.Load() >= 1
	}, 2*time.Second, 5*time.Millisecond, "expected the replay to fire")

	time.Sleep(200 * time.Millisecond) // let any incorrect charge/rearm settle

	updated, err := env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.InDelta(t, 0.14, updated.OwnCost, 1e-9,
		"the paid replay response must be accounted even when its cost crosses maxCost")

	require.EqualValues(t, 1, lm.calls.Load(),
		"the cap is checked after accounting, so a crossing charge must stop rearming")

	_, scheduled := a.cacheKeepAlive.Get(sess.ID)
	require.False(t, scheduled)
}

// TestFireCacheKeepAlive_ChargeFailureStopsRearm pins K-3 (2026-08-26 review):
// when the cost charge errors (DB outage), recordCacheKeepAliveCost must return
// false so the caller stops re-arming; the old code logged the error and returned
// true, yielding repeated unbilled replays under DB trouble.
//
// This test simulates a DB outage by closing env.conn (fakeEnv exposes the raw
// *sql.DB precisely so tests can do this — see common_test.go's comment). The
// close happens BEFORE scheduling. Note testEnv's own cleanup double-closes conn,
// which is safe (sql.DB.Close is idempotent).
func TestFireCacheKeepAlive_ChargeFailureStopsRearm(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 3)
	a, env := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	sess, err := env.sessions.Create(t.Context(), "keepalive k3 charge-failure test")
	require.NoError(t, err)

	lm := &usageKeepAliveModel{
		provider: anthropic.Name,
		usage:    fantasy.Usage{InputTokens: 500},
	}
	model := testKeepAliveModel(Model{Model: lm})
	model.CatwalkCfg.CostPer1MIn = 100.0 // cost 0.05 (nonzero, so charge path runs)

	// Simulate DB outage by closing the connection BEFORE scheduling.
	env.conn.Close()

	// maxCost=0 (unlimited) so the charge goes through IncrementCost, which will
	// error on the closed DB.
	a.scheduleCacheKeepAlive(sess.ID, model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)

	require.Eventually(t, func() bool {
		return lm.calls.Load() >= 1
	}, 2*time.Second, 5*time.Millisecond, "expected the replay to fire")

	time.Sleep(200 * time.Millisecond) // with maxExtensions=3, incorrect rearm would fire up to 2 more replays

	require.EqualValues(t, 1, lm.calls.Load(),
		"a failed charge must stop rearming — no repeated unbilled replays under DB trouble (K-3)")

	_, scheduled := a.cacheKeepAlive.Get(sess.ID)
	require.False(t, scheduled,
		"a failed charge must not leave a re-armed entry behind")
}
