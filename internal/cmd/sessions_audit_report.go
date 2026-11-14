package cmd

// Rendering one or more scanned databases: the text output an operator
// reads and the JSON a script reads. Everything goes through cmd's own
// writers so a caller (and the tests) can capture it.

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/PHPCraftdream/rush/internal/db/storepath"
	"github.com/spf13/cobra"
)

// auditDBReport is what one scanned data directory produced.
type auditDBReport struct {
	DataDir      string            `json:"data_dir"`
	DBPath       string            `json:"db_path"`
	Notices      []string          `json:"notices,omitempty"`
	SchemaIssues []string          `json:"schema_issues,omitempty"`
	Health       *auditDBHealth    `json:"health,omitempty"`
	Sessions     []auditSessionRow `json:"sessions,omitempty"`
	Findings     []auditFinding    `json:"findings,omitempty"`
	// Batch is the additive flag-on block: batch facts from batch_nodes and
	// the unfinished_jobs rule's status. Absent with the flag off.
	Batch *auditBatchReport `json:"batch,omitempty"`
	// Summary is the always-present scan total (text line + JSON object).
	Summary auditSummary `json:"summary"`
}

// auditSummary is the honest total of one scanned database: how many sessions
// the scan covered, how many tripped at least one rule, how many were clean.
// It is always present — a run whose output ends after the session table is a
// clean scan, never a truncated one.
type auditSummary struct {
	Scanned      int `json:"scanned"`
	WithFindings int `json:"with_findings"`
	Clean        int `json:"clean"`
}

// auditReportSummary totals one scan. Clean is derived from the other two
// numbers, so the three can never disagree; findings name only scanned
// sessions, so WithFindings never exceeds Scanned.
func auditReportSummary(rows []auditSessionRow, findings []auditFinding) auditSummary {
	summary := auditSummary{Scanned: len(rows)}
	suspicious := make(map[string]bool, len(findings))
	for _, finding := range findings {
		suspicious[finding.SessionID] = true
	}
	summary.WithFindings = len(suspicious)
	summary.Clean = summary.Scanned - summary.WithFindings
	return summary
}

// auditPrintSummary is the one line every report ends with, plus the explicit
// "no anomalies" a clean scan must say out loud.
func auditPrintSummary(w io.Writer, summary auditSummary) {
	fmt.Fprintf(w, "scanned %d sessions: %d with findings, %d clean\n",
		summary.Scanned, summary.WithFindings, summary.Clean)
	if summary.WithFindings == 0 {
		fmt.Fprintln(w, "no anomalies")
	}
}

// auditPrintReports writes every scanned database, either as text or as a
// JSON array (one object per database, so --worktrees' many databases share
// one shape).
func auditPrintReports(cmd *cobra.Command, reports []auditDBReport, asJSON bool) error {
	if asJSON {
		encoded, err := json.MarshalIndent(reports, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal audit report: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
		return nil
	}

	for _, report := range reports {
		if err := auditPrintText(cmd.OutOrStdout(), report); err != nil {
			return err
		}
	}
	return nil
}

func auditPrintText(w io.Writer, report auditDBReport) error {
	for _, notice := range report.Notices {
		fmt.Fprintln(w, notice)
	}
	// A notice is the whole report: a missing database or an ambiguous
	// prefix has no health or sessions to print after it.
	if len(report.Notices) > 0 {
		auditPrintSummary(w, report.Summary)
		return nil
	}

	fmt.Fprintf(w, "== %s (%s)\n", report.DataDir, storepath.ActiveFilename)

	for _, issue := range report.SchemaIssues {
		fmt.Fprintf(w, "%s: this does not look like a Rush session database (a foreign one, or one too old to analyse); audit is read-only and changed nothing\n", issue)
	}

	if report.Health != nil {
		auditPrintHealth(w, *report.Health)
	}

	if len(report.Sessions) == 0 {
		fmt.Fprintln(w, "(no sessions)")
		auditPrintSummary(w, report.Summary)
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "sessions (%d):\n", len(report.Sessions))
	fmt.Fprintln(tw, "  ID\tTITLE\tCREATED\tUPDATED\tMSGS")
	for _, row := range report.Sessions {
		// The full id, not the short one: an operator reads a row here to
		// re-run the audit on that exact session.
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%d\n",
			row.ID, truncate(row.Title, 40),
			auditFormatUnix(row.CreatedAt), auditFormatUnix(row.UpdatedAt), row.MessageCount)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	for _, finding := range report.Findings {
		fmt.Fprintf(w, "%s: session %s — %s (x%d)\n",
			finding.Rule, short(finding.SessionID), finding.Detail, finding.Count)
	}
	report.Batch.printText(w)
	auditPrintSummary(w, report.Summary)
	return nil
}

func auditPrintHealth(w io.Writer, health auditDBHealth) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "db health:")
	fmt.Fprintf(tw, "  size\t%s\n", auditFormatBytes(health.SizeBytes))
	if health.PageSize > 0 {
		fmt.Fprintf(tw, "  pages\t%d x %d B\n", health.PageCount, health.PageSize)
	}
	fmt.Fprintf(tw, "  freelist\t%d pages (%.1f%% of the file is free space)\n", health.FreelistPages, health.FreePercent)
	if health.WALBytes > 0 {
		fmt.Fprintf(tw, "  wal\t%s\n", auditFormatBytes(health.WALBytes))
	}
	if health.SHMBytes > 0 {
		fmt.Fprintf(tw, "  shm\t%s\n", auditFormatBytes(health.SHMBytes))
	}
	// A tabwriter cannot fail on a bytes.Buffer or a terminal, and the
	// health block has nothing left to report afterwards, so a flush error
	// here has no actionable meaning.
	_ = tw.Flush()
}
