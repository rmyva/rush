package message

// Fork patch: this file diverges from upstream in two ways.
//
//  1. CreateMessageParams adds `ReasoningEffort` and `Hidden`; the Service
//     interface adds `Notify` (DB-less pubsub for streaming deltas) and
//     `SetPinned`. Matching DB migrations live under
//     `internal/db/migrations/20260310*`, `20260311*`, `20260313000001`.
//
//  2. We removed upstream's debounced/coalesced update layer (defaultUpdate-
//     Debounce, pendingState, Flush/FlushAll). Our streaming path uses the
//     in-process pubsub (Notify) for high-frequency UI updates and falls back
//     to synchronous Update for terminal-state writes — this matches the
//     latest-snapshot ticker in `internal/agent/agent.go`.
//
// Before merging upstream changes here: read CHANGELOG.fork.md section 2
// ("internal/message/message.go") and section 4.C (DB migrations).

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/google/uuid"
)

type CreateMessageParams struct {
	Role             MessageRole
	Parts            []ContentPart
	Model            string
	Provider         string
	ReasoningEffort  string // "low", "medium", "high", or "max" - reasoning effort for Claude models
	IsSummaryMessage bool
	// Hidden marks the message as invisible in the UI. Used for silent
	// background summaries that provide context to the LLM without cluttering
	// the conversation view.
	Hidden bool
	// AutoResumed marks a user message that was created by Phase 4 autonomous
	// idle-resume, not typed by a human; surfaced as a web badge.
	AutoResumed bool
	// BackgroundJobNotice marks a system-injected background-job-completion
	// notice so the web renders it as a notice, not a human message.
	BackgroundJobNotice bool
	// NoticeKind distinguishes the kind of notice -- see Message.NoticeKind's
	// doc for the value set.
	NoticeKind string
	// Origin marks the entry channel this message arrived through
	// (message.OriginCLI/Web/SDK); empty = unspecified.
	Origin Origin
}

