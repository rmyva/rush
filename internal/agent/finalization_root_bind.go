package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// rootFinalizationGeneration is the durable generation of a root run's
// finalization record: one root incarnation, generation 1.
const rootFinalizationGeneration = 1

// startExecution is the Run entry (agent_run.go): the root bind, then the
// wiring's driver start. A nil wiring (flag off) is the no-op pass-through:
// the context, the call and a no-op cleanup come back unchanged.
func (a *sessionAgent) startExecution(ctx context.Context, call SessionAgentCall) (context.Context, SessionAgentCall, func(), error) {
	return a.finalizationWiring().bindRootFinalization(ctx, a, call)
}

// bindRootFinalization binds the owner's finalization record for a FRESH root
// call: retire stale active incarnations of the session, bind identity
// generation 1, carry the authority in the context and on the call, register
// it in the process registry so runner-side batch commits can admit, then
// run the existing driver start. A call that already carries a requirement
// for its own session (descendants, queue-managed calls), a sub-agent call,
// a driver-bound call, or a session without the finalization service skips
// the bind and passes straight through. Rebinding an identity whose record
// already left the active phases (an accepted rerun) applies no
// finalization and reports no error: the call runs the legacy path. Every
// other bind failure fails closed and refuses the run.
func (w *finalizationWiring) bindRootFinalization(ctx context.Context, a *sessionAgent, call SessionAgentCall) (context.Context, SessionAgentCall, func(), error) {
	if w == nil {
		return ctx, call, func() {}, nil
	}
	if !rootBindEligible(a, call) {
		return passThroughExecution(ctx, w, call)
	}
	svc, ok := a.sessions.(session.AgentFinalizationService)
	if !ok {
		return passThroughExecution(ctx, w, call)
	}
	if w.authorities == nil {
		return nil, call, nil, errors.New("finalization wiring: authority registry unavailable")
	}
	if w.store == nil {
		return nil, call, nil, errors.New("finalization wiring: store unavailable")
	}
	if _, err := w.store.RetireStaleActive(ctx, call.SessionID, call.LogicalCallID); err != nil {
		return nil, call, nil, err
	}
	authority, err := svc.BindAgentFinalization(ctx, session.AgentFinalizationIdentity{
		SessionID: call.SessionID, LogicalCallID: call.LogicalCallID, Generation: rootFinalizationGeneration,
	})
	if err != nil {
		if errors.Is(err, session.ErrAgentFinalizationChanged) {
			// The logical call's record already left the active phases (an
			// accepted rerun): finalization is not applicable, the call
			// goes legacy without an execution.
			return ctx, call, func() {}, nil
		}
		return nil, call, nil, err
	}
	boundCtx, err := session.WithAgentFinalizationContext(ctx, authority)
	if err != nil {
		return nil, call, nil, err
	}
	call.finalizationAuthorities = session.CaptureAgentFinalizationContext(boundCtx)
	unregisterAuthority := w.authorities.Register(call.SessionID, authority)
	execCtx, unregisterExecution, err := w.startExecution(boundCtx, call)
	if err != nil {
		unregisterAuthority()
		return nil, call, nil, err
	}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			unregisterAuthority()
			if execCtx != nil && execCtx.Err() != nil {
				// The run unwound through cancellation while its record is
				// still active: the incarnation is over, mark it cancelled
				// so the terminal vocabulary and the ticket fencing agree.
				retireCtx, retireCancel := context.WithTimeout(context.WithoutCancel(execCtx), 5*time.Second)
				defer retireCancel()
				if _, retireErr := w.store.RetireCancelledLogical(retireCtx, call.SessionID, call.LogicalCallID); retireErr != nil {
					slog.Warn("Failed to retire the cancelled finalization record",
						"session_id", call.SessionID, "err", retireErr)
				}
			}
			unregisterExecution()
		})
	}
	return execCtx, call, cleanup, nil
}

// rootBindEligible reports whether the call is a fresh root incarnation that
// may bind its own finalization record.
func rootBindEligible(a *sessionAgent, call SessionAgentCall) bool {
	if a == nil || a.isSubAgent || call.driverGeneration != 0 {
		return false
	}
	if call.SessionID == "" || call.LogicalCallID == "" {
		return false
	}
	if requirement := call.finalizationAuthorities.Requirement(); requirement != nil {
		for _, identity := range requirement.Authorities {
			if identity.SessionID == call.SessionID {
				return false
			}
		}
	}
	return true
}

// passThroughExecution adapts the wiring driver start to the entry
// signature for calls that skip the root bind.
func passThroughExecution(ctx context.Context, w *finalizationWiring, call SessionAgentCall) (context.Context, SessionAgentCall, func(), error) {
	execCtx, unregister, err := w.startExecution(ctx, call)
	return execCtx, call, unregister, err
}
