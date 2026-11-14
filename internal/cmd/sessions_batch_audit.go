package cmd

// Batch-side facts for `rush sessions audit` (R12-B part 3): the audit
// counts live batch node rows through the
// R10a live reader plus the R10c history snapshot, and per-owner task state
// counts through the BatchHistoryStateCounts query (batch_history.sql), all
// over the same strictly read-only handle the audit already opened — never a
// second, read-write connection. The legacy async_jobs ledger is not read:
// the unfinished_jobs rule counts held batch roots instead. A leftover legacy
// table is reported via the audit report, never treated as a silent disable.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"sort"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
)

// auditBatchRuleBatchStatus is the rule status when the flag is on and the
// rule counts batch facts (with or without the legacy table present).
const auditBatchRuleBatchStatus = "counted from batch_nodes (batch runtime on)"

// auditBatchScan is everything the flag-on audit read about the selected
// sessions' batch work: the per-session live facts (roots, debt, task
// counts, conservative unknown) and the per-owner task state distribution.
type auditBatchScan struct {
	Facts       map[string]batchSessionFacts
	StateCounts map[string]map[string]int64
}

// auditScanBatchFacts reads the batch facts of the selected sessions through
// the audit's own read-only connection. The live reader gets the audited
// data directory so its host probe sees exactly the host registry the real
// writer of that database uses; an error (batch_nodes unreadable) is
// returned, not swallowed — a missing batch_nodes table is a schema problem,
// caught earlier as a schema issue, never a silent empty scan.
func auditScanBatchFacts(ctx context.Context, conn *sql.DB, dataDir string, ids []string) (*auditBatchScan, error) {
	reader := session.NewBatchLiveReader(db.New(conn), dataDir)
	queries := db.New(conn)
	facts := reduceBatchSessionFacts(ctx, reader, session.NewBatchStore(conn), queries, ids)
	counts, err := auditBatchStateCounts(ctx, conn, ids)
	if err != nil {
		return nil, err
	}
	return &auditBatchScan{Facts: facts, StateCounts: counts}, nil
}

// auditBatchStateCounts reads every selected session's per-state task counts
// with ONE grouped query per chunk. The per-owner query
// (db.Queries.BatchHistoryStateCounts) has no index that fits it — its WHERE
// has no node_id = root_id, so every partial index on batch_nodes is unusable
// and each call is a full scan; an --all over 2000+ sessions paid that scan
// once per session. Grouping by owner keeps the exact per-session numbers the
// per-owner loop produced.
func auditBatchStateCounts(ctx context.Context, conn *sql.DB, ids []string) (map[string]map[string]int64, error) {
	counts := make(map[string]map[string]int64, len(ids))
	for start := 0; start < len(ids); start += auditQueryChunk {
		end := start + auditQueryChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		query := "SELECT owner_session_id, state, count(*) AS count FROM batch_nodes" +
			" WHERE owner_session_id IN (" + auditPlaceholders(len(chunk)) + ")" +
			" AND kind = 'task' GROUP BY owner_session_id, state"
		params := make([]any, 0, len(chunk))
		for _, id := range chunk {
			params = append(params, id)
		}
		if err := auditScanBatchStateCounts(ctx, conn, query, params, counts); err != nil {
			return nil, err
		}
	}
	// A session with no batch rows keeps an empty (non-nil) map, exactly as
	// the per-owner loop did, so the JSON block's shape is unchanged.
	for _, id := range ids {
		if counts[id] == nil {
			counts[id] = map[string]int64{}
		}
	}
	return counts, nil
}

// auditScanBatchStateCounts folds one grouped query's rows into counts.
func auditScanBatchStateCounts(ctx context.Context, conn *sql.DB, query string, params []any, counts map[string]map[string]int64) error {
	rows, err := conn.QueryContext(ctx, query, params...)
	if err != nil {
		return fmt.Errorf("query batch_nodes state counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			owner string
			state string
			count int64
		)
		if err := rows.Scan(&owner, &state, &count); err != nil {
			return fmt.Errorf("scan batch state count: %w", err)
		}
		if counts[owner] == nil {
			counts[owner] = make(map[string]int64, 8)
		}
		counts[owner][state] = count
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate batch state counts: %w", err)
	}
	return nil
}

