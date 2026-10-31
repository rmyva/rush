// Cache keep-alive: after a turn that wrote to the provider's ephemeral
// prompt cache, schedule a lightweight detached "replay" request shortly
// before the cache TTL expires, to extend it. Bounded by
// cacheKeepAliveMaxExtensions so a session cannot keep paying for this
// forever.
package agent

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/heartbeat"
)

// cacheKeepAliveDefaultInterval is the fallback used when a keep-alive-
// eligible provider's profile has no TTL (ttl == 0) — unreachable today
// since every keepAliveEligible profile sets ttl=5m, kept as a safety net.
const cacheKeepAliveDefaultInterval = 4*time.Minute + 45*time.Second

// cacheKeepAliveTTLMargin is how long before a provider's cache TTL expires
// we fire the replay.
const cacheKeepAliveTTLMargin = 15 * time.Second

// cacheKeepAliveInterval, when nonzero, overrides the per-provider TTL-derived
// interval — the package's test-seam idiom, so tests can shrink it instead of
// sleeping through the real delay. Zero (the default) means "derive from the
// triggering provider's cache profile TTL", via cacheKeepAliveIntervalFor.
var cacheKeepAliveInterval time.Duration

// cacheKeepAliveIntervalFor returns how long after a cache-writing turn to
// fire a keep-alive replay for provider. Respects the cacheKeepAliveInterval
// test-seam override when set; otherwise derives from the provider's cache
// profile TTL (falling back to cacheKeepAliveDefaultInterval if the profile
// has no TTL).
func cacheKeepAliveIntervalFor(provider string) time.Duration {
	if cacheKeepAliveInterval != 0 {
		return cacheKeepAliveInterval
	}
	ttl := cacheProfileFor(provider).ttl
	if ttl == 0 {
		return cacheKeepAliveDefaultInterval
	}
	return ttl - cacheKeepAliveTTLMargin
}

// cacheKeepAliveMaxExtensions caps how many times a single idle period can
// be extended (~15 minutes total) so a forgotten session cannot keep firing
// paid replay requests indefinitely.
var cacheKeepAliveMaxExtensions = 3

// cacheKeepAliveCallTimeout bounds each detached replay call. Independent of
// any turn's context — it must survive the triggering turn having long
// since ended.
var cacheKeepAliveCallTimeout = 30 * time.Second

// cacheKeepAliveFireSeam is a test-only hook — see its call site in
// fireCacheKeepAlive. nil (a no-op) in every production path.
var cacheKeepAliveFireSeam func()

// cacheKeepAliveRearmSeam is a test-only hook — see its call site in
// fireCacheKeepAlive's rearm decision, between the ctx.Err() check and the
// rearm critical section. nil (a no-op) in every production path.
var cacheKeepAliveRearmSeam func()

// cacheKeepAliveEntry is the pending-timer state for one session. It holds
// only what the generation-guard compare-and-act sequences actually read
// back (timer, generation) plus the extension counter — messages, tools,
// providerOptions, and maxCost are NOT duplicated here: the armed
// time.AfterFunc closure already captures everything fireCacheKeepAlive
// needs to run, so storing them a second time on this struct would just be
// redundant memory held per idle session (messages in particular can be a
// full conversation history).
type cacheKeepAliveEntry struct {
	timer      *time.Timer
	extension  int
	generation int64
}

// cacheKeepAliveInFlightEntry is the in-flight registration for one replay
// call currently executing inside fireCacheKeepAlive: its cancel func plus
// the generation of the fire that owns it. The generation makes ownership
// testable under cacheKeepAliveMu — "the entry is still mine" is what the
// rearm decision (K-1) and the deferred release both check, so a consumed
// (cancelled) or replaced (superseded by a newer fire) registration is
// distinguishable from "still mine", and a finishing fire can never drop a
// NEWER fire's registration with a blind Del.
type cacheKeepAliveInFlightEntry struct {
	generation int64
	cancel     context.CancelFunc
}

