package jobbatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRuntimeCommitOwnerPinsAndRelease(t *testing.T) {
	t.Parallel()
	for _, op := range []runnerRequestOp{runnerRequestOutput, runnerRequestInject} {
		name := "output"
		if op == runnerRequestInject {
			name = "inject"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entered, release := make(chan struct{}), make(chan struct{})
			accepted := make(chan *runnerRequest, 16)
			settled := make(chan struct{}, 1)
			r, e, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
				if tr.Event.Type == EventCapacityAvailable {
					close(entered)
					<-release
				}
				return nil
			}, &runnerRequestHooks{accepted: func(q *runnerRequest) { accepted <- q }, transitioned: func(tr Transition) {
				if tr.Event.Type == EventSettled {
					settled <- struct{}{}
				}
			}})
			b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
			disposed := make(chan struct{}, 1)
			a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{dispose: func(context.Context) error { disposed <- struct{}{}; return nil }})
			runtimeTestReceive(t, ctx, accepted)
			capacity := make(chan error, 1)
			go func() { capacity <- r.CapacityAvailable(ctx, "") }()
			runtimeTestReceive(t, ctx, entered)
			runtimeTestReceive(t, ctx, accepted)
			answer := make(chan runnerAnswer, 1)
			go func() { answer <- r.request(ctx, &runnerRequest{op: op, node: "r.1"}) }()
			runtimeTestReceive(t, ctx, accepted)
			barrier := make(chan error, 1)
			go func() { barrier <- r.Stop(ctx, "unknown") }()
			runtimeTestReceive(t, ctx, accepted)
			// The next accepted request is after pinning the inspection in owner order.
			r.mu.Lock()
			h := r.handles[0]
			r.mu.Unlock()
			h.mu.Lock()
			pins := h.pins
			h.mu.Unlock()
			require.Equal(t, 1, pins, "inspection pin delayed until publication")
			a.report(Result{State: StateCompleted})
			runtimeTestReceive(t, ctx, settled)
			released := make(chan error, 1)
			go func() { released <- r.Release(ctx, "r") }()
			runtimeTestReceive(t, ctx, accepted)
			// An additional request barriers after Release, which must give up ownership.
			go func() { _ = r.Stop(ctx, "unknown") }()
			runtimeTestReceive(t, ctx, accepted)
			h.mu.Lock()
			givenUp := h.givenUp
			h.mu.Unlock()
			require.True(t, givenUp, "Release ownership waited for commit")
			require.Empty(t, disposed, "Dispose overlapped undelivered inspection pin")
			select {
			case <-b.Done():
				t.Fatal("unpublished Settled closed Done")
			default:
			}
			close(release)
			require.NoError(t, runtimeTestReceive(t, ctx, capacity))
			got := runtimeTestReceive(t, ctx, answer)
			require.NoError(t, got.err)
			got.handle.unpin()
			require.ErrorIs(t, runtimeTestReceive(t, ctx, barrier), ErrUnknownNode)
			require.NoError(t, runtimeTestReceive(t, ctx, released))
			runtimeTestWait(t, ctx, b)
			runtimeTestReceive(t, ctx, disposed)
			_, err := r.View(ctx, "r")
			require.ErrorIs(t, err, ErrReleased)
			runtimeTestClose(t, ctx, r)
		})
	}
}

func TestRuntimeCommitOwnerCapturedRecords(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	accepted := make(chan struct{}, 4)
	r, e, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventSubmit {
			close(entered)
			<-release
		}
		return nil
	}, &runnerRequestHooks{accepted: func(q *runnerRequest) {
		if q.event.Type == EventStopRequested {
			accepted <- struct{}{}
		}
	}})
	reply := make(chan *Batch, 1)
	go func() {
		b, _ := r.Submit(ctx, SubmitRequest{Root: "r", Spec: OneItem(TaskSpec{Kind: "test"})})
		reply <- b
	}()
	runtimeTestReceive(t, ctx, entered)
	stop := make(chan error, 1)
	go func() { stop <- r.Stop(ctx, "r") }()
	runtimeTestReceive(t, ctx, accepted)
	go func() { _ = r.Stop(ctx, "unknown") }()
	runtimeTestReceive(t, ctx, accepted)
	close(release)
	b := runtimeTestReceive(t, ctx, reply)
	require.NoError(t, runtimeTestReceive(t, ctx, stop))
	summary := runtimeTestWait(t, ctx, b)
	page := runtimeTestPage(t, ctx, r)
	require.Equal(t, StateRunning, page.Records[0].State, "record used later Engine state")
	require.Equal(t, StateCancelled, summary.State)
	require.Empty(t, e.starts, "stopped unpublished Start dispatched")
	runtimeTestClose(t, ctx, r)
}

