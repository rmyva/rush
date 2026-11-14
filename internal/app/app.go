// Package app wires together services, coordinates agents, and manages
// application lifecycle.
package app

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/notify"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/filetracker"
	"github.com/PHPCraftdream/rush/internal/heartbeat"
	"github.com/PHPCraftdream/rush/internal/history"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/shell"
)

// coordinatorAdapterImpl wraps agent.Coordinator to satisfy session.Coordinator,
// avoiding an import cycle (session → agent → session).
//
// Reads app.AgentCoordinator lazily, on every Run() call, rather than
// capturing it once at construction time.
//
// History (P0-1, found in the final @oh review of tasks #337-349): this
// adapter used to be constructed with the run queue pump BEFORE
// InitCoderAgent assigned app.AgentCoordinator, so an eager-capture struct
// permanently captured nil, silently disabling the pump for the entire
// process lifetime. The PRIMARY fix was reordering App.New so the pump is
// now only constructed and started AFTER InitCoderAgent succeeds (see the
// pump construction site near the end of New) — by the time this adapter
// is built, app.AgentCoordinator is already correctly assigned, so an
// eager capture would work correctly too.
//
// The lazy read here is kept anyway as defense-in-depth, not as the
// primary fix — verified directly: reverting ONLY this adapter to eager
// capture (while keeping the corrected pump-start ordering) does NOT
// reproduce the original bug. It costs nothing and removes any future
// ordering hazard if App.New's construction sequence changes again,
// without depending on that ordering being preserved by every future
// edit.
type coordinatorAdapterImpl struct {
	app *App
}

func (a *coordinatorAdapterImpl) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	coord := a.app.AgentCoordinator
	if coord == nil {
		return nil, fmt.Errorf("app: coordinator not ready yet")
	}
	if handled, result, err := runMarkedFinalizationQueue(ctx, coord, callData); handled {
		return result, err
	}

	// Convert session.SessionAgentCallData to agent.SessionAgentCall
	// This requires rebuilding the full Model from the serialized ModelCfg,
	// which only the coordinator can do (it has access to provider configs
	// and catwalk registry). Delegate to coordinator.RebuildSessionAgentCall.
	// The pump stamps the leased row's durable id onto the context
	// (R12-H2b §2.4) so the rebuild can release the row-keyed Continuation
	// holder; callers outside the pump pass "".
	call, err := coord.RebuildSessionAgentCall(ctx, session.RunQueueRowIDFrom(ctx), callData)
	if err != nil {
		return nil, fmt.Errorf("failed to rebuild session agent call: %w", err)
	}

	result, err := coord.RunSessionAgentCall(ctx, call)
	if result == nil && err == nil {
		// The session was already owned by a live, in-process turn: the
		// call was appended to that owner's mailbox queue rather than
		// executed by this call (agent.sessionAgent.Run returns (nil, nil)
		// for this case — see tryReserveSession). Report this distinctly
		// so the pump does not treat it as an ordinary success (which
		// would Ack/delete a durable row for work that has not actually
		// run) — see session.ErrCallQueuedNotExecuted's doc for the full
		// rationale, found by the third @oh review pass over #337-349.
		return nil, session.ErrCallQueuedNotExecuted
	}
	// Return as any - we only care about error handling, not the result
	var anyResult any
	if result != nil {
		anyResult = result
	}
	return &anyResult, err
}

