package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadLockPID_FromCLI covers the parser used by sessions kill /
// sessions reset --force. The original cmd-local readPIDFromLock did a
// naive strconv.Atoi(TrimSpace(file)) and would return 0 for the
// multi-line lock files that TryAcquireSessionLockWithTimeout writes
// (PID on line 1, timeout-in-seconds on line 2). That bug made
// `rush sessions kill` silently skip the kill step on any session
// started with `rush run --timeout ...`. Regression coverage stays in
// this file even though the parser now lives in the session package.
func TestReadLockPID_FromCLI(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
		want    int
	}{
		{"plain pid", "12345", 12345},
		{"pid with newline", "12345\n", 12345},
		{"pid with crlf", "12345\r\n", 12345},
		{"pid with surrounding whitespace", "  12345  ", 12345},
		{"pid plus timeout line", "12345\n900\n", 12345},
		{"pid plus timeout no trailing nl", "12345\n900", 12345},
		{"empty file", "", 0},
		{"whitespace only", "   \n", 0},
		{"non-numeric", "not-a-pid", 0},
		{"pid plus garbage same line", "12345 extra", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".lock")
			require.NoError(t, os.WriteFile(path, []byte(c.content), 0o644))
			assert.Equal(t, c.want, session.ReadLockPID(path))
		})
	}
}

func TestReadLockPID_FileMissing(t *testing.T) {
	assert.Equal(t, 0, session.ReadLockPID(filepath.Join(t.TempDir(), "nope.lock")))
}

func TestSanitiseSessionIDForFilename(t *testing.T) {
	cases := map[string]string{
		"simple-id":        "simple-id",
		"with/slash":       "with_slash",
		"with\\backslash":  "with_backslash",
		"with space":       "with_space",
		"a:b*c?d\"e<f>g|h": "a_b_c_d_e_f_g_h",
	}
	for in, want := range cases {
		assert.Equal(t, want, sanitiseSessionIDForFilename(in), "input=%q", in)
	}
}

func TestRemoveLockWithRetry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.lock")
	require.NoError(t, os.WriteFile(path, []byte("1\n"), 0o644))
	require.NoError(t, removeLockWithRetry(path, 0))
	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err))
	// Removing a missing file is success.
	assert.NoError(t, removeLockWithRetry(filepath.Join(dir, "missing.lock"), 0))
}

// TestForceKillHolder_InvalidPID pins the fail-closed contract for the
// "live holder, unusable PID" case.
//
// forceKillHolder is reached ONLY from the busy branch of
// probeThenKillHolder / acquireSessionLockForReset — after the OS has just
// proven a live holder exists. A pid<=0 there means "a holder exists and we
// cannot identify it", NOT "nobody holds the lock". An earlier draft asserted
// ConfirmedDead==true here with the rationale "pid<=0: no live holder", which
// pinned a false claim as a contract: ConfirmedDead is the single field every
// destructive follow-up is gated on, so a spurious true is precisely how a
// live holder's lock file gets unlinked and two owners appear.
func TestForceKillHolder_InvalidPID(t *testing.T) {
	kr := forceKillHolder("", "", 0, time.Second)
	assert.Contains(t, kr.Report, "no usable PID")
	assert.False(t, kr.ConfirmedDead,
		"a live holder was proven to exist by the caller's probe; an unreadable PID cannot upgrade that to 'confirmed dead'")
	assert.Equal(t, holderUnidentified, kr.State)
	kr = forceKillHolder("", "", -5, time.Second)
	assert.Contains(t, kr.Report, "no usable PID")
	assert.False(t, kr.ConfirmedDead)
}

func TestForceKillHolder_AlreadyDead(t *testing.T) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = platform.Command(context.Background(), "cmd.exe", "/c", "exit", "0")
	default:
		cmd = platform.Command(context.Background(), "true")
	}
	require.NoError(t, cmd.Run())
	kr := forceKillHolder("", "", cmd.Process.Pid, time.Second)
	assert.Contains(t, kr.Report, "already gone")
	assert.True(t, kr.ConfirmedDead)
	assert.Equal(t, holderAlreadyDead, kr.State)
}

