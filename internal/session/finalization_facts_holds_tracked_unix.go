//go:build !windows

package session

// finalizationTrackedProcessHoldCount snapshots the Unix tracked-group
// registry under its mutex. The lock is held for one map-length read; no
// I/O and no signal is sent.
func finalizationTrackedProcessHoldCount() int {
	trackedGroupsMu.Lock()
	defer trackedGroupsMu.Unlock()
	return len(trackedGroups)
}
