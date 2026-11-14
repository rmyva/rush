package cmd

// The `sessions list` subcommand: table / NDJSON listing of top-level
// sessions. The STATUS column is the one session-activity classifier's
// verdict (App.SessionActivityBatch), the same verdicts `sessions why`
// prints -- there is no second status machinery here.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Args:  cobra.NoArgs,
	Short: "List all top-level sessions",
	Long: `List all top-level (non-child) sessions in this workspace.

Without --json the output is a fixed-width table; with --json each line is
one JSON object suitable for jq / streaming consumers.`,
	Example: `
# Human-readable table
rush sessions list

# Machine-readable (one object per line)
rush sessions list --json | jq 'select(.message_count > 0)'
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		sessions, err := a.Sessions.List(cmd.Context())
		if err != nil {
			return fmt.Errorf("failed to list sessions: %w", err)
		}

		// Filter out internal child sessions (sub-agents, title-generators).
		visible := sessions[:0]
		for _, s := range sessions {
			if s.ParentSessionID != "" {
				continue
			}
			visible = append(visible, s)
		}
		sessions = visible

		// Order by delegation-subtree activity (#1130): a busy child keeps
		// its root on top without the child ever writing the parent's
		// updated_at. A failed activity read degrades to the row's own
		// timestamp (the unsorted order is still valid output).
		orderBySubtreeActivity(cmd.Context(), a.Sessions, sessions)

		// One classifier for the whole list (R-ACT): every STATUS comes from
		// App.SessionActivityBatch -- the same verdicts `sessions why` prints --
		// with no per-session promotion layers. The verdict renders through
		// the one shared STATUS label (sessionStatusLabel,
		// sessions_status_label.go): in turn / between turns -> "running"
		// (live work), delegating -> "delegating", crashed -> "crashed"; an
		// ended session renders from its recorded ended_reason -- "done" for
		// the known vocabulary, "paused: provider peak hours" for
		// peak_hours, anything unknown verbatim (never "done"); idle ->
		// blank (at rest). A live status with a recorded ended_reason is
		// annotated "(exit <reason>)" so list surfaces the same exit fact
		// `sessions status` reports. In-process knowledge (the coordinator's parked-delegation
		// registry) is deliberately not consulted: a parked delegation keeps
		// its own live batch root, so another process sees the same
		// verdict (decision 9).
		ids := idsOf(sessions)
		batchFacts := batchRuntimeFacts(cmd.Context(), a, a.Config().Options.DataDirectory, ids)
		var statusByID map[string]string
		var awaitingByID map[string]session.ChildQuestion
		activities, actErr := a.SessionActivityBatch(cmd.Context(), ids)
		if actErr != nil {
			return fmt.Errorf("failed to classify sessions: %w", actErr)
		}
		statusByID, awaitingByID = listStatusesWithAwaiting(cmd.Context(), sessionQuestionReaderForApp(a), activities, ids)
		for _, id := range ids {
			bf := batchFacts[id]
			st := statusByID[id]
			if bf.Unknown {
				st += " (batch unknown)"
			} else if bf.Live {
				st += " (batch live)"
			}
			// A held batch question annotates the status with the
			// same "(awaiting answer)" substate the legacy path used to print.
			// The fallback also surfaces when the status is empty (at
			// rest), so a held question never renders as blank/at rest.
			if bf.HasQuestion {
				if st == "" {
					st = "batch question awaiting answer"
				} else if !strings.Contains(st, "awaiting answer") {
					st += " (awaiting answer)"
				}
			}
			statusByID[id] = st
		}

		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			for _, s := range sessions {
				item := makeSessionListItem(s, sessionBudget(cmd.Context(), a.Sessions, s))
				if st := statusByID[s.ID]; st != "" {
					item.Status = st
				}
				if q, ok := awaitingByID[s.ID]; ok {
					item.AwaitingAnswer = true
					item.AwaitingChild = q.ChildSessionID
					item.AwaitingQuestion = q.Question
				}
				if bf, ok := batchFacts[s.ID]; ok {
					// A held question with no visible root and a complete read
					// must still surface, so the answer flag sits outside the
					// roots/unknown guard (false in the common no-question
					// case, so the omitempty wire shape is unchanged).
					item.BatchAwaitingAnswer = bf.awaitingAnswer()
					if len(bf.Roots) > 0 || bf.Unknown {
						item.BatchRoots = len(bf.Roots)
						item.BatchDebt = bf.Debt
						if bf.Unknown {
							item.BatchStatus = "unknown"
						} else {
							item.BatchStatus = "live"
						}
					}
				}
				if err := enc.Encode(item); err != nil {
					return err
				}
			}
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "HASH\tID\tTITLE\tMSGS\tSTATUS\tUPDATED\tTOKENS\tCOST")
		for _, s := range sessions {
			fmt.Fprintf(
				tw, "%s\t%s\t%s\t%d\t%s\t%s\t%d\t$%.4f\n",
				short(session.HashID(s.ID)),
				s.ID,
				truncate(s.Title, 40),
				s.MessageCount,
				statusOrDash(statusByID[s.ID]),
				time.Unix(s.UpdatedAt, 0).Format("2006-01-02 15:04"),
				s.PromptTokens+s.CompletionTokens,
				// COST matches --json cost_usd: the subtree budget, delegated
				// spend included.
				sessionBudget(cmd.Context(), a.Sessions, s),
			)
		}
		return tw.Flush()
	},
}

type pendingChildQuestionReader interface {
	PendingChildQuestions(context.Context, string) ([]session.ChildQuestion, error)
}

func sessionQuestionReaderForApp(a *app.App) pendingChildQuestionReader {
	if a == nil {
		return nil
	}
	if a.DB == nil {
		return nil
	}
	conn := a.DB()
	if conn == nil {
		return nil
	}
	return session.NewSessionNoticeStore(conn)
}

// listStatusesWithAwaiting maps the classifier's verdicts to the STATUS
// column, adding the awaiting-answer substate (#1158): each listed root's
// pending child_question notices re-classify its verdict (read-only; a
// failed notice read leaves the plain verdict) and become the
// "(awaiting answer)" annotation plus the JSON wire fields.
func listStatusesWithAwaiting(ctx context.Context, store pendingChildQuestionReader, activities app.SessionActivityBatchResult, ids []string) (map[string]string, map[string]session.ChildQuestion) {
	questionsByRoot := map[string][]session.ChildQuestion{}
	if store != nil {
		for _, id := range ids {
			if qs, err := store.PendingChildQuestions(ctx, id); err == nil && len(qs) > 0 {
				questionsByRoot[id] = qs
			}
		}
	}
	statusByID := make(map[string]string, len(ids))
	awaitingByID := make(map[string]session.ChildQuestion, len(questionsByRoot))
	for id, act := range activities.ByID {
		v := act.Verdict
		if qs := questionsByRoot[id]; len(qs) > 0 {
			f := act.Facts
			f.ChildQuestions = qs
			v = session.ClassifySessionActivity(f)
			awaitingByID[id] = qs[0]
		}
		st := sessionStatusLabel(v, act.Facts.EndedReason)
		if _, ok := questionsByRoot[id]; ok && st != "" {
			st += " (awaiting answer)"
		}
		statusByID[id] = st
	}
	return statusByID, awaitingByID
}

// idsOf projects the session list to the id slice the batched
// classifier takes.
func idsOf(sessions []session.Session) []string {
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.ID
	}
	return ids
}

// listStatus maps the classifier's verdict to the STATUS-column vocabulary
// ("running" / "delegating" / "crashed" / "done"; "" = at rest) through the
// one shared label this file, `sessions why` and `sessions status` render
// from. The verdict-only shape carries no recorded end: callers holding the
// facts call sessionStatusLabel directly, so a live session's recorded exit
// is annotated instead of hidden (FIX-STATUS (a)).
func listStatus(v session.ActivityVerdict) string {
	return sessionStatusLabel(v, "")
}

func statusOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// sessionListItem is the JSON shape of `rush sessions list --json`. Held
// as a separate struct (rather than just marshalling session.Session
// directly) so the wire-stable field names don't drift if session.Session
// gains internal fields we don't want to publish.
type sessionListItem struct {
	ID           string  `json:"id"`
	Hash         string  `json:"hash"`
	Title        string  `json:"title"`
	MessageCount int64   `json:"message_count"`
	CreatedAt    int64   `json:"created_at"`
	UpdatedAt    int64   `json:"updated_at"`
	Tokens       int64   `json:"tokens"`
	CostUSD      float64 `json:"cost_usd"`
	YoloEnabled  bool    `json:"yolo_enabled"`
	// WorkspaceRoot/GitBranch are the WS-1 audit fields (#1142 step C): the
	// canonical checkout root the session is bound to and the branch that
	// checkout was on at creation. Empty for legacy (pre-binding) rows.
	// Additive to the JSON shape.
	WorkspaceRoot string `json:"workspace_root"`
	GitBranch     string `json:"git_branch"`
	// EndedReason is how the session's last run ended (its exit_reason);
	// empty while a run is in progress or when none ever ended.
	EndedReason string `json:"ended_reason,omitempty"`
	// Status is "running" (live work: in a turn or between turns),
	// "delegating" (a live sub-agent delegation), "crashed" (a crash fact,
	// no live work), "done" (a recorded ended_reason; "paused: provider
	// peak hours" for ended_reason peak_hours; an unknown ended_reason
	// renders verbatim) or absent (at rest). A live status with a recorded
	// ended_reason is annotated "(exit <reason>)" instead of hiding it.
	// Classified by the one session-activity classifier; omitempty keeps
	// the wire shape minimal for at-rest sessions.
	Status string `json:"status,omitempty"`
	// AwaitingAnswer marks a live session whose held delegation's child
	// paused on a question (#1158): additive to the wire shape, set only
	// together with AwaitingChild/AwaitingQuestion.
	AwaitingAnswer   bool   `json:"awaiting_answer,omitempty"`
	AwaitingChild    string `json:"awaiting_child,omitempty"`
	AwaitingQuestion string `json:"awaiting_question,omitempty"`
	// Batch read fields: BatchStatus is
	// "live" (held batch root) or "unknown" (incomplete/failed read).
	BatchRoots  int    `json:"batch_roots,omitempty"`
	BatchDebt   int64  `json:"batch_debt,omitempty"`
	BatchStatus string `json:"batch_status,omitempty"`
	// BatchAwaitingAnswer marks a held batch question (or undelivered batch
	// notice) awaiting an answer; additive, flag ON only.
	BatchAwaitingAnswer bool `json:"batch_awaiting_answer,omitempty"`
}

// makeSessionListItem projects a session.Session into the wire-stable
// sessionListItem shape used by `rush sessions list --json`.
func makeSessionListItem(s session.Session, costUSD float64) sessionListItem {
	return sessionListItem{
		ID:           s.ID,
		Hash:         session.HashID(s.ID),
		Title:        s.Title,
		MessageCount: s.MessageCount,
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
		Tokens:       s.PromptTokens + s.CompletionTokens,
		// #1130: cost_usd keeps its meaning (subtree budget since the last
		// reset); the node's own ledger is shown separately where relevant.
		CostUSD:     costUSD,
		YoloEnabled: s.YoloEnabled,
		EndedReason: s.EndedReason,
		// WS-1 audit fields (#1142 step C).
		WorkspaceRoot: s.WorkspaceRoot,
		GitBranch:     s.GitBranch,
	}
}
