// providers show/list command tests running the real subcommand RunE
// through runProvidersCmdInIsolatedApp (isolated config fixture):
// peak-hours rendering in show, its omission without peak hours, and
// the list PEAK column.
package cmd

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	rushlog "github.com/PHPCraftdream/rush/internal/log"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runProvidersCmdInIsolatedAppFull executes a real providers subcommand's
// RunE against an isolated config fixture, capturing real stdout. It stands
// up a full app via setupApp (the same path the CLI uses) in a temp data dir
// with network/provider-discovery disabled, so the output is produced by the
// real rendering code in providers.go — not a reimplementation.
//
// cmd is the real providersShowCmd/providersListCmd/providersSetCmd/
// providersAddCmd. providerJSON is the raw JSON for the "providers" object
// written into the isolated global rush.json before the command runs. args
// is the positional/flag payload parsed onto cmd (e.g. "with-peak" for show,
// "--json" for list, "with-peak --peak-hours 10:00-20:00" for a write
// command).
//
// dataDir selects the config layout the command runs under — pass "" or a
// relative suffix such as ".rush":
//
//   - "" keeps the historical behaviour: the carrier's --data-dir is the
//     isolated global data dir (tmp), so the workspace scope collapses onto
//     the SAME file as the global scope (both resolve to
//     <tmp>/rush.json — global is RUSH_GLOBAL_DATA, workspace is
//     <DataDirectory>/rush.json). workspacePath is returned as "" because
//     the two are indistinguishable; use this for single-scope tests.
//   - ".rush" (production layout <cwd>/.rush) points the carrier's
//     --data-dir at <workDir>/.rush, which makes the workspace scope a
//     DIFFERENT file from the global one. Use this when a test passes
//     --local and must read back / distinguish the workspace file from the
//     untouched global one.
//
// Returns the captured stdout, the global rush.json path
// (RUSH_GLOBAL_DATA/rush.json) and the workspace rush.json path — "" when
// dataDir is "", otherwise <dataDir>/rush.json, exactly what
// configPath(ScopeWorkspace) resolves to (workspace = <DataDirectory>/
// rush.json, appName "rush"). runErr is the raw RunE error, RETURNED rather
// than failed on so callers can assert on it.
func runProvidersCmdInIsolatedAppFull(t *testing.T, cmd *cobra.Command, providerJSON, args, dataDir string) (stdout, globalPath, workspacePath string, runErr error) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	t.Setenv("RUSH_GLOBAL_DATA", tmp)
	// GlobalConfig() (RUSH_GLOBAL_CONFIG/XDG_CONFIG_HOME) is a SEPARATE
	// resolution path from GlobalConfigData() (RUSH_GLOBAL_DATA) above — see
	// CLAUDE.md's "two real config paths" caveat and the longer explanation
	// in isolatedModelsEnv (models_use_test.go). Without this, setupApp's
	// app.New() -> mcp.Initialize reads the real host
	// ~/.config/rush/rush.json and, if it configures MCP servers, tries to
	// open real network connections to them from inside the test — this is
	// the exact path that previously hung a stress run for 9+ minutes.
	//
	// Use a SEPARATE subdirectory from RUSH_GLOBAL_DATA (not the same tmp)
	// so lookupConfigs (internal/config/load.go), which loads and merges
	// both GlobalConfig() and GlobalConfigData(), doesn't load the same file
	// path twice under two different env vars — see the "Low, latent"
	// duplicate-merge caveat fixed in isolatedModelsEnv.
	configDir := filepath.Join(tmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	// Cache-only so provider discovery makes no network calls.
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	// Pre-initialise the once-only global logger so setupApp's log.Setup
	// call is a no-op and does not open a lumberjack handle inside the
	// temp dir (which would lock the file and break t.TempDir cleanup).
	rushlog.Setup("", false)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	globalDataPath := filepath.Join(tmp, "rush.json")
	require.NoError(t, os.WriteFile(globalDataPath, []byte(providerJSON), 0o644))

	// setupApp reads debug/data-dir/cwd off the command it receives. Those
	// are normally rootCmd persistent flags; build a carrier command that
	// carries them so we can invoke the real subcommand's RunE directly.
	ctx, cancel := context.WithCancel(context.Background())
	// dataDir == "" collapses the workspace scope onto the global file, so
	// the pooled SQLite connection lives directly under tmp. Otherwise it
	// lives under the workspace data dir (<workDir>/<dataDir>).
	dbDir := tmp
	if dataDir != "" {
		dbDir = filepath.Join(workDir, dataDir)
	}
	workspaceDataPath := filepath.Join(dbDir, "rush.json")
	t.Cleanup(func() {
		_ = os.Chdir(orig)
		// setupApp opens a pooled SQLite connection under the data dir;
		// cancel ctx and release THIS test's own connection so t.TempDir
		// cleanup doesn't hit a locked rush.db / rush.log on Windows.
		//
		// db.Release(dir), not db.ResetPool(): this file alone has ~19
		// t.Parallel() tests sharing this helper. ResetPool() used to nuke
		// the ENTIRE process-wide connection pool, including any other
		// still-running parallel test's live connection to a different
		// data dir — real cross-test interference, not just OS-level
		// handle-release lag, and a genuine contributor to this package's
		// Windows-only "process cannot access the file" flakiness.
		cancel()
		_ = db.Release(dbDir)
	})
	carrier := &cobra.Command{Use: "rush"}
	carrier.Flags().Bool("debug", false, "")
	carrier.Flags().String("data-dir", dbDir, "")
	carrier.Flags().String("cwd", workDir, "")
	carrier.SetContext(ctx)

	// Reset EVERY flag of the subcommand so state from a prior invocation in
	// the same process doesn't leak in — pflag's Changed otherwise survives
	// between tests, and since these command values are package-level
	// singletons (e.g. a prior test's --peak-hours) would corrupt a later
	// message-only test. DefValue is the registered default.
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	})
	cmd.SetArgs(nil)

	var runArgs []string
	if args != "" {
		runArgs = strings.Fields(args)
	}
	require.NoError(t, cmd.ParseFlags(runArgs))
	// cmd.RunE below is invoked as cmd.RunE(carrier, runArgs) — carrier,
	// not cmd, so setupAppLite(cmd) inside RunE (whose "cmd" parameter is
	// carrier at call time) can read the debug/data-dir/cwd flags only
	// carrier defines. But every subcommand-owned flag (peak-hours,
	// api-key, json, ...) was just parsed onto cmd's OWN FlagSet above —
	// without merging it in here, RunE's Flags().Changed/Get* calls would
	// silently see carrier's empty FlagSet instead. AddFlagSet shares the
	// underlying *pflag.Flag values (not copies), so carrier sees the
	// exact Value/Changed state cmd.ParseFlags just set.
	carrier.Flags().AddFlagSet(cmd.Flags())

	// Capture os.Stdout — providers list/show write there directly.
	var buf bytes.Buffer
	oldOut := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, r); close(done) }()

	// RunE receives the positional arguments only, as under cobra: flags
	// such as --all were already parsed onto cmd above.
	runErr = cmd.RunE(carrier, cmd.Flags().Args())

	_ = w.Close()
	os.Stdout = oldOut
	<-done

	if dataDir == "" {
		// The workspace scope collapses onto the global file — both resolve
		// to tmp/rush.json — so there is no distinct path to return.
		return buf.String(), globalDataPath, "", runErr
	}
	return buf.String(), globalDataPath, workspaceDataPath, runErr
}

