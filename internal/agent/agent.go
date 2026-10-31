// Package agent is the core orchestration layer for Rush AI agents.
//
// It provides session-based AI agent functionality for managing
// conversations, tool execution, and message handling. It coordinates
// interactions between language models, messages, sessions, and tools while
// handling features like automatic summarization, queuing, and token
// management.
package agent

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/notify"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/version"
)

const (
	DefaultSessionName = "Untitled Session"

	// contextSlideRatio is the fraction of context window retained when the
	// sliding window kicks in (e.g. 0.70 = keep the newest 70% of tokens).
	contextSlideRatio = 0.70

	// contextSlideThreshold is the fraction of remaining context that triggers
	// the sliding window. When less than (1-contextSlideRatio) of the window is
	// left we trim the oldest messages so the next call fits within the budget.
	contextSlideThreshold = 1.0 - contextSlideRatio

	// Constants for auto-summarization thresholds (used only for background
	// summarisation triggered at the same time as the sliding window).
	largeContextWindowThreshold = 200_000
	largeContextWindowBuffer    = 20_000
	smallContextWindowRatio     = 0.2

	// streamIdleTimeoutDefault is the default tolerance for "no streaming
	// event for this long" before the watchdog cancels the LLM request.
	// Configurable per-app via Options.StreamIdleTimeoutSeconds, plumbed
	// through SessionAgentOptions.StreamIdleTimeout. Read at Run()-time
	// via effectiveStreamIdleTimeout below.
	//
	// Raised to 10 min on 2026-06-17. Extended-thinking models (GLM-5.2
	// on max effort, Opus 4.7+ with large thinking budgets, Sonnet 4.5
	// with thinking_budget ~32k) routinely go silent at the wire while
	// reasoning server-side — no reasoning_content deltas are streamed
	// until the final answer arrives. The previous 3-minute default
	// killed those runs prematurely. 10 minutes covers every observed
	// case so far without letting truly hung streams sit forever.
	// Operators who want the old behaviour can set the value back via
	// Options.StreamIdleTimeoutSeconds.
	streamIdleTimeoutDefault = 10 * time.Minute
	// streamWatchdogTick is how often the watchdog re-checks the
	// last-activity timestamp. Keep small enough that a stall is detected
	// promptly (well under streamIdleTimeout) but large enough not to
	// dominate logs.
	streamWatchdogTick = 30 * time.Second
)

const (
	// toolExecutionMaxDefault is the never-freeze backstop: the maximum
	// wall-clock a single tool may run while the stream watchdog is paused
	// (between OnToolCall and OnToolResult) before the watchdog force-
	// cancels the turn. Bash auto-backgrounds at 60s and batch output
	// polling is bounded, so this only catches truly stuck tools (hung MCP
	// tools, a blocked polling wait on a deadlocked process, or a sub-agent
	// delegation via the "agent" tool that never returns).
	// Configurable via Options.StreamToolTimeoutSeconds.
	//
	// One value applies uniformly to every tool, including a sub-agent
	// delegation — there used to be a separate, larger cap
	// (orchestratorToolExecutionMaxDefault, 45m) reserved for delegations
	// while plain tools kept a shorter one (15m). That split caused its own
	// false cutoffs: a sub-agent's OWN plain tool call (a slow build/test
	// inside ITS turn, not a delegation) still only got the short cap, so
	// legitimate long-running work inside a sub-agent kept getting killed
	// just as often as a parent waiting on a delegation used to be killed
	// by the old single 15m cap. Unifying to one generous value avoids
	// both directions of false cutoff at the cost of a genuinely wedged
	// tool taking longer to get caught — worth it since the point is to
	// bound the wait, not to bound it tightly.
	toolExecutionMaxDefault = 45 * time.Minute

	// toolCleanupGraceDefault is a fixed buffer added ON TOP of
	// toolMaxDuration before the stream watchdog is allowed to force-cancel
	// a tool-in-flight. It exists for the parent/child cancellation race
	// created by toolExecutionMaxDefault's unification above: a tool call
	// that is itself an `agent`-tool delegation runs a nested Run()/runTurn()
	// with its own stream watchdog, started strictly LATER than the
	// parent's (the parent starts timing from OnToolCall — the moment it
	// decided to delegate; the child starts timing only once its own turn
	// actually begins executing, after init and the DB preamble). Both
	// watchdogs share the same toolMaxDuration, so the parent's clock — with
	// its head start — would always reach the cap first and cancel genCtx,
	// which cascades into the child's ctx, before the child's own watchdog
	// ever gets a chance to fire on ITS cap and unwind cleanly (finish part,
	// cost transfer per task #197, goroutine dump). This grace is applied
	// uniformly to every tool-in-flight regardless of kind — it is not a
	// tool-name-keyed special case (that pattern was deliberately rejected
	// above); it just gives whichever tool is running a little extra runway
	// past its nominal cap, which is harmless for a plain tool and decisive
	// for a delegation. Configurable via Options.ToolCleanupGrace for tests;
	// 0 falls back to this default rather than disabling the grace, since an
	// accidental zero would silently reopen the race this constant exists
	// to close.
	//
	// HONEST SCOPE (found overstated by an @oh review of task #205's own
	// fix — corrected here rather than in a follow-up commit that could go
	// unread): 90s only guarantees the child wins the race against the
	// PARENT'S watchdog start skew — the gap between the parent's OnToolCall
	// and the child's own watchdog starting (init + DB preamble), which
	// really is sub-second to low seconds. It does NOT bound how long the
	// child may productively work before the tool call that actually wedges
	// starts. Concretely: parent fires at delegationStart + M + 90s; child
	// fires at (delegationStart + skew + workDuration) + M, where
	// workDuration is the child's own prior productive work before hitting
	// its stuck tool — unbounded, since the child's watchdog resets on every
	// tool-call boundary (see toolFinished's doc below). The child wins only
	// when skew + workDuration < 90s, i.e. only when the child wedges
	// EARLY (near-immediately after being delegated to) — the common "child
	// worked productively for minutes, THEN wedged deep into its turn" case
	// is NOT covered: the parent still force-cancels it first. What this
	// grace DOES guarantee unconditionally: the parent never fires before
	// its own toolMaxDuration+90s has elapsed since delegating, giving any
	// child that happens to wedge quickly (the case this was originally
	// observed in) real runway to clean up. A structural fix for the general
	// case would need the child to push progress signals the parent's own
	// watchdog resets on — not implemented; tracked as a follow-up, not a
	// release blocker, since the practical loss on the uncovered path is
	// diagnostic quality (finish part / goroutine dump), not correctness —
	// the parent still terminates, and task #197 already made the
	// cost-transfer step itself cancel-immune.
	//
	// Fork patch, task #205: this default is applied ONLY to a NON-sub-agent
	// (top-level) session — see effectiveToolCleanupGrace's doc. Applying it
	// to a sub-agent's own watchdog as well (task #200's original fix) made
	// the race worse, not better: with identical toolMaxDuration+grace on
	// both sides, the 90s cancels out of the "child must fire before parent"
	// inequality algebraically, leaving the parent's head start (from
	// OnToolCall, before the child's own watchdog even starts) as the only
	// deciding factor — so the parent always still won regardless of how
	// early the child wedged. A sub-agent can never itself be waiting on a
	// nested `agent`-tool delegation (the `agent` tool is excluded from
	// workerToolNames for sub-agents — see buildToolsAgentConfig), so it is
	// always the deepest watchdog in the chain and never needs runway to let
	// a nested watchdog go first.
	toolCleanupGraceDefault = 90 * time.Second

	// defaultCheckpointInterval is the default coalescing interval for
	// mid-stream DB flushes of in-progress assistant text. When > 0,
	// the auto-checkpoint ticker writes the Parts to DB at most once
	// per interval, bounding the text lost to a SIGTERM during final
	// composition. 0 disables checkpointing. Overridden by
	// SessionAgentOptions.CheckpointInterval.
	// Fork patch: batch 8.
	defaultCheckpointInterval = 2 * time.Second

	// peakHoursPollInterval is the mid-turn safety check cadence. The
	// OnStepFinish check catches normal step boundaries; this ticker catches
	// long streams, retries, and tool execution.
	peakHoursPollInterval = 10 * time.Second
)

