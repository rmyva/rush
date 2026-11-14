package app

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// finalizationWiringTestCoordinator is the minimal non-nil coordinator for
// install tests; none of its embedded methods is invoked by the installer.
type finalizationWiringTestCoordinator struct {
	agent.Coordinator
}

// TestAppFinalizationWiringInstallFlagOffIsNoop pins the flag-off contract
// at the composition root: no wiring, no coordinator binding, no terminal
// gate allocation.
func TestAppFinalizationWiringInstallFlagOffIsNoop(t *testing.T) {
	// A config-less App is a no-op regardless of the coordinator.
	var nilConfigApp *App = &App{AgentCoordinator: finalizationWiringTestCoordinator{}}
	require.NoError(t, nilConfigApp.installAgentFinalizationWiring())
	_, ok := nilConfigApp.AgentCoordinator.(finalizationWiringTestCoordinator)
	require.True(t, ok, "the coordinator value stays untouched")
	require.Nil(t, nilConfigApp.FinalizationTerminalGate())

	// An isolated config store with the flag explicitly off.
	isolation := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", isolation)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolation)
	store, err := config.Init(isolation, isolation, false)
	require.NoError(t, err)
	off := false
	store.Config().Options.Finalization = &config.FinalizationOptions{Enabled: &off}

	app := &App{config: store, AgentCoordinator: finalizationWiringTestCoordinator{}}
	require.NoError(t, app.installAgentFinalizationWiring())
	coord, ok := app.AgentCoordinator.(finalizationWiringTestCoordinator)
	require.True(t, ok)
	require.NotNil(t, coord, "the coordinator must stay the same value")
	require.Nil(t, app.FinalizationTerminalGate(), "the W1b terminal gate must not be built while the flag is off")
}

// TestAppFinalizationWiringInstallFlagOnFailsClosedWithoutDB pins the
// fail-closed contract: with the flag on, a missing database accessor or a
// nil coordinator must never install a half wiring.
func TestAppFinalizationWiringInstallFlagOnFailsClosedWithoutDB(t *testing.T) {
	// Nil coordinator short-circuits before any flag work.
	nilCoordApp := &App{}
	require.NoError(t, nilCoordApp.installAgentFinalizationWiring())

	isolation := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", isolation)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolation)
	store, err := config.Init(isolation, isolation, false)
	require.NoError(t, err)
	on := true
	store.Config().Options.Finalization = &config.FinalizationOptions{Enabled: &on}

	// Flag on with a coordinator but no DB accessor: fail closed.
	app := &App{config: store, AgentCoordinator: finalizationWiringTestCoordinator{}}
	err = app.installAgentFinalizationWiring()
	require.Error(t, err)
	require.Nil(t, app.FinalizationTerminalGate(), "no gate is built without a database")
}
