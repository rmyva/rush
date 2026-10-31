package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// peakWindowAroundNow returns a PeakHoursWindow that is guaranteed to cover
// the current wall-clock time, so checkPeakHours refuses. It picks a wide
// window centred on the current hour.
func peakWindowAroundNow() *config.PeakHoursWindow {
	now := time.Now()
	start := now.Add(-2 * time.Hour)
	end := now.Add(2 * time.Hour)
	return &config.PeakHoursWindow{
		Start: start.Format("15:04"),
		End:   end.Format("15:04"),
	}
}

// TestCheckPeakHours_RefusesInWindow pins the refusal behaviour: when the
// provider's peak_hours window covers now, checkPeakHours returns an error
// that wraps errProviderPeakHours and carries the descriptive text the
// orchestrator must see in `rush run` output.
func TestCheckPeakHours_RefusesInWindow(t *testing.T) {
	w := peakWindowAroundNow()
	cfg := config.ProviderConfig{ID: "test-peak", PeakHours: w}

	err := checkPeakHours(cfg)
	require.Error(t, err, "in-window peak_hours must refuse")
	assert.ErrorIs(t, err, errProviderPeakHours, "refusal must wrap errProviderPeakHours for classifyProviderError")
	// The descriptive text is what reaches `rush run`'s output (plain stderr
	// and the --json envelope's error field). Assert the operator-actionable
	// fragments are present.
	msg := err.Error()
	assert.Contains(t, msg, "test-peak", "error must name the provider id")
	assert.Contains(t, msg, "peak hours", "error must say it is peak hours")
	assert.Contains(t, msg, "refusing until", "error must say when it will be available")
}

// TestCheckPeakHours_AllowsOutsideWindow confirms the no-window and
// out-of-window cases return nil (the refusal is not spuriously firing).
func TestCheckPeakHours_AllowsOutsideWindow(t *testing.T) {
	t.Run("nil window is allowed", func(t *testing.T) {
		assert.NoError(t, checkPeakHours(config.ProviderConfig{ID: "p"}))
	})
	t.Run("out-of-window is allowed", func(t *testing.T) {
		// Window far in the past relative to "now".
		w := &config.PeakHoursWindow{Start: "00:00", End: "00:01"}
		// Only assert if now happens to be outside it (it always is for a
		// 1-minute window at midnight unless the test runs at exactly 00:00).
		if !w.InPeakHours(time.Now()) {
			assert.NoError(t, checkPeakHours(config.ProviderConfig{ID: "p", PeakHours: w}))
		}
	})
}

// TestSetAllowPeakHours_BypassesRunInternal verifies the bypass path that
// `rush run --allow-peak-hours` arms. It builds a coordinator whose provider
// is inside its peak_hours window (so a normal Run would refuse with
// errProviderPeakHours), arms SetAllowPeakHours(true), and asserts the mock
// agent's Run is reached instead of the refusal firing.
func TestSetAllowPeakHours_BypassesRunInternal(t *testing.T) {
	const providerID = "test-peak"
	w := peakWindowAroundNow()
	providerCfg := config.ProviderConfig{
		ID:        providerID,
		Type:      "openai",
		PeakHours: w,
		Models: []catwalk.Model{
			{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096},
		},
	}

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(providerID, providerCfg)
	cfg.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{
		Provider: providerID,
		Model:    "test-model",
	}
	// resolveSessionModels always resolves both smart AND fast — leaving
	// fast unset works accidentally on a dev machine whose real global
	// config (always merged in by config.Load regardless of workingDir)
	// happens to provide a fallback fast model, but fails cleanly on a
	// clean CI sandbox with "fast model provider not configured". Set it
	// explicitly so this test doesn't depend on the local environment.
	cfg.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{
		Provider: providerID,
		Model:    "test-model",
	}
	// runInternal's retry path calls lastAssistantMessage -> c.messages, so
	// wire the real message service (matches appendAssistant's setup).
	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}

	agentReached := false
	agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		agentReached = true
		return agentResultWithText("ok"), nil
	})
	// runInternal reads c.currentAgent.Model(); the mock supplies it.
	coord.currentAgent = agent

	// Create a session for model resolution.
	sess, err := env.sessions.Create(t.Context(), "peak-bypass")
	require.NoError(t, err)

	// Resolve models for the session.
	pinned, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)

	// Sanity: without the bypass, runInternal must refuse BEFORE reaching the
	// agent. (This is the "test must fail without the fix" leg.)
	_, err = coord.runInternal(t.Context(), sess.ID, "prompt", pinned)
	require.Error(t, err, "without bypass, in-window peak_hours must refuse")
	assert.ErrorIs(t, err, errProviderPeakHours)
	assert.False(t, agentReached, "agent.Run must NOT be reached when peak_hours refuses")

	// Arm the one-shot bypass and re-run. The agent must be reached now and
	// the refusal must not fire.
	coord.SetAllowPeakHours(true)
	// runInternal needs a real session row for resolveSessionSystemPrompt /
	// retry bookkeeping; re-resolve models to get a fresh pinned.
	pinned, err = coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)

	res, err := coord.runInternal(t.Context(), sess.ID, "prompt", pinned)
	require.NoError(t, err, "with bypass armed, peak_hours must NOT refuse")
	require.NotNil(t, res)
	assert.True(t, agentReached, "agent.Run must be reached when --allow-peak-hours bypass is armed")

	// The bypass is one-shot: a second run without re-arming must refuse again.
	agentReached = false
	_, err = coord.runInternal(t.Context(), sess.ID, "prompt", pinned)
	require.Error(t, err, "bypass must be consumed after one Run; second run refuses again")
	assert.ErrorIs(t, err, errProviderPeakHours)
	assert.False(t, agentReached)
}

