package cmd

// The `sessions locks` subcommand: list session lock files with heartbeat
// pulse classification, plus the --prune path guarded by the
// lockHolderProvablyDead OS-level probe.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsLocksCmd = &cobra.Command{
	Use:   "locks [session-id]",
	Short: "List active session lock files",
	Long: `List session lock files in the configured data directory (normally
.rush/locks) and show the recorded PID, pulse, pulse age, elapsed mtime age
and any saved timeout budget. The initial pulse is based on lock-file mtime:
it reads alive through 10s, ping through 15s, stopping through 20s, then
offline. Fresher message or sub-agent activity can update the displayed
pulse age, and shared activity can relabel a live between-turn wait. Mtime
age is not run duration and by itself does not prove that a process died.

Idle lock files — a lock that records no holder PID, whose pulse reads
offline, and which no live-driver evidence overturned (exactly the state a
clean Release leaves behind) — are collapsed by the default view into a
one-line summary such as:

  3 idle lock files (no holder, no PID) hidden — use --all to list

Pass --all to list them anyway. Passing an explicit session id shows that
session's lock even when it is idle. With --json, the NDJSON stream ends
with a final summary line {"summary":{"total":N,"shown":M,"hidden_idle":K}}
(hidden_idle is 0 with --all); --stale-only still filters on top of the
collapse.

A session lock is held only during a turn. A live "rush run" may release
it while waiting between turns on a job, delegation or retry; shared
session-activity and batch/host evidence can label that row "between turns"
instead of offline. A released lock's age is distinct from a live run
driver's status, and a worker's per-process host lock is distinct from the
session lock. Use "rush sessions why <id>" to inspect the combined session
and batch evidence.

By default this command is READ-ONLY and never removes lock files.
--stale-only filters entries that remain offline after available live-
activity checks; it is not simply an age or PID test. --prune opts in to
removal only for an entry older than 60s when a real OS-level lock probe
proves there is no current holder and the activity classifier does not
report live session work. The probe is more reliable than mtime alone, but
prune is still an explicit removal operation.

Pass a session id (full id or hash prefix) to list only that session's
lock. Use --json for NDJSON output suitable for metrics collection or
automation.`,
	Args: cobra.MaximumNArgs(1),
	Example: `
# Show all locks
rush sessions locks

# One session's heartbeat
rush sessions locks pr-42

# Show only stale locks
rush sessions locks --stale-only

# Include idle lock files the default view collapses
rush sessions locks --all

# Stream to jq for scripting
rush sessions locks --json | jq '.session_id'
  `,
	RunE: sessionsLocksCmdRun,
}

// lockPulseStatus classifies a lock file by its heartbeat mtime.
// Heartbeat interval = 10s, stale threshold = 20s (session.lockStaleDuration).
//
//	0–10s  → "alive"    (fresh heartbeat)
//	10–15s → "ping"     (one beat overdue, likely OK)
//	15–20s → "stopping" (two beats missed, probably finishing)
//	>20s   → "offline"  (stale — holder crashed, exited without Release, or
//	                     released the lock after a turn: see "between-turns")
//
// "between-turns" is not derived from age: the caller sets it for an
// "offline" lock of a session a live `rush run` loop drives.
func lockPulseStatus(ageSec int64) string {
	switch {
	case ageSec <= 10:
		return "alive"
	case ageSec <= 15:
		return "ping"
	case ageSec <= 20:
		return "stopping"
	default:
		return "offline"
	}
}

