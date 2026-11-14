package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsReapCmd = &cobra.Command{
	Use:   "reap",
	Args:  cobra.NoArgs,
	Short: "Remove lock files proven (via a real OS lock probe) to have no live holder",
	Long: `Scan .rush/locks/ and remove any lock that a real OS-level lock
probe (actually attempting flock/LockFileEx) proves has no live holder.
Unlike the old PID/mtime heuristic — which could unlink a path out from
under a live OS lock (advisory locks are bound to the inode, not the path,
so the unlink didn't revoke the live holder, it just let a second process
create a fresh inode at the same path and believe it owned the session:
two owners of one session id) — reap now acquires the real lock per entry
and only removes files it could freely acquire.

Use --dry-run to see what would be reclaimed without touching anything.

Note: an empty lock file with no held OS lock is harmless (the next
acquirer reopens and overwrites it; see internal/session/lock.go's
Release). It is also what a CLEAN release leaves — a "rush run" loop
waiting between turns on a job, a delegation or a retry, or a web session
between turns, holds no lock — so reap keeps every lock file that records
no PID, unprobed (probing would take the lock the owner needs for its next
turn). Only locks that record a PID, which is what a crash leaves behind,
are probed and reclaimed; reap exists to tidy those for operators.

Ancient idle lock files are now reclaimed too: a PID-less lock whose mtime
is older than --idle-older-than (default 7d; "0" disables idle reclaim) is
removed with the same real OS-lock probe and the same POSIX/Windows unlink
ordering as crash locks, plus an os.SameFile file-identity check
immediately before the unlink (the path must still name the probed file; a
path replaced under the probe is left untouched). The threshold keeps the
probe away from any plausible between-turns wait: a loop that took a turn
within the threshold leaves a younger mtime and is still kept unprobed.
--dry-run lists idle candidates without touching or probing them.

The legacy --all flag is accepted for backward compatibility but is now a
no-op: a lock that records a PID is removed if (and only if) the OS-lock
probe proves no live holder, regardless of --all; a lock with no readable
PID is kept.`,
	Example: `
rush sessions reap
rush sessions reap --dry-run
rush sessions reap --all      # legacy no-op
rush sessions reap --dry-run --idle-older-than 30d
  `,
	RunE: sessionsReapCmdRun,
}

// reapRemoveRetryBudget bounds how long a whole sweep may spend retrying
// unlinks of locks it has already proven dead — shared across every lock,
// not granted per lock. It only has to outlast a handle that is on its way
// out (see the call site), so seconds, not tens of seconds: a handle still
// open after this is held by something that is not going to let go, and
// reporting the failure beats hanging the sweep.
const reapRemoveRetryBudget = 3 * time.Second

// reapAfterProbeHook is a deterministic test seam invoked while the probe
// lock is held and immediately before unlinking on Unix.
var reapAfterProbeHook func(path string)

// reapAfterUnlinkHook is a deterministic test seam invoked after successful unlink.
var reapAfterUnlinkHook func(path string)

// reapPreIdentitySeam is a deterministic test-only seam fired after the
// probe lock is won and before the identity check; nil in production.
var reapPreIdentitySeam func(path string)

// reapLockIdentityLost reports whether lockPath no longer names the same
// file it named at baseline: another process may have unlinked and
// recreated the path between the pre-probe stat and the unlink, and
// unlinking a path a NEW owner just created would hand them an unlinked
// inode. Mirrors RemoveDeadHostFile's os.SameFile identity discipline
// (internal/session/host_lock.go); any stat failure is treated as lost —
// fail closed, keep the path untouched.
func reapLockIdentityLost(baseline os.FileInfo, lockPath string) bool {
	current, err := os.Stat(lockPath)
	if err != nil {
		return true
	}
	return !os.SameFile(baseline, current)
}

