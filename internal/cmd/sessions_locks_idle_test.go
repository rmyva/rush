package cmd

// Tests for the FIX-LOCKS default collapse of idle lock files in
// `sessions locks`: PID-less, stale, non-between-turns, non-stalled entries
// (what a clean Release leaves behind) are hidden behind a one-line summary
// unless --all or an explicit session-id argument is given, and --json ends
// with a {"summary":{...}} envelope line.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// locksIdleFixture seeds dataDir/locks with three idle locks (empty content,
// no .pid sidecar, mtime backdated 1h) and one crash-trace lock (PID
// "999999", backdated 1h), and returns the locks dir.
func locksIdleFixture(t *testing.T, dataDir string) string {
	t.Helper()
	locksDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))
	for _, id := range []string{"idle-a", "idle-b", "idle-c"} {
		path := filepath.Join(locksDir, "session-"+id+".lock")
		require.NoError(t, os.WriteFile(path, []byte(""), 0o644))
		ageLock(t, path, time.Hour)
	}
	crashPath := filepath.Join(locksDir, "session-crashy.lock")
	require.NoError(t, os.WriteFile(crashPath, []byte("999999\n"), 0o644))
	ageLock(t, crashPath, time.Hour)
	return locksDir
}

// locksIdleSetup isolates the config env, chdirs into a temp workDir, points
// the sessionsLocksCmd data-dir flag stand-in at dataDir, and resets every
// flag the command reads (flags persist between tests in this package).
func locksIdleSetup(t *testing.T, dataDir string) {
	t.Helper()
	_ = isolateConfigEnvForTests(t)
	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	ensureRootFlagStandIns(sessionsLocksCmd, dataDir)
	if f := sessionsLocksCmd.Flags().Lookup("cwd"); f == nil {
		sessionsLocksCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsLocksCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsLocksCmd.Flags().Set("json", "false"))
	require.NoError(t, sessionsLocksCmd.Flags().Set("stale-only", "false"))
	require.NoError(t, sessionsLocksCmd.Flags().Set("prune", "false"))
	require.NoError(t, sessionsLocksCmd.Flags().Set("all", "false"))
	sessionsLocksCmd.SetContext(t.Context())
}

// TestLockIsIdle_Classification unit-tests the pure collapse predicate: a
// settled no-holder leftover is PID-less, stale, not between turns, not
// stalled. Revert-check: if the classification widens (e.g. fresh PID-less
// files, between-turns or stalled rows) these cases must flip.
func TestLockIsIdle_Classification(t *testing.T) {
	t.Parallel() // pure function, no stdout/chdir
	require.True(t, lockIsIdle(0, true, false, false), "PID-less stale entry is idle")
	require.False(t, lockIsIdle(5, true, false, false), "recorded holder PID is not idle")
	require.False(t, lockIsIdle(0, true, true, false), "between turns is live evidence, not idle")
	require.False(t, lockIsIdle(0, true, false, true), "stalled is not idle")
	require.False(t, lockIsIdle(0, false, false, false), "fresh pulse is not idle")
}

// TestSessionsLocks_IdleFilesCollapsedByDefault is the headline revert-check:
// default `sessions locks` must hide idle lock files behind the summary line
// while keeping crash-trace rows visible. If the collapse is removed, the
// idle stems reappear and the summary line vanishes, failing this test.
func TestSessionsLocks_IdleFilesCollapsedByDefault(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	locksIdleSetup(t, dataDir)
	locksDir := locksIdleFixture(t, dataDir)

	stdout := captureStdout(t, func() {
		require.NoError(t, sessionsLocksCmd.RunE(sessionsLocksCmd, nil))
	})

	require.Contains(t, stdout, "999999", "crash-trace lock must stay visible")
	require.Contains(t, stdout, "3 idle lock files (no holder, no PID) hidden — use --all to list")
	assert.NotContains(t, stdout, "idle-a")
	assert.NotContains(t, stdout, "idle-b")
	assert.NotContains(t, stdout, "idle-c")

	// Read-only command: the idle files must still be on disk.
	for _, id := range []string{"idle-a", "idle-b", "idle-c"} {
		require.FileExists(t, filepath.Join(locksDir, "session-"+id+".lock"))
	}
}