// lockHolderProvablyDead decides, via a real OS-level lock attempt, whether
// the lock file at lockPath under dataDir/locks/session-<id>.lock may safely
// be auto-deleted. Mirrors sessions_kill.go's probeThenKillHolder — same
// "don't trust mtime alone, prove it via the real OS lock" discipline (task
// #222 hardening).
//
// Why mtime alone stopped being safe here: task #214/#222 gated the
// heartbeat's mtime-touch on real RecordActivity() calls instead of an
// unconditional 10s timer. A session blocked on a single long-running tool
// call (bounded by toolExecutionMaxDefault, up to 45 minutes) can now go
// well past the old 60s auto-delete threshold with zero recorded activity
// while still being completely healthy and still holding the real OS lock.
// Unconditionally os.Remove-ing the path in that case does NOT revoke the
// live holder's flock/LockFileEx (advisory locks are bound to the inode,
// not the path) — it just lets a SECOND process create a fresh inode at the
// same path and believe it owns the session, producing two simultaneous
// owners of one session id (see the package doc on session.SessionLock).
//
// Returns true only when TryAcquireSessionLock itself succeeds — i.e. we
// just proved, at the kernel level, that nobody holds the lock right now.
// The lock we acquired to prove that is released immediately. Any other
// outcome (busy error, or an unidentified failure) is treated as "do not
// delete" — the conservative default, since a false "provably dead" is what
// causes the two-owners bug, while a false "still alive" merely means the
// stale entry lingers one more `sessions locks` invocation.
//
// What this does NOT solve (task #230, narrower follow-ups to #222): two
// related gaps remain, both real but far narrower than the unconditional
// os.Remove this function replaced.
//
//  1. Residual TOCTOU between probe and removal. This function proves
//     "nobody holds the lock" and releases immediately; it does not itself
//     remove the file. Its caller (sessionsLocksCmdRun) calls os.Remove
//     afterward, as a separate, non-atomic step. In the gap between this
//     function's Release() and the caller's os.Remove(), a fresh
//     `rush run --session <id>` can legitimately re-acquire the same
//     session id and start writing — and the caller would then unlink
//     that new, live holder's lock file, reproducing (in a window on the
//     order of a syscall or two, not the original unbounded-mtime window)
//     the exact two-owners scenario this hardening exists to prevent.
//     Closing this fully would mean holding the OS lock across the
//     os.Remove itself (return the still-held *session.SessionLock to the
//     caller instead of releasing here, remove the path while holding it,
//     then release). That was deliberately NOT done: this package's sibling
//     filelock.FileLock.Release doc comment already documents why removing a
//     path while a lock on it is held is cross-platform-fragile — POSIX
//     unlink of a locked-but-open file is harmless (flock is keyed off the
//     inode), but the lock files here are opened without FILE_SHARE_DELETE
//     on Windows, so a concurrent opener's handle can make the delete fail
//     with a sharing violation, or a successful delete can race a
//     subsequent opener into creating a distinct file object at the same
//     path while an older locked one still exists — i.e. the fix risks
//     reintroducing a shaped version of the same two-owners bug on Windows
//     specifically, in exchange for closing a gap that is already down to
//     single-digit milliseconds. Not worth it here; the window is accepted
//     and documented instead.
//  2. Probe-induced transient contention. Proving death takes a real
//     exclusive OS lock (open + LockFileEx/flock + truncate + PID write +
//     Sync + sidecar write + Chtimes, then release) on every lock file
//     `sessions locks` inspects that looks older than autoDeleteAfter. A
//     `rush run` legitimately starting on that exact session id during
//     that brief window gets a hard "already in use" abort with no retry.
//     This is why the fix for (1) above is not simply "hold the lock
//     longer" — extending how long the probe holds the OS lock would widen
//     this exact contention window, trading one narrow risk for a wider
//     one. Given how narrow the window already is (a single acquire+release
//     cycle, not the run's full lifetime) and that it only collides with a
//     `rush run` starting on the SAME session id in that SAME instant,
//     this is accepted as-is rather than gated behind a flag — see
//     sessionsLocksCmdRun's doc comment for the fuller default-behavior
//     tradeoff.
func lockHolderProvablyDead(dataDir, sessionID string) bool {
	cleanupDone := make(chan struct{})
	opts := append([]session.LockOption{session.WithCleanupDone(cleanupDone)}, lockProbeOptions...)
	lk, err := session.TryAcquireSessionLockWithOptions(dataDir, sessionID, opts...)
	if err != nil {
		// Busy (a real holder exists) or an unidentified probe failure —
		// neither is proof of death. Do not delete.
		return false
	}
	_ = lk.Release()
	// Release waits only releaseMetadataCleanupBound for the PID/sidecar
	// cleanup; past that bound the cleanup goroutine keeps the file open,
	// and the caller's os.Remove then hits a Windows sharing violation
	// (a spurious "could not remove" warning and a lock left behind).
	// Join the cleanup, bounded so a hung FS/AV/SMB cannot stall the CLI.
	select {
	case <-cleanupDone:
	case <-time.After(lockProbeCleanupJoinBound):
	}
	return true
}