// TestProbeThenKillHolder_StalePIDNotKilled is the regression test for the
// stale/reused-PID bug: a lock's original holder exited cleanly (Release()
// ran, so the OS lock is gone and, since the fix in session.Release, the
// PID metadata is cleared too) but we simulate an OLDER lock file — one
// that still carries a leftover PID on disk (predating the metadata-wipe
// fix, or written directly as tests sometimes do) — pointing at THIS TEST
// PROCESS's own PID (os.Getpid()), a real, currently-alive process that
// must never be touched by this test. Before the fix, sessionsKillCmdRun
// unconditionally called forceKillHolder on whatever PID the file
// contained; the fix must instead probe the real OS lock first, see that
// nobody holds it, and refuse to kill anything.
func TestProbeThenKillHolder_StalePIDNotKilled(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, ".rush")

	// Simulate a holder that finished (no live OS lock on this session id)
	// but whose lock file still names a PID — specifically our own PID, so
	// if the fix regresses and forceKillHolder is invoked on it, the test
	// process itself would be killed, which is an unmistakable failure. The
	// file is written directly: acquiring and releasing the lock to create it
	// would leave a background metadata-cleanup goroutine racing this write.
	lockDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-stale-pid-id.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))

	kr := probeThenKillHolder(dataDir, "stale-pid-id", os.Getpid(), time.Second)
	assert.Contains(t, kr.Report, "stale")
	assert.NotContains(t, kr.Report, "killed PID")
	assert.True(t, kr.ConfirmedDead, "no live OS lock -> holder confirmed dead (nothing to kill)")
	assert.Equal(t, holderAlreadyDead, kr.State)
	assert.True(t, session.IsProcessAlive(os.Getpid()), "probe must never kill the calling test process")
}

// TestProbeThenKillHolder_LiveHolderStillKilled is the "didn't break the
// happy path" companion: a real second process holds the OS lock (via the
// same cross-process helper the session package's own lock tests use), so
// the probe must report contention and forceKillHolder must still run and
// actually terminate it — exactly like before this change.
func TestProbeThenKillHolder_LiveHolderStillKilled(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, ".rush")

	// reapInBackground=true: this test measures whether forceKillHolder's
	// SIGKILL-then-poll observes death within its wait budget, which needs
	// a concurrent reaper so the victim never sits as an unreaped zombie
	// (see spawnKillTestLockHolder's doc comment).
	holder := spawnKillTestLockHolder(t, dataDir, "live-holder-id", true)
	defer holder.stop(t)

	require.True(t, session.IsProcessAlive(holder.pid))

	kr := probeThenKillHolder(dataDir, "live-holder-id", holder.pid, 5*time.Second)
	t.Logf("report: %s", kr.Report)
	assert.True(t, strings.Contains(kr.Report, "killed PID") || strings.Contains(kr.Report, "already gone"))
	assert.True(t, kr.ConfirmedDead, "a genuinely live holder that was killed must be confirmed dead")
	assert.False(t, session.IsProcessAlive(holder.pid), "a genuinely live holder must still be killed")
}

// Old-vs-new behavior (pre-fix unconditional kill vs. the probe gate that
// replaced it) is already fully demonstrated by two other tests working
// together, without needing a third, non-calling "sanity revert" test that
// can't safely exercise the real kill path against its own PID:
//   - TestForceKillHolder_LiveProcess shows forceKillHolder unconditionally
//     kills whatever live PID it's handed — the OLD, ungated danger.
//   - TestProbeThenKillHolder_StalePIDNotKilled shows the NEW
//     probeThenKillHolder gate refuses to touch a stale PID when no real
//     OS lock is held.

