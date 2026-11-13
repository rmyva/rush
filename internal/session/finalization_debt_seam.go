package session

import "context"

// AgentFinalizationDebtSettler idempotently settles the owner's outstanding
// reaction and notification debts. The implementation belongs to lane L3, and
// the wait/compose loop (L5) calls it before retrying the seal. It returns the
// number of records settled by this call; a repeat call returns 0, and a
// settled debt never counts again.
type AgentFinalizationDebtSettler interface {
	SettleFinalizationDebts(ctx context.Context, ownerSessionID string) (int, error)
}
