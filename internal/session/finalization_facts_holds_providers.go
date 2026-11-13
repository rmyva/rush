package session

import (
	"context"
	"database/sql"
	"fmt"
)

// FinalizationHoldsProvider is one in-process physical-hold source: it counts
// the holds owned by ownerSessionID WITHOUT the seal transaction. Providers
// must never hold a registry lock longer than one read and must never do
// long-blocking I/O: TrySeal invokes the composite inside its writer
// transaction. An OS file probe, if a source ever needs one, must be
// non-blocking TryLock-style.
type FinalizationHoldsProvider func(ctx context.Context, ownerSessionID string) (int, error)

// providerFinalizationPhysicalHoldsFacts is the composite physical-holds
// facts reader that sums in-process hold providers per owner instead of
// reading the database.
type providerFinalizationPhysicalHoldsFacts struct {
	providers []FinalizationHoldsProvider
}

var _ FinalizationPhysicalHoldsFacts = providerFinalizationPhysicalHoldsFacts{}

// NewFinalizationPhysicalHoldsFactsFromProviders builds a composite
// physical-holds facts reader that sums the in-process providers per owner.
//
// The composite deliberately IGNORES the *sql.Tx: every hold source keeps
// state outside the database (see finalization_facts_holds.go per-source
// evidence), so no row readable through the tx attests the holds. Accepting
// the tx only satisfies the seam; leaving it unused keeps any provider from
// answering with DB rows that would lie about liveness.
//
// An empty provider set or a nil member fails closed with
// ErrFinalizationPhysicalHoldsUnreadable, never a silent 0. Cross-process
// holds are not observable in-process and stay covered by the durable
// descendants facts reader.
func NewFinalizationPhysicalHoldsFactsFromProviders(providers ...FinalizationHoldsProvider) FinalizationPhysicalHoldsFacts {
	return providerFinalizationPhysicalHoldsFacts{providers: providers}
}

// FinalizationPhysicalHoldsTx sums the in-process providers for
// ownerSessionID, ignoring the seal transaction, failing closed on an empty
// or incomplete provider set, a cancelled context, provider errors, or a
// negative count.
func (f providerFinalizationPhysicalHoldsFacts) FinalizationPhysicalHoldsTx(ctx context.Context, _ *sql.Tx, ownerSessionID string) (int, error) {
	if len(f.providers) == 0 {
		return 0, ErrFinalizationPhysicalHoldsUnreadable
	}
	for i, p := range f.providers {
		if p == nil {
			return 0, fmt.Errorf("%w: holds provider %d is nil", ErrFinalizationPhysicalHoldsUnreadable, i)
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("read finalization physical holds: %w", err)
	}
	if ownerSessionID == "" {
		// Empty owner deterministically means zero owned holds; the
		// providers are not invoked.
		return 0, nil
	}
	sum := 0
	for i, p := range f.providers {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("read finalization physical holds: %w", err)
		}
		n, err := p(ctx, ownerSessionID)
		if err != nil {
			return 0, fmt.Errorf("finalization holds provider %d: %w", i, err)
		}
		if n < 0 {
			return 0, fmt.Errorf("finalization holds provider %d returned a negative hold count %d", i, n)
		}
		sum += n
	}
	return sum, nil
}