type Service interface {
	pubsub.Subscriber[Message]
	pubsub.Shutdowner
	Create(ctx context.Context, sessionID string, params CreateMessageParams) (Message, error)
	// CreateTx is Create's transaction-aware counterpart (phase-4 step 3,
	// docs/plans/2026-09-28-async-phase4-durable-core.md sec.3.3): the INSERT
	// runs on tx instead of this service's own connection, so a caller that
	// also needs to UPDATE another table (session_notices' delivery outbox) in
	// the SAME transaction can do so atomically -- "one
	// transaction per notice: UPDATE ... RETURNING -> INSERT the history
	// message -> id in the row". Deliberately does NOT publish CreatedEvent
	// (tx may still roll back) -- the caller commits tx first, then calls
	// PublishCreated with the returned Message.
	CreateTx(ctx context.Context, tx *sql.Tx, sessionID string, params CreateMessageParams) (Message, error)
	// PublishCreated publishes msg's CreatedEvent exactly as Create does,
	// for a caller that created the row itself via CreateTx after its own
	// transaction committed (see CreateTx's doc).
	PublishCreated(msg Message)
	Update(ctx context.Context, message Message) error
	// UpdateTx is Update's transaction-aware counterpart (phase-4 step 4,
	// docs/plans/2026-09-28-async-phase4-durable-core.md sec.3.4): the same
	// write, but against tx instead of this service's own connection, and
	// with publishing deferred to the returned func -- call it ONLY after
	// tx commits (the reaction-debt marker writes the step's final message
	// and marks session_notices rows reacted=1 in ONE
	// transaction; publishing before commit could let a subscriber observe
	// a write that still rolls back). On success the returned func is never
	// nil; it publishes nothing when the update affected 0 rows (e.g. the
	// message was concurrently deleted). On error it is nil.
	UpdateTx(ctx context.Context, tx *sql.Tx, message Message) (publish func(), err error)
	// Notify publishes a message update to the UI without writing to the database.
	// Use this for high-frequency streaming updates where DB durability is not
	// required on every token; call Update at the end to persist the final state.
	Notify(message Message)
	Get(ctx context.Context, id string) (Message, error)
	List(ctx context.Context, sessionID string) ([]Message, error)
	// ListWithWatermark is List plus the session's delete-generation
	// watermark as of the same read (task #737): a per-session counter
	// bumped once on every Delete/ForceDelete call, read here BEFORE the
	// List query runs (0 for a session with no deletes yet). Callers that
	// forward this watermark to a client (the messages_list WS reply) let
	// that client detect a snapshot whose read is PROVABLY older than a
	// delete it has already applied -- see Message.DeleteGeneration's doc
	// comment for the full mechanism. Returned messages do NOT carry their
	// own DeleteGeneration (this is the session-level watermark, not a
	// per-message one); only Delete/ForceDelete populate that.
	ListWithWatermark(ctx context.Context, sessionID string) ([]Message, int64, error)
	// ListPaginated returns at most limit messages for a session, newest first
	// (DESC by created_at), skipping the first offset rows. It is the paginated
	// counterpart to List so callers can read just the window they need instead
	// of decoding the entire history.
	//
	// WARNING: calling this back-to-back with a separate Count call is racy
	// under concurrent inserts (the two are independent round trips, so a
	// message written in between can make the two disagree) - callers that
	// need a self-consistent (window, total) pair, most notably reading a
	// LIVE, still-growing transcript, should use ListPaginatedSnapshot instead.
	ListPaginated(ctx context.Context, sessionID string, limit, offset int) ([]Message, error)
	// Count returns the total number of messages stored for a session. Used by
	// callers that page with ListPaginated to render accurate "N earlier
	// omitted" markers without loading the full history.
	Count(ctx context.Context, sessionID string) (int64, error)
	// ListPaginatedSnapshot is the race-free counterpart to calling
	// ListPaginated and Count separately: it returns a window of at most limit
	// messages, newest-first, skipping the first offset rows, TOGETHER with
	// the total message count - both computed from a single consistent
	// snapshot pinned at the moment this call begins.
	//
	// This matters specifically for offset-based paging over a session whose
	// message list can grow WHILE it is being read (the primary use case:
	// read_delegation_transcript.go observing a live sub-agent delegation).
	// Offset counts back from the newest end of a DESC-ordered list, so it is
	// only meaningful relative to a fixed snapshot of "what the newest row
	// is" - if new messages are inserted between reading the total and
	// reading the window, a numeric offset silently refers to a different
	// logical position than the one the caller computed it against,
	// producing skipped or duplicated messages on the next read of a
	// still-growing transcript. ListPaginatedSnapshot pins the boundary row
	// (the row `offset` positions back from the newest, as of one atomic
	// query) FIRST, then fetches the window strictly at-or-before that
	// boundary via a keyset filter - so any message inserted after this call
	// begins is correctly excluded from both the returned total and the
	// returned window, rather than shifting either one.
	ListPaginatedSnapshot(ctx context.Context, sessionID string, limit, offset int) (window []Message, total int64, err error)
	ListUserMessages(ctx context.Context, sessionID string) ([]Message, error)
	ListAllUserMessages(ctx context.Context) ([]Message, error)
	// ListCandidateInterruptedAssistantSessions returns, for every session
	// whose CHRONOLOGICALLY LAST message is an unfinished assistant message,
	// that session's id, the candidate message's id, and the session's
	// parent session id. Used by app.recoverInterruptedTurns (task #774) to
	// find the small set of sessions that actually need recovery instead of
	// scanning every session's full message history. See
	// ListCandidateInterruptedAssistantSessions (messages.sql) for the exact
	// query and its tie-break/correctness notes.
	ListCandidateInterruptedAssistantSessions(ctx context.Context) ([]InterruptedAssistantCandidate, error)
	// StampInterruptedAssistantIfStillLast conditionally applies a
	// process-restart error finish to msg (task #777, P1 release blocker).
	// The write lands only if, atomically in the same statement, msg.ID is
	// still unfinished AND still the chronologically last message in
	// sessionID. Returns (applied=false, nil) if the candidate went stale
	// between discovery and this call (superseded by a newer message, or
	// finished concurrently by its live owner) -- callers must treat that as
	// "skip, do not retry", exactly like DeleteMessageIfTerminal's
	// 0-rows-affected contract. See StampInterruptedAssistantIfStillLast
	// (messages.sql) for the full rationale.
	StampInterruptedAssistantIfStillLast(ctx context.Context, sessionID string, msg Message) (applied bool, err error)
	Delete(ctx context.Context, id string) error
	// ForceDelete unconditionally deletes a message, bypassing the
	// DeleteMessageIfTerminal streaming guard. This is ONLY for callers that
	// have verified no live agent turn owns the row (session cancelled + idle,
	// or crash recovery after holder death). The caller must ensure the row
	// is truly orphaned before calling this method; otherwise it can corrupt
	// the transcript by deleting a message a live turn is still writing to.
	ForceDelete(ctx context.Context, id string) error
	// DeleteTx is Rerun truncation's batch delete: it removes the given rows
	// of sessionID on tx, UNCONDITIONALLY (no streaming guard -- the caller
	// must hold the same proofs as for ForceDelete), and returns exactly the
	// rows this statement deleted (an id already gone is absent). Nothing is
	// published: call the returned publish func only AFTER tx commits (it
	// bumps the delete generation and publishes one DeletedEvent per row).
	DeleteTx(ctx context.Context, tx *sql.Tx, sessionID string, ids []string) (deleted []Message, publish func(), err error)
	DeleteSessionMessages(ctx context.Context, sessionID string) error
	SetPinned(ctx context.Context, id string, pinned bool) error
	// SetUsage records this message's token accounting and prompt-cache
	// breakdown (task #469). Called by the agent right after the step's cost
	// delta is computed; separate from Update because the Finish part is
	// appended before usage is known.
	//
	// Callers should treat a failure as non-fatal: statistics must never
	// abort a turn.
	SetUsage(ctx context.Context, id string, usage TokenUsage) error
	// UsageBySession returns per-model token/cache/cost aggregates for a
	// session, together with the number of assistant messages that carry no
	// usage at all, so a caller can state what its numbers were computed
	// over instead of implying full coverage.
	UsageBySession(ctx context.Context, sessionID string) (UsageReport, error)
	// UsageByModelInRange aggregates usage across ALL sessions in a time
	// window (Unix seconds), grouped by the model that produced each message
	// rather than by the session's current model.
	UsageByModelInRange(ctx context.Context, since, until int64) (UsageReport, error)
	// UsageByDayInRange is the same window bucketed by local calendar day.
	UsageByDayInRange(ctx context.Context, since, until int64) ([]DayUsage, error)
}