// sessionPreambleMaxDurationDefault bounds the DB preamble at the top of
// Run() — sessions.Get, getSessionMessages, createUserMessage — all of which
// route through the single-writer sql.DB connection (SetMaxOpenConns(1) in
// internal/db/connect.go). The stream watchdog is not started until AFTER
// this preamble, so before this fix a stuck writer connection (a concurrent
// sub-agent's own preamble wedged on it, etc.) hung the whole turn invisibly
// and unboundedly: no watchdog running yet means no cancellation, no timeout
// log line, nothing — just a process that sits there with a "SessionAgent.Run:
// starting" log line and never another. Observed live: PID 28908 logged that
// line for a sub-agent session and then went silent for 10+ minutes while the
// lock heartbeat kept ticking (heartbeat is independent of actual progress —
// see task #192). This cap is generous — normal SQLite reads/writes take low
// milliseconds — so tripping it means something is genuinely wedged, not
// slow. Overridden per-agent via SessionAgentOptions.SessionPreambleMaxDuration,
// resolved at Run()-time via effectiveSessionPreambleMaxDuration below.
const sessionPreambleMaxDurationDefault = 60 * time.Second

var userAgent = fmt.Sprintf("Rush/%s (https://github.com/PHPCraftdream/rush)", version.Version)

