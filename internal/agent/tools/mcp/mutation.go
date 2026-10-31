package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
)

// DisableSingle disables and closes a single MCP client by name.
func DisableSingle(cfg *config.ConfigStore, name string) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	return disableSingleForOwner(o, cfg, name)
}

// DisableSingle closes the session for a single MCP client by name.
// A nil receiver resolves the process-current owner exactly like the
// package-level DisableSingle function, so callers holding an optional owner
// can call the method unconditionally.
func (o *Owner) DisableSingle(cfg *config.ConfigStore, name string) error {
	if o == nil {
		return DisableSingle(cfg, name)
	}
	return disableSingleForOwner(o, cfg, name)
}

func disableSingleForOwner(o *Owner, cfg *config.ConfigStore, name string) error {
	if err := o.rememberConfig(cfg); err != nil {
		return err
	}
	if err := o.reconcileUncertainty(context.Background(), cfg, name); err != nil {
		return err
	}
	if !o.beginInit() {
		return ErrOwnerBusy
	}
	defer o.endInit()
	lease := serverLeaseFor(name)
	lease.Lock()
	o.invalidateServer(name)
	oldSession := detachSessionLocked(name)
	clearAdvertised(name)
	updateState(name, StateDisabled, nil, nil, Counts{})
	lease.Unlock()
	retireMCPClient(name, oldSession)

	slog.Info("Disabled mcp client", "name", name)
	return nil
}

// DisableServer disables an MCP server: closes its session, removes its tools,
// and persists the disabled flag to config.
func DisableServer(ctx context.Context, cfg *config.ConfigStore, name string) error {
	return disableServerWithResultPersistence(ctx, cfg, name,
		func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
			if pending != nil {
				return cfg.PersistMCPConfigResult(scope, name, *pending)
			}
			return cfg.PersistMCPDisabledOverrideResult(scope, name, true)
		})
}

// DisableServer disables an MCP server: closes its session, removes its tools,
// and persists the disabled flag to config.
// A nil receiver resolves the process-current owner exactly like the
// package-level DisableServer function, so callers holding an optional owner
// can call the method unconditionally.
func (o *Owner) DisableServer(ctx context.Context, cfg *config.ConfigStore, name string) error {
	if o == nil {
		return DisableServer(ctx, cfg, name)
	}
	return disableServerWithResultPersistenceForOwner(o, ctx, cfg, name,
		func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
			if pending != nil {
				return cfg.PersistMCPConfigResult(scope, name, *pending)
			}
			return cfg.PersistMCPDisabledOverrideResult(scope, name, true)
		})
}

// disableServerPersister performs exactly one durable mutation. pending is the
// complete disabled definition for an in-flight Add, or nil when an existing
// durable definition only needs a disabled field overlay.
type disableServerPersister func(*config.ConfigStore, config.Scope, string, *config.MCPConfig) error

type disableServerResultPersister func(*config.ConfigStore, config.Scope, string, *config.MCPConfig) (config.MCPMutationResult, error)

func disableServerWithPersistence(
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	persist disableServerPersister,
) error {
	return disableServerWithResultPersistence(ctx, cfg, name, func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
		if err := persist(cfg, scope, name, pending); err != nil {
			if outcome, ok := config.CommitOutcomeFromError(err); ok && commitOutcomeIsReconciled(outcome) {
				return currentMCPMutationResult(cfg, "disable", name, name), err
			}
			return config.MCPMutationResult{}, err
		}
		return currentMCPMutationResult(cfg, "disable", name, name), nil
	})
}

func disableServerWithResultPersistence(
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	persist disableServerResultPersister,
) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	return disableServerWithResultPersistenceForOwner(o, ctx, cfg, name, persist)
}

