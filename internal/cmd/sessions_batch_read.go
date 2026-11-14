package cmd

// Batch read side of the four session status commands (R12-B part 1):
// `sessions jobs/why/show/list` read live batch node
// rows through the R10c session read APIs (BatchLiveReader +
// BatchHistorySnapshot). Reads are conservative
// (R10a rule): an incomplete or failed read is reported as UNKNOWN, never as
// absence -- a command must never print "idle"/no-work while a read is
// incomplete. A cancelling root is held, nonterminal work: "stopped but
// awaiting termination".

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// batchRootFact is one root incarnation's read-side summary: identity, the
// observed state, the held/termination verdict, and the delivery/debt facts
// from the history snapshot where available.
type batchRootFact struct {
	Owner, RootID, ToolCallID string
	State                     string
	Held                      bool // pending | starting | running | cancelling
	Cancelling                bool
	AwaitingTermination       bool   // cancelling: stopped but awaiting termination
	AwaitingAnswer            bool   // a held child_question notice is bound to this root
	QuestionChildID           string // parsed from the bound notice's frame, when held
	QuestionText              string
	Debt                      int64
	Tasks                     session.BatchLiveCounts
	Unknown                   bool // this root's own read was incomplete or failed
}

// batchSessionFacts aggregates one session's root facts. Live or Unknown must
// keep every consumer from reporting idle/at rest (conservative-read rule).
type batchSessionFacts struct {
	Roots   []batchRootFact
	Live    bool // at least one held root (or an unclassifiable one)
	Unknown bool // any read in the tree was incomplete or errored
	Debt    int64
	Tasks   session.BatchLiveCounts
	// GuardDebt is the pending-inclusive delivery debt (terminal roots'
	// selected unreacted notices plus retained session_notices rows) that
	// protects the session from gc/compact; Debt stays the display count.
	GuardDebt   bool
	GuardDebtN  int
	HasQuestion bool // a held child_question notice awaits an answer
	// QuestionChildID/QuestionText are the parsed bound notice's frame (the
	// first root that carries one); empty when only the session-level
	// fallback flagged the question.
	QuestionChildID string
	QuestionText    string
}

// awaitingAnswer reports whether any held batch question (session-level
// fallback or a root's bound held question) awaits an answer.
func (f batchSessionFacts) awaitingAnswer() bool {
	if f.HasQuestion {
		return true
	}
	for _, r := range f.Roots {
		if r.AwaitingAnswer {
			return true
		}
	}
	return false
}

// renderBatchQuestionNote prints the awaiting-answer section of `sessions
// why`: the generic conservative note plus, when the bound notice's frame
// parsed, the child that asked and how the orchestrating agent answers it
// in-process via the inject_agent tool (no operator relay line).
func renderBatchQuestionNote(f batchSessionFacts, out io.Writer) {
	if !f.HasQuestion {
		return
	}
	fmt.Fprintln(out, "note: a held batch question is awaiting an answer")
	if f.QuestionChildID == "" && f.QuestionText == "" {
		return
	}
	fmt.Fprintf(out, "  waiting for your answer: child %s asked: %s\n", f.QuestionChildID, f.QuestionText)
	fmt.Fprintf(out, "  answer: the orchestrating agent must call `inject_agent` with child_session_id=%q and its answer as message.\n", f.QuestionChildID)
}

func (f *batchSessionFacts) addRoot(r batchRootFact) {
	f.Roots = append(f.Roots, r)
	if r.QuestionChildID != "" && f.QuestionChildID == "" {
		f.QuestionChildID = r.QuestionChildID
		f.QuestionText = r.QuestionText
	}
	f.Debt += r.Debt
	f.Tasks.Pending += r.Tasks.Pending
	f.Tasks.Starting += r.Tasks.Starting
	f.Tasks.Running += r.Tasks.Running
	f.Tasks.Cancelling += r.Tasks.Cancelling
	f.Tasks.Active += r.Tasks.Active
	f.Tasks.Completed += r.Tasks.Completed
	f.Tasks.Failed += r.Tasks.Failed
	f.Tasks.Cancelled += r.Tasks.Cancelled
	f.Tasks.TimedOut += r.Tasks.TimedOut
	f.Tasks.Interrupted += r.Tasks.Interrupted
	f.Tasks.Skipped += r.Tasks.Skipped
	if r.Held || r.Unknown {
		f.Live = true
	}
	if r.Unknown {
		f.Unknown = true
	}
}

