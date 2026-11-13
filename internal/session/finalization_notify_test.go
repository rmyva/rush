package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// finalizationNotifyGateStub is a deterministic in-memory terminal gate for
// the notify-filter unit tests: no goroutines, no tickers, no polling.
type finalizationNotifyGateStub struct {
	mu        sync.Mutex
	active    map[string]bool
	awaitErr  error // Non-nil: AwaitSealed fails immediately with this error.
	failAllow bool  // Deterministic AllowTerminal=false (unreadable store).
}

func newFinalizationNotifyGateStub() *finalizationNotifyGateStub {
	return &finalizationNotifyGateStub{active: map[string]bool{}}
}

func (s *finalizationNotifyGateStub) activate(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[owner] = true
}

func (s *finalizationNotifyGateStub) AllowTerminal(_ context.Context, owner string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAllow {
		return false
	}
	return !s.active[owner]
}

func (s *finalizationNotifyGateStub) AwaitSealed(_ context.Context, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.awaitErr != nil {
		return s.awaitErr
	}
	if !s.active[owner] {
		return nil
	}
	// Unit tests never activate an owner and then AwaitSealed on this stub;
	// the real polling gate covers the blocking path in the DB tests.
	return errors.New("finalization notify stub cannot block on active owners")
}

// finalizationNotifyStubRead records the (owner, root) arguments and returns
// the configured result, so tests can assert passthrough fidelity.
type finalizationNotifyStubRead struct {
	mu        sync.Mutex
	owners    []string
	roots     []string
	envelope  BatchDeliveryEnvelope
	ok        bool
	err       error
	callsSeen int
}

func (r *finalizationNotifyStubRead) read(
	_ context.Context, owner, root string,
) (BatchDeliveryEnvelope, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owners = append(r.owners, owner)
	r.roots = append(r.roots, root)
	r.callsSeen++
	return r.envelope, r.ok, r.err
}

func (r *finalizationNotifyStubRead) called() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.callsSeen
}

func TestFinalizationTerminalFinishReasonTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		reason message.FinishReason
		want   bool
	}{
		{message.FinishReasonEndTurn, true},
		{message.FinishReasonMaxTokens, true},
		{message.FinishReasonCanceled, true},
		{message.FinishReasonError, true},
		{message.FinishReasonToolUse, false},
		{message.FinishReasonUnknown, false},
		{"weird", false},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, FinalizationTerminalFinishReason(tc.reason),
			"reason %q", tc.reason)
	}
}

func TestFinalizationTerminalMessageRecognizesOnlyTerminalAssistantFinish(t *testing.T) {
	t.Parallel()
	finish := func(reason message.FinishReason, partial bool) message.Finish {
		return message.Finish{Reason: reason, Partial: partial}
	}
	terminal := message.Message{
		ID: "m1", Role: message.Assistant, SessionID: "s",
		Parts: []message.ContentPart{finish(message.FinishReasonEndTurn, false)},
	}
	require.True(t, FinalizationTerminalMessage(terminal))
	midStep := terminal
	midStep.Parts = []message.ContentPart{finish(message.FinishReasonToolUse, false)}
	require.False(t, FinalizationTerminalMessage(midStep))
	unknown := terminal
	unknown.Parts = []message.ContentPart{finish(message.FinishReasonUnknown, false)}
	require.False(t, FinalizationTerminalMessage(unknown))
	checkpoint := terminal
	checkpoint.Parts = []message.ContentPart{finish(message.FinishReasonEndTurn, true)}
	require.False(t, FinalizationTerminalMessage(checkpoint))
	noFinish := message.Message{ID: "m2", Role: message.Assistant, SessionID: "s"}
	require.False(t, FinalizationTerminalMessage(noFinish))
	user := terminal
	user.Role = message.User
	require.False(t, FinalizationTerminalMessage(user))
}

func TestFinalizationSessionEndedDetectsEndedReason(t *testing.T) {
	t.Parallel()
	require.True(t, FinalizationSessionEnded(Session{EndedReason: "end_turn"}))
	require.False(t, FinalizationSessionEnded(Session{ID: "s"}))
}