func disableServerWithResultPersistenceForOwner(
	o *Owner,
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	persist disableServerResultPersister,
) error {
	if err := o.rememberConfig(cfg); err != nil {
		return err
	}
	if err := o.reconcileUncertainty(ctx, cfg, name); err != nil {
		return err
	}
	if !o.beginInit() {
		return ErrOwnerBusy
	}
	defer o.endInit()
	if _, ok := cfg.MCPConfig(name); !ok {
		return fmt.Errorf("MCP server %q not found: %w", name, config.ErrMCPNotFound)
	}
	lease := serverLeaseFor(name)
	if !lease.lockContext(ctx, true) {
		return ctx.Err()
	}
	var detached *ClientSession
	unlock := func() {
		lease.Unlock()
		retireMCPClient(name, detached)
	}
	mcpCfg, ok := cfg.MCPConfig(name)
	if !ok {
		unlock()
		return fmt.Errorf("MCP server %q disappeared while disabling: %w", name, config.ErrMCPNotFound)
	}
	if o.isUncertain(cfg, name) {
		unlock()
		return ErrMCPConfigUncertain
	}
	if !o.acceptsSession() {
		unlock()
		return ErrOwnerBusy
	}
	// Resolve the origin while holding the ordered server lease. A user
	// workspace definition must stay in the workspace file, while project,
	// system, external, and otherwise unrepresentable definitions fail closed.
	scope, err := resolveMCPMutationScope(cfg, name, mcpCfg)
	if err != nil {
		unlock()
		return fmt.Errorf("cannot determine writable scope for MCP server %q: %w", name, err)
	}
	// Persist first. If disk persistence fails, the session and in-memory
	// snapshot remain enabled, so the operation has no half-applied result.
	transaction := o.pendingGlobalAdd(name, cfg)
	var pending *config.MCPConfig
	if transaction != nil {
		fullConfig := mcpCfg
		fullConfig.Disabled = true
		pending = &fullConfig
	}
	if !o.acceptsSession() {
		unlock()
		return ErrOwnerBusy
	}
	result, err := persist(cfg, scope, name, pending)
	if err != nil {
		outcome, hasOutcome := config.CommitOutcomeFromError(err)
		if hasOutcome && commitOutcomeIsReconciled(outcome) {
			if !result.NewExists {
				unlock()
				return fmt.Errorf("failed to persist MCP disabled state for %q: %w", name, err)
			}
			if transaction != nil {
				transaction.markUserMutation()
			}
			o.invalidateServer(name)
			detached = detachSessionLocked(name)
			clearAdvertised(name)
			updateState(name, StateDisabled, nil, nil, Counts{})
			unlock()
			return fmt.Errorf("failed to persist MCP disabled state for %q: %w", name, err)
		}
		if hasOutcome && commitOutcomeNeedsRuntimeFence(outcome) {
			if transaction != nil {
				transaction.markUserMutation()
			}
			detached = fenceMCPRuntimeLocked(o, cfg, name)
			unlock()
			return fmt.Errorf("failed to persist MCP disabled state for %q: %w", name, err)
		}
		unlock()
		return fmt.Errorf("failed to persist MCP disabled state for %q: %w", name, err)
	}
	if !result.NewExists {
		unlock()
		return fmt.Errorf("MCP server %q disappeared while disabling: %w", name, config.ErrMCPNotFound)
	}
	if transaction != nil {
		transaction.markUserMutation()
	}
	// Persistence is the fallible part of this transaction. Only after it
	// succeeds may this operation invalidate candidates; the write lease keeps
	// a candidate from publishing between these steps and the runtime update.
	o.invalidateServer(name)
	detached = detachSessionLocked(name)
	clearAdvertised(name)
	updateState(name, StateDisabled, nil, nil, Counts{})
	unlock()
	return nil
}

// EnableServer re-enables a disabled MCP server and starts a new session.
func EnableServer(ctx context.Context, cfg *config.ConfigStore, name string) error {
	return enableServerWithPersistence(ctx, cfg, name, func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
		return enableMCPConfig(cfg, scope, name, pending)
	})
}

// EnableServer re-enables a disabled MCP server and starts a new session.
// A nil receiver resolves the process-current owner exactly like the
// package-level EnableServer function, so callers holding an optional owner
// can call the method unconditionally.
func (o *Owner) EnableServer(ctx context.Context, cfg *config.ConfigStore, name string) error {
	if o == nil {
		return EnableServer(ctx, cfg, name)
	}
	return enableServerWithPersistenceForOwner(o, ctx, cfg, name, func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
		return enableMCPConfig(cfg, scope, name, pending)
	})
}

type enableServerResultPersister func(*config.ConfigStore, config.Scope, string, *config.MCPConfig) (config.MCPMutationResult, error)

func enableServerWithPersistence(ctx context.Context, cfg *config.ConfigStore, name string, persist enableServerResultPersister) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	return enableServerWithPersistenceForOwner(o, ctx, cfg, name, persist)
}

