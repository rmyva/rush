-- name: GetMessage :one
SELECT *
FROM messages
WHERE id = ? LIMIT 1;

-- name: ListMessagesBySession :many
SELECT *
FROM messages
WHERE session_id = ?
-- rowid is the tie-breaker: created_at is stored in SECONDS, so a single agent
-- turn produces dozens of rows with an identical created_at. Without a total
-- order SQLite does not guarantee a stable order among those ties. This is the
-- same class of bug fixed for ListMessagesBySessionPaginated (see its comment
-- below) - here applied to the oldest-first, non-paginated variant, so
-- (created_at ASC, rowid ASC) is a deterministic oldest-first total order.
-- rowid is SQLite's implicit monotonic insertion counter (messages.id is a
-- non-monotonic UUID, unsuitable as a tiebreaker).
ORDER BY created_at ASC, rowid ASC;

-- name: SetSummaryMessageCreatedAt :execrows
UPDATE messages
SET created_at = ?
WHERE id = ? AND session_id = ? AND is_summary_message = 1;

-- name: CreateMessage :one
INSERT INTO messages (
    id,
    session_id,
    role,
    parts,
    model,
    provider,
    reasoning_effort,
    is_summary_message,
    hidden,
    auto_resumed,
    background_job_notice,
    notice_kind,
    origin,
    created_at,
    updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, strftime('%s', 'now'), strftime('%s', 'now')
)
RETURNING *;

-- name: UpdateMessage :execrows
-- task #595: was :exec (rows-affected discarded). The terminal write path in
-- message.Service.Update used to hardcode rowsAffected = 1 for this branch,
-- which was true for the normal case (the row exists, one row updates) but
-- false whenever the row was deleted concurrently -- a plain UPDATE against a
-- WHERE id = ? that matches nothing affects 0 rows and returns no error.
-- Reporting rows actually affected lets the caller stop publishing
-- UpdatedEvent for a message that no longer exists (see the P1-1 finding in
-- docs/reviews/2026-08-19-release-readiness-static-follow-up-d3ee9841.md: a
-- streaming assistant message deleted mid-turn would otherwise "resurrect" in
-- the live UI via this terminal write, absent from the DB, and vanish again
-- on reload).
UPDATE messages
SET
    parts = ?,
    finished_at = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;

-- name: UpdateMessageIfNotTerminal :execrows
-- P0-4 fix: Only update if the message does NOT already have a non-partial finish.
-- This prevents a hung checkpoint from overwriting a terminal finish after unblocking.
-- A checkpoint writes with Partial=true (finished_at NULL), so this condition rejects
-- the update if finished_at is already non-NULL (real terminal finish).
-- Returns number of rows affected (0 = skipped because already terminal, 1 = updated).
--
-- P1-3: finished_at alone cannot tell two PARTIAL generations apart. runTurn
-- replaces its checkpoint writer per step and stopCheckpoint waits only a
-- bounded grace for the old one, so a stale writer can still land after a
-- newer one and overwrite fresher parts. The in-memory generation check runs
-- before the write, and the write happens after the lock is released, which
-- is the whole gap. checkpoint_generation moves the check into the statement.
--
-- <= and not <: one generation checkpoints many times and must be allowed to
-- rewrite its own row. Only a STRICTLY older generation is rejected, and a
-- rejection shows up as 0 rows affected, which the caller already treats as
-- "this write lost, do not publish it".
UPDATE messages
SET
    parts = ?,
    finished_at = ?,
    checkpoint_generation = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?
  AND finished_at IS NULL
  AND checkpoint_generation <= ?;

-- name: UpdateMessagePinned :exec
UPDATE messages
SET
    pinned = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;


-- name: DeleteMessage :exec
DELETE FROM messages
WHERE id = ?;

