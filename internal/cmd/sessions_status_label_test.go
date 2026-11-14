package cmd

// Tests for the one STATUS vocabulary shared by `sessions list`, `sessions
// why` and `sessions status` (FIX-STATUS (a)/(c)): a recorded ended_reason
// must render the same way on every surface -- peak_hours as a pause,
// unknown reasons verbatim, and a live status must not hide a recorded end.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestEndedStatusLabel_PeakHoursRendersPaused: the peak-hours pause is a
// pause, never a completion.
//
// Revert-check: mapping peak_hours to "done" (or leaving it unlabeled)
// fails the equality.
func TestEndedStatusLabel_PeakHoursRendersPaused(t *testing.T) {
	t.Parallel()
	require.Equal(t, "paused: provider peak hours", endedStatusLabel("peak_hours"))
}

// TestEndedStatusLabel_UnknownReasonVerbatim: a reason this binary does not
// know renders word-for-word, never as "done".
//
// Revert-check: an unknown reason collapsing to "done" fails.
func TestEndedStatusLabel_UnknownReasonVerbatim(t *testing.T) {
	t.Parallel()
	require.Equal(t, "provider_maintenance", endedStatusLabel("provider_maintenance"))
	require.Equal(t, "someone_else_reason", endedStatusLabel("someone_else_reason"))
}

// TestEndedStatusLabel_KnownReasonsDone: the known exit_reason vocabulary
// keeps the historical whole-column "done" rendering -- no list regression.
//
// Revert-check: dropping a known reason from the set (it starts rendering
// verbatim) or rendering known reasons with a label fails.
func TestEndedStatusLabel_KnownReasonsDone(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{
		"awaiting_answer", "batch_fault", "canceled", "done", "end_turn",
		"error", "max_cost", "max_tokens", "queued", "stalled", "stop",
		"timeout", "tool_use", "unknown",
	} {
		require.Equal(t, "done", endedStatusLabel(reason), reason)
	}
}

// TestSessionStatusLabel_LiveWorkAnnotatesRecordedEnd: a live session whose
// row still records an ended_reason shows that exit in its status -- the
// exact "list said running while status reported exit_reason timeout"
// mismatch this lane fixes.
//
// Revert-check: dropping the "(exit ...)" annotation (hiding the recorded
// end again) fails the first assertion; annotating without a recorded end
// fails the second.
func TestSessionStatusLabel_LiveWorkAnnotatesRecordedEnd(t *testing.T) {
	t.Parallel()
	v := session.ActivityVerdict{Kind: session.ActivityBetweenTurns}
	require.Equal(t, "running (exit timeout)", sessionStatusLabel(v, "timeout"))
	require.Equal(t, "running", sessionStatusLabel(v, ""))
	require.Equal(t, "delegating (exit peak_hours)", sessionStatusLabel(session.ActivityVerdict{Kind: session.ActivityDelegating}, "peak_hours"))
}

// TestSessionStatusLabel_EndedRendersThroughTheSharedLabel: the Ended
// verdict goes through the same ended-reason label as every other surface.
//
// Revert-check: a private Ended-to-"done" shortcut in a caller fails here
// or in the peak-hours assertion.
func TestSessionStatusLabel_EndedRendersThroughTheSharedLabel(t *testing.T) {
	t.Parallel()
	require.Equal(t, "done", sessionStatusLabel(session.ActivityVerdict{Kind: session.ActivityEnded, EndedReason: "end_turn"}, ""))
	require.Equal(t, "paused: provider peak hours", sessionStatusLabel(session.ActivityVerdict{Kind: session.ActivityEnded, EndedReason: "peak_hours"}, "peak_hours"))
	require.Equal(t, "", sessionStatusLabel(session.ActivityVerdict{Kind: session.ActivityIdle}, ""))
}