func enableServerWithPersistenceForOwner(
	o *Owner,
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	persist enableServerResultPersister,
) error {
	return enableServerWithPersistenceAndInitializerForOwner(o, ctx, cfg, name, persist, initClientAdmitted)
}

func enableServerWithPersistenceAndInitializer(
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	persist enableServerResultPersister,
	initialize admittedClientInitializer,
) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	return enableServerWithPersistenceAndInitializerForOwner(o, ctx, cfg, name, persist, initialize)
}

func enableServerWithPersistenceAndInitializerForOwner(
	o *Owner,
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	persist enableServerResultPersister,
	initialize admittedClientInitializer,
) error {
	return enableServerWithPersistenceAndInitializerAndRollbackForOwner(o, ctx, cfg, name, persist, initialize, nil)
}

func enableServerWithPersistenceAndInitializerAndRollback(
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	persist enableServerResultPersister,
	initialize admittedClientInitializer,
	rollbackPersist enableServerResultPersister,
) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	return enableServerWithPersistenceAndInitializerAndRollbackForOwner(o, ctx, cfg, name, persist, initialize, rollbackPersist)
}

func enableServerWithPersistenceAndInitializerAndRollbackForOwner(
	o *Owner,
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	persist enableServerResultPersister,
	initialize admittedClientInitializer,
	rollbackPersist enableServerResultPersister,
) error {
	if err := o.rememberConfig(cfg); err != nil {
		return err
	}
	if err := o.reconcileUncertainty(ctx, cfg, name); err != nil {
		return err
	}
	if !o.beginInit() {
		return ErrOwnerBusy
	}
	defer o.endInit()
	if _, ok := cfg.MCPConfig(name); !ok {
		return fmt.Errorf("MCP server %q not found: %w", name, config.ErrMCPNotFound)
	}
	lease := serverLeaseFor(name)
	if !lease.lockContext(ctx, true) {
		return ctx.Err()
	}
	mcpCfg, ok := cfg.MCPConfig(name)
	if !ok {
		lease.Unlock()
		return fmt.Errorf("MCP server %q disappeared while enabling", name)
	}
	if o.isUncertain(cfg, name) {
		lease.Unlock()
		return ErrMCPConfigUncertain
	}
	scope, err := resolveMCPMutationScope(cfg, name, mcpCfg)
	if err != nil {
		lease.Unlock()
		return fmt.Errorf("cannot determine writable scope for MCP server %q: %w", name, err)
	}
	transaction := o.pendingGlobalAdd(name, cfg)
	var pending *config.MCPConfig
	if transaction != nil {
		fullConfig := mcpCfg
		fullConfig.Disabled = false
		pending = &fullConfig
	}
	result, err := persist(cfg, scope, name, pending)
	var commitUncertainty error
	if err != nil {
		outcome, ok := config.CommitOutcomeFromError(err)
		if !ok || (!outcome.Committed && !outcome.MaybeCommitted) {
			lease.Unlock()
			return fmt.Errorf("failed to persist MCP enabled state for %q: %w", name, err)
		}
		if transaction != nil {
			transaction.markUserMutation()
		}
		if commitOutcomeNeedsRuntimeFence(outcome) {
			detached := fenceMCPRuntimeLocked(o, cfg, name)
			lease.Unlock()
			retireMCPClient(name, detached)
			return fmt.Errorf("failed to persist MCP enabled state for %q: %w", name, err)
		}
		commitUncertainty = err
	}
	if transaction != nil {
		transaction.markUserMutation()
	}
	type rollbackResult struct {
		err        error
		needsFence bool
	}
	rollbackPersistence := func() rollbackResult {
		var rollbackErr error
		if rollbackPersist != nil {
			var rollback *config.MCPConfig
			if transaction != nil {
				rollbackConfig := mcpCfg
				rollbackConfig.Disabled = true
				rollback = &rollbackConfig
			}
			_, rollbackErr = rollbackPersist(cfg, scope, name, rollback)
		} else {
			_, rollbackErr = cfg.PersistMCPEnableRollbackResult(scope, name, result)
		}
		if rollbackErr == nil {
			return rollbackResult{}
		}
		outcome, hasOutcome := config.CommitOutcomeFromError(rollbackErr)
		knownMismatch := errors.Is(rollbackErr, config.ErrMCPMutationStale)
		return rollbackResult{err: rollbackErr, needsFence: knownMismatch || hasOutcome && commitOutcomeNeedsRuntimeFence(outcome)}
	}
	rollbackAndReport := func(cause error) (*ClientSession, error) {
		rollback := rollbackPersistence()
		if rollback.err == nil {
			return nil, cause
		}
		if rollback.needsFence {
			detached := fenceMCPRuntimeLocked(o, cfg, name)
			return detached, errors.Join(cause, ErrMCPConfigUncertain,
				fmt.Errorf("failed to roll back MCP enabled state for %q: %w", name, rollback.err))
		}
		return nil, errors.Join(cause,
			fmt.Errorf("failed to roll back MCP enabled state for %q: %w", name, rollback.err))
	}
	if !o.acceptsSession() {
		if commitUncertainty == nil {
			detached, rollbackErr := rollbackAndReport(ErrOwnerBusy)
			lease.Unlock()
			retireMCPClient(name, detached)
			return rollbackErr
		} else {
			detached := fenceMCPRuntimeLocked(o, cfg, name)
			lease.Unlock()
			retireMCPClient(name, detached)
			if commitUncertainty != nil {
				return fmt.Errorf("failed to persist MCP enabled state for %q: %w", name, commitUncertainty)
			}
			return ErrOwnerBusy
		}
	}
	// The new admission below is the only initializer allowed to publish this
	// epoch. A failed persistence round-trip therefore leaves the old epoch
	// untouched and the operation has no half-applied lifecycle result.
	admissionSnapshot := cfg.SnapshotMCPAdmission(name)
	resolver := admissionSnapshot.Resolver
	if result.Generation != 0 && admissionSnapshot.Generation != result.Generation {
		detached, rollbackErr := rollbackAndReport(config.ErrMCPMutationStale)
		lease.Unlock()
		retireMCPClient(name, detached)
		return rollbackErr
	}
	o.invalidateServer(name)
	if !result.NewExists {
		if commitUncertainty != nil {
			detached := fenceMCPRuntimeLocked(o, cfg, name)
			lease.Unlock()
			retireMCPClient(name, detached)
			return fmt.Errorf("failed to persist MCP enabled state for %q: %w", name, commitUncertainty)
		}
		detached, rollbackErr := rollbackAndReport(fmt.Errorf("MCP server %q disappeared while enabling: %w", name, config.ErrMCPNotFound))
		lease.Unlock()
		retireMCPClient(name, detached)
		return rollbackErr
	}
	admission, err := o.admitServerForConfig(ctx, cfg, name, result.NewConfig, true)
	if err != nil {
		if commitUncertainty != nil {
			detached := fenceMCPRuntimeLocked(o, cfg, name)
			lease.Unlock()
			retireMCPClient(name, detached)
			return fmt.Errorf("failed to persist MCP enabled state for %q: %w", name, commitUncertainty)
		}
		detached, rollbackErr := rollbackAndReport(err)
		lease.Unlock()
		retireMCPClient(name, detached)
		return rollbackErr
	}
	admissionOwned := true
	defer func() {
		if admissionOwned {
			admission.done()
		}
	}()
	admission.candidate = true
	admission.mcpRevision = admissionSnapshot.MCPRevision
	admission.resolverRevision = admissionSnapshot.ResolverRevision
	admission.mcpAdmission = admissionSnapshot
	admission.mutationResult = &result
	if !admission.valid() {
		detached, rollbackErr := rollbackAndReport(config.ErrMCPMutationStale)
		lease.Unlock()
		retireMCPClient(name, detached)
		return rollbackErr
	}
	updateAdmissionState(&admission, StateStarting, nil, nil, Counts{})
	lease.Unlock()
	go func() {
		if err := initialize(ctx, cfg, name, result.NewConfig, resolver, &admission); err != nil {
			cleanupFailedAdmission(&admission, err)
			slog.Error("Failed to enable MCP server", "name", name, "err", err)
		}
	}()
	admissionOwned = false
	if commitUncertainty != nil {
		return fmt.Errorf("failed to persist MCP enabled state for %q: %w", name, commitUncertainty)
	}
	return nil
}

