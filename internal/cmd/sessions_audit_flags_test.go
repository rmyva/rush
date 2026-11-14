package cmd

// The flag surface of `rush sessions audit`: what each flag adds to the scan.
// --worktrees is the only one that widens the scan beyond a single database,
// and it does so through the registered project list — so the registry has
// to be isolated rather than trusted. --since narrows both what the rules
// count and which sessions --all scans, and --json is the shape a script
// reads, so it has to carry the findings themselves.
//
// The --since window itself over the per-rule counts (old/recent/mixed
// evidence against the repeated-view rule) is already proved by
// TestAuditRules_SinceWindow in sessions_audit_rules_test.go; what is left
// over here is that the window bounds which sessions are scanned and listed
// at all, and that it is not specific to one rule.

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db/storepath"
	"github.com/PHPCraftdream/rush/internal/projects"
	"github.com/stretchr/testify/require"
)

// TestSessionsAudit_HelpNoLongerDefersTheRules: the help must not claim the
// rules themselves are still to come — they are implemented, in
// sessions_audit_findings.go with the readers in sessions_audit_rules.go,
// and the thresholds are the const block the help describes. A stale "later
// phase" sentence here is exactly the kind of text that sends a reader
// looking for work that is already done.
//
// Revert-check: reintroducing the phase-1 wording into Long fails this.
func TestSessionsAudit_HelpNoLongerDefersTheRules(t *testing.T) {
	t.Parallel()

	long := oneLine(sessionsAuditCmd.Long)
	require.Contains(t, long, "Every rule is implemented and every threshold is declared once as a constant")
	for _, stale := range []string{"Phase 1", "phase 1", "later phase", "lands in a later", "are yet to come"} {
		require.NotContains(t, long, stale,
			"the help must not claim the rules still arrive in a later phase")
	}
}

// TestSessionsAudit_WorktreesScansEveryRegisteredDatabase: --worktrees audits
// the linked worktrees' databases alongside the current one — one report
// object per database — and still writes to neither of them.
//
// The registry is isolated the same way the rest of this package isolates
// config: projectsFilePath() is the projects.json that sits next to
// config.GlobalConfigData(), which honours RUSH_GLOBAL_DATA/XDG_DATA_HOME —
// exactly the two variables isolateConfigEnvForTests points at a throwaway
// directory. So this test registers its own second checkout with
// projects.Register and never sees (or disturbs) the operator's real list.
//
// Revert-check: making auditWorktreeDataDirs return nil, or dropping the
// --worktrees flag wiring in sessionsAuditCmdRun, turns the second database
// missing and this fails on the report count.
func TestSessionsAudit_WorktreesScansEveryRegisteredDatabase(t *testing.T) {
	isolateConfigEnvForTests(t)

	dataDir := t.TempDir()
	linkedDir := t.TempDir()
	currentDB := seedAuditFixtureDB(t, dataDir)
	linkedDB := seedAuditFixtureDB(t, linkedDir)

	// The way `rush` registers a checkout on startup: its working directory
	// and the data directory it keeps its own session store file in.
	require.NoError(t, projects.Register(filepath.Join(linkedDir, "checkout"), linkedDir))
	require.Equal(t, []string{linkedDir}, auditWorktreeDataDirs(dataDir),
		"the linked worktree's data dir is the only extra database to scan")

	beforeCurrent := auditFileSHA256(t, currentDB)
	beforeLinked := auditFileSHA256(t, linkedDB)

	stdout, stderr, err := runAuditCmd(t, "--data-dir", dataDir, "--worktrees", "--all")
	require.NoError(t, err, "stderr: %s", stderr)
	require.Contains(t, stdout, dataDir, "the text report must name the current database")
	require.Contains(t, stdout, linkedDir, "the text report must name the linked worktree's database")
	require.Equal(t, 2, strings.Count(stdout, "db health:"),
		"one health block per scanned database")

	jsonStdout, _, err := runAuditCmd(t, "--data-dir", dataDir, "--worktrees", "--all", "--json")
	require.NoError(t, err)
	var reports []auditDBReport
	require.NoError(t, json.Unmarshal([]byte(jsonStdout), &reports))
	require.Len(t, reports, 2, "one JSON object per scanned database, not per rule or per session")
	require.Equal(t, dataDir, reports[0].DataDir, "the current database is scanned first")
	require.Equal(t, linkedDir, reports[1].DataDir)
	for _, report := range reports {
		require.Equal(t, storepath.ActivePath(report.DataDir), report.DBPath)
		require.NotNil(t, report.Health, "every scanned database reports its health")
		require.NotEmpty(t, report.Sessions, "every scanned database reports its sessions")
	}

	require.Equal(t, beforeCurrent, auditFileSHA256(t, currentDB),
		"--worktrees must not modify the current database")
	require.Equal(t, beforeLinked, auditFileSHA256(t, linkedDB),
		"--worktrees must not modify the linked worktree's database")
	require.Equal(t, []string{storepath.ActiveFilename}, auditDirNames(t, linkedDir),
		"scanning a linked worktree must not create a sidecar in its data dir")

	// A registered checkout whose store file is gone is skipped rather than
	// reported as an error, so the scan still covers exactly two databases.
	vanishedDir := t.TempDir()
	require.NoError(t, projects.Register(filepath.Join(vanishedDir, "checkout"), vanishedDir))
	require.Equal(t, []string{linkedDir}, auditWorktreeDataDirs(dataDir),
		"a registered checkout without a store file is skipped")
	jsonStdout, _, err = runAuditCmd(t, "--data-dir", dataDir, "--worktrees", "--all", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(jsonStdout), &reports))
	require.Len(t, reports, 2, "a skipped worktree is not a report")
}

