package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// holdsProviderStub is a counting in-process hold source.
type holdsProviderStub struct {
	calls     int
	lastOwner string
	count     int
	err       error
}

func (s *holdsProviderStub) provider(ctx context.Context, ownerSessionID string) (int, error) {
	s.calls++
	s.lastOwner = ownerSessionID
	if s.err != nil {
		return 0, s.err
	}
	return s.count, nil
}

// Revert-check: a composite that returns a silent (0, nil) for an empty or
// incomplete provider set claims hold coverage it cannot observe and lets
// TrySeal seal with physical holds open.
func TestFinalizationHoldsProviderCompositeFailsClosedOnEmptySet(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	tx, err := env.conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()

	reader := NewFinalizationPhysicalHoldsFactsFromProviders()
	count, err := reader.FinalizationPhysicalHoldsTx(t.Context(), tx, "owner")
	require.ErrorIs(t, err, ErrFinalizationPhysicalHoldsUnreadable)
	require.Zero(t, count)
	count, err = reader.FinalizationPhysicalHoldsTx(t.Context(), nil, "owner")
	require.ErrorIs(t, err, ErrFinalizationPhysicalHoldsUnreadable)
	require.Zero(t, count)

	// A typed nil is still a member: pass via slice expansion so the nil
	// member is preserved by the variadic call.
	nilSet := []FinalizationHoldsProvider{nil}
	nilReader := NewFinalizationPhysicalHoldsFactsFromProviders(nilSet...)
	count, err = nilReader.FinalizationPhysicalHoldsTx(t.Context(), tx, "owner")
	require.ErrorIs(t, err, ErrFinalizationPhysicalHoldsUnreadable)
	require.Zero(t, count)
}

// Revert-check: a composite that drops or double-counts a member
// under-counts or over-counts holds — the sum must be exact, otherwise
// TrySeal seals too early or never.
func TestFinalizationHoldsProviderCompositeSumsProviders(t *testing.T) {
	first := &holdsProviderStub{count: 2}
	second := &holdsProviderStub{count: 3}
	reader := NewFinalizationPhysicalHoldsFactsFromProviders(first.provider, second.provider)

	count, err := reader.FinalizationPhysicalHoldsTx(t.Context(), nil, "owner")
	require.NoError(t, err)
	require.Equal(t, 5, count)
	require.Equal(t, "owner", first.lastOwner)

	// A second read of the same composite stays exact: each provider is
	// invoked once per read.
	count, err = reader.FinalizationPhysicalHoldsTx(t.Context(), nil, "owner")
	require.NoError(t, err)
	require.Equal(t, 5, count)
	require.Equal(t, 2, first.calls)
	require.Equal(t, 2, second.calls)
}

// Revert-check: a composite that swallows a provider error as (0, nil)
// treats "source unreadable" as "clear" and seals on unobserved facts.
func TestFinalizationHoldsProviderCompositePropagatesProviderError(t *testing.T) {
	first := &holdsProviderStub{err: errFinalizationSeamStub}
	second := &holdsProviderStub{count: 7}
	reader := NewFinalizationPhysicalHoldsFactsFromProviders(first.provider, second.provider)

	count, err := reader.FinalizationPhysicalHoldsTx(t.Context(), nil, "owner")
	require.ErrorIs(t, err, errFinalizationSeamStub)
	require.Zero(t, count)
	require.Zero(t, second.calls)
}

// Revert-check: a composite that clamps negative counts to zero trusts a
// corrupted source's silent under-report and could seal on a hidden hold.
func TestFinalizationHoldsProviderCompositeRejectsNegativeCount(t *testing.T) {
	negative := func(ctx context.Context, ownerSessionID string) (int, error) {
		return -1, nil
	}
	reader := NewFinalizationPhysicalHoldsFactsFromProviders(negative)

	count, err := reader.FinalizationPhysicalHoldsTx(t.Context(), nil, "owner")
	require.Error(t, err)
	require.Contains(t, err.Error(), "negative")
	require.Zero(t, count)
}

