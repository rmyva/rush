// R11F (M2) batch-runtime installation: App.New builds the batch runtime
// via the M1 factory unconditionally for every persisted agent-setup App,
// and the shutdown path replaces the "release host identity and writer on
// agent-idle" sites with the R11E drain gate (BatchRuntime.DrainAndRelease).
package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/google/uuid"
)

// appBatchDrainTimeout bounds the Runner drain inside an ordinary
// (graceful) App shutdown. A Close that outlives it makes the gate retain
// the still-draining host identity and writer and report Forced — the same
// verdict the agent CancelAll grace produces, and the same consequence:
// the caller must exit its process. 5s matches the per-cleanup shutdownCtx
// budget of releaseResources.
const appBatchDrainTimeout = 5 * time.Second

// batchRuntimeConfigHook is a test-only seam applied to the runtime config
// right after the factory build and BEFORE the fault watcher starts: tests
// inject WaitFunc/ExitFunc to exercise fault/timeout paths in-process. Nil
// in production; production code must never set it.
var batchRuntimeConfigHook func(*BatchRuntimeConfig)

// batchInstallDepsHook is a test-only, per-install dependency decorator.
// Nil in production; tests restore it immediately after New returns.
var batchInstallDepsHook func(*agent.BatchInstallDeps)

// batchInstallWiringHook observes the completed app-owned wake wiring in tests.
// Nil in production; tests restore it immediately after New returns.
var batchInstallWiringHook func(agent.BatchInstallDeps)

