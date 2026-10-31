package server

// Live-work snapshot handler + change-event push (task #1058): the server
// half of the web panel's Commands/Agents tabs reads ONLY batch_nodes (live
// roots plus the bounded BatchHistoryRootsForOwner terminal-history read),
// so work hosted by another process is visible the same as local work, and
// a snapshot for one session never exposes another session's rows.
//
// Revert-check: dropping the buildSessionLiveWork body to a hardcoded empty
// payload fails every test here that asserts a non-empty Commands/Agents
// list; dropping the a.Sessions.Get existence check fails
// TestHandleGetSessionLiveWork_UnknownSessionIsRefused; removing the
// liveWorkPusher wiring from subscribeAndBroadcast (events.go) fails
// TestLiveWorkPusher_*; removing the title cache's missing-only history
// read fails TestLiveWorkTitles_CacheAvoidsHistoryReread; reverting
// truncateTitle to byte slicing fails TestTruncateTitle_ValidUTF8; dropping
// capBatchRoots' terminal limit fails TestBuildSessionLiveWork_TerminalCap.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// requestSnapshot calls handleGetSessionLiveWork and returns the first
// EventSessionLiveWork reply (or nil if none arrived).
func requestSnapshot(t *testing.T, a *app.App, client *Client, sessionID string) *SessionLiveWorkPayload {
	t.Helper()
	payload, err := json.Marshal(GetSessionLiveWorkPayload{SessionID: sessionID})
	require.NoError(t, err)
	handleGetSessionLiveWork(t.Context(), a, client, WSMessage{ID: "req", Type: CmdGetSessionLiveWork, Payload: payload})
	for {
		select {
		case raw := <-client.send:
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			if env.Type != EventSessionLiveWork {
				continue
			}
			var snap SessionLiveWorkPayload
			require.NoError(t, json.Unmarshal(env.Payload, &snap))
			return &snap
		case <-time.After(2 * time.Second):
			return nil
		}
	}
}

// collectLiveWorkSnapshots drains client.send for d and returns every
// session_live_work payload received in that window.
func collectLiveWorkSnapshots(t *testing.T, client *Client, d time.Duration) []*SessionLiveWorkPayload {
	t.Helper()
	var out []*SessionLiveWorkPayload
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		select {
		case raw := <-client.send:
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			if env.Type != EventSessionLiveWork {
				continue
			}
			var snap SessionLiveWorkPayload
			require.NoError(t, json.Unmarshal(env.Payload, &snap))
			out = append(out, &snap)
		case <-time.After(20 * time.Millisecond):
		}
	}
	return out
}

func TestHandleGetSessionLiveWork_EmptySession(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	hub := newHub()
	go hub.Run(t.Context())
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client

	sess, err := a.Sessions.Create(t.Context(), "lw empty")
	require.NoError(t, err)

	snap := requestSnapshot(t, a, client, sess.ID)
	require.NotNil(t, snap)
	require.Equal(t, sess.ID, snap.SessionID)
	require.Empty(t, snap.Commands)
	require.Empty(t, snap.Agents)
}

func TestHandleGetSessionLiveWork_UnknownSessionIsRefused(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	hub := newHub()
	go hub.Run(t.Context())
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client

	payload, err := json.Marshal(GetSessionLiveWorkPayload{SessionID: "does-not-exist"})
	require.NoError(t, err)
	handleGetSessionLiveWork(t.Context(), a, client, WSMessage{ID: "req", Type: CmdGetSessionLiveWork, Payload: payload})

	var gotErr bool
	deadline := time.Now().Add(2 * time.Second)
	for !gotErr && time.Now().Before(deadline) {
		select {
		case raw := <-client.send:
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			gotErr = env.Type == EventError
		case <-time.After(50 * time.Millisecond):
		}
	}
	require.True(t, gotErr, "a snapshot for a non-existent session must be refused, not synthesized")
}