// cacheKeepAliveExplicitCacheProvider reports whether provider is one of the
// explicit prompt-cache providers getCacheControlOptions marks up. Mirrors
// that switch exactly — an implicit-cache provider gets zero benefit from a
// replay, so scheduling one would just be a wasted paid request.
func cacheKeepAliveExplicitCacheProvider(provider string) bool {
	return cacheProfileFor(provider).keepAliveEligible
}

// scheduleCacheKeepAlive arms (or re-arms) a keep-alive timer for sessionID
// after a turn observed CacheCreationTokens > 0. A fresh cache write resets
// any existing timer and its extension counter — a genuine new turn already
// re-warmed the cache on its own.
//
// tools and providerOptions are the exact values the triggering turn passed
// to fantasy.NewAgent/AgentStreamCall, so the replay can reproduce the same
// request shape. systemPrompt is NOT re-applied via fantasy.WithSystemPrompt
// on the replay: fantasy's createPrompt already folds WithSystemPrompt into
// the messages it hands PrepareStep as a MessageRoleSystem entry, and
// stepMessages (what scheduleCacheKeepAlive is called with) is cloned from
// exactly those already-prepared messages — re-adding WithSystemPrompt would
// duplicate the system message in the replay's prompt.
func (a *sessionAgent) scheduleCacheKeepAlive(sessionID string, model Model, messages []fantasy.Message, tools []fantasy.AgentTool, providerOptions fantasy.ProviderOptions, maxCost float64) {
	a.scheduleCacheKeepAliveFromCtx(heartbeat.Context{}, sessionID, model, messages, tools, providerOptions, maxCost)
}

// scheduleCacheKeepAliveFromCtx carries the triggering turn's attribution into
// the detached replay (the fire's own ctx is built from context.Background()).
func (a *sessionAgent) scheduleCacheKeepAliveFromCtx(hb heartbeat.Context, sessionID string, model Model, messages []fantasy.Message, tools []fantasy.AgentTool, providerOptions fantasy.ProviderOptions, maxCost float64) {
	if t, _ := strconv.ParseBool(os.Getenv("RUSH_DISABLE_ANTHROPIC_CACHE")); t {
		return
	}
	// Opt-in: the replay must reproduce the exact cached request shape (task
	// #761) — off by default until that is verified in production.
	if t, _ := strconv.ParseBool(os.Getenv("RUSH_CACHE_KEEPALIVE")); !t {
		return
	}
	if !cacheKeepAliveExplicitCacheProvider(model.Model.Provider()) {
		return
	}

	a.cacheKeepAliveMu.Lock()
	defer a.cacheKeepAliveMu.Unlock()

	if old, ok := a.cacheKeepAlive.Take(sessionID); ok {
		old.timer.Stop()
	}

	gen := a.cacheKeepAliveGen.Add(1)
	entry := &cacheKeepAliveEntry{generation: gen}
	entry.timer = time.AfterFunc(cacheKeepAliveIntervalFor(model.Model.Provider()), func() {
		a.fireCacheKeepAliveHB(hb, sessionID, model, messages, tools, providerOptions, 0, gen, maxCost)
	})
	a.cacheKeepAlive.Set(sessionID, entry)
}

// noExecuteToolCallRefused is returned by every noExecuteTool in place of
// actually running — a provider that ignores ToolChoiceNone (or a step that
// otherwise slips a tool_call through) still gets a normal tool-result turn,
// not a panic or a hang, so the replay just finishes as a no-op instead of
// executing anything.
const noExecuteToolCallRefused = "cache keep-alive replay: tool execution is disabled for this request"

// noExecuteTool wraps an AgentTool so Info()/ProviderOptions() (the parts
// that must byte-match the triggering turn's tools for the provider cache to
// recognize the same prefix) pass through unchanged, but Run() refuses
// unconditionally. See its call site in fireCacheKeepAlive for why this
// exists alongside ToolChoiceNone rather than instead of it.
type noExecuteTool struct {
	fantasy.AgentTool
}

func (noExecuteTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextErrorResponse(noExecuteToolCallRefused), nil
}

