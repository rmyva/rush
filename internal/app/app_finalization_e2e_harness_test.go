package app

// Finalization e2e harness: a parameterized copy of newAdmissionRaceApp
// that can switch options.finalization.enabled on through the real
// rush.json. Everything else — env isolation, the on-disk provider, the
// seeded session — stays identical to the admission fixture so the
// flag-off shape characterizes main byte-for-byte and the flag-on shape
// differs in exactly the one flag.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// newFinalizationE2EApp isolates global config, starts a mock openaicompat
// SSE provider driven by handler, and builds a full App with one seeded
// session (non-default title plus one pre-existing user message, so
// background title generation never fires and every provider request in
// the test belongs to the main flow). finalizationEnabled appends the
// "options": {"finalization": {"enabled": true}} block to rush.json before
// the object's closing brace; when false nothing is appended, because an
// absent flag block means the feature is off (FinalizationOptions.Enabled
// is a pointer so absent stays distinguishable from explicit false — and
// both mean off). Returns the app and the session id.
func newFinalizationE2EApp(t *testing.T, handler http.HandlerFunc, finalizationEnabled bool) (*App, string) {
	t.Helper()

	// Both GlobalConfig() and GlobalConfigData() resolution paths must be
	// isolated separately (see the golden envelope test's identical block).
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	dataDir := t.TempDir()
	trace := newAdmissionTrace()
	srv := httptest.NewServer(trace.wrap(handler))
	t.Cleanup(srv.Close)

	// The provider lives on disk, not only in memory: a config reload (the disk
	// poller or a config write's auto-reload) rebuilds from disk, and a
	// memory-only provider would vanish with the coder agent.
	finalizationOptionsJSON := ""
	if finalizationEnabled {
		finalizationOptionsJSON = `, "options": {"finalization": {"enabled": true}}`
	}
	rushJSON := fmt.Sprintf(`{
  "disable_default_providers": true,
  "providers": {
    "openaicompat": {
      "id": "openaicompat", "type": %q, "base_url": %q,
      "api_key": "probe", "discover_models": false,
      "models": [{"id":"probe","name":"probe","context_window":200000,"default_max_tokens":1000}]
    }
  },
  "models": {
    "smart": {"provider":"openaicompat","model":"probe"},
    "fast": {"provider":"openaicompat","model":"probe"}
  }%s}`, openaicompat.Name, srv.URL, finalizationOptionsJSON)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), []byte(rushJSON), 0o644))
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	// The wiring reads exactly this config path (app_finalization_wiring.go
	// and finalization_gate.go both gate on Config().Options.
	// AgentFinalizationEnabled()), so the flag must have reached the
	// composition root before any test below can attribute its outcome to
	// the flag.
	require.Equal(t, finalizationEnabled, application.Config().Options.AgentFinalizationEnabled(),
		"the rush.json finalization flag must reach the composition root the finalization wiring reads")
	t.Cleanup(application.Shutdown)
	// The inline window stays ON: a restricted bash probe is denied at the
	// permission gate well inside the window, so the denial (StopTurn)
	// returns inline and the turn stops there. Detaching bash (the window
	// off) would return "started" and let the turn continue to a second
	// provider round.

	sess, err := application.Sessions.Create(context.Background(), "finalization-e2e-title")
	require.NoError(t, err)
	_, err = application.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)
	// Registered last, so it runs before application.Shutdown on cleanup.
	logAdmissionAppOnFailure(t, application, store, sess.ID, trace)
	return application, sess.ID
}
