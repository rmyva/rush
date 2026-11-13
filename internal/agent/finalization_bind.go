package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/session"
)

// NewFinalizationBindScopeHook builds the driver-start BindScope hook (lane
// L4). The returned hook binds the finalization scope of one logical call
// from an already trusted capability: it resolves the call identity through
// finalizationLogicalIdentity, refuses any conflict with that capability
// (fail-closed), and installs the trusted authorities in the driver registry
// through bindFinalizationDriverCall. A call without bound authorities is
// inert and reports no error, matching the legacy flag-off behavior.
//
// The hook must be invoked in the frozen S1 order: after
// restoreFinalizationCallContext validated the carried capability, and
// before the first provider step. It never duplicates or replaces that
// restore, and it changes identity transport only: frozen policy,
// credentials, tools, budgets and model settings stay untouched.
func NewFinalizationBindScopeHook(agent *sessionAgent) func(context.Context, SessionAgentCall) error {
	return func(ctx context.Context, call SessionAgentCall) error {
		if call.finalizationAuthorities.Requirement() == nil {
			return nil
		}
		if agent == nil {
			return session.ErrAgentFinalizationDriverUnavailable
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		trusted, err := finalizationLogicalIdentity(call.finalizationAuthorities, call.SessionID, call.LogicalCallID)
		if err != nil {
			return err
		}
		if trusted != call.LogicalCallID {
			return session.ErrAgentFinalizationChanged
		}
		return agent.bindFinalizationDriverCall(call)
	}
}