func TestForceKillHolder_LiveProcess(t *testing.T) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = platform.Command(context.Background(), "ping", "-n", "30", "127.0.0.1")
	default:
		cmd = platform.Command(context.Background(), "sleep", "30")
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn child: %v", err)
	}

	pid := cmd.Process.Pid
	// Reap in the background so the child does not linger as a zombie once
	// forceKillHolder kills it. forceKillHolder polls IsProcessAlive, and on
	// Unix a zombie still answers kill(pid, 0) until the parent waits on it —
	// so without this concurrent reap the poll never observes the exit.
	go func() { _, _ = cmd.Process.Wait() }()

	require.True(t, session.IsProcessAlive(pid))
	kr := forceKillHolder("", "", pid, 5*time.Second)
	t.Logf("report: %s", kr.Report)
	assert.True(t, strings.Contains(kr.Report, "killed PID") || strings.Contains(kr.Report, "already gone"))
	assert.Contains(t, kr.Report, "exited")
	assert.True(t, kr.ConfirmedDead, "a killed live PID must be reported confirmed dead")
	assert.False(t, session.IsProcessAlive(pid), "PID should be dead after forceKillHolder")
}

// ---------------------------------------------------------------------
// Cross-process helper for TestProbeThenKillHolder_LiveHolderStillKilled.
//
// probeThenKillHolder's whole point is to distinguish "the OS lock is
// genuinely held right now" from "the lock file just names some PID".
// That distinction can only be proven against a REAL second process
// holding the real OS lock — a same-process fake can't reproduce it (see
// the equivalent rationale in internal/session/lock_helper_test.go, which
// this mirrors). This package can't reuse that helper directly (it's
// unexported in the session package), so it re-implements the same
// re-exec idiom scoped to this test binary.
// ---------------------------------------------------------------------

const killTestHelperProcessEnv = "RUSH_SESSIONS_KILL_LOCK_HELPER"