func enableMCPConfig(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
	if pending != nil {
		return cfg.PersistMCPConfigResult(scope, name, *pending)
	}
	return cfg.PersistMCPDisabledOverrideResult(scope, name, false)
}

// resolveMCPMutationScope resolves the writable origin for a disable/enable
// operation. External definitions are intentionally represented by a
// workspace-only disabled overlay; all other definitions must be owned by an
// exact writable config scope.
func resolveMCPMutationScope(cfg *config.ConfigStore, name string, mcpCfg config.MCPConfig) (config.Scope, error) {
	if mcpCfg.Source == config.MCPSourceExternal {
		if !cfg.HasWorkspaceConfig() {
			return config.ScopeWorkspace, config.ErrNoWorkspaceConfig
		}
		return config.ScopeWorkspace, nil
	}
	scope, err := cfg.ResolveMCPWritableScope(name)
	if err == nil {
		return scope, nil
	}
	// AddServer has a deliberate global-scope contract, but its definition is
	// not on disk until initialization finishes. Preserve the cancellation
	// path for a concurrent disable/remove of that in-flight add; an ordinary
	// unowned in-memory definition remains fail-closed below.
	if (errors.Is(err, config.ErrMCPUnwritableOrigin) || errors.Is(err, config.ErrMCPNotFound)) && hasPendingGlobalAddFor(cfg, name) {
		return config.ScopeGlobal, nil
	}
	return config.ScopeGlobal, err
}