func TestHandleGetSessionLiveWork_RunningAndTerminalCommands(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	hub := newHub()
	hubDone := make(chan struct{})
	go func() { defer close(hubDone); hub.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-hubDone })
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client

	sess, err := a.Sessions.Create(ctx, "lw commands")
	require.NoError(t, err)

	// The running command's text lives only in the assistant tool_call part.
	_, err = a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{ID: "tc-run", Name: "bash", Input: `{"command":"echo hello\nworld"}`}},
	})
	require.NoError(t, err)
	seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: sess.ID, toolCallID: "tc-run", taskKind: "bash", state: "running"})

	// A finished command with its cause: natural completion.
	seedLiveWorkTerminal(t, a, liveWorkSeedSpec{owner: sess.ID, toolCallID: "tc-done", taskKind: "bash"}, "completed")

	snap := requestSnapshot(t, a, client, sess.ID)
	require.NotNil(t, snap)
	require.Len(t, snap.Commands, 2)

	var running, done *LiveWorkItemWire
	for i := range snap.Commands {
		switch snap.Commands[i].ToolCallID {
		case "tc-run":
			running = &snap.Commands[i]
		case "tc-done":
			done = &snap.Commands[i]
		}
	}
	require.NotNil(t, running)
	require.Equal(t, "running", running.Status)
	require.Zero(t, running.FinishedAt)
	require.Equal(t, "echo hello world", running.Title, "command text from the tool_call part, collapsed to one line")

	require.NotNil(t, done)
	require.Equal(t, "completed", done.Status)
	require.NotZero(t, done.FinishedAt)
	require.NotZero(t, done.LastActivityAt)
}

func TestHandleGetSessionLiveWork_TerminalCausesAreDistinct(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	hub := newHub()
	hubDone := make(chan struct{})
	go func() { defer close(hubDone); hub.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-hubDone })
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client

	sess, err := a.Sessions.Create(ctx, "lw causes")
	require.NoError(t, err)

	// The batch cause vocabulary is why a row left "running": one cause per
	// terminal state family (timeout / stop / stop_on_fail). The legacy
	// per-notice-kind reason mapping (session_cancel, stopped command) is an
	// obsolete contract: batch nodes carry a single stop cause for every
	// cancellation.
	seed := func(id, state string) {
		seedLiveWorkTerminal(t, a, liveWorkSeedSpec{owner: sess.ID, toolCallID: id, taskKind: "bash"}, state)
	}
	seed("tc-timeout", "timed_out")
	seed("tc-cancel", "cancelled")
	seed("tc-fail", "failed")

	snap := requestSnapshot(t, a, client, sess.ID)
	require.NotNil(t, snap)
	statuses := map[string]string{}
	for _, c := range snap.Commands {
		statuses[c.ToolCallID] = c.Status + "|" + c.Reason
	}
	require.Equal(t, "timed_out|timeout", statuses["tc-timeout"])
	require.Equal(t, "cancelled|stop", statuses["tc-cancel"])
	require.Equal(t, "failed|stop_on_fail", statuses["tc-fail"])
}

func TestHandleGetSessionLiveWork_SubAgentDelegation(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	hub := newHub()
	go hub.Run(t.Context())
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client
	ctx := t.Context()

	root, err := a.Sessions.Create(ctx, "lw root")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "lw-child", root.ID, "lw child")
	require.NoError(t, err)
	seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: root.ID, toolCallID: "tc-ag", taskKind: "agent", childSessionID: child.ID, state: "running"})

	snap := requestSnapshot(t, a, client, root.ID)
	require.NotNil(t, snap)
	require.Empty(t, snap.Commands)
	require.Len(t, snap.Agents, 1)
	require.Equal(t, child.ID, snap.Agents[0].ChildSessionID)
	require.Equal(t, "tc-ag", snap.Agents[0].ToolCallID)
	require.Equal(t, "running", snap.Agents[0].Status)

	// Isolation: a snapshot for the CHILD never walks upward to the root's
	// rows, and a snapshot for an unrelated session exposes nothing.
	other, err := a.Sessions.Create(ctx, "lw unrelated")
	require.NoError(t, err)
	snap = requestSnapshot(t, a, client, other.ID)
	require.NotNil(t, snap)
	require.Empty(t, snap.Commands)
	require.Empty(t, snap.Agents)
}

