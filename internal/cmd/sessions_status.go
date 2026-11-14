package cmd

// The `rush sessions status <id>` subcommand: one machine-callable snapshot
// answering, in a single read-only call, whether the session's process is
// still alive, what phase it is in, how long since its last real activity,
// whether it has live children, and whether its associated batch work is
// terminal.
//
// Strictly read-only: no lock is acquired, no database row is written, and
// nothing is pruned. The snapshot never claims more than its evidence
// proves -- any state the evidence could not establish is reported as the
// literal value "unknown", never as a guess. A recorded ended_reason is a
// database fact, not proof that a process physically exited, so
// process.alive stays "unknown" in that case; the "crashed" phase is only
// ever set from a real dead-PID or dead-host probe.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsStatusCmd = &cobra.Command{
	Use:   "status <id>",
	Short: "One snapshot: process liveness, phase, activity age, children, batch state — with honest unknowns",
	Long: `Print one snapshot of a session: is its process still alive, what
phase is it in, how long since real activity, does it have live
children, and is its batch work terminal.

Strictly READ-ONLY: this command acquires no lock, writes no database
row, and prunes nothing. It reads the session row (including the
recorded ended_reason), the session lock fact (a real OS liveness
probe of the recorded holder PID), run-driver markers plus host lock
probes, live batch roots through the shared batch live reader (the
same read "sessions why" and "sessions jobs" use), open wake
schedules, messages and tool calls, call-tree activity across
sub-agent descendants, pending child-question notices, and the
sub-session tree.

Honesty rule: any state the evidence could not establish is reported
as the literal value "unknown" -- never a guess. A recorded
ended_reason is a database fact, NOT proof that a process exited, so
process.alive stays "unknown" when the only end evidence is a recorded
reason or an absent lock. The "crashed" phase is deliberately part of
this command's vocabulary beyond the base five phases (turn,
between_turns, waiting, idle, finished): it is only ever set from a
real dead-PID or dead-host probe, so a script can assert on it without
re-deriving a crash from absence of evidence. Peak hours (an ended_reason
of peak_hours) renders as the paused phase, and an unknown ended_reason
stays the finished phase with its verbatim reason in exit_reason.

Phases: turn | between_turns | waiting | idle | finished | paused |
crashed | unknown.`,
	Args: cobra.ExactArgs(1),
	Example: `
# One snapshot of a session's process, phase, children and batch work
rush sessions status pr-42

# Machine-readable; assert on it from an orchestrator script
rush sessions status pr-42 --json | jq '.phase, .process.alive, .batch.terminal'

# Same, by hash prefix
rush sessions status 8a3f0c --json
  `,
	RunE: sessionsStatusCmdRun,
}

// sessionStatusProcess is the process half of the snapshot. Alive is true
// only with live evidence, false only from a real probe result, and the
// literal string "unknown" when there is nothing to probe.
type sessionStatusProcess struct {
	Alive     any    `json:"alive"`      // true | false | "unknown"
	DriverPID *int64 `json:"driver_pid"` // nil when there is no run-driver marker
	Evidence  string `json:"evidence"`
}

// sessionStatusLastTool names the newest tool call and how long ago its
// message was persisted.
type sessionStatusLastTool struct {
	Name       string `json:"name"`
	AgeSeconds int64  `json:"age_seconds"`
}

// sessionStatusChildren summarizes the session's delegation tree. The
// nullable counts are nil exactly when the read behind them failed, so a
// failed read can never read as a fabricated zero.
type sessionStatusChildren struct {
	Live          int  `json:"live"`           // live delegation rows in the tree
	Total         *int `json:"total"`          // distinct descendant sessions ever; nil when the walk failed
	HeldQuestions *int `json:"held_questions"` // nil when the notice read failed
}

// sessionStatusBatchRoot is one batch root of the session: identity, the
// observed state (the why renderer's vocabulary, cancelling and
// disappeared included), and whether it is nonterminal held work.
type sessionStatusBatchRoot struct {
	RootID string `json:"root_id"`
	State  string `json:"state"`
	Held   bool   `json:"held"`
}

// sessionStatusBatch answers "is the batch work terminal" with the same
// conservative tri-state as the process block: an incomplete read is the
// literal "unknown", never proof of completion.
type sessionStatusBatch struct {
	Terminal any                      `json:"terminal"` // true | false | "unknown"
	Roots    []sessionStatusBatchRoot `json:"roots"`
}