// TestHelperKillTestLockHold is the re-exec entry point for
// spawnKillTestLockHolder. Under a normal `go test` run (sentinel env var
// unset) it does nothing.
func TestHelperKillTestLockHold(t *testing.T) {
	if os.Getenv(killTestHelperProcessEnv) != "1" {
		return
	}
	dataDir := os.Getenv("RUSH_SESSIONS_KILL_LOCK_HELPER_DATADIR")
	sessionID := os.Getenv("RUSH_SESSIONS_KILL_LOCK_HELPER_SESSIONID")
	if dataDir == "" || sessionID == "" {
		fmt.Println("FAILED missing-env")
		os.Exit(2)
	}
	lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
	if err != nil {
		fmt.Printf("FAILED %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("LOCKED %d\n", lk.HolderPID)

	// Block until killed / stdin closes.
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	_ = lk.Release()
	os.Exit(0)
}

type killTestLockHolder struct {
	cmd     *exec.Cmd
	pid     int
	stdinW  io.WriteCloser
	waitErr chan error
}

// spawnKillTestLockHolder starts a real child process that holds a
// session lock on (dataDir, sessionID) via session.TryAcquireSessionLock,
// blocking until stop() kills it. Blocks until the child reports it
// actually holds the lock.
//
// c.Stdin is deliberately wired to a pipe this function itself controls
// (stdinW), rather than left nil. exec.Cmd treats a nil Stdin as "read from
// the null device" — and reading from the null device returns EOF
// immediately, not "block forever". TestHelperKillTestLockHold's blocking
// read (`os.Stdin.Read(buf)`) therefore returned immediately with a nil
// Stdin, so the child released its lock and exited right after printing
// "LOCKED", instead of staying alive until stop() explicitly killed it. That
// was invisible to callers who probe the lock immediately after spawn
// returns (the child's own exit hadn't yet propagated to the OS lock
// release), but any caller doing a bit more work first — e.g. a second
// setupApp() call — could lose the race and see "no live holder" against a
// holder that looked alive moments earlier. Giving the child a real pipe
// that only closes in stop() makes "blocking until stop()" the helper's
// doc comment promises actually true.
//
// reapInBackground controls whether the child is Wait()-ed concurrently
// with whatever the caller does next, rather than only inside stop().
// This is NOT a "better default" toggle — the two behaviors serve opposite
// test intents and neither is strictly safer than the other:
//
//   - reapInBackground=true (used by TestProbeThenKillHolder_
//     LiveHolderStillKilled and TestSessionsReset_ForceStillKillsLiveHolder):
//     these tests measure whether forceKillHolder's SIGKILL-then-poll
//     actually observes the victim's death within its wait budget. On
//     Unix, a SIGKILL'd child that nobody has Wait()-ed is a zombie, and
//     session.IsProcessAlive (kill(pid, 0)) reports a zombie as "alive"
//     forever — so without a concurrent reaper these tests' own poll loop
//     can never see the exit, independent of whether forceKillHolder's
//     logic is correct. See TestForceKillHolder_LiveProcess's identical
//     concurrent-reap rationale a few dozen lines above for the same
//     mechanism against a plain (non-lock-holding) child.
//   - reapInBackground=false (used by TestProbeThenKillHolder_
//     CapturesVictimGenerationWhileHolderAlive and
//     TestAcquireSessionLockForReset_SweepsOnlyAfterReacquire in
//     sessions_kill_sweep_unix_test.go): these tests deliberately WANT the
//     victim to remain an unreaped zombie for a while after the kill —
//     that window is what lets a second process race in and steal the
//     session lock (proving the generation-capture ordering fix) before
//     the test's own explicit holder.stop() call finally reaps it. Reaping
//     eagerly in the background would collapse that window and the race
//     they exist to exercise would never happen, making forceKillHolder's
//     poll return (and the test's own require.Eventually on the new
//     owner's generation) racy or simply return before the steal occurs.
//
// If a future reader sees an unreaped holder here and "fixes" it by
// flipping this to true unconditionally, they will silently break the
// ordering tests above — read this comment first.
func spawnKillTestLockHolder(t *testing.T, dataDir, sessionID string, reapInBackground bool) *killTestLockHolder {
	t.Helper()

	exe, err := os.Executable()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c := platform.Command(ctx, exe, "-test.run=^TestHelperKillTestLockHold$")
	c.Env = append(os.Environ(),
		killTestHelperProcessEnv+"=1",
		"RUSH_SESSIONS_KILL_LOCK_HELPER_DATADIR="+dataDir,
		"RUSH_SESSIONS_KILL_LOCK_HELPER_SESSIONID="+sessionID,
	)
	stdoutR, err := c.StdoutPipe()
	require.NoError(t, err)
	c.Stderr = nil

	stdinR, stdinW, err := os.Pipe()
	require.NoError(t, err)
	c.Stdin = stdinR

	require.NoError(t, c.Start())
	// The child inherited its own copy of the read end via fork/exec; close
	// our reference so the pipe's only remaining open end is stdinW, which
	// stop() closes to signal the child.
	_ = stdinR.Close()

	// See reapInBackground's doc comment above for why this is opt-in
	// rather than always-on. waitErr is buffered so the goroutine can never
	// leak even if stop() ends up being the one to call Wait() instead (it
	// never reads from waitErr when reapInBackground is false).
	var waitErr chan error
	if reapInBackground {
		waitErr = make(chan error, 1)
		go func() { waitErr <- c.Wait() }()
	}

	lineCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdoutR)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				continue
			}
			lineCh <- line
			return
		}
		lineCh <- ""
	}()

	select {
	case line := <-lineCh:
		require.True(t, strings.HasPrefix(line, "LOCKED"), "helper failed to lock: %s", line)
	case <-time.After(15 * time.Second):
		_ = c.Process.Kill()
		t.Fatalf("timed out waiting for kill-test helper process to report lock status")
	}

	return &killTestLockHolder{cmd: c, pid: c.Process.Pid, stdinW: stdinW, waitErr: waitErr}
}

// reapedFromWaitErr adapts the background reaper's error channel to the
// struct{}-close shape waitOrDump expects. The background goroutine in
// spawnKillTestLockHolder does `waitErr <- c.Wait()`, so draining it (as the
// old bare `<-h.waitErr` did) is what signals the child has been reaped.
func reapedFromWaitErr(waitErr <-chan error) <-chan struct{} {
	reaped := make(chan struct{})
	go func() {
		<-waitErr
		close(reaped)
	}()
	return reaped
}

