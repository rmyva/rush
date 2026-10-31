package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDispatchRecoversPanic verifies that a handler panic inside dispatch is
// recovered and logged rather than propagating and crashing the process (the
// underlying goroutine belongs to the Go runtime's scheduler, so an
// unrecovered panic there brings down the whole binary — every other
// connection and in-flight agent session along with it).
func TestDispatchRecoversPanic(t *testing.T) {
	// Deliberately NOT t.Parallel(): this test captures process-global
	// slog.Default() output. TestDispatchConnectionSurvivesHandlerPanic
	// below also triggers a "ws: handler panic" log via dispatch's
	// recover(), which always logs through whatever slog.Default()
	// currently is - running them concurrently lets one test's panic log
	// leak into (or crowd out) the other's captured buffer, since neither
	// test's t.Cleanup ordering is synchronized with the other's capture
	// window.
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := newClient(newHub(), nil)
	t.Cleanup(func() { close(c.workQueue) })

	var done sync.WaitGroup
	done.Add(1)
	c.dispatch("handlePanicky", "", func() {
		defer done.Done()
		var m map[string]int
		m["boom"] = 1 //nolint:staticcheck // SA5000: deliberate nil-map write to trigger a real panic for this test
	})

	// If the panic escaped dispatch's recover, the test binary itself would
	// crash here rather than reaching this point — so simply completing the
	// wait is part of the assertion.
	waitWithTimeout(t, &done, 2*time.Second)

	require.Eventually(t, func() bool {
		return bytes.Contains(buf.Bytes(), []byte("ws: handler panic"))
	}, time.Second, 10*time.Millisecond, "panic must be logged")
	require.Contains(t, buf.String(), "handlePanicky", "log must identify the handler by name")
}

// TestDispatchConnectionSurvivesHandlerPanic proves the SAME connection stays
// usable after one of its handlers panics: further dispatch calls on the same
// *Client still run to completion. This is the "server stays alive" half of
// the H-3 fix — panic isolation is only useful if the connection (and thus
// the rest of the session) keeps working afterwards.
func TestDispatchConnectionSurvivesHandlerPanic(t *testing.T) {
	// Deliberately NOT t.Parallel(): see the comment on
	// TestDispatchRecoversPanic above - this test's panic also logs
	// through the process-global slog.Default(), which that test captures.
	c := newClient(newHub(), nil)
	t.Cleanup(func() { close(c.workQueue) })

	var panicked sync.WaitGroup
	panicked.Add(1)
	c.dispatch("boom", "", func() {
		defer panicked.Done()
		panic("simulated handler panic")
	})
	waitWithTimeout(t, &panicked, 2*time.Second)

	// The connection's work queue/worker pool must still work: run N more
	// handlers after the panic and confirm every one executes.
	const n = 5
	var ran int32
	var rest sync.WaitGroup
	rest.Add(n)
	for i := 0; i < n; i++ {
		c.dispatch("afterPanic", "", func() {
			defer rest.Done()
			atomic.AddInt32(&ran, 1)
		})
	}
	waitWithTimeout(t, &rest, 2*time.Second)

	require.EqualValues(t, n, atomic.LoadInt32(&ran), "handlers after a panic must still run")
}

// TestDispatchBoundsConcurrencyPerConnection verifies the fixed worker pool
// actually caps in-flight handlers at workerPoolSize
// (== maxConcurrentHandlersPerConn): a burst of far more than that many
// blocking handlers never has more than the cap running at once. Unlike the
// old blocking semaphore, dispatch itself never blocks the caller now — the
// burst is fired from many goroutines (not the connection's reader) purely
// so a burst larger than workQueueDepth+workerPoolSize can still be
// delivered without any one dispatch call being dropped as overload.
func TestDispatchBoundsConcurrencyPerConnection(t *testing.T) {
	t.Parallel()

	c := newClient(newHub(), nil)
	release := make(chan struct{})
	releaseOnce := sync.Once{}
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	const burst = maxConcurrentHandlersPerConn * 4
	var producers sync.WaitGroup
	t.Cleanup(func() {
		unblock()
		producers.Wait()
		close(c.workQueue)
	})
	var inFlight int32
	var maxObserved int32
	var started sync.WaitGroup
	started.Add(burst)
	poolStarted := make(chan struct{}, workerPoolSize)

	for i := 0; i < burst; i++ {
		producers.Add(1)
		go func() {
			defer producers.Done()
			c.dispatch("slow", "", func() {
				n := atomic.AddInt32(&inFlight, 1)
				for {
					old := atomic.LoadInt32(&maxObserved)
					if n <= old || atomic.CompareAndSwapInt32(&maxObserved, old, n) {
						break
					}
				}
				started.Done()
				if n <= workerPoolSize {
					select {
					case poolStarted <- struct{}{}:
					default:
					}
				}
				<-release
				atomic.AddInt32(&inFlight, -1)
			})
		}()
	}

	// Wait for every worker to be executing a blocked handler before making
	// the concurrency-bound assertion; a timer alone can observe startup lag.
	startupTimer := time.NewTimer(5 * time.Second)
	defer startupTimer.Stop()
	for i := 0; i < workerPoolSize; i++ {
		select {
		case <-poolStarted:
		case <-startupTimer.C:
			t.Fatal("timed out waiting for dispatched handlers to finish")
		}
	}
	require.Equal(t, int32(workerPoolSize), atomic.LoadInt32(&maxObserved),
		"the worker pool must be saturated before asserting its concurrency bound")
	require.LessOrEqual(t, atomic.LoadInt32(&maxObserved), int32(workerPoolSize),
		"in-flight handlers for one connection must never exceed the per-connection worker pool size")

	unblock()
	waitWithTimeout(t, &started, 5*time.Second)
}