// sessionStatusReport is the snapshot: the JSON wire shape of
// `rush sessions status <id> --json` and the struct the text renderer reads.
type sessionStatusReport struct {
	SessionID            string                 `json:"session_id"`
	Phase                string                 `json:"phase"`
	Process              sessionStatusProcess   `json:"process"`
	LastTool             *sessionStatusLastTool `json:"last_tool"`
	SecondsSinceActivity *int64                 `json:"seconds_since_activity"`
	Children             sessionStatusChildren  `json:"children"`
	Batch                sessionStatusBatch     `json:"batch"`
	ExitReason           string                 `json:"exit_reason,omitempty"`
	Sources              []string               `json:"sources"`
	Notes                []string               `json:"notes,omitempty"`
}

func sessionsStatusCmdRun(cmd *cobra.Command, args []string) error {
	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sess, err := resolveSessionID(cmd.Context(), a.Sessions, args[0])
	if err != nil {
		return err
	}

	rep, err := sessionsStatusSnapshot(cmd.Context(), a, a.Config().Options.DataDirectory, sess.ID, time.Now())
	if err != nil {
		return err
	}
	asJSON, err := cmd.Flags().GetBool("json")
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(rep)
	}
	renderSessionsStatusReport(os.Stdout, rep)
	return nil
}

// sessionsStatusSnapshot is the testable core of `rush sessions status`:
// it takes the app services, the resolved data directory (for the locks
// dir), the session id, and a clock (now), so tests can drive it with a
// hand-built *app.App and a t.TempDir() without spinning up cobra.
//
// It must tolerate a nil a.Messages and a nil or absent a.DB (a configless
// test App): every degraded read becomes an unknown plus a note, never a
// panic and never a fabricated fact. The returned error is always nil — a
// failed read is a reported unknown, not a command failure.
func sessionsStatusSnapshot(ctx context.Context, a *app.App, dataDir, sessionID string, now time.Time) (sessionStatusReport, error) {
	rep := sessionStatusReport{SessionID: sessionID}
	var notes []string

	// Phase and facts: the shared session-activity reader collects the lock,
	// driver and live-work facts in one batched pass. A failed read degrades
	// to "unknown" with a note; it never fails the snapshot.
	act, actErr := a.SessionActivity(ctx, sessionID)
	var facts session.ActivityFacts
	lockRead := false
	if actErr != nil {
		rep.Phase = "unknown"
		notes = append(notes, fmt.Sprintf("session activity read failed: %v", actErr))
		rep.Process = sessionStatusProcess{
			Alive:    "unknown",
			Evidence: fmt.Sprintf("session facts could not be read: %v", actErr),
		}
	} else {
		facts = act.Facts
		lockRead = true
		rep.Phase = sessionsStatusPhase(act.Verdict)
		rep.Process = sessionsStatusProcessBlock(facts, &notes)
	}

	// Last tool: the newest ToolCall part across the session's messages.
	lastTool, msgsRead := sessionsStatusLastTool(ctx, a, sessionID, now, &notes)
	rep.LastTool = lastTool

	// Freshness: the freshest of the call-tree activity (sub-agent aware)
	// and the session lock heartbeat mtime. The field never names the
	// winner; the contributing snapshots land in sources instead.
	tree := callTreeActivity{}
	if a.Sessions != nil {
		tree = computeCallTreeActivity(ctx, a, sessionID)
	}
	var quiet *int64
	if tree.LatestUnix > 0 {
		age := int64(now.Sub(time.Unix(tree.LatestUnix, 0)).Seconds())
		quiet = &age
	}
	pulse, pulseFound := lockPulseAge(dataDir, sessionID, now)
	if pulseFound {
		age := int64(pulse.Seconds())
		if quiet == nil || age < *quiet {
			quiet = &age
		}
	}
	rep.SecondsSinceActivity = quiet

	// Children: live delegation rows from the facts, the pending
	// child-question count from the notice store, and the distinct
	// descendant count from a bounded breadth-first walk.
	children := sessionStatusChildren{Live: facts.LiveDelegations}
	questionsRead := false
	if reader := sessionQuestionReaderForApp(a); reader == nil {
		notes = append(notes, "child question reader unavailable (no database handle)")
	} else if qs, err := reader.PendingChildQuestions(ctx, sessionID); err != nil {
		notes = append(notes, fmt.Sprintf("child question read failed: %v", err))
	} else {
		n := len(qs)
		children.HeldQuestions = &n
		questionsRead = true
	}
	walkOK := false
	if a.Sessions == nil {
		notes = append(notes, "sub-session walk failed: no session service")
	} else if total, err := sessionsStatusSubSessionTotal(ctx, a.Sessions, sessionID); err != nil {
		notes = append(notes, fmt.Sprintf("sub-session walk failed: %v", err))
	} else {
		children.Total = &total
		walkOK = true
	}
	rep.Children = children

	// Batch: the exact same live read `sessions why` and `sessions jobs`
	// use. Unknown beats live beats terminal, and a configless App (no
	// database handle) is an explicit unknown, never "no work".
	batchFacts := batchRuntimeFacts(ctx, a, dataDir, []string{sessionID})
	f := batchFacts[sessionID]
	batch := sessionStatusBatch{Roots: []sessionStatusBatchRoot{}}
	if a.DB == nil {
		notes = append(notes, "batch facts unavailable (no database handle)")
	}
	switch {
	case f.Unknown:
		batch.Terminal = "unknown"
		notes = append(notes, "batch read incomplete; live batch work unknown")
	case f.Live:
		batch.Terminal = false
	default:
		batch.Terminal = true
	}
	for _, r := range f.Roots {
		batch.Roots = append(batch.Roots, sessionStatusBatchRoot{RootID: r.RootID, State: r.State, Held: r.Held})
	}
	rep.Batch = batch

	// Exit reason: a recorded database fact only. It never feeds the
	// process verdict; EndUnreadable is a note, never an end fact.
	rep.ExitReason = facts.EndedReason
	if facts.EndUnreadable {
		notes = append(notes, "ended reason could not be read")
	}

	rep.Sources = sessionsStatusSources(sessionStatusSourceFlags{
		LockRead:        lockRead,
		DriverSeen:      facts.Driver != nil || facts.DeadHostDriver,
		BatchNodesRead:  a.DB != nil,
		HasSchedules:    len(facts.OpenSchedules) > 0,
		PulseFound:      pulseFound,
		MessagesRead:    msgsRead,
		TreeHasActivity: tree.LatestUnix > 0,
		QuestionsRead:   questionsRead,
		WalkOK:          walkOK,
	})
	rep.Notes = notes
	return rep, nil
}

