// Subtree run-cost reads and the budget freeze (#1130): the ONLY "cost
// including children" is a query over the delegation tree, never a value
// transferred between rows.

package session

import (
	"context"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/pubsub"
)

// SubtreeBudget — see Service.SubtreeBudget. Budget = spent - base, clamped
// at 0: a deleted child can leave the base above the remaining spent.
func (s *service) SubtreeBudget(ctx context.Context, sessionID string) (float64, error) {
	spent, err := s.q.GetSubtreeSpent(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("read subtree spent: %w", err)
	}
	base, err := s.q.GetSessionCostBase(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("read cost base: %w", err)
	}
	return max(0, spent-base), nil
}

// SubtreeSpent — see Service.SubtreeSpent.
func (s *service) SubtreeSpent(ctx context.Context, sessionID string) (float64, error) {
	spent, err := s.q.GetSubtreeSpent(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("read subtree spent: %w", err)
	}
	return spent, nil
}

// SubtreeUpdatedAt — see Service.SubtreeUpdatedAt.
func (s *service) SubtreeUpdatedAt(ctx context.Context, sessionID string) (int64, error) {
	updated, err := s.q.GetSubtreeUpdatedAt(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("read subtree activity: %w", err)
	}
	return updated, nil
}

const resetCostBaseSQL = `
WITH RECURSIVE sub(node) AS (
    SELECT id FROM sessions WHERE id = ?
    UNION
    SELECT child.id FROM sessions child JOIN sub ON child.cost_parent_id = sub.node
)
UPDATE sessions
SET cost_base = (
        SELECT CAST(COALESCE(SUM(n.cost_self), 0) AS REAL)
        FROM sub JOIN sessions n ON n.id = sub.node
    ),
    updated_at = strftime('%s', 'now')
WHERE id = ?
`

// ResetCostBase — see Service.ResetCostBase. ONE statement computes the
// subtree snapshot and writes cost_base atomically; cost_self is untouched.
func (s *service) ResetCostBase(ctx context.Context, sessionID string) error {
	if _, err := s.db.ExecContext(ctx, resetCostBaseSQL, sessionID, sessionID); err != nil {
		return fmt.Errorf("freeze cost base: %w", err)
	}
	if sess, err := s.Get(ctx, sessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}