func TestFinalizationGatedBatchResultWithoutGatePassesThrough(t *testing.T) {
	t.Parallel()
	stub := &finalizationNotifyStubRead{
		envelope: BatchDeliveryEnvelope{State: "failed", Code: "x"}, ok: true,
	}
	envelope, ok, err := FinalizationGatedBatchResult(
		t.Context(), nil, "owner", "root", stub.read)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, BatchDeliveryEnvelope{State: "failed", Code: "x"}, envelope)
	require.Equal(t, []string{"owner"}, stub.owners)
	require.Equal(t, []string{"root"}, stub.roots)
}

func TestFinalizationGatedBatchResultWithholdsWhileRecordActive(t *testing.T) {
	t.Parallel()
	gate := newFinalizationNotifyGateStub()
	gate.activate("owner")
	stub := &finalizationNotifyStubRead{
		ok:       true,
		envelope: BatchDeliveryEnvelope{State: "failed", Code: "exec_failed"},
	}
	envelope, ok, err := FinalizationGatedBatchResult(
		t.Context(), gate, "owner", "root", stub.read)
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, envelope)
	require.Equal(t, 0, stub.called(), "the read must not run while withheld")
}

func TestFinalizationGatedBatchResultDeliversAfterRelease(t *testing.T) {
	t.Parallel()
	gate := newFinalizationNotifyGateStub()
	want := BatchDeliveryEnvelope{State: "failed", Code: "exec_failed", Ref: "n#2"}
	stub := &finalizationNotifyStubRead{ok: true, envelope: want}
	envelope, ok, err := FinalizationGatedBatchResult(
		t.Context(), gate, "owner", "root", stub.read)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, envelope)
	require.Equal(t, 1, stub.called())
}

func TestFinalizationGatedBatchResultFailsClosedOnGateReadError(t *testing.T) {
	t.Parallel()
	// failAllow models the unreadable-store answer: AllowTerminal=false
	// (fail-closed), so the completion is withheld and the read never runs.
	gate := newFinalizationNotifyGateStub()
	gate.failAllow = true
	stub := &finalizationNotifyStubRead{
		ok:       true,
		envelope: BatchDeliveryEnvelope{State: "failed", Code: "exec_failed"},
	}
	envelope, ok, err := FinalizationGatedBatchResult(
		t.Context(), gate, "owner", "root", stub.read)
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, envelope)
	require.Equal(t, 0, stub.called())
}

func TestFinalizationGatedBatchResultCancelledContextFailsClosed(t *testing.T) {
	t.Parallel()
	gate := newFinalizationNotifyGateStub()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stub := &finalizationNotifyStubRead{
		ok:       true,
		envelope: BatchDeliveryEnvelope{State: "failed", Code: "x"},
	}
	envelope, ok, err := FinalizationGatedBatchResult(
		ctx, gate, "owner", "root", stub.read)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, ok)
	require.Empty(t, envelope)
	require.Equal(t, 0, stub.called())
}

func TestFinalizationGatedBatchResultNilReadIsError(t *testing.T) {
	t.Parallel()
	for _, gate := range []FinalizationTerminalGate{nil, newFinalizationNotifyGateStub()} {
		envelope, ok, err := FinalizationGatedBatchResult(
			t.Context(), gate, "owner", "root", nil)
		require.ErrorIs(t, err, ErrFinalizationBatchResultReadMissing)
		require.False(t, ok)
		require.Empty(t, envelope)
	}
}

func TestFinalizationAwaitGatedBatchResultReturnsGateErrorWithoutRead(t *testing.T) {
	t.Parallel()
	gate := newFinalizationNotifyGateStub()
	gate.activate("owner")
	gate.awaitErr = errors.New("stub store failure")
	stub := &finalizationNotifyStubRead{
		ok:       true,
		envelope: BatchDeliveryEnvelope{State: "failed", Code: "x"},
	}
	envelope, ok, err := FinalizationAwaitGatedBatchResult(
		t.Context(), gate, "owner", "root", stub.read)
	require.EqualError(t, err, "stub store failure")
	require.False(t, ok)
	require.Empty(t, envelope)
	require.Equal(t, 0, stub.called())
}

