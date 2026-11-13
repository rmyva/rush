//go:build windows

package session

// finalizationTrackedProcessHoldCount snapshots the Windows tracked-tree
// registry under its mutex. The lock is held for one map-length read; no
// I/O and no job handle is touched.
func finalizationTrackedProcessHoldCount() int {
	trackedJobsMu.Lock()
	defer trackedJobsMu.Unlock()
	return len(trackedJobs)
}