type SessionAgentCall struct {
	SessionID       string
	Prompt          string
	ProviderOptions fantasy.ProviderOptions
	Attachments     []message.Attachment
	// Origin marks the entry channel this call's user message arrived
	// through (message.OriginCLI/Web/SDK). Stamped by buildCall from the
	// call's context so it survives the InterruptAndSend queued-replacement
	// handoff; SessionAgentCall literals built outside buildCall (sub-agent
	// dispatch) leave it unspecified. Persisted on the created user
	// message by createUserMessage.
	Origin              message.Origin
	AutoResumed         bool
	BackgroundJobNotice bool
	// NoticeKind distinguishes the kind of notice this call's persisted
	// message carries -- see message.Message.NoticeKind's doc for the value
	// set (§5.3bis). "" for every ordinary call (legacy default).
	NoticeKind       string
	MaxOutputTokens  int64
	Temperature      *float64
	TopP             *float64
	TopK             *int64
	FrequencyPenalty *float64
	PresencePenalty  *float64
	NonInteractive   bool
	// SystemPromptOverride, if non-empty, replaces the agent's global system prompt
	// for this single call. Used to apply per-session system prompts from the DB.
	SystemPromptOverride string
	// MaxCost aborts the run if total session cost exceeds this value (0 = no cap).
	MaxCost float64
	// MaxTokens aborts the run if total prompt+completion tokens exceed this value
	// (0 = no cap).
	MaxTokens int64
	// ExistingMessageID, when non-empty, marks this call as referencing a
	// user message that already exists in the DB (created by another process,
	// e.g. `rush sessions inject --interrupt`). The queue-drain path in
	// Run's PrepareStep must then load that message by ID and splice it into
	// the prompt WITHOUT calling createUserMessage — otherwise the operator
	// would see the same message twice in history. Set by
	// QueueExistingMessage on the interrupt path.
	ExistingMessageID string

	// OnUserMessageCreated, if non-nil, is invoked once with the ID of the
	// user message createUserMessage creates for this call. Not invoked
	// when ExistingMessageID is already set (no new message was created).
	// Lets a caller capture the ID without a second DB round trip -- e.g.
	// coordinator_run.go's transient-retry loop sets this on the initial
	// attempt, then feeds the captured ID back in as ExistingMessageID on
	// each retry so a provider hiccup doesn't re-persist the same prompt
	// as a fresh row every attempt.
	// json:"-": in-process callback, never durable-queue-persisted (unlike
	// ExistingMessageID above, session.SessionAgentCallData has no mirror
	// for this field -- a retry always runs in the same process that set
	// it). Also lets code that shortcuts through json.Marshal(SessionAgentCall{...})
	// keep working now that not every field is a plain value type.
	OnUserMessageCreated func(messageID string) `json:"-"`

	// OnAssistantMessageCreated, if non-nil, is invoked once for EVERY
	// assistant message row the turn creates (PrepareStep runs once per
	// step, so a multi-step tool loop fires it once per step; the LAST
	// invocation names the turn's terminal assistant row). Lets a caller
	// identify the exact row THIS call's turn wrote instead of
	// re-deriving it from session-wide state -- coordinator_run.go's
	// transient-retry loop classifies only this row, because the
	// session's last assistant message at classification time can belong
	// to a concurrent caller's turn (R3-1, round 5).
	// json:"-": in-process callback, never durable-queue-persisted (same
	// rationale as OnUserMessageCreated above).
	OnAssistantMessageCreated func(messageID string) `json:"-"`

	// replacementHandoff marks a call extracted from mailbox.replacement.
	replacementHandoff bool

	// InjectID, when non-empty, is the ID of a pending_injects row that
	// must be deleted AFTER successful OS lock acquisition. Set by the
	// cross-process interrupt inject path (startDetachedRun) to
	// prevent data loss if the detached run loses the OS lock race.
	// P0-2 fix.
	InjectID string

	// SmartModel/FastModel/SystemPromptPrefix, when set, pin this call's
	// model and prompt configuration instead of reading the agent's shared,
	// mutable fields (task #265, P0-1).
	//
	// The agent's smartModel/fastModel/systemPromptPrefix are process-wide
	// and get REWRITTEN in place by coordinator.applyModelOverrides, which
	// any per-session model override triggers. Every turn re-reads them, so
	// with two sessions running concurrently — the whole point of this fork
	// — session A's override lands in session B's next turn, and a turn can
	// even observe a model change partway through its own run. These fields
	// let a caller hand the resolved values down with the call, so a run
	// stays self-consistent regardless of what another session does
	// meanwhile.
	//
	// Pointers, so "explicitly set" is distinguishable from "zero value":
	// nil means "use the agent's shared value", which is exactly what every
	// existing caller that never sets them keeps getting.
	SmartModel         *Model
	FastModel          *Model
	SystemPromptPrefix *string
	// Credentials keeps non-durable per-call provider credentials attached to
	// the in-process call snapshot. It is never serialized or rebuilt.
	Credentials *CredentialSet `json:"-"`

	// PersistSmartModel/PersistFastModel are the explicitly requested session
	// slots for an admitted ExecuteRun. They are written after the mailbox and
	// inter-process session lock are both acquired, so a rejected call cannot
	// alter durable session state. Nil means leave that slot untouched.
	PersistSmartModel *session.ModelSlotUpdate `json:"-"`
	PersistFastModel  *session.ModelSlotUpdate `json:"-"`

	// SystemPrompt pins the BASE system prompt — the one applyModelOverrides
	// rebuilds from the resolved provider/model. It is distinct from, and
	// lower precedence than, SystemPromptOverride: the override is the
	// per-session prompt persisted in the DB, which still wins when present.
	//
	// This one matters on the paths where the per-session prompt is empty
	// (resolveSessionSystemPrompt returning "" on a DB error, mainly) — the
	// turn then falls back to the agent's shared systemPrompt, which is
	// exactly the field another session's override rewrites.
	SystemPrompt *string

	// Tools pins THIS call's complete tool slice (R3-1). It is built per
	// call by resolveSessionModels/applyModelOverrides from the same
	// pinned config snapshot and context as SmartModel/SystemPrompt above,
	// so the per-call policy (CallOptions.DisableSubAgents, ModelRole)
	// decides it, and the turn consumes its own slice at start and at
	// every PrepareStep instead of re-reading the shared a.tools — which
	// any concurrent call's UpdateModels/buildTools rewrites in place.
	// nil (legacy callers; direct sessionAgent.Run tests) keeps the
	// historical shared-slice behavior, including PrepareStep's live
	// re-read. json:"-": never durable-queue-persisted — tool objects are
	// live in-process values, so pump-driven rebuilds fall back to the
	// shared path exactly like any other legacy caller.
	Tools []fantasy.AgentTool `json:"-"`

	// RunAllowlist pins THIS call's restricted-run permission policy
	// (R3-4): the compiled matcher, armed by the turn loop (runOwned) ONLY
	// when this call becomes the active turn — never at queue time. It is
	// stamped by buildCall/runInternal from the call's context, and
	// recompiled from the serialized spec by RebuildSessionAgentCall for
	// durable-queue restarts (see RunAllowlistSpec below). nil (legacy
	// in-process callers, and durable rows persisted before the spec field
	// existed) arms nothing: such turns fall back to the session baseline
	// ExecuteRun armed for their session (F2,
	// permission.SessionRunAllowlistBaselineManager — now a legacy-row
	// fallback only), and only then to the process-wide gate.
	// json:"-": the compiled matcher is an in-process value; the
	// serializable spec travels in RunAllowlistSpec below.
	RunAllowlist *permission.RunAllowlist `json:"-"`

	// RunAllowlistSpec pins THIS call's restricted-run policy in its
	// serializable, pre-compilation form (the spec RunAllowlist was compiled
	// from). Unlike RunAllowlist above, it is NOT json:"-": it is the field
	// ToSessionAgentCallData persists on the durable run queue, and
	// RebuildSessionAgentCall recompiles it into RunAllowlist so a
	// durably-restarted call runs under ITS OWN caller's declared policy —
	// keyed by LogicalCallID — instead of a session-wide last-writer-wins
	// fallback (review R4-1) and instead of leaving its sub-agents to
	// inherit auto-approval with no restriction (review R4-2). Its presence
	// on a pump-rebuilt call is also the durable marker that the call's
	// caller ran the session non-interactive with auto-approve
	// (RunSessionAgentCall uses it to re-arm AutoApproveSession after a
	// real process restart, review R4-3). nil for legacy in-process
	// callers, pre-migration durable rows, and non-ExecuteRun callers.
	RunAllowlistSpec *permission.RunAllowlistSpec

	// FolderScopeSpec pins THIS call's folder scope in its serializable,
	// pre-compilation form (the spec CallOptions.FolderScope was compiled
	// from). Unlike CallOptions above, it is NOT json:"-": it is the
	// field ToSessionAgentCallData persists on the durable run queue,
	// and RebuildSessionAgentCall recompiles it into a rebuilt call's
	// CallOptions.FolderScope so a durably-restarted call gets its
	// scoped filesystem toolset back instead of silently restarting
	// with the shared unscoped toolset (T12). nil for unscoped calls,
	// legacy in-process callers, and pre-migration durable rows.
	FolderScopeSpec *permission.FolderScopeSpec

	// DiskRequirement is the process-bound execution requirement THIS call
	// must run under (R12-H2b): the opaque binding ID (with the
	// serializable policy baseline) a continuation restores or refuses.
	// It is stamped at admission and round-trips through
	// SessionAgentCallData so every continuation carries its own identity
	// instead of inheriting "the latest provider for the session". nil for
	// ordinary OS-disk calls. Unlike CallOptions (json:"-") this IS
	// serialized: ToSessionAgentCallData persists it on the durable row.
	DiskRequirement *session.SessionExecutionRequirement

	// FromDurableQueue is true when this call originates from the durable
	// run queue (task #340). When true and the session's mailbox is already
	// owned by another in-process turn, mailbox.submit does NOT append this
	// call to mb.submitted — the durable row itself provides the retry path,
	// so no in-process handoff is needed. This prevents double-execution
	// where both the live owner (draining mb.submitted) and the pump (after
	// its backoff expires) would execute the same logical request independently
	// (P0-1 in docs/reviews/2026-08-11-release-readiness-concurrency-and-code-review.md).
	//
	// For non-durable calls (web/CLI turns, interrupt-inject via
	// handleInterruptTick → InterruptAndReplace), FromDurableQueue is false and the mailbox queue
	// is the only retry path, so submit's normal mb.submitted behavior applies.
	FromDurableQueue bool

	// LogicalCallID is a stable identifier for this logical request, generated
	// ONCE when the call is first created (e.g. in buildCall). It is used as the
	// idempotency key for durable enqueue operations (task #340), ensuring that
	// retries of the same logical request reuse the same session_run_queue row
	// instead of creating duplicates. P2-1 fix.
	//
	// Previously, the idempotency key was generated from time.Now().UnixNano()
	// at enqueue time, which meant each retry got a different key — breaking the
	// idempotency contract and potentially creating multiple durable rows for the
	// same logical request.
	LogicalCallID string

	// CallOptions pins THIS call's execution policy (R1-1) - model role,
	// cost/token caps, peak-hours bypass, timeout watchdog policy, the
	// sub-agent ban and the fail-fast busy contract. Stamped by
	// runInternal/buildCall from the call's context (WithCallOptions); nil
	// for legacy callers, whose runs keep reading the coordinator's shared
	// Set*-state. Never durable-queue-persisted: ToSessionAgentCallData
	// does not copy it, so pump-driven rebuilds fall back to the shared
	// path exactly like any other legacy caller.
	CallOptions *CallOptions `json:"-"`

	// onQueueResolved, if non-nil, is invoked exactly once with the outcome
	// of THIS specific call's own turn -- whether it ran immediately or was
	// queued behind a busy mailbox and executed later by runOwned's own
	// dispatch loop (agent_run.go's `for` loop, one call per iteration).
	// Unlike OnAssistantMessageCreated (fires per assistant row, mid-turn),
	// this fires once, after the turn this exact call became has fully
	// returned. nil for every existing caller -- added for
	// coordinator.runAwaitingAdmission (#1036).
	// json:"-": in-process only, never durable-queue-persisted (same
	// rationale as OnUserMessageCreated/OnAssistantMessageCreated above).
	//
	// CONTRACT: a call carrying this hook has a live in-process waiter with
	// no implicit timeout anywhere in the chain -- every path that removes a
	// call from a mailbox queue WITHOUT running it as a turn (durable
	// orphan/restart enqueue, ClearQueue, a replacement overwritten by a
	// newer interrupt-and-replace) MUST resolve this hook with an error
	// (see queued_call_resolution.go's resolveCallWithError/
	// splitCallsWithoutWaiters) instead of silently dropping the call, or
	// the waiter blocks forever.
	onQueueResolved func(*fantasy.AgentResult, error) `json:"-"`

	// FailIfSessionBusy rejects this call instead of queueing it when the
	// session's mailbox is already owned (submit returns false): Run fails
	// with an error wrapping ErrSessionBusy instead of the historical
	// silent nil. Sourced from CallOptions.FailIfSessionBusy. The guard is
	// enforced AT the atomic mailbox reservation, so two simultaneous
	// starters can never both observe "idle" and slip into the queue -
	// exactly one wins, the loser fails fast (#818/R1-4). Not durable: a
	// pump-driven retry is by definition not racing a live in-process
	// starter for ownership.
	FailIfSessionBusy bool

	// IsDrain: phase-4 step 3 drain call. Set by newDrainCall
	// (agent_drain.go) or, for the prompt-string entry points, from
	// WithDrainCall's ctx marker (buildCall/runInternal) -- see agent_drain.go
	// for the full contract. Never durable-queue-persisted.
	IsDrain bool
	// drainTurnCommitted marks a retry of a Drain call whose first attempt
	// already reached the provider (a transient failure or 401 retry): the
	// notice it reacts to was pulled by that first attempt, so the retry
	// must not re-apply the turn-start "pulled a wake=1 notice" gate.
	drainTurnCommitted bool
}

