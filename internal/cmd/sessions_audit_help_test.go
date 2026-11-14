package cmd

// Help text of `rush sessions audit` and of its parent. The command sells
// itself as strictly read-only, so that promise has to be in the help an
// operator reads — and in the list every subcommand has to appear in.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSessionsAuditHelp_StatesTheReadOnlyContract: the help must carry the
// promise the implementation keeps (mode=ro, query_only, no migrations, no
// writes) and document every flag the command registers.
//
// Revert-check: with the "read-only" clause and the mode=ro mention removed
// from Long, this test fails naming them.
func TestSessionsAuditHelp_StatesTheReadOnlyContract(t *testing.T) {
	t.Parallel()

	long := oneLine(sessionsAuditCmd.Long)
	require.Contains(t, long, "read-only", "the command's core contract must be in the help")
	require.Contains(t, long, "mode=ro")
	require.Contains(t, long, "PRAGMA query_only")
	require.Contains(t, long, "no migrations, no locks, no writes")
	require.Contains(t, long, "the session store file is not modified")
	require.Contains(t, long, "creates nothing", "the help must say audit creates nothing")

	short := oneLine(sessionsAuditCmd.Short)
	require.Contains(t, short, "read-only")

	require.Equal(t, "audit [<id>|--all]", sessionsAuditCmd.Use)

	for _, name := range []string{"all", "since", "worktrees", "json"} {
		require.NotNilf(t, sessionsAuditCmd.Flags().Lookup(name), "flag --%s is missing from the help", name)
	}
	require.Contains(t, oneLine(sessionsAuditCmd.Flags().Lookup("worktrees").Usage), "read-only")

	// --data-dir is the root command's persistent flag: the help documents
	// it, it is not registered twice.
	require.Nil(t, sessionsAuditCmd.PersistentFlags().Lookup("data-dir"),
		"--data-dir must not be re-registered on the subcommand")

	require.Contains(t, long, "scanned N sessions", "the help must document the always-printed summary line")
	require.Contains(t, long, "no anomalies", "the help must document the explicit clean-scan line")
	require.Contains(t, oneLine(sessionsAuditCmd.Flags().Lookup("since").Usage), "not scanned",
		"the help must say --since also decides which sessions --all scans")
}

// TestSessionsAuditHelp_DeclaresTheRulesAndTheirThresholds: the help lists
// every suspicion rule and the threshold it fires at, and the code has a
// constant for each of them.
func TestSessionsAuditHelp_DeclaresTheRulesAndTheirThresholds(t *testing.T) {
	t.Parallel()

	long := oneLine(sessionsAuditCmd.Long)
	require.Contains(t, long, "the same file viewed again and again")
	require.Contains(t, long, "the same normalized bash command repeated")
	require.Contains(t, long, "wait-only commands")
	require.Contains(t, long, "tool errors, counted per error class")
	require.Contains(t, long, "supervision / wake_failed notices")
	require.Contains(t, long, "unfinished batch work")

	thresholds := map[string]int{
		"file views":      auditThresholdFileViews,
		"bash commands":   auditThresholdBashCommands,
		"wait-only":       auditThresholdWaitOnlyCommands,
		"tool errors":     auditThresholdToolErrorsPerClass,
		"notices":         auditThresholdNotices,
		"unfinished jobs": auditThresholdUnfinishedJobs,
	}
	require.Equal(t, 3, thresholds["file views"])
	require.Equal(t, 3, thresholds["bash commands"])
	require.Equal(t, 3, thresholds["wait-only"])
	require.Equal(t, 3, thresholds["tool errors"])
	require.Equal(t, 1, thresholds["notices"])
	require.Equal(t, 1, thresholds["unfinished jobs"])

	require.Contains(t, long, "DB health", "the help must mention the DB health block")
	require.Contains(t, long, "freelist")
	require.Contains(t, long, "free space")
}

// TestSessionsAuditHelp_ExamplesCoverTheFlags: the examples show the flags
// in the combinations an operator actually reaches for.
func TestSessionsAuditHelp_ExamplesCoverTheFlags(t *testing.T) {
	t.Parallel()

	example := oneLine(sessionsAuditCmd.Example)
	require.Contains(t, example, "rush sessions audit --all --since 24h")
	require.Contains(t, example, "rush sessions audit --worktrees --json")
	require.Contains(t, example, "rush sessions audit --data-dir")
}

// TestSessionsParentHelp_ListsAudit: the `sessions` root help lists every
// subcommand it has — one that is missing from it does not exist for the
// operator reading it.
//
// Revert-check: removing "audit" from the Observe block (or from
// AddCommand) turns this red.
func TestSessionsParentHelp_ListsAudit(t *testing.T) {
	t.Parallel()

	observe := oneLine(sessionsCmd.Long)
	require.Contains(t, observe, "audit")
	require.Contains(t, observe, "anomaly scan")

	var registered bool
	for _, sub := range sessionsCmd.Commands() {
		if sub.Name() == "audit" {
			registered = true
			require.Same(t, sessionsAuditCmd, sub, "the registered command must be the one under test")
		}
	}
	require.True(t, registered, "`sessions audit` must be registered on the sessions command")
}
