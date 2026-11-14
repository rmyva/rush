package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsWhyCmd = &cobra.Command{
	Use:   "why <id>",
	Short: "Explain why a session has the status it has",
	Long: `Print a one-shot diagnostic from the current batch-node read, the
session lock, and the shared session-activity classifier. It reads the
session's recorded ended_reason, session lock files (in the default layout,
.rush/locks), and batch roots with read-only probes of per-process host
locks under .rush/hosts by default. A host lock is evidence about a batch
worker process, not the owner's session lock. This command does not use the
legacy async-job ledger and does not read external logs or orchestrator
redirect output.

The shared activity view is also used by "sessions list", "watch" and
the web surfaces. Live work (a live run driver, an own running job, a live
delegation, an open one-shot wake schedule, or unreadable live-work
evidence) keeps a session active. A held batch root takes precedence in
this command as "batch live"; an incomplete batch read is "batch unknown",
not proof that no work exists.

Status guidance:
  done       — no crash or live-work fact takes precedence, and an
               ended_reason is recorded. A finish such as end_turn only
               finishes an assistant turn; it is not by itself a session-end
               or task-completion signal.
  paused     — the recorded ended_reason is peak_hours (a provider
               peak-hours pause); nothing is live and no crash fact
               applies. Rendered "paused: provider peak hours".
  crashed    — a dead session-lock PID or driver marker naming a
               dead host is recorded, and neither live work nor incomplete
               batch evidence takes precedence.
  running    — the session lock is held or the session-activity view finds
               live work between turns (for example, its driver, own job, or
               open wake schedule).
  delegating (sessions list) — a live batch delegation is active; this
               command reports its held root as "batch live". The worker's
               per-process host lock is distinct from the owner's session lock.
  batch unknown — the batch read was incomplete; live batch work is unknown.
  at rest    — no live work or incomplete batch evidence is found, no crash
               fact takes precedence, and no ended_reason is recorded.

An ended_reason outside this binary's known vocabulary renders verbatim
(never as done). A live status with a recorded ended_reason annotates it as
"(exit <reason>)" — the same exit fact "sessions status" reports. An "at
rest" verdict with a finished (end_turn) last assistant turn explains that
contradiction in its reason line instead of a bare no-end sentence.

A stale/dead recorded PID is only an annotation when live work exists; it
does not override that work. The last assistant finish_reason is shown as
context, but only ended_reason records that the run ended. Treat these as
a snapshot of available evidence; they do not read external logs.`,
	Args: cobra.ExactArgs(1),
	Example: `
# Why does sessions list show this one as crashed?
rush sessions why pr-42

# Same, by hash prefix
rush sessions why 8a3f0c
  `,
	RunE: sessionsWhyCmdRun,
}

func sessionsWhyCmdRun(cmd *cobra.Command, args []string) error {
	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sess, err := resolveSessionID(cmd.Context(), a.Sessions, args[0])
	if err != nil {
		return err
	}

	// explainSessionStatus's second-to-last string parameter is the root
	// whose "<root>/.rush/locks" subtree holds the session's lock file —
	// historically named cwd because setupApp's --data-dir-aware
	// resolution wasn't wired through here. Pass the already-resolved data
	// directory instead of the raw --cwd value so `sessions why` honors
	// --data-dir / a configured data_directory like `sessions list` /
	// `sessions locks` / `sessions watch` do (task #233 — same
	// cwd-hardcoding bug class as #219/#224/#231).
	return explainSessionStatus(cmd.Context(), a, a.Config().Options.DataDirectory, sess.ID, os.Stdout)
}

// explainSessionStatus writes a terse, plain-text explanation of why the
// session has the status it has. It is the testable core of
// `rush sessions why`: it takes the app services, the resolved data
// directory (for the locks dir), the session id, and an output writer, so
// tests can drive it with a hand-built *app.App and a t.TempDir() without
// spinning up cobra.
//
// dataDir is the rush data directory itself (e.g. what
// a.Config().Options.DataDirectory resolves to — honoring --data-dir / a
// configured data_directory), NOT the project cwd: the lock file lives at
// <dataDir>/locks/session-<id>.lock, one level shallower than the old
// <cwd>/.rush/locks layout this parameter used to assume. See task #233
// (same cwd-hardcoding bug class as #219/#224/#231): the caller used to
// pass the raw --cwd value here, ignoring --data-dir entirely.
//
// The whole explanation is built from the batch facts, the session lock fact
// and the shared session-activity classifier (explainWhyBatch); no legacy
// async_jobs read runs.
func explainSessionStatus(ctx context.Context, a *app.App, dataDir, sessionID string, out io.Writer) error {
	return explainWhyBatch(ctx, a, dataDir, sessionID, out)
}

// describeRunDriver renders the "why" clause naming the live `rush run` loop
// that drives a session, e.g.
//
//	"a `rush run` loop (PID 1234, host 8a3f0c2b alive) drives this session; a reaction is owed and it retries at its next opportunity"
func describeRunDriver(d session.SessionDriver, reactionOwed bool) string {
	clause := fmt.Sprintf("a `rush run` loop (PID %d, host %s %s) drives this session", d.PID, short(d.HostID), strings.ToLower(d.Status.String()))
	if reactionOwed {
		return clause + "; a reaction is owed and it retries at its next opportunity"
	}
	return clause + "; it waits for its scope to close"
}

// finishReasonOrUnknown returns the finish reason string, or "(unknown)"
// when there is no Finish part at all (message never finished).
func finishReasonOrUnknown(f *message.Finish) string {
	if f == nil || f.Reason == "" {
		return "(unknown)"
	}
	return string(f.Reason)
}