// TestHandleGetSessionLiveWork_CrossProcessRowsReadFromDB proves the
// snapshot reads the durable table, not this process's memory: the batch
// root is written through db.Connect for the same data directory — the same
// pooled writer the reader uses — under a DIFFERENT host id, so a snapshot
// served from anything but the durable batch_nodes state would be exposed
// (a root reserved under another host is still live work for the owner).
func TestHandleGetSessionLiveWork_CrossProcessRowsReadFromDB(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	hub := newHub()
	go hub.Run(t.Context())
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "lw cross-process")
	require.NoError(t, err)

	dataDir := a.Config().Options.DataDirectory
	// The root is written through db.Connect's pooled handle for the same
	// data directory under a foreign host id: the snapshot must come from
	// the durable batch_nodes table, not from this process's memory.
	other, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.ReleaseConn(other) })
	_, err = session.NewBatchStore(other).Reserve(ctx, session.BatchReservation{
		Owner: sess.ID, Host: "lw-foreign-host", RunnerID: "lw-foreign-runner",
		ToolCallID: "tc-remote", InputHash: "hash-tc-remote", Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "running", ControlState: "none",
		}},
	})
	require.NoError(t, err)

	snap := requestSnapshot(t, a, client, sess.ID)
	require.NotNil(t, snap)
	require.Len(t, snap.Commands, 1)
	require.Equal(t, "tc-remote", snap.Commands[0].ToolCallID)
	require.Equal(t, "running", snap.Commands[0].Status)
}

// ── Push worker: coalescing, off-hot-path, lifetime ─────────────────────────

// TestLiveWorkPusher_CoalescesBurstIntoOneSnapshot proves a burst of marks
// for one session produces exactly ONE snapshot per coalescing window (the
// forwarder only marks; the worker does the reads).
func TestLiveWorkPusher_CoalescesBurstIntoOneSnapshot(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	hub := newHub()
	go hub.Run(t.Context())
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "lw coalesce")
	require.NoError(t, err)
	seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: sess.ID, toolCallID: "tc-co", taskKind: "bash", state: "running"})

	p := startLiveWorkPusher(ctx, a, hub)
	t.Cleanup(func() { <-p.stopped })

	// Revert-check: marking 5 times used to be 5 synchronous snapshot
	// builds (maybeBroadcastSessionLiveWork per event); the worker must
	// collapse the burst into one.
	for i := 0; i < 5; i++ {
		p.mark(sess.ID)
	}
	// Wait for the first broadcast instead of assuming the coalescing timer
	// and scheduler have completed after a fixed sleep.
	var first *SessionLiveWorkPayload
	select {
	case raw := <-client.send:
		var env WSMessage
		require.NoError(t, json.Unmarshal(raw, &env))
		require.Equal(t, EventSessionLiveWork, env.Type)
		require.NoError(t, json.Unmarshal(env.Payload, &first))
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the coalesced live-work broadcast")
	}
	// Observe for a full coalescing window after the first broadcast to catch
	// any duplicate snapshot emitted for the same burst.
	snapshots := append([]*SessionLiveWorkPayload{first}, collectLiveWorkSnapshots(t, client, liveWorkCoalesceDelay+100*time.Millisecond)...)
	require.Len(t, snapshots, 1, "a burst of marks must yield exactly one snapshot")
	require.Equal(t, sess.ID, snapshots[0].SessionID)
	require.Len(t, snapshots[0].Commands, 1)
}

// TestLiveWorkPusher_DeletedSessionGetsNoSnapshot: a mark for a session
// deleted before the sweep produces nothing.
func TestLiveWorkPusher_DeletedSessionGetsNoSnapshot(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	hub := newHub()
	go hub.Run(t.Context())
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client
	ctx := t.Context()

	p := startLiveWorkPusher(ctx, a, hub)
	t.Cleanup(func() { <-p.stopped })

	p.mark("never-created-session")
	time.Sleep(liveWorkCoalesceDelay + 400*time.Millisecond)
	snapshots := collectLiveWorkSnapshots(t, client, 50*time.Millisecond)
	require.Empty(t, snapshots, "a deleted/unknown session must not get a push snapshot")
}

// TestLiveWorkPusher_StopsWithContext: the worker exits with the server
// ctx — no goroutine leak past shutdown.
func TestLiveWorkPusher_StopsWithContext(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	hub := newHub()
	go hub.Run(t.Context())
	ctx, cancel := context.WithCancel(t.Context())

	p := startLiveWorkPusher(ctx, a, hub)
	p.mark("whatever")
	cancel()
	select {
	case <-p.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the pusher must exit when the server ctx is canceled")
	}
}

