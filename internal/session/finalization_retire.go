package session

import (
	"context"
	"errors"
	"fmt"
)

// RetireStaleActive marks every active finalization record of sessionID that
// belongs to a different logical call as interrupted. It is called only at a
// root run start that holds the session's inter-process lock, so every other
// active incarnation is a dead one. keepLogicalCallID names the incarnation
// that stays; an empty value retires every active record of the session. The
// call is idempotent: rows already terminal are never touched and a second
// call finds no matching rows. It returns the number of retired rows.
func (s *AgentFinalizationStore) RetireStaleActive(ctx context.Context, sessionID, keepLogicalCallID string) (int, error) {
	if s == nil || s.conn == nil {
		return 0, errors.New("finalization retire store unavailable")
	}
	if sessionID == "" {
		return 0, errors.New("finalization retire: missing session")
	}
	result, err := s.conn.ExecContext(ctx, `UPDATE session_agent_finalizations
		SET phase='interrupted', revision=revision+1
		WHERE session_id=? AND phase IN ('executing', 'waiting_work', 'composing')
		AND logical_call_id<>?`, sessionID, keepLogicalCallID)
	if err != nil {
		return 0, fmt.Errorf("retire stale finalization records: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("retire stale finalization records: %w", err)
	}
	return int(changed), nil
}

// RetireCancelledLogical marks the owner's active record of exactly one
// logical call as cancelled, with the same revision bump the other
// transitions use. Terminal rows are never touched and the call is
// idempotent. It returns the number of retired rows.
func (s *AgentFinalizationStore) RetireCancelledLogical(ctx context.Context, sessionID, logicalCallID string) (int, error) {
	if s == nil || s.conn == nil {
		return 0, errors.New("finalization retire store unavailable")
	}
	if sessionID == "" {
		return 0, errors.New("finalization retire: missing session")
	}
	if logicalCallID == "" {
		return 0, errors.New("finalization retire: missing logical call")
	}
	result, err := s.conn.ExecContext(ctx, `UPDATE session_agent_finalizations
		SET phase='cancelled', revision=revision+1
		WHERE session_id=? AND logical_call_id=?
		AND phase IN ('executing', 'waiting_work', 'composing')`, sessionID, logicalCallID)
	if err != nil {
		return 0, fmt.Errorf("retire cancelled finalization record: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("retire cancelled finalization record: %w", err)
	}
	return int(changed), nil
}
