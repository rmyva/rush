package agent

import "sync"

// FinalizationCancelLookup resolves an owner session id to the in-process
// finalization work that must stop. The coordinator-interrupt side knows only
// the session id; the process-local registry below is the other half, wired
// by W1 together with this hook.
type FinalizationCancelLookup interface {
	// CancelFinalizationSession cancels every finalization execution currently
	// registered for sessionID. Unknown sessions are a no-op.
	CancelFinalizationSession(sessionID string)
}

// finalizationCancelRegistry is the minimal in-process index from owner
// session id to the live finalization executions of that session. There is no
// package-level registry: a finalizationExecution otherwise lives only in the
// driver context, so a coordinator interrupt could not find it. W1 registers
// an execution right after a successful withFinalizationExecution and
// unregisters it when the run's generation unwinds; the registry itself never
// owns execution lifetime.
type finalizationCancelRegistry struct {
	mu    sync.Mutex
	byKey map[string]map[*finalizationExecution]struct{}
}

func newFinalizationCancelRegistry() *finalizationCancelRegistry {
	return &finalizationCancelRegistry{byKey: make(map[string]map[*finalizationExecution]struct{})}
}

// register indexes one execution under its session id. Executions with no
// session, no gate, or a nil pointer are ignored: cancelling must never panic
// on a half-built run.
func (r *finalizationCancelRegistry) register(execution *finalizationExecution) {
	if r == nil || execution == nil || execution.sessionID == "" || execution.gate == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	targets, ok := r.byKey[execution.sessionID]
	if !ok {
		targets = make(map[*finalizationExecution]struct{})
		r.byKey[execution.sessionID] = targets
	}
	targets[execution] = struct{}{}
}

// unregister removes exactly this execution (pointer identity) and leaves any
// other execution of the same session untouched. Registering again later is a
// fresh, independent entry.
func (r *finalizationCancelRegistry) unregister(execution *finalizationExecution) {
	if r == nil || execution == nil || execution.sessionID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	targets, ok := r.byKey[execution.sessionID]
	if !ok {
		return
	}
	delete(targets, execution)
	if len(targets) == 0 {
		delete(r.byKey, execution.sessionID)
	}
}

// cancelSession cancels every gate currently registered for sessionID. The
// gates are snapshotted under the registry lock and cancelled without it, so
// the call can never block on gate activity, and gate.cancel itself is
// idempotent and wakes waiters without waiting for them.
func (r *finalizationCancelRegistry) cancelSession(sessionID string) {
	if r == nil || sessionID == "" {
		return
	}
	r.mu.Lock()
	gates := make([]*finalizationGate, 0, len(r.byKey[sessionID]))
	for execution := range r.byKey[sessionID] {
		if execution != nil && execution.gate != nil {
			gates = append(gates, execution.gate)
		}
	}
	r.mu.Unlock()
	for _, gate := range gates {
		gate.cancel()
	}
}

// CancelFinalizationSession satisfies FinalizationCancelLookup so the
// registry can be passed to NewFinalizationOnCancelHook directly.
func (r *finalizationCancelRegistry) CancelFinalizationSession(sessionID string) {
	r.cancelSession(sessionID)
}

// NewFinalizationOnCancelHook builds the coordinator-interrupt OnCancel hook
// (lane L6). The returned hook is the in-process half of finalization
// cancellation: it looks the session's live executions up in the registry W1
// maintains and cancels their gates, which forbids further mutations and
// seals and wakes any waitState waiter. The durable half (cancel_requested on
// the session row, refused SealTx and StoreCandidateTx) is already implemented
// in the session package and is deliberately not duplicated here.
//
// The hook is inert by construction: a nil lookup, unknown session ids and
// empty ids are no-ops, it never blocks and never panics, and it has no
// production call site until W1 installs it.
func NewFinalizationOnCancelHook(lookup FinalizationCancelLookup) func(sessionID string) {
	return func(sessionID string) {
		if lookup == nil {
			return
		}
		lookup.CancelFinalizationSession(sessionID)
	}
}