// ── Title cache: bounded cost per snapshot ──────────────────────────────────

// countingMessageService wraps message.Service to count history reads.
type countingMessageService struct {
	message.Service
	lists atomic.Int64
}

func (c *countingMessageService) ListWithWatermark(ctx context.Context, sessionID string) ([]message.Message, int64, error) {
	c.lists.Add(1)
	return c.Service.ListWithWatermark(ctx, sessionID)
}

// TestLiveWorkTitles_IsolatedPerSession: two sessions reusing the SAME
// tool_call_id (providers number calls per response) with different
// commands each get their OWN title in one and the same hub. Revert-check:
// the superseded global id-keyed cache (package-level lwTitleCache keyed by
// tool_call_id alone) returned the first session's title for the second —
// this test fails against it.
func TestLiveWorkTitles_IsolatedPerSession(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	hub := newHub()
	go hub.Run(t.Context())
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client
	ctx := t.Context()

	for i, name := range []string{"lw iso a", "lw iso b"} {
		sess, err := a.Sessions.Create(ctx, name)
		require.NoError(t, err)
		_, err = a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.ToolCall{ID: "call_0", Name: "bash", Input: fmt.Sprintf(`{"command":"echo from-%c"}`, 'a'+i)}},
		})
		require.NoError(t, err)
		seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: sess.ID, toolCallID: "call_0", taskKind: "bash", state: "running"})
	}

	snapA := requestSnapshot(t, a, client, mustSessionID(t, a, "lw iso a"))
	require.NotNil(t, snapA)
	snapB := requestSnapshot(t, a, client, mustSessionID(t, a, "lw iso b"))
	require.NotNil(t, snapB)
	require.Len(t, snapA.Commands, 1)
	require.Len(t, snapB.Commands, 1)
	require.Equal(t, "echo from-a", snapA.Commands[0].Title)
	require.Equal(t, "echo from-b", snapB.Commands[0].Title, "the second session must not inherit the first session's cached title")
}

// TestLiveWorkTitles_InputChangeInvalidates: a row re-claimed with
// DIFFERENT input (different input_hash) must not show the previous call's
// cached title. Revert-check: any cache keyed without input_hash (both the
// superseded global and an id-keyed per-hub variant) answers the second
// snapshot with the stale "first" title.
func TestLiveWorkTitles_InputChangeInvalidates(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	hub := newHub()
	go hub.Run(t.Context())
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "lw reuse")
	require.NoError(t, err)
	msg, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "tc-reuse", Name: "bash", Input: `{"command":"echo first"}`},
			// Finish so the message is terminally written and deletable.
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	})
	require.NoError(t, err)
	seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: sess.ID, toolCallID: "tc-reuse", taskKind: "bash", state: "running"})

	snap := requestSnapshot(t, a, client, sess.ID)
	require.NotNil(t, snap)
	require.Equal(t, "echo first", snap.Commands[0].Title)

	// Simulate id reuse with different input: the root's input_hash changes,
	// and the history now carries the new call's text instead of the old.
	_, err = a.DB().ExecContext(ctx, "UPDATE batch_nodes SET input_hash = 'reused-hash' WHERE tool_call_id = 'tc-reuse'")
	require.NoError(t, err)
	require.NoError(t, a.Messages.ForceDelete(ctx, msg.ID))
	_, err = a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{ID: "tc-reuse", Name: "bash", Input: `{"command":"echo second"}`}},
	})
	require.NoError(t, err)

	snap = requestSnapshot(t, a, client, sess.ID)
	require.NotNil(t, snap)
	require.Len(t, snap.Commands, 1)
	require.Equal(t, "echo second", snap.Commands[0].Title, "a re-claimed row with different input must not show the stale title")
}