type service struct {
	*pubsub.Broker[Message]
	q db.Querier

	// qRead backs the standalone, read-only hot paths (Get, List,
	// ListPaginated, Count, ListUserMessages, ListAllUserMessages) that
	// don't need read-your-own-write consistency with a subsequent write in
	// the same call — most notably transcript pagination, which can walk a
	// large OFFSET and would otherwise sit in the same single-connection
	// queue as every in-flight agent message write. Defaults to q (see
	// NewService), so callers that don't opt into a separate reader get
	// today's serialized behavior unchanged.
	qRead db.Querier

	// deleteGenMu guards deleteGen.
	deleteGenMu sync.Mutex
	// deleteGen is the task #737 per-session delete-generation counter:
	// keyed by sessionID, incremented once on every Delete/ForceDelete
	// call for that session. Replaces task #731's MAX(rowid)-based
	// watermark, which did not move for a delete of a non-tail message
	// (see Message.DeleteGeneration's doc comment).
	//
	// An in-memory map is correct here specifically because the SAME
	// server process that publishes the DeletedEvent also serves
	// ListWithWatermark reads — there is no cross-process or persisted-
	// state requirement. A server restart resets every session's counter
	// to 0; that is intentional and safe, not a bug: a long-lived browser
	// client may still hold a higher remembered high-water mark from
	// before the restart, which just makes every snapshot compare as
	// stale until that client's own state catches up, forcing the
	// (already-correct) epoch/tombstone fallback more often than strictly
	// needed. That is the conservative direction — it can never cause a
	// resurrection, only an occasional unnecessary merge. Do NOT persist
	// this counter to the DB to "fix" that; it is not broken.
	deleteGen map[string]int64
}

func NewService(q db.Querier) Service {
	return &service{
		Broker:    pubsub.NewBroker[Message](),
		q:         q,
		qRead:     q,
		deleteGen: make(map[string]int64),
	}
}

// NewServiceWithReader is NewService plus a separate read-only Querier
// (qRead) for the standalone hot read paths documented on the service
// struct. Production wiring (internal/app.New) passes db.ConnectRead's
// WAL-mode read-only pool here so transcript pagination and message listing
// run concurrently with the single writer connection instead of queuing
// behind it. Passing a nil qRead behaves exactly like NewService.
//
// qRead is deliberately the concrete *db.Queries type, not the db.Querier
// interface: a nil *db.Queries boxed into a db.Querier interface value is
// itself non-nil (Go's typed-nil-in-interface gotcha — the interface has a
// type descriptor even though the underlying pointer is nil), which would
// make the "if qRead != nil" fallback below never fire. That is exactly
// the case internal/app.New hits whenever db.ConnectRead fails and
// deliberately passes a nil *db.Queries reader intending to fall back to
// the writer (see app.go's doc comment on that path) — with the interface
// type, this method would silently install the typed-nil as qRead instead
// of falling back, and every subsequent s.qRead.* call would nil-dereference.
func NewServiceWithReader(q db.Querier, qRead *db.Queries) Service {
	svc := NewService(q).(*service)
	if qRead != nil {
		svc.qRead = qRead
	}
	return svc
}

// ErrMessageStillStreaming is returned by Delete when the target is a
// non-summary assistant message that is not yet terminally finished (no
// Finish part, or only a Partial one from the auto-checkpoint ticker) --
// i.e. it is still owned by an in-flight agent turn. See Delete's doc
// comment for why this must be refused rather than raced, and for why
// summary messages (IsSummaryMessage) are exempt.
var ErrMessageStillStreaming = errors.New("message is still streaming and cannot be deleted yet; please wait for it to finish")

// bumpDeleteGeneration increments sessionID's delete-generation counter and
// returns the POST-increment value -- this is the value Delete/ForceDelete
// attach to the DeletedEvent payload. See the deleteGen field's doc comment
// on the service struct for why an in-memory map is correct here, and
// ListWithWatermark's doc comment for the read-ordering requirement this
// pairs with.
func (s *service) bumpDeleteGeneration(sessionID string) int64 {
	s.deleteGenMu.Lock()
	defer s.deleteGenMu.Unlock()
	s.deleteGen[sessionID]++
	return s.deleteGen[sessionID]
}

// currentDeleteGeneration returns sessionID's delete-generation counter as
// of this call, without mutating it (0 for a session with no deletes yet
// this process). Used by ListWithWatermark, which MUST call this BEFORE
// running the List query -- see that method's doc comment.
func (s *service) currentDeleteGeneration(sessionID string) int64 {
	s.deleteGenMu.Lock()
	defer s.deleteGenMu.Unlock()
	return s.deleteGen[sessionID]
}

func (s *service) SetSummaryCreatedAt(ctx context.Context, sessionID, messageID string, createdAt int64) (bool, error) {
	n, err := s.q.SetSummaryMessageCreatedAt(ctx, db.SetSummaryMessageCreatedAtParams{CreatedAt: createdAt, ID: messageID, SessionID: sessionID})
	return n == 1, err
}