func hasPendingGlobalAdd(name string) bool {
	return hasPendingGlobalAddFor(nil, name)
}

func hasPendingGlobalAddFor(cfg *config.ConfigStore, name string) bool {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	currentOwner := owner
	if currentOwner == nil {
		return false
	}
	transaction := currentOwner.pendingGlobalAdds[name]
	return transaction != nil && transaction.token != 0 && (cfg == nil || transaction.cfg == cfg)
}

// RemoveServer removes an MCP server, closes its session, and removes it from config.
// External servers (from .mcp.json) cannot be removed — only disabled.
func RemoveServer(cfg *config.ConfigStore, name string) error {
	return removeServerWithResultPersistence(cfg, name,
		func(cfg *config.ConfigStore, scope config.Scope, name string) (config.MCPMutationResult, error) {
			if pending := pendingGlobalAddFor(cfg, name); pending != nil {
				return cfg.PersistRemovePendingMCPConfigResult(scope, name)
			}
			return cfg.PersistRemoveMCPConfigResult(scope, name)
		})
}

// RemoveServer removes an MCP server, closes its session, and removes it from
// config. External servers (from .mcp.json) cannot be removed — only disabled.
// A nil receiver resolves the process-current owner exactly like the
// package-level RemoveServer function, so callers holding an optional owner
// can call the method unconditionally.
func (o *Owner) RemoveServer(cfg *config.ConfigStore, name string) error {
	if o == nil {
		return RemoveServer(cfg, name)
	}
	return removeServerWithResultPersistenceForOwner(o, cfg, name,
		func(cfg *config.ConfigStore, scope config.Scope, name string) (config.MCPMutationResult, error) {
			if pending := pendingGlobalAddFor(cfg, name); pending != nil {
				return cfg.PersistRemovePendingMCPConfigResult(scope, name)
			}
			return cfg.PersistRemoveMCPConfigResult(scope, name)
		})
}

type (
	removeServerPersister       func(*config.ConfigStore, string) error
	scopedRemoveServerPersister func(*config.ConfigStore, config.Scope, string) error
	removeServerResultPersister func(*config.ConfigStore, config.Scope, string) (config.MCPMutationResult, error)
)

func removeServerWithPersistence(
	cfg *config.ConfigStore,
	name string,
	persist removeServerPersister,
) error {
	return removeServerWithScopedPersistence(cfg, name,
		func(cfg *config.ConfigStore, _ config.Scope, name string) error {
			return persist(cfg, name)
		})
}

func removeServerWithScopedPersistence(
	cfg *config.ConfigStore,
	name string,
	persist scopedRemoveServerPersister,
) error {
	return removeServerWithResultPersistence(cfg, name, func(cfg *config.ConfigStore, scope config.Scope, name string) (config.MCPMutationResult, error) {
		if err := persist(cfg, scope, name); err != nil {
			if outcome, ok := config.CommitOutcomeFromError(err); ok && commitOutcomeIsReconciled(outcome) {
				return currentMCPMutationResult(cfg, "remove", name, name), err
			}
			return config.MCPMutationResult{}, err
		}
		return currentMCPMutationResult(cfg, "remove", name, name), nil
	})
}

