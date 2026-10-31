package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPMutationReconcilesOnlySelectedDefinitionAndToken(t *testing.T) {
	for _, mutation := range []struct {
		name  string
		apply func(*ConfigStore) error
	}{
		{
			name: "field",
			apply: func(store *ConfigStore) error {
				return store.PersistMCPFields(ScopeGlobal, "A", map[string]any{"url": "http://a-updated.example"})
			},
		},
		{
			name: "field-exact",
			apply: func(store *ConfigStore) error {
				return store.PersistMCPFieldsExact(ScopeGlobal, "A", map[string]any{"url": "http://a-updated.example"})
			},
		},
		{
			name: "ordinary-mutation",
			apply: func(store *ConfigStore) error {
				_, err := store.PersistMCPDisabledOverrideResult(ScopeGlobal, "A", true)
				return err
			},
		},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			first, root := isolatedMCPConfigStore(t)
			require.NoError(t, first.PersistMCPConfig(ScopeGlobal, "A", MCPConfig{Type: MCPHttp, URL: "http://a.example"}))
			require.NoError(t, first.PersistMCPConfig(ScopeGlobal, "B", MCPConfig{Type: MCPHttp, URL: "http://b-old.example"}))
			second, err := Init(root, root, false)
			require.NoError(t, err)

			// A pending in-memory Add is not part of this disk transaction.
			pending := MCPConfig{Type: MCPHttp, URL: "http://pending.example"}
			require.True(t, first.AddMCP("pending", pending))
			beforeB := first.SnapshotMCPAdmission("B")
			beforePending := first.SnapshotMCPAdmission("pending")

			// The other store commits B, then this store mutates only A.
			require.NoError(t, second.PersistMCPFieldsExact(ScopeGlobal, "B", map[string]any{"url": "http://b-new.example"}))
			require.NoError(t, mutation.apply(first))

			b, ok := first.MCPConfig("B")
			require.True(t, ok)
			require.Equal(t, "http://b-old.example", b.URL, "unrelated stale definition remains unchanged")
			afterB := first.SnapshotMCPAdmission("B")
			require.Equal(t, beforeB.MCPInput, afterB.MCPInput, "unrelated stale admission token remains unchanged")
			require.Equal(t, beforeB.MCPRevision, afterB.MCPRevision)

			gotPending, ok := first.MCPConfig("pending")
			require.True(t, ok, "pending in-memory definition must survive publication")
			require.Equal(t, pending, gotPending)
			afterPending := first.SnapshotMCPAdmission("pending")
			require.Equal(t, beforePending.MCPInput, afterPending.MCPInput)
			require.Equal(t, beforePending.MCPRevision, afterPending.MCPRevision)

			err = first.WithCurrentMCPAdmission(beforeB, "B", func(MCPAdmissionGuard) error { return nil })
			require.ErrorIs(t, err, ErrMCPMutationStale, "fresh disk input for B must reject the stale admission")
		})
	}
}
