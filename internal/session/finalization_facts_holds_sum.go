package session

import (
	"context"
	"database/sql"
	"fmt"
)

// sumFinalizationPhysicalHoldsFacts folds exactly two physical-holds facts
// readers into one: the in-process providers composite and, from lane L2c,
// the host-driver marker reader. The sum is the seam's production holds
// reader.
type sumFinalizationPhysicalHoldsFacts struct {
	a, b FinalizationPhysicalHoldsFacts
}

var _ FinalizationPhysicalHoldsFacts = sumFinalizationPhysicalHoldsFacts{}

// NewFinalizationPhysicalHoldsFactsSum builds the two-member composite. It
// performs no nil check at construction: a nil member fails at read time,
// wrapping ErrFinalizationPhysicalHoldsUnreadable, never as a silent 0.
func NewFinalizationPhysicalHoldsFactsSum(a, b FinalizationPhysicalHoldsFacts) FinalizationPhysicalHoldsFacts {
	return sumFinalizationPhysicalHoldsFacts{a: a, b: b}
}

// FinalizationPhysicalHoldsTx sums the two members for ownerSessionID. An
// incomplete composite (a nil member) fails closed before any read; a
// cancelled context, a member error, or a negative member count is
// returned as an error. An empty owner is zero facts and the members are
// not invoked. Member errors name the failing member and stop the read:
// member b is not invoked after member a failed.
func (f sumFinalizationPhysicalHoldsFacts) FinalizationPhysicalHoldsTx(ctx context.Context, tx *sql.Tx, ownerSessionID string) (int, error) {
	if f.a == nil {
		return 0, fmt.Errorf("%w: finalization physical holds facts member a is nil",
			ErrFinalizationPhysicalHoldsUnreadable)
	}
	if f.b == nil {
		return 0, fmt.Errorf("%w: finalization physical holds facts member b is nil",
			ErrFinalizationPhysicalHoldsUnreadable)
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("read finalization physical holds: %w", err)
	}
	if ownerSessionID == "" {
		// Empty owner deterministically means zero owned holds; the
		// members are not invoked.
		return 0, nil
	}
	na, err := f.a.FinalizationPhysicalHoldsTx(ctx, tx, ownerSessionID)
	if err != nil {
		return 0, fmt.Errorf("finalization physical holds facts member a: %w", err)
	}
	nb, err := f.b.FinalizationPhysicalHoldsTx(ctx, tx, ownerSessionID)
	if err != nil {
		return 0, fmt.Errorf("finalization physical holds facts member b: %w", err)
	}
	if na < 0 {
		return 0, fmt.Errorf("finalization physical holds facts member a returned a negative hold count %d", na)
	}
	if nb < 0 {
		return 0, fmt.Errorf("finalization physical holds facts member b returned a negative hold count %d", nb)
	}
	return na + nb, nil
}
