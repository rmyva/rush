package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// finalizationWiringDepsOn builds enabled deps over a test environment.
func finalizationWiringDepsOn(env fakeEnv) FinalizationWiringDeps {
	return FinalizationWiringDeps{
		Enabled:     true,
		Conn:        env.conn,
		Messages:    env.messages,
		NoticeStore: session.NewSessionNoticeStore(env.conn),
		DataDir:     env.dbDir,
	}
}

// newFinalizationWiringAssembled builds a wiring literal with real durable
// stores and either the stub scope sources (nil stub) or the real readers.
func newFinalizationWiringAssembled(t *testing.T, env fakeEnv, stub *finalizationSeamScopeStub) *finalizationWiring {
	t.Helper()
	var descendants session.FinalizationDescendantsFacts = stub
	var queued session.FinalizationQueuedCallsFacts = stub
	var holds session.FinalizationPhysicalHoldsFacts = stub
	var debt session.FinalizationReactionDebtFacts = stub
	if stub == nil {
		descendants = session.NewFinalizationDescendantsReader()
		queued = session.NewFinalizationQueuedCallsFacts()
		holds = session.NewFinalizationHostDriverHoldsFacts(env.dbDir)
		debt = session.NewFinalizationDebtStore(env.conn)
	}
	return &finalizationWiring{
		coord:       &coordinator{},
		conn:        env.conn,
		store:       session.NewAgentFinalizationStore(env.conn),
		messages:    env.messages,
		noticeStore: session.NewSessionNoticeStore(env.conn),
		descendants: descendants,
		queued:      queued,
		holds:       holds,
		debtFacts:   debt,
		settler:     session.NewFinalizationDebtStore(env.conn),
		cancel:      newFinalizationCancelRegistry(),
		poll:        defaultSupervisionInterval(),
	}
}

// completedWiringNode projects one batch node to a terminal completed state
// while respecting the batch_nodes CHECK constraints.
func completedWiringNode(node string) db.ProjectBatchNodeParams {
	return db.ProjectBatchNodeParams{NodeID: node, State: "completed", OutcomeState: "completed", OutcomeCode: "done"}
}

// TestFinalizationWiringFactoryFlagOffIsNil pins the flag-off contract: no
// wiring is built, nothing is bound, and every nil-receiver entry point is a
// silent no-op.
func TestFinalizationWiringFactoryFlagOffIsNil(t *testing.T) {
	t.Parallel()
	deps := FinalizationWiringDeps{Enabled: false}
	w, err := newFinalizationWiring(deps, &coordinator{})
	require.NoError(t, err)
	require.Nil(t, w, "flag off builds nothing")
	coord := &coordinator{}
	require.NoError(t, SetCoordinatorFinalizationWiring(coord, deps))
	require.Nil(t, coord.finalization, "flag off binds nothing")

	var nilWiring *finalizationWiring
	ctx := t.Context()
	gotCtx, unregister, err := nilWiring.startExecution(ctx, SessionAgentCall{SessionID: "s", LogicalCallID: "l"})
	require.NoError(t, err)
	require.Equal(t, ctx, gotCtx, "the no-op keeps the caller's context")
	require.NotNil(t, unregister)
	require.NotPanics(t, func() { unregister() })
	require.NoError(t, nilWiring.finalizeTurn(t.Context(), nil))
	require.NotPanics(t, func() { (&coordinator{}).finalizationOnCancel("s") })
	require.Nil(t, (&sessionAgent{}).finalizationWiring())
}