// batchRootHeld reports whether a root state is nonterminal. Cancelling is
// held: the projection stays nonterminal until the confirmed termination
// report, so a cancelling root is "stopped but awaiting termination".
func batchRootHeld(state string) bool {
	switch state {
	case "pending", "starting", "running", "cancelling":
		return true
	}
	return false
}

// batchStatusAnnotation is the shared conservative suffix every command
// appends to its status word: "(batch live)" for held batch work, "(batch
// unknown)" when the read was incomplete -- never a silent blank.
func batchStatusAnnotation(f batchSessionFacts) string {
	switch {
	case f.Unknown:
		return " (batch unknown)"
	case f.Live:
		return " (batch live)"
	}
	return ""
}

// reduceBatchSessionFacts reads the given sessions' live batch roots through
// the R10a live reader and the R10c history snapshots (delivery/debt per
// root). A nil reader or source, a read error, or an incomplete read marks
// the session Unknown instead of empty. A non-nil q adds the per-root bound
// question check (session_notices by tool call id); nil only skips that
// check, the conservative verdicts still stand.
func reduceBatchSessionFacts(ctx context.Context, reader *session.BatchLiveReader, store *session.BatchStore, q *db.Queries, ids []string) map[string]batchSessionFacts {
	out := make(map[string]batchSessionFacts, len(ids))
	if reader == nil || reader.Source == nil || store == nil || ctx == nil {
		for _, id := range ids {
			out[id] = batchSessionFacts{Unknown: true, Live: true}
		}
		return out
	}
	live := reader.BatchLiveForSessions(ctx, ids)
	for _, id := range ids {
		var f batchSessionFacts
		res := live[id]
		if res.Err != nil || !res.Complete {
			f.Unknown = true
			if len(res.Tree) == 0 {
				// An incomplete read with no roots must never read as idle.
				f.Live = true
			}
		}
		for _, root := range res.Tree {
			rf := batchRootFact{
				Owner:      root.Ref.OwnerSessionID,
				RootID:     root.Ref.RootID,
				ToolCallID: root.ToolCallID,
				State:      root.State,
				Tasks:      root.Counts,
			}
			rf.Held = batchRootHeld(rf.State)
			rf.Cancelling = rf.State == "cancelling"
			rf.AwaitingTermination = rf.Cancelling
			snap, err := store.BatchHistorySnapshot(ctx, reader, rf.Owner, rf.RootID)
			switch {
			case err != nil:
				f.Unknown = true
				rf.Unknown = true
			default:
				if !snap.Complete {
					f.Unknown = true
					rf.Unknown = true
				}
				rf.Debt = snap.Debt
				if snap.Disappeared {
					// Deletion is not a confirmed termination fact: still live.
					rf.State = "disappeared"
					rf.Held = true
				}
			}
			// Bound held question: the R10c query names the notices bound to
			// THIS root's tool call, so a question fact stays per-root.
			if q != nil {
				notices, nerr := q.BatchListBoundNoticesByToolCallID(ctx, db.BatchListBoundNoticesByToolCallIDParams{
					Owner: rf.Owner, ToolCallID: sql.NullString{String: rf.ToolCallID, Valid: true},
				})
				if nerr != nil {
					f.Unknown = true
					f.Live = true
				} else {
					for _, n := range notices {
						if n.Kind == session.NoticeKindChildQuestion && n.Wake == 1 && n.Reacted == 0 &&
							(n.Delivery == "pending" || n.Delivery == "done") {
							rf.AwaitingAnswer = true
							f.HasQuestion = true
							// The notice text embeds the canonical question frame;
							// parse it once here so every surface can quote it.
							if q, ok := session.ParseChildQuestionNotice(n.Text, rf.ToolCallID); ok {
								rf.QuestionChildID = q.ChildSessionID
								rf.QuestionText = q.Question
							}
						}
					}
				}
			}
			f.addRoot(rf)
		}
		// Pending-inclusive delivery debt (terminal roots included) and held
		// questions guard the session from gc/compact even at rest. A debt
		// read error is conservative unknown; skipped on an incomplete live
		// read, which is already unknown (the guard reasons on that first).
		if res.Err == nil && res.Complete {
			debt, debtErr := store.BatchReactionPendingInclusiveDebtRows(ctx, id)
			if debtErr != nil {
				f.Unknown = true
				f.Live = true
			} else {
				f.GuardDebtN = len(debt.Nodes) + len(debt.Notices)
				f.GuardDebt = f.GuardDebtN > 0
				// Conservative catch-all for questions whose bound root is not
				// visible in the live tree: a debt row of kind child_question
				// still flags the session-level question fact (the per-root
				// bound check above is the precise signal; this is the fallback).
				for _, n := range debt.Notices {
					if n.Kind == session.NoticeKindChildQuestion {
						f.HasQuestion = true
					}
				}
				// Batch-published questions are unbound (job_tool_call_id NULL),
				// so the per-root query above never sees them: quote the asking
				// child from the pending notice's batch frame.
				if f.HasQuestion && f.QuestionChildID == "" && q != nil {
					fillBatchQuestionFromPending(ctx, q, id, &f)
				}
			}
		}
		out[id] = f
	}
	return out
}