func (h *killTestLockHolder) stop(t *testing.T) {
	if h.stdinW != nil {
		_ = h.stdinW.Close()
	}
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	if h.waitErr != nil {
		// A background reap goroutine (spawnKillTestLockHolder with
		// reapInBackground=true) already claimed Wait() on this *exec.Cmd —
		// calling it a second time here would be undefined behavior. Block
		// until that goroutine has actually reaped the child instead, so
		// stop() keeps its original promise that the process is gone by the
		// time it returns.
		if err := waitOrDump(t, reapedFromWaitErr(h.waitErr), 30*time.Second, "background reaper goroutine of lock-holder process"); err != nil {
			t.Error(err)
		}
		return
	}
	// No background reaper (reapInBackground=false, or this holder was
	// built by a different constructor such as spawnSweepTestNewOwner that
	// never sets waitErr at all): nobody has called Wait() on this cmd yet,
	// so stop() is the first and only caller — reap it directly, exactly as
	// before this file introduced the opt-in background reaper.
	waited := make(chan struct{})
	go func() { _ = h.cmd.Wait(); close(waited) }()
	if err := waitOrDump(t, waited, 30*time.Second, "Wait() on killed lock-holder process"); err != nil {
		t.Error(err)
	}
}

// TestSessionsKillCmdRun_HonorsConfiguredDataDir is the regression test for
// task #219: sessionsKillCmdRun used to hardcode the lock/data path as
// filepath.Join(cwd, ".rush"), completely ignoring --data-dir. This test
// points --data-dir at a directory that is deliberately NOT <cwd>/.rush,
// writes a real lock file (naming this test process's own PID, mirroring
// the stale-PID pattern used elsewhere in this file) at the path the FIX
// computes (<configured-data-dir>/locks/session-<id>.lock), and runs the
// real sessionsKillCmd.RunE. Before the fix this would print "no lock file
// at <cwd>/.rush/locks/..." (wrong path) and no-op; after the fix it must
// find the lock at the configured location and remove it.
func TestSessionsKillCmdRun_HonorsConfiguredDataDir(t *testing.T) {
	// sessionsKillCmdRun now resolves the data directory via
	// config.ResolveDataDirectory, which — like config.Load/config.Init —
	// reads real config paths from the environment unless isolated. See
	// isolateConfigEnvForTests's doc comment (task #224 finding 3).
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	// Deliberately outside workDir entirely, so filepath.Join(cwd, ".rush")
	// can never accidentally coincide with this path.
	configuredDataDir := filepath.Join(tmp, "elsewhere-data")

	ensureRootFlagStandIns(sessionsKillCmd, configuredDataDir)
	if f := sessionsKillCmd.Flags().Lookup("cwd"); f == nil {
		sessionsKillCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsKillCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsKillCmd.Flags().Set("keep-lock", "false"))
	require.NoError(t, sessionsKillCmd.Flags().Set("wait", "2s"))
	sessionsKillCmd.SetContext(context.Background())

	const sessionID = "configured-datadir-id"
	lockDir := filepath.Join(configuredDataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))

	// Sanity: the WRONG (pre-fix) path must not exist, so a false pass via
	// "no lock file at <wrong path>" being silently treated as success is
	// impossible to confuse with the real assertion below.
	wrongPath := filepath.Join(workDir, ".rush", "locks", "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	_, wrongStatErr := os.Stat(wrongPath)
	require.True(t, os.IsNotExist(wrongStatErr))

	stderr := captureStderr(t, func() {
		runErr := sessionsKillCmd.RunE(sessionsKillCmd, []string{sessionID})
		require.NoError(t, runErr)
	})
	t.Logf("sessions kill stderr:\n%s", stderr)

	require.NotContains(t, stderr, "no lock file at",
		"fix must find the lock at the --data-dir-configured location, not report it missing")
	require.Contains(t, stderr, configuredDataDir,
		"report must reference the configured data dir's lock path, not a cwd-based guess")

	_, statErr := os.Stat(lockPath)
	require.True(t, os.IsNotExist(statErr), "lock file at the configured data dir must be removed")
}