// turnConfig is the immutable per-call snapshot of everything model- and
// prompt-shaped a turn needs (task #265, P0-1). Resolved ONCE — from the
// call where it pins values, otherwise from the agent's shared fields —
// then passed by value, so nothing downstream can observe a mid-run change
// made by a concurrent session.
type turnConfig struct {
	smartModel   Model
	fastModel    Model
	systemPrompt string
	promptPrefix string
}

// resolveTurnConfig builds the snapshot for one call. Reads each shared
// field exactly once; a value pinned on the call wins over the shared one.
func (a *sessionAgent) resolveTurnConfig(call SessionAgentCall) turnConfig {
	cfg := turnConfig{
		smartModel:   a.smartModel.Get(),
		fastModel:    a.fastModel.Get(),
		systemPrompt: a.systemPrompt.Get(),
		promptPrefix: a.systemPromptPrefix.Get(),
	}
	if call.SmartModel != nil {
		cfg.smartModel = *call.SmartModel
	}
	if call.FastModel != nil {
		cfg.fastModel = *call.FastModel
	}
	if call.SystemPromptPrefix != nil {
		cfg.promptPrefix = *call.SystemPromptPrefix
	}
	if call.SystemPrompt != nil {
		cfg.systemPrompt = *call.SystemPrompt
	}
	// Per-session system prompt beats the global one — same precedence this
	// had when it lived inline in runTurn.
	if call.SystemPromptOverride != "" {
		cfg.systemPrompt = call.SystemPromptOverride
	}
	return cfg
}

