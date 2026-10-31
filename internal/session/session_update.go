// Column-scoped session state updates: cost accrual, usage and summary
// pointers, todos, model slots, reasoning effort, system prompt, rename —
// plus the cross-process cancel flag and the fork-patch budget/ended-reason
// persistence.

package session

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/pubsub"
)

// IncrementCost adds delta to the session's OWN cost (cost_self) atomically.
// The only writer of the monotonic per-node ledger (#1130); a negative delta
// is an error — `sessions reset` freezes a budget via ResetCostBase instead
// of lowering anything.
func (s *service) IncrementCost(ctx context.Context, sessionID string, delta float64) (Session, error) {
	if delta == 0 {
		return s.Get(ctx, sessionID)
	}
	if delta < 0 {
		return Session{}, fmt.Errorf("increment cost: negative deltas are not supported, reset the budget via ResetCostBase instead")
	}
	dbSession, err := s.q.IncrementSessionCost(ctx, db.IncrementSessionCostParams{
		ID:       sessionID,
		Cost:     delta,
		CostSelf: delta,
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.UpdatedEvent, session)
	return session, nil
}

// IncrementCostIfUnderMax — see interface doc on
// Service.IncrementCostIfUnderMax for rationale. maxCost <= 0 means
// "unlimited" and falls through to the unconditional path. The budget read
// and the charge share one transaction on the single-connection writer pool
// (SetMaxOpenConns(1)), so two concurrent callers cannot both pass the
// check: the second sees the first one's cost_self — the #782 TOCTOU stays
// closed with the budget now being the SUBTREE budget (#1130).
func (s *service) IncrementCostIfUnderMax(ctx context.Context, sessionID string, delta, maxCost float64) (Session, bool, error) {
	if maxCost <= 0 {
		sess, err := s.IncrementCost(ctx, sessionID, delta)
		return sess, err == nil, err
	}
	if delta == 0 {
		sess, err := s.Get(ctx, sessionID)
		return sess, err == nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	qtx := s.q.WithTx(tx)

	spent, err := qtx.GetSubtreeSpent(ctx, sessionID)
	if err != nil {
		return Session{}, false, fmt.Errorf("read subtree spent: %w", err)
	}
	base, err := qtx.GetSessionCostBase(ctx, sessionID)
	if err != nil {
		return Session{}, false, fmt.Errorf("read cost base: %w", err)
	}
	if max(0, spent-base)+delta >= maxCost {
		// Refused: no charge lands. Read the snapshot INSIDE the tx (the
		// writer pool has one connection — a Get after the tx would wait on
		// this very tx), then let the deferred rollback discard it.
		item, getErr := qtx.GetSessionByID(ctx, sessionID)
		if getErr != nil {
			return Session{}, false, getErr
		}
		return s.fromDBItem(item), false, nil
	}
	dbSession, err := qtx.IncrementSessionCost(ctx, db.IncrementSessionCostParams{
		ID:       sessionID,
		Cost:     delta,
		CostSelf: delta,
	})
	if err != nil {
		return Session{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, false, fmt.Errorf("commit charge: %w", err)
	}
	sess := s.fromDBItem(dbSession)
	s.Publish(pubsub.UpdatedEvent, sess)
	return sess, true, nil
}

// UpdateSystemPrompt saves a custom system prompt for a session.
func (s *service) UpdateSystemPrompt(ctx context.Context, sessionID, prompt string) error {
	if err := s.q.UpdateSessionSystemPrompt(ctx, db.UpdateSessionSystemPromptParams{
		ID:           sessionID,
		SystemPrompt: prompt,
	}); err != nil {
		return err
	}
	if sess, err := s.Get(ctx, sessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// SetUsage overwrites only the prompt/completion token counters for a
// session. It does not touch title, todos, summary, or cost, so it cannot
// clobber concurrent edits to those fields the way a full Save did. Used by
// the agent's per-step finalization to persist the latest context-window
// token snapshot.
func (s *service) SetUsage(ctx context.Context, sessionID string, promptTokens, completionTokens int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET prompt_tokens = ?, completion_tokens = ?, updated_at = strftime('%s', 'now') WHERE id = ?`,
		promptTokens, completionTokens, sessionID,
	); err != nil {
		return err
	}
	if sess, err := s.Get(ctx, sessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// SetSummaryAndUsage overwrites summary_message_id together with the
// prompt/completion token counters in one UPDATE. Used by the summarization
// paths (manual and silent compaction) and by `sessions reset`, which must
// flip the summary pointer and reset token counters as one logical op. Like
// SetUsage it leaves title, todos, and cost untouched, so it cannot lose
// concurrent edits to those columns.
//
// NULL vs empty-string note: `sessions reset` calls this with
// summaryMessageID equal to the Go zero value to clear the pointer, which
// writes a SQL empty string rather than NULL to summary_message_id —
// unlike the old generic Save/UpdateSession path, which stored a Go
// zero-value string as NULL via sql.NullString{Valid: false}. This is
// intentionally NOT treated as a bug: every reader of SummaryMessageID
// compares it against the empty string (Session.SummaryMessageID != ""),
// and no SQL query anywhere filters or joins on
// `summary_message_id IS NULL`. An empty string and NULL are therefore
// equivalent for every consumer of this column today. Do not "fix" this
// without first auditing for a new IS NULL usage.
func (s *service) SetSummaryAndUsage(ctx context.Context, sessionID, summaryMessageID string, promptTokens, completionTokens int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET summary_message_id = ?, prompt_tokens = ?, completion_tokens = ?, updated_at = strftime('%s', 'now') WHERE id = ?`,
		summaryMessageID, promptTokens, completionTokens, sessionID,
	); err != nil {
		return err
	}
	if sess, err := s.Get(ctx, sessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// SetTodos overwrites the todos and deleted_todos (tombstone) columns for a
// session in one UPDATE. It leaves title, token counters, summary, and cost
// untouched, so a todos edit can no longer clobber a concurrent rename or
// agent step the way a full Save did.
func (s *service) SetTodos(ctx context.Context, sessionID string, todos []Todo, deletedTodos []string) error {
	todosJSON, err := marshalTodos(todos)
	if err != nil {
		return err
	}
	deletedTodosJSON, err := marshalDeletedTodos(deletedTodos)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET todos = ?, deleted_todos = ?, updated_at = strftime('%s', 'now') WHERE id = ?`,
		sql.NullString{String: todosJSON, Valid: todosJSON != ""},
		deletedTodosJSON,
		sessionID,
	); err != nil {
		return err
	}
	if sess, err := s.Get(ctx, sessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// UpdateModels writes explicit per-session model overrides. A nil large or
// small argument leaves that slot completely untouched in the DB row —
// neither sets nor clears it. A non-nil argument with an empty
// Provider/Model clears the slot back to inheriting the folder/system
// default (the "" = inherit convention resolveSessionModels already applies
// when reading the row back); a non-nil argument with values sets an
// explicit override.
//
// The nil-means-untouched distinction exists because a caller changing only
// ONE slot (e.g. the web UI's per-slot model picker) must not silently wipe
// the OTHER slot's override back to unset — before this signature, every
// caller had to pass all four strings, so "leave the other slot alone" and
// "no override" were indistinguishable at this layer, and the web UI ended
// up pinning both smart and small on every single-slot switch (task #461).
func (s *service) UpdateModels(ctx context.Context, sessionID string, smart, fast *ModelSlotUpdate) error {
	params := db.UpdateSessionModelsParams{ID: sessionID}
	if smart != nil {
		params.SmartModelProvider = sql.NullString{String: smart.Provider, Valid: true}
		params.SmartModelID = sql.NullString{String: smart.Model, Valid: true}
	}
	if fast != nil {
		params.FastModelProvider = sql.NullString{String: fast.Provider, Valid: true}
		params.FastModelID = sql.NullString{String: fast.Model, Valid: true}
	}
	err := s.q.UpdateSessionModels(ctx, params)
	if err != nil {
		return err
	}

	// Publish an update event so the UI gets the new session state
	sess, err := s.Get(ctx, sessionID)
	if err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// UpdateWorkerReviewerModels is UpdateModels' sibling for the optional
// worker/reviewer slots (task #466) — same nil-means-untouched semantics.
func (s *service) UpdateWorkerReviewerModels(ctx context.Context, sessionID string, worker, reviewer *ModelSlotUpdate) error {
	params := db.UpdateSessionWorkerReviewerModelsParams{ID: sessionID}
	if worker != nil {
		params.WorkerModelProvider = sql.NullString{String: worker.Provider, Valid: true}
		params.WorkerModelID = sql.NullString{String: worker.Model, Valid: true}
	}
	if reviewer != nil {
		params.ReviewerModelProvider = sql.NullString{String: reviewer.Provider, Valid: true}
		params.ReviewerModelID = sql.NullString{String: reviewer.Model, Valid: true}
	}
	if err := s.q.UpdateSessionWorkerReviewerModels(ctx, params); err != nil {
		return err
	}
	if sess, err := s.Get(ctx, sessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// UpdateWorkerReviewerReasoningEffort is UpdateReasoningEffort's sibling for
// the worker/reviewer slots — same always-touch semantics (an empty string
// clears the effort field) as the smart/fast original.
func (s *service) UpdateWorkerReviewerReasoningEffort(ctx context.Context, sessionID, workerEffort, reviewerEffort string) error {
	err := s.q.UpdateSessionWorkerReviewerReasoningEffort(ctx, db.UpdateSessionWorkerReviewerReasoningEffortParams{
		ID:                           sessionID,
		WorkerModelReasoningEffort:   sql.NullString{String: workerEffort, Valid: workerEffort != ""},
		ReviewerModelReasoningEffort: sql.NullString{String: reviewerEffort, Valid: reviewerEffort != ""},
	})
	if err != nil {
		return err
	}
	if sess, err := s.Get(ctx, sessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// UpdateReasoningEffort updates the reasoning effort for large and fast models.
func (s *service) UpdateReasoningEffort(ctx context.Context, sessionID, smartEffort, fastEffort string) error {
	err := s.q.UpdateSessionReasoningEffort(ctx, db.UpdateSessionReasoningEffortParams{
		ID:                        sessionID,
		SmartModelReasoningEffort: sql.NullString{String: smartEffort, Valid: smartEffort != ""},
		FastModelReasoningEffort:  sql.NullString{String: fastEffort, Valid: fastEffort != ""},
	})
	if err != nil {
		return err
	}

	// Publish an update event so the UI gets the new session state
	sess, err := s.Get(ctx, sessionID)
	if err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// UpdateSmartReasoningEffort, UpdateFastReasoningEffort,
// UpdateWorkerReasoningEffort and UpdateReviewerReasoningEffort write ONE
// effort column. The pair methods above always write both columns, so a
// caller that sends a single slot must use these: backfilling the unsent
// slot from an earlier read and writing the pair lets two concurrent
// single-slot requests restore each other's old value. An empty effort
// clears the column to NULL, like the pair methods.
func (s *service) UpdateSmartReasoningEffort(ctx context.Context, sessionID, effort string) error {
	return s.afterEffortWrite(ctx, sessionID, s.q.UpdateSessionSmartReasoningEffort(ctx, db.UpdateSessionSmartReasoningEffortParams{
		ID:                        sessionID,
		SmartModelReasoningEffort: sql.NullString{String: effort, Valid: effort != ""},
	}))
}

func (s *service) UpdateFastReasoningEffort(ctx context.Context, sessionID, effort string) error {
	return s.afterEffortWrite(ctx, sessionID, s.q.UpdateSessionFastReasoningEffort(ctx, db.UpdateSessionFastReasoningEffortParams{
		ID:                       sessionID,
		FastModelReasoningEffort: sql.NullString{String: effort, Valid: effort != ""},
	}))
}

func (s *service) UpdateWorkerReasoningEffort(ctx context.Context, sessionID, effort string) error {
	return s.afterEffortWrite(ctx, sessionID, s.q.UpdateSessionWorkerReasoningEffort(ctx, db.UpdateSessionWorkerReasoningEffortParams{
		ID:                         sessionID,
		WorkerModelReasoningEffort: sql.NullString{String: effort, Valid: effort != ""},
	}))
}

func (s *service) UpdateReviewerReasoningEffort(ctx context.Context, sessionID, effort string) error {
	return s.afterEffortWrite(ctx, sessionID, s.q.UpdateSessionReviewerReasoningEffort(ctx, db.UpdateSessionReviewerReasoningEffortParams{
		ID:                           sessionID,
		ReviewerModelReasoningEffort: sql.NullString{String: effort, Valid: effort != ""},
	}))
}

// afterEffortWrite publishes the updated session after a successful write.
func (s *service) afterEffortWrite(ctx context.Context, sessionID string, err error) error {
	if err != nil {
		return err
	}
	if sess, getErr := s.Get(ctx, sessionID); getErr == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// Rename updates only the title of a session and bumps updated_at without
// changing usage fields.
//
// It publishes an UpdatedEvent like every other column-scoped update here.
// That publish is what carries a generated session title to the browser:
// the agent's background title generation (internal/agent's generateTitle)
// has no access to the web server's Hub, so the pubsub bridge in
// internal/server/events.go — which forwards every UpdatedEvent as
// session_updated — is the ONLY path a title save has to the tabs. Rename
// used to be the one session mutation that published nothing, which is why
// a session's tab and sidebar row kept showing the pre-generation name
// until the next 5s sessions_list poll while a hand-rename (whose handler
// broadcasts explicitly) updated instantly.
func (s *service) Rename(ctx context.Context, id string, title string) error {
	if err := s.q.RenameSession(ctx, db.RenameSessionParams{
		ID:    id,
		Title: title,
	}); err != nil {
		return err
	}
	if sess, err := s.Get(ctx, id); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// RequestCancel sets the cancel_requested flag for a session so a
// running agent (possibly in a different process) stops gracefully.
func (s *service) RequestCancel(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET cancel_requested = 1 WHERE id = ?",
		sessionID,
	)
	return err
}

// IsCancelRequested checks whether a cancel signal is set on the session.
func (s *service) IsCancelRequested(ctx context.Context, sessionID string) (bool, error) {
	var v int64
	err := s.db.QueryRowContext(ctx,
		"SELECT cancel_requested FROM sessions WHERE id = ?",
		sessionID,
	).Scan(&v)
	if err != nil {
		return false, err
	}
	return v != 0, nil
}

// ClearCancelRequest resets the cancel_requested flag. Called when a
// new run starts so a stale flag from a previous run does not kill it.
func (s *service) ClearCancelRequest(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET cancel_requested = 0 WHERE id = ?",
		sessionID,
	)
	return err
}

func (s *service) SetEndedReason(ctx context.Context, sessionID, reason string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET ended_reason = ?, updated_at = strftime('%s', 'now') WHERE id = ?",
		reason, sessionID,
	)
	return err
}

func (s *service) SetBudget(ctx context.Context, sessionID string, maxCost float64, maxTokens, timeoutSec int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET budget_max_cost = ?, budget_max_tokens = ?, budget_timeout_sec = ?,
		 updated_at = strftime('%s', 'now') WHERE id = ?`,
		maxCost, maxTokens, timeoutSec, sessionID,
	)
	return err
}