func TestRuntimeCommitOwnerFinalGiveUp(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	r, e, _, ctx := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
		if tr.Final {
			close(entered)
			<-release
		}
		return nil
	})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	disposed := make(chan struct{}, 1)
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{dispose: func(context.Context) error {
		r.feed.mu.Lock()
		final := r.feed.final
		r.feed.mu.Unlock()
		if final == 0 {
			t.Error("Dispose before Final publication")
		}
		disposed <- struct{}{}
		return nil
	}})
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	runtimeTestWait(t, ctx, b)
	reply := make(chan error, 1)
	go func() { reply <- r.Close(ctx) }()
	runtimeTestReceive(t, ctx, entered)
	r.mu.Lock()
	h := r.handles[0]
	r.mu.Unlock()
	h.mu.Lock()
	givenUp := h.givenUp
	h.mu.Unlock()
	require.False(t, givenUp, "Final gave up at Apply")
	require.Empty(t, disposed)
	close(release)
	require.NoError(t, runtimeTestReceive(t, ctx, reply))
	runtimeTestReceive(t, ctx, disposed)
}

func TestRuntimeCommitOwnerBound(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	accepted := make(chan struct{}, runnerPublicationLimit+8)
	progress := make(chan EventType, 8)
	r, e, clock, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventCapacityAvailable {
			once.Do(func() { close(entered) })
			<-release
		}
		return nil
	}, &runnerRequestHooks{accepted: func(q *runnerRequest) {
		if q.event.Type == EventCapacityAvailable || (q.event.Type == EventSubmit && q.event.Root == "extra") {
			accepted <- struct{}{}
		}
	}, transitioned: func(tr Transition) {
		if tr.Event.Type == EventSettled || tr.Event.Type == EventDeadlineExpired {
			progress <- tr.Event.Type
		}
	}})
	drainDone := make(chan struct{})
	defer close(drainDone)
	go func() {
		for {
			select {
			case <-clock.armed:
			case <-drainDone:
				return
			}
		}
	}()
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test", Timeout: 10 * time.Second}))
	stopped := make(chan struct{}, 1)
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(context.Context) error { stopped <- struct{}{}; return nil }})
	for range runnerPublicationLimit {
		go func() { _ = r.CapacityAvailable(ctx, "") }()
		runtimeTestReceive(t, ctx, accepted)
	}
	runtimeTestReceive(t, ctx, entered)
	extraCtx, cancel := context.WithCancel(ctx)
	extra := make(chan error, 1)
	go func() {
		_, err := r.Submit(extraCtx, SubmitRequest{Root: "extra", Spec: runtimeTestBatch(ModeParallel, NotifyAll)})
		extra <- err
	}()
	clock.mu.Lock()
	var deadline *runtimeTestTimer
	for _, timer := range clock.timers {
		if timer.at == 10*time.Second {
			deadline = timer
			break
		}
	}
	clock.mu.Unlock()
	require.NotNil(t, deadline)
	deadline.f()
	require.Equal(t, EventDeadlineExpired, runtimeTestReceive(t, ctx, progress))
	// Stop remains accepted independently of the bounded noncontainment queue.
	stop := make(chan error, 1)
	go func() { stop <- r.Stop(ctx, "r") }()
	runtimeTestReceive(t, ctx, stopped)
	require.Empty(t, accepted, "publication FIFO accepted beyond bound")
	cancel()
	require.ErrorIs(t, runtimeTestReceive(t, ctx, extra), context.Canceled)
	a.report(Result{State: StateCancelled})
	require.Equal(t, EventSettled, runtimeTestReceive(t, ctx, progress))
	close(release)
	require.NoError(t, runtimeTestReceive(t, ctx, stop))
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
}

