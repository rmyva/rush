package cmd

// `rush sessions audit` — a strictly read-only scan of a session database
// for anomalies (a session repeating the same work, a delegation that never
// finished, a supervision loop).
//
// The command is the scaffold plus the rules: data-dir resolution, a mode=ro
// open, a schema check, DB health and a per-session summary live in
// sessions_audit_db.go, and the suspicion rules themselves in
// sessions_audit_findings.go (their thresholds are the const block below)
// with the readers they count with in sessions_audit_rules.go.
//
// This command deliberately does NOT go through setupApp: setupApp opens the
// database read-write and runs migrations, which is exactly what an audit
// must never do — it may be pointed at a live data directory whose real
// writer is mid-turn.

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db/storepath"
	"github.com/PHPCraftdream/rush/internal/projects"
	"github.com/spf13/cobra"
)

// Suspicion thresholds, declared once so the help text below and the rules
// that consume them (auditFindings, sessions_audit_findings.go) can never
// drift apart.
const (
	auditThresholdFileViews          = 3
	auditThresholdBashCommands       = 3
	auditThresholdWaitOnlyCommands   = 3
	auditThresholdToolErrorsPerClass = 3
	auditThresholdNotices            = 1
	auditThresholdUnfinishedJobs     = 1
)

// What "only waiting" means, declared exactly once and next to the thresholds
// that count it. A bash command is wait-only when, after every run of digits
// has been folded to "N", the WHOLE command matches one of these patterns.
// The whole-line anchor is the point: a command that waits and then works
// (`sleep 5 && git status`) is ordinary work and must never be counted here.
// The list is deliberately minimal — one pattern per way a Rush agent can
// spend a turn waiting — and the wait_only rule in sessions_audit_rules.go is
// its only reader.
var auditWaitOnlyPatterns = []*regexp.Regexp{
	// `sleep 5`, `sleep 5m`, `sleep 0.5`: the canonical wait, with or
	// without an argument.
	regexp.MustCompile(`^\s*sleep(\s+\S+)?\s*$`),
	// `timeout 30`, `timeout 30 <cmd>`: bounded waiting.
	regexp.MustCompile(`^\s*timeout\s+\S+(\s+\S+)?\s*$`),
	// The shell's own `wait`, with or without a list of pids.
	regexp.MustCompile(`^\s*wait(\s+\S+)?\s*$`),
	// Any other poll/wait helper run only to block, e.g. `poll.sh --wait`,
	// `wait-for-ci --interval=5`.
	regexp.MustCompile(`^\s*\S*(poll|wait)\S*(\s+(\S+=\S*|--?wait\S*|[0-9A-Za-z_.-]+))*\s*$`),
}

var sessionsAuditCmd = &cobra.Command{
	Use:   "audit [<id>|--all]",
	Short: "Scan session DB for anomalies (strictly read-only)",
	Long: `Audit a session database for anomalies: a session repeating the same
work, a delegation that never finished, a supervision loop.

Read-only: the database is opened with mode=ro and PRAGMA query_only; no
migrations, no locks, no writes — the session store file is not modified (not
even its mtime), and no -wal/-shm/journal sidecar is created or removed. This
is deliberately NOT the setupApp path, which opens the database read-write and
migrates it: audit resolves the data directory itself, the same lightweight
way "sessions reap" and "sessions kill" do.

A missing database, or one whose schema is not a Rush schema, is reported
and exits 0 — audit creates nothing, migrates nothing and fixes nothing.

Flags:
  --all          audit every session in the database
  --since <d>    only messages newer than a Go duration (30m, 24h), a day
                 suffix (7d), or a bare integer read as days; with --all,
                 sessions with no message inside the window are not scanned
  --worktrees    also scan the databases of the linked worktrees: every
                 other checkout Rush has registered keeps its own
                 .rush store (see "rush projects", read-only)
  --json         one JSON object per scanned database instead of text
  --data-dir <p> (inherited from the root command) the data directory to
                 read; defaults to <cwd>/.rush

Selection: pass a full session id, or a prefix of one. A prefix matching
more than one session is refused — re-run with the full id.

The scan always ends with one honest total: "scanned N sessions: M with
findings, K clean", plus an explicit "no anomalies" line when M is zero —
output that stops after the session table is a clean scan, not a truncated
one. --json carries the same total as a "summary" object per database.

Rules and thresholds:
  the same file viewed again and again                >= 3 times
  the same normalized bash command repeated           >= 3 times
  wait-only commands (sleep/poll loops)               >= 3 times
  tool errors, counted per error class                >= 3 times
  supervision / wake_failed notices                   >= 1
  unfinished batch work (held roots, unknown reads)      >= 1
A session's verdict lists the rules it tripped, the evidence (file path,
command or notice) and the counts. Every rule is implemented and every
threshold is declared once as a constant, so the help and the analysis can
never drift apart. Wait-only means a command that does nothing but wait:
after every run of digits has been folded to "N", its whole line has to
match one of 'sleep 5', 'timeout 30 <cmd>', 'wait' or a poll/wait helper.
A command that waits and then works ('sleep 5 && git
status') is ordinary work, counted as a repeated command instead.

DB health is printed for every scanned database: size, page_size x
page_count, freelist pages and the percentage of the file that is free
space, plus the size of the -wal and -shm sidecars when the
database is live in WAL mode.`,
	Example: `
# Audit every session's last 24 hours of activity
rush sessions audit --all --since 24h

# Include the linked worktrees' databases, machine-readable
rush sessions audit --worktrees --json

# Point at a data directory other than <cwd>/.rush
rush sessions audit --data-dir /path/to/.rush --all

# A single session, by id or by prefix
rush sessions audit 7f3a91c2
  `,
	Args: cobra.MaximumNArgs(1),
	RunE: sessionsAuditCmdRun,
}

