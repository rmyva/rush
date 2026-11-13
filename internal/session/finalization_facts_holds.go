package session

import (
	"context"
	"database/sql"
	"errors"
)

// ErrFinalizationPhysicalHoldsUnreadable is the fail-closed verdict of
// FinalizationPhysicalHoldsTx: every plan-named hold source keeps its state
// outside the database, so the seal transaction cannot observe any of them.
var ErrFinalizationPhysicalHoldsUnreadable = errors.New(
	"finalization physical holds unreadable inside the seal transaction: " +
		"host locks are OS file locks under the data dir, tracked process trees " +
		"live in process memory, batch root leases live in the in-process capability manager")

// finalizationPhysicalHoldsFacts is the production physical-holds facts
// reader. The lane investigated all three plan-named physical-hold sources
// and NONE stores its state in the database, so a reader living inside the
// seal's SQL transaction cannot observe any of them; it therefore fails
// closed and NEVER returns a silent (0, nil) that would claim coverage.
type finalizationPhysicalHoldsFacts struct{}

var _ FinalizationPhysicalHoldsFacts = finalizationPhysicalHoldsFacts{}

// NewFinalizationPhysicalHoldsFacts builds the physical-holds facts reader.
//
// Per-source evidence:
//
//   - Host locks (internal/session/host_store.go, host_lock.go): the hold is
//     the exclusive OS file lock on <dataDir>/hosts/<host_id>.lock; the
//     async_hosts DB row is explicitly display-only bookkeeping (see
//     host_lock.go's file doc comment), and the session_drivers marker's
//     liveness verdict requires the same OS file probe, so no row in this
//     database attests the lock. Counting any DB row instead would both lie
//     about liveness and deadlock the owner's own driver, which holds its
//     own marker.
//
//   - Tracked process trees (internal/session/track_windows.go
//     TrackProcessTree, track_unix.go): in-process maps of handles/process
//     groups guarded by a mutex (trackedJobs / trackedGroups).
//
//   - Batch root leases (internal/agent/batch_root_lease.go
//     newBatchRootLeaseStore via internal/agent/batch_install_adapters.go):
//     an in-process map plus the in-memory diskCapabilityManager owner sets
//     (internal/agent/disk_provider_capability.go); the durable projection
//     of live roots is batch_nodes, which belongs to the descendants facts
//     reader, not here.
//
// Returning ErrFinalizationPhysicalHoldsUnreadable makes the composite
// reader surface "the reader is broken" to TrySeal, which returns it to the
// caller — the owner waits instead of sealing on unobserved facts. Widening
// coverage to a DB-visible hold registry is an operator decision, never a
// silent zero.
func NewFinalizationPhysicalHoldsFacts() FinalizationPhysicalHoldsFacts {
	return finalizationPhysicalHoldsFacts{}
}

// FinalizationPhysicalHoldsTx always fails closed: every hold source lives
// outside the database, so no transaction can count them.
func (finalizationPhysicalHoldsFacts) FinalizationPhysicalHoldsTx(context.Context, *sql.Tx, string) (int, error) {
	return 0, ErrFinalizationPhysicalHoldsUnreadable
}