type SessionAgent interface {
	Run(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)
	// ReserveExclusive atomically claims exclusive ownership of sessionID's
	// mailbox without starting a turn (task #614). See the sessionAgent
	// implementation's doc for the full contract: the returned epoch/cancel
	// pair must be handed to exactly one of RunWithReservedOwnership or
	// ReleaseExclusive, exactly once. ok is false (fail closed) when the
	// session is already owned or the mailbox is stopped. Returns holdCtx
	// (the derived context that cancellation signals), the epoch, a cancel
	// func, and ok. The caller must present the epoch and cancel to exactly
	// one of RunWithReservedOwnership or ReleaseExclusive.
	ReserveExclusive(ctx context.Context, sessionID string) (holdCtx context.Context, epoch uint64, cancel context.CancelFunc, ok bool)
	// ReleaseExclusive drops a reservation taken by ReserveExclusive without
	// running a turn, safely handing off anything that raced into the
	// mailbox during the hold. Use on any bail-out path after a successful
	// ReserveExclusive that will not call RunWithReservedOwnership.
	ReleaseExclusive(sessionID string, epoch uint64, cancel context.CancelFunc)
	// RunWithReservedOwnership executes call using ownership already claimed
	// by ReserveExclusive, continuing the SAME ownership era instead of
	// releasing and re-claiming — see its own doc for why that gap matters.
	// onHandoff, if non-nil, is invoked immediately before the handoff to
	// runOwned; it is used by the caller to transfer release responsibility.
	RunWithReservedOwnership(ctx context.Context, call SessionAgentCall, epoch uint64, cancel context.CancelFunc, onHandoff func()) (*fantasy.AgentResult, error)
	SetModels(smart Model, fast Model)
	SetTools(tools []fantasy.AgentTool)
	SetSystemPrompt(systemPrompt string)
	SetSystemPromptPrefix(prefix string)
	SystemPrompt() string
	Cancel(sessionID string)
	CancelAll() (stillBusy bool)
	IsSessionBusy(sessionID string) bool
	IsBusy() bool
	QueuedPrompts(sessionID string) int
	QueuedPromptsList(sessionID string) []string
	ClearQueue(sessionID string)
	// QueueMessage appends a call to the session's pending queue without
	// starting a Run. Used by the "interrupt and send" path in the web
	// server: the caller queues, then Cancel()s the running turn, and the
	// in-flight Run() drains the queue from its cancel-handling branch.
	//
	// P2-1 API footgun warning: there is NO production caller of this method
	// today (grep "QueueMessage(" outside tests returns empty). The method
	// queues a call WITHOUT starting a runner — a caller MUST independently
	// guarantee a Run() will drain it soon. There is no owner-side timeout.
	// For new production code, prefer Run()/InterruptAndReplace, which provide
	// atomic submit-and-run or handoff semantics. This method exists primarily
	// for legacy mailbox migration tests.
	QueueMessage(call SessionAgentCall)
	// InterruptAndReplace atomically records call as the replacement the
	// current owner runs next and cancels only the in-flight generation
	// (design §4). Replaces the QueueMessage+Cancel two-step that
	// deterministically wiped its own queued message (P0-2). Returns true
	// when a turn was actually interrupted; false when the session was idle
	// (the caller should then queue the call for the next Run itself).
	InterruptAndReplace(sessionID string, call SessionAgentCall) bool
	// InjectMessage persists `call` as a regular user message in the DB
	// immediately (so the UI sees it the moment the operator clicks Inject)
	// AND — if the session is currently running — schedules the message to
	// be appended to `prepared.Messages` at the next PrepareStep boundary so
	// it lands in the next provider request without a restart. Returns the
	// persisted message. When the session is NOT busy, the message is just
	// persisted; the caller can decide whether to start a new Run.
	InjectMessage(ctx context.Context, call SessionAgentCall) (message.Message, error)
	// Summarize compresses the session history. If the session is currently
	// busy the request is queued; call TakeSummarizeQueue after the task
	// finishes to pick it up.  Returns ErrSummarizeQueued when queued.
	//
	// The snapshot contains the model, provider options, and prompt prefix
	// resolved from the target session (or shared state for sessions without
	// overrides), ensuring the entire summarize operation uses consistent
	// configuration regardless of concurrent SetModels calls (task #341).
	Summarize(context.Context, string, *SummarizeSnapshot) error
	// SummarizeQueued reports whether a manual summarise is pending for the
	// given session.
	SummarizeQueued(sessionID string) bool
	// TakeSummarizeQueue atomically removes and returns the pending summarise
	// snapshot for the session (if any).
	TakeSummarizeQueue(sessionID string) (*SummarizeSnapshot, bool)
	// CancelQueuedSummarize removes a pending summarise from the queue.
	CancelQueuedSummarize(sessionID string)
	// SetTimeoutOptions configures the stream watchdog's deadline extension
	// behaviour for the next and subsequent Run() calls. Called from
	// RunNonInteractive when --timeout-extends-on-progress is set.
	// Fork patch: batch 8.
	SetTimeoutOptions(extendsOnProgress bool, hardCap time.Duration)
	Model() Model
}

type Model struct {
	Model      fantasy.LanguageModel
	CatwalkCfg catwalk.Model
	ModelCfg   config.SelectedModel
	FlatRate   bool
}