// TestSessionsLocks_AllFlagShowsIdleFiles is the --all revert-check: opting
// in must list every idle stem and suppress the hidden summary line.
func TestSessionsLocks_AllFlagShowsIdleFiles(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	locksIdleSetup(t, dataDir)
	locksIdleFixture(t, dataDir)
	require.NoError(t, sessionsLocksCmd.Flags().Set("all", "true"))
	t.Cleanup(func() { _ = sessionsLocksCmd.Flags().Set("all", "false") })

	stdout := captureStdout(t, func() {
		require.NoError(t, sessionsLocksCmd.RunE(sessionsLocksCmd, nil))
	})

	for _, stem := range []string{"idle-a", "idle-b", "idle-c", "crashy"} {
		require.Contains(t, stdout, stem, "--all must list %s", stem)
	}
	assert.NotContains(t, stdout, "hidden")
}

// TestSessionsLocks_JSONSummaryLine is the --json contract revert-check: the
// NDJSON stream must end with a summary envelope carrying the collapse
// counts, and no emitted row may be an idle stem.
func TestSessionsLocks_JSONSummaryLine(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	locksIdleSetup(t, dataDir)
	locksIdleFixture(t, dataDir)
	require.NoError(t, sessionsLocksCmd.Flags().Set("json", "true"))

	stdout := captureStdout(t, func() {
		require.NoError(t, sessionsLocksCmd.RunE(sessionsLocksCmd, nil))
	})

	var summary struct {
		Total      int `json:"total"`
		Shown      int `json:"shown"`
		HiddenIdle int `json:"hidden_idle"`
	}
	foundSummary := false
	dec := json.NewDecoder(strings.NewReader(stdout))
	for dec.More() {
		var row map[string]json.RawMessage
		require.NoError(t, dec.Decode(&row))
		if raw, ok := row["summary"]; ok {
			require.NoError(t, json.Unmarshal(raw, &summary))
			foundSummary = true
			continue
		}
		if sid, ok := row["session_id"]; ok {
			assert.NotContains(t, string(sid), "idle-", "no idle row may be emitted in default --json view")
		}
	}
	require.True(t, foundSummary, "NDJSON stream must end with a summary line")
	require.Equal(t, 4, summary.Total, "total = shown + hidden idle")
	require.Equal(t, 1, summary.Shown, "only the crash-trace row is shown")
	require.Equal(t, 3, summary.HiddenIdle, "three idle files collapsed")
}

// TestSessionsLocks_FreshPIDLessLockShown is the false-positive guard: a
// PID-less lock with a FRESH mtime has a live pulse and must NOT be
// collapsed. Revert-check: a regression classifying fresh locks as idle
// hides an active session's lock.
func TestSessionsLocks_FreshPIDLessLockShown(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	locksIdleSetup(t, dataDir)
	locksDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))
	path := filepath.Join(locksDir, "session-fresh-idle.lock")
	require.NoError(t, os.WriteFile(path, []byte(""), 0o644)) // fresh mtime, no backdating

	stdout := captureStdout(t, func() {
		require.NoError(t, sessionsLocksCmd.RunE(sessionsLocksCmd, nil))
	})

	require.Contains(t, stdout, "fresh-idle")
	assert.NotContains(t, stdout, "hidden")
}

// TestSessionsLocks_ExplicitArgShowsIdleLock is the explicit-argument
// revert-check: `sessions locks <id>` bypasses the collapse so an operator
// inspecting one session still sees its idle lock.
func TestSessionsLocks_ExplicitArgShowsIdleLock(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	locksIdleSetup(t, dataDir)
	locksDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))
	path := filepath.Join(locksDir, "session-idle-explicit.lock")
	require.NoError(t, os.WriteFile(path, []byte(""), 0o644))
	ageLock(t, path, time.Hour)

	stdout := captureStdout(t, func() {
		require.NoError(t, sessionsLocksCmd.RunE(sessionsLocksCmd, []string{"idle-explicit"}))
	})

	require.Contains(t, stdout, "idle-explicit")
	assert.NotContains(t, stdout, "hidden")
}
