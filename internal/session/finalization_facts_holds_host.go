package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// finalizationHostDriverHoldsFacts counts the durable external-driver
// marker as a physical hold. The hold definition: another live (alive or
// undecidable) host drives this session per the durable session_drivers
// marker, liveness attested by the non-blocking shared OS-file probe.
//
// The marker row is read INSIDE the passed seal transaction (the same
// sanctioned inline-SQL pattern as finalization_facts_queue.go): the row is
// durable state the seal transaction must observe exactly as it commits.
// Liveness, in contrast, lives outside the database -- the host lock is an
// OS file lock under the data dir -- so it is attested by ProbeHostShared,
// a non-acquiring OS probe outside the DB. No nested database work happens
// inside the seal transaction.
type finalizationHostDriverHoldsFacts struct {
	dataDir string
}

var _ FinalizationPhysicalHoldsFacts = finalizationHostDriverHoldsFacts{}

// NewFinalizationHostDriverHoldsFacts builds the host-driver holds facts
// reader for one data dir (the host lock files live under it).
func NewFinalizationHostDriverHoldsFacts(dataDir string) FinalizationPhysicalHoldsFacts {
	return finalizationHostDriverHoldsFacts{dataDir: dataDir}
}

// FinalizationPhysicalHoldsTx returns 1 when a foreign live host drives
// ownerSessionID per the session_drivers marker read inside tx, 0 when no
// row exists, the marker names this process's own host, or that host is
// provably dead. It fails closed -- wrapping
// ErrFinalizationPhysicalHoldsUnreadable -- when the row cannot be read or
// the probe cannot decide liveness: an undecidable probe must refuse,
// never seal. Exactly one row exists per session (session_id is its primary
// key), so one foreign live driver is one hold. An empty owner is zero
// facts, never an error; the owner's driver always holds its own marker
// while TrySeal runs, so counting the own host would deadlock every
// finalization.
func (f finalizationHostDriverHoldsFacts) FinalizationPhysicalHoldsTx(ctx context.Context, tx *sql.Tx, ownerSessionID string) (int, error) {
	if tx == nil {
		return 0, fmt.Errorf("finalization host driver holds facts require a transaction")
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("read finalization physical holds: %w", err)
	}
	if ownerSessionID == "" {
		return 0, nil
	}
	const query = `SELECT host_id FROM session_drivers WHERE session_id = ?`
	var hostID string
	err := tx.QueryRowContext(ctx, query, ownerSessionID).Scan(&hostID)
	if errors.Is(err, sql.ErrNoRows) {
		// No driver row: no host drives this session, so no hold.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read finalization host driver holds: %w", err)
	}
	if IsOwnHostID(hostID) {
		return 0, nil
	}
	status, probeErr := ProbeHostShared(f.dataDir, hostID)
	if probeErr != nil {
		return 0, fmt.Errorf("%w: host %s liveness probe: %v",
			ErrFinalizationPhysicalHoldsUnreadable, hostID, probeErr)
	}
	switch status {
	case HostStatusDead:
		return 0, nil
	case HostStatusAlive:
		return 1, nil
	default:
		return 0, fmt.Errorf("%w: host %s liveness is undecidable",
			ErrFinalizationPhysicalHoldsUnreadable, hostID)
	}
}