type reapItem struct {
	Path   string
	PID    int
	AgeSec int
	Action string // "remove-dead", "probe-candidate", "idle-candidate", "remove-unreadable", "skip-alive", "skip-young", "skip-released", "skip-probe-error", "skip-replaced"
	Err    error
	// Idle: the lock records no PID and is older than --idle-older-than.
	Idle bool
	// Baseline: a pre-probe stat paired against a fresh stat at unlink time
	// (reapLockIdentityLost).
	Baseline os.FileInfo
}

func sessionsReapCmdRun(cmd *cobra.Command, args []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	idleRaw, _ := cmd.Flags().GetString("idle-older-than")
	var idleOlderThan time.Duration
	if strings.TrimSpace(idleRaw) != "0" {
		d, parseErr := parseAge(idleRaw)
		if parseErr != nil {
			return fmt.Errorf("invalid --idle-older-than %q: %w", idleRaw, parseErr)
		}
		idleOlderThan = d
	}

	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return err
	}

	// Resolve the data directory the same lightweight way `sessions kill`
	// does (task #219/#224): honor --data-dir first, then the project's
	// configured data_directory, and only fall back to <cwd>/.rush if
	// neither is set. This command is purely filesystem-based (no DB,
	// no provider config), so config.ResolveDataDirectory's pure, local
	// resolution is used instead of pulling in the full setupApp. See
	// task #233 — the same cwd-hardcoding bug class as #219/#224/#231.
	dataDirFlag, _ := cmd.Flags().GetString("data-dir")
	dataDir, err := config.ResolveDataDirectory(cwd, dataDirFlag)
	if err != nil {
		return fmt.Errorf("failed to resolve data directory: %w", err)
	}

	locksDir := filepath.Join(dataDir, "locks")
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "(no locks directory)")
			return nil
		}
		return err
	}

	// A lock younger than one heartbeat may be mid-acquisition; see the
	// skip-young branch below for why probing it is actively dangerous.
	const heartbeatThreshold = 10 * time.Second
	now := time.Now()
	items := []reapItem{}
	removed := 0

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "session-") || !strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}
		path := filepath.Join(locksDir, entry.Name())
		info, statErr := entry.Info()
		if statErr != nil {
			continue
		}
		age := now.Sub(info.ModTime())
		sessionID := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "session-"), ".lock")
		pid := session.ReadLockPID(path)
		item := reapItem{Path: path, PID: pid, AgeSec: int(age.Seconds())}

		// Too young to judge — skip without even probing.
		//
		// session.acquireSessionLockFile opens the file and takes the OS lock
		// as two SEPARATE syscalls (internal/session/lock.go: os.OpenFile then
		// tryLockFile). In the window between them the file exists on disk
		// with age ~0 and no lock held, so a probe would acquire successfully
		// and we would classify a session that is starting RIGHT NOW as
		// "provably dead" and unlink its path. That process then locks its own
		// (now unlinked) inode and believes it owns the session, while the
		// next run creates a fresh inode at the same path and also acquires:
		// two owners of one session id — the exact bug this command exists to
		// prevent, reintroduced by the cleanup itself.
		//
		// Young locks are also the worst case for the probe-induced contention
		// documented on lockHolderProvablyDead: a lock seconds old is far more
		// likely to belong to a run that is starting than to the crash backlog
		// reap is for. Skipping them costs nothing — they are not the backlog.
		if age < heartbeatThreshold {
			item.Action = "skip-young"
			items = append(items, item)
			continue
		}

		// A lock file with no recorded PID is what a CLEAN release leaves (the
		// holder truncates it): a `rush run` loop waiting between turns, a web
		// session between turns. Nothing died, so there is nothing to reclaim,
		// and probing it takes the lock the owner needs for its next turn and
		// unlinks a path it will reuse (R8C-5; the same guard `sessions locks
		// --prune` has). A crash leaves its PID in the file and is still swept.
		if pid <= 0 {
			if idleOlderThan > 0 && age >= idleOlderThan {
				item.Idle = true
			} else {
				item.Action = "skip-released"
				items = append(items, item)
				continue
			}
		}

		if dryRun {
			if item.Idle {
				item.Action = "idle-candidate"
			} else {
				item.Action = "probe-candidate"
			}
			items = append(items, item)
			continue
		}

		// The ONLY grounds for removing a stable per-session lock file is
		// proving at the OS level that no live process holds it — by actually
		// acquiring the real OS lock (flock/LockFileEx). PID liveness and
		// mtime age are reporting only here: a stale/dead RECORDED pid is not
		// proof the holder is gone (the OS may have recycled that PID number
		// for an unrelated process, or a live holder may be running with a
		// cleared/stale PID after a partial write). Unlinking a path out from
		// under a live holder does not revoke its OS lock (advisory locks are
		// bound to the inode, not the path); it just lets a second process
		// create a fresh inode at the same path and believe it owns the
		// session — two owners of one session id. See internal/session/lock.go
		// and lockHolderProvablyDead (sessions.go) for the full rationale.
		// Baseline for the identity check: a fresh stat right before the probe,
		// paired against the file at unlink time (see reapLockIdentityLost).
		baselineInfo, baselineErr := os.Stat(path)
		if baselineErr != nil {
			item.Action = "skip-probe-error"
			items = append(items, item)
			continue
		}
		item.Baseline = baselineInfo

		lk, probeErr := acquireReapProbe(dataDir, sessionID)
		switch probeErr {
		case nil:
			item.Action = "remove-dead"
			if reapPreIdentitySeam != nil {
				reapPreIdentitySeam(path)
			}
			// Identity check while the probe lock is held: the path must still
			// name the exact file this process just locked.
			if reapLockIdentityLost(baselineInfo, path) {
				_ = lk.Release()
				item.Action = "skip-replaced"
				items = append(items, item)
				continue
			}
			// On Unix, unlink while holding flock: it is attached to the inode,
			// so releasing first lets an owner acquire before the path is removed.
			// Windows requires closing handles before unlink; the second probe
			// below checks for an owner that acquired during that interval.
			if runtime.GOOS == "windows" {
				_ = lk.Release()
			} else {
				if reapAfterProbeHook != nil {
					reapAfterProbeHook(path)
				}
				// Clear stale sidecars before unlinking: after unlink a new owner
				// may create a different lock at this path.
				removeReapSidecars(path)
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					item.Action = "remove-failed"
					item.Err = err
				} else {
					item.Action = "removed"
					removed++
					if reapAfterUnlinkHook != nil {
						reapAfterUnlinkHook(path)
					}
				}
				_ = lk.Release()
			}
		default:
			var busyErr *session.SessionLockBusyError
			if errors.As(probeErr, &busyErr) {
				item.Action = "skip-alive"
			} else {
				// Unknown probe failure — fail closed; never unlink on an
				// uncertain state.
				item.Action = "skip-probe-error"
			}
		}
		items = append(items, item)
	}

	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "(no locks)")
		return nil
	}

	// One retry budget for the WHOLE sweep, not one per lock.
	//
	// Not a bare os.Remove: on Windows the unlink fails with
	// ERROR_SHARING_VIOLATION while ANY handle to the file is still open,
	// and at this point one plausibly is. Release() above closes the lock
	// handle synchronously but then clears the holder metadata in a
	// goroutine that reopens the file, and only waits
	// releaseMetadataCleanupBound (50ms) for it — on a loaded machine that
	// reopened handle outlives the wait. An indexer or scanner touching a
	// freshly written file does the same thing. Either way it clears on its
	// own in milliseconds, so giving up after one attempt turns a transient
	// handle into "reclaimed 0 lock(s)".
	//
	// But removeLockWithRetry only short-circuits on IsNotExist: a
	// PERMANENT failure — ACCESS_DENIED, a read-only volume, a share
	// without delete rights — is retried for the full window too. Per-lock,
	// that made a crash backlog of 40 undeletable locks take two silent
	// minutes instead of failing instantly, on Unix as well, where the
	// sharing violation this exists for cannot happen. A shared budget
	// keeps the transient case (one or two locks) fully covered while a
	// hopeless sweep degrades to the old one-shot behaviour after the
	// budget is spent, rather than multiplying the wait by the number of
	// locks.
	retryDeadline := time.Now().Add(reapRemoveRetryBudget)
	budgetSpent := false
	for _, it := range items {
		switch it.Action {
		case "remove-dead":
			// Windows cannot unlink while the probe handle is open. Re-probe
			// immediately before unlink so an owner that acquired after Release
			// is observed and preserved.
			if runtime.GOOS == "windows" {
				if reapAfterProbeHook != nil {
					reapAfterProbeHook(it.Path)
				}
				lk, err := acquireReapProbe(dataDir, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(it.Path), "session-"), ".lock"))
				if err != nil {
					var busyErr *session.SessionLockBusyError
					if errors.As(err, &busyErr) {
						fmt.Fprintf(os.Stderr, "kept   live lock %s (OS lock held, age %ds)\n", filepath.Base(it.Path), it.AgeSec)
					} else {
						fmt.Fprintf(os.Stderr, "kept   lock %s (probe inconclusive; left untouched, age %ds)\n", filepath.Base(it.Path), it.AgeSec)
					}
					continue
				}
				_ = lk.Release()
				// Identity check before removal (Windows path): the path must
				// still name the file the second probe just won.
				if reapLockIdentityLost(it.Baseline, it.Path) {
					fmt.Fprintf(os.Stderr, "kept   lock %s (path no longer names the probed file; left untouched, age %ds)\n", filepath.Base(it.Path), it.AgeSec)
					continue
				}
			}
			action := "would remove"
			if !dryRun {
				retryFor := time.Until(retryDeadline)
				if retryFor <= 0 {
					retryFor = 0
					budgetSpent = true
				}
				if err := removeLockWithRetry(it.Path, retryFor); err == nil {
					action = "removed"
					removed++
					removeReapSidecars(it.Path)
					if reapAfterUnlinkHook != nil {
						reapAfterUnlinkHook(it.Path)
					}
				} else {
					action = "failed to remove (" + err.Error() + ")"
				}
			}
			if it.Idle {
				fmt.Fprintf(os.Stderr, "%s idle lock %s (no recorded PID, OS lock free, idle %s, over --idle-older-than %s)\n",
					action, filepath.Base(it.Path), formatDurationShort(time.Duration(it.AgeSec)*time.Second), idleOlderThan)
			} else {
				fmt.Fprintf(os.Stderr, "%s orphan lock %s (holder provably dead via OS lock, age %ds)\n",
					action, filepath.Base(it.Path), it.AgeSec)
			}
		case "removed":
			if it.Idle {
				fmt.Fprintf(os.Stderr, "removed idle lock %s (no recorded PID, OS lock free, idle %s)\n", filepath.Base(it.Path), formatDurationShort(time.Duration(it.AgeSec)*time.Second))
			} else {
				fmt.Fprintf(os.Stderr, "removed orphan lock %s (holder provably dead via OS lock, age %ds)\n", filepath.Base(it.Path), it.AgeSec)
			}
		case "remove-failed":
			if it.Idle {
				fmt.Fprintf(os.Stderr, "failed to remove (%v) idle lock %s (no recorded PID, OS lock free, idle %s, over --idle-older-than %s)\n", it.Err, filepath.Base(it.Path), formatDurationShort(time.Duration(it.AgeSec)*time.Second), idleOlderThan)
			} else {
				fmt.Fprintf(os.Stderr, "failed to remove (%v) orphan lock %s (holder provably dead via OS lock, age %ds)\n", it.Err, filepath.Base(it.Path), it.AgeSec)
			}
		case "probe-candidate":
			fmt.Fprintf(os.Stderr, "would probe lock %s (aged PID recorded; candidate, age %ds)\n",
				filepath.Base(it.Path), it.AgeSec)
		case "idle-candidate":
			fmt.Fprintf(os.Stderr, "would remove idle lock %s (no recorded PID, not probed by dry-run, idle %s, over --idle-older-than %s)\n",
				filepath.Base(it.Path), formatDurationShort(time.Duration(it.AgeSec)*time.Second), idleOlderThan)
		case "skip-replaced":
			fmt.Fprintf(os.Stderr, "kept   lock %s (path no longer names the probed file; left untouched, age %ds)\n",
				filepath.Base(it.Path), it.AgeSec)
		case "skip-alive":
			if it.Idle {
				fmt.Fprintf(os.Stderr, "kept   idle lock %s (OS lock held: a holder exists, age %ds)\n",
					filepath.Base(it.Path), it.AgeSec)
			} else {
				fmt.Fprintf(os.Stderr, "kept   live lock %s (OS lock held, age %ds)\n",
					filepath.Base(it.Path), it.AgeSec)
			}
		case "skip-probe-error":
			fmt.Fprintf(os.Stderr, "kept   lock %s (probe inconclusive; left untouched, age %ds)\n",
				filepath.Base(it.Path), it.AgeSec)
		case "skip-released":
			fmt.Fprintf(os.Stderr, "kept   released lock %s (no recorded PID: a clean release, e.g. a loop between turns; not probed, age %ds)\n",
				filepath.Base(it.Path), it.AgeSec)
		case "skip-young":
			fmt.Fprintf(os.Stderr, "kept   young lock %s (age %ds, may still be mid-acquisition — not probed)\n",
				filepath.Base(it.Path), it.AgeSec)
		}
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "(dry-run; %d lock(s) would be probed)\n", countAction(items, "probe-candidate"))
		if idleCount := countAction(items, "idle-candidate"); idleCount > 0 {
			fmt.Fprintf(os.Stderr, "(dry-run; %d idle lock(s) would be removed over --idle-older-than %s)\n", idleCount, idleOlderThan)
		}
	} else {
		// Say it, rather than just going quiet: once the budget is gone the
		// later locks got a single attempt each, so a failure below that
		// line means "did not retry", not "retried and lost".
		if budgetSpent {
			fmt.Fprintf(os.Stderr,
				"(retry budget of %s spent on earlier locks; the rest got one attempt each)\n",
				reapRemoveRetryBudget)
		}
		fmt.Fprintf(os.Stderr, "reclaimed %d lock(s)\n", removed)
	}
	return nil
}