// TestSessionsStatusPhase_PeakHoursIsPaused: only the peak-hours pause
// changes the phase; every other recorded reason stays "finished" and is
// reported verbatim through exit_reason.
//
// Revert-check: rendering the peak_hours phase as "finished" fails the
// first assertion; moving unknown reasons off "finished" fails the third.
func TestSessionsStatusPhase_PeakHoursIsPaused(t *testing.T) {
	t.Parallel()
	require.Equal(t, "paused", sessionsStatusPhase(session.ActivityVerdict{Kind: session.ActivityEnded, EndedReason: "peak_hours"}))
	require.Equal(t, "finished", sessionsStatusPhase(session.ActivityVerdict{Kind: session.ActivityEnded, EndedReason: "end_turn"}))
	require.Equal(t, "finished", sessionsStatusPhase(session.ActivityVerdict{Kind: session.ActivityEnded, EndedReason: "provider_maintenance"}))
	require.Equal(t, "idle", sessionsStatusPhase(session.ActivityVerdict{Kind: session.ActivityIdle}))
	require.Equal(t, "between_turns", sessionsStatusPhase(session.ActivityVerdict{Kind: session.ActivityBetweenTurns}))
}

// TestSessionsStatus_SnapshotRendersPeakHoursPaused: the snapshot reports
// the pause as the phase and labels the exit reason in the text render.
//
// Revert-check: a snapshot rendering phase "finished" for peak_hours, or a
// renderer losing the paused label, fails.
func TestSessionsStatus_SnapshotRendersPeakHoursPaused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dataDir := t.TempDir()
	a, s := sessionsStatusTestApp(t, dataDir)
	sess, err := s.Create(ctx, "status peak pause")
	require.NoError(t, err)
	require.NoError(t, s.SetEndedReason(ctx, sess.ID, "peak_hours"))

	rep, err := sessionsStatusSnapshot(ctx, a, dataDir, sess.ID, time.Now())
	require.NoError(t, err)
	require.Equal(t, "paused", rep.Phase)
	require.Equal(t, "peak_hours", rep.ExitReason)

	var buf bytes.Buffer
	renderSessionsStatusReport(&buf, rep)
	out := buf.String()
	require.Contains(t, out, "phase: paused")
	require.Contains(t, out, "exit reason: peak_hours (paused: provider peak hours)")
}

// TestSessionsStatus_UnknownEndedReasonVerbatim: an unknown recorded reason
// keeps the finished phase and renders verbatim -- never as done.
//
// Revert-check: an unknown reason rendered "(done)" or relabeled fails.
func TestSessionsStatus_UnknownEndedReasonVerbatim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dataDir := t.TempDir()
	a, s := sessionsStatusTestApp(t, dataDir)
	sess, err := s.Create(ctx, "status odd reason")
	require.NoError(t, err)
	require.NoError(t, s.SetEndedReason(ctx, sess.ID, "provider_maintenance"))

	rep, err := sessionsStatusSnapshot(ctx, a, dataDir, sess.ID, time.Now())
	require.NoError(t, err)
	require.Equal(t, "finished", rep.Phase)
	require.Equal(t, "provider_maintenance", rep.ExitReason)

	var buf bytes.Buffer
	renderSessionsStatusReport(&buf, rep)
	out := buf.String()
	require.Contains(t, out, "exit reason: provider_maintenance")
	require.NotContains(t, out, "(done)")
}

// TestExplainWhy_EndedPeakHoursRendersPaused: `sessions why` prints the
// paused label as its status line for a recorded peak_hours pause.
//
// Revert-check: why printing "status: done" (the pre-lane rendering) fails.
func TestExplainWhy_EndedPeakHoursRendersPaused(t *testing.T) {
	t.Parallel()
	conn, q := newSessionFactsTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)
	sess, err := s.Create(context.Background(), "peak pause")
	require.NoError(t, err)
	require.NoError(t, s.SetEndedReason(context.Background(), sess.ID, "peak_hours"))

	a := &app.App{Messages: m, Sessions: s, DB: func() *sql.DB { return conn }}
	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, t.TempDir(), sess.ID, &buf))
	out := buf.String()
	require.True(t, strings.HasPrefix(out, "status: paused: provider peak hours\n"),
		"why output must lead with the paused label, got %q", out)
}

