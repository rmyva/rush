package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/session"
)

// ErrFinalizationSettlerUnavailable refuses to wait without a debt settler:
// without one an open debt would never be settled and the seal would refuse
// forever, so fail-closed beats a silent endless wait.
var ErrFinalizationSettlerUnavailable = errors.New("finalization debt settler unavailable")

// settleFinalizationDebts settles the owner's reaction debts once per wait
// entry. A settlement error is logged and swallowed: the settler is
// idempotent, the retry happens on the next wait entry, and an unsettled debt
// keeps refusing the seal anyway; only the missing settler is fatal.
func settleFinalizationDebts(ctx context.Context, settler session.AgentFinalizationDebtSettler, ownerSessionID string) error {
	if settler == nil {
		return fmt.Errorf("%w: no debt settler is installed", ErrFinalizationSettlerUnavailable)
	}
	if _, err := settler.SettleFinalizationDebts(ctx, ownerSessionID); err != nil {
		slog.Warn("Finalization debt settlement failed", "error", err)
	}
	return nil
}
