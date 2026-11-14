package app

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// newFinalizationAuthorityApp builds a bare installable App over an isolated
// config store with the finalization flag set to enabled.
func newFinalizationAuthorityApp(t *testing.T, enabled bool) *App {
	t.Helper()
	isolation := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", isolation)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolation)
	store, err := config.Init(isolation, isolation, false)
	require.NoError(t, err)
	on := enabled
	store.Config().Options.Finalization = &config.FinalizationOptions{Enabled: &on}
	dataDir := t.TempDir()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	q := db.New(conn)
	return &App{
		DB:       func() *sql.DB { return conn },
		dbConns:  []*sql.DB{conn},
		dataDir:  dataDir,
		Sessions: session.NewService(q, conn),
		Messages: message.NewService(q),
		config:   store,
	}
}

// TestAppFinalizationAuthorityRegistryFlagOffIsNil pins the flag-off
// contract: the registry is never allocated, repeated calls stay nil, and
// the finalization wiring install short-circuits without touching anything.
// SEQUENTIAL: batchInstallDepsHook is a package global (see the install
// tests below).
func TestAppFinalizationAuthorityRegistryFlagOffIsNil(t *testing.T) {
	app := newFinalizationAuthorityApp(t, false)
	require.Nil(t, app.finalizationAuthorityRegistry(),
		"the flag-off App must not allocate a registry")
	require.Nil(t, app.finalizationAuthorityRegistry(),
		"a repeated call must stay nil, not memoize lazily")
	require.NoError(t, app.installAgentFinalizationWiring(),
		"the flag-off wiring install must short-circuit to nil")
}

// TestAppFinalizationAuthorityRegistryFlagOnSingleInstance pins the
// exactly-once contract: with the flag on, repeated accesses return the
// SAME registry instance — the one both the batch install and the agent
// wiring must observe. SEQUENTIAL (package-global hook).
func TestAppFinalizationAuthorityRegistryFlagOnSingleInstance(t *testing.T) {
	app := newFinalizationAuthorityApp(t, true)
	r1 := app.finalizationAuthorityRegistry()
	require.NotNil(t, r1, "the flag-on App must build the registry on first use")
	r2 := app.finalizationAuthorityRegistry()
	require.Same(t, r1, r2, "the registry must be built exactly once per App")
}

// TestAppFinalizationAuthorityRegistryReachesBatchInstallDeps proves the
// install captures the registry's Lookup into the batch deps, and that the
// captured lookup closes over the SAME instance a root bind registers into:
// a real bound authority is visible through the captured lookup, an unknown
// owner is not. SEQUENTIAL (package-global hook); the ShutdownWithResult
// cleanup joins the runtime the install started.
func TestAppFinalizationAuthorityRegistryReachesBatchInstallDeps(t *testing.T) {
	app := newFinalizationAuthorityApp(t, true)
	ctx := context.Background()
	owner, err := app.Sessions.Create(ctx, "authority-registry-owner")
	require.NoError(t, err)

	var captured agent.BatchInstallDeps
	old := batchInstallDepsHook
	batchInstallDepsHook = func(deps *agent.BatchInstallDeps) { captured = *deps }
	t.Cleanup(func() { batchInstallDepsHook = old })
	require.NoError(t, app.installBatchRuntime(ctx))
	t.Cleanup(func() { _ = app.ShutdownWithResult() })

	require.NotNil(t, captured.FinalizationAuthorityFor,
		"the flag-on install must carry the registry lookup into the batch deps")

	api, ok := app.Sessions.(session.AgentFinalizationService)
	require.True(t, ok)
	authority, err := api.BindAgentFinalization(ctx, session.AgentFinalizationIdentity{SessionID: owner.ID, LogicalCallID: "reg-call", Generation: 1})
	require.NoError(t, err)
	unregister := app.finalizationAuthorityRegistry().Register(owner.ID, authority)
	defer unregister()
	require.Same(t, authority, captured.FinalizationAuthorityFor(owner.ID),
		"the captured lookup must resolve the authority registered on the SAME instance")
	require.Nil(t, captured.FinalizationAuthorityFor("no-such-owner"))
}

// TestAppFinalizationAuthorityRegistryFlagOffInstallKeepsLookupNil pins the
// flag-off byte-identical install contract: the install succeeds, the deps
// carry NO lookup, and the App still holds no registry. SEQUENTIAL
// (package-global hook).
func TestAppFinalizationAuthorityRegistryFlagOffInstallKeepsLookupNil(t *testing.T) {
	app := newFinalizationAuthorityApp(t, false)
	ctx := context.Background()
	owner, err := app.Sessions.Create(ctx, "authority-registry-owner-off")
	require.NoError(t, err)
	require.NotEmpty(t, owner.ID)

	var captured agent.BatchInstallDeps
	old := batchInstallDepsHook
	batchInstallDepsHook = func(deps *agent.BatchInstallDeps) { captured = *deps }
	t.Cleanup(func() { batchInstallDepsHook = old })
	require.NoError(t, app.installBatchRuntime(ctx))
	t.Cleanup(func() { _ = app.ShutdownWithResult() })

	require.Nil(t, captured.FinalizationAuthorityFor,
		"the flag-off install must leave the batch deps byte-identical")
	require.Nil(t, app.finalizationAuthorityRegistry(),
		"an installed flag-off App must still hold no registry")
}
