package session

import (
	"context"
	"errors"

	"github.com/PHPCraftdream/rush/internal/message"
)

// ErrFinalizationBatchResultReadMissing is returned when a gated batch-result
// wrapper is built without an underlying read.
var ErrFinalizationBatchResultReadMissing = errors.New(
	"finalization batch result read is missing")

// FinalizationTerminalFinishReason reports whether a finish reason ends the
// agent's turn instead of continuing it. It mirrors isTerminalFinishReason in
// internal/cmd/sessions_watch.go: end_turn, max_tokens, canceled and error are
// terminal; tool_use and unknown mean the loop is still mid-step.
func FinalizationTerminalFinishReason(reason message.FinishReason) bool {
	switch reason {
	case message.FinishReasonEndTurn,
		message.FinishReasonMaxTokens,
		message.FinishReasonCanceled,
		message.FinishReasonError:
		return true
	default:
		return false
	}
}

// FinalizationTerminalMessage reports whether a message event is the terminal
// answer of its session: an assistant message carrying a non-partial Finish
// whose reason is terminal. Partial checkpoint finishes (the provisional
// finalization candidate) are never terminal.
func FinalizationTerminalMessage(m message.Message) bool {
	if m.Role != message.Assistant {
		return false
	}
	finish := m.FinishPart()
	if finish == nil || finish.Partial {
		return false
	}
	return FinalizationTerminalFinishReason(finish.Reason)
}

// FinalizationSessionEnded reports whether a session event payload marks the
// end of the session: a non-empty EndedReason on the row.
func FinalizationSessionEnded(s Session) bool {
	return s.EndedReason != ""
}

// FinalizationBatchResultRead reads the owner's batch completion; it matches
// (*BatchStore).BatchDeliveryOneItemCompletion and
// (*BatchStore).BatchDeliveryRootEnvelope so either binds as a method value.
// ok=false means no deliverable completion yet, error is a read failure.
type FinalizationBatchResultRead func(
	ctx context.Context, owner, root string,
) (BatchDeliveryEnvelope, bool, error)

// FinalizationGatedBatchResult filters ONE batch-result read through the
// owner's terminal gate: while the owner holds an active finalization record
// the completion is withheld as (BatchDeliveryEnvelope{}, false, nil), which
// the batch consumers' durable poll (LoadDone/AwaitDone) already treats as
// "keep waiting", so no consumer can observe a batch result before the
// parent's answer is sealed. A nil gate is the inert wiring: the read result
// is returned byte-for-byte unchanged. A read failure and a canceled context
// are returned as errors, never as a delivered result.
func FinalizationGatedBatchResult(
	ctx context.Context,
	gate FinalizationTerminalGate,
	owner, root string,
	read FinalizationBatchResultRead,
) (BatchDeliveryEnvelope, bool, error) {
	if read == nil {
		return BatchDeliveryEnvelope{}, false, ErrFinalizationBatchResultReadMissing
	}
	// Inert passthrough: a nil gate must not touch the context or the gate.
	if gate == nil {
		return read(ctx, owner, root)
	}
	if err := ctx.Err(); err != nil {
		return BatchDeliveryEnvelope{}, false, err
	}
	// Fail-closed withholding: an unreadable gate store also refuses here.
	if !gate.AllowTerminal(ctx, owner) {
		return BatchDeliveryEnvelope{}, false, nil
	}
	return read(ctx, owner, root)
}

// FinalizationAwaitGatedBatchResult blocks until the owner's record leaves the
// active phases (AwaitSealed) and then reads the completion once. A nil gate
// is the inert default and never waits. AwaitSealed errors — cancellation or
// a store read failure — are returned as errors and the read never runs, so a
// broken gate can never turn into a delivered "success".
func FinalizationAwaitGatedBatchResult(
	ctx context.Context,
	gate FinalizationTerminalGate,
	owner, root string,
	read FinalizationBatchResultRead,
) (BatchDeliveryEnvelope, bool, error) {
	if read == nil {
		return BatchDeliveryEnvelope{}, false, ErrFinalizationBatchResultReadMissing
	}
	if gate == nil {
		gate = DefaultFinalizationTerminalGate()
	}
	if err := gate.AwaitSealed(ctx, owner); err != nil {
		return BatchDeliveryEnvelope{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return BatchDeliveryEnvelope{}, false, err
	}
	return read(ctx, owner, root)
}