// arms its fault watcher. It fails closed: any missing resource surfaces as
// a New error — the process never silently continues with a half-installed
// runtime. Caller must have ensured agent setup and a non-empty data dir.
func (app *App) installBatchRuntime(ctx context.Context) error {
	if app.batchRuntime != nil {
		// First wins: one install, one authority per App. A second call
		// (only possible from tests/re-entry) never swaps the Runner.
		return nil
	}
	// R11F (M3): the commit-hook identity must exist BEFORE the first Claim,
	// so the install registers the host up front (HostID() is empty until
	// the lazy ensureHost registration). The identity is the job-independent
	// HostStore's.
	host, err := app.batchHostEnsure(ctx)
	if err != nil {
		return err
	}
	deps := agent.BatchInstallDeps{
		// R11F round-2 (P1-1) decision — delivery ownership: the install
		// carries NO fixed owner. Owner=host was an FK violation on the
		// first Submit (batch_nodes.owner_session_id REFERENCES
		// sessions(id); async_hosts rows are not sessions). The commit hook
		// resolves every Submit's owner from the INVOKING session carried
		// in the spec payload (BatchCommitIdentity.OwnerFor); deliveries
		// and elections fence by the invocation's session
		// (batchDeliveryImpl.owner); Host stays ONLY the fence identity.
		Host: host, RunnerID: uuid.NewString(),
		DiskBindings: agent.NewDiskProviderRegistry(),
	}
	// W1c: the flag-on registry must exist before the batch install so the
	// commit-hook identity carries the same lookup the finalization wiring
	// registers authorities into. Flag off: nil, byte-identical install.
	if registry := app.finalizationAuthorityRegistry(); registry != nil {
		deps.FinalizationAuthorityFor = registry.Lookup
	}
	if batchInstallDepsHook != nil {
		batchInstallDepsHook(&deps)
	}
	factory, err := newBatchRuntimeFactory(app, deps)
	if err != nil {
		return err
	}
	app.batchTimeoutWake = newBatchTimeoutWakeDispatcher()
	// Ordinary completions ride the same bounded dispatcher: the commit
	// hook enqueues the touched root (nonblocking, off the Runner commit
	// stack); the dispatcher resolves the owner from the durable row and
	// hands it to the coordinator's existing wake gates. The dispatcher
	// is NOT a delivery authority — the durable selected notice stays
	// the only fact; the wake only schedules the reaction turn.
	app.batchTimeoutWake.rootOwner = pwBatchCompletionRootOwner(app)
	app.batchTimeoutWake.skipOwner = apiServiceOwner(app)
	factory.wake = app.batchTimeoutWake.Wake
	factory.deps.CompletionWake = app.batchTimeoutWake.WakeCompletion
	factory.deps.QuestionWake = app.batchTimeoutWake.Wake
	if batchInstallWiringHook != nil {
		batchInstallWiringHook(factory.deps)
	}
	if factory.deps.Profile == nil {
		profile, perr := agent.NewBatchProfileDefault()
		if perr != nil {
			if app.batchTimeoutWake != nil {
				app.batchTimeoutWake.Stop()
				app.batchTimeoutWake = nil
			}
			return perr
		}
		factory.deps.Profile = profile
	}
	if factory.deps.Registries == nil {
		factory.deps.Registries = agent.NewBatchLeafRegistryStore()
	}
	rt, install, err := factory.build(ctx)
	if err != nil {
		if app.batchTimeoutWake != nil {
			app.batchTimeoutWake.Stop()
			app.batchTimeoutWake = nil
		}
		return err
	}
	// R11F (M3): the exported one-authority ingress seam over the installed
	// runtime; InitCoderAgent binds it to the coordinator.
	app.batchWorkIngress, err = agent.NewBatchWorkIngressWithDiskBindings(install.Runtime, factory.deps.Registries, install.DiskBindings)
	if err != nil {
		if app.batchTimeoutWake != nil {
			app.batchTimeoutWake.Stop()
			app.batchTimeoutWake = nil
		}
		return err
	}
	// R12-H2 P1-2/P1-4: bind the ingress into the runtime so batchLeafEnv
	// carries it — without this, provider-bound delegation fails closed.
	agent.BindRuntimeIngress(install.Runtime, app.batchWorkIngress)
	app.batchModelControl = agent.NewBatchModelControl(install.RunnerHandle, install.Delivery, factory.deps.Store, factory.live, app.Sessions, factory.deps.Host, factory.deps.RunnerID, factory.deps.Profile, factory.deps.Resolver, install.RootLeases)
	app.batchLiveReader = factory.live
	// Wire the R11E release seams onto the gate BEFORE the watcher starts:
	// RetainHostIdentity mirrors the forced path's CloseKeepLock,
	// ReleaseHostIdentity the graceful Close, and ReleaseWriter the
	// generation-safe per-reference DB release of releaseResources.
	// Host release goes through the HostStore -- CloseKeepLock pins the OS
	// lock, Close deletes via DeleteAsyncHostIfNoNodes, never async_jobs.
	rt.cfg.BeforeExit = app.handleBatchFaultOutput
	rt.cfg.RetainHostIdentity = app.batchHostReleaseKeepLock
	rt.cfg.ReleaseHostIdentity = func(ctx context.Context) error {
		// R12-H2 §1.3: residual construction scopes close AFTER the
		// disposal drain and BEFORE host/writer release (the gate's
		// release order is host, then writer). Forced shutdown and the
		// fault fence never reach this seam, so the manager's remaining
		// owners are retained until process exit.
		app.drainResidualConstructions()
		return app.batchHostRelease(ctx)
	}
	rt.cfg.ReleaseWriter = func(ctx context.Context) error {
		return app.releaseBatchWriter()
	}
	if batchRuntimeConfigHook != nil {
		batchRuntimeConfigHook(&rt.cfg)
	}
	rt.cfg.TimeoutCheckin = batchTimeoutWakeRuntime{checkin: rt.cfg.TimeoutCheckin, dispatch: app.batchTimeoutWake}
	app.batchRuntime = rt
	app.batchInstall = install
	// Start arms the fault watcher (idempotent; SetRunner already ran inside
	// build — Start and SetRunner are order-independent).
	rt.Start()
	// Held-question inject bridge (D1): with a live Runner and writer,
	// install the sink inject_agent uses to release a child's
	// CURRENT-incarnation held question through Runner.Inject. A
	// runnerless install (Start logged the RunnerFactory failure) leaves
	// the sink nil and inject_agent keeps its legacy behaviour
	// byte-identically.
	if runner := install.Runner; runner != nil {
		sink, serr := agent.NewBatchHeldAnswerSink(app.batchCutoverWriter(), runner)
		if serr != nil {
			slog.Warn("app: batch held-answer sink unavailable; inject_agent keeps the legacy path", "err", serr)
		} else {
			app.batchHeldAnswerSink = sink
		}
	}
	// H4 W2: arm the App-owned dead-host maintenance (startup pass + 60s
	// periodic pass) alongside the fault watcher. Covers CLI, server and
	// SDK hosts; stopped and joined by releaseResources.
	app.startBatchMaintenance()
	slog.Info("app: batch runtime installed", "host", host)
	return nil
}