// unavailableBatchFacts marks every requested id with explicit unknown facts
// (Unknown + Live): a configless App's batch facts are UNAVAILABLE — a
// conservative unknown — never "no live work". Consumers must refuse to
// report idle.
func unavailableBatchFacts(ids []string) map[string]batchSessionFacts {
	out := make(map[string]batchSessionFacts, len(ids))
	for _, id := range ids {
		out[id] = batchSessionFacts{Unknown: true, Live: true}
	}
	return out
}

// batchRuntimeFacts is the production entry point. A nil App or a nil/absent
// database handle → explicit unknown facts from
// unavailableBatchFacts — never nil, never a panic, and never an "at rest"
// verdict on unreadable evidence. Otherwise the live batch facts of the
// requested sessions.
func batchRuntimeFacts(ctx context.Context, a *app.App, dataDir string, ids []string) map[string]batchSessionFacts {
	if a == nil || a.DB == nil {
		return unavailableBatchFacts(ids)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	conn := a.DB()
	if conn == nil {
		return unavailableBatchFacts(ids)
	}
	reader := session.NewBatchLiveReader(db.New(conn), dataDir)
	return reduceBatchSessionFacts(ctx, reader, session.NewBatchStore(conn), db.New(conn), ids)
}

// renderBatchRootsForWhy renders the `sessions why` batch section from
// already-read facts: one line per root (identity, state, held/termination
// verdict, debt), then the conservative unknown line when the read was
// incomplete.
func renderBatchRootsForWhy(f batchSessionFacts, out io.Writer) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Batch roots:")
	if len(f.Roots) == 0 {
		if f.Unknown {
			fmt.Fprintln(out, "  (batch read incomplete; live batch work unknown)")
		} else {
			fmt.Fprintln(out, "  (none)")
		}
		return
	}
	for _, r := range f.Roots {
		verdict := r.State
		switch {
		case r.Unknown:
			verdict = "unknown (incomplete read)"
		case r.AwaitingTermination:
			verdict = r.State + " (stopped but awaiting termination)"
		case r.Held:
			verdict = r.State + " (held)"
		}
		awaiting := ""
		if r.AwaitingAnswer {
			awaiting = ", awaiting delivery/answer"
		}
		fmt.Fprintf(out, "  root %s tool call %s (owner %s): %s, debt %d, tasks %d pending/%d running/%d done%s\n",
			short(r.RootID), r.ToolCallID, short(session.HashID(r.Owner)), verdict, r.Debt,
			r.Tasks.Pending, r.Tasks.Active, r.Tasks.Completed+r.Tasks.Failed+r.Tasks.Cancelled+r.Tasks.TimedOut+r.Tasks.Interrupted+r.Tasks.Skipped, awaiting)
	}
	if f.Unknown {
		fmt.Fprintln(out, "  note: the batch read was incomplete; treat live batch work as unknown")
	}
}