func removeServerWithResultPersistence(
	cfg *config.ConfigStore,
	name string,
	persist removeServerResultPersister,
) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	return removeServerWithResultPersistenceForOwner(o, cfg, name, persist)
}

func removeServerWithResultPersistenceForOwner(
	o *Owner,
	cfg *config.ConfigStore,
	name string,
	persist removeServerResultPersister,
) error {
	if err := o.rememberConfig(cfg); err != nil {
		return err
	}
	if err := o.reconcileUncertainty(context.Background(), cfg, name); err != nil {
		return err
	}
	if !o.beginInit() {
		return ErrOwnerBusy
	}
	defer o.endInit()
	mcpCfg, exists := cfg.MCPConfig(name)
	if !exists {
		return fmt.Errorf("MCP server %q not found: %w", name, config.ErrMCPNotFound)
	}
	if mcpCfg.Source == config.MCPSourceExternal {
		return fmt.Errorf("MCP server %q is from .mcp.json and cannot be removed (disable it instead): %w", name, config.ErrMCPExternal)
	}

	lease := serverLeaseFor(name)
	lease.Lock()
	var detached *ClientSession
	unlock := func() {
		lease.Unlock()
		retireMCPClient(name, detached)
	}
	mcpCfg, exists = cfg.MCPConfig(name)
	if o.isUncertain(cfg, name) {
		unlock()
		return ErrMCPConfigUncertain
	}
	if !o.acceptsSession() {
		unlock()
		return ErrOwnerBusy
	}
	if !exists {
		unlock()
		return fmt.Errorf("MCP server %q disappeared while removing: %w", name, config.ErrMCPNotFound)
	}
	if mcpCfg.Source == config.MCPSourceExternal {
		unlock()
		return fmt.Errorf("MCP server %q is from .mcp.json and cannot be removed (disable it instead): %w", name, config.ErrMCPExternal)
	}
	if transaction := o.pendingGlobalAdd(name, cfg); transaction != nil {
		if !o.acceptsSession() {
			unlock()
			return ErrOwnerBusy
		}
		result, persistErr := persist(cfg, config.ScopeGlobal, name)
		var commitUncertainty error
		if persistErr != nil {
			outcome, ok := config.CommitOutcomeFromError(persistErr)
			if !ok || (!outcome.Committed && !outcome.MaybeCommitted) {
				unlock()
				return fmt.Errorf("failed to remove pending MCP server %q from config: %w", name, persistErr)
			}
			transaction.markUserMutation()
			if commitOutcomeNeedsRuntimeFence(outcome) {
				detached = fenceMCPRuntimeLocked(o, cfg, name)
				_, _ = cfg.RemoveMCP(name)
				unlock()
				return fmt.Errorf("failed to remove pending MCP server %q from config: %w", name, persistErr)
			}
			commitUncertainty = persistErr
		}
		if result.NewExists {
			unlock()
			return fmt.Errorf("MCP server %q remained configured after removal: %w", name, config.ErrMCPTargetExists)
		}
		transaction.markUserMutation()
		o.invalidateServer(name)
		_, _ = cfg.RemoveMCP(name)
		detached = detachSessionLocked(name)
		clearAdvertised(name)
		states.Del(name)
		// The delete is part of this server's lease linearization point. It
		// must reach subscribers before the lease is released to a same-name
		// Add, and before the detached session can block in Close.
		publishEvent(pubsub.DeletedEvent, Event{Type: EventStateChanged, Name: name, State: StateDisabled})
		unlock()
		if commitUncertainty != nil {
			return fmt.Errorf("failed to remove pending MCP server %q from config: %w", name, commitUncertainty)
		}
		return nil
	}
	scope, err := resolveMCPMutationScope(cfg, name, mcpCfg)
	if err != nil {
		unlock()
		return fmt.Errorf("cannot determine writable scope for MCP server %q: %w", name, err)
	}
	if !o.acceptsSession() {
		unlock()
		return ErrOwnerBusy
	}
	result, err := persist(cfg, scope, name)
	var commitUncertainty error
	if err != nil {
		outcome, ok := config.CommitOutcomeFromError(err)
		if !ok || (!outcome.Committed && !outcome.MaybeCommitted) {
			unlock()
			return fmt.Errorf("failed to remove MCP server %q from config: %w", name, err)
		}
		if commitOutcomeNeedsRuntimeFence(outcome) {
			detached = fenceMCPRuntimeLocked(o, cfg, name)
			unlock()
			return fmt.Errorf("failed to remove MCP server %q from config: %w", name, err)
		}
		commitUncertainty = err
	}
	// Persistence is the fallible part of this transaction. Only after it
	// succeeds may this operation invalidate candidates; the write lease keeps
	// a candidate from publishing until runtime state is removed.
	o.invalidateServer(name)
	detached = detachSessionLocked(name)
	clearAdvertised(name)
	states.Del(name)
	if result.NewExists {
		if result.NewConfig.Disabled {
			updateState(name, StateDisabled, nil, nil, Counts{})
			unlock()
			if commitUncertainty != nil {
				return fmt.Errorf("failed to remove MCP server %q from config: %w", name, commitUncertainty)
			}
			return nil
		}
		publishEvent(pubsub.DeletedEvent, Event{Type: EventStateChanged, Name: name, State: StateDisabled})
		lease.Unlock()
		startFallbackAfterRetirement(name, detached, cfg, result, o)
		if commitUncertainty != nil {
			return fmt.Errorf("failed to remove MCP server %q from config: %w", name, commitUncertainty)
		}
		return nil
	}
	publishEvent(pubsub.DeletedEvent, Event{Type: EventStateChanged, Name: name, State: StateDisabled})
	unlock()
	if commitUncertainty != nil {
		return fmt.Errorf("failed to remove MCP server %q from config: %w", name, commitUncertainty)
	}
	return nil
}