// lockProbeCleanupJoinBound bounds how long lockHolderProvablyDead waits for
// the probe's metadata cleanup after Release returns.
const lockProbeCleanupJoinBound = 2 * time.Second

// lockProbeOptions is a test-only seam: extra options for the death probe's
// acquire (e.g. a slow metadata cleanup). Always nil in production.
var lockProbeOptions []session.LockOption

// preAutoDeleteRemoveHook is a test-only seam for sessionsLocksCmdRun's
// auto-delete branch. When non-nil, it is called synchronously between
// lockHolderProvablyDead proving a holder dead and the os.Remove call,
// allowing a test to deterministically reproduce the TOCTOU window where a
// concurrent process removes the lock file between the probe and the Remove.
// Always nil in production.
var preAutoDeleteRemoveHook func(lockPath string)

// lockEntryInfo is a seam for handling DirEntry.Info failures.
var lockEntryInfo = func(entry os.DirEntry) (os.FileInfo, error) {
	return entry.Info()
}

// locksJSONSummary closes the --json NDJSON stream: machine consumers get
// the collapse count even when the array part is filtered.
type locksJSONSummary struct {
	Total      int `json:"total"`       // entries considered after the session-id filter (shown + hidden idle)
	Shown      int `json:"shown"`       // rows listed above the summary line
	HiddenIdle int `json:"hidden_idle"` // idle lock files collapsed by the default view; 0 with --all
}

// locksJSONEnvelope is the final --json line.
type locksJSONEnvelope struct {
	Summary locksJSONSummary `json:"summary"`
}

// lockIsIdle reports whether a classified lock entry is a settled
// no-holder leftover: no PID recorded (neither file content nor sidecar),
// an offline pulse no live evidence overturned, not between turns, not
// stalled. This is exactly what a clean Release leaves behind — harmless,
// but noise at listing time.
func lockIsIdle(pid int, stale, betweenTurns, stalled bool) bool {
	return pid <= 0 && stale && !betweenTurns && !stalled
}