-- name: DeleteMessageIfTerminal :execrows
-- task #595 (P1-1 of the 2026-08-19 static follow-up review): plain
-- DeleteMessage above deletes an assistant message unconditionally, even
-- while an agent turn still owns it (no terminal Finish yet, or only a
-- Partial one from the auto-checkpoint ticker). The turn's terminal write
-- lands afterward through UpdateMessage/UpdateMessageIfNotTerminal and, at
-- the time this predicate was added, published UpdatedEvent regardless of
-- whether the row still existed -- "resurrecting" a deleted message in the
-- live UI only, absent from the DB, until the next reload wipes it again.
--
-- A DB predicate is used instead of read-then-delete (Get, check
-- IsFinished(), then DeleteMessage) because a Get-then-act pair is not
-- atomic: nothing stops the turn's own write from landing in the gap between
-- the read and the delete. The WHERE clause here makes the check and the
-- delete a single statement.
--
-- role != 'assistant' is deliberate, not merely permissive: only assistant
-- rows are ever streamed (see message.Message.IsFinished's doc), so user/
-- system/tool rows have no live turn that could contest a delete and must
-- remain deletable exactly as before. finished_at IS NOT NULL is the same
-- terminal test IsFinished() applies in Go, evaluated in SQL for atomicity.
--
-- is_summary_message = 1 is ALSO exempt, and this is not merely permissive
-- either: the risk this predicate defends against is an EXTERNAL actor (an
-- operator's Trash click or bulk-selection delete, reached through the web
-- UI) deleting a message a DIFFERENT, still-live turn intends to keep
-- writing to. A summary message (internal/agent/agent_compaction.go's
-- runSummarizeSilent/runSummarizeBody) is never reachable through that UI
-- at all -- web/src/components/Message/Message.tsx routes IsSummaryMessage
-- rows to SummaryMessage.tsx, which renders no Trash control and is outside
-- the selection-checkbox tree entirely -- so the only caller that ever
-- deletes one is the SAME turn that created it, cleaning up its own
-- abandoned draft after its own stream was cancelled, before writing
-- anything else to that id. That is single-writer self-cleanup, not the
-- external race this predicate exists to prevent, and gating it here caused
-- a real regression (TestP1_4_CleanupUsesCancelImmuneContext in
-- internal/agent/p1_3_p1_4_regression_test.go) where the cancel-path
-- cleanup delete of an unfinished summary message was wrongly refused.
--
-- Returns rows affected: 0 means either the row didn't exist, or it exists
-- but is a still-streaming (non-summary) assistant message and the delete
-- was refused. Service.Delete disambiguates those two cases with a
-- preceding Get.
DELETE FROM messages
WHERE id = ?
  AND (role != 'assistant' OR finished_at IS NOT NULL OR is_summary_message = 1);

-- name: DeleteSessionMessagesByIDs :many
-- Rerun truncation (message.Service.DeleteTx): unconditional delete of an
-- explicit id set inside the caller's transaction. RETURNING is the exact
-- set of rows this statement removed (an id someone else already deleted is
-- simply absent), which the caller reconciles its batch state against.
DELETE FROM messages
WHERE session_id = @session_id AND id IN (sqlc.slice('ids'))
RETURNING *;

-- name: DeleteSessionMessages :exec
DELETE FROM messages
WHERE session_id = ?;

-- name: ListUserMessagesBySession :many
SELECT *
FROM messages
WHERE session_id = ? AND role = 'user'
-- rowid is the tie-breaker: created_at is stored in SECONDS, so multiple user
-- messages within the same second (e.g. rapid follow-ups) are not given a
-- stable order by created_at alone. Same class of bug fixed for
-- ListMessagesBySession/ListMessagesBySessionPaginated - rowid is SQLite's
-- implicit monotonic insertion counter (messages.id is a non-monotonic UUID,
-- unsuitable as a tiebreaker), so (created_at DESC, rowid DESC) is a
-- deterministic newest-first total order.
ORDER BY created_at DESC, rowid DESC;

-- name: ListAllUserMessages :many
SELECT *
FROM messages
WHERE role = 'user'
-- rowid is the tie-breaker: see ListUserMessagesBySession above - identical
-- reasoning applies across all sessions, not just one.
ORDER BY created_at DESC, rowid DESC;

-- name: ListMessagesBySessionPaginated :many
SELECT *
FROM messages
WHERE session_id = ?
-- rowid is the tie-breaker: created_at is stored in SECONDS, so a single agent
-- turn produces dozens of rows with an identical created_at. Without a total
-- order SQLite does not guarantee a stable order among those ties, which makes
-- OFFSET pagination lose/duplicate rows when the query plan shifts between
-- page fetches. rowid is SQLite's implicit monotonic insertion counter, so
-- (created_at DESC, rowid DESC) is a deterministic newest-first total order.
--
-- NOTE: plain OFFSET is still vulnerable to a DIFFERENT instability than the
-- tie-breaker bug above: concurrent inserts at the head of the DESC order (new
-- messages from a still-running delegation) shift what a given numeric offset
-- points at between two separate calls of this query. This query remains here
-- as the plain, unprotected primitive; message.Service.ListPaginatedSnapshot
-- is the race-free caller-facing wrapper read_delegation_transcript.go
-- actually uses, built from GetTranscriptWindowCursor (pins one consistent
-- snapshot boundary) plus the keyset pair ListMessagesBySessionOlderThan-
-- CreatedAt / ListMessagesBySessionAtCreatedAt (fetches the window strictly
-- at-or-before that boundary, immune to head insertions by construction).
ORDER BY created_at DESC, rowid DESC
LIMIT ? OFFSET ?;

-- name: CountMessagesBySession :one
SELECT COUNT(*)
FROM messages
WHERE session_id = ?;

-- name: GetTranscriptWindowCursor :one
-- Returns, in a SINGLE round trip, the (created_at, rowid) of the row at
-- `offset` positions back from the newest message in the session, together
-- with the session's total message count as of that same statement
-- execution (SQLite evaluates COUNT(*) OVER() over the full WHERE-matched row
-- set before LIMIT/OFFSET are applied, in the same query plan as the LIMIT 1
-- OFFSET ? row selection - both numbers come from one consistent snapshot).
--
-- This is the fix for the cross-statement race in read_delegation_transcript.go:
-- previously Count() and ListPaginated() were two independent queries, so a
-- message inserted between them (the normal case while observing a live
-- sub-agent) could make the total and the offset-derived window disagree.
-- message.Service.ListPaginatedSnapshot calls this query FIRST to pin down a
-- single high-water-mark snapshot (the boundary row's created_at/row_id, plus
-- total), then fetches the window strictly at-or-before that boundary via
-- ListMessagesBySessionOlderThanCreatedAt + ListMessagesBySessionAtCreatedAt.
-- Any message inserted after this query executes is newer than the pinned
-- boundary and is therefore correctly excluded from both the "total" figure
-- and the follow-up window fetch - the read as a whole reflects one
-- consistent point in time rather than drifting across separate round trips.
--
-- created_at/row_id are NULL when offset falls at or past the end of the
-- session's messages (i.e. there is no row at that position): callers must
-- treat NULL as "start of history", matching offset's existing clamp-to-empty
-- behavior in clampTranscriptWindow.
--
-- row_id is exposed via CAST(rowid AS INTEGER): sqlc's SQLite catalog has no
-- notion of SQLite's implicit rowid column outside ORDER BY (confirmed by
-- hand: any bare `rowid` reference in a WHERE clause or plain SELECT-list
-- position fails static analysis with "column \"rowid\" does not exist",
-- while the identical reference wrapped in CAST(... AS INTEGER) in the
-- SELECT list is accepted) - see ListMessagesBySessionOlderThanCreatedAt's
-- comment for why the keyset filter itself therefore avoids rowid in a WHERE
-- clause entirely rather than fighting this further.
SELECT
    created_at,
    CAST(rowid AS INTEGER) AS row_id,
    COUNT(*) OVER () AS total_count