func startFallbackAfterRetirement(
	name string,
	session *ClientSession,
	cfg *config.ConfigStore,
	result config.MCPMutationResult,
	o *Owner,
) {
	if session == nil {
		o.enqueueFallback(cfg, result)
		return
	}
	adoptSessionForRetirement(name, session)
	session.afterCloseContext(o.lifecycleCtx, func() {
		o.enqueueFallback(cfg, result)
	})
	session.retire()
}

func currentMCPMutationResult(cfg *config.ConfigStore, operation, oldName, newName string) config.MCPMutationResult {
	result := config.MCPMutationResult{
		Operation: operation,
		OldName:   oldName,
		NewName:   newName,
	}
	if current, ok := cfg.MCPConfig(newName); ok {
		result.NewExists = true
		result.NewConfig = current
	}
	if current, ok := cfg.MCPConfig(oldName); ok {
		result.OldExists = true
		result.OldConfig = current
		if operation == "remove" || (operation == "replace" && oldName != newName) {
			result.FallbackExists = true
			result.FallbackConfig = current
		}
	}
	return result
}

type replacementCleanup struct {
	detachedOld *ClientSession
	detachedNew *ClientSession
	canceled    []context.CancelFunc
	events      []replacementEvent
}

type replacementEvent struct {
	kind  pubsub.EventType
	event Event
}

// cleanupReplacementRuntimeLocked leaves both names inactive after a durable
// replacement that can no longer publish its prepared candidate.
func cleanupReplacementRuntimeLocked(o *Owner, cfg *config.ConfigStore, oldName, newName string) replacementCleanup {
	cleanup := replacementCleanup{}
	configured, _ := cfg.Snapshot()
	seen := make(map[string]struct{}, 2)
	for _, name := range []string{oldName, newName} {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		cleanup.canceled = append(cleanup.canceled, o.invalidateServerLocked(name)...)
		var hadRuntime bool
		if name == oldName {
			cleanup.detachedOld = detachSessionLifecycleLocked(name)
		} else {
			cleanup.detachedNew = detachSessionLifecycleLocked(name)
		}
		_, hadState := states.Get(name)
		if name == oldName && cleanup.detachedOld != nil {
			hadRuntime = true
		}
		if name == newName && cleanup.detachedNew != nil {
			hadRuntime = true
		}
		clearAdvertised(name)
		_, exists := configured.MCP[name]
		if exists {
			setState(name, StateDisabled, nil, nil, Counts{})
			if name == newName || hadRuntime || hadState {
				cleanup.events = append(cleanup.events, replacementEvent{
					kind:  pubsub.UpdatedEvent,
					event: Event{Type: EventStateChanged, Name: name, State: StateDisabled},
				})
			}
		} else {
			states.Del(name)
			emitDeleted := (name == newName && oldName != newName) ||
				(name != newName && (hadRuntime || hadState))
			if emitDeleted {
				cleanup.events = append(cleanup.events, replacementEvent{
					kind:  pubsub.DeletedEvent,
					event: Event{Type: EventStateChanged, Name: name, State: StateDisabled},
				})
			}
		}
	}
	return cleanup
}