// TestFinalizationWiringFactoryFlagOnInstallsFullHooks pins the flag-on
// contract: a full hook set is installed on the coordinator and a partially
// installable dependency set fails closed.
func TestFinalizationWiringFactoryFlagOnInstallsFullHooks(t *testing.T) {
	env := testEnv(t)
	coord := &coordinator{}
	require.NoError(t, SetCoordinatorFinalizationWiring(coord, finalizationWiringDepsOn(env)))
	require.NotNil(t, coord.finalization)
	w := coord.finalization
	require.NoError(t, w.hooks.Validate())
	require.NotNil(t, w.hooks.BindScope)
	require.NotNil(t, w.hooks.RecoverScope)
	require.NotNil(t, w.hooks.FinalizeTurn)
	require.NotNil(t, w.hooks.OnCancel)
	require.NotNil(t, w.settler)
	require.NotNil(t, w.descendants)
	require.NotNil(t, w.queued)
	require.NotNil(t, w.holds)
	require.NotNil(t, w.debtFacts)
	require.Positive(t, int64(w.poll), "the supervision poll must be armed")
	require.NotNil(t, w.store)
	require.NotNil(t, w.conn)
	require.NotNil(t, w.noticeStore)

	// Fail closed: a missing notice store must refuse the assembly.
	deps := finalizationWiringDepsOn(env)
	deps.NoticeStore = nil
	_, err := newFinalizationWiring(deps, &coordinator{})
	require.Error(t, err)
	deps = finalizationWiringDepsOn(env)
	deps.Messages = nil
	_, err = newFinalizationWiring(deps, &coordinator{})
	require.Error(t, err)
	deps = finalizationWiringDepsOn(env)
	deps.Conn = nil
	_, err = newFinalizationWiring(deps, &coordinator{})
	require.Error(t, err)
}

// newFinalizationFactsEnv binds "session"/"logical" and assembles a wiring
// whose scope sources are the seam stub, so every facts field is controlled.
func newFinalizationFactsEnv(t *testing.T) (fakeEnv, *finalizationWiring, *finalizationSeamScopeStub, session.AgentFinalizationIdentity) {
	t.Helper()
	env := testEnv(t)
	identity := session.AgentFinalizationIdentity{SessionID: "session", LogicalCallID: "logical", Generation: 1}
	api, ok := env.sessions.(session.AgentFinalizationService)
	require.True(t, ok)
	_, err := api.BindAgentFinalization(t.Context(), identity)
	require.NoError(t, err)
	stub := &finalizationSeamScopeStub{}
	w := newFinalizationWiringAssembled(t, env, stub)
	return env, w, stub, identity
}

// readFactsMust reads facts or fails the test; never hold a transaction
// across this call: the wiring opens and rolls back its own.
func readFactsMust(t *testing.T, w *finalizationWiring, identity session.AgentFinalizationIdentity) finalizationFacts {
	t.Helper()
	facts, err := w.readFacts(t.Context(), identity)
	require.NoError(t, err)
	require.True(t, facts.complete)
	return facts
}

