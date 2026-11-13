package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/message"
)

// ErrFinalizationComposeUnavailable refuses to rebuild the answer when there
// is nothing recomposable to rebuild; it is fail-closed, never a silent pass.
var ErrFinalizationComposeUnavailable = errors.New("finalization compose unavailable")

// FinalizationComposeInput carries what a recomposition needs: the turn, its
// execution, the cloned provisional answer, and a fresh stamp from
// gate.beginCandidate.
type FinalizationComposeInput struct {
	Turn        *turnStream
	Execution   *finalizationExecution
	Provisional message.Message
	Stamp       finalizationStamp
}

// FinalizationComposeFunc rebuilds the provisional answer under the fresh
// stamp and returns the persisted candidate; nil deps.Compose uses the
// built-in defaultComposeFinalizationCandidate.
type FinalizationComposeFunc func(context.Context, FinalizationComposeInput) (persistedFinalizationCandidate, error)

// composeFinalizationCandidate rebuilds the provisional answer under a fresh
// stamp and writes the fresh stamp and persisted candidate back to the turn.
// FinalizeTurn runs after the callback sequence, so these fields are only
// touched here and by the loop itself.
func composeFinalizationCandidate(ctx context.Context, ts *turnStream, execution *finalizationExecution, deps FinalizationWaitDeps) error {
	ts.mu.Lock()
	if ts.currentAssistant == nil {
		ts.mu.Unlock()
		return fmt.Errorf("%w: no provisional assistant answer to recompose", ErrFinalizationComposeUnavailable)
	}
	snapshot := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	stamp, err := execution.gate.beginCandidate(ctx, execution.read)
	if err != nil {
		return fmt.Errorf("finalization fresh stamp failed: %w", err)
	}
	var persisted persistedFinalizationCandidate
	if deps.Compose != nil {
		persisted, err = deps.Compose(ctx, FinalizationComposeInput{
			Turn: ts, Execution: execution, Provisional: snapshot, Stamp: stamp,
		})
	} else {
		persisted, err = defaultComposeFinalizationCandidate(ctx, ts, execution, snapshot, stamp)
	}
	if err != nil {
		return err
	}
	ts.finalizationStamp = stamp
	ts.finalizationCandidate = persisted
	return nil
}

// defaultComposeFinalizationCandidate re-marks the cloned provisional answer
// with the fresh stamp identity and persists it; creds, role, tools and
// budgets are preserved by construction because it is the same turnStream.
func defaultComposeFinalizationCandidate(ctx context.Context, ts *turnStream, execution *finalizationExecution, snapshot message.Message, stamp finalizationStamp) (persistedFinalizationCandidate, error) {
	finish := snapshot.FinishPart()
	if finish == nil || !finish.Partial || finish.FinalizationKey != execution.logicalID {
		return persistedFinalizationCandidate{}, fmt.Errorf("%w: the provisional answer carries no recomposable candidate footer", ErrFinalizationComposeUnavailable)
	}
	identity := message.FinalizationIdentity{
		Key: execution.logicalID, Generation: stamp.epoch,
		Revision: stamp.durableRevision, Serial: stamp.serial,
	}
	if err := snapshot.MarkFinalizationCandidate(identity); err != nil {
		return persistedFinalizationCandidate{}, err
	}
	return persistFinalizationCandidate(ctx, ts.a.sessions, ts.a.messages, execution.scope, identity, snapshot)
}

// finalizationPublishClosure builds the publication gate.trySeal calls back:
// it presents the turn's persisted candidate, verifies the accepted row, and
// reports a durable scope change through the scopeChanged flag so the loop can
// wait instead of recomposing. It runs only under the gate's mutex.
func finalizationPublishClosure(ts *turnStream, execution *finalizationExecution, scopeChanged *bool) func(context.Context, finalizationCandidate) error {
	return func(ctx context.Context, candidate finalizationCandidate) error {
		persisted := ts.finalizationCandidate
		if candidate.messageID != persisted.messageID || candidate.hash != persisted.hash {
			*scopeChanged = true
			return ErrFinalizationScopeChanged
		}
		accepted, err := publishFinalizationCandidate(ctx, ts.a.sessions, ts.a.messages, execution.scope, persisted.identity, candidate)
		if err != nil {
			if errors.Is(err, ErrFinalizationScopeChanged) {
				*scopeChanged = true
			}
			return err
		}
		// A successful publication clears the flag: the flag survives only
		// while the durable side keeps answering "not yet".
		finish := accepted.FinishPart()
		if !accepted.IsFinished() || accepted.ID != candidate.messageID || accepted.SessionID != ts.call.SessionID ||
			finish == nil || finish.Reason != message.FinishReasonEndTurn ||
			finish.FinalizationKey != persisted.identity.Key ||
			finish.FinalizationGeneration != persisted.identity.Generation ||
			finish.FinalizationRevision != persisted.identity.Revision ||
			finish.FinalizationSerial != persisted.identity.Serial ||
			finish.FinalizationHash != candidate.hash {
			return fmt.Errorf("%w: publisher returned no matching accepted row", ErrFinalizationFactsUnavailable)
		}
		ts.mu.Lock()
		*ts.currentAssistant = accepted
		ts.mu.Unlock()
		return nil
	}
}
