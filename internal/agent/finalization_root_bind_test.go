package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// newRootBindFixture builds a wiring-backed agent whose sessions service
// implements the finalization API.
func newRootBindFixture(t *testing.T) (fakeEnv, *sessionAgent, *finalizationWiring) {
	t.Helper()
	env := testEnv(t)
	a := &sessionAgent{sessions: env.sessions}
	coord := &coordinator{currentAgent: a}
	w, err := newFinalizationWiring(finalizationWiringDepsOn(env), coord)
	require.NoError(t, err)
	w.authorities = NewFinalizationAuthorityRegistry()
	coord.finalization = w
	a.coord = coord
	return env, a, w
}

// bindRootFixtureAuthority binds a real authority through the fixture's
// sessions service.
func bindRootFixtureAuthority(t *testing.T, env fakeEnv, sessionID, logical string) *session.AgentFinalizationAuthority {
	t.Helper()
	api, ok := env.sessions.(session.AgentFinalizationService)
	require.True(t, ok)
	authority, err := api.BindAgentFinalization(t.Context(), session.AgentFinalizationIdentity{
		SessionID: sessionID, LogicalCallID: logical, Generation: 1,
	})
	require.NoError(t, err)
	return authority
}

// TestFinalizationRootBindCreatesRecordAndCarriesAuthority walks the happy
// root bind: the durable generation-1 record, the authority on the call and
// in the context, the process registry entry, the OnCancel reach into the
// gate, and the unregister on cleanup.
func TestFinalizationRootBindCreatesRecordAndCarriesAuthority(t *testing.T) {
	env, a, w := newRootBindFixture(t)
	store := session.NewAgentFinalizationStore(env.conn)
	identity := session.AgentFinalizationIdentity{SessionID: "session", LogicalCallID: "logical", Generation: 1}
	call := SessionAgentCall{SessionID: "session", LogicalCallID: "logical"}

	execCtx, got, cleanup, err := a.startExecution(t.Context(), call)
	require.NoError(t, err)
	require.NotNil(t, cleanup)

	exec := finalizationExecutionFor(execCtx, got)
	require.NotNil(t, exec)
	record, err := store.Snapshot(t.Context(), identity)
	require.NoError(t, err)
	require.Equal(t, "executing", record.Phase)

	requirement := got.finalizationAuthorities.Requirement()
	require.NotNil(t, requirement)
	require.Contains(t, requirement.Authorities, identity)
	require.NotNil(t, session.CaptureAgentFinalizationContext(execCtx).Requirement())

	require.NotNil(t, w.authorities.Lookup("session"))
	require.Nil(t, w.authorities.Lookup("other"))

	// The registered execution is cancellable through the coordinator
	// interrupt while the cleanup has not run yet.
	w.hooks.OnCancel("session")
	require.True(t, exec.gate.cancelled)

	cleanup()
	require.Nil(t, w.authorities.Lookup("session"))
}

// TestFinalizationRootBindRetiresStaleActiveBeforeBind proves the root bind
// retires every other active incarnation of the session before binding the
// fresh one.
func TestFinalizationRootBindRetiresStaleActiveBeforeBind(t *testing.T) {
	env, a, _ := newRootBindFixture(t)
	store := session.NewAgentFinalizationStore(env.conn)
	oldIdentity := session.AgentFinalizationIdentity{SessionID: "session", LogicalCallID: "old", Generation: 1}
	bindRootFixtureAuthority(t, env, "session", "old")

	fresh := SessionAgentCall{SessionID: "session", LogicalCallID: "fresh"}
	_, _, cleanup, err := a.startExecution(t.Context(), fresh)
	require.NoError(t, err)

	old, err := store.Snapshot(t.Context(), oldIdentity)
	require.NoError(t, err)
	require.Equal(t, "interrupted", old.Phase)
	require.EqualValues(t, 1, old.Revision)

	freshRecord, err := store.Snapshot(t.Context(), session.AgentFinalizationIdentity{
		SessionID: "session", LogicalCallID: "fresh", Generation: 1,
	})
	require.NoError(t, err)
	require.Equal(t, "executing", freshRecord.Phase)

	cleanup()
}

// TestFinalizationRootBindSubAgentCallIsInert proves a sub-agent call passes
// through without binding its own record and stays inert.
func TestFinalizationRootBindSubAgentCallIsInert(t *testing.T) {
	env, a, _ := newRootBindFixture(t)
	parent := bindRootFixtureAuthority(t, env, "session", "parent-call")
	bound, err := session.WithAgentFinalizationContext(t.Context(), parent)
	require.NoError(t, err)

	child := SessionAgentCall{SessionID: "child", LogicalCallID: "child-call", driverGeneration: 7}
	child.finalizationAuthorities = session.CaptureAgentFinalizationContext(bound)
	sub := &sessionAgent{sessions: env.sessions, isSubAgent: true, coord: a.coord}

	execCtx, got, cleanup, err := sub.startExecution(t.Context(), child)
	require.NoError(t, err)
	require.NotPanics(t, cleanup)

	var rows int
	require.NoError(t, env.conn.QueryRowContext(t.Context(),
		`SELECT count(*) FROM session_agent_finalizations WHERE session_id='child'`).Scan(&rows))
	require.Zero(t, rows, "an inert call must not create a finalization record")

	require.Nil(t, finalizationExecutionFor(execCtx, got))
}

