//go:build windows

package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFinalizationTrackedProcessHoldCountTracksWindowsJobs: the exported
// counter is a live snapshot of the trackedJobs registry -- planting one
// entry raises it by exactly one and untracking returns it to baseline.
//
// Revert-check: a counter that snapshots a copy at init time (or reads the
// map without the mutex) misses entries planted after init, so a live
// tracked tree would not defer any owner's seal.
func TestFinalizationTrackedProcessHoldCountTracksWindowsJobs(t *testing.T) {
	pid := spawnLiveVictim(t)
	baseline := CountFinalizationTrackedProcessHolds()

	plantStaleTrackedEntry(t, pid)
	require.Equal(t, baseline+1, CountFinalizationTrackedProcessHolds(),
		"one planted tracked entry must raise the process hold count by one")

	UntrackProcessTree(pid)
	require.Equal(t, baseline, CountFinalizationTrackedProcessHolds(),
		"physical teardown returns the count to baseline")
}