func (s *service) Delete(ctx context.Context, id string) error {
	message, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	// task #595 (P1-1 of the 2026-08-19 static follow-up review): commit
	// 547b0815 refused content/part EDITS to an assistant message with no
	// terminal Finish (updateMessageAndVerify in
	// internal/server/handlers_messages.go), but delete went through this
	// method unconditionally -- read, unconditional DELETE, publish a
	// terminal DeletedEvent. The live turn keeps its own in-memory
	// currentAssistant and, independently of this call, still writes its
	// terminal state back to the same id via Update. Before this fix, that
	// terminal write hardcoded rowsAffected = 1 and published UpdatedEvent
	// regardless of whether the row still existed, so a deleted streaming
	// message could "resurrect" in the live UI (absent from the DB, gone
	// again on reload) and different subscribers could observe different
	// event orders.
	//
	// DeleteMessageIfTerminal makes "is this row safe to delete" a single
	// atomic DB predicate (role != 'assistant' OR finished_at IS NOT NULL OR
	// is_summary_message = 1) rather than a read-then-act check racing the
	// turn's own write: the Get above is ONLY used to build the DeletedEvent
	// payload and to distinguish "row never existed" (Get already failed
	// above) from "row exists but is still streaming" (rowsAffected == 0
	// here) -- it is not itself the guard.
	//
	// Summary messages are exempt because the risk this predicate defends
	// against is an EXTERNAL actor deleting a message a DIFFERENT live turn
	// still owns -- a summary message is never reachable through the web
	// delete UI at all (see the query's own comment in
	// internal/db/sql/messages.sql for the full explanation), so its only
	// deleter is the same call that created it, cleaning up its own
	// abandoned draft. Gating that case too caused a real regression
	// (internal/agent's TestP1_4_CleanupUsesCancelImmuneContext) that this
	// exemption fixes.
	rowsAffected, err := s.q.DeleteMessageIfTerminal(ctx, message.ID)
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		// The row existed at the Get above (or this branch would already
		// have returned) but the predicate rejected the delete: an
		// assistant message with no terminal Finish yet. Report a distinct,
		// actionable error rather than silently no-op'ing -- callers (the
		// WS delete handlers) surface this to the operator instead of
		// replying "ok" over a delete that did not happen.
		return ErrMessageStillStreaming
	}
	// Bump the session's delete-generation counter (task #737) now that the
	// row is confirmed gone, and attach the POST-increment value to the
	// DeletedEvent payload -- see Message.DeleteGeneration's doc comment for
	// why this replaced the old per-row RowID watermark.
	message.DeleteGeneration = s.bumpDeleteGeneration(message.SessionID)
	// Clone the message before publishing to avoid race conditions with
	// concurrent modifications to the Parts slice.
	//
	// Deletion is a terminal, low-frequency event: if it's silently
	// dropped because a subscriber's channel is momentarily full, the UI
	// keeps showing a message that no longer exists in the DB, with no
	// further event ever arriving to correct it (unlike Update, nothing
	// else republishes this message's state afterward). Use
	// PublishMustDeliver so the drop only happens after a bounded wait,
	// not on the first full buffer.
	s.PublishMustDeliver(ctx, pubsub.DeletedEvent, message.Clone())
	return nil
}

// ForceDelete unconditionally deletes a message, bypassing the
// DeleteMessageIfTerminal streaming guard. This is ONLY for callers that
// have verified no live agent turn owns the row (session cancelled + idle,
// or crash recovery after holder death). The caller must ensure the row
// is truly orphaned before calling this method; otherwise it can corrupt
// the transcript by deleting a message a live turn is still writing to.
//
// Implementation: Get the message (for the event payload; propagate error),
// then call the unconditional DeleteMessage query, then publish DeletedEvent.
func (s *service) ForceDelete(ctx context.Context, id string) error {
	message, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.q.DeleteMessage(ctx, message.ID); err != nil {
		return err
	}
	// See the identical bump in Delete above -- same mechanism, same
	// "increment only once the delete is confirmed" ordering.
	message.DeleteGeneration = s.bumpDeleteGeneration(message.SessionID)
	s.PublishMustDeliver(ctx, pubsub.DeletedEvent, message.Clone())
	return nil
}

func (s *service) Create(ctx context.Context, sessionID string, params CreateMessageParams) (Message, error) {
	message, err := s.createWith(ctx, s.q, sessionID, params)
	if err != nil {
		return Message{}, err
	}
	// Clone the message before publishing to avoid race conditions with
	// concurrent modifications to the Parts slice.
	//
	// Create is deliberately left on best-effort Publish: a brand-new
	// message is (outside of Hidden/summary rows) about to be updated
	// repeatedly as the assistant streams, via Notify/Update below,
	// which already use must-deliver where it matters. If this
	// CreatedEvent is dropped under contention, the next Update quickly
	// re-establishes the message for subscribers; there's no terminal
	// state here worth blocking the caller for.
	s.Publish(pubsub.CreatedEvent, message.Clone())
	return message, nil
}

// CreateTx is Create's transaction-aware counterpart -- see the Service
// interface doc comment for the full contract. It shares createWith with
// Create, differing only in which db.Querier the INSERT runs against and in
// NOT publishing (the caller commits tx first, then calls PublishCreated).
func (s *service) CreateTx(ctx context.Context, tx *sql.Tx, sessionID string, params CreateMessageParams) (Message, error) {
	return s.createWith(ctx, db.New(tx), sessionID, params)
}

// PublishCreated publishes msg's CreatedEvent -- see the Service interface
// doc comment on CreateTx for why this is a separate call.
func (s *service) PublishCreated(msg Message) {
	s.Publish(pubsub.CreatedEvent, msg.Clone())
}

