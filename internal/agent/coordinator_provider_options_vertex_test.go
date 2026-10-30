package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/google"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestGetProviderOptionsVertexThinkingOptions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		modelID    string
		effort     string
		wantBudget bool
		wantLevel  string
	}{
		{name: "Gemini 2 budget", modelID: "gemini-2.5-pro", wantBudget: true},
		{name: "newer model thinking level", modelID: "gemini-3-pro-preview", effort: "high", wantLevel: "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := Model{
				CatwalkCfg: catwalk.Model{
					ID:              tc.modelID,
					CanReason:       true,
					ReasoningLevels: []string{"low", "high"},
				},
				ModelCfg: config.SelectedModel{Provider: "vertexai", ReasoningEffort: tc.effort},
			}
			providerCfg := config.ProviderConfig{ID: "vertexai", Type: catwalk.TypeVertexAI}

			opts := getProviderOptions("test-session", model, providerCfg)

			raw, ok := opts[google.Name]
			require.True(t, ok, "Vertex AI should use Google's provider options")
			parsed, ok := raw.(*google.ProviderOptions)
			require.True(t, ok)
			require.NotNil(t, parsed.ThinkingConfig)
			require.NotNil(t, parsed.ThinkingConfig.IncludeThoughts)
			require.True(t, *parsed.ThinkingConfig.IncludeThoughts)
			if tc.wantBudget {
				require.NotNil(t, parsed.ThinkingConfig.ThinkingBudget)
				require.Equal(t, int64(2000), *parsed.ThinkingConfig.ThinkingBudget)
				require.Nil(t, parsed.ThinkingConfig.ThinkingLevel)
			} else {
				require.NotNil(t, parsed.ThinkingConfig.ThinkingLevel)
				require.Equal(t, tc.wantLevel, *parsed.ThinkingConfig.ThinkingLevel)
				require.Nil(t, parsed.ThinkingConfig.ThinkingBudget)
			}
		})
	}
}

func TestGetProviderOptionsGoogleAndVertexThinkingOptionsMatch(t *testing.T) {
	for _, modelID := range []string{"gemini-2.5-pro", "gemini-3-pro-preview"} {
		t.Run(modelID, func(t *testing.T) {
			model := Model{
				CatwalkCfg: catwalk.Model{ID: modelID, CanReason: true, ReasoningLevels: []string{"low", "high"}},
				ModelCfg:   config.SelectedModel{Provider: "google", ReasoningEffort: "high"},
			}
			googleOptions := getProviderOptions("test-session", model, config.ProviderConfig{ID: "google", Type: catwalk.TypeGoogle})[google.Name]
			vertexOptions := getProviderOptions("test-session", model, config.ProviderConfig{ID: "vertexai", Type: catwalk.TypeVertexAI})[google.Name]
			require.NotNil(t, googleOptions)
			require.NotNil(t, vertexOptions)
			require.Equal(t, googleOptions, vertexOptions)
		})
	}
}
