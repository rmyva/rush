package app

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// finalizationGateTestConfig builds a config store whose finalization flag
// matches enabled.
func finalizationGateTestConfig(enabled bool) *config.Config {
	opts := &config.Options{}
	if enabled {
		on := true
		opts.Finalization = &config.FinalizationOptions{Enabled: &on}
	}
	return &config.Config{Options: opts}
}

// The accessor must be inert by construction: a hand-assembled App without a
// config, a flag-off config, and a flag-on config without a database writer
// all yield nil, the "no gate" wiring.
func TestFinalizationTerminalGateAccessorInertWithoutFlagOrStore(t *testing.T) {
	t.Parallel()

	require.Nil(t, (&App{}).FinalizationTerminalGate())
	require.Nil(t, (&App{config: config.NewTestStore(finalizationGateTestConfig(false))}).FinalizationTerminalGate())
	require.Nil(t, (&App{config: config.NewTestStore(finalizationGateTestConfig(true))}).FinalizationTerminalGate())
}

// With the flag on and a real database the accessor returns a working
// polling gate: an active-phase finalization record refuses terminal
// delivery, a terminal phase and an unknown owner allow it, AwaitSealed
// returns nil once sealed, and the gate is cached per App.
func TestFinalizationTerminalGateAccessorFlagOnWorkingGateOverStore(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })

	a := &App{
		config: config.NewTestStore(finalizationGateTestConfig(true)),
		DB:     func() *sql.DB { return conn },
	}
	gate := a.FinalizationTerminalGate()
	require.NotNil(t, gate)
	require.Equal(t, gate, a.FinalizationTerminalGate(), "the gate must be cached per App")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, err = db.New(conn).CreateSession(ctx, db.CreateSessionParams{ID: "owner", Title: "owner"})
	require.NoError(t, err)
	identity := session.AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "call", Generation: 1}
	require.NoError(t, session.NewAgentFinalizationStore(conn).Bind(ctx, identity))
	// Pin the phase explicitly so the fixture does not depend on the schema
	// default of a fresh record.
	_, err = conn.ExecContext(ctx, `UPDATE session_agent_finalizations SET phase='executing' WHERE session_id='owner'`)
	require.NoError(t, err)
	require.False(t, gate.AllowTerminal(ctx, "owner"),
		"an active-phase finalization record must refuse terminal delivery")

	_, err = conn.ExecContext(ctx, `UPDATE session_agent_finalizations SET phase='accepted' WHERE session_id='owner'`)
	require.NoError(t, err)
	require.True(t, gate.AllowTerminal(ctx, "owner"),
		"a sealed (terminal-phase) record must allow terminal delivery")
	require.NoError(t, gate.AwaitSealed(ctx, "owner"))
	require.True(t, gate.AllowTerminal(ctx, "nobody"),
		"an owner without any record must allow terminal delivery")
}
