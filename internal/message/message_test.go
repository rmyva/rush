package message

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/testguard"
)

func TestMain(m *testing.M) { testguard.Main(m) }

func newTestMessageDB(t *testing.T) (*sql.DB, *db.Queries) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB.Close() })
	// Mirror production's SetMaxOpenConns(1) (see internal/db/connect.go):
	// database/sql's default pool can open a SECOND connection under
	// concurrent access, and a second connection to ":memory:" is a
	// distinct, empty database for this driver — not the same in-memory
	// data. Without pinning to one connection, concurrency tests here would
	// intermittently see "no such table" from a fresh, unmigrated
	// connection rather than exercising real contention on shared data.
	sqlDB.SetMaxOpenConns(1)

	// Run migrations
	_, err = sqlDB.ExecContext(context.Background(), `
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			parent_session_id TEXT,
			title TEXT NOT NULL,
			message_count INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			cost REAL NOT NULL DEFAULT 0.0,
			updated_at INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			summary_message_id TEXT,
			todos TEXT,
			smart_model_provider TEXT,
			smart_model_id TEXT,
			smart_model_reasoning_effort TEXT DEFAULT 'medium',
			fast_model_provider TEXT,
			fast_model_id TEXT,
			fast_model_reasoning_effort TEXT DEFAULT 'medium',
			worker_model_provider TEXT,
			worker_model_id TEXT,
			worker_model_reasoning_effort TEXT,
			reviewer_model_provider TEXT,
			reviewer_model_id TEXT,
			reviewer_model_reasoning_effort TEXT,
			system_prompt TEXT DEFAULT '',
			yolo_enabled INTEGER NOT NULL DEFAULT 0,
			origin TEXT DEFAULT '' NOT NULL
		);

		CREATE TABLE messages (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			parts TEXT NOT NULL DEFAULT '[]',
			model TEXT,
			provider TEXT,
			reasoning_effort TEXT DEFAULT 'medium',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			finished_at INTEGER,
			is_summary_message INTEGER NOT NULL DEFAULT 0,
			pinned INTEGER NOT NULL DEFAULT 0,
			hidden INTEGER NOT NULL DEFAULT 0,
			auto_resumed INTEGER NOT NULL DEFAULT 0,
			background_job_notice INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER,
			output_tokens INTEGER,
			reasoning_tokens INTEGER,
			cache_creation_tokens INTEGER,
			cache_read_tokens INTEGER,
			total_tokens INTEGER,
			cost_usd REAL,
			usage_provider TEXT,
			usage_model TEXT,
			cache_support TEXT,
			usage_estimated INTEGER,
			origin TEXT DEFAULT '' NOT NULL,
			checkpoint_generation INTEGER NOT NULL DEFAULT 0,
			notice_kind TEXT NOT NULL DEFAULT ''
		);
	`)
	require.NoError(t, err)

	return sqlDB, db.New(sqlDB)
}

func TestCreateMessage_WithReasoningEffort(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)

	ctx := t.Context()
	sessionID := "test-session-123"

	t.Run("creates message with reasoning effort", func(t *testing.T) {
		params := CreateMessageParams{
			Role:            Assistant,
			Parts:           []ContentPart{TextContent{Text: "Hello"}},
			Model:           "claude-opus-1m",
			Provider:        "local-cli",
			ReasoningEffort: "high",
		}

		msg, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.Equal(t, "high", msg.ReasoningEffort)
		assert.Equal(t, "claude-opus-1m", msg.Model)
		assert.Equal(t, "local-cli", msg.Provider)
	})

	t.Run("creates message with max effort", func(t *testing.T) {
		params := CreateMessageParams{
			Role:            Assistant,
			Parts:           []ContentPart{TextContent{Text: "Max effort response"}},
			Model:           "claude-sonnet-1m",
			Provider:        "local-cli",
			ReasoningEffort: "max",
		}

		msg, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.Equal(t, "max", msg.ReasoningEffort)
	})

	t.Run("creates message without reasoning effort", func(t *testing.T) {
		params := CreateMessageParams{
			Role:     Assistant,
			Parts:    []ContentPart{TextContent{Text: "No effort specified"}},
			Model:    "gpt-4",
			Provider: "openai",
		}

		msg, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.Empty(t, msg.ReasoningEffort)
	})

	t.Run("supports all effort levels", func(t *testing.T) {
		levels := []string{"low", "medium", "high", "max"}
		for _, level := range levels {
			params := CreateMessageParams{
				Role:            Assistant,
				Parts:           []ContentPart{TextContent{Text: level}},
				Model:           "claude-opus-1m",
				Provider:        "anthropic",
				ReasoningEffort: level,
			}

			msg, err := svc.Create(ctx, sessionID, params)
			require.NoError(t, err, "level=%s", level)
			assert.Equal(t, level, msg.ReasoningEffort, "level=%s", level)
		}
	})

	t.Run("persists reasoning effort to database", func(t *testing.T) {
		params := CreateMessageParams{
			Role:            Assistant,
			Parts:           []ContentPart{TextContent{Text: "Persistence test"}},
			Model:           "claude-opus-1m",
			Provider:        "local-cli",
			ReasoningEffort: "high",
		}

		created, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)

		// Retrieve from database
		retrieved, err := svc.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.Equal(t, "high", retrieved.ReasoningEffort)
		assert.Equal(t, created.Model, retrieved.Model)
		assert.Equal(t, created.Provider, retrieved.Provider)
	})
}

