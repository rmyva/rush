package agent

// Fork patch: this Coordinator drives N concurrent web sessions, not a single
// TUI run. The fork-specific additions visible in the diff against upstream
// include:
//
//   - ModelOverride struct + RunWithOverrides path used by `handleSendMessage`
//     in `internal/server/handlers.go` so the WUI can pick a model per turn.
//   - TakeSummarizeQueue + queued background summarisation that does not
//     block the user's next message (paired with agent.go's sliding window).
//   - Wiring to `internal/agent/cliprovider` for npx-claude-code / Gemini /
//     Codex CLI providers, including MCP bridge initialisation.
//
// Upstream's `copilotResponsesModels` table and per-model Responses-API
// special-casing were removed when the dispatch was refactored. Keep an eye
// on that during merges: if upstream adds a new Responses-only model, the
// adapter selection in this file is where it needs to land.
//
// See CHANGELOG.fork.md sections 4.D (agent extensions) and 4.E (CLI
// providers) before resolving a merge conflict.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/notify"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/filetracker"
	"github.com/PHPCraftdream/rush/internal/history"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/PHPCraftdream/rush/internal/skills"
)

// ModelOverride allows callers to specify per-run model overrides (provider + model ID).
type ModelOverride struct {
	Provider        string
	Model           string
	ReasoningEffort string
}

// Coordinator errors.
var (
	errCoderAgentNotConfigured         = errors.New("coder agent not configured")
	errModelProviderNotConfigured      = errors.New("model provider not configured")
	errSmartModelNotSelected           = errors.New("smart model not selected")
	errFastModelNotSelected            = errors.New("fast model not selected")
	errSmartModelProviderNotConfigured = errors.New("smart model provider not configured")
	errFastModelProviderNotConfigured  = errors.New("fast model provider not configured")
	// errProviderPeakHours is returned when a provider's peak_hours
	// window refuses the request. It is operator-actionable (the user
	// configured the window on purpose) and MUST NOT be retried: the
	// condition only clears when the wall clock leaves the window,
	// which the backoff loop cannot accelerate. classifyProviderError
	// pins it to classTerminal as defense-in-depth.
	errProviderPeakHours = errors.New("provider is inside its configured peak-hours window")
)

// PeakHoursError is the concrete error checkPeakHours returns while a
// provider is inside its configured peak_hours window. It carries the exact
// reopen time as a time.Time (not just formatted into the error string) so
// callers that need to act on it precisely — e.g. an orchestrating agent
// scheduling a resume — don't have to parse Error()'s text.
type PeakHoursError struct {
	ProviderID string
	Start, End string // HH:MM, as configured
	ReopensAt  time.Time
	// Message is the operator-authored config.PeakHoursWindow.Message, if
	// any — appended to PeakHoursGuidance's output.
	Message string
}

func (e *PeakHoursError) Error() string {
	return fmt.Sprintf(
		"provider %s is in peak hours (%s–%s), refusing until %s",
		e.ProviderID, e.Start, e.End, e.ReopensAt.Format("15:04"),
	)
}

// Unwrap lets errors.Is(err, errProviderPeakHours) keep working for callers
// that only care about the error class, not the structured detail.
func (e *PeakHoursError) Unwrap() error { return errProviderPeakHours }

// errAwaitingAnswer classifies AwaitingAnswerError the same way
// errProviderPeakHours classifies PeakHoursError: it lets callers that only
// care about the error class (not the structured question/options/session
// detail) use errors.Is without knowing the concrete type.
var errAwaitingAnswer = errors.New("agent asked a question and is awaiting an operator/orchestrator answer")

// AwaitingAnswerError is the sentinel error the (forthcoming) question tool
// returns to force-stop the current turn instead of blocking on a synchronous
// answer — this fork's `rush run` has no code path that can wait mid-turn
// for operator input (both headless and web sessions auto-approve
// permissions unconditionally, see internal/server/handlers.go:163-171), so
// "ask a question" has to mean "stop the turn cleanly and hand the operator
// an unambiguous resume command" rather than "block until answered".
//
// Structurally this mirrors PeakHoursError: a concrete, typed error carrying
// exactly what the orchestrator-facing guidance needs (the question, any
// suggested answer options, and the session id to resume), so callers don't
// have to parse Error()'s prose to act on it.
type AwaitingAnswerError struct {
	Question  string
	Options   []string
	SessionID string
}