// TestExplainWhy_AtRestWithFinishedTurnExplainsMissingEnd: "at rest" with a
// finished (end_turn) last assistant turn must say WHY no end is recorded
// (dropped exit write or a killed loop) instead of a bare no-end sentence.
//
// Revert-check: reverting to the bare "idle: no live work and no recorded
// end" without the explanation fails the second assertion.
func TestExplainWhy_AtRestWithFinishedTurnExplainsMissingEnd(t *testing.T) {
	t.Parallel()
	conn, q := newSessionFactsTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)
	sess, err := s.Create(context.Background(), "idle but finished")
	require.NoError(t, err)
	assistant, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "done"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, m.Update(context.Background(), assistant))

	a := &app.App{Messages: m, Sessions: s, DB: func() *sql.DB { return conn }}
	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, t.TempDir(), sess.ID, &buf))
	out := buf.String()
	require.Contains(t, out, "status: at rest")
	require.Contains(t, out, "no ended_reason is recorded",
		"the at-rest reason must explain the contradictory end_turn finish")
}

// TestSessionsListJSONStatusRendersEndedReasons: end to end through the real
// list command, a recorded peak_hours pause renders "paused: provider peak
// hours" and an unknown reason renders verbatim in the STATUS field.
//
// Revert-check: list rendering "done" for peak_hours or for an unknown
// reason fails.
func TestSessionsListJSONStatusRendersEndedReasons(t *testing.T) {
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()
	peak, err := a.Sessions.CreateWithID(ctx, "list-peak", "peak pause session")
	require.NoError(t, err)
	require.NoError(t, a.Sessions.SetEndedReason(ctx, peak.ID, "peak_hours"))
	odd, err := a.Sessions.CreateWithID(ctx, "list-odd-reason", "unknown reason session")
	require.NoError(t, err)
	require.NoError(t, a.Sessions.SetEndedReason(ctx, odd.ID, "provider_maintenance"))
	a.Shutdown()

	out := runSessionsCmd(t, sessionsListCmd, dataDir, map[string]bool{"json": true})
	type row struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		EndedReason string `json:"ended_reason"`
	}
	byID := map[string]row{}
	dec := json.NewDecoder(strings.NewReader(out))
	for dec.More() {
		var r row
		require.NoError(t, dec.Decode(&r))
		byID[r.ID] = r
	}
	require.Equal(t, "paused: provider peak hours", byID[peak.ID].Status)
	require.Equal(t, "peak_hours", byID[peak.ID].EndedReason)
	require.Equal(t, "provider_maintenance", byID[odd.ID].Status,
		"an unknown ended_reason renders verbatim, never as done")
}

// TestSessionsListJSONStatusAnnotatesRecordedEndUnderLiveWork: a live
// session (its lock names this live process) whose row records ended_reason
// "timeout" is listed as "running (exit timeout)" -- list must surface the
// same exit fact `sessions status` reports instead of a bare "running".
//
// Revert-check: dropping the annotation (the pre-lane bare "running") fails.
func TestSessionsListJSONStatusAnnotatesRecordedEndUnderLiveWork(t *testing.T) {
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "list-live-end", "live with recorded end")
	require.NoError(t, err)
	require.NoError(t, a.Sessions.SetEndedReason(ctx, sess.ID, "timeout"))
	lockDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock"),
		[]byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))
	a.Shutdown()

	out := runSessionsCmd(t, sessionsListCmd, dataDir, map[string]bool{"json": true})
	type row struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	found := false
	dec := json.NewDecoder(strings.NewReader(out))
	for dec.More() {
		var r row
		require.NoError(t, dec.Decode(&r))
		if r.ID == sess.ID {
			found = true
			require.Equal(t, "running (exit timeout)", r.Status)
		}
	}
	require.True(t, found, "the seeded session must be listed")
}