// TestDispatchQueueFullRepliesOverloadWithoutBlockingCaller is the direct
// unit-level counterpart of the #612 fix: once workerPoolSize handlers are
// running AND workQueueDepth more are already queued behind them, a further
// dispatch call must not block its caller — admission is a non-blocking
// select, and the excess item is dropped with an EventError overload reply
// instead. This is what makes it safe for dispatch to be called synchronously
// from readPump's read loop (see TestReadPumpCancelBypassesQueuedWorkFrame
// for the full end-to-end version going through the actual socket).
func TestDispatchQueueFullRepliesOverloadWithoutBlockingCaller(t *testing.T) {
	t.Parallel()

	c := newClient(newHub(), nil)
	t.Cleanup(func() { close(c.workQueue) })
	c.send = make(chan []byte, workQueueDepth+workerPoolSize+256)
	warmUpWorkerPool(t, c)

	release := make(chan struct{})
	defer close(release)

	overflowRaw := saturateUntilSteadyOverload(t, c, release)

	var overflowMsg WSMessage
	require.NoError(t, json.Unmarshal(overflowRaw, &overflowMsg))
	require.Equal(t, EventError, overflowMsg.Type, "an item that doesn't fit must be rejected with an overload EventError, not silently admitted")
	require.Equal(t, "filler-id", overflowMsg.ID)

	// Confirm the SAME non-blocking behavior for one more probe dispatch,
	// this time asserting explicitly that the caller (this goroutine) is
	// never blocked acquiring a slot — the actual #612 property under test.
	done := make(chan struct{})
	go func() {
		c.dispatch("overflow", "overflow-id", func() {})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dispatch blocked its caller once the queue was full — admission must always be non-blocking")
	}

	var seen []string
	ok := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && !ok; {
		select {
		case raw := <-c.send:
			seen = append(seen, string(raw))
			var m WSMessage
			if err := json.Unmarshal(raw, &m); err == nil && m.ID == "overflow-id" && m.Type == EventError {
				ok = true
			}
		case <-time.After(10 * time.Millisecond):
		}
	}
	require.Truef(t, ok, "overflowing dispatch must reply with an overload EventError (frames seen on c.send while waiting: %v)", seen)
}

// TestDispatchControlBypassesWorkSemaphore is the regression test for the
// bug fixed here: with the work queue's slots (workerPoolSize running +
// workQueueDepth queued) already occupied/held open on one Client
// (simulating long-running agent turns in flight, exactly like
// TestDispatchBoundsConcurrencyPerConnection does), a control-plane-shaped
// dispatch (dispatchControl, which is what CmdCancelAgent/CmdInterruptAndSend
// now go through instead of dispatch) must still complete promptly instead
// of queuing behind those slots.
//
// Before the fix, dispatchControl didn't exist and control-plane commands
// were routed through dispatch/c.sem — this test would time out waiting for
// "control" to run, exactly reproducing the readPump starvation scenario
// described in the bug report (a stuck cancel/interrupt eventually leads to
// a 60s i/o-timeout connection teardown because readPump never calls
// ReadMessage() again while blocked acquiring a semaphore slot).
func TestDispatchControlBypassesWorkSemaphore(t *testing.T) {
	t.Parallel()

	c := newClient(newHub(), nil)
	t.Cleanup(func() { close(c.workQueue) })

	// Saturate the WORK queue with workerPoolSize handlers that block until
	// the test releases them.
	release := make(chan struct{})
	defer close(release)
	var blocked sync.WaitGroup
	blocked.Add(workerPoolSize)
	for i := 0; i < workerPoolSize; i++ {
		c.dispatch("slowWork", "", func() {
			blocked.Done()
			<-release
		})
	}
	// Wait for all of them to actually be running (i.e. the work queue's
	// worker pool is fully saturated) before exercising the control-plane path.
	waitWithTimeout(t, &blocked, 2*time.Second)

	// Now dispatch a control-plane-shaped command. If it shared the work
	// queue/pool with the workerPoolSize blocked handlers above, this would
	// hang until release fires.
	var controlRan sync.WaitGroup
	controlRan.Add(1)
	start := time.Now()
	c.dispatchControl("handleCancelAgent", "", func() {
		defer controlRan.Done()
	})
	waitWithTimeout(t, &controlRan, time.Second)
	elapsed := time.Since(start)

	require.Less(t, elapsed, time.Second,
		"control-plane dispatch must not queue behind saturated work handlers")
}