// TestSessionsKillAndReset_RelativeDataDirResolveConsistently is the
// regression test for task #224 finding 1: sessionsKillCmdRun used to
// prefer the RAW --data-dir flag string over the config-resolved value
// (`dataDir := dataDirFlag; if dataDir == "" { ... }`). config.setDefaults
// resolves a relative data_directory against workingDir via
// filepathext.SmartJoin, and `sessions.go`'s reset --force block (via
// setupApp -> config.Init -> config.Load) always uses that fully-resolved
// value. So a RELATIVE --data-dir combined with a non-default --cwd used to
// make `sessions kill` resolve the lock path against the process's actual
// cwd (os.Getwd(), from before ResolveCwd's os.Chdir(--cwd) even if the
// working directory implied by --cwd never changed the shell's real cwd for
// unrelated reasons — the point is the two commands could disagree), while
// `sessions reset --force` resolved it against --cwd correctly — exactly
// the "wrong lock path, silent no-op" bug class task #219/6439f0f3 was
// supposed to have fixed, just for relative paths.
//
// This test drives both commands with the SAME relative --data-dir and the
// SAME non-default --cwd and asserts they resolve to the identical final
// absolute data directory, closing the divergence.
func TestSessionsKillAndReset_RelativeDataDirResolveConsistently(t *testing.T) {
	// Capture the real starting cwd BEFORE anything below calls
	// ResolveCwd(--cwd ...), which os.Chdir()s the whole process.
	orig, err := os.Getwd()
	require.NoError(t, err)

	tmp := isolateConfigEnvForTests(t)

	// Register the chdir-restore cleanup AFTER isolateConfigEnvForTests's
	// own t.TempDir() call above: t.Cleanup runs LIFO, so this must run
	// BEFORE tmp's TempDir cleanup tries to RemoveAll(tmp) — on Windows a
	// directory that is still the process's cwd cannot be deleted, and
	// projectDir (created under tmp below) is exactly what --cwd chdirs
	// into.
	t.Cleanup(func() { _ = os.Chdir(orig) })

	// The process's actual OS cwd is deliberately left as whatever the test
	// binary started in (or wherever a prior test in this file chdir'd to);
	// both commands are pointed at a project dir via --cwd instead, so a
	// resolution that (incorrectly) ignores --cwd and uses the process's
	// raw os.Getwd() would resolve to a visibly different, wrong path.
	projectDir := filepath.Join(tmp, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	const relativeDataDir = "relative-data-dir"
	wantDataDir := filepath.Clean(filepath.Join(projectDir, relativeDataDir))

	// --- sessions kill's resolution: exercised through the REAL RunE, not
	// a hand-rolled reimplementation of its resolution logic — otherwise
	// this test could pass even if sessionsKillCmdRun regressed, since it
	// wouldn't be calling the code under test at all. Mirrors
	// TestSessionsKillCmdRun_HonorsConfiguredDataDir's approach: seed a
	// real lock file at the path the FIX computes (wantDataDir), run the
	// real command, and assert it found (and removed) that lock rather
	// than reporting "no lock file at <wrong, cwd-based path>".
	const sessionID = "relative-datadir-consistency-id"
	lockDir := filepath.Join(wantDataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))

	ensureRootFlagStandIns(sessionsKillCmd, relativeDataDir)
	if f := sessionsKillCmd.Flags().Lookup("cwd"); f == nil {
		sessionsKillCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsKillCmd.Flags().Set("cwd", projectDir))
	// sessionsKillCmd is a package-level var shared across every test in
	// this file/binary. Reset --cwd back to "" once this test is done so a
	// later test never inherits a --cwd pointing at this test's (by then
	// deleted) projectDir — this bit a sibling package test
	// (TestSessionsReset_Force*) the first time this test was written,
	// since projectDir no longer exists once t.TempDir() cleans it up.
	t.Cleanup(func() { _ = sessionsKillCmd.Flags().Set("cwd", "") })
	require.NoError(t, sessionsKillCmd.Flags().Set("keep-lock", "false"))
	require.NoError(t, sessionsKillCmd.Flags().Set("wait", "2s"))
	sessionsKillCmd.SetContext(context.Background())

	killStderr := captureStderr(t, func() {
		runErr := sessionsKillCmd.RunE(sessionsKillCmd, []string{sessionID})
		require.NoError(t, runErr)
	})
	t.Logf("sessions kill stderr:\n%s", killStderr)

	require.NotContains(t, killStderr, "no lock file at",
		"sessions kill must resolve a relative --data-dir against --cwd, not the process's raw cwd")
	require.Contains(t, killStderr, wantDataDir,
		"sessions kill's report must reference the --cwd-resolved data dir, not a cwd-based guess")
	_, statErr := os.Stat(lockPath)
	require.True(t, os.IsNotExist(statErr), "lock file at the --cwd-resolved data dir must be removed")

	// --- sessions reset --force's resolution (via the real setupApp path) ---
	ensureRootFlagStandIns(sessionsResetCmd, relativeDataDir)
	if f := sessionsResetCmd.Flags().Lookup("cwd"); f == nil {
		sessionsResetCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsResetCmd.Flags().Set("cwd", projectDir))
	// Same package-level-shared-command hazard as sessionsKillCmd above:
	// reset --cwd back to "" so isolatedResetEnv/isolatedResetEnvWithConfiguredDataDir
	// (which leave --cwd unset, relying on ResolveCwd's os.Getwd() fallback)
	// never inherit this test's now-deleted projectDir.
	t.Cleanup(func() { _ = sessionsResetCmd.Flags().Set("cwd", "") })
	ctx, cancel := context.WithCancel(context.Background())
	sessionsResetCmd.SetContext(ctx)

	built, err := setupApp(sessionsResetCmd)
	require.NoError(t, err)
	t.Cleanup(func() {
		builtDataDir := built.Config().Options.DataDirectory
		built.Shutdown()
		cancel()
		_ = db.Release(builtDataDir)
		waitForSQLiteHandleRelease(t, builtDataDir)
	})

	// Both commands must resolve the SAME relative --data-dir + --cwd
	// combination to the identical absolute path (wantDataDir): sessions
	// kill's real RunE was already asserted above (via the lock file it
	// found and removed) to have operated against wantDataDir; this
	// asserts sessions reset --force's real resolution (setupApp's
	// config.Init) lands on the exact same value, closing the divergence.
	require.Equal(t, wantDataDir, built.Config().Options.DataDirectory,
		"sessions kill and sessions reset --force must resolve the same relative --data-dir to the same absolute path")
}