// TestSessionsAudit_SinceLimitsScannedSessions: --since narrows BOTH what the
// rules count and which sessions --all scans — a session whose every message
// is older than the window is not scanned and not listed, so the session
// table, the findings and the summary describe the same window. DB health is
// file-level and stays in every report.
//
// Revert-check: removing auditRecentSessionsFilter from auditSelectSessions
// (the old behaviour: --all lists every session regardless of the window)
// turns the stale-fixture assertions below red — the old-only session comes
// back into reports[0].Sessions and "(no sessions)" disappears.
func TestSessionsAudit_SinceLimitsScannedSessions(t *testing.T) {
	const stale = "audit-since-stale-1"
	const fresh = "audit-since-fresh-1"

	old := time.Now().Add(-72 * time.Hour).Unix()
	recent := time.Now().Add(-time.Minute).Unix()

	wait := func(sessionID string, createdAt int64) auditMessageFixture {
		return auditMessageFixture{
			SessionID: sessionID,
			Parts:     auditToolCall(t, "bash", map[string]any{"command": "sleep 5"}),
			CreatedAt: createdAt,
		}
	}

	// One session with only out-of-window messages, one with a message
	// inside the window.
	fixture := auditRulesFixture{Messages: []auditMessageFixture{
		wait(stale, old), wait(stale, old+1), wait(stale, old+2),
		wait(fresh, recent),
	}}

	// With a window, the stale session is not scanned, not listed, not
	// counted and not summarized — only the fresh one is described.
	stdout, reports := auditRunRulesAll(t, fixture, "--since", "24h")
	require.Len(t, reports[0].Sessions, 1, "stdout: %s", stdout)
	require.Equal(t, fresh, reports[0].Sessions[0].ID,
		"the session with an in-window message is the only one scanned")
	require.NotContains(t, stdout, stale,
		"a session with nothing inside the window is not listed at all")
	require.Empty(t, auditFindingsForRule(reports, "wait_only"),
		"three out-of-window sleeps cannot be counted against the window")
	require.Equal(t, auditSummary{Scanned: 1, WithFindings: 0, Clean: 1},
		reports[0].Summary, "the summary counts the window, not the database")
	require.Contains(t, stdout, "scanned 1 sessions: 0 with findings, 1 clean")
	require.Contains(t, stdout, "no anomalies",
		"a window with nothing suspicious in it says so out loud")
	require.NotNil(t, reports[0].Health, "DB health is file-level and survives the window")

	// Without a window, both sessions are scanned and summarized — and the
	// stale session's three sleeps are counted again, so this run is the
	// contrast case for the windowed one above.
	stdout, reports = auditRunRulesAll(t, fixture)
	require.Len(t, reports[0].Sessions, 2, "no window: every session is scanned")
	require.Len(t, auditFindingsForRule(reports, "wait_only"), 1,
		"without a window the stale session's three sleeps are a finding again")
	require.Equal(t, auditSummary{Scanned: 2, WithFindings: 1, Clean: 1},
		reports[0].Summary)
	require.Contains(t, stdout, "scanned 2 sessions: 1 with findings, 1 clean")

	// A database whose every message is outside the window is still audited —
	// it reports honestly that it scanned nothing inside the window.
	staleOnly := auditRulesFixture{Messages: []auditMessageFixture{
		wait(stale, old), wait(stale, old+1), wait(stale, old+2),
	}}
	stdout, reports = auditRunRulesAll(t, staleOnly, "--since", "24h")
	require.Empty(t, reports[0].Sessions, "nothing inside the window is scanned")
	require.Contains(t, stdout, "(no sessions)")
	require.Contains(t, stdout, "scanned 0 sessions: 0 with findings, 0 clean")
	require.Contains(t, stdout, "no anomalies")
}

