// Fork tests: the transactional fork happy path, ForkOptions truncation and
// parent linkage, the empty-source no-limit default, and midway-failure
// rollback, plus the count helpers they assert against.
package session

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestForkSessionTx_PreservesFinishedAt verifies completed assistant messages
// retain their terminal timestamp when copied. Without it, SQL sees the forked
// row as still streaming despite the copied Finish part.
func TestForkSessionTx_PreservesFinishedAt(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	src, err := svc.Create(ctx, "source")
	require.NoError(t, err)
	_, err = sqlDB.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at, finished_at) VALUES (?, ?, 'assistant', ?, 100, 100, ?)`,
		uuid.NewString(), src.ID, `[{"type":"finish","data":{"reason":"stop"}}]`, 123)
	require.NoError(t, err)

	fork, _, err := svc.(*service).ForkSessionTx(ctx, src.ID, ForkOptions{})
	require.NoError(t, err)

	var finishedAt sql.NullInt64
	require.NoError(t, sqlDB.QueryRowContext(ctx,
		`SELECT finished_at FROM messages WHERE session_id = ? AND role = 'assistant'`, fork.ID).Scan(&finishedAt))
	require.True(t, finishedAt.Valid, "forked assistant should retain its terminal timestamp")
	require.EqualValues(t, 123, finishedAt.Int64)
}

func countMsgsForSession(t *testing.T, ctx context.Context, sqlDB *sql.DB, sessionID string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&n))
	return n
}

func countAllSessions(t *testing.T, ctx context.Context, sqlDB *sql.DB) int64 {
	t.Helper()
	var n int64
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n))
	return n
}

// TestForkSession_HappyPath verifies the transactional fork clones the
// source session's models, reasoning effort, system prompt, todos, and
// EVERY message into a new session. Reasoning effort is asserted here
// because ForkSession (the web fork button's path) used to silently drop it
// — a divergence from the independently-written CLI fork path, which copied
// reasoning effort but dropped todos. Both entry points now share
// ForkSessionTx, which copies the union of both column sets.
func TestForkSession_HappyPath(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	src, err := svc.Create(ctx, "source")
	require.NoError(t, err)
	require.NoError(t, svc.UpdateModels(ctx, src.ID, &ModelSlotUpdate{Provider: "provL", Model: "modelL"}, &ModelSlotUpdate{Provider: "provS", Model: "modelS"}))
	require.NoError(t, svc.UpdateReasoningEffort(ctx, src.ID, "high", "low"))
	require.NoError(t, svc.UpdateSystemPrompt(ctx, src.ID, "be careful"))
	require.NoError(t, svc.SetTodos(ctx, src.ID, []Todo{{Content: "do thing", Status: TodoStatusPending}}, nil))

	// Seed three messages with distinguishable parts and deterministic order.
	seedRoles := []string{"user", "assistant", "user"}
	seedParts := []string{
		`[{"type":"text","data":{"text":"a"}}]`,
		`[{"type":"text","data":{"text":"b"}}]`,
		`[{"type":"text","data":{"text":"c"}}]`,
	}
	for i := range seedRoles {
		_, err := sqlDB.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), src.ID, seedRoles[i], seedParts[i], 100+i, 100+i)
		require.NoError(t, err)
	}
	require.EqualValues(t, 3, countMsgsForSession(t, ctx, sqlDB, src.ID))

	fork, err := svc.ForkSession(ctx, src.ID, "")
	require.NoError(t, err)
	require.NotEqual(t, src.ID, fork.ID)
	require.Equal(t, "source fork", fork.Title)
	require.Equal(t, "provL", fork.SmartModelProvider)
	require.Equal(t, "modelS", fork.FastModelID)
	require.Equal(t, "high", fork.SmartModelReasoningEffort)
	require.Equal(t, "low", fork.FastModelReasoningEffort)
	require.Equal(t, "be careful", fork.SystemPrompt)
	require.Len(t, fork.Todos, 1)
	require.Equal(t, "do thing", fork.Todos[0].Content)

	require.EqualValues(t, 3, countMsgsForSession(t, ctx, sqlDB, fork.ID))

	rows, err := q.ListMessagesBySession(ctx, fork.ID)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	require.Equal(t, "user", rows[0].Role)
	require.Equal(t, "assistant", rows[1].Role)
	require.Equal(t, "user", rows[2].Role)
	require.Equal(t, seedParts[0], rows[0].Parts)
}

// TestForkSessionTx_AtTruncationAndChild verifies ForkOptions.LimitMsgs
// truncates the copy to the first N messages and ForkOptions.ParentID links
// the fork as a child session — the two CLI-specific knobs (--at, --child)
// that ForkSession's defaults do not exercise.
func TestForkSessionTx_AtTruncationAndChild(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	src, err := svc.Create(ctx, "source")
	require.NoError(t, err)
	for i, role := range []string{"user", "assistant", "user", "assistant"} {
		_, err := sqlDB.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at) VALUES (?, ?, ?, '[]', ?, ?)`,
			uuid.NewString(), src.ID, role, 100+i, 100+i)
		require.NoError(t, err)
	}

	fork, count, err := svc.(*service).ForkSessionTx(ctx, src.ID, ForkOptions{
		ParentID:  src.ID,
		LimitMsgs: 2,
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
	require.Equal(t, src.ID, fork.ParentSessionID)
	require.EqualValues(t, 2, countMsgsForSession(t, ctx, sqlDB, fork.ID))
}

// TestForkSessionTx_EmptySourceNoLimit verifies forking a source session
// with zero messages succeeds when LimitMsgs is left at its zero value: the
// range check (limit < 1 || limit > len(srcMsgs)) must be skipped for the
// "copy everything" default rather than resolving limit to 0 and then
// rejecting it as out of range. This is the regression covered by the CLI's
// TestForkSessionCLI_EmptySourceNoAt for the underlying shared function.
func TestForkSessionTx_EmptySourceNoLimit(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	src, err := svc.Create(ctx, "source")
	require.NoError(t, err)
	require.Zero(t, countMsgsForSession(t, ctx, sqlDB, src.ID))

	fork, count, err := svc.(*service).ForkSessionTx(ctx, src.ID, ForkOptions{})
	require.NoError(t, err)
	require.Zero(t, count)
	require.NotEqual(t, src.ID, fork.ID)
}

// TestForkSessionTx_SummaryPointerFollowsCopiedBoundary covers pointer mapping
// when copied, no source pointer, and a pointer excluded by LimitMsgs.
func TestForkSessionTx_SummaryPointerFollowsCopiedBoundary(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		pointerAt int
		limit     int
		wantPtr   bool
	}{
		{name: "mapped pointer", pointerAt: 2, wantPtr: true},
		{name: "no pointer"},
		{name: "prefix excludes pointer", pointerAt: 3, limit: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			limitParallel(t)
			sqlDB, q := newTestDB(t)
			svc := NewService(q, sqlDB).(*service)
			ctx := t.Context()
			src, err := svc.Create(ctx, "source")
			require.NoError(t, err)
			var sourceMessageID string
			sourceParts := []string{
				`[{"type":"text","data":{"text":"row-1"}}]`,
				`[{"type":"text","data":{"text":"row-2"}}]`,
				`[{"type":"text","data":{"text":"row-3"}}]`,
			}
			var summaryParts string
			for i := 1; i <= 3; i++ {
				id := uuid.NewString()
				_, err := sqlDB.ExecContext(ctx,
					`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at) VALUES (?, ?, 'user', ?, ?, ?)`,
					id, src.ID, sourceParts[i-1], 100+i, 100+i)
				require.NoError(t, err)
				if i == tc.pointerAt {
					sourceMessageID = id
					summaryParts = sourceParts[i-1]
				}
			}
			if sourceMessageID != "" {
				_, err = sqlDB.ExecContext(ctx, `UPDATE sessions SET summary_message_id = ? WHERE id = ?`, sourceMessageID, src.ID)
				require.NoError(t, err)
			}
			fork, _, err := svc.ForkSessionTx(ctx, src.ID, ForkOptions{LimitMsgs: tc.limit})
			require.NoError(t, err)
			if !tc.wantPtr {
				require.Empty(t, fork.SummaryMessageID)
				return
			}
			require.NotEmpty(t, fork.SummaryMessageID)
			require.NotEqual(t, sourceMessageID, fork.SummaryMessageID)
			var copiedSessionID, copiedParts string
			require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT session_id, parts FROM messages WHERE id = ?`, fork.SummaryMessageID).Scan(&copiedSessionID, &copiedParts))
			require.Equal(t, fork.ID, copiedSessionID)
			require.Equal(t, summaryParts, copiedParts)
		})
	}
}

// TestForkSessionTx_PreservesPinnedMessage verifies pinned messages remain
// pinned when their rows are cloned into the fork.
func TestForkSessionTx_PreservesPinnedMessage(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB).(*service)
	ctx := t.Context()
	src, err := svc.Create(ctx, "source")
	require.NoError(t, err)
	pinnedID := uuid.NewString()
	_, err = sqlDB.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, parts, pinned, created_at, updated_at) VALUES (?, ?, 'user', '[]', 1, 1, 1)`,
		pinnedID, src.ID)
	require.NoError(t, err)

	fork, _, err := svc.ForkSessionTx(ctx, src.ID, ForkOptions{})
	require.NoError(t, err)
	var pinned int64
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT pinned FROM messages WHERE session_id = ?`, fork.ID).Scan(&pinned))
	require.EqualValues(t, 1, pinned)
}

// TestForkSession_MidwayFailureRollsBack arms a trigger that aborts the
// copy of a sentinel-role message placed AFTER two normal messages, then
// asserts the ENTIRE fork (new session row + every copied message) is rolled
// back and the caller receives the error — no partial fork survives.
func TestForkSession_MidwayFailureRollsBack(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	src, err := svc.Create(ctx, "source")
	require.NoError(t, err)

	// Two normal messages (copied first), then a sentinel message whose role
	// trips the trigger. created_at orders ListMessagesBySession so the
	// sentinel is reached only after the normal copies succeed.
	for i, role := range []string{"user", "assistant", "TRIPWIRE"} {
		_, err := sqlDB.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at) VALUES (?, ?, ?, '[]', ?, ?)`,
			uuid.NewString(), src.ID, role, 100+i, 100+i)
		require.NoError(t, err)
	}

	// Arm the tripwire AFTER seeding the source so seeding is not aborted.
	// The BEFORE INSERT trigger fires (and aborts) only when ForkSession
	// copies the TRIPWIRE message.
	_, err = sqlDB.ExecContext(ctx, `
		CREATE TRIGGER tripwire_before_insert
		BEFORE INSERT ON messages
		WHEN new.role = 'TRIPWIRE'
		BEGIN
			SELECT RAISE(ABORT, 'injected midway copy failure');
		END`)
	require.NoError(t, err)

	beforeSessions := countAllSessions(t, ctx, sqlDB)
	beforeSrcMsgs := countMsgsForSession(t, ctx, sqlDB, src.ID)

	_, err = svc.ForkSession(ctx, src.ID, "should-not-survive")
	require.Error(t, err, "midway copy failure must surface as an error to the caller")

	// No new session row committed: total session count unchanged.
	require.Equal(t, beforeSessions, countAllSessions(t, ctx, sqlDB), "fork session must not survive a rolled-back fork")
	// Source is untouched.
	require.Equal(t, beforeSrcMsgs, countMsgsForSession(t, ctx, sqlDB, src.ID), "source messages must be untouched")
	// No orphan messages reference any non-source session.
	var orphanCount int64
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE session_id != ?`, src.ID).Scan(&orphanCount))
	require.Zero(t, orphanCount, "no partial fork messages may remain after rollback")
}