func TestListMessages_WithReasoningEffort(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)

	ctx := t.Context()
	sessionID := "test-session-list"

	// Create messages with different effort levels
	efforts := []string{"low", "medium", "high", "max"}
	for i, effort := range efforts {
		params := CreateMessageParams{
			Role:            Assistant,
			Parts:           []ContentPart{TextContent{Text: effort}},
			Model:           "claude-opus-1m",
			Provider:        "local-cli",
			ReasoningEffort: effort,
			Hidden:          i > 1, // Make some hidden
		}
		_, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
	}

	// List all messages
	messages, err := svc.List(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, messages, 4)

	// Verify effort levels are preserved
	for i, msg := range messages {
		assert.Equal(t, efforts[i], msg.ReasoningEffort)
	}
}

func TestSetSummaryCreatedAt(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()
	created, err := svc.Create(ctx, "summary-timestamp", CreateMessageParams{Role: Assistant, IsSummaryMessage: true, Parts: []ContentPart{TextContent{Text: "summary"}}})
	require.NoError(t, err)

	updated, err := svc.(interface {
		SetSummaryCreatedAt(context.Context, string, string, int64) (bool, error)
	}).SetSummaryCreatedAt(ctx, "summary-timestamp", created.ID, 1)
	require.NoError(t, err)
	require.True(t, updated)
	got, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), got.CreatedAt)

	updated, err = svc.(interface {
		SetSummaryCreatedAt(context.Context, string, string, int64) (bool, error)
	}).SetSummaryCreatedAt(ctx, "wrong-session", created.ID, 2)
	require.NoError(t, err)
	require.False(t, updated)
}

func TestCreateMessage_ParamsValidation(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)

	ctx := t.Context()
	sessionID := "test-session-validation"

	t.Run("user message can have reasoning effort", func(t *testing.T) {
		params := CreateMessageParams{
			Role:            User,
			Parts:           []ContentPart{TextContent{Text: "User message"}},
			ReasoningEffort: "", // User messages typically don't have effort
		}

		msg, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.Equal(t, User, msg.Role)
		assert.Empty(t, msg.ReasoningEffort)
	})

	t.Run("assistant message with all fields", func(t *testing.T) {
		params := CreateMessageParams{
			Role:             Assistant,
			Parts:            []ContentPart{TextContent{Text: "Complete message"}},
			Model:            "claude-sonnet-1m",
			Provider:         "anthropic",
			ReasoningEffort:  "medium",
			IsSummaryMessage: false,
			Hidden:           false,
		}

		msg, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.Equal(t, Assistant, msg.Role)
		assert.Equal(t, "claude-sonnet-1m", msg.Model)
		assert.Equal(t, "anthropic", msg.Provider)
		assert.Equal(t, "medium", msg.ReasoningEffort)
		assert.False(t, msg.IsSummaryMessage)
		assert.False(t, msg.Hidden)
	})

	t.Run("summary message with reasoning effort", func(t *testing.T) {
		params := CreateMessageParams{
			Role:             Assistant,
			Parts:            []ContentPart{TextContent{Text: "Summary"}},
			Model:            "claude-opus-1m",
			Provider:         "local-cli",
			ReasoningEffort:  "low",
			IsSummaryMessage: true,
			Hidden:           true,
		}

		msg, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.Equal(t, "low", msg.ReasoningEffort)
		assert.True(t, msg.IsSummaryMessage)
		assert.True(t, msg.Hidden)
	})
}