// TestDispatchControlDoesNotStarveWorkQueue proves the fix doesn't cut both
// ways: dispatching control-plane commands (even many of them) must never
// let them interfere with, or starve, the legitimate bounded-concurrency
// applied to long-running work handlers. Concretely: saturating the work
// queue's running slots still leaves the queue's bounded-admission semantics
// intact regardless of how many control-plane dispatches ran concurrently —
// a dispatch call made once BOTH the running slots and the queue slots are
// full is rejected with an overload reply rather than silently blocking or
// silently being admitted past the cap.
func TestDispatchControlDoesNotStarveWorkQueue(t *testing.T) {
	t.Parallel()

	c := newClient(newHub(), nil)
	c.send = make(chan []byte, workQueueDepth+workerPoolSize+256)
	warmUpWorkerPool(t, c)
	release := make(chan struct{})
	releaseOnce := sync.Once{}
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var producers sync.WaitGroup
	t.Cleanup(func() {
		unblock()
		producers.Wait()
		close(c.workQueue)
	})

	// Saturate the queue+pool to steady-state overload — see
	// saturateUntilSteadyOverload's doc for why a single overload reply
	// isn't a reliable saturation signal under -race scheduling variance.
	saturateUntilSteadyOverload(t, c, release)

	// Fire a burst of control-plane dispatches concurrently with the
	// saturated work queue — none of this should affect the work queue's
	// admission state.
	const controlBurst = 50
	var controlDone sync.WaitGroup
	controlDone.Add(controlBurst)
	for i := 0; i < controlBurst; i++ {
		c.dispatchControl("handleCancelAgent", "", func() {
			controlDone.Done()
		})
	}
	waitWithTimeout(t, &controlDone, 2*time.Second)

	// The work queue is still fully saturated (nothing above can have freed
	// a slot: every admitted filler blocks on <-release, and control-plane
	// dispatches never touch the work queue at all). One more work-shaped
	// dispatch, sent now, must be rejected with an overload reply rather
	// than blocking the caller or silently exceeding the cap.
	done := make(chan struct{})
	producers.Add(1)
	go func() {
		defer producers.Done()
		c.dispatch("oneMoreWork", "one-more-id", func() {})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch blocked its caller instead of rejecting once the queue was full — admission must always be non-blocking")
	}

	require.Eventually(t, func() bool {
		select {
		case raw := <-c.send:
			var m WSMessage
			if err := json.Unmarshal(raw, &m); err == nil && m.ID == "one-more-id" && m.Type == EventError {
				return true
			}
		default:
		}
		return false
	}, time.Second, 10*time.Millisecond, "work dispatch sent while saturated must be rejected with overload, not silently admitted")

	// Release the held handlers so the pool can drain.
	unblock()
}

// saturateUntilSteadyOverload dispatches "filler" items (name "filler",
// msgID "filler-id") against c, one at a time, each blocking on <-release,
// until it observes a run of steadyOverloadStreak consecutive overload
// replies in a row with no successful admission in between. It returns the
// raw bytes of the LAST overload reply in that streak.
//
// Why a streak, not just the first overload: under heavy -race scheduling
// (many parallel tests contending for OS threads), a single early overload
// reply can be a false signal that the pool/queue aren't actually
// warmed up yet — even after warmUpWorkerPool's two-wave barrier, a probe
// dispatch sent immediately after finding "the" overload has repeatedly
// been observed (in this file's own iteration history — see the #612
// test-fix notes) to be admitted into a queue that still had free slots,
// proving the earlier overload was transient, not steady-state saturation.
// Requiring several overloads in an unbroken row is a much stronger signal:
// once genuinely saturated (workerPoolSize running on <-release forever +
// workQueueDepth queued), EVERY subsequent dispatch must overload, forever,
// until release closes — so a streak can only end by transitioning to
// "still not saturated, one more got admitted", never by chance.
// dispatchBounded calls c.dispatch and fails the test if it does not return
// promptly, instead of letting a blocking admission hang the whole package.
//
// This exists because of how a regression here actually presents. Reverting
// dispatch's select/default back to a plain blocking send makes the very
// first filler that fills the queue block THIS goroutine forever, inside
// saturateUntilSteadyOverload's loop -- before the caller ever reaches its
// own bounded probe further down. Measured: `go test ./internal/server/`
// then ran until the 10-minute package timeout and reported
// `panic: test timed out after 10m0s` with the goroutine parked in
// (*Client).dispatch on a chan send. In CI that is a stuck job for the full
// timeout, not a red test -- the same failure shape that cost this project
// ten minutes per run when an unrelated pair of fixes interacted earlier in
// this series. A regression should say what broke, immediately.
//
// The goroutine is deliberately not waited on when it does block: it stays
// parked on the channel send until release closes, which is fine in an
// already-failing test and is the only way to report the hang at all.
func dispatchBounded(t *testing.T, c *Client, name, msgID string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		c.dispatch(name, msgID, fn)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("dispatch(%q) blocked its caller: admission must be non-blocking so readPump is never trapped behind in-flight work (#612)", name)
	}
}

