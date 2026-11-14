package agent

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// finalizationSeamScopeStub implements the four session facts sources with
// fixed counters, recording every transaction it saw.
type finalizationSeamScopeStub struct {
	tx                                                 *sql.Tx
	descendants, queued, holds, debt                   int
	descendantCalls, queuedCalls, holdCalls, debtCalls int
}

func (s *finalizationSeamScopeStub) FinalizationActiveDescendantsTx(_ context.Context, tx *sql.Tx, _ string) (int, error) {
	s.descendantCalls++
	s.tx = tx
	return s.descendants, nil
}

func (s *finalizationSeamScopeStub) FinalizationQueuedCallsTx(_ context.Context, tx *sql.Tx, _ string) (int, error) {
	s.queuedCalls++
	s.tx = tx
	return s.queued, nil
}

func (s *finalizationSeamScopeStub) FinalizationPhysicalHoldsTx(_ context.Context, tx *sql.Tx, _ string) (int, error) {
	s.holdCalls++
	s.tx = tx
	return s.holds, nil
}

func (s *finalizationSeamScopeStub) FinalizationReactionDebtTx(_ context.Context, tx *sql.Tx, _ string) (int, error) {
	s.debtCalls++
	s.tx = tx
	return s.debt, nil
}

var (
	_ session.FinalizationDescendantsFacts   = (*finalizationSeamScopeStub)(nil)
	_ session.FinalizationQueuedCallsFacts   = (*finalizationSeamScopeStub)(nil)
	_ session.FinalizationPhysicalHoldsFacts = (*finalizationSeamScopeStub)(nil)
	_ session.FinalizationReactionDebtFacts  = (*finalizationSeamScopeStub)(nil)
)

// finalizationDriverSeamEnv is the bound-driver fixture: a real migrated
// database, an active bound finalization record, the composite scope reader
// over the stub sources, and a finalizationExecution carrying gate, scope,
// reader and a driver-ready context.
type finalizationDriverSeamEnv struct {
	fakeEnv
	ctx       context.Context
	call      SessionAgentCall
	identity  session.AgentFinalizationIdentity
	scope     finalizationPublicationScope
	execution *finalizationExecution
	reader    session.AgentFinalizationScopeReader
	stub      *finalizationSeamScopeStub
}

// newFinalizationDriverSeamEnv binds "session"/"logical" through the real
// BindAgentFinalization producer and installs a finalizationExecution whose
// scopeFacts is the composite reader over the stub sources.
func newFinalizationDriverSeamEnv(t *testing.T) finalizationDriverSeamEnv {
	t.Helper()
	env := finalizationDriverSeamEnv{fakeEnv: testEnv(t)}
	env.identity = session.AgentFinalizationIdentity{SessionID: "session", LogicalCallID: "logical", Generation: 1}
	api, ok := env.sessions.(session.AgentFinalizationService)
	require.True(t, ok)
	_, err := api.BindAgentFinalization(t.Context(), env.identity)
	require.NoError(t, err)
	env.stub = &finalizationSeamScopeStub{}
	env.reader = session.NewAgentFinalizationScopeReader(env.identity.SessionID, env.stub, env.stub, env.stub, env.stub)
	env.scope = finalizationPublicationScope{store: session.NewAgentFinalizationStore(env.conn), identity: env.identity}
	env.execution = &finalizationExecution{
		sessionID: env.identity.SessionID,
		logicalID: env.identity.LogicalCallID,
		gate:      newFinalizationGate(uint64(env.identity.Generation)),
		read: func(context.Context) (finalizationFacts, error) {
			return finalizationFacts{complete: true}, nil
		},
		scope:      env.scope,
		scopeFacts: env.reader,
	}
	env.call = SessionAgentCall{SessionID: env.identity.SessionID, LogicalCallID: env.identity.LogicalCallID}
	env.ctx, err = withFinalizationExecution(t.Context(), env.execution)
	require.NoError(t, err)
	return env
}

// finalizationSeamHooks returns the complete hooks set with inert bodies.
func finalizationSeamHooks() FinalizationDriverHooks {
	return FinalizationDriverHooks{
		BindScope: func(context.Context, SessionAgentCall) error { return nil },
		RecoverScope: func(context.Context, *finalizationExecution) (session.AgentFinalizationRecovery, error) {
			return session.AgentFinalizationNoCandidate, nil
		},
		FinalizeTurn: func(context.Context, *turnStream) error { return nil },
		OnCancel:     func(string) {},
	}
}

// TestFinalizationDriverSeamEnvBindsExecution checks the fixture itself: the
// bound record is active, the execution is reachable through the driver
// context, and the composite reader reports complete facts in a transaction.
func TestFinalizationDriverSeamEnvBindsExecution(t *testing.T) {
	env := newFinalizationDriverSeamEnv(t)
	record, err := session.NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, env.identity)
	require.NoError(t, err)
	require.Equal(t, "executing", record.Phase)
	require.Same(t, env.execution, finalizationExecutionFor(env.ctx, env.call))
	tx, err := env.conn.BeginTx(env.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	facts, err := env.reader.FinalizationSealScopeFacts(env.ctx, tx)
	require.NoError(t, err)
	require.True(t, facts.Complete)
	require.Same(t, tx, env.stub.tx)
}

// TestFinalizationDriverHooksZeroIsInert pins the zero set as valid and inert.
func TestFinalizationDriverHooksZeroIsInert(t *testing.T) {
	t.Parallel()
	var hooks FinalizationDriverHooks
	require.True(t, hooks.IsZero())
	require.NoError(t, hooks.Validate())
}

// TestFinalizationDriverHooksFullSetIsValid pins the complete set as installable.
func TestFinalizationDriverHooksFullSetIsValid(t *testing.T) {
	t.Parallel()
	hooks := finalizationSeamHooks()
	require.False(t, hooks.IsZero())
	require.NoError(t, hooks.Validate())
}

// TestFinalizationDriverHooksPartialSetIsRefused pins every partial set as an
// installation error naming the unset hooks.
func TestFinalizationDriverHooksPartialSetIsRefused(t *testing.T) {
	t.Parallel()
	for name, drop := range map[string]func(*FinalizationDriverHooks){
		"no BindScope":    func(h *FinalizationDriverHooks) { h.BindScope = nil },
		"no RecoverScope": func(h *FinalizationDriverHooks) { h.RecoverScope = nil },
		"no FinalizeTurn": func(h *FinalizationDriverHooks) { h.FinalizeTurn = nil },
		"no OnCancel":     func(h *FinalizationDriverHooks) { h.OnCancel = nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hooks := finalizationSeamHooks()
			drop(&hooks)
			require.False(t, hooks.IsZero())
			err := hooks.Validate()
			require.ErrorIs(t, err, ErrFinalizationHooksIncomplete)
			require.Contains(t, err.Error(), strings.TrimPrefix(name, "no "))
		})
	}
}
