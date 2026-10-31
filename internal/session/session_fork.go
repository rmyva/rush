// Session forking: ForkOptions plus the single-transaction clone
// (ForkSessionTx) and its web-button wrapper (ForkSession), copying a
// session row and its messages into a brand-new session.

package session

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/google/uuid"
)

// ForkOptions carries the knobs ForkSessionTx supports beyond "copy
// everything from srcID into a new top-level session". See the Service
// interface doc on ForkSessionTx for the zero-value default of each field.
type ForkOptions struct {
	// NewID is the caller-chosen ID for the forked session. Empty means
	// generate a fresh uuid.New().String().
	NewID string
	// Title is the forked session's title. Empty means "<src title> fork".
	Title string
	// ParentID sets the fork's parent_session_id, making it a child session
	// (as `rush sessions fork --child` does). Empty means top-level (no
	// parent) — the web fork button's behavior.
	ParentID string
	// LimitMsgs truncates the copy to the first LimitMsgs messages
	// (1-indexed). 0 means "copy every message". A non-zero value outside
	// 1..len(source messages) is rejected.
	LimitMsgs int
}

// ForkSession is a thin wrapper around ForkSessionTx using the web fork
// button's defaults: server-generated ID, top-level session, every message
// copied. It additionally publishes a pubsub.CreatedEvent after commit,
// since the web path and its subscribers live in the same process.
func (s *service) ForkSession(ctx context.Context, srcID, title string) (Session, error) {
	fork, _, err := s.ForkSessionTx(ctx, srcID, ForkOptions{Title: title})
	if err != nil {
		return Session{}, err
	}
	s.Publish(pubsub.CreatedEvent, fork)
	return fork, nil
}