func sessionsLocksCmdRun(cmd *cobra.Command, args []string) error {
	asJSON, _ := cmd.Flags().GetBool("json")
	staleOnly, _ := cmd.Flags().GetBool("stale-only")
	prune, _ := cmd.Flags().GetBool("prune")
	showAll, _ := cmd.Flags().GetBool("all")

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	// Use the data directory setupApp already resolved onto `a` (honors
	// --data-dir and the project's configured data_directory) instead of
	// recomputing a cwd-based guess — see task #219/#224, and task #231
	// (this exact function was left unfixed when those landed).
	dataDir := a.Config().Options.DataDirectory
	locksDir := filepath.Join(dataDir, "locks")

	onlyStem := ""
	if len(args) == 1 {
		if onlyStem, err = resolveLocksFilterStem(cmd.Context(), a, args[0]); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		if os.IsNotExist(err) {
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				return enc.Encode(locksJSONEnvelope{Summary: locksJSONSummary{}})
			}
			fmt.Println("(no locks)")
			return nil
		}
		return err
	}

	type lockItem struct {
		SessionID   string `json:"session_id"`
		PID         int    `json:"pid"`
		PulseSec    int64  `json:"pulse_sec"`
		Pulse       string `json:"pulse"` // alive / ping / stopping / offline / between-turns
		AcquiredAt  int64  `json:"acquired_at_unix"`
		DurationSec int64  `json:"duration_seconds"`
		Stale       bool   `json:"stale"`
		BudgetSec   int64  `json:"budget_sec,omitempty"` // --timeout seconds, 0 if not set
		// SubAgent, when non-empty, means the freshest activity in this
		// session's call tree came from an in-flight sub-agent delegation,
		// NOT the top-level heartbeat — so PulseSec/Pulse below are the
		// sub-agent's activity age, which is the honest "is anything actually
		// making progress" signal an operator wants during a long delegation.
		SubAgent string `json:"sub_agent,omitempty"`
		// BetweenTurns: the lock is a clean-release leftover of a session a live
		// `rush run` loop still drives (waiting on a job, a delegation or a
		// retry); Pulse is "between-turns", Stale false, DriverPID the loop's.
		BetweenTurns bool `json:"between_turns,omitempty"`
		DriverPID    int  `json:"driver_pid,omitempty"`
		// Stalled: the heartbeat mtime is past stallPulseThreshold while a
		// holder PID is still recorded (sessions_stall.go).
		Stalled bool `json:"stalled,omitempty"`
	}

	// One classifier pass over every known session (top-level list + any
	// id carrying a driver marker): the lock-file stem -> real ids map comes
	// straight from the batch result (D11), and the per-stem verdict decides
	// "between turns" and the prune guard -- no local lock-name aggregation.
	allIDs := stemIDsForLocks(cmd.Context(), a)
	acts, actErr := a.SessionActivityBatch(cmd.Context(), allIDs)
	stemIDs := map[string][]string{}
	if acts.LockStems != nil {
		stemIDs = acts.LockStems
	}
	// Include unlisted lock stems so orphaned session files retain the previous
	// call-tree activity lookup semantics, but perform all lookups in one batch.
	activityIDs := append([]string(nil), allIDs...)
	seenActivityIDs := make(map[string]struct{}, len(activityIDs))
	for _, id := range activityIDs {
		seenActivityIDs[id] = struct{}{}
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "session-") || !strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}
		stem := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "session-"), ".lock")
		if len(stemIDs[stem]) == 0 {
			if _, ok := seenActivityIDs[stem]; !ok {
				activityIDs = append(activityIDs, stem)
				seenActivityIDs[stem] = struct{}{}
			}
		}
	}
	callTreeActivities := computeCallTreeActivityBatch(cmd.Context(), a, activityIDs)
	liveVerdict := func(stem string) (session.ActivityVerdict, bool) {
		if actErr != nil {
			return session.ActivityVerdict{}, false
		}
		var live session.ActivityVerdict
		found := false
		for _, id := range stemIDs[stem] {
			v := acts.ByID[id].Verdict
			if kindIsLive(v.Kind) && (!found || v.PID > live.PID) {
				live, found = v, true
			}
		}
		return live, found
	}

	var locks []lockItem
	idleHidden := 0
	now := time.Now()
	const autoDeleteAfter = 60 * time.Second

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "session-") || !strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}

		sessionID := strings.TrimPrefix(entry.Name(), "session-")
		sessionID = strings.TrimSuffix(sessionID, ".lock")
		if onlyStem != "" && sessionID != onlyStem {
			continue
		}

		info, infoErr := lockEntryInfo(entry)
		if infoErr != nil {
			continue
		}
		age := now.Sub(info.ModTime())

		lockPath := filepath.Join(locksDir, entry.Name())
		// removeFailed marks a provably-dead lock whose removal failed: its
		// file now has a wiped PID and fresh mtime, so it must never be
		// collapsed into the idle summary.
		removeFailed := false

		// Auto-delete candidate: mtime older than 1 minute AND the operator
		// explicitly opted in via --prune. By default `sessions locks` is
		// READ-ONLY — it never removes anything. Deleting stable per-session
		// lock files is unnecessary in the first place (an empty lock file
		// with no held OS lock is harmless: the next acquirer reopens and
		// overwrites it, see internal/session/lock.go's Release), so the
		// whole class of TOCTOU "unlink a path that a live holder may be
		// reusing" bugs is closed by NOT auto-deleting unless an operator
		// asks for it.
		//
		// mtime alone is NO LONGER proof of death (task #222) — the
		// heartbeat's mtime touch is gated on RecordActivity, so a session
		// blocked on a single long-running tool call (up to
		// toolExecutionMaxDefault, 45 minutes) can look stale here while
		// still being completely healthy and still holding the real OS lock.
		// Before deleting under --prune, prove it via a real OS-level lock
		// attempt (lockHolderProvablyDead) — same discipline as
		// sessions_kill.go's probeThenKillHolder. Unlinking a path out from
		// under a live holder's flock/LockFileEx does not revoke it (advisory
		// locks are bound to the inode, not the path); it just lets a second
		// process create a fresh inode at the same path and believe it owns
		// the session — two owners of one session id.
		// A session a live `rush run` loop drives (between turns) is never
		// pruned: the death probe would take the lock the loop is about to
		// take for its next turn.
		if _, driven := liveVerdict(sessionID); prune && age > autoDeleteAfter && !driven {
			if lockHolderProvablyDead(dataDir, sessionID) {
				if preAutoDeleteRemoveHook != nil {
					preAutoDeleteRemoveHook(lockPath)
				}
				err := os.Remove(lockPath)
				switch {
				case err == nil, errors.Is(err, fs.ErrNotExist):
					// errors.Is(err, fs.ErrNotExist) (task #244): a concurrent
					// process — `sessions reap`, `sessions kill`, `sessions
					// reset --force`, or another parallel `sessions locks`
					// racing the same auto-delete path — may have already
					// removed this exact file between lockHolderProvablyDead's
					// probe above and this Remove call. The goal of this
					// Remove ("the stale lock file is gone from disk") is
					// already achieved in that case; it is not a failure.
					// Treating ENOENT as an error here (as task #234's fix
					// briefly did) produced a false "warning: could not
					// remove..." on stderr plus a phantom display-path row
					// below for a file that no longer exists (PID 0, offline,
					// age computed from the stale cached entry.Info()) —
					// exactly the false positive an orchestrating script
					// polling `sessions locks` alongside a concurrent
					// `sessions kill` would hit. Report success and move on,
					// same as the pre-#234 behavior for this specific case.
					fmt.Fprintf(os.Stderr, "removed stale lock %s (age %ds, holder provably dead)\n", entry.Name(), int(age.Seconds()))
					continue
				default:
					// Do NOT silently swallow this (task #234). A failed
					// Remove here is not a harmless no-op: lockHolderProvablyDead's
					// own acquire+release probe (see its doc comment) truncates
					// the lock file's content and freshens its mtime as part of
					// proving death, moments before this Remove runs. If the
					// Remove then fails for a REAL reason (e.g. a Windows
					// sharing-violation window from a concurrent opener that
					// still holds the file open, or permission denied — NOT
					// the file already being gone, which is handled above),
					// the file is left behind with a wiped PID AND a fresh
					// mtime — which every PID-fallback/mtime-based liveness
					// consumer below (the session-activity classifier reads
					// the same lock fact) would read
					// as LIVE for the next heartbeat-stale window, even though
					// this probe JUST proved the holder dead. Silently
					// `continue`-ing also made the entry vanish from this
					// listing entirely, so an operator had zero signal
					// anything was wrong. Surface the failure, then fall
					// through to the normal display path below instead of
					// `continue`-ing: age is already > autoDeleteAfter (60s),
					// which lockPulseStatus always classifies as "offline"
					// (>20s), so the entry is correctly shown as stale/offline
					// rather than silently dropped from the listing while its
					// stale file lingers on disk.
					removeFailed = true
					fmt.Fprintf(os.Stderr, "warning: could not remove provably-dead lock %s: %v\n", entry.Name(), err)
				}
			}
			// mtime looks stale but the real OS lock is still held (or the
			// probe was inconclusive), or the provably-dead lock's removal
			// itself failed — do NOT delete (or it's already gone from disk
			// in the success case above, which already `continue`d). Fall
			// through and display it like any other lock; its Pulse will
			// read "offline" so operators still see it's not heartbeating,
			// without risking a second process reclaiming a session that is
			// still alive.
		}

		pulseSec := int64(age.Seconds())
		pulse := lockPulseStatus(pulseSec)
		stale := pulse == "offline"

		// Call-tree pulse override: the lock mtime only ticks on real
		// activity the heartbeat goroutine's RecordActivity gate observed
		// (stream chunks — task #300), NOT on tool-call execution itself.
		// A session can be genuinely busy running ordinary top-level tool
		// calls (view, todos, ...) between model responses, with no stream
		// chunk arriving for well over the heartbeat's stale threshold —
		// the heartbeat mtime then reads "offline" for a live, working
		// session (task #321; observed live: PULSE_AGE == ELAPSED == 36s
		// with the process alive and real tool calls in progress).
		// computeCallTreeActivity's LatestUnix already covers the ROOT
		// session's own message activity (created_at/updated_at, bumped on
		// every tool-input-start/tool-call/tool-result), not just a
		// descendant sub-agent's — so this override no longer requires
		// act.SubAgentActive to apply. It's still used below to decide
		// whether to show a specific sub-agent's hash: a fresher signal
		// that came from the session's OWN activity, not a delegation,
		// gets no such label.
		var subAgentLabel string
		if act, fresher := lockCallTreeActivityLookup(callTreeActivities, stemIDs[sessionID], sessionID, info.ModTime().Unix()); fresher {
			if subAge, ok := act.Age(now); ok {
				pulseSec = int64(subAge.Seconds())
				pulse = lockPulseStatus(pulseSec)
				stale = pulse == "offline"
				if act.SubAgentActive {
					subAgentLabel = short(session.HashID(act.DeepestSessionID))
				}
			}
		}

		// session.ReadLockPID (not a raw os.ReadFile+Sscanf) so a live
		// holder's PID is still readable on Windows, where the holder's
		// mandatory LockFileEx range-lock can make the lock file's own
		// content unreadable to this process — see readLockFile's sidecar
		// fallback (internal/session/lock.go) and task #231.
		pid := session.ReadLockPID(lockPath)

		// Between turns (R6C-1): the lock is held only during a turn and
		// its file is truncated on release, so a loop waiting on a job
		// leaves an aging PID-less file that reads "offline". A live
		// verdict for the session (driver between turns, or a live
		// delegation) says the scope is open: not stale, not offline.
		var driverPID int
		betweenTurns := false
		if v, ok := liveVerdict(sessionID); ok {
			label, between := verdictPulse(pulse, v)
			if between {
				betweenTurns = true
				driverPID = int(v.PID)
			}
			if label != pulse {
				pulse, stale = label, false
			}
		}

		// Phase 0 stall marker (sessions_stall.go): stale heartbeat, live PID.
		stalled := !betweenTurns && pulseSec >= int64(stallPulseThreshold/time.Second) && pid > 0

		// Default view: collapse settled no-holder leftovers into the summary
		// line (Part of the FIX-LOCKS change). An explicit session argument
		// bypasses the collapse: the operator asked for that exact lock.
		if !removeFailed && lockIsIdle(pid, stale, betweenTurns, stalled) && !showAll && onlyStem == "" {
			idleHidden++
			continue
		}

		if staleOnly && !stale {
			continue
		}

		budgetSec := session.ReadLockTimeoutSec(lockPath)

		// Approximate acquire time: mtime when pulse was fresh.
		// For alive locks mtime ≈ now, so we use file birthtime via stat
		// if available; otherwise mtime is the best proxy.
		acqTime := info.ModTime().Unix()
		duration := int64(now.Sub(info.ModTime()).Seconds())

		locks = append(locks, lockItem{
			SessionID:   sessionID,
			PID:         pid,
			PulseSec:    pulseSec,
			Pulse:       pulse,
			AcquiredAt:  acqTime,
			DurationSec: duration,
			Stale:       stale,
			BudgetSec:   budgetSec,
			SubAgent:    subAgentLabel,

			BetweenTurns: betweenTurns,
			DriverPID:    driverPID,
			Stalled:      stalled,
		})
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		for _, lock := range locks {
			if err := enc.Encode(lock); err != nil {
				return err
			}
		}
		return enc.Encode(locksJSONEnvelope{Summary: locksJSONSummary{
			Total:      len(locks) + idleHidden,
			Shown:      len(locks),
			HiddenIdle: idleHidden,
		}})
	}

	if len(locks) == 0 {
		if len(args) == 1 {
			fmt.Printf("(no lock file for session %s)\n", args[0])
			return nil
		}
		if idleHidden > 0 {
			unit := "files"
			if idleHidden == 1 {
				unit = "file"
			}
			fmt.Printf("%d idle lock %s (no holder, no PID) hidden — use --all to list\n", idleHidden, unit)
			return nil
		}
		if staleOnly {
			fmt.Println("(no stale locks)")
		} else {
			fmt.Println("(no locks)")
		}
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION_ID\tPID\tPULSE\tPULSE_AGE\tELAPSED\tBUDGET\tSUB-AGENT")
	for _, lock := range locks {
		budget := "∞"
		if lock.BudgetSec > 0 {
			budget = formatDurationShort(time.Duration(lock.BudgetSec) * time.Second)
		}
		subAgent := "-"
		if lock.SubAgent != "" {
			subAgent = lock.SubAgent
		}
		pidCell, pulseCell := lock.PID, lock.Pulse
		if lock.BetweenTurns {
			if lock.DriverPID > 0 {
				pidCell = lock.DriverPID
				pulseCell = fmt.Sprintf("between turns (rush run PID %d)", lock.DriverPID)
			} else {
				pulseCell = "between turns (delegating)"
			}
		}
		if lock.Stalled {
			pulseCell = "STALLED " + formatDurationShort(time.Duration(lock.PulseSec)*time.Second)
		}
		fmt.Fprintf(
			tw, "%s\t%d\t%s\t%ds ago\t%s\t%s\t%s\n",
			truncate(lock.SessionID, 28),
			pidCell,
			pulseCell,
			lock.PulseSec,
			formatDurationShort(time.Duration(lock.DurationSec)*time.Second),
			budget,
			subAgent,
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if idleHidden > 0 {
		unit := "files"
		if idleHidden == 1 {
			unit = "file"
		}
		fmt.Printf("%d idle lock %s (no holder, no PID) hidden — use --all to list\n", idleHidden, unit)
	}
	return nil
}

// verdictPulse relabels a lock's mtime pulse from a live verdict (R-ACT: the
// classifier, not mtime, decides liveness). A released lock ages from the
// moment a turn ends, so a session waiting between turns used to read
// "ping"/"stopping" for 10-20 s -- as if its process were exiting. Between
// turns or delegating is always "between-turns"; an in-turn holder keeps its
// heartbeat pulse, except "offline", which the verdict proves wrong.
func verdictPulse(pulse string, v session.ActivityVerdict) (label string, betweenTurns bool) {
	if v.Kind == session.ActivityInTurn {
		if pulse == "offline" {
			return "alive", false
		}
		return pulse, false
	}
	return "between-turns", true
}

// kindIsLive reports whether a verdict Kind means the session's scope is
// open -- the one liveness question the locks/reap/prune paths ask.
func kindIsLive(k session.ActivityKind) bool {
	switch k {
	case session.ActivityInTurn, session.ActivityBetweenTurns, session.ActivityDelegating:
		return true
	}
	return false
}

// stemIDsForLocks is the id universe the locks view classifies: every
// top-level session plus every id carrying a driver marker (a deleted
// session's marker can outlive its row until the purge sweep).
func stemIDsForLocks(ctx context.Context, a *app.App) []string {
	seen := map[string]struct{}{}
	if sessions, err := a.Sessions.List(ctx); err == nil {
		for _, s := range sessions {
			seen[s.ID] = struct{}{}
		}
	}
	if drivers, err := a.LiveSessionDrivers(ctx); err == nil {
		for id := range drivers {
			seen[id] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}

// resolveLocksFilterStem turns the `sessions locks <id>` argument (a session
// id or hash prefix) into the lock-file stem to match. An argument that
// names no session is taken as a literal id or stem, so a lock whose
// session was deleted can still be inspected; an ambiguous prefix is an
// error.
func resolveLocksFilterStem(ctx context.Context, a *app.App, arg string) (string, error) {
	sess, err := resolveSessionID(ctx, a.Sessions, arg)
	switch {
	case err == nil:
		return session.SessionLockStem(sess.ID), nil
	case strings.HasPrefix(err.Error(), "session not found"):
		return session.SessionLockStem(arg), nil
	default:
		return "", err
	}
}

// lockCallTreeActivityLookup returns the freshest activity for a lock's real
// session IDs, or falls back to its stem when no ID is mapped.
func lockCallTreeActivityLookup(activities map[string]callTreeActivity, ids []string, stem string, baselineUnix int64) (callTreeActivity, bool) {
	if len(ids) == 0 {
		ids = []string{stem}
	}
	var best callTreeActivity
	found := false
	for _, id := range ids {
		act, ok := activities[id]
		if ok && act.LatestUnix > baselineUnix && (!found || act.LatestUnix > best.LatestUnix) {
			best, found = act, true
		}
	}
	return best, found
}