// runProvidersCmdInIsolatedApp is runProvidersCmdInIsolatedAppFull with
// the workspace scope collapsed onto the global config file (dataDir "")
// and RunE errors failing the test.
func runProvidersCmdInIsolatedApp(t *testing.T, cmd *cobra.Command, providerJSON, args string) (stdout, dataPath string) {
	out, globalPath, _, runErr := runProvidersCmdInIsolatedAppFull(t, cmd, providerJSON, args, "")
	require.NoError(t, runErr, "command RunE failed; stdout was:\n%s", out)
	return out, globalPath
}

const peakFixtureJSON = `{
  "providers": {
    "with-peak": {
      "name": "With Peak",
      "type": "openai",
      "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1",
      "models": [{"id": "gpt-4o"}],
      "peak_hours": {"start": "09:00", "end": "18:00"}
    },
    "with-peak-message": {
      "name": "With Peak Message",
      "type": "openai",
      "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1",
      "models": [{"id": "gpt-4o"}],
      "peak_hours": {"start": "09:00", "end": "18:00", "message": "Ping #ops-oncall first."}
    },
    "no-peak": {
      "name": "No Peak",
      "type": "anthropic",
      "base_url": "https://api.anthropic.com",
      "models": [{"id": "claude-sonnet-4"}]
    }
  }
}`