// ForkSessionTx clones srcID into a brand-new session in a single DB
// transaction. It creates a fresh session row, copies the source's models,
// system prompt, reasoning effort, and todos/deleted_todos, and copies the
// first o.LimitMsgs messages (or all of them, when o.LimitMsgs is 0)
// verbatim — all inside one tx so a failure at any point (e.g. the Nth
// message copy) rolls back the new session row and every message copied so
// far. The caller gets an error and no half-built fork is left behind.
// Mirrors the transactional shape of Delete and TransferChildCostToParent.
//
// See the Service interface doc on ForkSessionTx for ForkOptions defaults
// and the pubsub-publish contract (this method does NOT publish; callers do).
func (s *service) ForkSessionTx(ctx context.Context, srcID string, o ForkOptions) (Session, int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, 0, fmt.Errorf("begin fork transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)

	// Read the source inside the tx so the copy is consistent with itself.
	src, err := qtx.GetSessionByID(ctx, srcID)
	if err != nil {
		return Session{}, 0, fmt.Errorf("load source session: %w", err)
	}

	srcMsgs, err := qtx.ListMessagesBySession(ctx, srcID)
	if err != nil {
		return Session{}, 0, fmt.Errorf("list source messages: %w", err)
	}

	// LimitMsgs == 0 means "copy everything" and is always valid, including
	// against an empty source (forking a brand-new, message-less session is
	// a legitimate operation). A non-zero LimitMsgs must fall within
	// 1..len(srcMsgs).
	limit := o.LimitMsgs
	if limit == 0 {
		limit = len(srcMsgs)
	} else if limit < 1 || limit > len(srcMsgs) {
		return Session{}, 0, fmt.Errorf("--at %d is out of range (1..%d)", limit, len(srcMsgs))
	}
	srcMsgs = srcMsgs[:limit]

	resolvedTitle := o.Title
	if resolvedTitle == "" {
		resolvedTitle = src.Title + " fork"
	}

	forkID := o.NewID
	if forkID == "" {
		forkID = uuid.New().String()
	}

	// The fork belongs to THIS process's workspace, not to the source's: a
	// fork is the one supported way to continue a session's history in a
	// different checkout (#1142 step C). Binding it to the source's workspace
	// would produce a row no process here may drive.
	createParams := s.bindWorkspace(db.CreateSessionParams{
		ID:     forkID,
		Title:  resolvedTitle,
		Origin: src.Origin,
		// No CostParentID: a fork is its OWN cost root (#1130), even when it
		// points at a parent for history.
	})
	if o.ParentID != "" {
		createParams.ParentSessionID = sql.NullString{String: o.ParentID, Valid: true}
	}
	if _, err := qtx.CreateSession(ctx, createParams); err != nil {
		return Session{}, 0, fmt.Errorf("create forked session: %w", err)
	}

	// Copy the source's model selection, system prompt, reasoning effort,
	// and todos onto the fork via column-scoped UPDATEs routed through qtx
	// so they share the tx.
	if err := qtx.UpdateSessionModels(ctx, db.UpdateSessionModelsParams{
		SmartModelProvider: src.SmartModelProvider,
		SmartModelID:       src.SmartModelID,
		FastModelProvider:  src.FastModelProvider,
		FastModelID:        src.FastModelID,
		ID:                 forkID,
	}); err != nil {
		return Session{}, 0, fmt.Errorf("copy models into fork: %w", err)
	}
	if err := qtx.UpdateSessionWorkerReviewerModels(ctx, db.UpdateSessionWorkerReviewerModelsParams{
		WorkerModelProvider:   src.WorkerModelProvider,
		WorkerModelID:         src.WorkerModelID,
		ReviewerModelProvider: src.ReviewerModelProvider,
		ReviewerModelID:       src.ReviewerModelID,
		ID:                    forkID,
	}); err != nil {
		return Session{}, 0, fmt.Errorf("copy worker/reviewer models into fork: %w", err)
	}
	if err := qtx.UpdateSessionSystemPrompt(ctx, db.UpdateSessionSystemPromptParams{
		SystemPrompt: src.SystemPrompt,
		ID:           forkID,
	}); err != nil {
		return Session{}, 0, fmt.Errorf("copy system prompt into fork: %w", err)
	}
	if err := qtx.UpdateSessionReasoningEffort(ctx, db.UpdateSessionReasoningEffortParams{
		SmartModelReasoningEffort: src.SmartModelReasoningEffort,
		FastModelReasoningEffort:  src.FastModelReasoningEffort,
		ID:                        forkID,
	}); err != nil {
		return Session{}, 0, fmt.Errorf("copy reasoning effort into fork: %w", err)
	}
	if err := qtx.UpdateSessionWorkerReviewerReasoningEffort(ctx, db.UpdateSessionWorkerReviewerReasoningEffortParams{
		WorkerModelReasoningEffort:   src.WorkerModelReasoningEffort,
		ReviewerModelReasoningEffort: src.ReviewerModelReasoningEffort,
		ID:                           forkID,
	}); err != nil {
		return Session{}, 0, fmt.Errorf("copy worker/reviewer reasoning effort into fork: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET todos = ?, deleted_todos = ?, updated_at = strftime('%s', 'now') WHERE id = ?`,
		src.Todos,
		src.DeletedTodos,
		forkID,
	); err != nil {
		return Session{}, 0, fmt.Errorf("copy todos into fork: %w", err)
	}

	// Copy every selected message verbatim. Parts is carried across as the
	// raw JSON blob (no decode/re-encode round-trip), so the fork is a
	// faithful copy of the source history. Any copy error aborts the whole
	// transaction.
	//
	// Deliberately no per-message pubsub.CreatedEvent here (unlike
	// message.Service.Create, which the old non-transactional path went
	// through): this loop writes via qtx.CreateMessage directly against the
	// tx, bypassing the message package's Service/Broker entirely, so there
	// is no message.Service handle available to publish through from inside
	// ForkSessionTx. Publishing would also need to wait until AFTER the
	// commit below (a subscriber must never observe a message row from an
	// uncommitted, possibly-rolled-back tx), which would mean re-reading
	// every copied message post-commit just to build event payloads.
	// Checked whether any subscriber actually needs incremental per-message
	// events for a fork: the only consumer is the web UI
	// (internal/server/events.go forwards message.Service's broker to
	// EventMessageCreated/Updated), and its client-side handler for the
	// session-level fork event does NOT rely on incremental message
	// events — web/src/useWS.ts's "session_created" handler unconditionally
	// calls `ws.send("load_messages", { sessionID: s.ID })` for every new
	// session (fork included), which does a full re-fetch of the session's
	// messages. So a client attached at fork time still ends up with a
	// complete, correct transcript. If a future subscriber needs
	// incremental per-message fork events, publish them AFTER tx.Commit()
	// below (loop over the re-read committed rows), not inside the tx.
	// Keep the source-to-fork IDs so session-level pointers can follow the
	// copied rows without referring back into the source session.
	messageIDMap := make(map[string]string, len(srcMsgs))
	for _, m := range srcMsgs {
		copiedMessageID := uuid.New().String()
		if _, err := qtx.CreateMessage(ctx, db.CreateMessageParams{
			ID:                  copiedMessageID,
			SessionID:           forkID,
			Role:                m.Role,
			Parts:               m.Parts,
			Model:               m.Model,
			Provider:            m.Provider,
			ReasoningEffort:     m.ReasoningEffort,
			IsSummaryMessage:    m.IsSummaryMessage,
			Hidden:              m.Hidden,
			AutoResumed:         m.AutoResumed,
			BackgroundJobNotice: m.BackgroundJobNotice,
			Origin:              m.Origin,
		}); err != nil {
			return Session{}, 0, fmt.Errorf("copy message into fork: %w", err)
		}
		messageIDMap[m.ID] = copiedMessageID
		// CreateMessage intentionally omits finished_at; preserve its exact
		// nullable value so completed assistant messages remain deletable as
		// terminal rows and unfinished messages remain streaming.
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET finished_at = ? WHERE id = ?`, m.FinishedAt, copiedMessageID); err != nil {
			return Session{}, 0, fmt.Errorf("copy message finish time into fork: %w", err)
		}
		if m.Pinned != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET pinned = ? WHERE id = ?`, m.Pinned, copiedMessageID); err != nil {
				return Session{}, 0, fmt.Errorf("copy pinned state into fork: %w", err)
			}
		}
	}

	if copiedSummaryID, ok := messageIDMap[src.SummaryMessageID.String]; ok && src.SummaryMessageID.Valid {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET summary_message_id = ? WHERE id = ?`,
			copiedSummaryID, forkID,
		); err != nil {
			return Session{}, 0, fmt.Errorf("copy summary pointer into fork: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return Session{}, 0, fmt.Errorf("commit fork transaction: %w", err)
	}

	// Re-read the committed fork to return its final, fully-populated state.
	fork, err := s.q.GetSessionByID(ctx, forkID)
	if err != nil {
		return Session{}, 0, fmt.Errorf("reload forked session: %w", err)
	}
	return s.fromDBItem(fork), len(srcMsgs), nil
}
