package session

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/db/dbtest"
	"github.com/stretchr/testify/require"
)

// errFinalizationSeamStub marks the member failure the reader tests inject.
var errFinalizationSeamStub = errors.New("finalization seam stub failure")

// finalizationSeamScopeStub implements all four facts sources with fixed
// counters and one shared error, recording every transaction it saw.
type finalizationSeamScopeStub struct {
	tx                                                 *sql.Tx
	err                                                error
	descendants, queued, holds, debt                   int
	descendantCalls, queuedCalls, holdCalls, debtCalls int
}

func (s *finalizationSeamScopeStub) FinalizationActiveDescendantsTx(_ context.Context, tx *sql.Tx, _ string) (int, error) {
	s.descendantCalls++
	s.tx = tx
	return s.descendants, s.err
}

func (s *finalizationSeamScopeStub) FinalizationQueuedCallsTx(_ context.Context, tx *sql.Tx, _ string) (int, error) {
	s.queuedCalls++
	s.tx = tx
	return s.queued, s.err
}

func (s *finalizationSeamScopeStub) FinalizationPhysicalHoldsTx(_ context.Context, tx *sql.Tx, _ string) (int, error) {
	s.holdCalls++
	s.tx = tx
	return s.holds, s.err
}

func (s *finalizationSeamScopeStub) FinalizationReactionDebtTx(_ context.Context, tx *sql.Tx, _ string) (int, error) {
	s.debtCalls++
	s.tx = tx
	return s.debt, s.err
}

var (
	_ FinalizationDescendantsFacts   = (*finalizationSeamScopeStub)(nil)
	_ FinalizationQueuedCallsFacts   = (*finalizationSeamScopeStub)(nil)
	_ FinalizationPhysicalHoldsFacts = (*finalizationSeamScopeStub)(nil)
	_ FinalizationReactionDebtFacts  = (*finalizationSeamScopeStub)(nil)
)

// finalizationSeamEnv is the lane environment: a fully migrated temp database
// with the fencing schema reapplied, the "owner" session seeded, and the
// finalization store and service bound to that connection.
type finalizationSeamEnv struct {
	conn  *sql.DB
	ctx   context.Context
	store *AgentFinalizationStore
	svc   *service
}

// newFinalizationSeamEnv builds finalizationSeamEnv on batchStoreTestDB and
// reapplies dbtest.ApplyFinalizationFencingSchema so lanes never hand-write DDL.
func newFinalizationSeamEnv(t *testing.T) *finalizationSeamEnv {
	t.Helper()
	_, conn, ctx := batchStoreTestDB(t)
	require.NoError(t, dbtest.ApplyFinalizationFencingSchema(ctx, conn))
	conn.SetMaxOpenConns(1)
	return &finalizationSeamEnv{
		conn:  conn,
		ctx:   ctx,
		store: NewAgentFinalizationStore(conn),
		svc:   NewService(db.New(conn), conn).(*service),
	}
}

// finalizationSeamBindOwner binds an active finalization record for sessionID
// through the real BindAgentFinalization producer and returns the authority.
func finalizationSeamBindOwner(t *testing.T, env *finalizationSeamEnv, sessionID, logicalCallID string) *AgentFinalizationAuthority {
	t.Helper()
	authority, err := env.svc.BindAgentFinalization(env.ctx, AgentFinalizationIdentity{SessionID: sessionID, LogicalCallID: logicalCallID, Generation: 1})
	require.NoError(t, err)
	return authority
}

// finalizationSeamLiveChild creates a live child session under parentID via
// CreateTaskSession and binds its active record, returning the child session.
func finalizationSeamLiveChild(t *testing.T, env *finalizationSeamEnv, parentID, toolCallID string) Session {
	t.Helper()
	child, err := env.svc.CreateTaskSession(env.ctx, toolCallID, parentID, "child")
	require.NoError(t, err)
	_, err = env.svc.BindAgentFinalization(env.ctx, AgentFinalizationIdentity{SessionID: child.ID, LogicalCallID: toolCallID + "-call", Generation: 1})
	require.NoError(t, err)
	return child
}

// finalizationSeamQueuedCallFixture prepares the trusted ancestry fixture so
// lanes can enqueue real queued calls carrying continuations.
func finalizationSeamQueuedCallFixture(t *testing.T) finalizationQueueFixture {
	t.Helper()
	return newFinalizationQueueFixture(t)
}

// finalizationSeamEnqueueQueuedCall enqueues a trusted queued call under rowID,
// producing the run-queue entry and its unconsumed continuation row.
func finalizationSeamEnqueueQueuedCall(t *testing.T, fixture finalizationQueueFixture, rowID string) {
	t.Helper()
	fixture.enqueue(t, rowID)
}

// finalizationSeamInsertDebt records a wake session notice, the reaction debt
// producer lane L3 settles.
func finalizationSeamInsertDebt(t *testing.T, env *finalizationSeamEnv, sessionID string) {
	t.Helper()
	require.NoError(t, NewSessionNoticeStore(env.conn).InsertSessionNotice(env.ctx, sessionID, NoticeKindWakeOnly, "finalization-debt", true, ""))
}

// finalizationSeamRequestCancel cancels sessionID through the real producer.
func finalizationSeamRequestCancel(t *testing.T, env *finalizationSeamEnv, sessionID string) {
	t.Helper()
	require.NoError(t, env.svc.RequestCancel(env.ctx, sessionID))
}

