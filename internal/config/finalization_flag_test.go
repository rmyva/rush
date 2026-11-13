package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/invopop/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func finalizationBoolPtr(v bool) *bool { return &v }

// TestAgentFinalizationEnabledAccessor pins the nil-safety and the
// false default of the finalization accessor.
func TestAgentFinalizationEnabledAccessor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts *Options
		want bool
	}{
		{"nil options", nil, false},
		{"empty options", &Options{}, false},
		{"nil enabled", &Options{Finalization: &FinalizationOptions{}}, false},
		{"explicit false", &Options{Finalization: &FinalizationOptions{Enabled: finalizationBoolPtr(false)}}, false},
		{"explicit true", &Options{Finalization: &FinalizationOptions{Enabled: finalizationBoolPtr(true)}}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.opts.AgentFinalizationEnabled())
		})
	}
}

// TestFinalizationFlagJSONParsing pins the JSON shape of the flag.
func TestFinalizationFlagJSONParsing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		doc  string
		want bool
	}{
		{`{"options":{"finalization":{"enabled":true}}}`, true},
		{`{"options":{"finalization":{"enabled":false}}}`, false},
		{`{"options":{}}`, false},
		{`{}`, false},
	}
	for _, tc := range cases {
		var cfg Config
		require.NoError(t, json.Unmarshal([]byte(tc.doc), &cfg))
		assert.Equal(t, tc.want, cfg.Options.AgentFinalizationEnabled(), tc.doc)
	}
}

// TestFinalizationOptionsMergeAcrossDocs pins the deep-merge behavior
// of options.finalization across global and project layers.
func TestFinalizationOptionsMergeAcrossDocs(t *testing.T) {
	t.Parallel()

	t.Run("global only survives project without options", func(t *testing.T) {
		t.Parallel()
		cfg, err := mergeDocs(t,
			`{"options":{"finalization":{"enabled":true}}}`,
			`{}`,
		)
		require.NoError(t, err)
		require.NotNil(t, cfg.Options.Finalization)
		assert.True(t, cfg.Options.AgentFinalizationEnabled())
	})

	t.Run("later document wins per key", func(t *testing.T) {
		t.Parallel()
		cfg, err := mergeDocs(t,
			`{"options":{"finalization":{"enabled":true}}}`,
			`{"options":{"finalization":{"enabled":false}}}`,
		)
		require.NoError(t, err)
		require.NotNil(t, cfg.Options.Finalization)
		assert.False(t, cfg.Options.AgentFinalizationEnabled())
	})

	t.Run("unrelated options key does not clobber sibling", func(t *testing.T) {
		t.Parallel()
		cfg, err := mergeDocs(t,
			`{"options":{"finalization":{"enabled":true}}}`,
			`{"options":{"debug":true}}`,
		)
		require.NoError(t, err)
		require.NotNil(t, cfg.Options.Finalization)
		assert.True(t, cfg.Options.AgentFinalizationEnabled())
	})

	t.Run("neither sets it", func(t *testing.T) {
		t.Parallel()
		cfg, err := mergeDocs(t, `{}`, `{}`)
		require.NoError(t, err)
		assert.False(t, cfg.Options.AgentFinalizationEnabled())
	})
}

// TestFinalizationFlagSchemaPinsTag pins the jsonschema tag output and
// the committed schema.json content against silent drift.
func TestFinalizationFlagSchemaPinsTag(t *testing.T) {
	t.Parallel()

	const wantDescription = "Agent finalization (batch agents): hold the final answer until children finish, reaction debts settle, there is no cancellation and no open mutations. Default: false."

	s := new(jsonschema.Reflector).Reflect(&Options{})
	require.NotNil(t, s)

	def, ok := s.Definitions["FinalizationOptions"]
	require.True(t, ok, "FinalizationOptions definition missing")
	enabled, ok := def.Properties.Get("enabled")
	require.True(t, ok, "enabled property missing")
	assert.Equal(t, "boolean", enabled.Type)

	optsDef, ok := s.Definitions["Options"]
	require.True(t, ok, "Options definition missing")
	finalization, ok := optsDef.Properties.Get("finalization")
	require.True(t, ok, "finalization property missing")
	assert.Equal(t, "#/$defs/FinalizationOptions", finalization.Ref)
	assert.Equal(t, wantDescription, finalization.Description)

	// Part 2: the committed schema.json must carry both the $defs key
	// and the exact description, or a tag edit without a schema update
	// fails here.
	raw, err := os.ReadFile(filepath.Join("..", "..", "schema.json"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"FinalizationOptions":`)
	assert.Contains(t, string(raw), wantDescription)
}