// createWith is Create/CreateTx's shared body, parameterized on the
// db.Querier the INSERT runs against (the service's own connection, or a
// caller-owned *sql.Tx via db.New(tx)) so a phase-4 notice pull can insert
// the history message in the SAME transaction as its delivery-outbox UPDATE
// (docs/plans/2026-09-28-async-phase4-durable-core.md sec.3.3).
func (s *service) createWith(ctx context.Context, q db.Querier, sessionID string, params CreateMessageParams) (Message, error) {
	if params.Role != Assistant {
		params.Parts = append(params.Parts, Finish{
			Reason: "stop",
		})
	}
	partsJSON, err := marshalParts(params.Parts)
	if err != nil {
		return Message{}, err
	}
	isSummary := int64(0)
	if params.IsSummaryMessage {
		isSummary = 1
	}
	hidden := int64(0)
	if params.Hidden {
		hidden = 1
	}
	autoResumed := int64(0)
	if params.AutoResumed {
		autoResumed = 1
	}
	backgroundJobNotice := int64(0)
	if params.BackgroundJobNotice {
		backgroundJobNotice = 1
	}
	dbMessage, err := q.CreateMessage(ctx, db.CreateMessageParams{
		ID:                  uuid.New().String(),
		SessionID:           sessionID,
		Role:                string(params.Role),
		Parts:               string(partsJSON),
		Model:               sql.NullString{String: string(params.Model), Valid: true},
		Provider:            sql.NullString{String: params.Provider, Valid: params.Provider != ""},
		ReasoningEffort:     sql.NullString{String: params.ReasoningEffort, Valid: params.ReasoningEffort != ""},
		IsSummaryMessage:    isSummary,
		Hidden:              hidden,
		AutoResumed:         autoResumed,
		BackgroundJobNotice: backgroundJobNotice,
		NoticeKind:          params.NoticeKind,
		Origin:              string(params.Origin),
	})
	if err != nil {
		return Message{}, err
	}
	return s.fromDBItem(dbMessage)
}

// DeleteSessionMessages deletes all messages for a session in a single DB
// operation, bypassing the per-message DeleteMessageIfTerminal predicate.
//
// This is the only code path that intentionally bypasses the streaming guard:
// total session teardown (called by `rush sessions reset --force`) has no notion
// of a "live turn that will still write" — the caller holds the session lock and
// has already killed the previous holder (via SIGKILL on Windows or Unix), so
// any orphaned streaming row will never receive a terminal Finish. Using the
// per-row Delete predicate here would strand that orphaned row forever.
//
// The unconditional DB delete (q.DeleteSessionMessages) is atomic with the
// session lock held, and DeletedEvents are published for every message that
// existed at the start of the call so subscribers can clean up their in-memory
// state.
func (s *service) DeleteSessionMessages(ctx context.Context, sessionID string) error {
	// List messages first: needed for events and to distinguish "nothing to do"
	// (an empty List is a no-op, not an error).
	messages, err := s.List(ctx, sessionID)
	if err != nil {
		return err
	}

	// Unconditional delete of all session messages in one statement.
	// This bypasses DeleteMessageIfTerminal by design: the caller holds the
	// session lock and has killed any previous holder, so there is no live
	// turn that could race this operation.
	if err := s.q.DeleteSessionMessages(ctx, sessionID); err != nil {
		return err
	}

	// Publish DeletedEvent for each message that existed before the wipe.
	// Use PublishMustDeliver (not best-effort Publish) because deletion is
	// terminal — if an event is dropped due to a full subscriber buffer, the
	// UI will show a message that no longer exists in the DB with no further
	// event to correct it.
	for _, message := range messages {
		s.PublishMustDeliver(ctx, pubsub.DeletedEvent, message.Clone())
	}

	return nil
}

func (s *service) Notify(message Message) {
	s.Publish(pubsub.UpdatedEvent, message.Clone())
}

func (s *service) Update(ctx context.Context, message Message) error {
	rowsAffected, partialCheckpoint, err := s.updateWith(ctx, s.q, message)
	if err != nil {
		return err
	}
	message.UpdatedAt = time.Now().Unix()
	s.publishUpdate(ctx, message, rowsAffected, partialCheckpoint)
	return nil
}

// UpdateTx is Update's transaction-aware counterpart -- see the Service
// interface doc comment. Shares updateWith's DB-write body, differing only
// in which db.Querier it runs against, and defers publishing to the
// returned func so the caller can invoke it strictly after tx commits.
func (s *service) UpdateTx(ctx context.Context, tx *sql.Tx, message Message) (func(), error) {
	rowsAffected, partialCheckpoint, err := s.updateWith(ctx, db.New(tx), message)
	if err != nil {
		return nil, err
	}
	message.UpdatedAt = time.Now().Unix()
	return func() { s.publishUpdate(ctx, message, rowsAffected, partialCheckpoint) }, nil
}