// TestFinalizationRootBindAcceptedLogicalCallFallsBackLegacy proves a rerun
// against an already accepted record applies no finalization: the call goes
// legacy with no error, no registry entry and no touched rows.
func TestFinalizationRootBindAcceptedLogicalCallFallsBackLegacy(t *testing.T) {
	env, a, w := newRootBindFixture(t)
	bindRootFixtureAuthority(t, env, "session", "accepted-call")
	_, err := env.conn.ExecContext(t.Context(),
		`UPDATE session_agent_finalizations SET phase='accepted'
		 WHERE session_id='session' AND logical_call_id='accepted-call'`)
	require.NoError(t, err)

	call := SessionAgentCall{SessionID: "session", LogicalCallID: "accepted-call"}
	execCtx, got, cleanup, err := a.startExecution(t.Context(), call)
	require.NoError(t, err)
	require.NotNil(t, cleanup)

	require.Nil(t, session.CaptureAgentFinalizationContext(execCtx).Requirement())
	require.Nil(t, got.finalizationAuthorities.Requirement())
	require.Nil(t, finalizationExecutionFor(execCtx, got))
	require.Nil(t, w.authorities.Lookup("session"))

	var rows int
	require.NoError(t, env.conn.QueryRowContext(t.Context(),
		`SELECT count(*) FROM session_agent_finalizations WHERE session_id='session'`).Scan(&rows))
	require.Equal(t, 1, rows)
	var phase string
	require.NoError(t, env.conn.QueryRowContext(t.Context(),
		`SELECT phase FROM session_agent_finalizations WHERE session_id='session'`).Scan(&phase))
	require.Equal(t, "accepted", phase)

	require.NotPanics(t, cleanup)
}

// TestFinalizationRootBindNilWiringIsNoop proves a flag-off agent passes the
// call through untouched.
func TestFinalizationRootBindNilWiringIsNoop(t *testing.T) {
	a := &sessionAgent{}
	call := SessionAgentCall{SessionID: "session", LogicalCallID: "logical"}
	execCtx, got, cleanup, err := a.startExecution(t.Context(), call)
	require.NoError(t, err)
	require.NotNil(t, execCtx)
	require.Equal(t, call, got)
	require.NotNil(t, cleanup)
	require.NotPanics(t, cleanup)
}

// TestFinalizationRootBindCleanupMarksCancelledOnCanceledContext proves the
// cleanup retires the still-active record as cancelled when the run unwound
// through cancellation, and that a second cleanup is a guarded no-op.
func TestFinalizationRootBindCleanupMarksCancelledOnCanceledContext(t *testing.T) {
	env, a, w := newRootBindFixture(t)
	store := session.NewAgentFinalizationStore(env.conn)
	identity := session.AgentFinalizationIdentity{SessionID: "session", LogicalCallID: "cancel-target", Generation: 1}

	parent, cancel := context.WithCancel(t.Context())
	defer cancel()

	_, _, cleanup, err := a.startExecution(parent, SessionAgentCall{
		SessionID: "session", LogicalCallID: "cancel-target",
	})
	require.NoError(t, err)
	record, err := store.Snapshot(t.Context(), identity)
	require.NoError(t, err)
	require.Equal(t, "executing", record.Phase)

	cancel()
	cleanup()
	require.NotPanics(t, cleanup, "the cleanup is guarded by a once")

	record, err = store.Snapshot(t.Context(), identity)
	require.NoError(t, err)
	require.Equal(t, "cancelled", record.Phase)
	require.EqualValues(t, 1, record.Revision)
	require.Nil(t, w.authorities.Lookup("session"))
}

// TestFinalizationRootBindRefusesWithoutRegistry proves the root bind fails
// closed when no authority registry is installed, without touching the
// durable records.
func TestFinalizationRootBindRefusesWithoutRegistry(t *testing.T) {
	env, a, w := newRootBindFixture(t)
	w.authorities = nil

	_, _, _, err := a.startExecution(t.Context(), SessionAgentCall{
		SessionID: "session", LogicalCallID: "logical",
	})
	require.ErrorContains(t, err, "authority registry unavailable")

	var rows int
	require.NoError(t, env.conn.QueryRowContext(t.Context(),
		`SELECT count(*) FROM session_agent_finalizations WHERE session_id='session'`).Scan(&rows))
	require.Zero(t, rows, "a refused bind must not create a finalization record")
}