// finalizationSeamSetPhase forces the owner's record phase directly in the
// database, the deterministic lever the polling-gate tests drive AwaitSealed with.
func finalizationSeamSetPhase(t *testing.T, env *finalizationSeamEnv, sessionID, phase string) {
	t.Helper()
	result, err := env.conn.ExecContext(env.ctx, `UPDATE session_agent_finalizations SET phase=? WHERE session_id=?`, phase, sessionID)
	require.NoError(t, err)
	changed, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, changed, "the phase lever must hit exactly the owner record")
}

func TestFinalizationScopeReaderFoldsCompleteFactsInTransaction(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	stub := &finalizationSeamScopeStub{descendants: 1, queued: 2, holds: 3, debt: 4}
	reader := NewAgentFinalizationScopeReader("owner", stub, stub, stub, stub)
	tx, err := env.conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	facts, err := reader.FinalizationSealScopeFacts(t.Context(), tx)
	require.NoError(t, err)
	require.True(t, facts.Complete)
	require.Equal(t, 1, facts.ActiveDescendants)
	require.Equal(t, 2, facts.QueuedCalls)
	require.Equal(t, 3, facts.PhysicalHolds)
	require.Equal(t, 4, facts.ReactionDebt)
	require.Same(t, tx, stub.tx, "every source must read inside the seal transaction")
}

func TestFinalizationScopeReaderNilMemberIsIncomplete(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	stub := &finalizationSeamScopeStub{descendants: 1, queued: 2, holds: 3, debt: 4}
	for name, reader := range map[string]AgentFinalizationScopeReader{
		"no descendants": NewAgentFinalizationScopeReader("owner", nil, stub, stub, stub),
		"no queued":      NewAgentFinalizationScopeReader("owner", stub, nil, stub, stub),
		"no holds":       NewAgentFinalizationScopeReader("owner", stub, stub, nil, stub),
		"no debt":        NewAgentFinalizationScopeReader("owner", stub, stub, stub, nil),
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := env.conn.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer tx.Rollback()
			facts, err := reader.FinalizationSealScopeFacts(t.Context(), tx)
			require.NoError(t, err)
			require.False(t, facts.Complete)
			require.Zero(t, stub.descendantCalls, "an incomplete reader must not call any source")
		})
	}
}

func TestFinalizationScopeReaderEmptyOwnerIsIncomplete(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	stub := &finalizationSeamScopeStub{descendants: 1}
	reader := NewAgentFinalizationScopeReader("", stub, stub, stub, stub)
	tx, err := env.conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	facts, err := reader.FinalizationSealScopeFacts(t.Context(), tx)
	require.NoError(t, err)
	require.False(t, facts.Complete)
	require.Zero(t, stub.descendantCalls)
}

func TestFinalizationScopeReaderMemberErrorPropagates(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	stub := &finalizationSeamScopeStub{err: errFinalizationSeamStub}
	reader := NewAgentFinalizationScopeReader("owner", stub, stub, stub, stub)
	tx, err := env.conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = reader.FinalizationSealScopeFacts(t.Context(), tx)
	require.ErrorIs(t, err, errFinalizationSeamStub)
	require.Equal(t, 1, stub.descendantCalls, "the first failing source stops the read")
	require.Zero(t, stub.queuedCalls, "later sources must not run after a member error")
}

func TestDefaultFinalizationTerminalGateAllowsEverything(t *testing.T) {
	gate := DefaultFinalizationTerminalGate()
	require.True(t, gate.AllowTerminal(t.Context(), "owner"))
	require.True(t, gate.AllowTerminal(t.Context(), ""))
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.NoError(t, gate.AwaitSealed(canceled, "owner"), "the inert default never waits")
}

func TestPollingFinalizationTerminalGateAllowsWithoutActiveRecord(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	gate := NewPollingFinalizationTerminalGate(env.store, time.Millisecond)
	require.True(t, gate.AllowTerminal(t.Context(), "owner"))
	require.True(t, gate.AllowTerminal(t.Context(), ""))
	require.NoError(t, gate.AwaitSealed(t.Context(), "owner"))
}

func TestPollingFinalizationTerminalGateRefusesActiveRecord(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	finalizationSeamBindOwner(t, env, "owner", "logical")
	gate := NewPollingFinalizationTerminalGate(env.store, time.Millisecond)
	require.False(t, gate.AllowTerminal(t.Context(), "owner"))
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, gate.AwaitSealed(canceled, "owner"), context.Canceled)
}

func TestPollingFinalizationTerminalGateTerminalPhasesAllowDelivery(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	finalizationSeamBindOwner(t, env, "owner", "logical")
	gate := NewPollingFinalizationTerminalGate(env.store, time.Millisecond)
	for _, phase := range []string{"accepted", "failed", "cancelled", "interrupted"} {
		finalizationSeamSetPhase(t, env, "owner", phase)
		require.True(t, gate.AllowTerminal(t.Context(), "owner"), phase)
	}
}

func TestPollingFinalizationTerminalGateAwaitSealedReturnsWhenRecordLeavesActive(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	finalizationSeamBindOwner(t, env, "owner", "logical")
	gate := NewPollingFinalizationTerminalGate(env.store, time.Millisecond)
	ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- gate.AwaitSealed(ctx, "owner") }()
	finalizationSeamSetPhase(t, env, "owner", "cancelled")
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("AwaitSealed did not observe the forced phase change")
	}
}

func TestPollingFinalizationTerminalGateAwaitSealedStopsOnCanceledContext(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	finalizationSeamBindOwner(t, env, "owner", "logical")
	gate := NewPollingFinalizationTerminalGate(env.store, time.Millisecond)
	ctx, cancel := context.WithCancel(env.ctx)
	cancel()
	require.ErrorIs(t, gate.AwaitSealed(ctx, "owner"), context.Canceled)
}