// TestFinalizationWiringReadFactsMapsDurableFacts pins the production
// adapter from durable stores to the gate's facts snapshot, one field at a
// time, against a real database.
func TestFinalizationWiringReadFactsMapsDurableFacts(t *testing.T) {
	t.Run("durable revision is a high-water across admit and release", func(t *testing.T) {
		env, w, _, identity := newFinalizationFactsEnv(t)
		_ = env
		before := readFactsMust(t, w, identity)
		_, err := w.store.Admit(t.Context(), identity, "t1")
		require.NoError(t, err)
		open := readFactsMust(t, w, identity)
		require.Equal(t, before.durableRevision+1, open.durableRevision, "admit advances the durable revision")
		require.Equal(t, 1, open.own)
		require.NoError(t, w.store.Release(t.Context(), identity, "t1"))
		after := readFactsMust(t, w, identity)
		require.Equal(t, open.durableRevision+1, after.durableRevision, "release advances the durable revision exactly like admit")
		require.Zero(t, after.own)
	})

	t.Run("open ticket keeps own busy", func(t *testing.T) {
		_, w, _, identity := newFinalizationFactsEnv(t)
		facts := readFactsMust(t, w, identity)
		require.Zero(t, facts.own)
		_, err := w.store.Admit(t.Context(), identity, "held")
		require.NoError(t, err)
		facts = readFactsMust(t, w, identity)
		require.Equal(t, 1, facts.own)
		require.NoError(t, w.store.Release(t.Context(), identity, "held"))
		facts = readFactsMust(t, w, identity)
		require.Zero(t, facts.own)
	})

	t.Run("scope counters map through the stub sources", func(t *testing.T) {
		_, w, stub, identity := newFinalizationFactsEnv(t)
		stub.descendants, stub.queued, stub.holds, stub.debt = 3, 5, 7, 9
		facts := readFactsMust(t, w, identity)
		require.Equal(t, 3, facts.descendants)
		require.Equal(t, 5, facts.queuedCalls)
		require.Equal(t, 7, facts.physicalHolds)
		require.Equal(t, 9, facts.debt)
	})

	t.Run("pending batch child question holds the seal", func(t *testing.T) {
		env, w, _, identity := newFinalizationFactsEnv(t)
		facts := readFactsMust(t, w, identity)
		require.False(t, facts.heldQuestion)
		// Batch-published frame: JobToolCallID NULL, parsed by
		// ParseBatchChildQuestionNotice, never subject to the void rule.
		text := fmt.Sprintf("[batch-question node=%s token=%d sequence=%d depth=%d child=%s]\n",
			"root.1", 7, 3, 1, "child-9") +
			"SUB-AGENT QUESTION: continue?\nAnswer with inject_agent for this existing batch node; do not resume the delegation as a new generation."
		// The owner is already bound, so the S1 ingress fence would refuse a
		// store-level insert from a context without the owner's authority;
		// the frame is written directly, exactly as the batch publisher does.
		_, err := env.conn.ExecContext(t.Context(),
			`INSERT INTO session_notices (owner, kind, text, wake, created_at, updated_at) VALUES (?, ?, ?, 0, 0, 0)`,
			"session", session.NoticeKindChildQuestion, text)
		require.NoError(t, err)
		facts = readFactsMust(t, w, identity)
		require.True(t, facts.heldQuestion)
		_, err = env.conn.ExecContext(t.Context(), `DELETE FROM session_notices WHERE owner = 'session'`)
		require.NoError(t, err)
		facts = readFactsMust(t, w, identity)
		require.False(t, facts.heldQuestion)
	})

	t.Run("cancel requested surfaces as cancelled", func(t *testing.T) {
		env, w, _, identity := newFinalizationFactsEnv(t)
		facts := readFactsMust(t, w, identity)
		require.False(t, facts.cancelled)
		_, err := env.conn.ExecContext(t.Context(), `UPDATE sessions SET cancel_requested = 1 WHERE id = 'session'`)
		require.NoError(t, err)
		facts = readFactsMust(t, w, identity)
		require.True(t, facts.cancelled)
		_, err = env.conn.ExecContext(t.Context(), `UPDATE sessions SET cancel_requested = 0 WHERE id = 'session'`)
		require.NoError(t, err)
	})

	t.Run("incomplete scope facts fail closed", func(t *testing.T) {
		env := testEnv(t)
		identity := session.AgentFinalizationIdentity{SessionID: "session", LogicalCallID: "logical", Generation: 1}
		api, ok := env.sessions.(session.AgentFinalizationService)
		require.True(t, ok)
		_, err := api.BindAgentFinalization(t.Context(), identity)
		require.NoError(t, err)
		stub := &finalizationSeamScopeStub{}
		w := newFinalizationWiringAssembled(t, env, stub)
		w.holds = nil // One nil composite member must break completeness.
		_, err = w.readFacts(t.Context(), identity)
		require.ErrorIs(t, err, ErrFinalizationFactsUnavailable)
	})

	t.Run("missing durable record wraps the row error", func(t *testing.T) {
		_, w, _, _ := newFinalizationFactsEnv(t)
		absent := session.AgentFinalizationIdentity{SessionID: "absent", LogicalCallID: "logical", Generation: 1}
		_, err := w.readFacts(t.Context(), absent)
		require.Error(t, err)
		require.ErrorIs(t, err, sql.ErrNoRows)
	})

	t.Run("empty identity is refused", func(t *testing.T) {
		_, w, _, _ := newFinalizationFactsEnv(t)
		_, err := w.readFacts(t.Context(), session.AgentFinalizationIdentity{})
		require.ErrorIs(t, err, ErrFinalizationFactsUnavailable)
	})
}

