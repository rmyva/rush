package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Revert-check: a physical-holds reader that returns a silent (0, nil) — for
// any input shape — claims coverage it does not have and lets TrySeal seal on
// unobserved facts; it must fail closed instead.
func TestFinalizationPhysicalHoldsTxRefusesUnreadableSources(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	tx, err := env.conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	reader := NewFinalizationPhysicalHoldsFacts()
	count, err := reader.FinalizationPhysicalHoldsTx(t.Context(), tx, "owner")
	require.ErrorIs(t, err, ErrFinalizationPhysicalHoldsUnreadable)
	require.Zero(t, count)
	count, err = reader.FinalizationPhysicalHoldsTx(t.Context(), nil, "owner")
	require.ErrorIs(t, err, ErrFinalizationPhysicalHoldsUnreadable)
	require.Zero(t, count)
	count, err = reader.FinalizationPhysicalHoldsTx(t.Context(), tx, "")
	require.ErrorIs(t, err, ErrFinalizationPhysicalHoldsUnreadable)
	require.Zero(t, count)
}

// Revert-check: (a) a TrySeal that checks queued before holds, or that
// accepts a record with physical holds open, publishes while a physical hold
// is unobserved; (b) a TrySeal that swallows the production reader's
// fail-closed verdict, or that seals on it, treats "unreadable" as "clear".
func TestTrySealPhysicalHoldsFactRefusals(t *testing.T) {
	e := newCandidateEnv(t)
	_, presented := sealStoredCandidate(t, e, 0, 1)
	holdsOpen := &finalizationSeamScopeStub{holds: 1}
	reader := NewAgentFinalizationScopeReader("owner", &finalizationSeamScopeStub{}, NewFinalizationQueuedCallsFacts(), holdsOpen, &finalizationSeamScopeStub{})
	outcome, err := e.store.TrySeal(t.Context(), e.id, presented, e.sealWriter(t), reader)
	require.NoError(t, err)
	require.False(t, outcome.Sealed)
	require.False(t, outcome.Accepted)
	require.Equal(t, AgentFinalizationSealPhysicalHolds, outcome.Reason)

	broken := NewAgentFinalizationScopeReader("owner", &finalizationSeamScopeStub{}, NewFinalizationQueuedCallsFacts(), NewFinalizationPhysicalHoldsFacts(), &finalizationSeamScopeStub{})
	outcome, err = e.store.TrySeal(t.Context(), e.id, presented, e.sealWriter(t), broken)
	require.ErrorIs(t, err, ErrFinalizationPhysicalHoldsUnreadable)
	require.Equal(t, AgentFinalizationSealOutcome{}, outcome)
}