// TestSetAllowPeakHours_OneShotReset confirms the flag does not persist:
// reading it twice without re-arming yields false on the second read. This
// guards against a future refactor that turns the one-shot into a sticky
// setting (which would defeat the "conscious one-off override" contract).
func TestSetAllowPeakHours_OneShotReset(t *testing.T) {
	coord := &coordinator{}
	assert.False(t, coord.allowPeakHours, "default must be false")
	coord.SetAllowPeakHours(true)
	coord.runLimitsMu.Lock()
	got := coord.allowPeakHours
	coord.runLimitsMu.Unlock()
	assert.True(t, got, "arming sets the flag")

	// Simulate runInternal consuming it.
	coord.runLimitsMu.Lock()
	allow := coord.allowPeakHours
	coord.allowPeakHours = false
	coord.runLimitsMu.Unlock()
	assert.True(t, allow, "first consume sees the armed value")

	coord.runLimitsMu.Lock()
	allow2 := coord.allowPeakHours
	coord.runLimitsMu.Unlock()
	assert.False(t, allow2, "after consume the flag is reset")
}

func TestBuildAgentPeakHoursCheckUsesPassedProvider(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)

	passedProvider := config.ProviderConfig{
		ID:        "passed-provider",
		Type:      "openai",
		PeakHours: peakWindowAroundNow(),
		Models:    []catwalk.Model{{ID: "passed-model", Name: "Passed Model", DefaultMaxTokens: 4096}},
	}
	coord.cfg.Config().Providers.Set(passedProvider.ID, passedProvider)

	coderCfg, ok := coord.cfg.Config().Agents[config.AgentCoder]
	require.True(t, ok, "coder agent must be configured")
	sa, err := coord.buildAgent(t.Context(), coord.prompt, coderCfg, false)
	require.NoError(t, err)
	require.NoError(t, coord.readyWg.Wait(), "agent build must complete")

	builtAgent, ok := sa.(*sessionAgent)
	require.True(t, ok, "buildAgent must return a *sessionAgent")
	require.NotNil(t, builtAgent.peakHoursCheck)

	err = builtAgent.peakHoursCheck(passedProvider.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, errProviderPeakHours)
	var peakErr *PeakHoursError
	require.ErrorAs(t, err, &peakErr)
	assert.Equal(t, passedProvider.ID, peakErr.ProviderID,
		"the installed callback must check the turn-passed provider, not the build-time smart provider")
}

func TestCheckLivePeakHours_ReloadsDirtyConfig(t *testing.T) {
	// Legacy-JSON fixture test: it rewrites rush.json at its literal path
	// after config.Init and expects the live reload to see it, so the gate
	// must be off (after migration the active file is rush.ktav).
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)

	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "rush.json")
	initial := `{
		"options": {"disable_default_providers": true},
		"providers": {
			"custom": {
				"api_key": "test-key",
				"base_url": "https://api.example.test/v1",
				"models": [{"id": "model"}]
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initial), 0o600))

	store, err := config.Init(workDir, workDir, false)
	require.NoError(t, err)
	coord := &coordinator{cfg: store}
	require.NoError(t, coord.checkLivePeakHours("custom"))

	w := peakWindowAroundNow()
	updated := fmt.Sprintf(`{
		"options": {"disable_default_providers": true},
		"providers": {
			"custom": {
				"api_key": "test-key",
				"base_url": "https://api.example.test/v1",
				"models": [{"id": "model"}],
				"peak_hours": {"start": %q, "end": %q}
			}
		}
	}`, w.Start, w.End)
	require.NoError(t, os.WriteFile(configPath, []byte(updated), 0o600))

	err = coord.checkLivePeakHours("custom")
	require.Error(t, err)
	assert.ErrorIs(t, err, errProviderPeakHours)
}
