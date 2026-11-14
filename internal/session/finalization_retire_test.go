package session

import (
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestAgentFinalizationRetireStaleActiveInterruptsForeignActiveRecords(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	_, err := env.svc.BindAgentFinalization(env.ctx, AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "call-a", Generation: 1})
	require.NoError(t, err)
	retired, err := env.store.RetireStaleActive(env.ctx, "owner", "call-b")
	require.NoError(t, err)
	require.EqualValues(t, 1, retired)
	record, err := env.store.Snapshot(env.ctx, AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "call-a", Generation: 1})
	require.NoError(t, err)
	require.Equal(t, "interrupted", record.Phase)
	require.EqualValues(t, 1, record.Revision)
	_, err = env.store.Snapshot(env.ctx, AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "call-b", Generation: 1})
	require.ErrorIs(t, err, sql.ErrNoRows, "the keep id was never bound, so no record exists")
}

func TestAgentFinalizationRetireStaleActiveKeepsOwnAndTerminalRecords(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	// The partial unique index keeps at most one active record per session,
	// so "done-call" binds first and "keep-call" binds only after
	// "done-call" has left the active vocabulary.
	_, err := env.svc.BindAgentFinalization(env.ctx, AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "done-call", Generation: 1})
	require.NoError(t, err)
	retired, err := env.store.RetireStaleActive(env.ctx, "owner", "keep-call")
	require.NoError(t, err)
	require.EqualValues(t, 1, retired)
	_, err = env.conn.ExecContext(env.ctx, `UPDATE session_agent_finalizations SET phase='accepted' WHERE session_id=? AND logical_call_id=?`, "owner", "done-call")
	require.NoError(t, err)
	_, err = env.svc.BindAgentFinalization(env.ctx, AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "keep-call", Generation: 1})
	require.NoError(t, err)
	retired, err = env.store.RetireStaleActive(env.ctx, "owner", "keep-call")
	require.NoError(t, err)
	require.EqualValues(t, 0, retired, "the kept record and the terminal record are never touched")
	keep, err := env.store.Snapshot(env.ctx, AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "keep-call", Generation: 1})
	require.NoError(t, err)
	require.Equal(t, "executing", keep.Phase)
	require.EqualValues(t, 0, keep.Revision)
	done, err := env.store.Snapshot(env.ctx, AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "done-call", Generation: 1})
	require.NoError(t, err)
	require.Equal(t, "accepted", done.Phase)
}

func TestAgentFinalizationRetireStaleActiveIsIdempotentAndScopedToSession(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	_, err := env.svc.BindAgentFinalization(env.ctx, AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "a", Generation: 1})
	require.NoError(t, err)
	require.NoError(t, seedSession(env.ctx, db.New(env.conn), "other-owner"))
	_, err = env.svc.BindAgentFinalization(env.ctx, AgentFinalizationIdentity{SessionID: "other-owner", LogicalCallID: "b", Generation: 1})
	require.NoError(t, err)
	retired, err := env.store.RetireStaleActive(env.ctx, "owner", "x")
	require.NoError(t, err)
	require.EqualValues(t, 1, retired)
	retired, err = env.store.RetireStaleActive(env.ctx, "owner", "x")
	require.NoError(t, err)
	require.EqualValues(t, 0, retired, "the second call finds no matching rows")
	other, err := env.store.Snapshot(env.ctx, AgentFinalizationIdentity{SessionID: "other-owner", LogicalCallID: "b", Generation: 1})
	require.NoError(t, err)
	require.Equal(t, "executing", other.Phase)
	// "a" is terminal now, so the fresh "a2" incarnation can bind and the
	// empty keep id must retire every active record of the session.
	_, err = env.svc.BindAgentFinalization(env.ctx, AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "a2", Generation: 1})
	require.NoError(t, err)
	retired, err = env.store.RetireStaleActive(env.ctx, "owner", "")
	require.NoError(t, err)
	require.EqualValues(t, 1, retired)
	_, err = env.store.RetireStaleActive(env.ctx, "", "x")
	require.Error(t, err, "an empty session is refused")
	_, err = env.store.RetireCancelledLogical(env.ctx, "owner", "")
	require.Error(t, err, "an empty logical call is refused")
}

func TestAgentFinalizationRetireCancelledLogicalMarksOnlyTargetActiveRecord(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	_, err := env.svc.BindAgentFinalization(env.ctx, AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "target", Generation: 1})
	require.NoError(t, err)
	retired, err := env.store.RetireCancelledLogical(env.ctx, "owner", "target")
	require.NoError(t, err)
	require.EqualValues(t, 1, retired)
	// The partial unique index keeps at most one active record per session,
	// so "survivor" binds only after "target" is terminal.
	_, err = env.svc.BindAgentFinalization(env.ctx, AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "survivor", Generation: 1})
	require.NoError(t, err)
	target, err := env.store.Snapshot(env.ctx, AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "target", Generation: 1})
	require.NoError(t, err)
	require.Equal(t, "cancelled", target.Phase)
	require.EqualValues(t, 1, target.Revision)
	survivor, err := env.store.Snapshot(env.ctx, AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "survivor", Generation: 1})
	require.NoError(t, err)
	require.Equal(t, "executing", survivor.Phase)
	require.EqualValues(t, 0, survivor.Revision)
	retired, err = env.store.RetireCancelledLogical(env.ctx, "owner", "target")
	require.NoError(t, err)
	require.EqualValues(t, 0, retired, "the terminal record is never touched again")
	target, err = env.store.Snapshot(env.ctx, AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "target", Generation: 1})
	require.NoError(t, err)
	require.Equal(t, "cancelled", target.Phase)
	retired, err = env.store.RetireCancelledLogical(env.ctx, "owner", "missing")
	require.NoError(t, err)
	require.EqualValues(t, 0, retired)
}