// TestFinalizationWiringNewExecutionIdentityAndEpoch pins the per-call
// execution construction: only calls owning a matching authority get one.
func TestFinalizationWiringNewExecutionIdentityAndEpoch(t *testing.T) {
	env := testEnv(t)
	identity := session.AgentFinalizationIdentity{SessionID: "session", LogicalCallID: "logical", Generation: 7}
	api, ok := env.sessions.(session.AgentFinalizationService)
	require.True(t, ok)
	authority, err := api.BindAgentFinalization(t.Context(), identity)
	require.NoError(t, err)
	ctx, err := session.WithAgentFinalizationContext(t.Context(), authority)
	require.NoError(t, err)
	w := newFinalizationWiringAssembled(t, env, nil)

	call := SessionAgentCall{SessionID: "session", LogicalCallID: "logical"}
	call.finalizationAuthorities = session.CaptureAgentFinalizationContext(ctx)
	exec, err := w.newExecution(call)
	require.NoError(t, err)
	require.NotNil(t, exec)
	require.Equal(t, "session", exec.sessionID)
	require.Equal(t, "logical", exec.logicalID)
	require.EqualValues(t, 7, exec.gate.epoch)
	require.Equal(t, identity, exec.scope.identity)
	require.NotNil(t, exec.scopeFacts)
	require.NotNil(t, exec.read)

	// A call with no finalization requirement stays inert.
	bare := SessionAgentCall{SessionID: "session", LogicalCallID: "logical"}
	exec, err = w.newExecution(bare)
	require.NoError(t, err)
	require.Nil(t, exec)

	// A descendant inheriting a foreign authority stays inert.
	child := SessionAgentCall{SessionID: "child", LogicalCallID: "child-call"}
	child.finalizationAuthorities = session.CaptureAgentFinalizationContext(ctx)
	exec, err = w.newExecution(child)
	require.NoError(t, err)
	require.Nil(t, exec)
}

// TestFinalizationWiringStartExecutionRegistersAndUnregisters pins the
// driver entry: registration feeds OnCancel, unregistration removes exactly
// that execution, and a refused bind leaves the registry untouched.
func TestFinalizationWiringStartExecutionRegistersAndUnregisters(t *testing.T) {
	env := testEnv(t)
	identity := session.AgentFinalizationIdentity{SessionID: "session", LogicalCallID: "logical", Generation: 1}
	api, ok := env.sessions.(session.AgentFinalizationService)
	require.True(t, ok)
	authority, err := api.BindAgentFinalization(t.Context(), identity)
	require.NoError(t, err)
	ctx, err := session.WithAgentFinalizationContext(t.Context(), authority)
	require.NoError(t, err)
	call := SessionAgentCall{SessionID: "session", LogicalCallID: "logical"}
	call.finalizationAuthorities = session.CaptureAgentFinalizationContext(ctx)

	coord := &coordinator{currentAgent: &sessionAgent{}}
	w, err := newFinalizationWiring(finalizationWiringDepsOn(env), coord)
	require.NoError(t, err)

	execCtx, unregister, err := w.startExecution(ctx, call)
	require.NoError(t, err)
	exec := finalizationExecutionFor(execCtx, call)
	require.NotNil(t, exec)
	require.False(t, exec.gate.cancelled)

	// After unregistration the coordinator interrupt no longer reaches this
	// execution's gate.
	unregister()
	w.hooks.OnCancel("session")
	require.False(t, exec.gate.cancelled, "an unregistered execution must not be cancelled")

	// A fresh registration is cancellable and beginCandidate fails closed.
	execCtx2, unregister2, err := w.startExecution(ctx, call)
	require.NoError(t, err)
	exec2 := finalizationExecutionFor(execCtx2, call)
	require.NotNil(t, exec2)
	w.hooks.OnCancel("session")
	require.True(t, exec2.gate.cancelled)
	_, err = exec2.gate.beginCandidate(t.Context(), exec2.read)
	require.ErrorIs(t, err, context.Canceled)
	unregister2()

	// A refused bind propagates and leaves the cancel registry empty.
	w.hooks.BindScope = func(context.Context, SessionAgentCall) error { return errors.New("bind refused") }
	_, _, err = w.startExecution(ctx, call)
	require.ErrorContains(t, err, "bind refused")
	w.cancel.mu.Lock()
	registered := len(w.cancel.byKey["session"])
	w.cancel.mu.Unlock()
	require.Zero(t, registered, "a failed start must not register anything")
	w.hooks.OnCancel("session") // No-op on the empty registry.
}

