package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// finalizationRecordPhaseActive reports whether a durable record phase still
// allows recovery work. The active phases mirror the frozen fencing SQL; the
// session package does not export a helper, and the frozen files must not be
// edited to add one.
func finalizationRecordPhaseActive(phase string) bool {
	switch phase {
	case "executing", "waiting_work", "composing":
		return true
	default:
		return false
	}
}

// NewFinalizationRecoverScopeHook builds the start/restart RecoverScope hook
// (lane L4). The returned hook re-adopts or fails the candidate a previous
// process recorded through the frozen recoverFinalizationCandidate, which
// also raises the gate's step counter above the durable high-water mark
// (Record.CandidateSerial), so a restarted owner is never fenced by its own
// history. The session.AgentFinalizationRecovery verdict (adopted, failed or
// none) is passed through unchanged for the wait/compose loop; installing
// that loop is lane L5's seam, not this hook's.
//
// A record that already left the active phases is terminal: recovery reports
// no candidate and touches nothing, so a restart after a seal is idempotent
// and never publishes twice. A cancelled owner still fails with
// context.Canceled through the frozen recovery path, and any read error is
// returned without clearing the recorded candidate (fail-closed).
func NewFinalizationRecoverScopeHook(messages message.Service) func(context.Context, *finalizationExecution) (session.AgentFinalizationRecovery, error) {
	return func(ctx context.Context, execution *finalizationExecution) (session.AgentFinalizationRecovery, error) {
		if err := ctx.Err(); err != nil {
			return session.AgentFinalizationNoCandidate, err
		}
		if execution == nil || execution.gate == nil || execution.scope.store == nil {
			return session.AgentFinalizationNoCandidate, ErrFinalizationFactsUnavailable
		}
		record, err := execution.scope.store.Snapshot(ctx, execution.scope.identity)
		if err != nil {
			return session.AgentFinalizationNoCandidate, err
		}
		if !finalizationRecordPhaseActive(record.Phase) {
			execution.gate.resumeCandidateSerial(uint64(record.CandidateSerial))
			return session.AgentFinalizationNoCandidate, nil
		}
		return recoverFinalizationCandidate(ctx, messages, execution.scope, execution.gate)
	}
}
