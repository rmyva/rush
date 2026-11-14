// Coordinator wiring and model selection: InitCoderAgent builds the
// agent.Coordinator, and the model-override helpers resolve per-run
// smart/fast model choices for `rush run`.

package app

import (
	"context"
	"fmt"
	"log/slog"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/shell"
)

func (app *App) UpdateAgentModel(ctx context.Context) error {
	if app.AgentCoordinator == nil {
		return fmt.Errorf("agent configuration is missing")
	}
	return app.AgentCoordinator.UpdateModels(ctx)
}

// resolveModelOverridesForNonInteractive parses and validates model strings
// without publishing or changing any process-wide model state.
// Format: "model-name" (searches all providers) or "provider/model-name".
func (app *App) resolveModelOverridesForNonInteractive(smartModel, fastModel string) (*agent.ModelOverride, *agent.ModelOverride, error) {
	providers := app.config.Config().Providers.Copy()

	smartMatches, fastMatches, err := findModels(providers, smartModel, fastModel)
	if err != nil {
		return nil, nil, err
	}

	var smartOverride, fastOverride *agent.ModelOverride

	if smartModel != "" {
		found, err := validateMatches(smartMatches, smartModel, "smart")
		if err != nil {
			return nil, nil, err
		}
		slog.Info("Overriding smart model for non-interactive run", "provider", found.provider, "model", found.modelID)
		smartOverride = &agent.ModelOverride{
			Provider: found.provider,
			Model:    found.modelID,
		}
	}

	if fastModel != "" {
		found, err := validateMatches(fastMatches, fastModel, "fast")
		if err != nil {
			return nil, nil, err
		}
		slog.Info("Overriding fast model for non-interactive run", "provider", found.provider, "model", found.modelID)
		fastOverride = &agent.ModelOverride{
			Provider: found.provider,
			Model:    found.modelID,
		}
	}

	return smartOverride, fastOverride, nil
}

// GetDefaultFastModel returns the default fast model for the given
// provider. Falls back to the smart model if no default is found.
func (app *App) GetDefaultFastModel(providerID string) config.SelectedModel {
	cfg := app.config.Config()
	smartModelCfg := cfg.Models[config.SelectedModelTypeSmart]

	// Find the provider in the known providers list to get its default fast model.
	knownProviders, _ := config.Providers(cfg)
	var knownProvider *catwalk.Provider
	for _, p := range knownProviders {
		if string(p.ID) == providerID {
			knownProvider = &p
			break
		}
	}

	// For unknown/local providers, use the smart model as small.
	if knownProvider == nil {
		slog.Warn("Using smart model as fast model for unknown provider", "provider", providerID, "model", smartModelCfg.Model)
		return smartModelCfg
	}

	defaultFastModelID := knownProvider.DefaultSmallModelID
	model := cfg.GetModel(providerID, defaultFastModelID)
	if model == nil {
		slog.Warn("Default fast model not found, using smart model", "provider", providerID, "model", smartModelCfg.Model)
		return smartModelCfg
	}

	slog.Info("Using provider default fast model", "provider", providerID, "model", defaultFastModelID)
	return config.SelectedModel{
		Provider:        providerID,
		Model:           defaultFastModelID,
		MaxTokens:       model.DefaultMaxTokens,
		ReasoningEffort: model.DefaultReasoningEffort,
	}
}