// TestRuntimeContainmentFloodStaysBounded proves containment traffic cannot
// grow the bounded publication queue: while a stop commit is held in the
// hook, recordless (unknown-node) and coalesced (already-cancelling) stops
// fence behind the held item instead of allocating publications. The depth
// snapshots are read from the queued hook, which runs on the owner loop, so
// the queue is observed synchronously.
func TestRuntimeContainmentFloodStaysBounded(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	stopRelease := make(chan struct{})
	stopped := make(chan struct{}, 1)
	first := make(chan error, 1)
	depths := make(chan int, 2)
	fencedCounts := make(chan int, 2)
	stopAccepted := make(chan struct{}, 900)
	var commitHoldOnce, stopHoldOnce sync.Once
	releaseCommitHold := func() { commitHoldOnce.Do(func() { close(release) }) }
	releaseStopHold := func() { stopHoldOnce.Do(func() { close(stopRelease) }) }
	var runner *Runner
	hooks := &runnerRequestHooks{queued: func(q *runnerRequest) {
		if q.event.Type == EventStopRequested {
			stopAccepted <- struct{}{}
		}
		if q.event.Type == EventCapacityAvailable {
			depths <- len(runner.publicationQueue)
			fencedTotal := 0
			for _, item := range runner.publicationQueue {
				fencedTotal += len(item.fenced)
			}
			fencedCounts <- fencedTotal
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, e, _, _ := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventStopRequested {
			entered <- struct{}{}
			<-release
		}
		return nil
	}, hooks)
	runner = r
	t.Cleanup(func() {
		releaseCommitHold()
		releaseStopHold()
	})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test", Group: "g"}))
	// The physical stop blocks until stopRelease, so no stop-result message
	// is processed while the hook is held: no ControlReported publication
	// can appear and the queue depth while held is deterministic.
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(context.Context) error {
		stopped <- struct{}{}
		<-stopRelease
		return nil
	}})
	// Fire the FIRST real stop alone. It applies a StopRequested transition
	// and parks in the commit hook, holding its publication.
	go func() { first <- r.Stop(ctx, "r") }()
	runtimeTestReceive(t, ctx, stopped)
	runtimeTestReceive(t, ctx, entered)
	// The FIRST real stop also passes the queued hook; drain its token so
	// only flood tokens remain for awaitAccepted.
	runtimeTestReceive(t, ctx, stopAccepted)
	// Flood callers use the same generous deadline as every other blocking step.
	// Flood while the commit is held, without waiting for replies: every
	// flood caller fences onto the held publication and blocks.
	flood := func(pairs int) (unknownErrs, redundantErrs chan error, floodDone chan struct{}) {
		// Each iteration launches a PAIR of callers (one unknown-root stop and
		// one redundant stop), so 2*pairs callers total; every caller gets
		// buffer for its reply.
		unknownErrs, redundantErrs = make(chan error, 2*pairs), make(chan error, 2*pairs)
		floodDone = make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < pairs; i++ {
			wg.Add(2)
			go func() { defer wg.Done(); unknownErrs <- r.Stop(ctx, "unknown.root") }()
			go func() { defer wg.Done(); redundantErrs <- r.Stop(ctx, "r") }()
		}
		go func() { wg.Wait(); close(floodDone) }()
		return unknownErrs, redundantErrs, floodDone
	}
	waitFlood := func(done chan struct{}) {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("flood callers did not resolve")
		}
	}
	// Cumulative barrier: acceptedSeen counts every StopRequested acceptance
	// so far (including the first real stop's token, drained above), so each
	// barrier waits for the exact number of callers launched by that flood.
	acceptedSeen := 0
	awaitAcceptedUntil := func(total int) {
		for acceptedSeen < total {
			select {
			case <-stopAccepted:
				acceptedSeen++
			case <-ctx.Done():
				t.Fatalf("stalled acceptance: only %d of %d flood callers were accepted before deadline: %v", acceptedSeen, total, ctx.Err())
			}
		}
	}
	unknown1, redundant1, done1 := flood(25) // 25 pairs = 50 callers
	awaitAcceptedUntil(50)
	capacity1 := make(chan error, 1)
	go func() { capacity1 <- r.CapacityAvailable(ctx, "") }()
	depth1 := runtimeTestReceive(t, ctx, depths)
	unknown400, redundant400, done400 := flood(200) // 200 pairs = 400 callers
	awaitAcceptedUntil(450)                         // 50 (flood-1) + 400 (flood-2), cumulative; the first stop's token was drained above
	capacity2 := make(chan error, 1)
	go func() { capacity2 <- r.CapacityAvailable(ctx, "") }()
	depth2 := runtimeTestReceive(t, ctx, depths)
	// While the hook is held, the queue contains exactly the held
	// StopRequested publication: recordless callers fence onto it without
	// allocating, and the sentinel CapacityAvailable has not been appended
	// yet when its own queued hook fires (the queued hook runs in
	// acceptRequest BEFORE handleRequest appends the publication).
	// Sentinel-1's publication stays uncommitted while the committer is
	// parked in the held hook, so the second snapshot reads exactly 2
	// ([held StopRequested, sentinel-1 CapacityAvailable]) — still
	// flood-size independent, which is the bounded property under test.
	require.Equal(t, 1, depth1)
	require.Equal(t, 2, depth2)
	// Fence derivation: every flood caller produces a recordless answer —
	// flood-1 launches 25 unknown + 25 redundant = 50 recordless callers
	// (unknown nodes fail lookup; a coalesced stop returns an empty answer),
	// and flood-2 launches 200 + 200 = 400 more. Each caller is appended to
	// the queue tail's fence list at acceptance time. At sentinel-1 the queue
	// is only the held item, so the sum is exactly 50. At sentinel-2 the
	// flood-2 fences are split between the held item and sentinel-1 depending
	// on acceptance interleaving, but their SUM over the whole queue is
	// deterministically 450 (50 + 400): fenced answers are never dropped and
	// never allocated publications.
	fenced1 := runtimeTestReceive(t, ctx, fencedCounts)
	fenced2 := runtimeTestReceive(t, ctx, fencedCounts)
	require.Equal(t, 50, fenced1)
	require.Equal(t, 450, fenced2)
	// Before release: no flood caller may have been answered — immediate
	// unfenced replies would violate the documented fence. The shared ctx bounds
	// every caller; awaitAccepted proves all were accepted before replies are asserted,
	// and the typed-rejection loops below ensure a timeout cannot pass silently.
	assertUnanswered := func(chs ...<-chan error) {
		for _, ch := range chs {
			select {
			case err := <-ch:
				t.Fatalf("flood caller answered while the commit was held: %v", err)
			default:
			}
		}
	}
	assertUnanswered(unknown1, redundant1, unknown400, redundant400)
	// Release the held commit: it publishes, replies nil to the first stop
	// and flushes every fenced flood answer from the held item's fence list.
	releaseCommitHold()
	waitFlood(done1)
	waitFlood(done400)
	close(unknown1)
	close(redundant1)
	close(unknown400)
	close(redundant400)
	for err := range unknown1 {
		require.ErrorIs(t, err, ErrUnknownNode)
	}
	for err := range redundant1 {
		require.NoError(t, err)
	}
	for err := range unknown400 {
		require.ErrorIs(t, err, ErrUnknownNode)
	}
	for err := range redundant400 {
		require.NoError(t, err)
	}
	require.NoError(t, runtimeTestReceive(t, ctx, capacity1))
	require.NoError(t, runtimeTestReceive(t, ctx, capacity2))
	// Now let the physical stop return so the ControlReported publication
	// is processed, then drain the first stop's reply and tear down.
	releaseStopHold()
	require.NoError(t, runtimeTestReceive(t, ctx, first))
	runtimeTestFinish(t, ctx, r, a, StateCancelled)
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
}