FROM messages
WHERE session_id = ?
ORDER BY created_at DESC, rowid DESC
LIMIT 1 OFFSET ?;

-- name: ListMessagesBySessionOlderThanCreatedAt :many
-- First half of the keyset pagination pair used by
-- read_delegation_transcript.go: returns messages strictly OLDER (by
-- created_at second) than the cursor boundary, deterministically ordered.
-- Combined in Go with ListMessagesBySessionAtCreatedAt (which handles the
-- boundary second's exact-tie rows via the rowid tiebreaker) to reproduce the
-- same (created_at, rowid) keyset semantics as a single
-- `(created_at, rowid) < (?, ?)` comparison would, without needing rowid in a
-- WHERE clause - see GetTranscriptWindowCursor's comment for why that's
-- avoided. Because the boundary is a concrete value pinned by
-- GetTranscriptWindowCursor at the start of the read (not "the Nth row from
-- the current end"), inserting new messages at the head between calls can
-- never shift which rows this query returns: the boundary itself doesn't move.
SELECT *
FROM messages
WHERE session_id = ?
  AND created_at < ?
ORDER BY created_at DESC, rowid DESC
LIMIT ?;

-- name: ListMessagesBySessionAtCreatedAt :many
-- Second half of the keyset pagination pair (see
-- ListMessagesBySessionOlderThanCreatedAt): returns every message in the
-- session sharing the exact boundary second, WITH its rowid (via
-- CAST(rowid AS INTEGER), the one position sqlc's SQLite catalog accepts a
-- bare rowid reference in - see GetTranscriptWindowCursor's comment), so the
-- Go caller can apply the `rowid < boundary_row_id` half of the tiebreaker
-- itself and merge the result with the older-seconds query above into one
-- deterministic (created_at DESC, rowid DESC) window.
SELECT
    sqlc.embed(messages),
    CAST(rowid AS INTEGER) AS row_id
FROM messages
WHERE session_id = ?
  AND created_at = ?
ORDER BY rowid DESC;

-- name: UpdateMessageUsage :exec
-- Per-message token usage and prompt-cache accounting (task #469).
--
-- Written by a separate statement rather than folded into UpdateMessage on
-- purpose: the agent appends the Finish part BEFORE the step's usage is
-- resolved (internal/agent/agent.go - AddFinish precedes fallbackStepUsage),
-- so there is no single moment where both are in hand. Keeping usage separate
-- avoids reordering that finish chain.
--
-- All placeholders are bare `?` in binding order. Do NOT mix bare `?` with
-- numbered/named args here: SQLite numbers a bare `?` from the highest
-- explicit index already used, which silently shifts positional binding.
UPDATE messages
SET
    input_tokens = ?,
    output_tokens = ?,
    reasoning_tokens = ?,
    cache_creation_tokens = ?,
    cache_read_tokens = ?,
    total_tokens = ?,
    cost_usd = ?,
    usage_provider = ?,
    usage_model = ?,
    cache_support = ?,
    usage_estimated = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;

-- name: SumMessageUsageBySession :many
-- Cache/token/cost aggregate for one session, grouped by the model that
-- actually produced each message (a session can switch models mid-run, and
-- averaging their cache behaviour into one number describes neither).
--
-- recorded/estimated are returned so a caller can state its coverage rather
-- than implying a total was computed over everything: rows written before this
-- feature existed have NULL usage and are excluded by the WHERE clause.
--
-- Every SUM is wrapped in CAST: without it sqlc infers interface{} for an
-- aggregate over a nullable column and the generated struct is unusable.
SELECT
    COALESCE(usage_provider, '') AS provider,
    COALESCE(usage_model, '') AS model,
    COALESCE(cache_support, '') AS cache_support,
    CAST(COUNT(*) AS INTEGER) AS recorded,
    CAST(COALESCE(SUM(usage_estimated), 0) AS INTEGER) AS estimated,
    CAST(COALESCE(SUM(input_tokens), 0) AS INTEGER) AS input_tokens,
    CAST(COALESCE(SUM(output_tokens), 0) AS INTEGER) AS output_tokens,
    CAST(COALESCE(SUM(reasoning_tokens), 0) AS INTEGER) AS reasoning_tokens,
    CAST(COALESCE(SUM(cache_creation_tokens), 0) AS INTEGER) AS cache_creation_tokens,
    CAST(COALESCE(SUM(cache_read_tokens), 0) AS INTEGER) AS cache_read_tokens,
    CAST(COALESCE(SUM(total_tokens), 0) AS INTEGER) AS total_tokens,
    CAST(COALESCE(SUM(cost_usd), 0) AS REAL) AS cost_usd
FROM messages
WHERE session_id = ? AND total_tokens IS NOT NULL
GROUP BY usage_provider, usage_model, cache_support;

-- name: CountMessagesMissingUsage :one
-- Assistant rows in a session that carry no usage at all. Reported alongside
-- any aggregate so "12% cache hit" is never presented without saying how many
-- messages it was computed over.
SELECT COUNT(*) FROM messages
WHERE session_id = ? AND role = 'assistant' AND total_tokens IS NULL;

-- name: SumMessageUsageByModelInRange :many
-- Per-model token/cache/cost totals across ALL sessions in a time window.
--
-- Grouped by the model that actually PRODUCED each message, not by the
-- session's current model. `sessions cost` groups by sessions.smart_model_id,
-- so a session that switched models attributes every token to whichever model
-- it happened to end on; this does not.
--
-- Child (sub-agent) sessions are deliberately INCLUDED and this does NOT
-- double-count. TransferChildCostToParent moves a child's cost into the
-- PARENT'S sessions.cost column and never touches message rows, so each
-- message's cost_usd is counted exactly once here. (`sessions cost` must
-- exclude child sessions for precisely the opposite reason.)
--
-- Every SUM is wrapped in CAST or sqlc infers interface{} for an aggregate
-- over a nullable column and the generated struct is unusable.
SELECT
    COALESCE(usage_provider, '') AS provider,
    COALESCE(usage_model, '') AS model,
    COALESCE(cache_support, '') AS cache_support,
    CAST(COUNT(*) AS INTEGER) AS recorded,
    CAST(COALESCE(SUM(usage_estimated), 0) AS INTEGER) AS estimated,
    CAST(COALESCE(SUM(input_tokens), 0) AS INTEGER) AS input_tokens,
    CAST(COALESCE(SUM(output_tokens), 0) AS INTEGER) AS output_tokens,
    CAST(COALESCE(SUM(reasoning_tokens), 0) AS INTEGER) AS reasoning_tokens,
    CAST(COALESCE(SUM(cache_creation_tokens), 0) AS INTEGER) AS cache_creation_tokens,
    CAST(COALESCE(SUM(cache_read_tokens), 0) AS INTEGER) AS cache_read_tokens,
    CAST(COALESCE(SUM(total_tokens), 0) AS INTEGER) AS total_tokens,
    CAST(COALESCE(SUM(cost_usd), 0) AS REAL) AS cost_usd
FROM messages
WHERE total_tokens IS NOT NULL
  AND created_at >= ?
  AND created_at <= ?
GROUP BY usage_provider, usage_model, cache_support;

-- name: SumMessageUsageByDayInRange :many
-- Same window and semantics as SumMessageUsageByModelInRange, bucketed by
-- local calendar day so it lines up with `sessions cost --by day`, which
-- formats with time.Unix(...).Format("2006-01-02") in local time.
SELECT
    CAST(strftime('%Y-%m-%d', created_at, 'unixepoch', 'localtime') AS TEXT) AS day,
    CAST(COUNT(*) AS INTEGER) AS recorded,
    CAST(COALESCE(SUM(usage_estimated), 0) AS INTEGER) AS estimated,
    CAST(COALESCE(SUM(input_tokens), 0) AS INTEGER) AS input_tokens,
    CAST(COALESCE(SUM(output_tokens), 0) AS INTEGER) AS output_tokens,
    CAST(COALESCE(SUM(reasoning_tokens), 0) AS INTEGER) AS reasoning_tokens,
    CAST(COALESCE(SUM(cache_creation_tokens), 0) AS INTEGER) AS cache_creation_tokens,
    CAST(COALESCE(SUM(cache_read_tokens), 0) AS INTEGER) AS cache_read_tokens,
    CAST(COALESCE(SUM(total_tokens), 0) AS INTEGER) AS total_tokens,
    CAST(COALESCE(SUM(cost_usd), 0) AS REAL) AS cost_usd
FROM messages
WHERE total_tokens IS NOT NULL
  AND created_at >= ?
  AND created_at <= ?
GROUP BY day
ORDER BY day;

-- name: CountMessagesMissingUsageInRange :one
-- Assistant messages in the window with no usage recorded. Reported next to
-- any aggregate so a ratio is never presented as the period's when it was
-- computed over a fraction of it.
SELECT CAST(COUNT(*) AS INTEGER) FROM messages
WHERE role = 'assistant'
  AND total_tokens IS NULL
  AND created_at >= ?
  AND created_at <= ?;

-- name: ListCandidateInterruptedAssistantSessions :many
-- task #774: startup recovery (app.recoverInterruptedTurns) used to call
-- Sessions.ListAll then, for EVERY session, Messages.List (the full message
-- history) just to find the last assistant message. Measured on a real dev
-- DB: 137 sessions, 10s+, tripping the sweep's own 10s deadline -- an
-- O(sessions) linear scan that only gets worse as session count grows.
--
-- This query returns only the small set of sessions whose CHRONOLOGICALLY
-- LAST message is an unfinished assistant message -- the exact candidate set
-- recovery needs -- so the caller can skip straight to those sessions
-- instead of touching every one.
--
-- Correctness nuance: a session can have an OLD unfinished assistant message
-- later superseded by a newer, finished one (e.g. a retried turn). A flat
-- `WHERE role = 'assistant' AND finished_at IS NULL` over the whole table
-- would wrongly flag that session even though its latest message is fine.
-- The window function ranks every message within its session by recency
-- first, and only rank-1 (the latest) rows are checked against the
-- unfinished-assistant predicate.
--
-- rowid is the tie-breaker for the same reason as ListMessagesBySession /
-- ListUserMessagesBySession above: created_at is stored in SECONDS, so a
-- single agent turn can produce multiple rows sharing one created_at value.
-- (created_at DESC, rowid DESC) is a deterministic newest-first total order.
--
-- idx_messages_unfinished_assistant (session_id, created_at) WHERE
-- role = 'assistant' AND finished_at IS NULL accelerates the base filter
-- pass (SQLite can seek directly to the small set of unfinished-assistant
-- rows via the index rather than a full table scan); the ranking itself
-- still needs each candidate session's full ORDER BY to determine "is this
-- really the last message", which the CTE below narrows to just the
-- sessions the index already flagged, not every session in the table.
--
-- parent_session_id is returned alongside so the caller can partition
-- top-level vs child sessions without a second per-candidate session
-- lookup.
WITH unfinished_candidates AS (
    SELECT DISTINCT session_id
    FROM messages
    WHERE role = 'assistant' AND finished_at IS NULL
),
ranked AS (
    SELECT
        m.id AS message_id,
        m.session_id AS session_id,
        m.role AS role,
        m.finished_at AS finished_at,
        m.created_at AS created_at,
        ROW_NUMBER() OVER (
            PARTITION BY m.session_id
            ORDER BY m.created_at DESC, m.rowid DESC
        ) AS rn
    FROM messages m
    WHERE m.session_id IN (SELECT session_id FROM unfinished_candidates)
)
SELECT
    ranked.session_id AS session_id,
    ranked.message_id AS message_id,
    ranked.created_at AS created_at,
    s.parent_session_id AS parent_session_id
FROM ranked
JOIN sessions s ON s.id = ranked.session_id
WHERE ranked.rn = 1
  AND ranked.role = 'assistant'
  AND ranked.finished_at IS NULL;

-- name: StampInterruptedAssistantIfStillLast :execrows
-- task #777 (P1 release blocker): recoverSessionInterruptedTurn used to
-- read the candidate message (Get), re-check IsFinished() in Go, check the
-- liveness lock, then call the plain message.Update, which rewrites the
-- WHOLE parts blob from that Get-time snapshot. Both the read-then-write gap
-- AND the whole-blob overwrite were unguarded TOCTOU windows:
--
--   1. ListCandidateInterruptedAssistantSessions proved this message was the
--      session's LAST message only at the instant the discovery query ran.
--      If a newer user/assistant message lands before this write, the
--      candidate is stale and must not be stamped -- the session has moved
--      on, and stamping the older message would show a spurious "Process
--      restarted" on a turn that already has a live successor.
--   2. A plain UPDATE ... WHERE id = ? has no way to notice #1, and also
--      clobbers any Parts content the live owner wrote in the same window
--      (e.g. a checkpoint tick) since the write is not conditioned on the
--      row being unchanged since the Get.
--
-- This mirrors DeleteMessageIfTerminal / UpdateMessageIfNotTerminal: the
-- read-then-act race is closed by moving the predicate into the WHERE clause
-- of a single statement instead of trusting a prior read. The WHERE
-- requires, atomically:
--   - finished_at IS NULL (still unfinished -- same re-check the Go code
--     used to do after the initial Get, now enforced in the same statement
--     as the write instead of a separate round trip before it)
--   - rowid = the MAX(rowid) among this session's messages (still the
--     session's chronologically last message, using rowid rather than
--     created_at for the same reason as ListCandidateInterruptedAssistantSessions
--     and ListMessagesBySession: created_at is second-granularity, so a
--     single turn's rows can tie, and rowid is SQLite's monotonic insertion
--     counter -- see ListAllUserMessages's comment for the canonical
--     explanation of why rowid is the established tiebreaker throughout this
--     file)
--
-- Returns rows affected: 0 means the candidate went stale between discovery
-- and this write (superseded by a newer message, or finished concurrently by
-- its live owner) -- the caller must treat that as "skip, do not retry",
-- exactly like DeleteMessageIfTerminal's 0-rows-affected contract.
UPDATE messages
SET
    parts = ?,
    finished_at = ?,
    updated_at = strftime('%s', 'now')
WHERE messages.id = ?
  AND messages.finished_at IS NULL
  AND messages.rowid = (
      SELECT MAX(other.rowid) FROM messages AS other WHERE other.session_id = ?
  );

-- task #737: GetMessageRowID / GetMaxMessageRowIDBySession (the task #731
-- delete-watermark queries) were removed here. MAX(rowid) over a session's
-- REMAINING messages is not a monotonic "highest rowid ever assigned"
-- value -- deleting a NON-TAIL message (e.g. an older message while a
-- newer one survives) does not lower MAX(rowid), so the watermark never
-- moved for that class of delete and a stale pre-delete snapshot could
-- still be judged "fresh", resurrecting the deleted message client-side.
-- Replaced by an in-memory per-session delete-generation counter
-- (message.service's deleteGen map) -- see internal/message/message.go's
-- ListWithWatermark doc comment for the full mechanism.
