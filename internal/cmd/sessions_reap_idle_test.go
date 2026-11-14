package cmd

// Tests for the FIX-LOCKS idle-reclaim path of `sessions reap`: PID-less lock
// files older than --idle-older-than are removed with the same OS-lock probe
// and an os.SameFile identity check immediately before the unlink; younger,
// held, or "0"-disabled cases stay untouched, and --dry-run only lists.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reapIdleSetup isolates the config env, chdirs into a temp workDir, points
// the sessionsReapCmd data-dir flag stand-in at dataDir, and resets every
// flag the command reads (flags persist between tests in this package).
func reapIdleSetup(t *testing.T, dataDir string) {
	t.Helper()
	_ = isolateConfigEnvForTests(t)
	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	ensureRootFlagStandIns(sessionsReapCmd, dataDir)
	if f := sessionsReapCmd.Flags().Lookup("cwd"); f == nil {
		sessionsReapCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsReapCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsReapCmd.Flags().Set("dry-run", "false"))
	require.NoError(t, sessionsReapCmd.Flags().Set("all", "false"))
	require.NoError(t, sessionsReapCmd.Flags().Set("idle-older-than", "168h"))
	sessionsReapCmd.SetContext(t.Context())
}

func reapIdleWriteLock(t *testing.T, dataDir, id string) string {
	t.Helper()
	locksDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))
	path := filepath.Join(locksDir, "session-"+id+".lock")
	require.NoError(t, os.WriteFile(path, []byte(""), 0o644))
	return path
}

// TestSessionsReap_IdleOlderThanThresholdRemoved is the headline revert-check:
// reap must reclaim a PID-less lock older than the threshold (file gone,
// "reclaimed 1 lock(s)") while keeping a young released lock unprobed and
// untouched.
func TestSessionsReap_IdleOlderThanThresholdRemoved(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	reapIdleSetup(t, dataDir)

	ancient := reapIdleWriteLock(t, dataDir, "idle-ancient")
	ageLock(t, ancient, 8*24*time.Hour)
	freshReleased := reapIdleWriteLock(t, dataDir, "released-young")
	ageLock(t, freshReleased, 2*time.Minute)

	_, stderr := captureStdoutAndStderr(t, func() {
		require.NoError(t, sessionsReapCmd.RunE(sessionsReapCmd, nil))
	})

	require.Contains(t, stderr, "idle lock", "the ancient idle lock must be removed via the idle wording")
	require.Contains(t, stderr, "reclaimed 1 lock(s)")
	require.Contains(t, stderr, "kept   released lock", "the 2-minute released lock stays unprobed")
	_, err := os.Stat(ancient)
	require.True(t, os.IsNotExist(err), "idle lock over the threshold must be removed")
	require.FileExists(t, freshReleased, "young released lock must stay")
	assert.NotContains(t, stderr, "removed idle lock released-young")
}

// TestSessionsReap_IdleDryRunShowsWithoutTouching is the --dry-run
// revert-check: idle candidates are reported but neither probed nor touched —
// the mtime must be bit-identical after the run.
func TestSessionsReap_IdleDryRunShowsWithoutTouching(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	reapIdleSetup(t, dataDir)
	path := reapIdleWriteLock(t, dataDir, "idle-dry")
	ageLock(t, path, 8*24*time.Hour)
	before, err := os.Stat(path)
	require.NoError(t, err)

	require.NoError(t, sessionsReapCmd.Flags().Set("dry-run", "true"))
	_, stderr := captureStdoutAndStderr(t, func() {
		require.NoError(t, sessionsReapCmd.RunE(sessionsReapCmd, nil))
	})

	require.Contains(t, stderr, "would remove idle lock")
	require.Contains(t, stderr, "(dry-run; 1 idle lock(s) would be removed")
	assert.NotContains(t, stderr, "removed idle lock")

	require.FileExists(t, path)
	after, statErr := os.Stat(path)
	require.NoError(t, statErr)
	require.True(t, before.ModTime().Equal(after.ModTime()), "dry-run must not touch the idle lock, not even its mtime")
}

// TestSessionsReap_IdleBelowThresholdKept is the threshold guard: a PID-less
// lock younger than --idle-older-than is a plausible between-turns leftover
// and must be kept unprobed. Revert-check: dropping the age gate would probe
// (and could unlink) a path a live loop is about to reuse.
func TestSessionsReap_IdleBelowThresholdKept(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	reapIdleSetup(t, dataDir)
	path := reapIdleWriteLock(t, dataDir, "idle-young")
	ageLock(t, path, 6*24*time.Hour)

	_, stderr := captureStdoutAndStderr(t, func() {
		require.NoError(t, sessionsReapCmd.RunE(sessionsReapCmd, nil))
	})

	require.Contains(t, stderr, "kept   released lock")
	assert.NotContains(t, stderr, "idle lock")
	require.FileExists(t, path)
}