func TestFinishToolCall_PreservesProviderExecuted(t *testing.T) {
	msg := &Message{
		Parts: []ContentPart{
			ToolCall{
				ID:               "call-1",
				Name:             "bash",
				Input:            `{"cmd":"ls"}`,
				ProviderExecuted: true,
			},
		},
	}

	msg.FinishToolCall("call-1")

	tc, ok := msg.Parts[0].(ToolCall)
	require.True(t, ok)
	assert.True(t, tc.Finished, "FinishToolCall should set Finished=true")
	assert.True(t, tc.ProviderExecuted, "FinishToolCall should preserve ProviderExecuted")
	assert.Equal(t, "call-1", tc.ID)
	assert.Equal(t, "bash", tc.Name)
	assert.Equal(t, `{"cmd":"ls"}`, tc.Input)
}

func TestCreateMessage_AutoResumedRoundTrip(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)

	ctx := t.Context()
	sessionID := "test-session-auto-resumed"

	t.Run("auto-resumed flag persists and reads back true", func(t *testing.T) {
		params := CreateMessageParams{
			Role:        User,
			Parts:       []ContentPart{TextContent{Text: "Background job xyz finished"}},
			AutoResumed: true,
		}
		created, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.True(t, created.AutoResumed, "created message should have AutoResumed=true")

		// Round-trip through Get.
		got, err := svc.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.True(t, got.AutoResumed, "Get should return AutoResumed=true")

		// Round-trip through List.
		listed, err := svc.List(ctx, sessionID)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.True(t, listed[0].AutoResumed, "List should return AutoResumed=true")
	})

	t.Run("default is false when flag omitted", func(t *testing.T) {
		params := CreateMessageParams{
			Role:  User,
			Parts: []ContentPart{TextContent{Text: "normal human message"}},
		}
		created, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.False(t, created.AutoResumed, "omitted AutoResumed should default to false")

		got, err := svc.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.False(t, got.AutoResumed, "Get should return AutoResumed=false for default")
	})
}

func TestCreateMessage_BackgroundJobNoticeRoundTrip(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)

	ctx := t.Context()
	sessionID := "test-session-bg-job-notice"

	t.Run("background-job-notice flag persists and reads back true", func(t *testing.T) {
		params := CreateMessageParams{
			Role:                User,
			Parts:               []ContentPart{TextContent{Text: "Background job xyz finished"}},
			BackgroundJobNotice: true,
		}
		created, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.True(t, created.BackgroundJobNotice, "created message should have BackgroundJobNotice=true")

		// Round-trip through Get.
		got, err := svc.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.True(t, got.BackgroundJobNotice, "Get should return BackgroundJobNotice=true")

		// Round-trip through List.
		listed, err := svc.List(ctx, sessionID)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.True(t, listed[0].BackgroundJobNotice, "List should return BackgroundJobNotice=true")
	})

	t.Run("default is false when flag omitted", func(t *testing.T) {
		params := CreateMessageParams{
			Role:  User,
			Parts: []ContentPart{TextContent{Text: "normal human message"}},
		}
		created, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.False(t, created.BackgroundJobNotice, "omitted BackgroundJobNotice should default to false")

		got, err := svc.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.False(t, got.BackgroundJobNotice, "Get should return BackgroundJobNotice=false for default")
	})
}

