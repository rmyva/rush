package session

import (
	"context"
	"database/sql"
	"fmt"
)

// FinalizationDescendantsFacts counts the owner's live descendant work inside
// the seal's writer transaction; the counter covers only active incarnations.
type FinalizationDescendantsFacts interface {
	FinalizationActiveDescendantsTx(ctx context.Context, tx *sql.Tx, ownerSessionID string) (int, error)
}

// FinalizationQueuedCallsFacts counts the owner's unfinished queued calls and
// continuations inside the seal's writer transaction.
type FinalizationQueuedCallsFacts interface {
	FinalizationQueuedCallsTx(ctx context.Context, tx *sql.Tx, ownerSessionID string) (int, error)
}

// FinalizationPhysicalHoldsFacts counts the owner's durable physical holds
// inside the seal's writer transaction.
type FinalizationPhysicalHoldsFacts interface {
	FinalizationPhysicalHoldsTx(ctx context.Context, tx *sql.Tx, ownerSessionID string) (int, error)
}

// FinalizationReactionDebtFacts counts the owner's unsettled reaction debts
// inside the seal's writer transaction.
type FinalizationReactionDebtFacts interface {
	FinalizationReactionDebtTx(ctx context.Context, tx *sql.Tx, ownerSessionID string) (int, error)
}

// compositeFinalizationScopeReader fans the seal's transaction out to the
// per-source facts readers and folds their counters into one complete result.
type compositeFinalizationScopeReader struct {
	ownerSessionID string
	descendants    FinalizationDescendantsFacts
	queued         FinalizationQueuedCallsFacts
	holds          FinalizationPhysicalHoldsFacts
	debt           FinalizationReactionDebtFacts
}

var _ AgentFinalizationScopeReader = compositeFinalizationScopeReader{}

// NewAgentFinalizationScopeReader builds the single production reader
// AgentFinalizationScopeReader. The reader is built per owner: the sources
// receive ownerSessionID here because FinalizationSealScopeFacts carries only
// the transaction. Any nil member, an empty owner, or a missing transaction
// yields incomplete facts without an error, so TrySeal refuses with
// AgentFinalizationSealScopeIncomplete (fail-closed) and the owner waits
// instead of publishing. A member's read error is returned as the facts error
// ("the reader is broken") and TrySeal surfaces it to its caller.
func NewAgentFinalizationScopeReader(
	ownerSessionID string,
	descendants FinalizationDescendantsFacts,
	queued FinalizationQueuedCallsFacts,
	holds FinalizationPhysicalHoldsFacts,
	debt FinalizationReactionDebtFacts,
) AgentFinalizationScopeReader {
	return compositeFinalizationScopeReader{
		ownerSessionID: ownerSessionID,
		descendants:    descendants,
		queued:         queued,
		holds:          holds,
		debt:           debt,
	}
}

// FinalizationSealScopeFacts reads every source inside the passed transaction,
// in declaration order: descendants, queued calls, physical holds, debt.
func (r compositeFinalizationScopeReader) FinalizationSealScopeFacts(ctx context.Context, tx *sql.Tx) (AgentFinalizationScopeFacts, error) {
	if tx == nil || r.ownerSessionID == "" || r.descendants == nil || r.queued == nil || r.holds == nil || r.debt == nil {
		return AgentFinalizationScopeFacts{}, nil
	}
	activeDescendants, err := r.descendants.FinalizationActiveDescendantsTx(ctx, tx, r.ownerSessionID)
	if err != nil {
		return AgentFinalizationScopeFacts{}, fmt.Errorf("read finalization descendants facts: %w", err)
	}
	queuedCalls, err := r.queued.FinalizationQueuedCallsTx(ctx, tx, r.ownerSessionID)
	if err != nil {
		return AgentFinalizationScopeFacts{}, fmt.Errorf("read finalization queued calls facts: %w", err)
	}
	physicalHolds, err := r.holds.FinalizationPhysicalHoldsTx(ctx, tx, r.ownerSessionID)
	if err != nil {
		return AgentFinalizationScopeFacts{}, fmt.Errorf("read finalization physical holds facts: %w", err)
	}
	reactionDebt, err := r.debt.FinalizationReactionDebtTx(ctx, tx, r.ownerSessionID)
	if err != nil {
		return AgentFinalizationScopeFacts{}, fmt.Errorf("read finalization reaction debt facts: %w", err)
	}
	return AgentFinalizationScopeFacts{
		Complete:          true,
		ActiveDescendants: activeDescendants,
		QueuedCalls:       queuedCalls,
		PhysicalHolds:     physicalHolds,
		ReactionDebt:      reactionDebt,
	}, nil
}