// sessionsStatusSourceFlags carries the per-source contribution facts the
// snapshot gathered, so the sources list can be assembled in one fixed
// order: only sources that actually contributed evidence appear, and the
// order is stable for script diffing.
type sessionStatusSourceFlags struct {
	LockRead        bool
	DriverSeen      bool
	BatchNodesRead  bool
	HasSchedules    bool
	PulseFound      bool
	MessagesRead    bool
	TreeHasActivity bool
	QuestionsRead   bool
	WalkOK          bool
}

// sessionsStatusSources assembles the sources list in the fixed order the
// command documents.
func sessionsStatusSources(f sessionStatusSourceFlags) []string {
	out := []string{"session_db"}
	if f.LockRead {
		out = append(out, "session_lock")
	}
	if f.DriverSeen {
		out = append(out, "driver_markers")
	}
	if f.BatchNodesRead {
		out = append(out, "batch_nodes")
	}
	if f.HasSchedules {
		out = append(out, "wake_schedules")
	}
	if f.PulseFound {
		out = append(out, "heartbeat")
	}
	if f.MessagesRead {
		out = append(out, "messages")
	}
	if f.TreeHasActivity {
		out = append(out, "call_tree_activity")
	}
	if f.QuestionsRead {
		out = append(out, "child_questions")
	}
	if f.WalkOK {
		out = append(out, "sub_sessions")
	}
	return out
}