func (e *AwaitingAnswerError) Error() string {
	return fmt.Sprintf(
		"agent asked a question and is awaiting an answer (session %s): %s",
		e.SessionID, e.Question,
	)
}

// Unwrap lets errors.Is(err, errAwaitingAnswer) keep working for callers
// that only care about the error class, not the structured detail.
func (e *AwaitingAnswerError) Unwrap() error { return errAwaitingAnswer }

// maxConsecutiveAutoResumes bounds Phase 4 autonomous idle-resumes per session
// without human involvement (reset by any human message). Anti-runaway: an
// agent that keeps backgrounding self-completing jobs cannot loop forever.
const maxConsecutiveAutoResumes = 5

type Coordinator interface {
	// INFO: (kujtim) this is not used yet we will use this when we have multiple agents
	// SetMainAgent(string)
	Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	// RunWithOverrides is like Run but allows overriding the smart and/or fast model for this call.
	RunWithOverrides(ctx context.Context, sessionID, prompt string, smart, fast *ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	Cancel(sessionID string)
	CancelAll() (stillBusy bool)
	IsSessionBusy(sessionID string) bool
	IsBusy() bool
	// ReserveExclusive atomically claims exclusive ownership of sessionID
	// without starting a turn (task #614). See SessionAgent.ReserveExclusive
	// for the full contract — in particular that the returned holdCtx/epoch/cancel
	// tuple must be handed to exactly one of RunWithReservedOwnership or
	// ReleaseExclusive, exactly once. ok is false (fail closed) when the
	// session is already busy or the session's mailbox is hard-stopped
	// (the CancelAll/shutdown latch — see mailbox.hardStop). This is a
	// pure mailbox-state check: it does NOT consult the coordinator's own
	// readiness gate (readyWg), so a coordinator whose parent ctx has
	// already been cancelled still grants reservations.
	ReserveExclusive(ctx context.Context, sessionID string) (holdCtx context.Context, epoch uint64, cancel context.CancelFunc, ok bool)
	// ReleaseExclusive drops a reservation taken by ReserveExclusive without
	// running a turn. Use on any bail-out path that will not call
	// RunWithReservedOwnership.
	ReleaseExclusive(sessionID string, epoch uint64, cancel context.CancelFunc)
	// RunWithReservedOwnership runs prompt for sessionID using ownership
	// already claimed by ReserveExclusive, continuing the SAME ownership
	// era instead of releasing and re-claiming it — this is what lets a
	// caller (handleRerunMessage) hold exclusive ownership across deleting
	// history and starting the replacement turn with no gap in between for
	// a concurrent Send/Rerun to slip through. smart/fast follow the same
	// override semantics as RunWithOverrides (nil means "use session/config
	// defaults"). onHandoff, if non-nil, is invoked immediately before the
	// handoff to the agent layer; it is used by the caller to transfer
	// release responsibility.
	RunWithReservedOwnership(ctx context.Context, sessionID, prompt string, epoch uint64, cancel context.CancelFunc, onHandoff func(), smart, fast *ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	QueuedPrompts(sessionID string) int
	QueuedPromptsList(sessionID string) []string
	ClearQueue(sessionID string)
	// InterruptAndSend queues a new user message and cancels the running
	// turn so the queued message picks up immediately with everything
	// produced so far retained in history.
	InterruptAndSend(ctx context.Context, sessionID, prompt string, smart, fast *ModelOverride, attachments ...message.Attachment) error
	// InjectMessage persists a user message and, if the session is currently
	// running, schedules it to be merged into the next provider request
	// without cancelling the in-flight turn. See SessionAgent.InjectMessage.
	InjectMessage(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (message.Message, error)
	// Summarize compresses the session history. If the session is currently
	// busy the request is queued; call TakeSummarizeQueue after the task
	// finishes to pick it up. Returns ErrSummarizeQueued when queued.
	//
	// The snapshot contains the model, provider options, and prompt prefix
	// resolved from the target session (or shared state for sessions without
	// overrides), ensuring the entire summarize operation uses consistent
	// configuration regardless of concurrent SetModels calls (task #341).
	Summarize(context.Context, string, *SummarizeSnapshot) error
	SummarizeQueued(sessionID string) bool
	TakeSummarizeQueue(sessionID string) (*SummarizeSnapshot, bool)
	CancelQueuedSummarize(sessionID string)
	Model() Model
	UpdateModels(ctx context.Context) error
	GetSystemPrompt() string
	BuildSystemPrompt(ctx context.Context) (string, error)
	BuildSystemPromptForSession(ctx context.Context, sessionID string) (string, error)
	UpdateSessionSystemPrompt(ctx context.Context, sessionID, prompt string) error
	// SetAgentTimeoutOptions configures the stream watchdog's deadline
	// extension on the current agent. Called from RunNonInteractive when
	// --timeout-extends-on-progress is set. Fork patch: batch 8.
	SetAgentTimeoutOptions(extendsOnProgress bool, hardCap time.Duration)
	// SetRunLimits sets cost and token caps for the next Run call.
	// Fork patch: batch 30.
	SetRunLimits(maxCost float64, maxTokens int64)
	// SetActiveModelRole records which named model slot (smart, fast,
	// worker, reviewer) is driving the CURRENT top-level run, so sub-agent
	// spawns can decide whether to prefer the cheaper Worker slot instead of
	// blindly inheriting the parent's Smart model. An empty/unset value
	// means "unknown — treat as smart": the interactive TUI/web path never
	// calls this, and for those the default behavior — use Smart for
	// everything, i.e. exactly today's behavior — is correct, since
	// "Smart" = the default slot. Fork patch (reviewer/worker roles).
	SetActiveModelRole(role config.SelectedModelType)
	// SetAllowPeakHours arms a one-shot bypass of the peak-hours refusal
	// for the next Run call. It exists so `rush run --allow-peak-hours`
	// can override an operator-configured peak_hours window for a single
	// conscious invocation without introducing a persistent "always
	// allow" config setting. Fork patch (peak-hours bypass).
	SetAllowPeakHours(allow bool)
	// SetPersistentMode marks this coordinator as the long-lived web/interactive
	// server (enables Phase 4 autonomous idle-resume eligibility). rush run
	// leaves it false.
	SetPersistentMode(persistent bool)
	// ResetAutoResumeCounter clears the Phase 4 consecutive-auto-resume bound
	// for a session. Called from the human send path so a human re-entering the
	// loop re-arms autonomy.
	ResetAutoResumeCounter(sessionID string)
	// RebuildSessionAgentCall reconstructs a full SessionAgentCall from SessionAgentCallData
	// for run queue pump execution (task #340, ROUND 3 migration).
	// rowID is the leased run-queue entry's durable id (R12-H2b §2.4):
	// the rebuild releases the row-keyed Continuation holder through it.
	// Callers without a row id pass "".
	RebuildSessionAgentCall(ctx context.Context, rowID string, data session.SessionAgentCallData) (SessionAgentCall, error)
	// RunSessionAgentCall executes a SessionAgentCall directly for run queue pump execution
	// (task #340, ROUND 3 migration). This bypasses the normal buildCall path since the
	// call is already fully reconstructed with all necessary data.
	RunSessionAgentCall(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error)
	// CancelTurn cancels ONLY sessionID's live generation (its own agent's
	// Cancel): no job stop, no delegation-tree walk, no wake zeroing, no
	// auto-resume suspension -- unlike Cancel, which is the full Stop. Rerun
	// uses it so a kept history's running jobs, unreacted debt and autonomy
	// survive the rerun (docs/reviews/2026-09-29-async-phase4-round1-rerun-
	// design.md).
	CancelTurn(sessionID string)
	// BatchRerunStopVoided stops the executors a committed batch rerun
	// truncation voided (session.BatchStore.BatchRerunReconcile's Voided
	// node set): each still-running local root incarnation is stopped with
	// Stop semantics, and every voided delegation's child tree is stopped
	// like Stop (stopTree). Best effort, called strictly AFTER the
	// truncation transaction committed -- a stop that fails or cannot reach
	// a foreign host's executor is safe: the rows are already void, so the
	// completion commits void (no debt, never pulled). See
	// batch_cutover_rerun.go (the BatchRerunStopper surface).
	BatchRerunStopVoided(ctx context.Context, sessionID string, voided []session.BatchRerunVoidedNode)
}

type coordinator struct {
	cfg         *config.ConfigStore
	sessions    session.Service
	messages    message.Service
	permissions permission.Service
	history     history.Service
	filetracker filetracker.Service
	prompt      *prompt.Prompt
	notify      pubsub.Publisher[notify.Notification]
	background  *shell.BackgroundShellManager
	// inlineWindowDisabled is the SetInlineWindowForTest seam: when set, the
	// bash delivery window is off (the pre-A14 "started" flow).
	inlineWindowDisabled atomic.Bool
	// notifyWG joins the async-completion wake goroutines, so a caller (a
	// test, a shutdown) can wait for their last store access.
	notifyWG sync.WaitGroup
	// callbackMu fences release/notification registration against shutdown Wait.
	// The context is lazy so bare coordinator fixtures retain the same lifecycle.
	callbackMu      sync.Mutex
	callbackClosing bool
	callbackCtx     context.Context
	callbackCancel  context.CancelFunc
	releaseWG       sync.WaitGroup
	// Signal-only lifecycle seams; nil in production, installed before use.
	releaseCallbackHook   func(context.Context)
	notifyCallbackHook    func(context.Context)
	callbackAfterWaitHook func()
	callbackJoinTimeout   func() <-chan time.Time
	// driverHints is the rehomed in-process coordination memory the deleted
	// workLedger carried (batch_rehome_coord_runtime.go): the per-session
	// fact-hint counter and this process's external-driver mirror.
	driverHints *batchDriverHints
	// supervision is the per-root supervision countdown registry; timeouts
	// is the single per-process deadline timer supervision arms through.
	// See supervision.go / timeout_service.go.
	supervision *supervisionRegistry
	timeouts    *timeoutService
	// noticeStore is the retained session_notices writer the completion
	// notice paths use (supervision tick, reaction-chain marker, bg-shell
	// done, held child question); bound via SetCoordinatorSessionNoticeStore.
	noticeStore *session.SessionNoticeStore
	// subAgentDrivers maps a delegated child session id to the SessionAgent
	// driving it, so a wake for that child's own async work never races a
	// second SessionAgent for its OS session lock. See
	// coordinator_subagent_drivers.go.
	subAgentDrivers *subAgentDriverRegistry

	// batchDriverClaimer is the durable session-driver-marker store
	// (session.HostStore): claim/release AND the foreign-driver read go
	// through it. See batch_cutover_driver.go / batch_rehome_coord_runtime.go.
	batchDriverClaimer BatchDriverClaimer

	// diskCaps is the App-owned capability manager bound at batch install
	// (disk_provider_capability.go); nil on flag-off Apps and bare
	// fixtures. PrepareContinuation resolves continuation bindings ONLY
	// through it.
	diskCaps *DiskProviderRegistry
	// execReqs is the durable session-execution-fence store (R12-H2b);
	// nil means no fence can exist (bare fixtures), which reads as "no
	// requirement" in PrepareContinuation.
	execReqs session.SessionExecutionRequirementStore

	// wakeScheduler is the stage-4b durable wake-schedule worker (one timer
	// goroutine over WakeScheduleStore.NextDue), wired via
	// SetWakeScheduleStore from internal/app; nil until then.
	wakeScheduler *wakeScheduler

	// sharedBuildScopeMu/sharedBuildScope guard the coordinator-level build
	// scope for tool assemblies WITHOUT trusted call state (the shared
	// session toolset). Per-call builds always use the call's own scope;
	// this one is never used for a custom-provider construction.
	sharedBuildScopeMu sync.Mutex
	sharedBuildScope   *buildResourceScope

	// mcpOwner is this config's MCP lifecycle owner (task #923). Nil keeps
	// the legacy process-current-owner resolution via the package functions.
	mcpOwner *mcp.Owner

	// credentialedRoots is sticky process-local knowledge: fail closed for any root
	// that has ever carried tenant credentials; a later Drain must not resolve
	// against process configuration.
	credentialedRootsMu sync.Mutex
	credentialedRoots   map[string]struct{}

	currentAgent SessionAgent
	agents       map[string]SessionAgent

	// Skills discovery results (session-start snapshot).
	allSkills    []*skills.Skill // Pre-filter: all discovered after dedup.
	activeSkills []*skills.Skill // Post-filter: active skills only.
	skillTracker *skills.Tracker

	// readyWg gates every run entry point on the asynchronous half of agent
	// construction (buildAgent's prompt/tool builds). Deliberately NOT an
	// errgroup.Group: buildAgent re-registers on this same gate from every
	// UpdateModels, while other concurrent runs are already parked in Wait —
	// which errgroup and the sync.WaitGroup under it explicitly forbid ("The
	// first call to Go must happen before a Wait") and which -race reports as
	// a real data race. See readyGate's doc comment in ready_gate.go.
	readyWg readyGate

	// Per-run limits. Set via SetRunLimits before Run(). Reset after use.
	// Fork patch: batch 30. Mutex added in review-fix (data race: SetRunLimits
	// called from HTTP handler, read in runInternal on agent goroutine).
	runLimitsMu sync.Mutex
	maxCost     float64
	maxTokens   int64

	// allowPeakHours is a one-shot bypass for the peak-hours refusal,
	// armed by SetAllowPeakHours from `rush run --allow-peak-hours`.
	// Reset to false after the next Run. Fork patch (peak-hours bypass).
	allowPeakHours bool

	// activeModelRole records which named model slot is driving the current
	// top-level run, set via SetActiveModelRole. Static per-process (unlike
	// maxCost/maxTokens above, there is no reset-after-use — `rush run` is
	// single-shot). Mutex guards the same race shape as runLimitsMu:
	// SetActiveModelRole is called from RunNonInteractive before the agent
	// goroutine starts, and buildAgentModels reads it from the agent
	// goroutine. Fork patch (reviewer/worker roles).
	activeModelRoleMu sync.Mutex
	activeModelRole   config.SelectedModelType

	// rateLimitNotify is the optional app-installed callback that surfaces
	// each provider rate-limit wait; see coordinator_rate_limit.go.
	rateLimitNotify atomic.Pointer[func(sessionID string, until time.Time, attempt int)]

	// Phase 4 autonomous idle-resume guardrails.
	// persistentMode: true only for the long-lived web server; false for
	// rush run. Currently written exactly once at process start (no real
	// race today), but every sibling field here (allowPeakHours,
	// activeModelRole, maxCost) is already lock/atomic-guarded, so a plain
	// bool would be a silent trap for the next caller who adds a second
	// SetPersistentMode call path — atomic.Bool costs nothing and keeps
	// this field consistent with its neighbors under `go test -race`.
	persistentMode atomic.Bool
	// arb is the turn arbiter's ONE mutable state (R-ARB-2, design sec.2.3):
	// holds, suspension, the bg-shell cap and its over-cap set, the reaction
	// chain, and the per-session launch gate -- one struct per session under
	// one mutex. See turn_arbiter_state.go.
	arb arbiter
	// bgArrival makes a completion's "insert the notice row + claim/refuse a
	// slot" one step relative to the cap check that reads both.
	bgArrival ctxMutex

	// recheckMu/recheckSet back doc sec.3.4 rule (b)/sec.3.5's 60s pass (the
	// web process, and `rush run` through ClaimExternalDriver's ticker): a
	// session whose launch must be retried -- refused by an admission gate (the
	// session-lock held by another process, shutdown), paced by the launch
	// gate, held by a rerun, or whose launch decision could not be read -- is
	// never forgotten: it goes here, and RecheckPass (coordinator_recheck.go)
	// retries it on the next tick rather than losing the wake.
	recheckMu   sync.Mutex
	recheckSet  map[string]struct{}
	recheckOnce sync.Once
	recheckStop context.CancelFunc
	// recheckDone is closed when the ticker goroutine actually returns
	// (mirrors startInterruptTicker's identical done-channel pattern,
	// coordinator_interrupt.go) -- lets a test (or a future graceful-
	// shutdown path) observe the goroutine's real exit instead of the
	// runtime's noisy, non-deterministic NumGoroutine() count.
	recheckDone chan struct{}
	// recheckWakeInFlight (guarded by recheckMu) is the set of sessions with
	// a detached recheck-pass wake running right now: one per session, and
	// its size is the concurrency bound. recheckWakes lets a caller wait for
	// them, and for the detached wakes of bg-shell completions, to finish
	// (waitRecheckWakes).
	recheckWakeInFlight map[string]struct{}
	recheckWakes        sync.WaitGroup

	// modelCache caches resolved (smart, fast) Model pairs keyed by their
	// combined provider+model+reasoning_effort tuple. Used by
	// resolveSessionModels to avoid rebuilding the same pair repeatedly.
	// Cached as a pair (not two independent per-slot entries) so a single
	// buildModelsFromCfg call always fills both roles together — see
	// resolveSessionModels's own comment for why a per-slot cache
	// previously mismatched smart/fast roles.
	modelCache       modelPairCache
	modelCacheMu     sync.Mutex
	modelCacheEpoch  uint64
	modelPairBuilder func(context.Context, *config.Config, config.SelectedModel, config.SelectedModel) (Model, Model, error)

	// refreshOAuth2TokenFn is nil in production. Tests use it to install a
	// deterministic refreshed provider credential without network traffic.
	refreshOAuth2TokenFn func(context.Context, config.ProviderConfig) error
}

// cachedModelPair holds a resolved (smart, fast) Model pair as built
// together by a single buildModelsFromCfg call.
type cachedModelPair struct {
	smart Model
	fast  Model
}

// modelPairCache is the small cache surface used by model resolution. Keeping
// it narrow preserves compatibility with existing csync.Map fixtures.
type modelPairCache interface {
	Len() int
}

// modelCacheMaxEntries bounds the number of resolved model pairs retained by a
// production coordinator. Entries are LRU-evicted, so historical config
// generations and arbitrary per-session overrides cannot grow without bound.
const modelCacheMaxEntries = 16

// NewCoordinator wires the agent coordinator. Async work is batch-only: the
// jobbatch engine (batch_nodes + the batch outbox) decides every outcome; the
// coordinator's own runtime state (hint counter, supervision registry,
// timeout service) is in-process coordination memory rehomed from the deleted
// workLedger (batch_rehome_coord_runtime.go).
func NewCoordinator(
	ctx context.Context,
	cfg *config.ConfigStore,
	sessions session.Service,
	messages message.Service,
	permissions permission.Service,
	history history.Service,
	filetracker filetracker.Service,
	notify pubsub.Publisher[notify.Notification],
	mcpOwner *mcp.Owner,
	backgroundManagers ...*shell.BackgroundShellManager,
) (Coordinator, error) {
	p, err := coderPrompt(prompt.WithWorkingDir(cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}

	// Discover skills once at session start.
	allSkills, activeSkills := discoverSkills(cfg)
	skillTracker := skills.NewTracker(activeSkills)

	background := shell.NewBackgroundShellManager()
	if len(backgroundManagers) > 0 && backgroundManagers[0] != nil {
		background = backgroundManagers[0]
	}

	c := &coordinator{
		cfg:          cfg,
		sessions:     sessions,
		messages:     messages,
		permissions:  permissions,
		history:      history,
		filetracker:  filetracker,
		prompt:       p,
		notify:       notify,
		background:   background,
		mcpOwner:     mcpOwner,
		agents:       make(map[string]SessionAgent),
		allSkills:    allSkills,
		activeSkills: activeSkills,
		skillTracker: skillTracker,
		modelCache:   newBoundedModelPairCache(modelCacheMaxEntries),
	}
	// The hint counter, supervision registry and the shared timeout service
	// are in-process coordination memory rehomed from the deleted workLedger
	// (batch_rehome_coord_runtime.go / supervision.go).
	c.driverHints = newBatchDriverHints()
	c.supervision = newSupervisionRegistry()
	c.timeouts = newTimeoutService()
	c.subAgentDrivers = newSubAgentDriverRegistry()

	agentCfg, ok := cfg.Config().Agents[config.AgentCoder]
	if !ok || agentCfg.ID == "" {
		// Self-heal: config.Load/reload always call SetupAgents once
		// IsConfigured() becomes true, but a caller that mutates
		// Providers/SelectedModel directly on an already-published config
		// (bypassing Load/reload entirely — a test-only pattern; found via
		// a CI-only failure this exact class of gap caused,
		// errCoderAgentNotConfigured, that never reproduced on a dev
		// machine because cliprovider.Available() synthesizes a local-cli
		// provider whenever claude/gemini/codex/qwen is on PATH, making
		// IsConfigured() true at initial Init regardless of any
		// RUSH_GLOBAL_*/XDG_* isolation — confirmed by the sixth @oh
		// review pass) never triggers that population. Also guards against
		// Agents[AgentCoder] being present but zero-value (ok==true,
		// ID=="") — the bare `!ok` check alone would skip self-heal for
		// that case and build a coder agent with an empty ID/AllowedTools.
		// SetupAgents is idempotent (derives Agents purely from Options/
		// DisabledTools, no I/O), so re-deriving it here on a genuine miss
		// is safe. See
		// p350_coder_agent_selfheal_test.go for a deterministic
		// regression test that does not depend on any environment leak.
		cfg.SetupAgents()
		agentCfg, ok = cfg.Config().Agents[config.AgentCoder]
	}
	if !ok {
		return nil, errCoderAgentNotConfigured
	}

	agent, err := c.buildAgent(ctx, p, agentCfg, false)
	if err != nil {
		return nil, err
	}
	c.currentAgent = agent
	c.agents[config.AgentCoder] = agent
	return c, nil
}

// Run implements Coordinator.
func (c *coordinator) Run(ctx context.Context, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	// R12-H2a round 2: resolution builds this call's toolset into the
	// trusted call state's build-resource scope before runInternal runs, so
	// every return from here — resolution failure included — must release
	// it. Close is idempotent; runInternal's own defer double-closes
	// harmlessly.
	if callOpts := callOptionsFrom(ctx); callOpts != nil {
		defer callOpts.buildScope.close()
	}
	if err := c.readyWg.Wait(); err != nil {
		return nil, c.drainRefused(ctx, sessionID, err)
	}

	// Resolve the session's model configuration from the DB or config defaults.
	// This always returns a valid snapshot (never nil), ensuring that every turn
	// runs with a complete, self-contained model configuration.
	pinned, err := c.resolveSessionModels(ctx, sessionID)
	if err != nil {
		return nil, c.drainRefused(ctx, sessionID, fmt.Errorf("failed to resolve session models: %w", err))
	}

	return c.runInternal(ctx, sessionID, prompt, pinned, attachments...)
}

// SetRunLimits stores cost and token caps for the next Run call.
// Fork patch: batch 30.
func (c *coordinator) SetRunLimits(maxCost float64, maxTokens int64) {
	c.runLimitsMu.Lock()
	c.maxCost = maxCost
	c.maxTokens = maxTokens
	c.runLimitsMu.Unlock()
}

// SetActiveModelRole records which named model slot is driving the current
// top-level run. Fork patch (reviewer/worker roles).
func (c *coordinator) SetActiveModelRole(role config.SelectedModelType) {
	c.activeModelRoleMu.Lock()
	c.activeModelRole = role
	c.activeModelRoleMu.Unlock()
}

// SetAllowPeakHours arms a one-shot bypass of the peak-hours refusal
// for the next Run call. Fork patch (peak-hours bypass).
func (c *coordinator) SetAllowPeakHours(allow bool) {
	c.runLimitsMu.Lock()
	c.allowPeakHours = allow
	c.runLimitsMu.Unlock()
}

// SetPersistentMode marks this coordinator as the long-lived web/interactive
// server (Phase 4 autonomous idle-resume eligibility). rush run leaves it
// false.
func (c *coordinator) SetPersistentMode(persistent bool) {
	c.persistentMode.Store(persistent)
	if persistent {
		// Doc sec.3.5: hints live only inside this process, so the web
		// process runs the 60s host-level pass (RecheckPass). `rush run` does
		// not call this: ClaimExternalDriver starts the same ticker for it,
		// and it also runs RunMaintenanceSweep once at loop start and
		// re-reads the DB itself while it waits.
		c.StartRecheckTicker()
	}
}

// autonomyEnabled reports whether Phase 4 auto-resume is opted in via config.
func (c *coordinator) autonomyEnabled() bool {
	opts := c.cfg.Config().Options
	return opts != nil && opts.AutoResumeOnJobDone != nil && *opts.AutoResumeOnJobDone
}

// consecutiveResume returns the number of SDK background-shell auto-resumes
// for sessionID since the last human message (the cap counter only; Stop's
// suspension is autoResumeSuspended).
func (c *coordinator) consecutiveResume(sessionID string) int {
	return c.arb.autoResumesOf(sessionID)
}

// resetConsecutiveResume clears, in the arbiter's one state, everything a
// human message re-arms: the bg-shell cap counter and its over-cap set, Stop's
// suspension, the reaction chain, await_tasks' sleep (its max_wait schedule
// is cancelled too), and the launch gate.
func (c *coordinator) resetConsecutiveResume(sessionID string) {
	c.clearSleepAll(sessionID)
	c.arb.resetForHumanMessage(sessionID)
}

// ResetAutoResumeCounter is the exported wrapper around resetConsecutiveResume
// for the server package's human send path.
func (c *coordinator) ResetAutoResumeCounter(sessionID string) {
	c.resetConsecutiveResume(sessionID)
}

// suspendAutoResume implements doc sec.3.4's "after Stop, automatic turns
// are suspended until the next human message": marks sessionID suspended so
// both autoResumeEligible and the arbiter (rule 4) refuse EVERY kind of
// automatic turn until a human message (ResetAutoResumeCounter) clears it.
// Kept apart from the bg-shell cap counter: that counter bounds only the SDK
// background-shell auto-resume and must not pause other wakes when full.
func (c *coordinator) suspendAutoResume(sessionID string) {
	c.arb.suspend(sessionID)
}

// autoResumeSuspended reports whether Stop suspended automatic turns for
// sessionID and no human message has lifted it yet.
func (c *coordinator) autoResumeSuspended(sessionID string) bool {
	return c.arb.suspended(sessionID)
}

// autoResumeEligible reports whether a finished background job should
// autonomously resume the (idle-or-busy; Run handles that) owning session.
// Pure autonomy policy: opt-in config, persistent (web) coordinator only, not
// Stop-suspended, and under the consecutive-resume runaway bound. Per-turn cost/token caps are
// still enforced by the normal Run path; a Cancel aborts the auto-turn like any
// other. NEVER eligible for rush run (persistentMode stays false there).
func (c *coordinator) autoResumeEligible(sessionID string) bool {
	return c.autonomyEnabled() &&
		c.persistentMode.Load() &&
		!c.autoResumeSuspended(sessionID) &&
		c.consecutiveResume(sessionID) < maxConsecutiveAutoResumes
}

// claimAutoResumeSlot spends one of maxConsecutiveAutoResumes bg-shell
// auto-resume slots for sessionID: the arbiter's ONE writer of the cap
// (R2B-16). A completion refused for a spent cap is recorded by its notice
// row id (rowID 0: the insert failed, nothing exists to defer); a
// Stop-suspended session spends nothing. Called under bgArrival
// (persistBGShellCompletion); eligible is the coordinator's own policy answer
// (persistent web coordinator with AutoResumeOnJobDone on).
func (c *coordinator) claimAutoResumeSlot(sessionID string, rowID int64, eligible bool) bool {
	return c.arb.claimAutoResumeSlot(sessionID, rowID, eligible)
}

// SetAgentTimeoutOptions delegates to the current agent's SetTimeoutOptions.
// Fork patch: batch 8.
func (c *coordinator) SetAgentTimeoutOptions(extendsOnProgress bool, hardCap time.Duration) {
	c.currentAgent.SetTimeoutOptions(extendsOnProgress, hardCap)
}