type App struct {
	Sessions               session.Service
	Messages               message.Service
	History                history.Service
	Permissions            permission.Service
	FileTracker            filetracker.Service
	BackgroundShellManager *shell.BackgroundShellManager

	AgentCoordinator agent.Coordinator

	// wakeScheduleStore is the stage-4b durable wake-schedule store
	// (internal/agent/wake_scheduler.go is the worker over it), built below
	// and handed to the coordinator in InitCoderAgent. Nil under
	// SkipAgentSetup or when dataDir=="".
	wakeScheduleStore *session.WakeScheduleStore

	// batchRuntime is the R11F app-owned batch runtime owner. Nil unless
	// the App installed the batch runtime at New (SkipAgentSetup and
	// dataDir=="" Apps never install).
	batchRuntime *BatchRuntime
	batchInstall *agent.BatchInstall

	// finalizationAuthorities is the W1c owner-to-authority registry built
	// once when the finalization flag is on: the batch commit identity and
	// the agent finalization wiring share this one instance. Nil when the
	// flag is off (nothing allocates).
	finalizationAuthorities *agent.FinalizationAuthorityRegistry

	// ephemeralDiskBindings is the capability registry for an ephemeral
	// library-mode App (dataDir == ""): such an App never installs the
	// batch runtime whose DiskBindings normally serve custom-DiskProvider
	// runs (app_run_setup.go's BindCallDiskProvider). The registry is
	// purely process-local in-memory state (internal/agent/
	// disk_provider_capability.go: no hosts/ dir, no lock files, no
	// database tables), so nothing touches the filesystem and there is no
	// Close to call — it has none (GC-managed) and is closed implicitly
	// with the App itself. Nil on persisted (dataDir != "") Apps, which bind
	// through the batch runtime install's registry; read only via
	// resolveDiskBindings.
	ephemeralDiskBindings *agent.DiskProviderRegistry

	// batchWorkIngress is the R11F (M3) exported one-authority seam over the
	// installed batch runtime. Nil when no runtime was installed; set
	// together with batchRuntime and bound to the coordinator in
	// InitCoderAgent.
	batchWorkIngress  agent.BatchWorkIngress
	batchModelControl agent.BatchModelControl
	batchLiveReader   *session.BatchLiveReader
	batchTimeoutWake  *batchTimeoutWakeDispatcher
	// batchHeldAnswerSink is the inject_agent held-question bridge over the
	// installed batch Runner (agent.NewBatchHeldAnswerSink). Nil on an App
	// without an installed runtime or a runnerless install; bound to the
	// coordinator in agent setup and cleared on shutdown with the rest of
	// the batch wiring.
	batchHeldAnswerSink agent.BatchHeldAnswerSink
	batchFaultOutputMu  sync.Mutex
	batchFaultOutput    func(error)
	batchFaultWriter    *batchFaultEnvelopeWriter

	// RunQueuePump is the background pump for durable orphaned/detached calls (task #340).
	// It scans session_run_queue periodically and executes pending work.
	RunQueuePump *session.RunQueuePump

	config *config.ConfigStore

	// DB is the underlying SQLite connection. Exposed for queue and other
	// raw-SQL features that don't have their own sqlc-generated package.
	DB func() *sql.DB

	// readDB is the read-only pool shared with the session/message services
	// (nil when it failed to open or the App was assembled by hand): the
	// driver-marker reader runs on it, off the single writer connection.
	readDB *sql.DB

	// dataDir is the path to .rush/ where the database lives. Stored here for
	// shutdown policy and diagnostics.
	dataDir string

	// workspaceRoot is this process's workspace (#1142 step C, WS-1): the
	// canonical checkout root of the ConfigStore's working directory, or ""
	// when the working directory is outside any git working tree. It is the
	// one immutable input to every WS-1 decision this package makes: the
	// workspace the session service binds new sessions to, and the workspace
	// whose sessions resolveSession and the run paths may drive. Read-only
	// once New has filled it in. It says NOTHING about home/not-home: that
	// is the separate home field below.
	workspaceRoot string

	// home is the explicit "this process owns its own data directory" flag
	// (WS-1, #1142 step C) -- the one input that decides whether legacy
	// unbound ('') rows are drivable here. It comes from the single source
	// config.WorkspaceHome() (false only for a linked worktree on the shared data
	// directory, SD-D #1143), NEVER from the
	// workspace root: a git checkout has a non-empty root and is still a
	// home process today, so deriving home from the root would refuse every
	// pre-existing legacy session to --continue, the pump and the wake
	// scheduler. Read-only once New has filled it in; ProcessHome exposes it
	// to the server package.
	home bool

	// dbConns are generation-safe release tokens for pooled connections
	// acquired during startup. Production-created Apps use these tokens.
	dbConns []*sql.DB

	// dbReleasesNeeded is retained for App values assembled by older internal
	// tests. Those values do not have dbConns and use the compatibility path.
	dbReleasesNeeded int

	// global context and cleanup functions
	globalCtx          context.Context
	cleanupFuncs       []func(context.Context) error
	mcpOwner           *mcp.Owner
	agentNotifications *pubsub.Broker[notify.Notification]
	events             *pubsub.Broker[any]

	// modelsSyncSubs are the coalescing (buffer 1) EventConfig fan-out
	// subscribers for poll-driven model changes.
	modelsSyncSubsMu sync.Mutex
	modelsSyncSubs   []chan struct{}
	modelsSyncClosed bool
	modelsSyncStop   chan struct{}
	modelsSyncSubsWG sync.WaitGroup
	modelsSyncWG     sync.WaitGroup

	// shutdownOnce makes the full App shutdown, including database release,
	// safe to repeat when a caller has both an explicit shutdown and a cleanup
	// hook.
	shutdownOnce   sync.Once
	shutdownResult ShutdownResult

	// recoveryOrphanAge — internal test seam for recoverInterruptedTurns.
	// nil = use the production default (30s). Tests set it to 0 so they
	// don't have to sleep waiting for fresh messages to "age out" before
	// recovery considers them orphans.
	recoveryOrphanAge *time.Duration

	// recoveryDataDir — internal test seam for recoverInterruptedTurns'
	// cross-process liveness probe (task #287). nil = resolve from the
	// app's own config, as production does. Tests point it at a temp dir
	// so they can seed a real session lock and drive the live-holder
	// branch without standing up a full config.
	recoveryDataDir *string
}