// finalizationWiringPublish builds the trySeal publish closure used by the
// integration tests; it counts publications and stamps the durable identity.
func finalizationWiringPublish(env fakeEnv, ts *turnStream, scope finalizationPublicationScope, counter *int) func(context.Context, finalizationCandidate) error {
	return func(ctx context.Context, candidate finalizationCandidate) error {
		*counter++
		stamp := ts.finalizationStamp
		_, err := publishFinalizationCandidate(ctx, env.sessions, env.messages, scope, message.FinalizationIdentity{
			Key: ts.finalization.logicalID, Generation: stamp.epoch, Revision: stamp.durableRevision, Serial: stamp.serial,
		}, candidate)
		return err
	}
}

// TestFinalizationWiringIntegrationWaitSealAccepted drives the full real
// composite: a live descendant and an unsettled debt hold the seal as
// WaitWork; once both clear, the same candidate seals as Accepted with one
// publication and an accepted durable record.
func TestFinalizationWiringIntegrationWaitSealAccepted(t *testing.T) {
	env := testEnv(t)
	// The S1 ingress fence refuses new batch reservations and notices for an owner
	// with a bound scope, so the live descendant and the open debt are seeded
	// BEFORE the bind, as in production.
	batches := session.NewBatchStore(env.conn)
	root := batchTerminalTestReserve(t, batches, t.Context(), "session", "call-w1a-live", "hash-w1a-live")
	notices := session.NewSessionNoticeStore(env.conn)
	require.NoError(t, notices.InsertSessionNotice(t.Context(), "session", session.NoticeKindWakeOnly, "finalization-debt", true, ""))
	identity := session.AgentFinalizationIdentity{SessionID: "session", LogicalCallID: "logical", Generation: 1}
	api, ok := env.sessions.(session.AgentFinalizationService)
	require.True(t, ok)
	authority, err := api.BindAgentFinalization(t.Context(), identity)
	require.NoError(t, err)
	ctx, err := session.WithAgentFinalizationContext(t.Context(), authority)
	require.NoError(t, err)
	call := SessionAgentCall{SessionID: "session", LogicalCallID: "logical"}
	call.finalizationAuthorities = session.CaptureAgentFinalizationContext(ctx)
	coord := &coordinator{currentAgent: &sessionAgent{}}
	w, err := newFinalizationWiring(finalizationWiringDepsOn(env), coord)
	require.NoError(t, err)

	execCtx, unregister, err := w.startExecution(ctx, call)
	require.NoError(t, err)
	defer unregister()
	exec := finalizationExecutionFor(execCtx, call)
	require.NotNil(t, exec)

	// Candidate message and a stamped finalization step.
	created, err := env.messages.Create(execCtx, "session", message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "provisional answer"}},
	})
	require.NoError(t, err)
	ts := &turnStream{
		finalization:     exec,
		currentAssistant: &created,
		fenceCheckpoint:  (&turnCheckpointWriter{}).fenceFinalization,
		call:             call,
	}
	persistCandidate := func() {
		stepID := ts.currentAssistant.FinishPart().FinalizationIdentity()
		_, err := persistFinalizationCandidate(execCtx, env.sessions, env.messages, exec.scope, stepID, *ts.currentAssistant)
		require.NoError(t, err)
	}
	publications := 0
	publish := finalizationWiringPublish(env, ts, exec.scope, &publications)

	// Round 1: live work holds the seal as WaitWork, no publication.
	require.NoError(t, ts.beginFinalizationStep(execCtx))
	require.False(t, ts.finalizationStamp.ready, "noisy facts must not arm a ready stamp")
	require.NoError(t, ts.markFinalizationStep())
	persistCandidate()
	hash, err := ts.currentAssistant.FinalizationContentHash()
	require.NoError(t, err)
	candidate := finalizationCandidate{messageID: created.ID, hash: hash}
	verdict, err := exec.gate.trySeal(execCtx, ts.finalizationStamp, candidate, exec.read, publish)
	require.NoError(t, err)
	require.Equal(t, finalizationWaitWork, verdict)
	require.Zero(t, publications, "noisy facts must not publish")

	// Clear the descendant: project the root and its task to completed.
	snap, err := batches.Read(execCtx, "session", root)
	require.NoError(t, err)
	require.Len(t, snap.Nodes, 2)
	// The producer path (BatchStore.Project) needs a finalization admission that
	// only the runner commit hook can build; the engine is out of scope here, so
	// the two rows are completed through the same sqlc update the projection uses.
	completeWiringNodes(t, env, "session", root, root, snap.Nodes[1].NodeID)

	// Clear the debt through the real settler.
	settled, err := w.settler.SettleFinalizationDebts(execCtx, "session")
	require.NoError(t, err)
	require.GreaterOrEqual(t, settled, 1)

	// Round 2: quiet facts seal the same shape as Accepted, once.
	require.NoError(t, ts.beginFinalizationStep(execCtx))
	require.True(t, ts.finalizationStamp.ready, "quiet facts must arm a ready stamp")
	require.NoError(t, ts.markFinalizationStep())
	persistCandidate()
	verdict, err = exec.gate.trySeal(execCtx, ts.finalizationStamp, candidate, exec.read, publish)
	require.NoError(t, err)
	require.Equal(t, finalizationAccepted, verdict)
	require.Equal(t, 1, publications)

	record, err := session.NewAgentFinalizationStore(env.conn).Snapshot(execCtx, identity)
	require.NoError(t, err)
	require.Equal(t, "accepted", record.Phase)
	stored, err := env.messages.Get(execCtx, created.ID)
	require.NoError(t, err)
	require.True(t, stored.IsFinished(), "the published final message must not stay partial")
	require.Equal(t, "logical", stored.FinishPart().FinalizationKey)
}