func TestProvidersShow_PeakHoursRendering(t *testing.T) {
	// Regression: this previously reimplemented the peak-hours line inline
	// and asserted the duplicate against itself. It now runs the real
	// providersShowCmd.RunE and asserts on the actual emitted stdout.
	out, _ := runProvidersCmdInIsolatedApp(t, providersShowCmd, peakFixtureJSON, "with-peak")

	assert.Contains(t, out, "id:          with-peak")
	assert.Contains(t, out, "peak hours:  09:00-18:00 (currently:")
	// The state must be one of the two real branches the command emits.
	assert.True(t, strings.Contains(out, "(currently: in peak)") || strings.Contains(out, "(currently: not in peak)"),
		"expected a real 'currently:' state in output:\n%s", out)
	assert.NotContains(t, out, "peak hours message", "a provider with no configured message must not show the line")
}

func TestProvidersShow_PeakHoursMessageRendering(t *testing.T) {
	// The message is settable from the CLI via --peak-hours-message, but it
	// can also come from the web UI — show must surface it either way, since
	// it's otherwise invisible to a CLI-only operator.
	out, _ := runProvidersCmdInIsolatedApp(t, providersShowCmd, peakFixtureJSON, "with-peak-message")

	assert.Contains(t, out, "id:          with-peak-message")
	assert.Contains(t, out, "peak hours message: Ping #ops-oncall first.")
}

func TestProvidersShow_NoPeakHoursOmitsLine(t *testing.T) {
	// Regression: this previously only asserted p.PeakHours == nil without
	// running the command. It now runs the real providersShowCmd.RunE on a
	// provider without peak hours and asserts the line is absent.
	out, _ := runProvidersCmdInIsolatedApp(t, providersShowCmd, peakFixtureJSON, "no-peak")

	assert.Contains(t, out, "id:          no-peak")
	assert.NotContains(t, out, "peak hours", "show must omit the peak-hours line when PeakHours is nil")
}

func TestProvidersList_PeakColumn(t *testing.T) {
	// Regression: this previously reimplemented the PEAK column rendering
	// inline. It now runs the real providersListCmd.RunE and asserts on the
	// actual table output.
	out, _ := runProvidersCmdInIsolatedApp(t, providersListCmd, peakFixtureJSON, "")

	assert.Contains(t, out, "PEAK", "list header must include the PEAK column")
	assert.Contains(t, out, "with-peak", "with-peak row must be present")
	assert.Contains(t, out, "no-peak", "no-peak row must be present")
	// The with-peak row must show the window; the no-peak row must show the
	// em-dash placeholder used by the list command's real rendering.
	assert.Contains(t, out, "09:00-18:00", "with-peak PEAK cell must show the window")
	assert.Contains(t, out, "—", "no-peak PEAK cell must show the placeholder")
}