func init() {
	sessionsAuditCmd.Flags().Bool("all", false, "Audit every session in the database (otherwise pass a session id or a prefix of one)")
	sessionsAuditCmd.Flags().String("since", "", "Only analyse messages newer than this: Go duration (30m, 24h), day suffix (7d), or a bare integer read as days; with --all, sessions with no message in the window are not scanned")
	sessionsAuditCmd.Flags().Bool("worktrees", false, "Scan linked worktrees' databases (read-only)")
	sessionsAuditCmd.Flags().Bool("json", false, "Emit one JSON object per scanned database instead of text")
}

func sessionsAuditCmdRun(cmd *cobra.Command, args []string) error {
	all, _ := cmd.Flags().GetBool("all")
	worktrees, _ := cmd.Flags().GetBool("worktrees")
	asJSON, _ := cmd.Flags().GetBool("json")
	sinceStr, _ := cmd.Flags().GetString("since")

	if !all && len(args) == 0 {
		return fmt.Errorf("nothing to audit: pass a session id or a prefix of one, or --all\n" +
			"usage: rush sessions audit <id|prefix> | rush sessions audit --all")
	}

	var since time.Duration
	if sinceStr != "" {
		parsed, err := parseSinceDuration(sinceStr)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		since = parsed
	}

	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return err
	}
	// The same lightweight resolution "sessions reap"/"sessions kill" use:
	// --data-dir first, then the project's configured data_directory, then
	// <cwd>/.rush. Never setupApp — it would open the DB read-write.
	dataDirFlag, _ := cmd.Flags().GetString("data-dir")
	dataDir, err := config.ResolveDataDirectory(cwd, dataDirFlag)
	if err != nil {
		return fmt.Errorf("failed to resolve data directory: %w", err)
	}

	dirs := []string{dataDir}
	if worktrees {
		dirs = append(dirs, auditWorktreeDataDirs(dataDir)...)
	}

	reports := make([]auditDBReport, 0, len(dirs))
	for _, dir := range dirs {
		report, err := auditDataDir(cmd.Context(), dir, args, all, since)
		if err != nil {
			return err
		}
		reports = append(reports, report)
	}
	return auditPrintReports(cmd, reports, asJSON)
}

// auditWorktreeDataDirs lists the data directories of the linked worktrees
// Rush knows about (the registered project list, read-only). Each linked
// worktree runs its own "rush" process and therefore keeps its own
// .rush store; --worktrees audits those alongside the current one.
// The current data dir is never repeated, and a project whose store is
// gone is skipped rather than reported as an error.
func auditWorktreeDataDirs(current string) []string {
	list, err := projects.List()
	if err != nil {
		return nil
	}
	var dirs []string
	for _, project := range list {
		if project.DataDir == "" || project.DataDir == current {
			continue
		}
		if _, err := os.Stat(storepath.ActivePath(project.DataDir)); err != nil {
			continue
		}
		dirs = append(dirs, project.DataDir)
	}
	sort.Strings(dirs)
	return dirs
}