// TestRuntimeDurableSubtreeStopRetry proves a durable retry Stop is applied,
// not coalesced, once the subtree has a ControlFailed leaf: the retry's own
// StopRequested transition commits (entering the hook a second time) and its
// StopLeaf command for the failed leaf is dispatched from applyDurable before
// the commit resolves.
func TestRuntimeDurableSubtreeStopRetry(t *testing.T) {
	t.Parallel()
	stopTransitions := make(chan struct{}, 4)
	entered2 := make(chan struct{}, 1)
	release2 := make(chan struct{})
	var retryReleaseOnce sync.Once
	releaseRetry := func() { retryReleaseOnce.Do(func() { close(release2) }) }
	t.Cleanup(releaseRetry)
	stopCalls := make(chan ID, 2)      // sends the NODE id of each physical Stop call
	stopResults := make(chan error, 1) // first physical stop fails, retry succeeds
	stopResults <- errors.New("PRIVATE")
	retryStopExecution := &runtimeTestExecution{stop: func(context.Context) error {
		stopCalls <- "R.1"
		return <-stopResults
	}}
	retryControlOK := make(chan struct{}, 1)
	var stopSeq int
	r, e, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventStopRequested {
			// Plain ints are safe: the hook runs serially in the committer
			// goroutine; records are appended only at publish.
			stopTransitions <- struct{}{}
			stopSeq++
			if stopSeq >= 2 {
				entered2 <- struct{}{}
				<-release2
			}
		}
		return nil
	}, &runnerRequestHooks{transitioned: func(tr Transition) {
		if tr.Event.Type == EventControlReported && tr.Event.Node == "R.1" && tr.Event.Code == "" {
			retryControlOK <- struct{}{}
		}
	}})
	b := runtimeTestSubmit(t, ctx, r, "R", runtimeTestBatch(ModeParallel, NotifyAll, runtimeTestTask(), runtimeTestTask()))
	// Physical Start ordering is deliberately not assumed; bind executions
	// by node ID exactly like TestRuntimeSelectiveSubtreeStopRetry.
	attempts := make(map[ID]*runtimeTestAttempt)
	for range 2 {
		attempt := runtimeTestReceive(t, ctx, e.starts)
		attempts[attempt.launch.ID] = attempt
		execution := Execution(&runtimeTestExecution{})
		if attempt.launch.ID == "R.1" {
			execution = retryStopExecution
		}
		attempt.decision <- runtimeTestDecision{execution: execution}
		runtimeTestRecord(t, ctx, r, RecordStarted, attempt.launch.ID)
	}
	a1, a2 := attempts["R.1"], attempts["R.2"]
	require.NoError(t, r.Stop(ctx, "R"))
	require.Equal(t, CodeStopFailed, runtimeTestRecord(t, ctx, r, RecordControl, "R.1").Code)
	runtimeTestRecord(t, ctx, r, RecordControl, "R.2")
	// Drain the FIRST physical stop's token: the next stopCalls token can
	// only come from the retry's StopLeaf dispatch.
	<-stopCalls
	// Fire the redundant durable retry. With the round-1 guard (no subtree
	// check) the root's own Control is not ControlFailed and the request
	// would return immediately without any transition, so entered2 would
	// never fire — this wait is the proof the retry was NOT coalesced.
	retry := make(chan error, 1)
	go func() { retry <- r.Stop(ctx, "R") }()
	select {
	case <-entered2:
	case <-time.After(10 * time.Second):
		t.Fatal("redundant durable retry was coalesced: no second StopRequested transition")
	}
	// The retry's StopLeaf for R.1 is dispatched in applyDurable BEFORE the
	// held commit resolves; this token can ONLY be the retry's because the
	// first physical stop's token was drained above.
	select {
	case <-stopCalls:
	case <-time.After(10 * time.Second):
		t.Fatal("retry StopLeaf for R.1 not dispatched while commit was held")
	}
	// Hand the retry's stop a nil result: its receive is the release point,
	// so the ControlReported below always carries Code "".
	stopResults <- nil
	releaseRetry()
	require.NoError(t, <-retry)
	// Production ignores stop results after retirement, so wait for the
	// retry's successful ControlReported record BY EVENT before settling.
	select {
	case <-retryControlOK:
	case <-time.After(10 * time.Second):
		t.Fatal("retry ControlReported for R.1 with Code \"\" never committed")
	}
	runtimeTestFinish(t, ctx, r, a1, StateCancelled)
	runtimeTestFinish(t, ctx, r, a2, StateCancelled)
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
	stopRequested, controlR1 := 0, 0
	page := runtimeTestPage(t, ctx, r)
	for _, rec := range page.Records {
		if rec.Type == RecordStopRequested && rec.Node == "R" {
			stopRequested++
		}
		if rec.Type == RecordControl && rec.Node == "R.1" {
			controlR1++
		}
	}
	if stopRequested != 2 {
		t.Fatalf("RecordStopRequested for R = %d, want 2 (not coalesced)", stopRequested)
	}
	if controlR1 != 2 {
		t.Fatalf("RecordControl for R.1 = %d, want 2 (failed + successful retry)", controlR1)
	}
}