// Option configures New's behavior. See SkipAgentSetup.
type Option func(*newOptions)

type newOptions struct {
	skipAgentSetup      bool
	skipMCP             bool
	restrictMCPToCLI    bool
	mcpOwner            *mcp.Owner
	batchFaultOutput    io.Writer
	batchFaultSessionID string
}

// SkipAgentSetup builds the App without recoverInterruptedTurns,
// mcp.Initialize, or InitCoderAgent (task #773). For commands that only
// read/write config (`models`, `providers`, `mcp`, `login`, `queue add/
// list/show/rm/clear`) none of the three ever get used: recovery sweeps
// session/message state, mcp.Initialize starts MCP servers, and
// InitCoderAgent builds the full AgentCoordinator (incl. skill
// discovery) — all pure overhead for a config-only invocation. Callers
// that pass this option get an App with AgentCoordinator == nil and
// RunQueuePump == nil; any code path that dereferences those must not be
// reached from a command using this option (setupAppLite in
// internal/cmd/root.go documents which commands qualify).
func SkipAgentSetup() Option {
	return func(o *newOptions) { o.skipAgentSetup = true }
}

// WithBatchFaultJSONOutput installs the stdout writer and session identity used
// to publish a fault envelope when a Runner fault ends a `rush run --json`.
func WithBatchFaultJSONOutput(output io.Writer, sessionID string) Option {
	return func(o *newOptions) {
		o.batchFaultOutput = output
		o.batchFaultSessionID = sessionID
	}
}

// SkipMCP prevents App from acquiring the process-wide MCP application
// owner. Library-mode clients use this option because they have no MCP
// configuration and must never close the application's MCP registry.
func SkipMCP() Option {
	return func(o *newOptions) { o.skipMCP = true }
}

// RestrictMCPToCLI makes mcp.Initialize start only MCP servers whose
// config has EnabledInCLI set — every other non-disabled server is
// skipped instead of started. internal/cmd/root.go's setupApp passes
// this for every non-interactive invocation (rush run and every other
// CLI subcommand except the bare `rush` that starts the web UI), unless
// the run itself asked for the old behavior (rush run --all-mcp). The
// interactive web/TUI path never passes this option, so it keeps
// starting every non-disabled server exactly as before — this option
// exists to opt CLI invocations OUT of automatic MCP startup, not to
// change what the web UI does.
func RestrictMCPToCLI() Option {
	return func(o *newOptions) { o.restrictMCPToCLI = true }
}