// TestFinalizationWiringTrySealPausesOnHeldQuestion pins the held-question
// pause: a pending child question blocks the seal without publishing.
func TestFinalizationWiringTrySealPausesOnHeldQuestion(t *testing.T) {
	env := testEnv(t)
	identity := session.AgentFinalizationIdentity{SessionID: "session", LogicalCallID: "logical", Generation: 1}
	api, ok := env.sessions.(session.AgentFinalizationService)
	require.True(t, ok)
	authority, err := api.BindAgentFinalization(t.Context(), identity)
	require.NoError(t, err)
	ctx, err := session.WithAgentFinalizationContext(t.Context(), authority)
	require.NoError(t, err)
	call := SessionAgentCall{SessionID: "session", LogicalCallID: "logical"}
	call.finalizationAuthorities = session.CaptureAgentFinalizationContext(ctx)
	coord := &coordinator{currentAgent: &sessionAgent{}}
	w, err := newFinalizationWiring(finalizationWiringDepsOn(env), coord)
	require.NoError(t, err)

	execCtx, unregister, err := w.startExecution(ctx, call)
	require.NoError(t, err)
	defer unregister()
	exec := finalizationExecutionFor(execCtx, call)
	require.NotNil(t, exec)

	text := fmt.Sprintf("[batch-question node=%s token=%d sequence=%d depth=%d child=%s]\n",
		"root.1", 7, 3, 1, "child-9") +
		"SUB-AGENT QUESTION: continue?\nAnswer with inject_agent for this existing batch node; do not resume the delegation as a new generation."
	notices := session.NewSessionNoticeStore(env.conn)
	require.NoError(t, notices.InsertSessionNotice(execCtx, "session", session.NoticeKindChildQuestion, text, false, ""))
	t.Cleanup(func() {
		_, _ = env.conn.ExecContext(context.Background(), `DELETE FROM session_notices WHERE owner = 'session'`)
	})

	created, err := env.messages.Create(execCtx, "session", message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "provisional answer"}},
	})
	require.NoError(t, err)
	ts := &turnStream{
		finalization:     exec,
		currentAssistant: &created,
		fenceCheckpoint:  (&turnCheckpointWriter{}).fenceFinalization,
		call:             call,
	}
	require.NoError(t, ts.beginFinalizationStep(execCtx))
	require.False(t, ts.finalizationStamp.ready, "a held question is not quiet")
	require.NoError(t, ts.markFinalizationStep())
	stepID := ts.currentAssistant.FinishPart().FinalizationIdentity()
	_, err = persistFinalizationCandidate(execCtx, env.sessions, env.messages, exec.scope, stepID, *ts.currentAssistant)
	require.NoError(t, err)
	hash, err := ts.currentAssistant.FinalizationContentHash()
	require.NoError(t, err)
	publications := 0
	publish := finalizationWiringPublish(env, ts, exec.scope, &publications)
	verdict, err := exec.gate.trySeal(execCtx, ts.finalizationStamp, finalizationCandidate{messageID: created.ID, hash: hash}, exec.read, publish)
	require.NoError(t, err)
	require.Equal(t, finalizationPauseQuestion, verdict)
	require.Zero(t, publications)
}

