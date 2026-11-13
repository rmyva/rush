//go:build !windows

package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFinalizationTrackedProcessHoldCountTracksUnixGroups: the exported
// counter is a live snapshot of the trackedGroups registry -- registering
// one spawned group raises it by exactly one and untracking returns it to
// baseline.
//
// Revert-check: a counter that snapshots a copy at init time (or reads the
// map without the mutex) misses entries registered after init, so a live
// tracked group would not defer any owner's seal.
func TestFinalizationTrackedProcessHoldCountTracksUnixGroups(t *testing.T) {
	leader, _ := spawnGroupChild(t, true)
	pid := leader.Process.Pid
	t.Cleanup(func() { UntrackProcessTree(pid) })

	baseline := CountFinalizationTrackedProcessHolds()
	require.NoError(t, RegisterSpawnedGroup(pid))
	require.Equal(t, baseline+1, CountFinalizationTrackedProcessHolds(),
		"one registered group must raise the process hold count by one")

	UntrackProcessTree(pid)
	require.Equal(t, baseline, CountFinalizationTrackedProcessHolds(),
		"physical teardown returns the count to baseline")
}