// TestSessionsReap_IdleOlderThanZeroDisables: --idle-older-than 0 must
// disable idle reclaim entirely — the ancient PID-less lock is treated as a
// plain released leftover.
func TestSessionsReap_IdleOlderThanZeroDisables(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	reapIdleSetup(t, dataDir)
	path := reapIdleWriteLock(t, dataDir, "idle-zero")
	ageLock(t, path, 8*24*time.Hour)
	require.NoError(t, sessionsReapCmd.Flags().Set("idle-older-than", "0"))
	t.Cleanup(func() { _ = sessionsReapCmd.Flags().Set("idle-older-than", "168h") })

	_, stderr := captureStdoutAndStderr(t, func() {
		require.NoError(t, sessionsReapCmd.RunE(sessionsReapCmd, nil))
	})

	require.Contains(t, stderr, "kept   released lock")
	require.FileExists(t, path)
}

// TestSessionsReap_IdleButOSLockHeldKept is the probe safety revert-check: an
// ancient PID-less file whose OS lock is actually held (a real between-turns
// wait) must be reported skip-alive, not removed.
func TestSessionsReap_IdleButOSLockHeldKept(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	reapIdleSetup(t, dataDir)

	probe, err := session.TryHoldSessionLockShared(dataDir, "idle-held")
	require.NoError(t, err)
	t.Cleanup(func() { _ = probe.Release() })

	path := filepath.Join(dataDir, "locks", "session-idle-held.lock")
	ageLock(t, path, 8*24*time.Hour) // Chtimes works under a range lock

	_, stderr := captureStdoutAndStderr(t, func() {
		require.NoError(t, sessionsReapCmd.RunE(sessionsReapCmd, nil))
	})

	require.Contains(t, stderr, "kept   idle lock")
	require.Contains(t, stderr, "OS lock held")
	require.FileExists(t, path)
}

// TestReapLockIdentityLost_Unit is the identity-discipline revert-check:
// reapLockIdentityLost must report "lost" only when the path stopped naming
// the baseline inode (replaced, or stat fails — fail closed).
func TestReapLockIdentityLost_Unit(t *testing.T) {
	t.Parallel() // pure file-stat logic, no stdout/chdir
	dir := t.TempDir()
	path := filepath.Join(dir, "session-ident.lock")
	require.NoError(t, os.WriteFile(path, []byte("a\n"), 0o644))
	baseline, err := os.Stat(path)
	require.NoError(t, err)

	require.False(t, reapLockIdentityLost(baseline, path), "same inode is not lost")

	// Move the baseline aside instead of removing it: a freed inode number is
	// reused immediately on Linux filesystems, so a plain remove-and-recreate
	// can hand the replacement the same inode and os.SameFile would call it
	// the same file. While the old file stays linked its inode cannot be reused.
	require.NoError(t, os.Rename(path, path+".old"))
	require.True(t, reapLockIdentityLost(baseline, path), "stat on a missing path fails closed: lost")
	require.NoError(t, os.WriteFile(path, []byte("b\n"), 0o644))
	require.True(t, reapLockIdentityLost(baseline, path), "recreated path is a new inode: lost")
}

// TestSessionsReap_IdentityReplacedAfterProbe_Skipped is the TOCTOU
// revert-check: if the lock path is unlinked and recreated while the probe
// lock is held, reap must NOT unlink the replacement (that would hand a new
// owner an unlinked inode) and must leave the replacement file intact.
// POSIX only: the swap needs unlink-while-open, impossible on Windows.
func TestSessionsReap_IdentityReplacedAfterProbe_Skipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX: the swap needs unlink-while-open, impossible on Windows")
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	reapIdleSetup(t, dataDir)

	locksDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))
	path := filepath.Join(locksDir, "session-crash-swap.lock")
	require.NoError(t, os.WriteFile(path, []byte("999999\n"), 0o644))
	ageLock(t, path, time.Minute)

	reapPreIdentitySeam = func(p string) {
		require.NoError(t, os.Remove(p))
		require.NoError(t, os.WriteFile(p, []byte("111111\n"), 0o644))
	}
	t.Cleanup(func() { reapPreIdentitySeam = nil })

	_, stderr := captureStdoutAndStderr(t, func() {
		require.NoError(t, sessionsReapCmd.RunE(sessionsReapCmd, nil))
	})

	require.Contains(t, stderr, "path no longer names the probed file")
	require.Contains(t, stderr, "kept")
	got, readErr := os.ReadFile(path)
	require.NoError(t, readErr, "the replacement file must survive untouched")
	require.Equal(t, "111111\n", string(got))
}