// TestRuntimeDurableRedundantStopCoalesced proves the durable coalescing
// guard still collapses a fully-acked subtree: with the only leaf
// ControlAcked (no ControlFailed anywhere), a second durable Stop applies no
// second StopRequested transition.
func TestRuntimeDurableRedundantStopCoalesced(t *testing.T) {
	t.Parallel()
	applied := make(chan struct{}, 4)
	r, e, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventStopRequested {
			applied <- struct{}{}
		}
		return nil
	}, nil)
	b := runtimeTestSubmit(t, ctx, r, "S", OneItem(TaskSpec{Kind: "test"}))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	require.NoError(t, r.Stop(ctx, "S"))
	select {
	case <-applied:
	case <-time.After(10 * time.Second):
		t.Fatal("first durable Stop did not apply")
	}
	// Wait for the leaf's ControlReported (Code "" after the physical stop
	// returned nil): the leaf is then Cancelling/ControlAcked and the
	// subtree has no ControlFailed, so the second Stop must coalesce.
	runtimeTestRecord(t, ctx, r, RecordControl, "S.1")
	require.NoError(t, r.Stop(ctx, "S"))
	// The redundant Stop's nil reply is only sent after its acceptRequest
	// completed, so any wrongly-applied second transition would already be
	// in the commit-hook applied channel before this reply — one
	// non-blocking check here is sufficient; no quiet-window sleep.
	select {
	case <-applied:
		t.Fatal("redundant durable Stop applied a second transition")
	default:
	}
	runtimeTestFinish(t, ctx, r, a, StateCancelled)
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
}