// WithMCPOwner supplies the MCP lifecycle owner the App should use instead
// of acquiring the process-wide one itself. The SDK's application-mode Open
// passes the owner it acquired (falling back to mcp.AcquireStandalone when
// the process-wide slot is held by a live client) so two application-mode
// clients can coexist in one process, each routing its MCP operations
// through its own owner. A nil value means "not provided": New then
// acquires the process-wide owner itself, exactly as callers that omit
// this option always have. Two owners sharing one server name still share
// one registry entry; disjoint server configs remain the contract.
func WithMCPOwner(owner *mcp.Owner) Option {
	return func(o *newOptions) { o.mcpOwner = owner }
}

// MCPOwner returns the MCP lifecycle owner this App routes its MCP
// operations through, or nil when the App was built with SkipMCP (library
// mode) or SkipAgentSetup. Server handlers and agent plumbing use this to
// bind operations to this App's registry instead of resolving the
// process-current owner behind their back.
func (a *App) MCPOwner() *mcp.Owner {
	return a.mcpOwner
}

// New initializes a new application instance.
func New(ctx context.Context, conn *sql.DB, store *config.ConfigStore, opts ...Option) (*App, error) {
	var o newOptions
	for _, opt := range opts {
		opt(&o)
	}
	q := db.New(conn)
	cfg := store.Config()

	// Open a separate read-only pool alongside the writer connection so the
	// heaviest standalone read paths (call-tree CTEs behind `sessions
	// list`/`why`/`watch`, transcript pagination, `sessions grep`) run
	// concurrently with the single writer connection instead of queuing
	// behind it and behind each other. See internal/db/connect.go's
	// ConnectRead doc and session/message's NewServiceWithReader doc for the
	// full rationale.
	//
	// This is additive to db.Connect, which the caller already invoked to
	// obtain conn — ConnectRead shares that same pool entry's refCount, so
	// both returned handles are retained as generation-safe cleanup tokens
	// and released individually (one per Connect/ConnectRead call this process
	// made) to avoid leaking the reader's reference. If the reader fails to
	// open for any reason, we degrade to today's single-connection
	// behavior (qRead/readDB alias the writer) rather than failing
	// startup over a purely load-shedding optimization.
	// WS-1 (#1142 step C, docs/plans/2026-10-01-shared-data-dir.md §1): this
	// process's workspace is the canonical checkout root of the ConfigStore's
	// working directory -- deliberately ConfigStore.WorkingDir() and not
	// os.Getwd(), which differ for an SDK host that built its config against
	// another directory. Home/not-home is a SEPARATE flag from the same
	// single source, config.WorkspaceHome() (false only for a linked
	// worktree on the shared data directory -- every other process owns its
	// data directory, so every pre-existing legacy row stays drivable); it is handed alongside the root to the session
	// service and the wake-schedule store, and the agent guard and the
	// server read the same source, so no WS-1 decision in this process is
	// taken against a different notion of either value.
	workingDir := store.WorkingDir()
	workspaceRoot := config.WorkspaceRoot(workingDir)
	home := config.WorkspaceHome()

	// Heartbeat attribution: snapshots land next to the global settings file,
	// and the startup slot models are what STALE-MODEL comparisons read.
	heartbeat.SetDirFunc(heartbeatDir)
	slots := map[string]string{}
	for _, slot := range []config.SelectedModelType{
		config.SelectedModelTypeSmart, config.SelectedModelTypeFast,
		config.SelectedModelTypeWorker, config.SelectedModelTypeReviewer,
	} {
		if m, ok := cfg.Models[slot]; ok && m.Provider != "" && m.Model != "" {
			slots[slot.String()] = m.Provider + "/" + m.Model
		}
	}
	heartbeat.Init(workingDir, slots)

	var qRead *db.Queries
	var readConn *sql.DB
	dataDir := cfg.Options.DataDirectory
	if dataDir != "" {
		rc, err := db.ConnectRead(ctx, dataDir)
		if err != nil {
			slog.Warn("Failed to open read-only DB pool, reads will serialize with writes", "error", err)
		} else {
			readConn = rc
			qRead = db.New(rc)
		}
	}

	sessions := session.NewServiceWithWorkspace(q, conn, qRead, readConn, workspaceRoot, workingDir, home)
	messages := message.NewServiceWithReader(q, qRead)
	files := history.NewService(q, conn)
	skipPermissionsRequests := store.Overrides().SkipPermissionRequests
	var allowedTools []string
	if cfg.Permissions != nil && cfg.Permissions.AllowedTools != nil {
		allowedTools = cfg.Permissions.AllowedTools
	}

	app := &App{
		Sessions:               sessions,
		Messages:               messages,
		History:                files,
		Permissions:            permission.NewPermissionService(ctx, store.WorkingDir(), skipPermissionsRequests, allowedTools, q),
		FileTracker:            filetracker.NewService(q),
		BackgroundShellManager: shell.NewBackgroundShellManager(),

		DB:      func() *sql.DB { return conn },
		readDB:  readConn,
		dataDir: dataDir,

		workspaceRoot: workspaceRoot,
		home:          home,

		globalCtx: ctx,

		config:             store,
		agentNotifications: pubsub.NewBroker[notify.Notification](),
		events:             pubsub.NewBroker[any](),
	}

	if o.batchFaultOutput != nil {
		app.batchFaultWriter = &batchFaultEnvelopeWriter{writer: o.batchFaultOutput}
	}
	if app.batchFaultWriter != nil {
		writer := app.batchFaultWriter
		app.batchFaultOutput = func(fault error) {
			if err := writer.writeFault(o.batchFaultSessionID, fault); err != nil {
				slog.Error("failed to encode batch-fault JSON result", "error", err)
			}
		}
	}

	// Stage 4b: the durable wake-schedule store over the writer pool,
	// built whenever this App has a data dir and isn't a config-only
	// SkipAgentSetup command. Construction does no I/O; the worker is
	// started by InitCoderAgent's SetWakeScheduleStore (a config-only App
	// never needs the timer).
	if !o.skipAgentSetup && dataDir != "" {
		app.wakeScheduleStore = session.NewWakeScheduleStoreWithWorkspace(conn, workspaceRoot, workingDir, home)
	} else if !o.skipAgentSetup && dataDir == "" {
		// Ephemeral library-mode App (sdk.ModeLibrary with no WorkingDir):
		// no persisted batch install runs here, hence no
		// batchInstall.DiskBindings for app_run_setup.go's per-call
		// BindCallDiskProvider. A custom-DiskProvider run still needs an
		// App-owned binding registry, so build one here: purely in-memory
		// (no hosts/ dir, no lock files, no database), process-local,
		// dying with the App. Persisted Apps are untouched: their runs keep
		// binding through the batch runtime install's registry via
		// resolveDiskBindings.
		app.ephemeralDiskBindings = newEphemeralDiskBindings()
	}

	// R11F (M2): install of the batch runtime. The install is
	// unconditional for every persisted, agent-setup App. The M1 factory
	// fails closed, so a missing App resource aborts New instead of
	// silently continuing with a half-installed runtime (mirror of the
	// InitCoderAgent failure path: release the reader reference New owns,
	// close the MCP owner, return no App).
	if !o.skipAgentSetup && dataDir != "" {
		if err := app.installBatchRuntime(ctx); err != nil {
			// PE: the install may have failed AFTER the host identity was
			// ensured (half-installed runtime). Release what New owns --
			// runtime state and host identity -- before giving up; the DB
			// references below keep their task #778 ownership split.
			app.peReleaseBatchRuntimeOnNewError(context.Background())
			if app.mcpOwner != nil {
				if closeErr := app.mcpOwner.Close(context.Background()); closeErr != nil {
					slog.Error("Failed to close MCP owner after batch runtime install failure", "error", closeErr)
				}
			}
			if readConn != nil {
				if relErr := db.ReleaseConn(readConn); relErr != nil {
					slog.Error("app: failed to release read-only DB reference after batch install failure", "error", relErr)
				}
			}
			return nil, fmt.Errorf("failed to install batch runtime: %w", err)
		}
	}

	// NOTE: the restricted-run allowlist is deliberately NOT armed here.
	// app.New builds the App shared by BOTH `rush run` and the
	// interactive web/TUI server, and the gate would then leak into
	// interactive sessions — an auto-approved sub-agent (e.g.
	// agentic_fetch) would be denied-by-default even though interactive
	// mode must stay exempt. RunNonInteractive arms the gate itself from
	// config + CLI overrides on every run, so the run path is unaffected.
	// Fork patch (run allowlist).
	if !o.skipMCP && !o.skipAgentSetup {
		mcpOwner := o.mcpOwner
		if mcpOwner == nil {
			var err error
			mcpOwner, err = mcp.Acquire()
			if err != nil {
				// PE: the install already ran above; tear its
				// runtime and host identity down before returning nil.
				app.peReleaseBatchRuntimeOnNewError(context.Background())
				if readConn != nil {
					if relErr := db.ReleaseConn(readConn); relErr != nil {
						slog.Error("Failed to release read-only DB reference after MCP owner acquisition failure", "error", relErr)
					}
				}
				return nil, fmt.Errorf("failed to acquire MCP application owner: %w", err)
			}
		}
		app.mcpOwner = mcpOwner
	}

	// Startup recovery: any assistant message left without a finish part
	// from a previous run is treated as an interrupted turn — we add a
	// FinishReasonError to it so the UI/non-interactive callers don't see
	// it as still in-flight. Backs the "Codec must surface control"
	// invariant: even when the previous process died ungracefully (kill,
	// power loss, panic) we release the session on next startup. See
	// the 162-promise-all post-mortem in CHANGELOG.fork.md section 4.D.
	//
	// Skipped entirely under SkipAgentSetup (task #773): config-only
	// commands never read session/message state, so this sweep — even at
	// its post-#774 candidate-proportional cost — is pure waste for them.
	if !o.skipAgentSetup {
		app.recoverInterruptedTurns(ctx)
		if app.mcpOwner != nil {
			if len(store.Config().MCP) == 0 {
				app.mcpOwner.Initialize(ctx, app.Permissions, store, o.restrictMCPToCLI)
			} else {
				go app.mcpOwner.Initialize(ctx, app.Permissions, store, o.restrictMCPToCLI)
			}
		}
	}

	// Release the shared database connection(s) on shutdown. The pool
	// closes the underlying *sql.DB when the last reference is released.
	// One Release call is needed per Connect/ConnectRead call this process
	// made against dataDir: the caller already did one Connect (for conn)
	// before calling New, and New itself did one ConnectRead above (when
	// dataDir != "" and it succeeded) — so we release twice, matching the
	// two increments, or once if the reader was never opened (dataDir=="").
	//
	// NOTE: DB cleanup is now handled directly in Shutdown() based on
	// whether shutdown was graceful or forced, NOT via cleanupFuncs.
	// This avoids the race where a timeout-abandoned Close() goroutine
	// continues running after the process has exited.
	app.dbConns = []*sql.DB{conn}
	if readConn != nil {
		app.dbConns = append(app.dbConns, readConn)
	}
	// Run queue pump stop is now handled in Shutdown() synchronously (after CancelAll)
	// to capture the stillBusy return value, not in cleanupFuncs.
	if app.mcpOwner != nil {
		app.cleanupFuncs = append(app.cleanupFuncs, func(ctx context.Context) error {
			return app.mcpOwner.Close(ctx)
		})
	}

	// Config-only commands (SkipAgentSetup) never touch AgentCoordinator or
	// RunQueuePump, so both are left nil/zero and construction stops here.
	if o.skipAgentSetup {
		return app, nil
	}

	// TODO: remove the concept of agent config, most likely.
	if !cfg.IsConfigured() {
		slog.Warn("No agent configuration found")
		return app, nil
	}
	if err := app.InitCoderAgent(ctx); err != nil {
		// PE: installBatchRuntime already ran above and holds the batch
		// host identity (hosts/<uuid>.lock) plus the runtime goroutines.
		// No *App is returned, so Shutdown will never run: tear the
		// runtime and host identity down here (bounded, idempotent, and
		// WITHOUT touching the DB references -- readConn is released just
		// below and conn is the caller's, per the ownership split above).
		app.peReleaseBatchRuntimeOnNewError(context.Background())
		if app.mcpOwner != nil {
			if closeErr := app.mcpOwner.Close(context.Background()); closeErr != nil {
				slog.Error("Failed to close MCP owner after app initialization failure", "error", closeErr)
			}
		}
		// Ownership split (task #778): New only ever took ONE reference
		// itself — the ConnectRead above (when it succeeded; readConn is
		// nil otherwise, and releasing a dataDir with no matching Connect/
		// ConnectRead call is a documented no-op anyway). The writer
		// reference (conn) was acquired by the CALLER via db.Connect
		// BEFORE calling New, so New never owned it and must not release
		// it here — that's the caller's job on this same error path (see
		// setupApp/setupAppLite in internal/cmd/root.go). Releasing it
		// from both places would double-release and could drop refCount
		// to zero while another live caller in this process still holds
		// a legitimate reference to the same pooled entry.
		//
		// On success, this same reference is instead released exactly
		// once via app.dbReleasesNeeded in Shutdown() — this error path
		// returns no *App, so nothing else will ever release it, and
		// skipping this call would leak the pool entry (and its OS file
		// handle) forever (task #778).
		if readConn != nil {
			if relErr := db.ReleaseConn(readConn); relErr != nil {
				slog.Error("app: failed to release read-only DB reference after init failure", "error", relErr)
			}
		}
		return nil, fmt.Errorf("failed to initialize coder agent: %w", err)
	}

	// Start the run queue pump (task #340) only AFTER InitCoderAgent has
	// assigned app.AgentCoordinator. This pump ensures that once a call is
	// enqueued as durable (including by a PREVIOUS process — the queue
	// survives restarts, which is the entire point of task #340), it will
	// eventually be executed. The pump lives for the lifetime of the
	// process and is stopped during Shutdown() (see the cleanupFuncs
	// registration above, which is nil-safe and already covers this).
	//
	// This ordering (not just coordinatorAdapterImpl's lazy field read) is
	// itself required, not merely nice-to-have: starting the pump earlier
	// — even with a lazy-reading adapter — raced a fresh AgentCoordinator
	// assignment against a concurrent pump goroutine reading the same
	// unsynchronized interface field the moment a restart-recovered queue
	// already had pending work (found in the final @oh review of
	// #337-349's own fix commit for P0-1 — a genuine data race per Go's
	// memory model on a plain struct field, not just "would eventually get
	// a working value"). Starting the pump only after InitCoderAgent's
	// synchronous assignment has already happened on this same goroutine
	// removes the race entirely, and also removes the separate failure
	// mode where a pump with a permanently-nil coordinator (config never
	// configured, or InitCoderAgent failing) would otherwise treat
	// "coordinator not ready" as an ordinary retryable failure and
	// eventually dead-letter (delete) durably-accepted work it was never
	// going to get a chance to run anyway.

	// D6 (docs/plans/2026-10-15-role-models-hot-reload.md): start the OAuth
	// disk-sync poller and its journal workers under the App-owned lifecycle.
	// Starting here, right after InitCoderAgent, covers rush (web), rush run,
	// and SDK application mode; SkipAgentSetup Apps return above and stay
	// without it. releaseResources stops and joins the poller before the
	// parallel cleanupFuncs batch.
	app.config.StartOAuthDiskSync()

	// Package B (docs/plans/2026-10-15-role-models-hot-reload.md §3.4): start
	// the config disk-sync poller under the same App-owned lifecycle. Same
	// placement rules as the OAuth poller above; releaseResources stops and
	// joins it before the parallel cleanupFuncs batch.
	app.startConfigDiskSync()

	if dataDir != "" {
		app.RunQueuePump = session.NewRunQueuePump(session.RunQueuePumpConfig{
			Sessions:    app.Sessions,
			Coordinator: &coordinatorAdapterImpl{app: app},
		})
		app.RunQueuePump.Start()
		slog.Info("app: started run queue pump", "data_dir", dataDir)
	}

	return app, nil
}

// heartbeatDir places snapshots next to the global settings file. A test
// binary without an isolated global dir never writes the real registry.
func heartbeatDir() string {
	return heartbeat.DefaultDir()
}
