package agent

import (
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/notify"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
)

type SessionAgentOptions struct {
	SmartModel           Model
	FastModel            Model
	SystemPromptPrefix   string
	SystemPrompt         string
	IsSubAgent           bool
	DisableAutoSummarize bool
	IsYolo               bool
	Sessions             session.Service
	Messages             message.Service
	Tools                []fantasy.AgentTool
	// Coord is the owning coordinator (batch facts, reaction store, notice
	// pull route through it). Nil on bare fixtures.
	Coord *coordinator
	// OnSessionIdle -- see sessionAgent.onSessionIdle's doc. Nil in every
	// test that builds a bare sessionAgent directly (no coordinator to
	// notify).
	OnSessionIdle func(sessionID string)
	// Config is the MCP ownership scope for this agent. MCP runtime state is
	// process-wide, so every turn filters registry-derived instructions by
	// this consuming ConfigStore.
	Config *config.ConfigStore
	Notify pubsub.Publisher[notify.Notification]
	// StreamIdleTimeout overrides streamIdleTimeoutDefault when > 0.
	// Plumbed from Options.StreamIdleTimeoutSeconds in the coordinator.
	StreamIdleTimeout time.Duration
	// TitleJoinGrace overrides the package-level titleJoinGrace const
	// (10s) when > 0. Test-only seam (task #454) — production callers leave
	// this unset.
	TitleJoinGrace time.Duration
	// CancelAllGrace overrides CancelAll's own 5s runWg.Wait grace period
	// when > 0. Test-only seam (task #454) — production callers leave this
	// unset.
	CancelAllGrace time.Duration
	// DataDirectory is the absolute path to .rush/. Used by Run() to
	// acquire an inter-process file lock per session (prevents two
	// rush processes from accidentally working on the same session
	// id — see internal/session/lock.go).
	DataDirectory string
	// CheckpointInterval controls how often in-progress streaming
	// text is flushed to the DB mid-step. When > 0, a coalescing
	// ticker writes the in-memory Parts to the message row (with
	// finished_at still NULL) at most once per interval — but only
	// when Parts have actually changed since the last flush. This
	// bounds the text lost to a SIGTERM during final composition.
	// 0 (default) disables mid-stream checkpointing entirely.
	// Fork patch: batch 8 — see CHANGELOG.fork.md section 6.
	CheckpointInterval time.Duration
	// TimeoutExtendsOnProgress, when true, makes the stream watchdog
	// reset its deadline every time streaming progress occurs. This
	// prevents killing healthy long compositions. Default: false.
	// Fork patch: batch 8.
	TimeoutExtendsOnProgress bool
	// TimeoutHardCap is the maximum wall-clock time the watchdog will
	// allow even with continuous progress. Default: 0 (no cap, but
	// callers typically set 4x the idle timeout when extending).
	// Fork patch: batch 8.
	TimeoutHardCap time.Duration
	// ToolMaxDuration bounds the watchdog's tool-pause (never-freeze
	// backstop). Past it the watchdog fires with a "tool timeout" reason
	// so the turn ends instead of hanging on a stuck tool. 0 = use the
	// built-in toolExecutionMaxDefault (45m), applied uniformly to every
	// tool including sub-agent delegations. Explicitly set (> 0), this
	// ALWAYS wins over the built-in default — plumbed from
	// Options.StreamToolTimeoutSeconds in the coordinator.
	ToolMaxDuration time.Duration
	// ToolCleanupGrace overrides the resolved grace when > 0 — the buffer
	// added on top of the resolved tool-max-duration before the watchdog
	// force-cancels a tool-in-flight, giving a nested (child) watchdog
	// inside an `agent`-tool delegation a chance to fire on its own cap and
	// unwind cleanly first. See toolCleanupGraceDefault's and
	// effectiveToolCleanupGrace's docs for the full rationale. 0 = no
	// explicit override: the built-in default (toolCleanupGraceDefault)
	// applies only to a top-level (non-sub-agent) session; a sub-agent
	// session gets 0 (no grace) by default, since it can never itself be
	// waiting on a nested delegation (task #205). Primarily exposed for
	// tests that want a short, non-zero grace instead of waiting out the
	// real default.
	ToolCleanupGrace time.Duration
	// PeakHoursCheck, when non-nil, is called once per step to re-check
	// whether the turn's pinned smart model provider has entered its peak_hours
	// window mid-turn. See the field doc on sessionAgent.peakHoursCheck.
	PeakHoursCheck func(providerID string) error
	// SessionPreambleMaxDuration overrides sessionPreambleMaxDurationDefault
	// when > 0 — the bound on Run()'s DB preamble before the stream watchdog
	// starts. See sessionPreambleMaxDurationDefault's doc for the full
	// rationale. 0 = use the built-in default; primarily exposed for tests
	// that want a short bound instead of waiting out the real one.
	SessionPreambleMaxDuration time.Duration
	// diskCaps is the coordinator's App-owned capability manager
	// (R12-H2b §2.4), wired unexported at construction: the durable enqueue
	// producer on this agent (restartOrphanedWithRetry) holds row-keyed
	// Continuation owners through it. Nil on bare fixtures — those never
	// enqueue provider-carrying calls.
	diskCaps *DiskProviderRegistry
	// TitleGenerationMaxDuration overrides titleGenerationMaxDurationDefault
	// when > 0 — the bound on the background title-generation goroutine. See
	// titleGenerationMaxDurationDefault's doc for the full rationale. 0 = use
	// the built-in default; primarily exposed for tests that want a short
	// bound instead of waiting out the real one.
	TitleGenerationMaxDuration time.Duration
	// StreamWatchdogTick overrides streamWatchdogTick when > 0 — the interval
	// at which the stream watchdog checks for stalls. 0 = use the built-in
	// default (30s); primarily exposed for tests that need fast watchdog
	// behavior (e.g., P2_3 regression tests).
	StreamWatchdogTick time.Duration
	// LockOptions allows tests to inject options into SessionLock acquisition
	// (e.g., WithClearHolderMetadataFn for hung cleanup tests). Passed to
	// session.TryAcquireSessionLockWithOptions when the agent acquires
	// inter-process locks. Primarily exposed for regression tests like
	// TestP0_338_FinalizerReachableDespiteHungCleanup.
	LockOptions []session.LockOption
	// RunAllowlists, when non-nil, lets the turn loop activate each call's
	// carried restricted-run policy (SessionAgentCall.RunAllowlist) at
	// turn start and clear it at loop end (R3-4). Nil (tests, bare
	// fixtures) disables the mechanism entirely — no session entry is
	// ever armed.
	RunAllowlists permission.SessionRunAllowlistManager
	// RestrictedRuns is the final dispatch gate for per-call tool allowlists.
	RestrictedRuns permission.RestrictedRunAuthorizer
}