// TestFinalizationWiringOnCancelUnblocksWaiter pins the interrupt contract:
// OnCancel cancels the registered gate, which unblocks a waiting seal
// immediately and makes the facts read fail closed.
func TestFinalizationWiringOnCancelUnblocksWaiter(t *testing.T) {
	env := testEnv(t)
	// The S1 ingress fence refuses new wake notices for a bound owner, so the
	// debt that makes the facts noisy is seeded BEFORE the bind.
	notices := session.NewSessionNoticeStore(env.conn)
	require.NoError(t, notices.InsertSessionNotice(t.Context(), "session", session.NoticeKindWakeOnly, "finalization-debt", true, ""))
	identity := session.AgentFinalizationIdentity{SessionID: "session", LogicalCallID: "logical", Generation: 1}
	api, ok := env.sessions.(session.AgentFinalizationService)
	require.True(t, ok)
	authority, err := api.BindAgentFinalization(t.Context(), identity)
	require.NoError(t, err)
	ctx, err := session.WithAgentFinalizationContext(t.Context(), authority)
	require.NoError(t, err)
	call := SessionAgentCall{SessionID: "session", LogicalCallID: "logical"}
	call.finalizationAuthorities = session.CaptureAgentFinalizationContext(ctx)
	coord := &coordinator{currentAgent: &sessionAgent{}}
	w, err := newFinalizationWiring(finalizationWiringDepsOn(env), coord)
	require.NoError(t, err)

	execCtx, unregister, err := w.startExecution(ctx, call)
	require.NoError(t, err)
	defer unregister()
	exec := finalizationExecutionFor(execCtx, call)
	require.NotNil(t, exec)

	// Noisy facts (the debt was seeded before the bind): the waiter must come
	// back as WaitWork with a signal.
	verdict, changed, err := exec.gate.waitState(t.Context(), exec.read)
	require.NoError(t, err)
	require.Equal(t, finalizationWaitWork, verdict)
	require.NotNil(t, changed)

	// Cancel first, then observe the unblock with a bounded select.
	w.hooks.OnCancel("session")
	require.True(t, exec.gate.cancelled)
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not unblocked by OnCancel")
	}
	// The gate refuses its own reads once cancelled; the durable record is
	// untouched by the in-process cancel (the durable half is RequestCancel).
	_, err = exec.gate.readLocked(t.Context(), exec.read)
	require.ErrorIs(t, err, context.Canceled)

	// Unregistration plus a repeat interrupt stays silent.
	unregister()
	require.NotPanics(t, func() { w.hooks.OnCancel("session") })
}

// completeWiringNodes marks batch nodes completed with the sqlc update
// BatchStore.Project applies, bypassing only the runner-side finalization
// admission (see the call site).
func completeWiringNodes(t *testing.T, env fakeEnv, owner, root string, nodes ...string) {
	t.Helper()
	q := db.New(env.conn)
	for _, node := range nodes {
		update := completedWiringNode(node)
		update.ControlState = "none"
		update.Sequence = 2 // the reservation wrote sequence 1; the update fence is projection_sequence < sequence
		update.UpdatedAt = 1
		update.RootID = root
		update.HostID = "host"
		update.OwnerSessionID = owner
		update.RunnerID = "runner-1"
		changed, err := q.ProjectBatchNode(t.Context(), update)
		require.NoError(t, err)
		require.EqualValues(t, 1, changed, "node %s must be updated", node)
	}
}