func saturateUntilSteadyOverload(t *testing.T, c *Client, release chan struct{}) []byte {
	t.Helper()
	const steadyOverloadStreak = 8

	streak := 0
	var lastOverload []byte
	const maxAttempts = (workerPoolSize + workQueueDepth) * 3
	for i := 0; i < maxAttempts; i++ {
		dispatchBounded(t, c, "filler", "filler-id", func() { <-release })
		select {
		case raw := <-c.send:
			var m WSMessage
			if json.Unmarshal(raw, &m) == nil && m.Type == EventError {
				streak++
				lastOverload = raw
				if streak >= steadyOverloadStreak {
					return lastOverload
				}
				continue
			}
			// Got a non-overload frame unexpectedly (shouldn't happen for
			// "filler" dispatches) — treat as a broken streak, same as an
			// admission, and keep going.
			streak = 0
		default:
			// Admitted (no reply at all) — resets the streak; the pool/queue
			// had a free slot this time, so saturation isn't steady yet.
			streak = 0
		}
	}
	t.Fatalf("never observed %d consecutive overload replies within %d filler dispatches — "+
		"queue+pool did not reach steady saturation", steadyOverloadStreak, maxAttempts)
	return nil
}

// warmUpWorkerPool blocks until all workerPoolSize worker goroutines for c
// (started lazily by newClient/startWorkers) have actually begun running —
// i.e. each has pulled at least one item off c.workQueue and started
// executing it. Without this, a test that immediately fires a tight,
// synchronous burst of workerPoolSize+workQueueDepth dispatch calls can
// outrun goroutine startup: if none of the workerPoolSize workers have been
// scheduled by the Go runtime yet, the queue's workQueueDepth buffer alone
// isn't enough room for `total` items and dispatch's overload branch fires
// during what was meant to be pure saturation setup — flaky under -race,
// where goroutine scheduling latency is highest. Calling this first pins
// down "the pool is alive and ready to drain concurrently" as a precondition
// instead of an assumption.
func warmUpWorkerPool(t *testing.T, c *Client) {
	t.Helper()
	// Two waves, not one: after the first wave's workerPoolSize items each
	// signal Done() and return, there is a brief window before every worker
	// goroutine has actually looped back to `range c.workQueue` and is ready
	// to receive again — a caller proceeding immediately after only the
	// first wave can race ahead of that and observe fewer than
	// workerPoolSize workers truly ready, undercounting how much of a
	// saturating burst gets admitted before overload (found via repeated
	// -race runs: a "queue full" reply for an early filler item was
	// observed while len(c.workQueue) was still well under workQueueDepth,
	// meaning saturation was declared before the pool had actually caught
	// up). A second wave of exactly workerPoolSize items can only have ALL
	// of its items start running if every one of the workerPoolSize workers
	// independently reset and picked one up — that's the actual proof the
	// pool is fully ready, not just "started once".
	for wave := 0; wave < 2; wave++ {
		var started sync.WaitGroup
		started.Add(workerPoolSize)
		for i := 0; i < workerPoolSize; i++ {
			c.dispatch("warmup", "", func() { started.Done() })
		}
		waitWithTimeout(t, &started, 5*time.Second)
	}
}

// waitWithTimeout fails the test instead of hanging forever if wg never
// completes (e.g. a regression reintroduces a deadlock in dispatch).
func waitWithTimeout(t *testing.T, wg *sync.WaitGroup, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for dispatched handlers to finish")
	}
}

// syncBuffer is a concurrency-safe bytes.Buffer, needed because dispatch logs
// from a background goroutine while the test reads the buffer concurrently.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