func acquireReapProbe(dataDir, sessionID string) (*session.SessionLock, error) {
	// A probe is not the session owner. On Unix, Release may run metadata
	// cleanup after reap unlinks the path, so suppress it to protect a new
	// owner at that path. Windows must retain normal cleanup: skipped/failed
	// removals rely on Release to clear the holder metadata as before.
	if runtime.GOOS == "windows" {
		return session.TryAcquireSessionLock(dataDir, sessionID)
	}
	return acquireUnixReapProbe(dataDir, sessionID)
}

// acquireUnixReapProbe keeps the Unix probe's no-cleanup policy independent
// of the host OS so its production behavior can be exercised on Windows too.
func acquireUnixReapProbe(dataDir, sessionID string) (*session.SessionLock, error) {
	return session.TryAcquireSessionLockWithOptions(dataDir, sessionID,
		session.WithClearHolderMetadataFn(func(string, string) {}),
	)
}

func removeReapSidecars(path string) {
	for _, sidecar := range []string{path + ".pid", path + ".gen"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "kept   sidecar %s (%v)\n", filepath.Base(sidecar), err)
		}
	}
}

func countAction(items []reapItem, action string) int {
	n := 0
	for _, it := range items {
		if it.Action == action {
			n++
		}
	}
	return n
}

func init() {
	sessionsReapCmd.Flags().Bool("dry-run", false, "Print what would be reclaimed without removing anything")
	sessionsReapCmd.Flags().Bool("all", false, "Legacy no-op: removal is now decided solely by the real OS-lock probe, so a lock that records a PID is reaped iff proven uncontended regardless of this flag")
	sessionsReapCmd.Flags().String("idle-older-than", "168h", "Age threshold for reclaiming idle (PID-less) lock files, e.g. 168h, 7d, 4w; \"0\" disables idle reclaim. Default 168h (7 days)")
}