// updateWith is Update/UpdateTx's shared DB-write body, parameterized on the
// db.Querier the UPDATE runs against (see createWith's identical rationale
// for CreateTx). Returns rowsAffected and whether this write was a partial
// checkpoint, both needed by publishUpdate to decide delivery semantics.
func (s *service) updateWith(ctx context.Context, q db.Querier, message Message) (rowsAffected int64, partialCheckpoint bool, err error) {
	parts, err := marshalParts(message.Parts)
	if err != nil {
		return 0, false, err
	}
	finishedAt := sql.NullInt64{}
	// Fork patch: batch 8 -- a Partial finish is NOT a real finish;
	// finished_at stays NULL so the row is still "in progress".
	// The auto-checkpoint ticker uses this to persist mid-stream state
	// without confusing IsFinished / recovery.
	finish := message.FinishPart()
	partialCheckpoint = finish != nil && finish.Partial
	if finish != nil && !finish.Partial {
		finishedAt.Int64 = finish.Time
		finishedAt.Valid = true
	}

	// P0-4 fix: Use conditional update for partial checkpoints to prevent
	// a hung checkpoint from overwriting a terminal finish after unblocking.
	// If the DB already has a non-partial finish (finished_at IS NOT NULL),
	// UpdateMessageIfNotTerminal skips the update (0 rows affected), which
	// is the correct outcome: the terminal state wins, the stale checkpoint
	// is safely discarded.
	if partialCheckpoint {
		// The DB update is conditional: it only touches rows that still
		// have finished_at IS NULL (no terminal finish yet). If a real
		// terminal finish landed concurrently, this returns 0 rows affected,
		// meaning the stale partial checkpoint lost and must NOT be published
		// to avoid reverting the UI to an incomplete state.
		// Both generation parameters are the WRITER's own generation: the
		// SET stamps the row with it, and the WHERE rejects the write if the
		// row already carries a newer one. See the query's own comment for
		// why the comparison is <= rather than <.
		rowsAffected, err = q.UpdateMessageIfNotTerminal(ctx, db.UpdateMessageIfNotTerminalParams{
			ID:                     message.ID,
			Parts:                  string(parts),
			FinishedAt:             finishedAt,
			CheckpointGeneration:   message.CheckpointGeneration,
			CheckpointGeneration_2: message.CheckpointGeneration,
		})
	} else {
		// task #595: this used to hardcode rowsAffected = 1 with the comment
		// "Terminal update always wins" -- true about PRECEDENCE (a terminal
		// write is never fenced by finished_at/checkpoint_generation the way a
		// partial checkpoint is) but not about EXISTENCE. A plain UPDATE ...
		// WHERE id = ? against a row that no longer exists (e.g. an operator
		// deleted this streaming assistant message out from under its own
		// live turn -- see DeleteMessageIfTerminal / Delete below) affects 0
		// rows and returns no error; the old hardcode reported success and
		// published UpdatedEvent anyway, "resurrecting" the deleted message in
		// the UI of whichever client received that event, with nothing in the
		// DB to back it and no further event to correct it until reload.
		// UpdateMessage is now :execrows so rowsAffected reflects what the DB
		// actually did.
		rowsAffected, err = q.UpdateMessage(ctx, db.UpdateMessageParams{
			ID:         message.ID,
			Parts:      string(parts),
			FinishedAt: finishedAt,
		})
	}
	return rowsAffected, partialCheckpoint, err
}

// publishUpdate implements Update's delivery semantics: a terminal write
// must-deliver (bounded per-subscriber wait), a partial checkpoint is
// best-effort, and either is skipped entirely when rowsAffected == 0 (the
// row was concurrently deleted/superseded) to avoid resurrecting or
// reverting the UI. message is cloned before publishing to avoid races with
// concurrent Parts mutation.
//
//   - Terminal (real non-Partial Finish, tool-result flush, summary):
//     PublishMustDeliver, so a momentarily full subscriber buffer doesn't
//     silently eat the final state, but only if rowsAffected > 0 (task
//     #595) -- a terminal write can legitimately affect 0 rows (the row
//     was deleted concurrently) and must not publish a phantom update for
//     a message that no longer exists.
//   - Partial checkpoint (Finish.Partial == true, written by the
//     auto-checkpoint ticker every ~2s during streaming): best-effort
//     Publish, but ONLY if the update actually touched the DB. A stale
//     checkpoint that returns 0 rows because a terminal finish already
//     landed must NOT be published, or the UI would show an incomplete
//     partial snapshot as the last event.
func (s *service) publishUpdate(ctx context.Context, message Message, rowsAffected int64, partialCheckpoint bool) {
	if partialCheckpoint {
		if rowsAffected > 0 {
			s.Publish(pubsub.UpdatedEvent, message.Clone())
		}
		return
	}
	if rowsAffected > 0 {
		s.PublishMustDeliver(ctx, pubsub.UpdatedEvent, message.Clone())
	}
}

func (s *service) Get(ctx context.Context, id string) (Message, error) {
	dbMessage, err := s.qRead.GetMessage(ctx, id)
	if err != nil {
		return Message{}, err
	}
	return s.fromDBItem(dbMessage)
}

