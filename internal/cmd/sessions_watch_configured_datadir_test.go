package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSessionsWatchCmdRun_HonorsConfiguredDataDir is the regression test for
// task #233 finding 3: sessionsWatchCmdRun used to compute locksDir as
// filepath.Join(ResolveCwd(cmd), ".rush", "locks"), ignoring --data-dir /
// a configured data_directory, even though setupApp(cmd) had already
// resolved the correct value onto `a`.
//
// The distinguishing scenario: a session has EndedReason set (which alone
// makes isSessionFinishedFromState report "done" when the lock is NOT
// alive), but a FRESH, live-looking lock file is seeded ONLY at the
// configured (non-cwd) data dir. With the fix, sessionsWatchCmdRun looks up
// the lock at the configured location, sees it alive, and the "alive lock
// blocks EndedReason" rule (isSessionFinishedFromState's signal-blocking
// behavior) keeps the watch loop running past the first tick — so a
// near-immediate context cancel makes it exit via the "(interrupted —
// session still running)" path, NOT the "--- session ended ---" summary.
// Before the fix, the lookup would miss the lock entirely (wrong, cwd-based
// path), see lockAlive=false, and print the end summary on literally the
// first tick, before the cancel can even land — an observably different,
// wrong outcome this test pins against.
func TestSessionsWatchCmdRun_HonorsConfiguredDataDir(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	// Deliberately outside workDir entirely, so filepath.Join(cwd, ".rush")
	// (the pre-fix hardcoded guess) can never accidentally coincide with it.
	configuredDataDir := filepath.Join(tmp, "elsewhere-data")

	ensureRootFlagStandIns(sessionsWatchCmd, configuredDataDir)
	if f := sessionsWatchCmd.Flags().Lookup("cwd"); f == nil {
		sessionsWatchCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsWatchCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsWatchCmd.Flags().Set("interval", "50ms"))

	ctx, cancel := context.WithCancel(context.Background())
	sessionsWatchCmd.SetContext(ctx)

	built, err := setupApp(sessionsWatchCmd)
	require.NoError(t, err)
	require.Equal(t, configuredDataDir, built.Config().Options.DataDirectory,
		"test setup assumption: resolved DataDirectory must equal the --data-dir we configured")
	t.Cleanup(func() {
		dataDir := built.Config().Options.DataDirectory
		built.Shutdown()
		waitForSQLiteHandleRelease(t, dataDir)
	})

	dbCtx := context.Background()
	sess, err := built.Sessions.CreateWithID(dbCtx, "watch-configured-datadir-id", "regression title")
	require.NoError(t, err)
	require.NoError(t, built.Sessions.SetEndedReason(dbCtx, sess.ID, "max_cost"))

	// Live-looking lock ONLY at the configured (non-cwd) data dir.
	lockDir := filepath.Join(configuredDataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))
	now := time.Now()
	require.NoError(t, os.Chtimes(lockPath, now, now))

	// Sanity: the WRONG (pre-fix) path must not exist.
	wrongPath := filepath.Join(workDir, ".rush", "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	_, wrongStatErr := os.Stat(wrongPath)
	require.True(t, os.IsNotExist(wrongStatErr))

	// SessionsWatchCmd.RunE creates its own full App. Release the seed App's
	// process-wide MCP owner before invoking the command.
	built.Shutdown()

	// Let the configured watch interval run before cancelling. A generous
	// timeout avoids relying on setupApp/db startup completing within a tight
	// wall-clock window.
	time.AfterFunc(10*time.Second, cancel)

	stderr := captureStderr(t, func() {
		runErr := sessionsWatchCmd.RunE(sessionsWatchCmd, []string{sess.ID})
		require.NoError(t, runErr)
	})
	t.Logf("sessions watch stderr:\n%s", stderr)

	require.Contains(t, stderr, "(interrupted",
		"fix must see the lock as alive at the configured data dir and keep watching past the first tick, exiting only via the interrupt")
	require.NotContains(t, stderr, "--- session ended ---",
		"fix must not report the session as ended while its lock (at the configured data dir) is still alive")
}
