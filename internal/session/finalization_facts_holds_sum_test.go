package session

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// holdsSumMemberStub is a fixed-count physical-holds facts member with a
// call counter, for the Sum combinator tests.
type holdsSumMemberStub struct {
	calls int
	count int
	err   error
}

func (s *holdsSumMemberStub) FinalizationPhysicalHoldsTx(_ context.Context, _ *sql.Tx, _ string) (int, error) {
	s.calls++
	return s.count, s.err
}

var _ FinalizationPhysicalHoldsFacts = (*holdsSumMemberStub)(nil)

// TestFinalizationHoldsFactsSumAddsMembers: the sum is exact, both members
// are invoked exactly once per read.
//
// Revert-check: a Sum that drops or double-counts a member under-counts or
// over-counts holds, so TrySeal seals too early or never.
func TestFinalizationHoldsFactsSumAddsMembers(t *testing.T) {
	a := &holdsSumMemberStub{count: 2}
	b := &holdsSumMemberStub{count: 3}
	sum := NewFinalizationPhysicalHoldsFactsSum(a, b)
	n, err := sum.FinalizationPhysicalHoldsTx(t.Context(), nil, "owner")
	require.NoError(t, err)
	require.Equal(t, 5, n)
	require.Equal(t, 1, a.calls)
	require.Equal(t, 1, b.calls)
}

// TestFinalizationHoldsFactsSumNilMemberFailsClosed: a nil member (passed
// explicitly as a nil interface value in a typed variable) fails closed
// wrapping ErrFinalizationPhysicalHoldsUnreadable, naming the member, and
// never invokes the healthy member.
//
// Revert-check: a Sum that treats a nil member as a zero-count source
// claims hold coverage it cannot observe and seals with holds open.
func TestFinalizationHoldsFactsSumNilMemberFailsClosed(t *testing.T) {
	var nilMember FinalizationPhysicalHoldsFacts
	good := &holdsSumMemberStub{count: 1}

	n, err := NewFinalizationPhysicalHoldsFactsSum(nilMember, good).
		FinalizationPhysicalHoldsTx(t.Context(), nil, "owner")
	require.ErrorIs(t, err, ErrFinalizationPhysicalHoldsUnreadable)
	require.Contains(t, err.Error(), "member a")
	require.Zero(t, n)
	require.Zero(t, good.calls)

	n, err = NewFinalizationPhysicalHoldsFactsSum(good, nilMember).
		FinalizationPhysicalHoldsTx(t.Context(), nil, "owner")
	require.ErrorIs(t, err, ErrFinalizationPhysicalHoldsUnreadable)
	require.Contains(t, err.Error(), "member b")
	require.Zero(t, n)
	require.Zero(t, good.calls)
}

// TestFinalizationHoldsFactsSumFirstMemberErrorSkipsSecond: a member error
// stops the read; member b is not invoked.
//
// Revert-check: a Sum that continues after a member failure could return a
// partial sum as a complete verdict.
func TestFinalizationHoldsFactsSumFirstMemberErrorSkipsSecond(t *testing.T) {
	a := &holdsSumMemberStub{err: errFinalizationSeamStub}
	b := &holdsSumMemberStub{count: 7}
	sum := NewFinalizationPhysicalHoldsFactsSum(a, b)
	n, err := sum.FinalizationPhysicalHoldsTx(t.Context(), nil, "owner")
	require.ErrorIs(t, err, errFinalizationSeamStub)
	require.Zero(t, n)
	require.Zero(t, b.calls)
}

// TestFinalizationHoldsFactsSumSecondMemberErrorPropagates: member b's
// error is propagated after member a succeeded.
//
// Revert-check: a Sum that swallows the second member's error as a partial
// sum seals on half-observed facts.
func TestFinalizationHoldsFactsSumSecondMemberErrorPropagates(t *testing.T) {
	a := &holdsSumMemberStub{count: 7}
	b := &holdsSumMemberStub{err: errFinalizationSeamStub}
	sum := NewFinalizationPhysicalHoldsFactsSum(a, b)
	n, err := sum.FinalizationPhysicalHoldsTx(t.Context(), nil, "owner")
	require.ErrorIs(t, err, errFinalizationSeamStub)
	require.Zero(t, n)
	require.Equal(t, 1, a.calls)
}

// TestFinalizationHoldsFactsSumEmptyOwnerSkipsMembers: an empty owner is
// deterministically (0, nil) and neither member is invoked.
//
// Revert-check: a Sum that forwards an empty owner to its members lets
// them count global, session-blind state against a nonexistent owner.
func TestFinalizationHoldsFactsSumEmptyOwnerSkipsMembers(t *testing.T) {
	a := &holdsSumMemberStub{count: 1}
	b := &holdsSumMemberStub{count: 1}
	sum := NewFinalizationPhysicalHoldsFactsSum(a, b)
	n, err := sum.FinalizationPhysicalHoldsTx(t.Context(), nil, "")
	require.NoError(t, err)
	require.Zero(t, n)
	require.Zero(t, a.calls)
	require.Zero(t, b.calls)
}

// TestFinalizationHoldsFactsSumRejectsNegativeMemberCount: a negative
// member count is refused, never summed or clamped.
//
// Revert-check: a Sum that clamps negatives to zero trusts a corrupted
// source's silent under-report and could seal on a hidden hold.
func TestFinalizationHoldsFactsSumRejectsNegativeMemberCount(t *testing.T) {
	a := &holdsSumMemberStub{count: 1}
	b := &holdsSumMemberStub{count: -1}
	sum := NewFinalizationPhysicalHoldsFactsSum(a, b)
	n, err := sum.FinalizationPhysicalHoldsTx(t.Context(), nil, "owner")
	require.Error(t, err)
	require.Contains(t, err.Error(), "negative hold count")
	require.Zero(t, n)
}