// drainResidualConstructions closes residual construction scopes on the
// graceful path only: construction records whose only remaining owners are
// their Construction pins are dropped (R12-H2 §1.3) — the forced path never
// calls this, retaining the manager and its owners until process exit.
func (app *App) drainResidualConstructions() {
	if app.batchInstall != nil {
		app.batchInstall.DrainDiskBindings()
	}
}

// releaseBatchWriter releases this App's pooled DB references exactly like
// the graceful branch of releaseResources' DB block (per Connect/ConnectRead
// call, or the legacy dbReleasesNeeded count for hand-assembled Apps).
func (app *App) releaseBatchWriter() error {
	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if len(app.dbConns) > 0 {
		for _, conn := range app.dbConns {
			record(db.ReleaseConn(conn))
		}
		return firstErr
	}
	for i := 0; i < app.dbReleasesNeeded; i++ {
		record(db.Release(app.dataDir))
	}
	return firstErr
}

// releaseBatchRuntimeGate is the replacement for the "release host
// identity and writer on agent-idle" block of releaseResources: the Runner
// drains first, then host identity, then writer — all inside
// DrainAndRelease. It returns the (possibly promoted) stillBusy verdict.
//
//   - Graceful (stillBusy=false): drain bounded by appBatchDrainTimeout;
//     a Close timeout or fault fence retains the draining resources and
//     promotes the shutdown to Forced — the DB block below then skips the
//     release exactly like the forced path.
//   - Forced (stillBusy=true): the watcher is disarmed, the host lock is
//     kept (CloseKeepLock) and the DB is never touched.
func (app *App) releaseBatchRuntimeGate(ctx context.Context, stillBusy bool, record func(error)) bool {
	// Disarm the held-question inject bridge first: once the drain begins no
	// answer may be routed into a draining Runner (the sink lookup is gone,
	// so a racing inject_agent takes the legacy path instead).
	// Close first: a sink already loaded by an in-flight inject_agent must
	// fail deterministically (jobbatch.ErrReleased-shaped) instead of
	// delivering into the draining Runner or silently degrading to the
	// mailbox with a half-delivered answer.
	if app.batchHeldAnswerSink != nil {
		app.batchHeldAnswerSink.Close()
	}
	agent.SetCoordinatorBatchHeldAnswerSink(app.AgentCoordinator, nil)
	app.batchHeldAnswerSink = nil
	rt := app.batchRuntime
	if rt == nil {
		if app.batchTimeoutWake != nil {
			app.batchTimeoutWake.Stop()
			app.batchTimeoutWake = nil
		}
		// Defensive: an App without an installed runtime must never take
		// this path; retain the host identity if it somehow does.
		app.batchHostReleaseKeepLock()
		return stillBusy
	}
	if stillBusy {
		rt.Stop()
		app.batchHostReleaseKeepLock()
		return true
	}
	rt.Stop()
	res := rt.DrainAndRelease(ctx, appBatchDrainTimeout)
	if res.Err != nil {
		record(res.Err)
	}
	if res.Forced {
		slog.Warn("app: batch drain forced - host identity and writer retained; caller must exit the process")
		return true
	}
	return false
}