// noExecuteTools wraps every tool in tools with noExecuteTool. See
// noExecuteTool's doc.
func noExecuteTools(tools []fantasy.AgentTool) []fantasy.AgentTool {
	wrapped := make([]fantasy.AgentTool, len(tools))
	for i, t := range tools {
		wrapped[i] = noExecuteTool{AgentTool: t}
	}
	return wrapped
}

// fireCacheKeepAlive runs one detached replay call and, on success, reschedules
// itself until cacheKeepAliveMaxExtensions is reached. gen is the generation
// captured when this fire's timer was armed: if a newer schedule has since
// replaced the map entry (time.Timer.Stop cannot cancel an already-fired or
// already-running callback), this fire is a lost-race survivor and must not
// touch the map or send a replay — doing so could delete/clobber the newer
// entry with stale data from this, older, turn.
//
// maxCost mirrors the triggering turn's SessionAgentCall.MaxCost (agent_turn.go's
// own abort check): checked FIRST, before the tryAdmitRunWg gate, since it is
// the cheapest possible way to skip a replay that must not happen — no point
// admitting into runWg or building a replay agent for a session already at
// its cap. Unlike a failed replay, a cost-capped skip does not reschedule,
// matching a real turn's max-cost abort being terminal rather than retried.
func (a *sessionAgent) fireCacheKeepAlive(sessionID string, model Model, messages []fantasy.Message, tools []fantasy.AgentTool, providerOptions fantasy.ProviderOptions, extension int, gen int64, maxCost float64) {
	a.fireCacheKeepAliveHB(heartbeat.Context{}, sessionID, model, messages, tools, providerOptions, extension, gen, maxCost)
}

