package cmd

// The closing summary of `rush sessions audit`: the one "scanned N sessions"
// line — and the explicit "no anomalies" of a clean scan — that every report
// ends with, in text and in --json, including the notice-only reports a
// database without a store file produces.

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSessionsAudit_SummaryLineWithFindings: a scan that produced findings
// still ends with the honest total, and does not claim "no anomalies".
//
// Revert-check: deleting auditPrintSummary or the Summary field (or setting
// Summary only when findings exist) returns the "table without a total" —
// the assertions on the closing line and on the absence of "no anomalies"
// fail.
func TestSessionsAudit_SummaryLineWithFindings(t *testing.T) {
	const busy = "audit-summary-busy-1"
	const quiet = "audit-summary-idle-1"

	view := func(sessionID, path string) auditMessageFixture {
		return auditMessageFixture{
			SessionID: sessionID,
			Parts:     auditToolCall(t, "view", map[string]any{"file_path": path}),
		}
	}
	bash := func(sessionID, command string) auditMessageFixture {
		return auditMessageFixture{
			SessionID: sessionID,
			Parts:     auditToolCall(t, "bash", map[string]any{"command": command}),
		}
	}

	// The busy session trips repeated-view and repeated_command; the quiet
	// session's single ordinary view stays below every threshold.
	fixture := auditRulesFixture{Messages: []auditMessageFixture{
		view(busy, "internal/cmd/sessions_audit.go"), view(busy, "internal/cmd/sessions_audit.go"),
		view(busy, "internal/cmd/sessions_audit.go"),
		bash(busy, "go build ./internal/... 1"), bash(busy, "go build ./internal/... 2"),
		bash(busy, "go build ./internal/... 3"),
		view(quiet, "README.md"),
	}}

	stdout, reports := auditRunRulesAll(t, fixture)
	require.Contains(t, stdout, "scanned 2 sessions: 1 with findings, 1 clean",
		"WithFindings is a count of distinct sessions, not of findings")
	require.NotContains(t, stdout, "no anomalies",
		"a scan with findings must not claim to be clean")
	require.Equal(t, auditSummary{Scanned: 2, WithFindings: 1, Clean: 1},
		reports[0].Summary)
}

// TestSessionsAudit_SummaryLineCleanScan: a scan that found nothing says so
// out loud, with the same total line every other report ends with.
//
// Revert-check: inverting the "no anomalies" print to depend on
// len(findings) > 0 makes the line disappear from this clean scan and fails;
// counting WithFindings per finding instead of per session would corrupt the
// numbers of any multi-session scan.
func TestSessionsAudit_SummaryLineCleanScan(t *testing.T) {
	const session = "audit-summary-clean-1"

	// One session, one ordinary view, zero findings anywhere.
	fixture := auditRulesFixture{Messages: []auditMessageFixture{
		{SessionID: session, Parts: auditToolCall(t, "view", map[string]any{"file_path": "README.md"})},
	}}

	stdout, reports := auditRunRulesAll(t, fixture)
	require.Contains(t, stdout, "scanned 1 sessions: 0 with findings, 1 clean")
	require.Contains(t, stdout, "no anomalies",
		"a clean scan must say 'no anomalies' out loud, not just print zeroes")
	require.Equal(t, 0, reports[0].Summary.WithFindings)
	require.Equal(t, 1, reports[0].Summary.Clean)
}

// TestSessionsAudit_SummaryOnMissingDatabase: a database that is not there
// still yields a complete report — the notice-only text report ends with the
// same total line, the --json object still carries summary, and nothing is
// created in the data directory.
//
// Revert-check: an early return from auditPrintText before auditPrintSummary
// (skipping the total on the notice-only branch) makes the closing line and
// "no anomalies" disappear from this run and fails.
func TestSessionsAudit_SummaryOnMissingDatabase(t *testing.T) {
	isolateConfigEnvForTests(t)
	dataDir := t.TempDir()

	stdout, stderr, err := runAuditCmd(t, "--data-dir", dataDir, "--all")
	require.NoError(t, err, "stderr: %s", stderr)
	require.Contains(t, stdout, "no session store found in "+dataDir)
	require.Contains(t, stdout, "scanned 0 sessions: 0 with findings, 0 clean",
		"the notice-only report ends with the same total as any other")
	require.Contains(t, stdout, "no anomalies")
	require.Empty(t, auditDirNames(t, dataDir),
		"auditing a missing store must not create anything in the data dir")

	jsonStdout, _, err := runAuditCmd(t, "--data-dir", dataDir, "--all", "--json")
	require.NoError(t, err, "stderr: %s", stderr)
	var reports []auditDBReport
	require.NoError(t, json.Unmarshal([]byte(jsonStdout), &reports))
	require.NotEmpty(t, reports)
	require.Equal(t, 0, reports[0].Summary.Scanned,
		"the summary object is present even without a database")
}