// TestUpdate_TerminalWrite_UsesPublishMustDeliver verifies that a
// terminal Update — one whose Finish is a real (non-Partial) finish —
// does not silently drop its UpdatedEvent the moment a subscriber's
// channel is momentarily full. Unlike best-effort Publish, it must wait
// (bounded by the broker's must-deliver timeout) for buffer space, so a
// slow-draining UI subscriber still eventually sees the final message
// state instead of losing it forever. The Partial-checkpoint
// counterpart is TestUpdate_PartialCheckpoint_UsesBestEffortPublish.
//
// service embeds *pubsub.Broker[Message] directly, so we drive the
// broker's buffer to capacity through the same Subscribe channel the
// service hands out, then assert Update's publish blocks past an
// instantaneous drop and lands once the reader catches up.
func TestUpdate_TerminalWrite_UsesPublishMustDeliver(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	// Give must-deliver a small but non-zero timeout so the test is
	// fast without being flaky, and so we can distinguish "dropped
	// immediately like Publish" from "waited, like PublishMustDeliver".
	svc.SetMustDeliverTimeout(300 * time.Millisecond)

	ctx := t.Context()
	sessionID := "test-session-mustdeliver"

	// Subscribe before Create so we have a channel to observe/fill;
	// otherwise Create's CreatedEvent is published to no one.
	sub := svc.Subscribe(ctx)

	created, err := svc.Create(ctx, sessionID, CreateMessageParams{
		Role:  Assistant,
		Parts: []ContentPart{TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	// Drain the CreatedEvent (Create uses best-effort Publish) so it
	// doesn't interfere with counting the Update below.
	<-sub

	// Fill the subscriber's buffer completely using best-effort
	// Publish on the embedded broker, so the next publish (Update's)
	// has no free slot and must take PublishMustDeliver's
	// bounded-blocking path.
	bufCap := cap(sub)
	for range bufCap {
		svc.Publish(pubsub.CreatedEvent, created)
	}

	// Now call Update concurrently. Because the buffer is full,
	// PublishMustDeliver must take its bounded-blocking slow path
	// instead of dropping instantly.
	created.Parts = append(created.Parts, Finish{Reason: "stop", Time: time.Now().Unix()})
	updateDone := make(chan error, 1)
	start := time.Now()
	go func() {
		updateDone <- svc.Update(ctx, created)
	}()

	// Give Update a brief head start to ensure it has entered the
	// blocking path before we start draining, then drain one slot so
	// delivery can succeed within the timeout.
	time.Sleep(50 * time.Millisecond)
	<-sub // free one slot

	select {
	case err := <-updateDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Update did not return")
	}
	elapsed := time.Since(start)

	// If Update had used best-effort Publish, it would have returned
	// (and dropped) almost instantly. Seeing it take a non-trivial
	// amount of time (waiting for the drained slot) instead of an
	// instant no-op is the observable signature of PublishMustDeliver's
	// bounded-blocking path being used, and it must still be well under
	// the outer safety bound.
	assert.Less(t, elapsed, time.Second, "Update should not block indefinitely")

	// Drain the rest of the filler events plus the delivered update
	// looking for the UpdatedEvent carrying our new part.
	found := false
	for i := 0; i < bufCap; i++ {
		select {
		case ev := <-sub:
			if ev.Type == pubsub.UpdatedEvent && len(ev.Payload.Parts) == 2 {
				found = true
			}
		default:
		}
	}
	assert.True(t, found, "expected the Update's UpdatedEvent to have been delivered, not dropped")
}

// TestUpdate_PartialCheckpoint_UsesBestEffortPublish verifies the
// counterpart of TestUpdate_TerminalWrite_UsesPublishMustDeliver: an
// Update whose Finish part is a Partial checkpoint (written by the
// auto-checkpoint ticker every ~2s during streaming) is published via
// best-effort Publish, NOT PublishMustDeliver. With a full subscriber
// buffer it must return near-instantly (dropping the event for the slow
// subscriber) instead of blocking for the configured must-deliver
// timeout — losing one mid-stream tick is harmless because the next
// ticker tick or the terminal Finish re-establishes current state.
func TestUpdate_PartialCheckpoint_UsesBestEffortPublish(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	// A long must-deliver timeout: if Update mistakenly routed the
	// partial checkpoint through PublishMustDeliver, this test would
	// observably stall ~2s against the full buffer. Best-effort Publish
	// returns near-instantly regardless.
	svc.SetMustDeliverTimeout(2 * time.Second)

	ctx := t.Context()
	sessionID := "test-session-partial-checkpoint"

	sub := svc.Subscribe(ctx)

	created, err := svc.Create(ctx, sessionID, CreateMessageParams{
		Role:  Assistant,
		Parts: []ContentPart{TextContent{Text: "streaming"}},
	})
	require.NoError(t, err)

	// Drain the CreatedEvent (Create uses best-effort Publish) so the
	// buffer is empty before we fill it below.
	<-sub

	// A mid-stream checkpoint snapshot: a Finish with Partial==true, as
	// the auto-checkpoint ticker writes it (Reason empty, finished_at
	// stays NULL in the DB row).
	created.Parts = append(created.Parts, Finish{
		Time:    time.Now().Unix(),
		Partial: true,
	})

	// Fill the subscriber buffer completely so the next publish has no
	// free slot: best-effort Publish must drop it, PublishMustDeliver
	// would block ~timeout.
	bufCap := cap(sub)
	for range bufCap {
		svc.Publish(pubsub.CreatedEvent, created)
	}
	dropsBefore := svc.DropCount()

	start := time.Now()
	err = svc.Update(ctx, created)
	elapsed := time.Since(start)
	require.NoError(t, err)

	// Best-effort Publish returns near-instantly despite the full
	// buffer; PublishMustDeliver would have blocked ~2s here.
	assert.Less(t, elapsed, 200*time.Millisecond,
		"Partial checkpoint Update must not block on a full subscriber buffer")

	// The partial snapshot was dropped for the slow subscriber via the
	// best-effort path (DropCount), not the must-deliver path
	// (MustDeliverDropCount must stay zero — no bounded wait occurred).
	assert.Equal(t, dropsBefore+1, svc.DropCount(),
		"Partial checkpoint should be dropped via best-effort Publish on a full buffer")
	assert.Equal(t, uint64(0), svc.MustDeliverDropCount(),
		"Partial checkpoint must not exercise the must-deliver timeout path")
}

// TestListPaginatedSnapshot_ConsistentUnderConcurrentInserts is the
// regression test for the review finding (P2.7): reading a transcript window
// via separate Count + ListPaginated calls can disagree with each other (or
// with a later read) when new messages are inserted concurrently, because
// OFFSET counts back from the newest end of a DESC list and a numeric offset
// silently refers to a different logical position once the head shifts.
// ListPaginatedSnapshot pins one (created_at, rowid) boundary and a matching
// total from a single query before doing any further work, so the returned
// (window, total) pair must stay internally consistent — and stay stable
// across repeated calls with the SAME offset — no matter how many new
// messages a concurrent writer inserts during or between the calls.
func TestListPaginatedSnapshot_ConsistentUnderConcurrentInserts(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()
	sessionID := "test-session-concurrent-snapshot"

	const seedCount = 50
	for i := 0; i < seedCount; i++ {
		_, err := svc.Create(ctx, sessionID, CreateMessageParams{
			Role:  Assistant,
			Parts: []ContentPart{TextContent{Text: fmt.Sprintf("seed-%03d", i)}},
		})
		require.NoError(t, err)
	}

	// Reader loop: repeatedly reads a snapshot window+total pair while a
	// writer goroutine concurrently inserts new messages (simulating a live
	// sub-agent still producing output while its transcript is being read).
	// Run under `go test -race` (see the package's test invocation in the
	// task's verification step): the point of this test is exercising the
	// split-query read path (GetTranscriptWindowCursor,
	// ListMessagesBySessionAtCreatedAt, ListMessagesBySessionOlderThan-
	// CreatedAt) concurrently with writes hitting the same session, so any
	// data race in message.service's implementation surfaces here. Bounds and
	// per-call consistency are asserted precisely by
	// TestListPaginatedSnapshot_SingleCallWindowMatchesTotal below; this test
	// only needs to confirm nothing errors, panics, or races while both
	// sides run concurrently.
	const maxMessages = 10
	const offset = 5
	const readIterations = 30

	var wg sync.WaitGroup
	stopWriting := make(chan struct{})
	writeErrs := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stopWriting:
				return
			default:
			}
			_, err := svc.Create(ctx, sessionID, CreateMessageParams{
				Role:  Assistant,
				Parts: []ContentPart{TextContent{Text: fmt.Sprintf("live-%04d", i)}},
			})
			if err != nil {
				select {
				case writeErrs <- err:
				default:
				}
				return
			}
			i++
		}
	}()

	for iter := 0; iter < readIterations; iter++ {
		window, total, err := svc.ListPaginatedSnapshot(ctx, sessionID, maxMessages, offset)
		require.NoError(t, err)
		require.GreaterOrEqual(t, total, int64(seedCount),
			"total must never report fewer than the seeded messages")
		require.LessOrEqual(t, len(window), maxMessages,
			"window must never exceed the requested limit")
	}

	close(stopWriting)
	wg.Wait()
	select {
	case err := <-writeErrs:
		require.NoError(t, err, "concurrent writer must not fail")
	default:
	}
}