// explainWhyBatch is the `sessions why` body: batch facts plus the
// session lock fact decide the status; no legacy async_jobs read runs.
func explainWhyBatch(ctx context.Context, a *app.App, dataDir, sessionID string, out io.Writer) error {
	facts := batchRuntimeFacts(ctx, a, dataDir, []string{sessionID})
	f := facts[sessionID]
	activity, activityErr := a.SessionActivity(ctx, sessionID)
	if activityErr != nil {
		return fmt.Errorf("failed to classify session %s: %w", sessionID, activityErr)
	}
	lock := activity.Facts.Lock
	// The message read feeds both the honest at-rest reason below and the
	// "Last assistant message" section -- one read, no second List.
	msgs, msgsErr := a.Messages.List(ctx, sessionID)
	lastFinish := ""
	if msgsErr == nil {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == message.Assistant {
				if fin := msgs[i].FinishPart(); fin != nil {
					lastFinish = string(fin.Reason)
				}
				break
			}
		}
	}
	label := sessionStatusLabel(activity.Verdict, activity.Facts.EndedReason)
	status, reason := "at rest", "no live batch work"
	switch {
	case f.Unknown:
		status = "batch unknown (incomplete read)"
		reason = "the batch read was incomplete; live batch work is unknown"
	case f.Live:
		status = "batch live"
		for _, r := range f.Roots {
			if r.Held {
				reason = fmt.Sprintf("batch root %s is %s (held work)", short(r.RootID), r.State)
				break
			}
		}
	case lock.Kind == session.LockHeld:
		status = annotateRecordedEnd("running", activity.Facts.EndedReason)
		reason = "the session lock is held by a live process"
	case label != "":
		status = label
		reason = activity.Verdict.Description
	default:
		reason = activity.Verdict.Description
		// A finished (end_turn) assistant turn is deliberately NOT a
		// session-end signal (classifier decision 4), so the verdict stays
		// idle -- but the reason must explain the contradiction instead of
		// a bare "no recorded end" sentence (FIX-STATUS (b)).
		if lastFinish == string(message.FinishReasonEndTurn) {
			reason += "; the last assistant turn finished (end_turn) but no ended_reason is recorded: the run's exit write may have been dropped under database contention (see the rush log) or the loop was killed after the turn"
		}
	}
	if !f.Unknown && status != "at rest" && activity.Verdict.Kind != session.ActivityCrashed &&
		(activity.Verdict.CrashedPID > 0 || lock.Kind == session.LockDead) {
		status += " (stale lock)"
		if f.Live {
			// Live work outranks the dead recorded holder; say which PID the
			// stale annotation refers to.
			pid := activity.Verdict.CrashedPID
			if pid == 0 {
				pid = int64(lock.PID)
			}
			if pid > 0 {
				reason += fmt.Sprintf(" (the session lock records dead PID %d)", pid)
			}
		}
	}
	printStallHeader(ctx, a, dataDir, sessionID, status, lock, out)
	fmt.Fprintf(out, "reason: %s.\n", reason)
	renderBatchQuestionNote(f, out)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Last assistant message:")
	if msgsErr != nil {
		fmt.Fprintf(out, "  (could not read: %v)\n", msgsErr)
	} else {
		var last *message.Message
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == message.Assistant {
				last = &msgs[i]
				break
			}
		}
		if last == nil {
			fmt.Fprintln(out, "  (none)")
		} else {
			finish := last.FinishPart()
			fmt.Fprintf(out, "  finish_reason: %s\n", finishReasonOrUnknown(finish))
			// The legacy render surfaced a stored error finish's text so the
			// operator sees the cause; the batch render keeps that contract.
			if finish != nil && finish.Reason == message.FinishReasonError {
				errText := finish.Message
				if errText == "" {
					errText = "(error finish reason but no error text stored)"
				}
				fmt.Fprintf(out, "  error:         %s\n", errText)
			}
		}
	}
	if activity.Facts.Driver != nil {
		fmt.Fprintf(out, "driver: %s.\n", describeRunDriver(*activity.Facts.Driver, activity.Facts.DriverOwes))
	}
	// The facts are already read above; render them without a re-read.
	renderBatchRootsForWhy(f, out)
	return nil
}