// startFallback keeps the old test/helper shape while production callers pass
// the exact durable mutation through startFallbackMutation.
func startFallback(ctx context.Context, cfg *config.ConfigStore, name string, mcpCfg config.MCPConfig, o *Owner) {
	snapshot := cfg.SnapshotMCPAdmission(name)
	result := config.MCPMutationResult{
		Operation:  "fallback",
		OldName:    name,
		NewName:    name,
		Generation: snapshot.Generation,
		NewExists:  snapshot.Exists,
		NewConfig:  mcpCfg,
	}
	startFallbackMutation(ctx, cfg, result, o)
}

func startFallbackMutation(ctx context.Context, cfg *config.ConfigStore, result config.MCPMutationResult, o *Owner) {
	if o == nil {
		return
	}
	name, mcpCfg, selectedExists := fallbackDefinition(result)
	if !selectedExists {
		return
	}
	if result.Operation == "fallback" && mcpCfg.Disabled {
		lease := serverLeaseFor(name)
		if lease.lockContext(ctx, true) {
			setState(name, StateDisabled, nil, nil, Counts{})
			publishStateEvent(name, StateDisabled, nil, Counts{})
			lease.Unlock()
		}
		return
	}
	if !selectedExists || mcpCfg.Disabled {
		return
	}
	if err := o.reconcileUncertainty(ctx, cfg, name); err != nil {
		slog.Debug("Failed to reconcile MCP fallback config", "name", name, "err", err)
		return
	}
	lease := serverLeaseFor(name)
	if !lease.lockContext(ctx, true) {
		return
	}
	var admission serverAdmission
	var resolver config.VariableResolver
	var startErr error
	var startingEvent Event
	var publishStarting bool
	publicationErr := cfg.WithCurrentMCPMutation(result, func() error {
		snapshot := cfg.SnapshotMCPAdmission(name)
		if !snapshot.Exists || snapshot.MCPConfig.Disabled || !reflect.DeepEqual(snapshot.MCPConfig, mcpCfg) {
			return config.ErrMCPMutationStale
		}
		resolver = snapshot.Resolver
		admission, startErr = o.admitServerForConfig(ctx, cfg, name, snapshot.MCPConfig, true)
		if startErr != nil {
			return startErr
		}
		admission.mcpRevision = snapshot.MCPRevision
		admission.resolverRevision = snapshot.ResolverRevision
		admission.mcpAdmission = snapshot
		admission.mutationResult = &result
		startingEvent, publishStarting = admissionStateEventUnpinned(&admission, StateStarting, nil, nil, Counts{})
		return nil
	})
	lease.Unlock()
	if publicationErr == nil && publishStarting {
		publishEvent(pubsub.UpdatedEvent, startingEvent)
	}
	if publicationErr != nil {
		if startErr != nil {
			cleanupFailedAdmission(&admission, startErr)
		}
		return
	}
	go func() {
		if err := initClientAdmittedWithState(ctx, cfg, name, mcpCfg, resolver, &admission, false); err != nil {
			cleanupFailedAdmission(&admission, err)
			slog.Error("Failed to initialize revealed MCP server", "name", name, "err", err)
		}
	}()
}

func fallbackDefinition(result config.MCPMutationResult) (string, config.MCPConfig, bool) {
	if result.Operation == "replace" && result.OldName != result.NewName {
		return result.OldName, result.FallbackConfig, result.FallbackExists
	}
	return result.NewName, result.NewConfig, result.NewExists
}

func clearAdvertised(name string) {
	allTools.Del(name)
	allPrompts.Del(name)
	allResources.Del(name)
}
