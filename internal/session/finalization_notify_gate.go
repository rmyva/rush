package session

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// FinalizationTerminalGate decides whether a terminal notification may be
// delivered (CLI final output, SDK RunResult, HTTP/WS session-ended, batch
// result). Inertness by construction: no active finalization record means
// allowed, so with the flag off (no records are created) every gate behaves
// like a no-op.
type FinalizationTerminalGate interface {
	// AllowTerminal reports false only while the owner holds a record in an
	// active phase (executing, waiting_work, composing). No record, a terminal
	// phase (accepted, failed, cancelled, interrupted), or an empty owner
	// allows the delivery; an unreadable store refuses (fail-closed).
	AllowTerminal(ctx context.Context, ownerSessionID string) bool

	// AwaitSealed blocks until the owner's record leaves the active phases or
	// there is nothing to wait for. It returns ctx.Err() or a store read error.
	AwaitSealed(ctx context.Context, ownerSessionID string) error
}

// defaultFinalizationTerminalGate is the inert default: everything is allowed.
type defaultFinalizationTerminalGate struct{}

func (defaultFinalizationTerminalGate) AllowTerminal(context.Context, string) bool { return true }

func (defaultFinalizationTerminalGate) AwaitSealed(context.Context, string) error { return nil }

// DefaultFinalizationTerminalGate returns the inert default gate that allows
// every terminal delivery immediately.
func DefaultFinalizationTerminalGate() FinalizationTerminalGate {
	return defaultFinalizationTerminalGate{}
}

// finalizationGatePollInterval backs the polling gate when it is built with a
// non-positive interval.
const finalizationGatePollInterval = 500 * time.Millisecond

// pollingFinalizationTerminalGate is the durable cross-process gate: it reads
// the owner's record on every poll because in-process signals cannot see other
// processes' finalizations.
type pollingFinalizationTerminalGate struct {
	store *AgentFinalizationStore
	poll  time.Duration
}

// NewPollingFinalizationTerminalGate builds the durable polling gate over
// store; poll is the delay between owner-phase reads, and a non-positive poll
// selects finalizationGatePollInterval.
func NewPollingFinalizationTerminalGate(store *AgentFinalizationStore, poll time.Duration) FinalizationTerminalGate {
	if poll <= 0 {
		poll = finalizationGatePollInterval
	}
	return pollingFinalizationTerminalGate{store: store, poll: poll}
}

// hasActiveFinalization reads whether the owner holds an active-phase record
// with one query and never holds a transaction across polls. A nil store, a
// canceled context, or a read error is an error, not "no record"; an empty
// owner provably has no record because Bind refuses empty session ids.
func (g pollingFinalizationTerminalGate) hasActiveFinalization(ctx context.Context, ownerSessionID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if g.store == nil || g.store.conn == nil {
		return false, errors.New("finalization terminal gate store is unavailable")
	}
	if ownerSessionID == "" {
		return false, nil
	}
	var active int
	err := g.store.conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalizations
		WHERE session_id=? AND phase IN ('executing', 'waiting_work', 'composing'))`, ownerSessionID).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("read active finalization record: %w", err)
	}
	return active != 0, nil
}

// AllowTerminal refuses only a readable active record; unreadable state
// refuses too, so a broken store can never publish a premature terminal.
func (g pollingFinalizationTerminalGate) AllowTerminal(ctx context.Context, ownerSessionID string) bool {
	active, err := g.hasActiveFinalization(ctx, ownerSessionID)
	return err == nil && !active
}

// AwaitSealed polls until the owner's record leaves the active phases, the
// context is done, or a read fails.
func (g pollingFinalizationTerminalGate) AwaitSealed(ctx context.Context, ownerSessionID string) error {
	ticker := time.NewTicker(g.poll)
	defer ticker.Stop()
	for {
		active, err := g.hasActiveFinalization(ctx, ownerSessionID)
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