// sessionsStatusPhase maps the shared classifier's verdict onto the
// command's phase vocabulary. "crashed" is kept deliberately: the
// classifier only produces it from a real dead-PID or dead-host probe, and
// a script should not have to re-derive a crash from absence of evidence.
// "paused" is issued ONLY for the ended_reason peak_hours; "finished"
// stays for every other recorded reason, including ones this binary does
// not know (the reason is visible verbatim in exit_reason). An
// unrecognized kind (a future verdict) maps to "unknown".
func sessionsStatusPhase(v session.ActivityVerdict) string {
	switch v.Kind {
	case session.ActivityInTurn:
		return "turn"
	case session.ActivityBetweenTurns:
		return "between_turns"
	case session.ActivityDelegating:
		return "waiting"
	case session.ActivityEnded:
		if v.EndedReason == endedReasonPeakHours {
			return "paused"
		}
		return "finished"
	case session.ActivityIdle:
		return "idle"
	case session.ActivityCrashed:
		return "crashed"
	}
	return "unknown"
}

// sessionsStatusProcessBlock decides the process block from already-read
// facts. Alive is true only on live evidence (a live run-driver marker
// whose host lock probe passed, or a held session lock whose recorded PID
// passed a real OS liveness probe), false only from a real probe result (a
// dead recorded PID, or a driver marker naming a dead host), and the
// literal string "unknown" whenever there is nothing to probe: an absent,
// released or unreadable lock plus no driver marker proves nothing about
// physical process exit, and a recorded ended_reason is a database fact,
// not an exit proof.
func sessionsStatusProcessBlock(f session.ActivityFacts, notes *[]string) sessionStatusProcess {
	p := sessionStatusProcess{Alive: "unknown"}
	switch {
	case f.Driver != nil:
		pid := f.Driver.PID
		p.Alive = true
		p.DriverPID = &pid
		p.Evidence = fmt.Sprintf(
			"rush run driver marker: PID %d on host %s, host lock probe says the host is alive",
			pid, short(f.Driver.HostID))
	case f.Lock.Kind == session.LockHeld:
		p.Alive = true
		p.Evidence = fmt.Sprintf(
			"session lock held by PID %d; OS liveness probe says the holder process is alive",
			f.Lock.PID)
	case f.Lock.Kind == session.LockDead:
		p.Alive = false
		p.Evidence = fmt.Sprintf(
			"session lock records PID %d; OS liveness probe says that process is not alive",
			f.Lock.PID)
	case f.DeadHostDriver:
		p.Alive = false
		p.Evidence = "a driver marker names a host whose lock probe says it is dead"
	default:
		ev := "no live-process evidence to probe: the session lock is absent or released and no driver marker exists"
		if f.EndedReason != "" {
			ev += fmt.Sprintf(
				"; ended_reason %q is a recorded database fact, not proof that a process exited",
				f.EndedReason)
		}
		if f.Lock.Kind == session.LockUnknown {
			ev += "; session lock state could not be read (fail-open)"
			*notes = append(*notes, "session lock state could not be read (fail-open)")
		}
		p.Evidence = ev
	}
	for _, u := range f.Unreadable {
		*notes = append(*notes, fmt.Sprintf("live-work fact could not be read: %s", u))
	}
	return p
}

// sessionsStatusLastTool scans the session's messages for the newest
// ToolCall part: message recency is latestMessageUnix (created/updated,
// the timestamp that moves during an in-flight turn), and within a
// message the last call in part order wins. A nil message service or a
// failed read yields nil plus a note; a session that never made a tool
// call yields nil with no note (verifiable absence, not an error). The
// second return value reports whether the message read succeeded at all.
func sessionsStatusLastTool(ctx context.Context, a *app.App, sessionID string, now time.Time, notes *[]string) (*sessionStatusLastTool, bool) {
	if a.Messages == nil {
		*notes = append(*notes, "messages could not be read: no message service")
		return nil, false
	}
	msgs, err := a.Messages.List(ctx, sessionID)
	if err != nil {
		*notes = append(*notes, fmt.Sprintf("messages could not be read: %v", err))
		return nil, false
	}
	var newestUnix int64
	var name string
	found := false
	for i := range msgs {
		calls := msgs[i].ToolCalls()
		if len(calls) == 0 {
			continue
		}
		ts := latestMessageUnix(msgs[i])
		if !found || ts >= newestUnix {
			found = true
			newestUnix = ts
			name = calls[len(calls)-1].Name
		}
	}
	if !found {
		return nil, true
	}
	age := int64(now.Sub(time.Unix(newestUnix, 0)).Seconds())
	return &sessionStatusLastTool{Name: name, AgeSeconds: age}, true
}