// Revert-check: a composite that forwards an empty owner to providers (or
// errors on it) lets sources count global, session-blind state against a
// nonexistent owner — the empty owner must deterministically mean zero
// owned holds.
func TestFinalizationHoldsProviderCompositeEmptyOwnerSkipsProviders(t *testing.T) {
	first := &holdsProviderStub{count: 1}
	second := &holdsProviderStub{count: 1}
	reader := NewFinalizationPhysicalHoldsFactsFromProviders(first.provider, second.provider)

	count, err := reader.FinalizationPhysicalHoldsTx(t.Context(), nil, "")
	require.NoError(t, err)
	require.Zero(t, count)
	require.Zero(t, first.calls)
	require.Zero(t, second.calls)
}

// Revert-check: a composite that ignores ctx runs provider reads inside a
// transaction whose owner already cancelled — wasted blocking work and
// misleading errors after cancellation. Note the ordering: the structural
// provider-set check (empty set, nil member) precedes the context check, so
// an empty set still fails closed with
// ErrFinalizationPhysicalHoldsUnreadable even under a cancelled context.
func TestFinalizationHoldsProviderCompositeRespectsCanceledContext(t *testing.T) {
	first := &holdsProviderStub{count: 1}
	second := &holdsProviderStub{count: 1}
	reader := NewFinalizationPhysicalHoldsFactsFromProviders(first.provider, second.provider)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	count, err := reader.FinalizationPhysicalHoldsTx(ctx, nil, "owner")
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, count)
	require.Zero(t, first.calls)
	require.Zero(t, second.calls)

	empty := NewFinalizationPhysicalHoldsFactsFromProviders()
	count, err = empty.FinalizationPhysicalHoldsTx(ctx, nil, "owner")
	require.ErrorIs(t, err, ErrFinalizationPhysicalHoldsUnreadable)
	require.Zero(t, count)
}

// Revert-check: (a) a TrySeal or composite that ignores the holds counter
// seals while a hold is open; (b) a composite that reports 0 when a provider
// reports 1 does the same; (c) a holds reader that can never return 0 would
// refuse forever — the 0-path proves the counter actually gates sealing, and
// proves that ignoring the tx does not break the seal decision.
func TestTrySealPhysicalHoldsProviderCompositeRefusalAndSeal(t *testing.T) {
	// Leg 1: refusal with one open in-process hold. TrySeal is committing,
	// so this leg uses its own env.
	e1 := newCandidateEnv(t)
	_, presented := sealStoredCandidate(t, e1, 0, 1)
	holdsOpen := NewFinalizationPhysicalHoldsFactsFromProviders((&holdsProviderStub{count: 1}).provider)
	reader := NewAgentFinalizationScopeReader("owner", &finalizationSeamScopeStub{}, NewFinalizationQueuedCallsFacts(), holdsOpen, &finalizationSeamScopeStub{})
	outcome, err := e1.store.TrySeal(t.Context(), e1.id, presented, e1.sealWriter(t), reader)
	require.NoError(t, err)
	require.False(t, outcome.Sealed)
	require.False(t, outcome.Accepted)
	require.Equal(t, AgentFinalizationSealPhysicalHolds, outcome.Reason)

	// Leg 2: seal with the provider reporting zero holds, in a fresh env.
	e2 := newCandidateEnv(t)
	_, presented = sealStoredCandidate(t, e2, 0, 1)
	holdsClear := NewFinalizationPhysicalHoldsFactsFromProviders((&holdsProviderStub{count: 0}).provider)
	reader = NewAgentFinalizationScopeReader("owner", &finalizationSeamScopeStub{}, NewFinalizationQueuedCallsFacts(), holdsClear, &finalizationSeamScopeStub{})
	outcome, err = e2.store.TrySeal(t.Context(), e2.id, presented, e2.sealWriter(t), reader)
	require.NoError(t, err)
	require.True(t, outcome.Sealed)
	require.False(t, outcome.Accepted)
	require.Equal(t, AgentFinalizationSealNone, outcome.Reason)
	require.Equal(t, "accepted", outcome.Record.Phase)
	require.True(t, outcome.Message.IsFinished())
}