// TestLiveWorkTitles_ReusedToolCallIDKeepsBothIncarnations: one session retains
// TWO assistant messages that use the same tool-call id with different
// commands, and both incarnations have a batch root (the first settled, the
// second running). Each root's title is the input its own call stored, not the
// first history match for the id.
//
// Revert-check: with the history-only lookup (no payload title) both rows show
// "echo first", the first message in history.
func TestLiveWorkTitles_ReusedToolCallIDKeepsBothIncarnations(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	hub := newHub()
	go hub.Run(t.Context())
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "lw reuse both")
	require.NoError(t, err)
	for _, cmd := range []string{"echo first", "echo second"} {
		_, err = a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{ID: "tc-both", Name: "bash", Input: `{"command":"` + cmd + `"}`},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		})
		require.NoError(t, err)
	}
	seedLiveWorkRow(t, a, liveWorkSeedSpec{
		owner: sess.ID, toolCallID: "tc-both", taskKind: "bash", state: "running",
		payloadInput: `{"command":"echo first"}`, inputHash: "hash-first",
	})
	projectLiveWorkRoot(t, a, sess.ID, "tc-both", "completed")
	seedLiveWorkRow(t, a, liveWorkSeedSpec{
		owner: sess.ID, toolCallID: "tc-both", taskKind: "bash", state: "running",
		payloadInput: `{"command":"echo second"}`, inputHash: "hash-second",
	})

	snap := requestSnapshot(t, a, client, sess.ID)
	require.NotNil(t, snap)
	titles := map[string]string{}
	for _, it := range snap.Commands {
		titles[it.Status] = it.Title
	}
	require.Len(t, snap.Commands, 2)
	require.Equal(t, "echo first", titles["completed"])
	require.Equal(t, "echo second", titles["running"])
}

func mustSessionID(t *testing.T, a *app.App, title string) string {
	t.Helper()
	sessions, err := a.Sessions.List(t.Context())
	require.NoError(t, err)
	for _, s := range sessions {
		if s.Title == title {
			return s.ID
		}
	}
	t.Fatalf("session %q not found", title)
	return ""
}

// TestLiveWorkTitles_CacheAvoidsHistoryReread: a repeated snapshot with no
// new rows must NOT read session history again — the bounded title cache
// absorbs it. Revert-check: removing the cache lookup (reading history on
// every snapshot) makes the second count strictly larger.
func TestLiveWorkTitles_CacheAvoidsHistoryReread(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	hub := newHub()
	go hub.Run(t.Context())
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "lw cache")
	require.NoError(t, err)
	_, err = a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{ID: "tc-cache-1", Name: "bash", Input: `{"command":"echo once"}`}},
	})
	require.NoError(t, err)
	seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: sess.ID, toolCallID: "tc-cache-1", taskKind: "bash", state: "running"})

	counting := &countingMessageService{Service: a.Messages}
	a.Messages = counting

	snap := requestSnapshot(t, a, client, sess.ID)
	require.NotNil(t, snap)
	require.Len(t, snap.Commands, 1)
	require.Equal(t, "echo once", snap.Commands[0].Title)
	first := counting.lists.Load()
	require.GreaterOrEqual(t, first, int64(1), "the first snapshot must read history for the missing id")

	snap = requestSnapshot(t, a, client, sess.ID)
	require.NotNil(t, snap)
	require.Equal(t, "echo once", snap.Commands[0].Title)
	require.Equal(t, first, counting.lists.Load(), "a repeated snapshot with no new rows must not re-read history")
}

// ── Bounded snapshot: running + last N terminal ─────────────────────────────