// TestReacquireAfterKill_WaitsOutLingeringOSLock: the OS may release a killed
// holder's lock after the PID already reads dead (LockFileEx under load). A
// second in-process handle reproduces "busy for a while, then free"; the
// reset re-acquire must ride that out within its wait budget.
func TestReacquireAfterKill_WaitsOutLingeringOSLock(t *testing.T) {
	dataDir := t.TempDir()
	held, err := session.TryAcquireSessionLock(dataDir, "lingering")
	require.NoError(t, err)
	released := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = held.Release()
		close(released)
	}()
	t.Cleanup(func() { <-released })

	lk, err := reacquireAfterKill(dataDir, "lingering", 5*time.Second)
	require.NoError(t, err)
	_ = lk.Release()
}

// TestReacquireAfterKill_GivesUpAfterWait: a lock still held when the budget
// runs out is reported busy, never acquired.
func TestReacquireAfterKill_GivesUpAfterWait(t *testing.T) {
	dataDir := t.TempDir()
	held, err := session.TryAcquireSessionLock(dataDir, "still-held")
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Release() })

	lk, err := reacquireAfterKill(dataDir, "still-held", 200*time.Millisecond)
	var busy *session.SessionLockBusyError
	require.ErrorAs(t, err, &busy)
	require.Nil(t, lk)
}