// renderSessionsJobsBatch is the `sessions jobs` path: batch roots of
// the session and its delegation tree instead of async_jobs rows.
func renderSessionsJobsBatch(ctx context.Context, a *app.App, dataDir, sessionID string, asJSON bool) error {
	facts := batchRuntimeFacts(ctx, a, dataDir, []string{sessionID})
	f := facts[sessionID]
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		if len(f.Roots) == 0 {
			if f.Unknown {
				return enc.Encode(jobsJSONItem{Kind: "batch", State: "unknown", BatchUnknown: true, BatchAwaitingAnswer: f.HasQuestion})
			}
			return nil
		}
		for _, r := range f.Roots {
			item := jobsJSONItem{
				OwnerSessionID: r.Owner,
				ToolCallID:     r.ToolCallID,
				Kind:           "batch",
				State:          r.State,
				Delivery:       "none",
			}
			if r.Unknown {
				item.State = "unknown"
				item.BatchUnknown = true
			}
			item.BatchUnknown = item.BatchUnknown || f.Unknown
			if r.Held {
				item.Liveness = "held"
			}
			if r.Cancelling {
				item.Liveness = "held"
				item.Hint = "cancelling: stopped but awaiting termination"
			}
			item.BatchRootID = r.RootID
			item.BatchHeld = r.Held
			item.BatchCancelling = r.Cancelling
			item.BatchAwaitingTermination = r.AwaitingTermination
			// Per-root bound fact ONLY: the session-wide HasQuestion fallback is
			// not attributable to this root; the session-level question surface
			// is the summary line and the fallback paths.
			item.BatchAwaitingAnswer = r.AwaitingAnswer
			item.BatchDebt = r.Debt
			item.BatchTasksPending = r.Tasks.Pending
			item.BatchTasksActive = r.Tasks.Active
			item.BatchTasksDone = r.Tasks.Completed + r.Tasks.Failed + r.Tasks.Cancelled + r.Tasks.TimedOut + r.Tasks.Interrupted + r.Tasks.Skipped
			if err := enc.Encode(item); err != nil {
				return err
			}
		}
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tROOT\tTOOL_CALL_ID\tSTATE\tHELD\tAWAITING\tDEBT\tTASKS(p/a/d)")
	for _, r := range f.Roots {
		state := r.State
		if r.Unknown {
			state = "unknown"
		}
		awaiting := "-"
		switch {
		case r.AwaitingTermination:
			awaiting = "termination"
		case r.AwaitingAnswer:
			// Per-root only: a question bound to ANOTHER root's tool call must
			// not render this row as awaiting; the session-level fallback is
			// carried by the summary/note surfaces.
			awaiting = "answer"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\t%s\t%d\t%d/%d/%d\n",
			short(session.HashID(r.Owner)), short(r.RootID), r.ToolCallID, state,
			r.Held, awaiting, r.Debt, r.Tasks.Pending, r.Tasks.Active,
			r.Tasks.Completed+r.Tasks.Failed+r.Tasks.Cancelled+r.Tasks.TimedOut+r.Tasks.Interrupted+r.Tasks.Skipped)
	}
	w.Flush()
	if len(f.Roots) == 0 {
		if f.Unknown {
			fmt.Println("(batch read incomplete; live batch work unknown)")
		} else {
			// F8: the same empty-state line the legacy path prints (user-facing
			// continuity across the flag; no batch-specific wording).
			fmt.Println("(no jobs)")
		}
	}
	if f.Unknown {
		fmt.Fprintln(os.Stderr, "warning: the batch read was incomplete; some batch roots may be missing")
	}
	return nil
}

// fillBatchQuestionFromPending parses the owner's pending unbound
// child_question notices with the batch frame parser and records the first
// parsable one on the session-level facts. A read error or an unparsable
// frame leaves the generic note, never a guess.
func fillBatchQuestionFromPending(ctx context.Context, q *db.Queries, owner string, f *batchSessionFacts) {
	rows, err := q.ListPendingSessionNoticesForOwner(ctx, owner)
	if err != nil {
		return
	}
	for _, row := range rows {
		if row.Kind != session.NoticeKindChildQuestion || row.JobToolCallID.Valid {
			continue
		}
		if cq, ok := session.ParseBatchChildQuestionNotice(row.Text); ok {
			f.QuestionChildID = cq.ChildSessionID
			f.QuestionText = cq.Question
			return
		}
	}
}