// sessionsStatusSubSessionTotal counts the DISTINCT descendant sessions
// reachable from rootID through parent-child links: a breadth-first walk
// with a visited set, one ListSubSessions per level, and a depth cap
// mirroring the watch walk's cycle guard, so an accidental parent/child
// cycle in the data must terminate. A level read error aborts the walk
// with the error so the caller can report what it knows instead of a
// fabricated count.
func sessionsStatusSubSessionTotal(ctx context.Context, svc session.Service, rootID string) (int, error) {
	const maxDepth = 16
	total := 0
	seen := map[string]bool{rootID: true}
	frontier := []string{rootID}
	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, parent := range frontier {
			children, err := svc.ListSubSessions(ctx, parent)
			if err != nil {
				return 0, err
			}
			for _, ch := range children {
				if seen[ch.ID] {
					continue
				}
				seen[ch.ID] = true
				total++
				next = append(next, ch.ID)
			}
		}
		frontier = next
	}
	return total, nil
}

// aliveWord renders the tri-state process verdict for the text output.
func aliveWord(alive any) string {
	if v, ok := alive.(bool); ok {
		if v {
			return "alive"
		}
		return "dead"
	}
	return "unknown"
}

// batchWord renders the tri-state batch verdict for the text output.
func batchWord(terminal any) string {
	if v, ok := terminal.(bool); ok {
		if v {
			return "terminal"
		}
		return "live"
	}
	return "unknown"
}

// intOrUnknown renders a nullable count: "(unknown)" when the read behind
// it failed, the number otherwise.
func intOrUnknown(p *int) string {
	if p == nil {
		return "(unknown)"
	}
	return strconv.Itoa(*p)
}

// renderSessionsStatusReport writes the plain-text rendering of a snapshot
// to w. It takes an io.Writer so tests can drive it without cobra or a
// captured stdout.
func renderSessionsStatusReport(w io.Writer, rep sessionStatusReport) {
	fmt.Fprintf(w, "session: %s (hash %s)\n", rep.SessionID, short(session.HashID(rep.SessionID)))
	fmt.Fprintf(w, "phase: %s\n", rep.Phase)
	fmt.Fprintf(w, "process: %s — %s\n", aliveWord(rep.Process.Alive), rep.Process.Evidence)
	if rep.Process.DriverPID != nil {
		fmt.Fprintf(w, "driver pid: %d\n", *rep.Process.DriverPID)
	} else {
		fmt.Fprintln(w, "driver pid: (none)")
	}
	if rep.LastTool != nil {
		fmt.Fprintf(w, "last tool: %s %s ago\n",
			rep.LastTool.Name, formatDurationShort(time.Duration(rep.LastTool.AgeSeconds)*time.Second))
	} else {
		fmt.Fprintln(w, "last tool: (none)")
	}
	if rep.SecondsSinceActivity != nil {
		fmt.Fprintf(w, "quiet for: %ds\n", *rep.SecondsSinceActivity)
	} else {
		fmt.Fprintln(w, "quiet for: (no activity evidence)")
	}
	fmt.Fprintf(w, "children: live %d, total %s, held questions %s\n",
		rep.Children.Live, intOrUnknown(rep.Children.Total), intOrUnknown(rep.Children.HeldQuestions))
	fmt.Fprintf(w, "batch: %s — %d root(s)", batchWord(rep.Batch.Terminal), len(rep.Batch.Roots))
	if len(rep.Batch.Roots) > 0 {
		parts := make([]string, 0, len(rep.Batch.Roots))
		for _, r := range rep.Batch.Roots {
			state := r.State
			if r.Held {
				state += " (held)"
			}
			parts = append(parts, fmt.Sprintf("root %s %s", short(r.RootID), state))
		}
		fmt.Fprintf(w, "; %s", strings.Join(parts, ", "))
	}
	fmt.Fprintln(w)
	if rep.ExitReason != "" {
		if label := endedStatusLabel(rep.ExitReason); label != rep.ExitReason {
			fmt.Fprintf(w, "exit reason: %s (%s)\n", rep.ExitReason, label)
		} else {
			fmt.Fprintf(w, "exit reason: %s\n", rep.ExitReason)
		}
	} else {
		fmt.Fprintln(w, "exit reason: (none)")
	}
	fmt.Fprintf(w, "sources: %s\n", strings.Join(rep.Sources, ", "))
	if len(rep.Notes) == 0 {
		fmt.Fprintln(w, "notes: (none)")
		return
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
}