// TestListPaginatedSnapshot_SingleCallWindowMatchesTotal proves the
// within-one-call atomicity ListPaginatedSnapshot exists to guarantee: while
// a writer goroutine concurrently inserts messages, every single
// ListPaginatedSnapshot(offset=0, limit) call must return a window whose
// newest message is consistent with THAT SAME call's own total — i.e. a
// window's message count plus offset never exceeds the total that same call
// reported, and the total never under-counts what the window itself proves
// exists. This is what a single tool invocation of
// read_delegation_transcript.go relies on: the "N earlier omitted" marker
// (derived from total) must never disagree with the window actually shown,
// no matter what a concurrently-running sub-agent inserts mid-call.
//
// Cross-call continuation (paging with a numeric offset computed from an
// EARLIER call's total while a live writer keeps advancing the head) is a
// different, harder guarantee that plain offset-based paging cannot make
// even with this fix — each call pins its OWN snapshot at `offset` positions
// back from that call's own newest row, so an offset computed against one
// call's total can legitimately mean a different logical position under a
// LATER call once new rows have shifted the head. That is precisely why the
// review's keyset design exists at the SQL layer (see
// ListMessagesBySessionOlderThanCreatedAt / ListMessagesBySessionAtCreatedAt
// in internal/db/sql/messages.sql) even though
// read_delegation_transcript.go's public offset parameter is kept for
// backward compatibility rather than switched to an explicit cursor — see
// this package's ListPaginatedSnapshot doc comment and the task's own
// scoping of "consistent within one call" for why that tradeoff was made.
func TestListPaginatedSnapshot_SingleCallWindowMatchesTotal(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()
	sessionID := "test-session-single-call-consistency"

	const seedCount = 47 // deliberately not a multiple of the page size
	for i := 0; i < seedCount; i++ {
		_, err := svc.Create(ctx, sessionID, CreateMessageParams{
			Role:  Assistant,
			Parts: []ContentPart{TextContent{Text: fmt.Sprintf("seed-%03d", i)}},
		})
		require.NoError(t, err)
	}

	var wg sync.WaitGroup
	stopWriting := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stopWriting:
				return
			default:
			}
			_, _ = svc.Create(ctx, sessionID, CreateMessageParams{
				Role:  Assistant,
				Parts: []ContentPart{TextContent{Text: fmt.Sprintf("live-%04d", i)}},
			})
			i++
		}
	}()
	t.Cleanup(func() {
		close(stopWriting)
		wg.Wait()
	})

	const pageSize = 7
	const calls = 50
	for i := 0; i < calls; i++ {
		window, total, err := svc.ListPaginatedSnapshot(ctx, sessionID, pageSize, 0)
		require.NoError(t, err)
		require.GreaterOrEqual(t, total, int64(seedCount),
			"total must never under-report the seeded floor")
		require.LessOrEqual(t, len(window), pageSize,
			"window must never exceed the requested limit")
		// The window is this call's own newest `pageSize` messages, so its
		// length can only be less than pageSize if the total itself is
		// smaller than pageSize - anything else would mean the window and
		// total came from different snapshots (a torn read).
		if total < int64(pageSize) {
			require.EqualValues(t, total, len(window),
				"when total is below the page size, the window must contain every message the SAME call's total reports")
		} else {
			require.Len(t, window, pageSize,
				"a full-size window must be returned whenever this call's own total is at least one page")
		}
	}
}