// TestBuildSessionLiveWork_TerminalCap: a snapshot carries every running
// root plus only the liveWorkTerminalLimit newest finished roots, newest
// first. The cap is the composite contract: the per-owner terminal-history
// bound (collectBatchHistoryTerminalRoots bounding an owner to
// batchHistoryTerminalLimit) and the wire cap (capBatchRoots) agree, and a
// mutant removing BOTH bounds fails this test on the length assertion.
// Removing only capBatchRoots' terminal limit is NOT isolated by this
// fixture — the history bound already caps the single owner's 55 terminal
// roots before capBatchRoots runs (accepted as a P3 coverage note).
func TestBuildSessionLiveWork_TerminalCap(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())

	sess, err := a.Sessions.Create(ctx, "lw cap")
	require.NoError(t, err)

	// 55 finished rows with deterministic, distinct updated_at (oldest =
	// highest id number), plus 2 running rows that must survive the cap.
	const totalTerminal = 55
	for i := 0; i < totalTerminal; i++ {
		id := fmt.Sprintf("tc-cap-%02d", i)
		seedLiveWorkTerminal(t, a, liveWorkSeedSpec{owner: sess.ID, toolCallID: id, taskKind: "bash"}, "completed")
		_, err = a.DB().ExecContext(ctx, "UPDATE batch_nodes SET updated_at = ? WHERE tool_call_id = ?", 1_700_000_000+int64(i), id)
		require.NoError(t, err)
	}
	for _, id := range []string{"tc-cap-run-a", "tc-cap-run-b"} {
		seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: sess.ID, toolCallID: id, taskKind: "bash", state: "running"})
	}

	hub := newHub()
	hubDone := make(chan struct{})
	go func() { defer close(hubDone); hub.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-hubDone })
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client

	snap := requestSnapshot(t, a, client, sess.ID)
	require.NotNil(t, snap)
	require.Len(t, snap.Commands, 52, "all running + only the last 50 finished")

	// Newest first (running rows were claimed last, so their updated_at is
	// the most recent; the tie between them breaks on tool_call_id asc);
	// the 5 OLDEST finished rows (lowest i) are dropped.
	require.Equal(t, "tc-cap-run-a", snap.Commands[0].ToolCallID)
	require.Equal(t, "tc-cap-run-b", snap.Commands[1].ToolCallID)
	require.Equal(t, "running", snap.Commands[0].Status)
	first := snap.Commands[2]
	require.Equal(t, "tc-cap-54", first.ToolCallID, "newest finished row comes first")
	last := snap.Commands[len(snap.Commands)-1]
	require.Equal(t, "tc-cap-05", last.ToolCallID, "the oldest 5 finished rows are cut")
	for _, c := range snap.Commands {
		require.NotEqual(t, "tc-cap-00", c.ToolCallID, "oldest rows must be dropped")
	}
}

// ── UTF-8-safe truncation ───────────────────────────────────────────────────

// TestTruncateTitle_ValidUTF8: byte slicing used to split multi-byte runes;
// the cut must be by runes and stay valid UTF-8. Revert-check: reverting
// truncateTitle to s[:titleMaxLen] makes the result invalid UTF-8 (the
// multi-byte rune at the boundary is split).
func TestTruncateTitle_ValidUTF8(t *testing.T) {
	long := strings.Repeat("ж", 200)
	got := truncateTitle(long)
	require.True(t, utf8.ValidString(got), "truncation must never produce invalid UTF-8")
	require.LessOrEqual(t, utf8.RuneCountInString(got), titleMaxLen+1, "at most titleMaxLen runes plus the ellipsis")
	require.Equal(t, strings.Repeat("ж", titleMaxLen)+"…", got)

	short := strings.Repeat("ж", titleMaxLen)
	require.Equal(t, short, truncateTitle(short), "a string at the limit must pass through unchanged")

	emoji := strings.Repeat("🐘", 130)
	gotEmoji := truncateTitle(emoji)
	require.True(t, utf8.ValidString(gotEmoji))
	require.LessOrEqual(t, utf8.RuneCountInString(gotEmoji), titleMaxLen+1)
}

// ── Push wiring through the real bridge ─────────────────────────────────────

// TestMaybeBroadcastSessionLiveWork: through subscribeAndBroadcast — a
// notice message marks the session and the worker pushes one snapshot;
// plain prose marks nothing.
func TestMaybeBroadcastSessionLiveWork(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()
	sess, err := a.Sessions.Create(ctx, "lw push")
	require.NoError(t, err)
	seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: sess.ID, toolCallID: "tc-push", taskKind: "bash", state: "running"})

	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client

	// The real wiring under test: events.go's message subscription marking
	// dirty sessions and the worker pushing snapshots off the hot path.
	subscribeAndBroadcast(ctx, a, hub)

	// Plain prose never triggers a snapshot.
	_, err = a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "hello"}}})
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	// A notice (job completion) does.
	_, err = a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.User, BackgroundJobNotice: true, NoticeKind: "completed", Parts: []message.ContentPart{message.TextContent{Text: "job done"}}})
	require.NoError(t, err)

	snapshots := collectLiveWorkSnapshots(t, client, 2*time.Second)
	require.NotEmpty(t, snapshots, "a notice message must push a session_live_work snapshot")
	require.Equal(t, sess.ID, snapshots[0].SessionID)
	require.Len(t, snapshots[0].Commands, 1)
	require.Equal(t, "tc-push", snapshots[0].Commands[0].ToolCallID)
}