type sessionAgent struct {
	smartModel         *csync.Value[Model]
	fastModel          *csync.Value[Model]
	systemPromptPrefix *csync.Value[string]
	systemPrompt       *csync.Value[string]
	tools              *csync.Slice[fantasy.AgentTool]
	// coord is the owning coordinator (nil on bare fixtures): the batch
	// facts, reaction store and notice pull all route through it. The
	// deleted workLedger's remaining retained state (hint counter, external
	// driver mirror, supervision) lives on the coordinator too.
	coord  *coordinator
	config *config.ConfigStore

	// wsHomeOverride is the WS-1 test seam (#1142 step C): when non-nil it
	// replaces config.WorkspaceHome() in processWorkspace, so a test can
	// model a shared-from-linked process (non-empty workspace, home=false)
	// without a config store that would produce one. Production wiring never
	// sets it; home in production comes from the single source
	// config.WorkspaceHome() (false only for a linked worktree on the shared data directory, SD-D #1143).
	wsHomeOverride *bool

	// onSessionIdle, when non-nil, fires with a session id every time THIS
	// agent's mailbox genuinely releases that session -- called from
	// abandonOwnershipWithHandoff (agent_ownership.go), the one function
	// every Run()/RunWithReservedOwnership/ReleaseExclusive exit funnels
	// through. Phase 3's by-construction "session became idle" trigger
	// (docs/plans/2026-09-28-async-phase3-spec.md, orchestrator decision
	// item 1): unlike the scattered per-caller re-check calls it fires for
	// EVERY exit on EVERY SessionAgent this coordinator builds, including a
	// delegated child's driver (whose turns never pass through
	// coordinator.runInternal at all) -- a future Run() caller that forgets
	// its own manual trigger cannot silently strand a delegation. A stale
	// firing (a new owner already re-claimed the mailbox) is a harmless
	// no-op: the callback re-reads current mailbox state, not a snapshot.
	onSessionIdle func(sessionID string)

	// runWg tracks all active Run() calls across this agent. CancelAll waits
	// on this WaitGroup to ensure all dispatcher goroutines have fully
	// unwound before proceeding with shutdown. This provides a true join
	// primitive instead of the old IsBusy() polling approach, which could
	// report "not busy" before the actual Run() goroutine had finished
	// its cleanup and unwound. Every entry point that starts a dispatcher
	// (Run, RunSessionAgentCall) calls Add(), and every exit path calls
	// Done() in a defer.
	runWg sync.WaitGroup

	// shuttingDown latches on the first CancelAll and is checked by Run
	// before it claims any session. Round 14 closing review, blocker 1:
	// the per-mailbox `stopped` latch only stops an EXISTING turn loop from
	// picking up more work — it cannot stop a NEW owner from appearing.
	// After CancelAll's sweep, a mailbox that ends at mbIdle happily grants
	// ownership to the next Run(), which then runs a full provider turn
	// that the (already-finished) sweep will never cancel. Worse, mailboxes
	// are created lazily, so a Run for a session id the sweep never saw
	// gets a fresh mailbox with stopped == false — no race required.
	//
	// Entry points that can still call Run after the sweep are real and
	// several: coordinator.startDetachedRun (its own detached goroutine on
	// a WithoutCancel context), runSummarize's tail, Summarize, and the web
	// handlers — none of which observe per-mailbox state.
	//
	// Agent-level and one-way, for the same reason `stopped` is: the agent
	// belongs to a process that is exiting.
	shuttingDown atomic.Bool

	// admitMu/stopping form the admission gate that closes a real "Add
	// concurrently with Wait" race between Run()/Summarize's runWg.Add(1)
	// and CancelAll's runWg.Wait(). P1-1: Run/Summarize used to call
	// runWg.Add(1) and then check shuttingDown as two separate,
	// unsynchronized steps. Between the Add and the check, a concurrent
	// CancelAll could set shuttingDown and spin up its runWg.Wait() goroutine
	// while the counter was still 0 — Wait() could then return (nothing to
	// wait for) a moment before the Add(1) landed, which either panics
	// ("Add called concurrently with Wait", per the sync.WaitGroup contract:
	// "calls with a positive delta that start when the counter is zero
	// must happen before a Wait") or lets a new Run/Summarize start after
	// CancelAll has already told its caller shutdown was safe.
	//
	// The fix: shuttingDown is only ever flipped false->true, and every
	// read (Run/Summarize via tryAdmitRunWg) and the one write (CancelAll)
	// happen under admitMu. So either the Run/Summarize critical section
	// runs entirely before CancelAll's (shuttingDown was still false:
	// runWg.Add(1) is guaranteed to complete, and complete-before, CancelAll's
	// subsequent cancel+Wait-spawn), or it runs entirely after (shuttingDown
	// is already true: tryAdmitRunWg bails out and never calls Add at all).
	// There is no interleaving left where Add and Wait can race.
	// Pattern mirrors P0-3's RunQueuePump.admitMu/stopping gate.
	admitMu sync.Mutex

	isSubAgent           bool
	sessions             session.Service
	messages             message.Service
	disableAutoSummarize bool
	isYolo               bool
	notify               pubsub.Publisher[notify.Notification]
	// streamIdleTimeout, when > 0, overrides streamIdleTimeoutDefault for
	// every Run() on this agent. Set from Options.StreamIdleTimeoutSeconds
	// via SessionAgentOptions at construction. 0 = use the default.
	streamIdleTimeout time.Duration
	// streamWatchdogTick, when > 0, overrides streamWatchdogTick for every
	// Run()/Summarize() on this agent. Set from SessionAgentOptions at
	// construction. 0 = use the default (30s).
	streamWatchdogTick time.Duration
	// titleJoinGrace, when > 0, overrides the package-level titleJoinGrace
	// const for every Run() on this agent. Set from
	// SessionAgentOptions.TitleJoinGrace at construction. 0 = use the
	// default (10s). Test-only seam (task #454, following up on task
	// #450/#453's test-speed investigation): several tests assert on the
	// grace period actually firing (a slow title provider must be
	// abandoned by the turn, not joined forever) and had no way to observe
	// that faster than the real bound.
	titleJoinGrace time.Duration
	// cancelAllGrace, when > 0, overrides CancelAll's own 5s runWg.Wait
	// grace period (a separate constant from titleJoinGrace, even though
	// both happen to default to 5s). Set from
	// SessionAgentOptions.CancelAllGrace at construction. 0 = use the
	// default (5s). Test-only seam (task #454), same rationale as
	// titleJoinGrace.
	cancelAllGrace time.Duration
	// titleGens holds the cancel funcs of the in-flight title-generation
	// goroutines so CancelAll can stop them. Their contexts are deliberately
	// detached from every turn's own cancel (agent_turn_title.go), which
	// makes this registry the only hook that reaches them — see
	// titleGenRegistry. Nil on a bare &sessionAgent{} (unit-test fixtures
	// that never launch a title goroutine); every production agent gets one
	// from NewSessionAgent.
	titleGens *titleGenRegistry
	// dataDir is the absolute path to .rush/, used for the per-session
	// inter-process file lock. Empty means locking is disabled (legacy
	// callers / tests). Plumbed from SessionAgentOptions.DataDirectory.
	dataDir string
	// lockOptions holds session.LockOption values passed from
	// SessionAgentOptions. Used when the agent acquires inter-process locks
	// via session.TryAcquireSessionLockWithOptions. Primarily for tests.
	lockOptions []session.LockOption
	// testReserveRebindSeam, when non-nil, is invoked by
	// RunWithReservedOwnership strictly after tryAdmitRunWg succeeds and
	// strictly before mailbox.rebindDispatcher runs — the narrow window in
	// which a concurrent CancelAll can latch mb.stopped between admission
	// and the rebind. Test-only (task #641 F-4); nil (a no-op) in every
	// production path, mirroring mailbox.testLoopRearmSeam's idiom.
	testReserveRebindSeam func()
	// checkpointInterval is plumbed from SessionAgentOptions.
	// When > 0 the Run method starts a coalescing ticker that flushes
	// in-memory streaming Parts to DB mid-step, bounding text loss on
	// SIGTERM. Fork patch: batch 8.
	checkpointInterval time.Duration
	// timeoutExtendsOnProgress, when true, makes the stream watchdog
	// extend its deadline every time streaming progress occurs.
	// Fork patch: batch 8.
	timeoutExtendsOnProgress bool
	// timeoutHardCap is the maximum wall-clock time the watchdog will
	// allow, even with continuous progress. 0 = no cap.
	// Fork patch: batch 8.
	timeoutHardCap time.Duration
	// toolMaxDuration bounds the watchdog's tool-pause (never-freeze
	// backstop). Past it the watchdog fires with a distinct "tool timeout"
	// reason so the agent turn ends instead of hanging on a stuck tool.
	// 0 = use toolExecutionMaxDefault. This is the EXPLICIT OPERATOR
	// OVERRIDE (Options.StreamToolTimeoutSeconds) and, when set, always
	// wins over the built-in default, in either direction.
	toolMaxDuration time.Duration
	// toolCleanupGrace is the buffer added on top of the resolved
	// toolMaxDuration before the watchdog force-cancels a tool-in-flight —
	// see toolCleanupGraceDefault's doc for why this exists (parent/child
	// delegation cancellation race) and effectiveToolCleanupGrace's doc for
	// full precedence. 0 = no explicit override: falls back to
	// toolCleanupGraceDefault for a top-level session, or to 0 (no grace)
	// for a sub-agent session (task #205). Tests may set a small explicit
	// value via SessionAgentOptions.ToolCleanupGrace, which always wins.
	toolCleanupGrace time.Duration
	// sessionPreambleMaxDuration bounds Run()'s DB preamble (sessions.Get,
	// getSessionMessages, createUserMessage) for THIS agent — see
	// sessionPreambleMaxDurationDefault's doc for the full rationale. 0 = use
	// sessionPreambleMaxDurationDefault; tests may override to a small value
	// via SessionAgentOptions.SessionPreambleMaxDuration instead of mutating
	// shared state.
	sessionPreambleMaxDuration time.Duration
	// titleGenerationMaxDuration bounds the background title-generation
	// goroutine for THIS agent — see titleGenerationMaxDurationDefault's doc
	// for the full rationale. 0 = use titleGenerationMaxDurationDefault;
	// tests may override to a small value via
	// SessionAgentOptions.TitleGenerationMaxDuration instead of mutating
	// shared state.
	titleGenerationMaxDuration time.Duration

	// activeRequests is retained for the per-turn cancel that
	// OnStepFinish's max-cost / max-tokens / peak-hours abort paths still
	// look up directly (Cancel/CancelAll now route through the mailbox
	// instead). The sessionID+"-summarize" synthetic key that used to live
	// here has been removed (#268/P0-4: compaction now owns the mailbox via
	// beginCompact, so its cancel lives in mailbox.current.cancel). Plain
	// sessionID entries are inert (already-fired cancelFuncs) and are no
	// longer consulted for busy state (IsSessionBusy reads the mailbox).
	activeRequests *csync.Map[string, context.CancelFunc]
	// summarizeQueue holds a pending manual-summarise snapshot per session,
	// queued while the session was busy. The snapshot contains the model,
	// provider options, and prompt prefix resolved from the target session,
	// ensuring the entire summarize operation uses consistent configuration
	// regardless of concurrent SetModels calls (task #341, P1-1).
	summarizeQueue *csync.Map[string, *SummarizeSnapshot]
	// mailboxes holds the per-session owner/mailbox state machine described
	// in docs/plans/2026-08-04-session-owner-mailbox-design.md. Stages
	// 2.1-2.4 migrated tryReserveSession/releaseSessionReservation,
	// InterruptAndReplace, the two-tier context split, and the inject path
	// onto the mailbox; injectQueue and sessionStartMu have been deleted as
	// fully dead. activeRequests is retained for OnStepFinish abort-path cancel
	// lookups; the sessionID+"-summarize" synthetic key has been removed
	// (#268). Both are removed in later stages. One mailbox per
	// session id, created lazily on first touch via GetOrSet and never
	// explicitly deleted — same "one map, lazily populated, entries live
	// forever" lifetime as activeRequests today.
	mailboxes *csync.Map[string, *mailbox]
	// cacheKeepAlive holds one pending keep-alive timer per session, armed
	// after a turn writes to the provider's ephemeral prompt cache. See
	// agent_cache_keepalive.go.
	cacheKeepAlive *csync.Map[string, *cacheKeepAliveEntry]
	// cacheKeepAliveMu serializes schedule/fire generation check-and-act
	// sequences on cacheKeepAlive, since csync.Map has no atomic CAS.
	cacheKeepAliveMu sync.Mutex
	// cacheKeepAliveGen is a monotonic counter; each scheduled entry captures
	// the generation current at schedule time so a racing stale fire can
	// detect it has been superseded. See agent_cache_keepalive.go.
	cacheKeepAliveGen atomic.Int64
	// cacheKeepAliveInFlight holds the in-flight registration for a replay
	// call currently executing inside fireCacheKeepAlive, keyed by session id.
	// The registration carries the owning fire's generation so the rearm
	// decision (K-1) and deferred release can test ownership under
	// cacheKeepAliveMu. Deliberately separate from cacheKeepAlive/cacheKeepAliveEntry:
	// that map entry is removed BEFORE the replay call starts (so a new
	// schedule is never blocked by an old in-flight call), so overloading it
	// here would conflict with that "removed = free to reschedule" invariant.
	// See fireCacheKeepAlive and cancelCacheKeepAlive.
	cacheKeepAliveInFlight *csync.Map[string, cacheKeepAliveInFlightEntry]
	// peakHoursCheck, when non-nil, is called once per step from
	// OnStepFinish to re-check whether the turn's pinned smart model provider
	// has entered its peak_hours refusal window mid-turn. Returns nil while
	// outside the window. Plumbed from coordinator.buildAgent, which is
	// the only layer with access to config.ProviderConfig — sessionAgent
	// itself only knows about Model (SelectedModel + catwalk metadata),
	// not the provider's peak_hours setting.
	peakHoursCheck func(providerID string) error

	// runAllowlists, when non-nil, lets the turn loop activate each call's
	// carried restricted-run policy (SessionAgentCall.RunAllowlist) at
	// turn start and clear it at loop end (R3-4). Nil (tests, bare
	// fixtures) disables the mechanism entirely — no session entry is
	// ever armed.
	runAllowlists  permission.SessionRunAllowlistManager
	restrictedRuns permission.RestrictedRunAuthorizer

	// diskCaps is the coordinator's App-owned capability manager, wired at
	// construction via SessionAgentOptions.diskCaps (R12-H2b §2.4): the
	// durable enqueue producer (restartOrphanedWithRetry) holds row-keyed
	// Continuation owners through it so a queued provider-backed call's
	// binding cannot be pruned before the pump rebuilds. Nil on bare
	// fixtures.
	diskCaps *DiskProviderRegistry

	// capsMu guards diskCaps, which is bound once at batch install,
	// possibly after construction.
	capsMu sync.Mutex
}