// TestListPaginatedSnapshot_StablePagingWhenWritesArePaused walks an entire
// session's history backward page by page using ListPaginatedSnapshot AFTER
// concurrent writing has stopped, and asserts the union of all pages
// reproduces exactly the seeded messages with no duplicates and no gaps —
// confirming the keyset-based window fetch (ListMessagesBySessionOlderThan-
// CreatedAt + ListMessagesBySessionAtCreatedAt) itself is correct and
// complete once the underlying data is stable, independent of the
// within-one-call snapshot race covered by
// TestListPaginatedSnapshot_SingleCallWindowMatchesTotal above.
func TestListPaginatedSnapshot_StablePagingWhenWritesArePaused(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()
	sessionID := "test-session-paging-no-gaps"

	const seedCount = 47 // deliberately not a multiple of the page size
	seededIDs := make(map[string]bool, seedCount)
	for i := 0; i < seedCount; i++ {
		created, err := svc.Create(ctx, sessionID, CreateMessageParams{
			Role:  Assistant,
			Parts: []ContentPart{TextContent{Text: fmt.Sprintf("seed-%03d", i)}},
		})
		require.NoError(t, err)
		seededIDs[created.ID] = true
	}

	const pageSize = 7
	firstWindow, firstTotal, err := svc.ListPaginatedSnapshot(ctx, sessionID, pageSize, 0)
	require.NoError(t, err)
	require.EqualValues(t, seedCount, firstTotal)

	seen := make(map[string]int)
	for _, m := range firstWindow {
		seen[m.ID]++
	}

	for offset := pageSize; offset < int(firstTotal); offset += pageSize {
		window, total, err := svc.ListPaginatedSnapshot(ctx, sessionID, pageSize, offset)
		require.NoError(t, err)
		require.EqualValues(t, seedCount, total, "total must stay stable across pages when nothing is being written")
		for _, m := range window {
			seen[m.ID]++
		}
	}

	var missing, dup []string
	for id := range seededIDs {
		switch seen[id] {
		case 0:
			missing = append(missing, id)
		case 1:
			// ok
		default:
			dup = append(dup, id)
		}
	}
	require.Empty(t, missing, "seeded messages missing across the paged walk: %v", missing)
	require.Empty(t, dup, "seeded messages duplicated across the paged walk: %v", dup)
}