func (s *service) List(ctx context.Context, sessionID string) ([]Message, error) {
	dbMessages, err := s.qRead.ListMessagesBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

// ListWithWatermark is List plus the session's delete-generation watermark
// -- see the Service interface doc comment for the full contract.
//
// task #737 replaced the original (task #731) watermark, which was
// GetMaxMessageRowIDBySession -- MAX(rowid) over a session's SURVIVING
// messages. That query's own doc comment used to claim it answered "the
// highest rowid this session's messages table has ever assigned", which is
// WRONG: deleting a non-tail message (an older message while a newer one
// survives) does not lower MAX(rowid) at all, so that watermark never moved
// for that class of delete. Concretely: rowid 10 and rowid 20 both exist;
// rowid 10 is deleted; MAX(rowid) is still 20, unchanged. A client that
// recorded delete-watermark 10 from that delete's push, comparing against a
// stale pre-delete snapshot reporting watermark 20, computes "10 > 20" ==
// false and wrongly treats the stale snapshot as fresh -- exactly the
// resurrection this mechanism exists to prevent. The watermark scheme as
// originally implemented only actually caught deletion of the CURRENT
// highest-rowid message in a session, not deletion in general.
//
// The fix: the watermark is now an in-memory per-session counter
// (s.deleteGen, guarded by s.deleteGenMu) that increments once on EVERY
// Delete/ForceDelete call for that session, regardless of which message was
// removed -- see bumpDeleteGeneration/currentDeleteGeneration and
// Message.DeleteGeneration's doc comment. An in-memory map is sufficient
// (no DB persistence needed) because the same server process that
// publishes each DeletedEvent also serves every ListWithWatermark read.
//
// ORDERING IS CRITICAL FOR CORRECTNESS: the generation counter is read
// BEFORE the List query runs, not after. Reading first guarantees the
// returned generation can only UNDER-represent deletes that raced this read
// -- if a delete lands in the gap between the generation read and the List
// query, the returned generation is stale-low relative to what List
// actually reflects, which is the SAFE direction: it just makes this
// snapshot look slightly more stale than it is, falling back to the
// pre-existing epoch/tombstone heuristic more often than strictly
// necessary, but never claiming freshness it hasn't earned. Reading the
// generation AFTER the List query would let a delete that lands in THAT
// gap advance the counter past what the returned message list actually
// reflects, making a genuinely-stale snapshot compare as falsely fresh --
// exactly the class of bug this mechanism exists to close. Do not reorder
// these two calls.
func (s *service) ListWithWatermark(ctx context.Context, sessionID string) ([]Message, int64, error) {
	watermark := s.currentDeleteGeneration(sessionID)
	messages, err := s.List(ctx, sessionID)
	if err != nil {
		return nil, 0, err
	}
	return messages, watermark, nil
}

func (s *service) ListPaginated(ctx context.Context, sessionID string, limit, offset int) ([]Message, error) {
	dbMessages, err := s.qRead.ListMessagesBySessionPaginated(ctx, db.ListMessagesBySessionPaginatedParams{
		SessionID: sessionID,
		Limit:     int64(limit),
		Offset:    int64(offset),
	})
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *service) Count(ctx context.Context, sessionID string) (int64, error) {
	return s.qRead.CountMessagesBySession(ctx, sessionID)
}

// ListPaginatedSnapshot implements the race-free (window, total) pairing
// documented on the Service interface. See that doc comment for the
// motivating race; this comment covers the implementation.
//
// Step 1 pins a single high-water-mark snapshot: GetTranscriptWindowCursor
// returns, from ONE query execution, both the session's total message count
// and the identity (created_at, rowid) of the row `offset` positions back
// from the newest message - i.e. exactly the first row the caller's window
// should contain. Any message inserted after this query executes is
// necessarily newer than that pinned row and is therefore correctly excluded
// from everything that follows, however many further round trips it takes.
//
// Step 2 fetches the window using that pinned boundary as an inclusive
// keyset cursor, split into two queries because sqlc's SQLite catalog cannot
// validate a bare `rowid` reference in a WHERE clause (confirmed by hand;
// see the doc comments on GetTranscriptWindowCursor/
// ListMessagesBySessionAtCreatedAt in internal/db/sql/messages.sql for the
// full explanation) - so the "older seconds" and "tied-second" halves of the
// keyset filter are two separate queries, both filtering only on the real
// `created_at` column, with the rowid tiebreaker applied in Go for the tied
// second only (which is bounded to however many messages share one second,
// not the whole table).
func (s *service) ListPaginatedSnapshot(ctx context.Context, sessionID string, limit, offset int) ([]Message, int64, error) {
	if limit <= 0 {
		// Defensive: the only caller (read_delegation_transcript.go's
		// clampTranscriptWindow) always clamps to a positive default before
		// calling in, so this is unreached in production today. But
		// make([]db.Message, 0, limit) below panics for a negative limit,
		// and limit == 0 would otherwise still return exactly one row (the
		// tied-second loop appends before checking len(dbMessages) >=
		// limit) instead of the zero rows a caller asking for "0 messages"
		// should get.
		total, err := s.qRead.CountMessagesBySession(ctx, sessionID)
		if err != nil {
			return nil, 0, err
		}
		return []Message{}, total, nil
	}

	cursor, err := s.qRead.GetTranscriptWindowCursor(ctx, db.GetTranscriptWindowCursorParams{
		SessionID: sessionID,
		Offset:    int64(offset),
	})
	if errors.Is(err, sql.ErrNoRows) {
		// offset is at or past the end of the session's history: no boundary
		// row exists, so the window is empty. Total still needs a value -
		// fall back to a plain count (no window race to guard against when
		// the window itself is empty).
		total, cerr := s.qRead.CountMessagesBySession(ctx, sessionID)
		if cerr != nil {
			return nil, 0, cerr
		}
		return []Message{}, total, nil
	}
	if err != nil {
		return nil, 0, err
	}

	// Tied-second half first: every message sharing the boundary's exact
	// created_at second, newest-rowid-first, keeping only rowid <= the
	// boundary's own rowid (the boundary row itself must be INCLUDED - it is
	// the first row of the window, not an exclusive fence post).
	tied, err := s.qRead.ListMessagesBySessionAtCreatedAt(ctx, db.ListMessagesBySessionAtCreatedAtParams{
		SessionID: sessionID,
		CreatedAt: cursor.CreatedAt,
	})
	if err != nil {
		return nil, 0, err
	}

	dbMessages := make([]db.Message, 0, limit)
	for _, row := range tied {
		if row.RowID > cursor.RowID {
			continue // newer than the boundary within the same second - excluded.
		}
		dbMessages = append(dbMessages, row.Message)
		if len(dbMessages) >= limit {
			break
		}
	}

	// Older-seconds half: only queried if the tied-second rows didn't
	// already fill the window, and LIMITed to exactly what's left.
	if len(dbMessages) < limit {
		older, err := s.qRead.ListMessagesBySessionOlderThanCreatedAt(ctx, db.ListMessagesBySessionOlderThanCreatedAtParams{
			SessionID: sessionID,
			CreatedAt: cursor.CreatedAt,
			Limit:     int64(limit - len(dbMessages)),
		})
		if err != nil {
			return nil, 0, err
		}
		dbMessages = append(dbMessages, older...)
	}

	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, 0, err
		}
	}
	return messages, cursor.TotalCount, nil
}

