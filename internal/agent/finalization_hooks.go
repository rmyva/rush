package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/PHPCraftdream/rush/internal/session"
)

// ErrFinalizationHooksIncomplete marks the installation of a partially filled
// hooks set, which is refused instead of silently running with missing steps.
var ErrFinalizationHooksIncomplete = errors.New("finalization driver hooks incomplete")

// FinalizationDriverHooks is the single place W1 wires lanes L4 (BindScope,
// RecoverScope), L5 (FinalizeTurn) and L6 (OnCancel) into the driver. The zero
// value keeps the inert legacy behavior: no production call site invokes any
// hook today, and the flag-off path must stay byte-identical to main. With the
// flag on, W1 must install every hook; a partially filled set is an
// installation error (fail-closed), not a quiet no-op.
type FinalizationDriverHooks struct {
	// BindScope binds the finalization scope at driver start, before the
	// first step (L4).
	BindScope func(context.Context, SessionAgentCall) error
	// RecoverScope recovers the persisted candidate at start/restart
	// (RecoverCandidate + resumeCandidateSerial, L4).
	RecoverScope func(context.Context, *finalizationExecution) (session.AgentFinalizationRecovery, error)
	// FinalizeTurn runs the wait/compose/seal loop at the end of a turn (L5).
	FinalizeTurn func(context.Context, *turnStream) error
	// OnCancel is the cancellation entry from the coordinator interrupt (L6).
	OnCancel func(sessionID string)
}

// IsZero reports whether no hook is installed.
func (h FinalizationDriverHooks) IsZero() bool {
	return h.BindScope == nil && h.RecoverScope == nil && h.FinalizeTurn == nil && h.OnCancel == nil
}

// Validate accepts the fully empty set (inert legacy behavior) and the fully
// installed set, and refuses a partially filled set by naming the unset hooks.
func (h FinalizationDriverHooks) Validate() error {
	var unset []string
	if h.BindScope == nil {
		unset = append(unset, "BindScope")
	}
	if h.RecoverScope == nil {
		unset = append(unset, "RecoverScope")
	}
	if h.FinalizeTurn == nil {
		unset = append(unset, "FinalizeTurn")
	}
	if h.OnCancel == nil {
		unset = append(unset, "OnCancel")
	}
	if len(unset) == 0 || len(unset) == 4 {
		return nil
	}
	return fmt.Errorf("%w: unset hooks: %s", ErrFinalizationHooksIncomplete, strings.Join(unset, ", "))
}