func (a *sessionAgent) fireCacheKeepAliveHB(hb heartbeat.Context, sessionID string, model Model, messages []fantasy.Message, tools []fantasy.AgentTool, providerOptions fantasy.ProviderOptions, extension int, gen int64, maxCost float64) {
	// Built before the lock so it is ready to register atomically with the
	// pending-entry removal below — see the in-flight-registration comment.
	ctx, cancel := context.WithTimeout(context.Background(), cacheKeepAliveCallTimeout)
	// Heartbeat attribution: carry the turn's root over the detached ctx (design pt. 2).
	if hb.SessionID != "" || hb.RootSessionID != "" {
		ctx = heartbeat.WithContext(ctx, hb)
	}
	ctx = withHeartbeat(ctx, sessionID, heartbeat.PurposeKeepalive, hb.Role, hb.Source)

	a.cacheKeepAliveMu.Lock()
	current, ok := a.cacheKeepAlive.Get(sessionID)
	if !ok || current.generation != gen {
		a.cacheKeepAliveMu.Unlock()
		cancel()
		return
	}
	a.cacheKeepAlive.Del(sessionID)
	// Registered in the SAME critical section as the Del above — not after
	// the maxCost check/tryAdmitRunWg/replay setup that used to follow it.
	// That gap let cancelCacheKeepAlive/CancelAll observe a window where
	// NEITHER the pending entry NOR the in-flight cancel existed for a fire
	// that had already committed to running: a cancel landing there was
	// silently lost, letting a stale replay run concurrently with (and later
	// re-arm on top of) the new turn it should have yielded to. Atomic
	// registration closes that gap — any observer taking cacheKeepAliveMu
	// after this point sees either the pending entry (not yet fired) or the
	// in-flight cancel (already committed), never neither. The registration
	// is ALSO the cancellation tombstone for the rearm decision (K-1):
	// cancelCacheKeepAlive/CancelAll consume it under this same mutex, so
	// a cancel landing anywhere before the rearm critical section is
	// observable there as "my registration is gone".
	a.cacheKeepAliveInFlight.Set(sessionID, cacheKeepAliveInFlightEntry{generation: gen, cancel: cancel})
	a.cacheKeepAliveMu.Unlock()

	// Test-only seam: fires right after the atomic registration above, before
	// the maxCost check/tryAdmitRunWg/replay call below — lets a test land a
	// concurrent cancelCacheKeepAlive deterministically inside the exact span
	// that used to be an unregistered gap. nil (a no-op) in every production
	// path.
	if cacheKeepAliveFireSeam != nil {
		cacheKeepAliveFireSeam()
	}

	defer func() {
		a.releaseCacheKeepAliveInFlight(sessionID, gen)
		cancel()
	}()

	if maxCost > 0 {
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
		budget, err := a.sessions.SubtreeBudget(checkCtx, sessionID)
		checkCancel()
		if err != nil {
			slog.Debug("cache keep-alive: failed to load session budget for max-cost check", "session_id", sessionID, "err", err)
			return
		}
		if budget >= maxCost {
			slog.Debug("cache keep-alive: skipping replay, session at or over max cost", "session_id", sessionID, "cost", budget, "max_cost", maxCost)
			return
		}
	}

	if a.coord != nil {
		if err := a.coord.checkLivePeakHours(model.Model.Provider()); err != nil {
			// Detached background work has no caller to notify; refuse quietly.
			return
		}
	}

	if !a.tryAdmitRunWg() {
		return
	}
	defer a.runWg.Done()

	// Tool DEFINITIONS must stay (WithTools(tools...)) — they are part of the
	// cached prompt prefix, and dropping them would itself invalidate the
	// cache this call exists to keep warm. But this is a background replay
	// with no operator watching: nothing must actually EXECUTE. ToolChoiceNone
	// tells the provider not to select a tool — belt only, not braces, since
	// it is a request hint the SDK does not itself enforce (fantasy's step
	// loop still calls executeSingleTool for any tool_call a provider returns
	// regardless of ToolChoice). noExecuteTools below is the braces: same
	// Info()/schema (so the cached prefix is byte-identical), Run() replaced
	// with a hard refusal, so even a provider that ignores ToolChoiceNone
	// cannot get Bash/edit/write/MCP/etc. to actually run. Do not drop either
	// half, and do not let a future refactor "simplify" this back to raw tools.
	toolChoiceNone := fantasy.ToolChoiceNone
	replayAgent := fantasy.NewAgent(
		model.Model,
		fantasy.WithTools(noExecuteTools(tools)...),
		fantasy.WithMaxOutputTokens(1),
		fantasy.WithUserAgent(userAgent),
		// fantasy's step loop has no built-in step cap: it keeps calling
		// the model again as long as the model keeps returning tool
		// calls (isStopConditionMet with an empty StopWhen never fires).
		// noExecuteTool's refusal is a NORMAL tool response (not a Go
		// error), so a provider that keeps selecting the same tool after
		// seeing the refusal drives an unbounded model<->tool ping-pong
		// this replay's own ctx timeout does not reliably interrupt
		// (confirmed empirically: a test double that always returns a
		// tool call hung fireCacheKeepAlive and its caller's runWg.Wait()
		// past ExecuteRun's watch limits, consuming multi-GB before being
		// killed). This is a single background ping with no operator
		// watching -- it never legitimately needs more than one
		// tool-calling round, so cap it firmly rather than trust the
		// model to stop on its own.
		fantasy.WithStopConditions(fantasy.StepCountIs(2)),
	)
	result, err := replayAgent.Stream(ctx, fantasy.AgentStreamCall{
		MaxRetries:      &ProviderMaxRetries,
		Messages:        messages,
		Headers:         sessionHeaders(sessionID),
		ProviderOptions: providerOptions,
		ToolChoice:      &toolChoiceNone,
	})
	if err != nil {
		slog.Debug("cache keep-alive replay failed", "session_id", sessionID, "err", err)
		return
	}

	// A successful replay has already incurred provider cost. Record it even
	// when this charge crosses maxCost; the cap prevents future calls, not
	// accounting for a response already paid for.
	if !a.recordCacheKeepAliveCost(ctx, sessionID, model, result, maxCost) {
		// The charge could not be persisted; do not rearm and risk further
		// unaccounted provider spend.
		return
	}

	if maxCost > 0 {
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
		budget, err := a.sessions.SubtreeBudget(checkCtx, sessionID)
		checkCancel()
		if err != nil || budget >= maxCost {
			// The response has been charged above, but don't schedule another
			// paid request once the subtree cap is met (or unverifiable).
			return
		}
	}

	if extension+1 >= cacheKeepAliveMaxExtensions {
		return
	}
	// Cheap early-out for a cancel that already landed: ctx.Err() is set the
	// moment cancelCacheKeepAlive invokes our cancel func, so this skips the
	// lock round trip in the common case. It is only a SUBSET of the
	// lock-held in-flight ownership check below — a cancel can still land
	// between this check and the lock (K-1), which is exactly the window the
	// ownership check exists to close; do not treat this check as sufficient
	// on its own.
	if ctx.Err() != nil {
		slog.Debug("cache keep-alive: cancelled after replay completed, not rearming", "session_id", sessionID)
		return
	}

	// Test-only seam: fires AFTER the ctx.Err() check but BEFORE the rearm
	// critical section below — the exact K-1 window where a
	// cancelCacheKeepAlive landing between the check and the lock used to be
	// invisible to the rearm decision (the pending map is empty at this
	// point, so the generation guard alone cannot see it). nil (a no-op) in
	// every production path.
	if cacheKeepAliveRearmSeam != nil {
		cacheKeepAliveRearmSeam()
	}

	a.cacheKeepAliveMu.Lock()
	defer a.cacheKeepAliveMu.Unlock()
	// K-1: cancellation must be visible to the rearm decision atomically,
	// under the same mutex the cancel path takes. This fire's in-flight
	// registration (set when the replay started, still held now) is that
	// signal: cancelCacheKeepAlive/CancelAll consume it with a Take under
	// this same mutex, and a newer fire that superseded us would have
	// replaced it with its own generation. If the entry is gone or no longer
	// ours, this fire was cancelled or superseded after the ctx.Err() check
	// above passed — rearming now would arm a stale timer on top of whatever
	// cancelled us (e.g. a brand-new turn), so stop here.
	if infl, ok := a.cacheKeepAliveInFlight.Get(sessionID); !ok || infl.generation != gen {
		return
	}
	// A concurrent schedule may have landed while the replay call was in
	// flight — only re-arm if this fire's generation is still the pending
	// map's current occupant (i.e. nothing newer has since claimed the
	// session).
	if current, ok := a.cacheKeepAlive.Get(sessionID); ok && current.generation != gen {
		return
	}
	// In-flight -> pending transition, atomically with the checks above: the
	// registration outlived the replay call precisely so the rearm decision
	// could see cancels; once the decision is made, hand the session back to
	// the pending map (the deferred releaseCacheKeepAliveInFlight becomes a
	// no-op since the entry is already gone).
	a.cacheKeepAliveInFlight.Del(sessionID)
	newGen := a.cacheKeepAliveGen.Add(1)
	entry := &cacheKeepAliveEntry{extension: extension + 1, generation: newGen}
	entry.timer = time.AfterFunc(cacheKeepAliveIntervalFor(model.Model.Provider()), func() {
		a.fireCacheKeepAliveHB(hb, sessionID, model, messages, tools, providerOptions, extension+1, newGen, maxCost)
	})
	a.cacheKeepAlive.Set(sessionID, entry)
}