// Fork merge note (origin/main 6716ef09 "feat(skills): user invocable skills"):
// upstream added setupEvents/setupSubscriber to forward service events into a
// bubbletea pubsub broker. We rejected this — our WebSocket hub
// (internal/server/hub.go) handles event fan-out to browser clients directly
// without going through tea.Msg. See CHANGELOG.fork.md Section 2.
func (app *App) InitCoderAgent(ctx context.Context) error {
	if app.BackgroundShellManager == nil {
		app.BackgroundShellManager = shell.NewBackgroundShellManager()
	}
	coderAgentCfg := app.config.Config().Agents[config.AgentCoder]
	if coderAgentCfg.ID == "" {
		// Self-heal: config.Load/reload always call SetupAgents once
		// IsConfigured() becomes true, but a caller that mutates
		// Providers/SelectedModel directly on an already-published config
		// (bypassing Load/reload entirely — a test-only pattern; found via
		// a CI-only failure this exact class of gap caused, "coder agent
		// configuration is missing", that never reproduced on a dev machine
		// with some stray provider config making IsConfigured() true at
		// initial Init) never triggers that population. SetupAgents is
		// idempotent (derives Agents purely from Options/DisabledTools, no
		// I/O), so re-deriving it here on a genuine miss is safe and closes
		// the whole class of gap at the one place every caller (test or
		// production) funnels through, rather than requiring every caller
		// to remember an explicit SetupAgents call of their own.
		app.config.SetupAgents()
		coderAgentCfg = app.config.Config().Agents[config.AgentCoder]
	}
	if coderAgentCfg.ID == "" {
		return fmt.Errorf("coder agent configuration is missing")
	}
	var err error
	app.AgentCoordinator, err = agent.NewCoordinator(
		ctx,
		app.config,
		app.Sessions,
		app.Messages,
		app.Permissions,
		app.History,
		app.FileTracker,
		app.agentNotifications,
		app.mcpOwner,
		app.BackgroundShellManager,
	)
	if err != nil {
		slog.Error("Failed to create coder agent", "err", err)
		return err
	}
	if noticeStore := app.batchNoticeStore(); noticeStore != nil {
		if err := agent.SetCoordinatorSessionNoticeStore(app.AgentCoordinator, noticeStore); err != nil {
			slog.Error("Failed to bind session notice store", "err", err)
			return err
		}
	}
	// R12-H2b §2.3: wire the durable session-execution-fence store so
	// continuations resolve the fence through the same session service; a
	// service that does not implement the store simply means no fence.
	if reqStore, ok := app.Sessions.(session.SessionExecutionRequirementStore); ok {
		if err := agent.SetCoordinatorExecutionRequirementStore(app.AgentCoordinator, reqStore); err != nil {
			slog.Error("Failed to bind session execution requirement store", "err", err)
			return err
		}
	}
	if reqStore, ok := app.Sessions.(session.SessionCredentialRequirementStore); ok {
		if err := agent.SetCoordinatorSessionCredentialRequirementStore(app.AgentCoordinator, reqStore); err != nil {
			slog.Error("Failed to bind session credential requirement store", "err", err)
			return err
		}
	}
	// F7b: an ephemeral App (dataDir == "") never installs the batch
	// runtime, so BindCoordinatorBatchControls below never runs and
	// the coordinator is left with NO capability manager. Its run path
	// (coordinator.PrepareContinuation) then fails closed with
	// agent.ErrDiskProviderUnavailable ("no capability manager is bound")
	// for every custom-DiskProvider run whose admission persisted a
	// session execution requirement — even though app_run_setup.go bound
	// the Call owner against the App's own in-memory registry. Bind the
	// SAME registry so continuations resolve through it. Persisted Apps
	// are untouched: app.New constructs ephemeralDiskBindings only on the
	// ephemeral branch.
	if app.ephemeralDiskBindings != nil {
		if err := agent.SetCoordinatorDiskCapabilityManager(app.AgentCoordinator, app.ephemeralDiskBindings); err != nil {
			slog.Error("Failed to bind ephemeral disk capability manager", "err", err)
			return err
		}
	}
	// R11F (M3): bind the coordinator to the batch runtime. After
	// this every tool build re-arms its work tools through the ONE ingress
	// authority and the old async/stall wrappers are not installed.
	if app.batchWorkIngress != nil {
		if err := agent.BindCoordinatorBatchControls(app.AgentCoordinator, app.batchWorkIngress, app.batchModelControl, app.batchLiveReader); err != nil {
			slog.Error("Failed to bind batch runtime", "err", err)
			return err
		}
	}
	// Held-question inject bridge: bind the App-owned sink to THIS
	// coordinator (never a process global) so non-interrupt inject_agent
	// releases a child's committed held question through Runner.Inject.
	if app.batchHeldAnswerSink != nil {
		agent.SetCoordinatorBatchHeldAnswerSink(app.AgentCoordinator, app.batchHeldAnswerSink)
	}
	// The CLI loop's durable driver markers route through the
	// job-independent HostStore (released by the shutdown drain gate), so
	// no legacy store ever ensures a host or creates a hosts/<id>.lock.
	if app.batchWorkIngress != nil && app.batchHostStore() != nil {
		if err := agent.SetBatchDriverClaimer(app.AgentCoordinator, app.batchHostStore()); err != nil {
			slog.Error("Failed to set batch driver claimer", "err", err)
			return err
		}
	}
	// Bind the node-backed cutover facts so every App that runs turns reads
	// batch_nodes instead of the conservative unknown fail-safe (its
	// permanent "node source unavailable" error made the CLI
	// loop retry a wiring defect for the full retry bound).
	if writer := app.batchCutoverWriter(); writer != nil {
		live := app.batchLiveReader
		if live == nil {
			live = session.NewBatchLiveReader(db.New(writer), app.dataDir)
			// TxStarter: every live read runs in ONE read-only tx on the writer.
			live.TxStarter = writer
		}
		agent.BindCoordinatorBatchCutoverFacts(app.AgentCoordinator, agent.NewBatchCutoverNodeFacts(session.NewBatchStoreWithReader(writer, app.batchCutoverReader(writer)), live, writer))
	}
	// Stage 4b: start the durable wake-schedule worker over this App's
	// store. Optional-interface assertion (agent.WakeScheduleController):
	// the Coordinator interface is not grown for it, so existing test
	// fakes keep compiling.
	if app.wakeScheduleStore != nil {
		if ctrl, ok := app.AgentCoordinator.(agent.WakeScheduleController); ok {
			ctrl.SetWakeScheduleStore(app.wakeScheduleStore)
		}
	}
	if app.batchTimeoutWake != nil {
		app.batchTimeoutWake.Attach(app.AgentCoordinator)
	}
	// Rate-limit wait diagnostics are run-scoped now: prepareExecuteRun
	// attaches a CLI-origin-gated sink (agent.WithRateLimitNotifySink) to
	// each run's context and the coordinator's notifier call site consumes
	// it, so an embedded/API run's diagnostics go to that run's stderr
	// instead of the process stderr, and non-CLI runs get none. The former
	// app-wide SetRateLimitNotify(os.Stderr/cliLoopStderr) callback is gone.
	// W1a: bind the finalization driver wiring when the flag is on; a nil
	// wiring below keeps every production path byte-identical to legacy.
	if err := app.installAgentFinalizationWiring(); err != nil {
		slog.Error("Failed to install agent finalization wiring", "err", err)
		return err
	}
	return nil
}