// TestSessionsAudit_JSONCarriesFindings: --json is what a script reads, so it
// carries the findings themselves — the rule, the session, the evidence and
// the count — and not only the summary the text output shows.
func TestSessionsAudit_JSONCarriesFindings(t *testing.T) {
	const session = "audit-json-flags-1"

	view := func(path string) auditMessageFixture {
		return auditMessageFixture{
			SessionID: session,
			Parts:     auditToolCall(t, "view", map[string]any{"file_path": path}),
		}
	}
	fixture := auditRulesFixture{Messages: []auditMessageFixture{
		// Two files, three views each, and one repeated command.
		view("internal/cmd/sessions_audit.go"), view("internal/cmd/sessions_audit.go"),
		view("internal/cmd/sessions_audit.go"),
		view("internal/cmd/sessions_audit_report.go"), view("internal/cmd/sessions_audit_report.go"),
		view("internal/cmd/sessions_audit_report.go"),
		{
			SessionID: session,
			Parts: auditToolCall(t, "bash",
				map[string]any{"command": "go test ./internal/cmd/ 2"}),
		},
		{
			SessionID: session,
			Parts: auditToolCall(t, "bash",
				map[string]any{"command": "go test ./internal/cmd/ 77"}),
		},
		{
			SessionID: session,
			Parts: auditToolCall(t, "bash",
				map[string]any{"command": "go test ./internal/cmd/ 909"}),
		},
	}}

	_, reports := auditRunRulesAll(t, fixture)
	require.Len(t, reports, 1)

	findings := reports[0].Findings
	require.NotEmpty(t, findings, "a fixture of repeats must produce findings in --json")
	for _, finding := range findings {
		require.NotEmpty(t, finding.Rule, "a finding without a rule cannot be acted on")
		require.NotEmpty(t, finding.SessionID, "a finding must name the session that tripped it")
		require.NotEmpty(t, finding.Detail, "a finding must carry its evidence")
		require.Greater(t, finding.Count, 0, "a finding is a count that reached its threshold")
		require.Equal(t, session, finding.SessionID)
	}

	views := auditFindingsForRule(reports, "repeated-view")
	require.Len(t, views, 2, "each repeated file is its own finding")
	require.Equal(t, "repeated-view", views[0].Rule)
	require.Equal(t, 3, views[0].Count)

	commands := auditFindingsForRule(reports, "repeated_command")
	require.Len(t, commands, 1)
	require.Equal(t, 3, commands[0].Count)
	require.Equal(t, "go test ./internal/cmd/ N", commands[0].Detail,
		"the JSON evidence is normalized exactly like the text output's")
}
