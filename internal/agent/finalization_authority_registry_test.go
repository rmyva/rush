package agent

import (
	"fmt"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// bindRegistryAuthority binds a real authority for one owner session so the
// registry tests exercise live capability values.
func bindRegistryAuthority(t *testing.T, env fakeEnv, owner, logical string) *session.AgentFinalizationAuthority {
	t.Helper()
	api, ok := env.sessions.(session.AgentFinalizationService)
	require.True(t, ok)
	authority, err := api.BindAgentFinalization(t.Context(), session.AgentFinalizationIdentity{
		SessionID: owner, LogicalCallID: logical, Generation: 1,
	})
	require.NoError(t, err)
	return authority
}

// TestFinalizationAuthorityRegistryRegisterLookupUnregister covers the basic
// register/lookup/unregister round trip, unknown owners, and the idempotent
// double unregister.
func TestFinalizationAuthorityRegistryRegisterLookupUnregister(t *testing.T) {
	env := testEnv(t)
	ensureFixtureSession(t, env.conn, "owner-a")
	authority := bindRegistryAuthority(t, env, "owner-a", "call-a")
	registry := NewFinalizationAuthorityRegistry()

	unregister := registry.Register("owner-a", authority)
	require.NotNil(t, unregister)
	require.Same(t, authority, registry.Lookup("owner-a"))
	require.Nil(t, registry.Lookup("owner-b"), "an unregistered owner must look up nil")

	unregister()
	require.Nil(t, registry.Lookup("owner-a"))
	require.NotPanics(t, func() { unregister() }, "a double unregister is a no-op")
	require.Nil(t, registry.Lookup("owner-a"))
}

// TestFinalizationAuthorityRegistryUnregisterRemovesOnlyOwnEntry covers
// same-owner overwrites, the stale-unregister guard, independent owners, and
// the no-op registrations for an empty owner or a nil authority.
func TestFinalizationAuthorityRegistryUnregisterRemovesOnlyOwnEntry(t *testing.T) {
	env := testEnv(t)
	ensureFixtureSession(t, env.conn, "owner-a")
	ensureFixtureSession(t, env.conn, "owner-b")
	first := bindRegistryAuthority(t, env, "owner-a", "call-a")
	second := bindRegistryAuthority(t, env, "owner-b", "call-b")
	registry := NewFinalizationAuthorityRegistry()

	// Same key: the later registration overwrites, and the earlier
	// registration's unregister must not drop the newer entry.
	firstUnregister := registry.Register("shared", first)
	secondUnregister := registry.Register("shared", second)
	require.Same(t, second, registry.Lookup("shared"))
	firstUnregister()
	require.Same(t, second, registry.Lookup("shared"), "a stale unregister must keep the newer authority")
	secondUnregister()
	require.Nil(t, registry.Lookup("shared"))

	// Different keys are independent: removing one leaves the other.
	aUnregister := registry.Register("key-a", first)
	bUnregister := registry.Register("key-b", second)
	require.Same(t, first, registry.Lookup("key-a"))
	require.Same(t, second, registry.Lookup("key-b"))
	aUnregister()
	require.Nil(t, registry.Lookup("key-a"))
	require.Same(t, second, registry.Lookup("key-b"))
	bUnregister()

	// An empty owner or a nil authority yields a callable no-op.
	require.NotNil(t, registry.Register("", first))
	require.NotNil(t, registry.Register("key-c", nil))
	require.NotPanics(t, func() { registry.Register("", first)() })
	require.NotPanics(t, func() { registry.Register("key-c", nil)() })
	require.Nil(t, registry.Lookup("key-c"))
}

// TestFinalizationAuthorityRegistryConcurrentAccess registers, reads and
// unregisters owners from parallel goroutines around one shared registration
// and asserts the shared entry and the per-owner cleanup after the join.
func TestFinalizationAuthorityRegistryConcurrentAccess(t *testing.T) {
	env := testEnv(t)
	ensureFixtureSession(t, env.conn, "shared-owner")
	shared := bindRegistryAuthority(t, env, "shared-owner", "shared-call")
	registry := NewFinalizationAuthorityRegistry()
	unregisterShared := registry.Register("shared-owner", shared)
	defer unregisterShared()

	const workers = 8
	owners := make([]string, workers)
	calls := make([]string, workers)
	authorities := make([]*session.AgentFinalizationAuthority, workers)
	for i := range owners {
		owners[i] = fmt.Sprintf("worker-owner-%d", i)
		calls[i] = fmt.Sprintf("worker-call-%d", i)
		ensureFixtureSession(t, env.conn, owners[i])
		authorities[i] = bindRegistryAuthority(t, env, owners[i], calls[i])
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			unregister := registry.Register(owners[i], authorities[i])
			for iteration := 0; iteration < 25; iteration++ {
				require.NotNil(t, registry.Lookup(owners[i]))
				require.NotNil(t, registry.Lookup("shared-owner"))
			}
			unregister()
			require.Nil(t, registry.Lookup(owners[i]))
		}(i)
	}
	wg.Wait()

	require.Same(t, shared, registry.Lookup("shared-owner"))
	for _, owner := range owners {
		require.Nil(t, registry.Lookup(owner), "every worker owner must be unregistered after the join")
	}
}
