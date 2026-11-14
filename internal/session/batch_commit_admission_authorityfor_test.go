package session

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestBatchCommitAdmissionAuthorityForAdmitsBoundOwner(t *testing.T) {
	env := newCommitHookEnv(t)
	service := finalizationCommitService(env)
	authority := finalizationCommitAuthority(t, service, "owner", "lookup-call", 1)
	finalizationCommitFailureDiagnostics(t, env, authority.identity)
	identity, err := service.WithAgentFinalizationAuthorities(env.id)
	require.NoError(t, err)
	identity.AuthorityFor = func(owner string) *AgentFinalizationAuthority {
		if owner == "owner" {
			return authority
		}
		return nil
	}
	require.NoError(t, NewBatchCommitHook(env.store, identity)(env.ctx, admissionSubmit("r", "call-r", 1)))
	require.EqualValues(t, 1, commitHookCount(t, env.conn,
		`SELECT count(*) FROM session_agent_finalization_tickets WHERE logical_call_id='lookup-call' AND released=1`),
		"the ticket was admitted and then released")
	require.Greater(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes`), int64(0),
		"the reservation is committed")
}

func TestBatchCommitAdmissionAuthorityForNilLookupRefusesBoundOwner(t *testing.T) {
	env := newCommitHookEnv(t)
	service := finalizationCommitService(env)
	authority := finalizationCommitAuthority(t, service, "owner", "lookup-call", 1)
	finalizationCommitFailureDiagnostics(t, env, authority.identity)
	identity, err := service.WithAgentFinalizationAuthorities(env.id)
	require.NoError(t, err)
	identity.AuthorityFor = func(string) *AgentFinalizationAuthority { return nil }
	err = NewBatchCommitHook(env.store, identity)(env.ctx, admissionSubmit("r", "call-r", 1))
	require.ErrorContains(t, err, "explicit authority required for owner")
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes`))
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_runner_checkpoints`))
}

func TestBatchCommitAdmissionAuthorityForForeignServiceConflicts(t *testing.T) {
	env := newCommitHookEnv(t)
	// The second database is a separate store; batchStoreTestDB already seeds
	// the "owner" session in it, so the foreign authority binds there.
	_, otherConn, _ := batchStoreTestDB(t)
	otherService := NewService(db.New(otherConn), otherConn).(AgentFinalizationService)
	foreignAuthority, err := otherService.BindAgentFinalization(env.ctx,
		AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "foreign-call", Generation: 1})
	require.NoError(t, err)
	service := finalizationCommitService(env)
	identity, err := service.WithAgentFinalizationAuthorities(env.id)
	require.NoError(t, err)
	identity.AuthorityFor = func(owner string) *AgentFinalizationAuthority {
		if owner == "owner" {
			return foreignAuthority
		}
		return nil
	}
	err = NewBatchCommitHook(env.store, identity)(env.ctx, admissionSubmit("r", "call-r", 1))
	require.ErrorContains(t, err, "conflicting authority binding")
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes`))
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_runner_checkpoints`))
}

func TestBatchCommitAdmissionAuthorityForNilKeepsLegacyRefusal(t *testing.T) {
	env := newCommitHookEnv(t)
	service := finalizationCommitService(env)
	authority := finalizationCommitAuthority(t, service, "owner", "lookup-call", 1)
	finalizationCommitFailureDiagnostics(t, env, authority.identity)
	identity, err := service.WithAgentFinalizationAuthorities(env.id)
	require.NoError(t, err)
	require.Nil(t, identity.AuthorityFor, "the zero value keeps the lookup nil")
	err = NewBatchCommitHook(env.store, identity)(env.ctx, admissionSubmit("r", "call-r", 1))
	require.ErrorContains(t, err, "explicit authority required for owner")
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes`))
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_runner_checkpoints`))
}