// recordCacheKeepAliveCost bills a successful replay's usage to the session,
// mirroring agent_title.go's generateTitle cost accounting exactly. Only
// IncrementCost is called — NOT sessions.SetUsage/PromptTokens/CompletionTokens
// — because those columns are a snapshot of the main conversation's current
// context-window size (see updateSessionTokenCounters' doc) that the next
// real turn overwrites, not a cumulative counter. A background replay adding
// to them here would corrupt that snapshot: whichever of the replay and the
// next real turn's own usage update lands last would silently win.
//
// Returns false only when the charge itself failed (IncrementCost returned an
// error, e.g. a DB outage) — the accounting is now unreliable for this session,
// so the caller must not keep firing unbilled replays against it. A successful
// response is always charged even if it crosses maxCost: maxCost gates future
// replays, but cannot erase the cost of a provider response already received.
func (a *sessionAgent) recordCacheKeepAliveCost(ctx context.Context, sessionID string, model Model, result *fantasy.AgentResult, _ float64) bool {
	if result == nil {
		return true
	}
	usage := normalizeProviderUsage(model.Model.Provider(), result.TotalUsage)

	var openrouterCost *float64
	for _, step := range result.Steps {
		stepCost := a.openrouterCost(step.ProviderMetadata)
		if stepCost != nil {
			newCost := *stepCost
			if openrouterCost != nil {
				newCost += *openrouterCost
			}
			openrouterCost = &newCost
		}
	}

	modelConfig := model.CatwalkCfg
	cost := modelConfig.CostPer1MInCached/1e6*float64(usage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(usage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(usage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(usage.OutputTokens)

	if openrouterCost != nil {
		cost = *openrouterCost
	}
	if model.FlatRate {
		cost = 0
	}
	// Heartbeat usage: recorded even when the DB charge is skipped at cost 0 (design pt. 4).
	addHeartbeatUsage(ctx, model.Model.Provider(), model.Model.Model(), usage, cost)
	if cost == 0 {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The preflight max-cost check and replay charge are separate: a replay
	// that has already completed is always recorded, even if a concurrent
	// charge crosses the cap. This post-charge check prevents rearming once
	// the subtree budget reaches the cap.
	sess, err := a.sessions.IncrementCost(ctx, sessionID, cost)
	if err != nil {
		slog.Error("cache keep-alive: failed to accrue replay cost", "session_id", sessionID, "err", err)
		return false
	}
	slog.Debug("cache keep-alive: replay cost recorded", "session_id", sessionID, "cost", cost, "own_cost", sess.OwnCost)
	return true
}

// releaseCacheKeepAliveInFlight removes the session's in-flight registration
// only when it still belongs to generation gen. Three things may already
// have consumed it by the time a finishing fire unwinds: cancelCacheKeepAlive/
// CancelAll's Take (the fire was cancelled), a newer fire's replacement Set
// (this fire was superseded), or this fire's own rearm transition inside
// fireCacheKeepAlive. A blind Del here could otherwise drop a NEWER fire's
// registration and make that replay uncancellable.
func (a *sessionAgent) releaseCacheKeepAliveInFlight(sessionID string, gen int64) {
	a.cacheKeepAliveMu.Lock()
	defer a.cacheKeepAliveMu.Unlock()
	if infl, ok := a.cacheKeepAliveInFlight.Get(sessionID); ok && infl.generation == gen {
		a.cacheKeepAliveInFlight.Del(sessionID)
	}
}

// cancelCacheKeepAlive stops and removes any pending keep-alive timer for
// sessionID, AND cancels any replay call already in flight for it. Called at
// the start of a genuine new turn: a real request already means the cache is
// about to be refreshed naturally, so any stale scheduled or in-flight
// keep-alive is moot and must not race the real turn's own request — without
// this, an in-flight replay (bounded only by cacheKeepAliveCallTimeout, 30s)
// would otherwise keep running underneath the new turn. The Take is also the
// K-1 tombstone — consuming the in-flight registration under cacheKeepAliveMu
// is what makes the cancel visible to a fire that is between its ctx.Err()
// check and its rearm critical section.
func (a *sessionAgent) cancelCacheKeepAlive(sessionID string) {
	// Both maps checked under ONE lock hold, not two separate cycles: since
	// fireCacheKeepAlive now moves an entry from "pending" to "in-flight"
	// atomically (see its own comment), a single hold here is guaranteed to
	// see a consistent snapshot — the pending entry, the in-flight cancel, or
	// neither (genuinely idle / already fully finished), never a torn state
	// in between.
	a.cacheKeepAliveMu.Lock()
	entry, hasPending := a.cacheKeepAlive.Take(sessionID)
	inFlight, hasInFlight := a.cacheKeepAliveInFlight.Take(sessionID)
	a.cacheKeepAliveMu.Unlock()

	if hasPending {
		entry.timer.Stop()
	}
	if hasInFlight {
		inFlight.cancel()
	}
}