func TestFinalizationAwaitGatedBatchResultCancelledContextReturnsError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stub := &finalizationNotifyStubRead{
		ok:       true,
		envelope: BatchDeliveryEnvelope{State: "failed", Code: "x"},
	}
	envelope, ok, err := FinalizationAwaitGatedBatchResult(
		ctx, newFinalizationNotifyGateStub(), "owner", "root", stub.read)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, ok)
	require.Empty(t, envelope)
	require.Equal(t, 0, stub.called())
}

func TestFinalizationGatedBatchResultWithholdsRealActiveRecord(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	finalizationSeamBindOwner(t, env, "owner", "logical")
	bs, _, bctx := batchStoreTestDB(t)
	root := batchTerminalTestReserve(t, bs, bctx, "", 0)
	snap, err := bs.Read(bctx, "owner", root)
	require.NoError(t, err)
	child := snap.Nodes[1].NodeID
	batchTerminalTestProject(t, bs, bctx, root, "runner", 2,
		batchTerminalTestSettledRoot(root), db.ProjectBatchNodeParams{
			NodeID: child, State: "failed", OutcomeState: "failed",
			ResponseEnvelope: batchStoreTestString(
				`{"state":"failed","code":"exec_failed","summary":"boom"}`),
			ResponseEnvelopeRef: batchStoreTestString(child + "#2"),
		})
	// Precondition: without the gate the deliverable completion is returned.
	envelope, ok, err := bs.BatchDeliveryOneItemCompletion(bctx, "owner", root)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "failed", envelope.State)
	gate := NewPollingFinalizationTerminalGate(env.store, time.Millisecond)
	envelope, ok, err = FinalizationGatedBatchResult(
		bctx, gate, "owner", root, bs.BatchDeliveryOneItemCompletion)
	require.NoError(t, err)
	require.False(t, ok, "a deliverable completion must be withheld while active")
	require.Empty(t, envelope)
	finalizationSeamSetPhase(t, env, "owner", "accepted")
	envelope, ok, err = FinalizationGatedBatchResult(
		bctx, gate, "owner", root, bs.BatchDeliveryOneItemCompletion)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "failed", envelope.State)
	require.Equal(t, "exec_failed", envelope.Code)
	require.Equal(t, child+"#2", envelope.Ref)
}

func TestFinalizationAwaitGatedBatchResultBlocksUntilSealedThenDelivers(t *testing.T) {
	env := newFinalizationSeamEnv(t)
	finalizationSeamBindOwner(t, env, "owner", "logical")
	bs, _, bctx := batchStoreTestDB(t)
	root := batchTerminalTestReserve(t, bs, bctx, "", 0)
	snap, err := bs.Read(bctx, "owner", root)
	require.NoError(t, err)
	child := snap.Nodes[1].NodeID
	batchTerminalTestProject(t, bs, bctx, root, "runner", 2,
		batchTerminalTestSettledRoot(root), db.ProjectBatchNodeParams{
			NodeID: child, State: "failed", OutcomeState: "failed",
			ResponseEnvelope: batchStoreTestString(
				`{"state":"failed","code":"exec_failed","summary":"boom"}`),
			ResponseEnvelopeRef: batchStoreTestString(child + "#2"),
		})
	gate := NewPollingFinalizationTerminalGate(env.store, time.Millisecond)
	type result struct {
		envelope BatchDeliveryEnvelope
		ok       bool
		err      error
	}
	done := make(chan result, 1)
	awakeCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	go func() {
		envelope, ok, err := FinalizationAwaitGatedBatchResult(
			awakeCtx, gate, "owner", root, bs.BatchDeliveryOneItemCompletion)
		done <- result{envelope, ok, err}
	}()
	finalizationSeamSetPhase(t, env, "owner", "accepted")
	var got result
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("AwaitGatedBatchResult did not observe the phase change")
	}
	require.NoError(t, got.err)
	require.True(t, got.ok)
	require.Equal(t, "failed", got.envelope.State)
	require.Equal(t, "exec_failed", got.envelope.Code)
	require.Equal(t, child+"#2", got.envelope.Ref)
}