// unfinishedEntries names the batch work a session left unfinished, one
// "tool_call_id:state" entry per held (nonterminal) root. A conservative
// unknown read contributes one "batch:unknown" entry: an audit must never
// call a session at rest while its batch read was incomplete.
func (s *auditBatchScan) unfinishedEntries(id string) []string {
	if s == nil {
		return nil
	}
	f := s.Facts[id]
	var entries []string
	for _, root := range f.Roots {
		if root.Held {
			entries = append(entries, root.ToolCallID+":"+root.State)
		}
	}
	if len(entries) == 0 && f.Unknown {
		entries = append(entries, "batch:unknown")
	}
	sort.Strings(entries)
	return entries
}

// auditBatchTasksJSON is the additive task-count triple of one session.
type auditBatchTasksJSON struct {
	Pending int `json:"pending"`
	Active  int `json:"active"`
	Done    int `json:"done"`
}

// auditBatchSessionJSON is one session's additive batch block: the live and
// unknown verdicts, debt, root counts and the per-state task distribution.
type auditBatchSessionJSON struct {
	Live        bool                `json:"live"`
	Unknown     bool                `json:"unknown"`
	Debt        int64               `json:"debt"`
	GuardDebt   bool                `json:"guard_debt"`
	Roots       int                 `json:"roots"`
	HeldRoots   int                 `json:"held_roots"`
	Tasks       auditBatchTasksJSON `json:"tasks"`
	StateCounts map[string]int64    `json:"state_counts"`
}

// auditBatchReport is the additive batch block of one audited database. It
// is present in --json only when the batch runtime flag is on.
type auditBatchReport struct {
	// UnfinishedJobsRule says what the unfinished_jobs rule counted from.
	UnfinishedJobsRule string                           `json:"unfinished_jobs_rule"`
	Sessions           map[string]auditBatchSessionJSON `json:"sessions"`
}

// report renders the scan into the additive JSON block.
func (s *auditBatchScan) report() *auditBatchReport {
	out := &auditBatchReport{
		UnfinishedJobsRule: auditBatchRuleBatchStatus,
		Sessions:           map[string]auditBatchSessionJSON{},
	}
	if s == nil {
		return out
	}
	for id, f := range s.Facts {
		held := 0
		for _, root := range f.Roots {
			if root.Held {
				held++
			}
		}
		out.Sessions[id] = auditBatchSessionJSON{
			Live:      f.Live,
			Unknown:   f.Unknown,
			Debt:      f.Debt,
			GuardDebt: f.GuardDebt,
			Roots:     len(f.Roots),
			HeldRoots: held,
			Tasks: auditBatchTasksJSON{
				Pending: f.Tasks.Pending,
				Active:  f.Tasks.Active,
				Done:    f.Tasks.Completed + f.Tasks.Failed + f.Tasks.Cancelled + f.Tasks.TimedOut + f.Tasks.Interrupted + f.Tasks.Skipped,
			},
			StateCounts: s.StateCounts[id],
		}
	}
	return out
}

// printText renders the batch block for the text report, sessions in a fixed
// order so the same database always renders the same lines.
func (r *auditBatchReport) printText(w io.Writer) {
	if r == nil {
		return
	}
	fmt.Fprintf(w, "batch: unfinished_jobs %s\n", r.UnfinishedJobsRule)
	ids := make([]string, 0, len(r.Sessions))
	for id := range r.Sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		f := r.Sessions[id]
		fmt.Fprintf(w,
			"  batch session %s: roots %d (held %d), debt %d, tasks %d pending/%d active/%d done, unknown %t\n",
			short(id), f.Roots, f.HeldRoots, f.Debt,
			f.Tasks.Pending, f.Tasks.Active, f.Tasks.Done, f.Unknown)
	}
}
