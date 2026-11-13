package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestFinalizationBindScopeHookCarriesTrustedAuthoritiesIntoDriver proves the
// hook installs the trusted capability in the driver registry through the
// frozen binding path, is idempotent for the same identity, and never lets
// the incoming call rewrite the frozen policy fields of the stored driver.
//
// The driver is registered without a logical identity and with parent-only
// authorities, so a hook that never reaches bindFinalizationDriverCall leaves
// the stored identity empty and fails the assertions below.
//
// Revert-check: dropping the bindFinalizationDriverCall call fails the stored
// logical id and authorities assertions; copying the incoming call wholesale
// fails the frozen-policy assertions.
func TestFinalizationBindScopeHookCarriesTrustedAuthoritiesIntoDriver(t *testing.T) {
	fixture := newFinalizationDriverFixture(t)
	registry := newSubAgentDriverRegistry()
	binder := &sessionAgent{coord: &coordinator{subAgentDrivers: registry}}
	credentials := &CredentialSet{}
	options := &CallOptions{MaxAgentDepth: 1, AgentDepthSet: true}
	parent, err := fixture.api.BindAgentFinalization(fixture.ctx, fixture.parent)
	require.NoError(t, err)
	parentCtx, err := session.WithAgentFinalizationContext(context.Background(), parent)
	require.NoError(t, err)
	original := SessionAgentCall{
		SessionID: fixture.child.SessionID,
		MaxCost:   2, MaxTokens: 100, Credentials: credentials, CallOptions: options,
		RequestedModelRole: RequestedModelRole("reviewer"),
	}
	generation := registry.register(original.SessionID, subAgentDriver{agent: binder, call: original, finalizationAuthorities: session.CaptureAgentFinalizationContext(parentCtx)})
	pre, ok := registry.get(original.SessionID)
	require.True(t, ok)
	require.Empty(t, pre.call.LogicalCallID, "the driver starts without a logical identity")
	incoming := original
	incoming.LogicalCallID = fixture.child.LogicalCallID
	incoming.driverGeneration = generation
	incoming.finalizationAuthorities = fixture.capability
	incoming.MaxCost, incoming.MaxTokens, incoming.RequestedModelRole = 200, 9999, RequestedModelRole("smart")
	hook := NewFinalizationBindScopeHook(binder)
	require.NoError(t, hook(fixture.ctx, incoming))
	require.NoError(t, hook(fixture.ctx, incoming), "rebinding the same trusted identity is idempotent")
	stored, ok := registry.get(original.SessionID)
	require.True(t, ok)
	require.Equal(t, generation, stored.generation)
	require.Equal(t, incoming.LogicalCallID, stored.call.LogicalCallID)
	require.Equal(t, fixture.capability.Requirement(), stored.finalizationAuthorities.Requirement())
	require.Same(t, credentials, stored.call.Credentials, "identity transport keeps the frozen credentials")
	require.Same(t, options, stored.call.CallOptions)
	require.Equal(t, original.MaxCost, stored.call.MaxCost)
	require.Equal(t, original.MaxTokens, stored.call.MaxTokens)
	require.Equal(t, original.RequestedModelRole, stored.call.RequestedModelRole)
}

// TestFinalizationBindScopeHookRefusesForeignIdentity proves every forged or
// unbindable call fails closed and leaves the registered driver untouched:
// a forged logical call id, a widened inherited authority, a stale driver
// generation and a bound requirement without a driver generation are all
// refused, while a call without bound authorities stays inert.
//
// Revert-check: deleting the finalizationLogicalIdentity pre-check alone is
// still caught (bindFinalizationIdentity re-checks); deleting BOTH the
// pre-check and the frozen checks is caught by the forged-logical and
// widened-authority refusals disappearing.
func TestFinalizationBindScopeHookRefusesForeignIdentity(t *testing.T) {
	fixture := newFinalizationDriverFixture(t)
	registry := newSubAgentDriverRegistry()
	binder := &sessionAgent{coord: &coordinator{subAgentDrivers: registry}}
	call := SessionAgentCall{SessionID: fixture.child.SessionID, LogicalCallID: fixture.child.LogicalCallID, finalizationAuthorities: fixture.capability}
	generation := registry.register(call.SessionID, subAgentDriver{agent: binder, call: call, finalizationAuthorities: fixture.capability})
	call.driverGeneration = generation
	hook := NewFinalizationBindScopeHook(binder)

	t.Run("forged logical call id", func(t *testing.T) {
		forged := call
		forged.LogicalCallID = "different"
		require.ErrorIs(t, hook(fixture.ctx, forged), session.ErrAgentFinalizationChanged)
	})
	t.Run("widened inherited authority", func(t *testing.T) {
		_, err := db.New(fixture.conn).CreateSession(fixture.ctx, db.CreateSessionParams{ID: "other-owner", Title: "other"})
		require.NoError(t, err)
		other, err := fixture.api.BindAgentFinalization(fixture.ctx, session.AgentFinalizationIdentity{SessionID: "other-owner", LogicalCallID: "other", Generation: 1})
		require.NoError(t, err)
		wider, err := session.WithAgentFinalizationContext(fixture.ctx, other)
		require.NoError(t, err)
		forged := call
		forged.finalizationAuthorities = session.CaptureAgentFinalizationContext(wider)
		require.ErrorIs(t, hook(fixture.ctx, forged), session.ErrAgentFinalizationChanged)
	})
	t.Run("stale driver generation", func(t *testing.T) {
		registry.register(call.SessionID, subAgentDriver{agent: binder, call: call, finalizationAuthorities: fixture.capability})
		require.ErrorIs(t, hook(fixture.ctx, call), session.ErrAgentFinalizationDriverUnavailable)
	})
	t.Run("bound requirement without driver generation", func(t *testing.T) {
		driverless := call
		driverless.driverGeneration = 0
		require.ErrorIs(t, hook(fixture.ctx, driverless), session.ErrAgentFinalizationDriverUnavailable)
	})
	t.Run("no authorities stays inert", func(t *testing.T) {
		require.NoError(t, hook(fixture.ctx, SessionAgentCall{SessionID: call.SessionID, LogicalCallID: call.LogicalCallID, driverGeneration: generation}))
	})
	stored, ok := registry.get(call.SessionID)
	require.True(t, ok)
	require.Equal(t, fixture.child.LogicalCallID, stored.call.LogicalCallID)
	require.Equal(t, fixture.capability.Requirement(), stored.finalizationAuthorities.Requirement())
}