func (s *service) ListUserMessages(ctx context.Context, sessionID string) ([]Message, error) {
	dbMessages, err := s.qRead.ListUserMessagesBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *service) ListAllUserMessages(ctx context.Context) ([]Message, error) {
	dbMessages, err := s.qRead.ListAllUserMessages(ctx)
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *service) fromDBItem(item db.Message) (Message, error) {
	parts, err := unmarshalParts([]byte(item.Parts))
	if err != nil {
		return Message{}, err
	}
	return Message{
		ID:                   item.ID,
		SessionID:            item.SessionID,
		Role:                 MessageRole(item.Role),
		Parts:                parts,
		Model:                item.Model.String,
		Provider:             item.Provider.String,
		ReasoningEffort:      item.ReasoningEffort.String,
		CreatedAt:            item.CreatedAt,
		UpdatedAt:            item.UpdatedAt,
		IsSummaryMessage:     item.IsSummaryMessage != 0,
		Pinned:               item.Pinned != 0,
		Hidden:               item.Hidden != 0,
		AutoResumed:          item.AutoResumed != 0,
		BackgroundJobNotice:  item.BackgroundJobNotice != 0,
		NoticeKind:           item.NoticeKind,
		Origin:               Origin(item.Origin),
		CheckpointGeneration: item.CheckpointGeneration,
		Usage:                usageFromDBItem(item),
	}, nil
}

// usageFromDBItem lifts the per-message token accounting off a row, or returns
// nil when none was recorded.
//
// total_tokens is the recorded/not-recorded marker (SetUsage always writes it
// non-NULL, even when the derived value is 0), which is the same signal the
// aggregate queries key on. Returning nil rather than a zero-valued struct
// keeps "never measured" distinguishable from "measured as zero" all the way
// out to the UI, so a message from before this feature cannot be rendered as
// a confident 0% cache hit.
func usageFromDBItem(item db.Message) *TokenUsage {
	if !item.TotalTokens.Valid {
		return nil
	}
	return &TokenUsage{
		InputTokens:         item.InputTokens.Int64,
		OutputTokens:        item.OutputTokens.Int64,
		ReasoningTokens:     item.ReasoningTokens.Int64,
		CacheCreationTokens: item.CacheCreationTokens.Int64,
		CacheReadTokens:     item.CacheReadTokens.Int64,
		TotalTokens:         item.TotalTokens.Int64,
		CostUSD:             item.CostUsd.Float64,
		Provider:            item.UsageProvider.String,
		Model:               item.UsageModel.String,
		CacheSupport:        CacheSupport(item.CacheSupport.String),
		Estimated:           item.UsageEstimated.Int64 != 0,
	}
}

func (s *service) SetPinned(ctx context.Context, id string, pinned bool) error {
	pinnedVal := int64(0)
	if pinned {
		pinnedVal = 1
	}
	err := s.q.UpdateMessagePinned(ctx, db.UpdateMessagePinnedParams{
		ID:     id,
		Pinned: pinnedVal,
	})
	if err != nil {
		return err
	}
	msg, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	// Explicit, low-frequency user action (pin/unpin) — not part of the
	// streaming hot path, so the bounded PublishMustDeliver wait is free
	// here, and the user should reliably see their own action reflected.
	s.PublishMustDeliver(ctx, pubsub.UpdatedEvent, msg.Clone())
	return nil
}

// TruncateStreamedContent removes every TextContent and ReasoningContent
// part: the text and reasoning a cut stream attempt accumulated, which a
// provider retry would otherwise duplicate when it replays the step's
// deltas. Tool calls, tool results and Finish parts are kept.
func (m *Message) TruncateStreamedContent() bool {
	kept := m.Parts[:0]
	for _, part := range m.Parts {
		switch part.(type) {
		case TextContent, ReasoningContent:
			continue
		default:
			kept = append(kept, part)
		}
	}
	removed := len(kept) < len(m.Parts)
	m.Parts = kept
	return removed
}