// bindDiskCaps binds the App-owned capability manager onto an
// already-constructed session agent (batch install path).
func (a *sessionAgent) bindDiskCaps(caps *DiskProviderRegistry) {
	a.capsMu.Lock()
	defer a.capsMu.Unlock()
	a.diskCaps = caps
}

// diskCapsRef returns the bound capability manager (nil until bound).
func (a *sessionAgent) diskCapsRef() *DiskProviderRegistry {
	a.capsMu.Lock()
	defer a.capsMu.Unlock()
	return a.diskCaps
}

// SessionAgentOptions is defined in agent_options.go (pure move, phase-4
// step 3 file-size ratchet: this file was already at the 1000-line cap).

func NewSessionAgent(
	opts SessionAgentOptions,
) SessionAgent {
	return &sessionAgent{
		smartModel:                 csync.NewValue(opts.SmartModel),
		fastModel:                  csync.NewValue(opts.FastModel),
		systemPromptPrefix:         csync.NewValue(opts.SystemPromptPrefix),
		systemPrompt:               csync.NewValue(opts.SystemPrompt),
		config:                     opts.Config,
		isSubAgent:                 opts.IsSubAgent,
		sessions:                   opts.Sessions,
		messages:                   opts.Messages,
		disableAutoSummarize:       opts.DisableAutoSummarize,
		tools:                      csync.NewSliceFrom(decorateSessionTools(opts.Tools, opts.RestrictedRuns)),
		coord:                      opts.Coord,
		onSessionIdle:              opts.OnSessionIdle,
		isYolo:                     opts.IsYolo,
		notify:                     opts.Notify,
		activeRequests:             csync.NewMap[string, context.CancelFunc](),
		summarizeQueue:             csync.NewMap[string, *SummarizeSnapshot](),
		mailboxes:                  csync.NewMap[string, *mailbox](),
		cacheKeepAlive:             csync.NewMap[string, *cacheKeepAliveEntry](),
		cacheKeepAliveInFlight:     csync.NewMap[string, cacheKeepAliveInFlightEntry](),
		streamIdleTimeout:          opts.StreamIdleTimeout,
		streamWatchdogTick:         opts.StreamWatchdogTick,
		titleJoinGrace:             opts.TitleJoinGrace,
		cancelAllGrace:             opts.CancelAllGrace,
		dataDir:                    opts.DataDirectory,
		lockOptions:                opts.LockOptions,
		checkpointInterval:         opts.CheckpointInterval,
		timeoutExtendsOnProgress:   opts.TimeoutExtendsOnProgress,
		timeoutHardCap:             opts.TimeoutHardCap,
		toolMaxDuration:            opts.ToolMaxDuration,
		toolCleanupGrace:           opts.ToolCleanupGrace,
		peakHoursCheck:             opts.PeakHoursCheck,
		runAllowlists:              opts.RunAllowlists,
		restrictedRuns:             opts.RestrictedRuns,
		sessionPreambleMaxDuration: opts.SessionPreambleMaxDuration,
		titleGenerationMaxDuration: opts.TitleGenerationMaxDuration,
		titleGens:                  newTitleGenRegistry(),
		diskCaps:                   opts.diskCaps,
	}
}

func (a *sessionAgent) SetModels(smart Model, fast Model) {
	a.smartModel.Set(smart)
	a.fastModel.Set(fast)
}

// SetTools replaces the agent's tools. Like the constructor, it wraps them
// for error logging: these two are the only doors tools have into a
// sessionAgent, so covering both means no assembly site can forget to.
func (a *sessionAgent) SetTools(tools []fantasy.AgentTool) {
	a.tools.SetSlice(decorateSessionTools(tools, a.restrictedRuns))
}

func (a *sessionAgent) SetSystemPrompt(systemPrompt string) {
	a.systemPrompt.Set(systemPrompt)
}

func (a *sessionAgent) SetSystemPromptPrefix(prefix string) {
	a.systemPromptPrefix.Set(prefix)
}

func (a *sessionAgent) SystemPrompt() string {
	return a.systemPrompt.Get()
}

func (a *sessionAgent) Model() Model {
	return a.smartModel.Get()
}