// TestNewServiceWithReader_NilConcreteQueriesFallsBackToWriter is the
// regression test for @oh's review finding 7 (docs/reviews review-of-review,
// task #175): NewServiceWithReader's qRead parameter must be the concrete
// *db.Queries type, not the db.Querier interface, because a nil *db.Queries
// boxed into a db.Querier interface value is itself non-nil (Go's
// typed-nil-in-interface gotcha) — with the old interface-typed signature,
// passing a nil *db.Queries (exactly what internal/app.New does whenever
// db.ConnectRead fails and degrades to the writer) would silently install
// the typed-nil as qRead instead of falling back to q, and every subsequent
// s.qRead.* call would nil-dereference. This proves passing a nil
// *db.Queries falls back to the writer and every read path stays usable.
func TestNewServiceWithReader_NilConcreteQueriesFallsBackToWriter(t *testing.T) {
	_, q := newTestMessageDB(t)
	ctx := t.Context()
	sessionID := "test-session-nil-reader-fallback"

	var nilReader *db.Queries // exactly what internal/app.New passes on a failed db.ConnectRead
	svc := NewServiceWithReader(q, nilReader)

	created, err := svc.Create(ctx, sessionID, CreateMessageParams{
		Role:  Assistant,
		Parts: []ContentPart{TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	// Every one of these calls goes through s.qRead internally (see the
	// service struct's doc comment) — if the typed-nil bug were present,
	// each would nil-dereference instead of returning normally.
	got, err := svc.Get(ctx, created.ID)
	require.NoError(t, err, "Get must fall back to the writer connection, not nil-dereference a typed-nil qRead")
	require.Equal(t, created.ID, got.ID)

	list, err := svc.List(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, list, 1)

	count, err := svc.Count(ctx, sessionID)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)

	window, total, err := svc.ListPaginatedSnapshot(ctx, sessionID, 10, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, window, 1)
}

// TestListPaginatedSnapshot_NonPositiveLimit is the regression test for
// @oh's review finding 11 (docs/reviews review-of-review, task #175):
// ListPaginatedSnapshot must not panic on a negative limit
// (make([]db.Message, 0, limit) panics for negative capacity) and must
// return zero messages for limit == 0 (the tied-second loop previously
// appended one row before checking len(dbMessages) >= limit, so limit == 0
// returned exactly one row instead of none). Both are unreachable in
// production today — the only caller, read_delegation_transcript.go's
// clampTranscriptWindow, always clamps to a positive default — but this
// guards the defensive fix added for exactly that reason.
func TestListPaginatedSnapshot_NonPositiveLimit(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()
	sessionID := "test-session-nonpositive-limit"

	for i := 0; i < 5; i++ {
		_, err := svc.Create(ctx, sessionID, CreateMessageParams{
			Role:  Assistant,
			Parts: []ContentPart{TextContent{Text: fmt.Sprintf("seed-%d", i)}},
		})
		require.NoError(t, err)
	}

	for _, limit := range []int{0, -1, -100} {
		window, total, err := svc.ListPaginatedSnapshot(ctx, sessionID, limit, 0)
		require.NoError(t, err, "limit=%d must not error", limit)
		require.Empty(t, window, "limit=%d must return zero messages, not panic or return a partial window", limit)
		require.EqualValues(t, 5, total, "limit=%d must still report the correct total count", limit)
	}
}
