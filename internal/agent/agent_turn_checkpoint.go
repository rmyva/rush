package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
)

// turnCheckpointWriter is runTurn's mid-stream persistence mechanism (Fork
// patch: batch 8 — auto-checkpoint state. See CHANGELOG.fork.md section 6).
// Split out of agent_turn.go's runTurn when the 1000-line limit landed:
// unlike runTurn's other callbacks, this one owns a genuinely self-contained
// slice of state (its own generation counter, stop/done channels and write
// cancel) and only READS the two things runTurn shares with it — sessionLock
// and the current assistant message — never owns them.
//
// Invariant: sessionLock protects EVERY touch of the current assistant
// message — mutation, Clone(), and even a bare len(Parts)/pointer read —
// because this writer's goroutine and the streaming callbacks (OnTextDelta,
// OnReasoningDelta, OnToolInputStart, ...) run concurrently on separate
// goroutines. message.Message.Clone() has no synchronization of its own, so
// a snapshot must be taken while holding sessionLock.
//
// The lock must NEVER be held across messages.Update (the SQLite write):
// the writer takes the lock, clones a private snapshot, releases the lock,
// then calls Update on the snapshot without the lock held. Otherwise each
// checkpoint tick would stall the whole streaming loop for the duration of
// a disk write. OnStepFinish drains the ticker and stops the goroutine
// (via stop()) before its final write; runTurn's own tail also calls
// stop() defensively before touching the assistant message, in case
// agent.Stream returned before OnStepFinish ever ran (e.g. the very first
// provider call failed).
//
// generation fences concurrent checkpoint writes across turns: each start()
// increments it, the goroutine captures the current value at launch, and
// stop() returns only after observing the goroutine's done signal. This
// ensures a hung checkpoint from turn N cannot race with a new checkpoint
// from turn N+1, and that stop()'s 5s timeout is reflected in the agent's
// runWg (see the P0-4 fix in start()'s write-context comment). mu
// synchronizes all access to generation; there is no shared
// checkpointPartsLen — each generation tracks its own lastPartsLen locally.
//
// stopCh and doneCh are reborn on every step. start() allocates a fresh
// pair and launches the ticker goroutine; stop() closes stopCh — the
// goroutine's dedicated exit signal — then waits on doneCh for it to
// actually exit, then nils both so the next step starts clean. The exit
// signal MUST be a dedicated channel, NOT genCtx.Done(): genCtx stays alive
// for the whole body of Run (cancelled only by the deferred cancel() at
// runTurn's return), so relying on it would force stop() to always hit its
// 5s backstop — the ~10s/turn stall this mechanism replaces. start/stop run
// on fantasy's single callback goroutine / the Run goroutine (never
// concurrent with each other); the ticker goroutine captures local channel
// refs at launch, so nil-ing the outer fields after stop does not affect
// it.
type turnCheckpointWriter struct {
	a         *sessionAgent
	sessionID string
	genCtx    context.Context

	// sessionLock and assistant are runTurn's own -- shared with every other
	// streaming callback, never owned here. assistant is a pointer to
	// runTurn's currentAssistant local so every read sees the latest value
	// PrepareStep/the streaming callbacks assign, exactly as a closure
	// capturing that local directly would.
	sessionLock *sync.Mutex
	assistant   **message.Message

	// stall is the turn's stall clock; each successful changed checkpoint
	// persist is a durable progress point.
	stall *stallClock

	mu          sync.Mutex
	generation  int64
	stopCh      chan struct{}
	doneCh      chan struct{}
	writeCancel context.CancelFunc
}

func newTurnCheckpointWriter(a *sessionAgent, sessionID string, genCtx context.Context, sessionLock *sync.Mutex, assistant **message.Message, stall *stallClock) *turnCheckpointWriter {
	return &turnCheckpointWriter{
		a:           a,
		sessionID:   sessionID,
		genCtx:      genCtx,
		sessionLock: sessionLock,
		assistant:   assistant,
		stall:       stall,
	}
}

func (cp *turnCheckpointWriter) start() {
	if cp.a.checkpointInterval <= 0 || cp.stopCh != nil {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	cp.stopCh = stop
	cp.doneCh = done
	// Fence the checkpoint writer so a hung write from turn N
	// cannot race with a new writer from turn N+1. The goroutine
	// captures the current generation at launch; stop() returns
	// only after observing the goroutine's done signal.
	cp.mu.Lock()
	cp.generation++
	myGeneration := cp.generation
	cp.mu.Unlock()
	// The generation context is deliberately cancellable by stop(), but has no
	// per-write deadline: each Update gets a fresh 30s budget from this parent.
	writeCtxParent, writeCancel := context.WithCancel(context.Background())
	cp.writeCancel = writeCancel
	// Register in runWg so a timeout reflects in stillBusy (P0-4).
	cp.a.runWg.Add(1)
	go func() {
		defer cp.a.runWg.Done()
		defer close(done)
		defer writeCancel()
		ticker := time.NewTicker(cp.a.checkpointInterval)
		defer ticker.Stop()
		// Keep coalescing state LOCAL to this generation to eliminate
		// cross-generation races. Each writer tracks its own lastPartsLen and
		// only writes if there's new content since its last write.
		lastPartsLen := 0
		lastContentBytes := 0
		for {
			select {
			case <-stop:
				return
			case <-cp.genCtx.Done():
				return
			case <-ticker.C:
				// stop() closes `stop` BEFORE cancelling writeCtx
				// (see its comment), so a tick that was already queued
				// while the previous write was blocked can become ready
				// in the SAME instant `stop` does — select's case order
				// above gives no priority, so it can pick ticker.C over
				// stop by chance. Re-checking non-blockingly here closes
				// that window: a write must never be attempted with a
				// writeCtx that stop() may have already
				// cancelled, since that write's failure would be
				// indistinguishable from any other cancellation to the
				// caller but still represents wasted, racy work the
				// goroutine was explicitly told to stop before reaching.
				select {
				case <-stop:
					return
				case <-cp.genCtx.Done():
					return
				default:
				}
				cp.sessionLock.Lock()
				var snap message.Message
				haveSnap := false
				var currentPartsLen int
				var currentContentBytes int
				cp.mu.Lock()
				isCurrentGen := myGeneration == cp.generation
				cp.mu.Unlock()
				if *cp.assistant != nil && isCurrentGen {
					currentPartsLen = len((*cp.assistant).Parts)
					currentContentBytes = checkpointContentBytes((*cp.assistant).Parts)
					// Text and reasoning deltas grow existing parts without changing
					// the part count, so length alone misses streamed content.
					if currentContentBytes != lastContentBytes || currentPartsLen != lastPartsLen {
						snap = (*cp.assistant).Clone()
						// Stamp the snapshot with THIS writer's generation
						// so the conditional update can reject it if a
						// newer writer already wrote (P1-3). The isCurrentGen
						// check above is not enough on its own: it runs
						// here, under sessionLock, while the DB write below
						// happens after the lock is released.
						snap.CheckpointGeneration = myGeneration
						snap.AddFinish(message.FinishReasonUnknown, "", "")
						for i := len(snap.Parts) - 1; i >= 0; i-- {
							if f, ok := snap.Parts[i].(message.Finish); ok {
								f.Partial = true
								snap.Parts[i] = f
								break
							}
						}
						haveSnap = true
					}
				}
				cp.sessionLock.Unlock()
				if haveSnap {
					// Each checkpoint attempt gets a fresh bounded budget, while
					// remaining cancellable by stop() through the parent.
					updateCtx, updateCancel := context.WithTimeout(writeCtxParent, 30*time.Second)
					// P0-4: stop() cancels writeCtxParent immediately to fence an
					// in-flight DB write.
					err := cp.a.messages.Update(updateCtx, snap)
					updateCancel()
					if err != nil {
						// Don't log cancelled errors as failures — they're
						// the expected outcome of stop() fencing.
						if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
							slog.Debug(
								"agent: checkpoint flush failed",
								"session_id", cp.sessionID,
								"message_id", snap.ID,
								"err", err,
							)
						}
					} else {
						// Update our local coalescing state and stall clock only
						// after the changed snapshot has been durably written.
						lastPartsLen = currentPartsLen
						lastContentBytes = currentContentBytes
						cp.stall.markProgress()
					}
				}
			}
		}
	}()
}

func checkpointContentBytes(parts []message.ContentPart) int {
	var size int
	for _, part := range parts {
		switch content := part.(type) {
		case message.TextContent:
			size += len(content.Text)
		case message.ReasoningContent:
			size += len(content.Thinking)
		}
	}
	return size
}

func (cp *turnCheckpointWriter) stop() {
	if cp.stopCh == nil {
		return
	}
	close(cp.stopCh)
	cp.stopCh = nil
	// Cancel the write's own context immediately rather than waiting out
	// the 5s grace below first (P0-4). A checkpoint write in flight when
	// stop is requested has nothing left to accomplish — the turn is
	// ending — so there is no reason to let it keep holding a DB
	// connection for up to 5 more seconds (or longer, unbounded, if the
	// underlying driver never itself times out) before this function
	// even starts waiting. If the goroutine is between ticks (not
	// writing), cancelling here is a harmless no-op; the ticker loop's
	// own <-stop case still handles the ordinary shutdown path.
	if cp.writeCancel != nil {
		cp.writeCancel()
		cp.writeCancel = nil
	}
	select {
	case <-cp.doneCh:
	case <-time.After(5 * time.Second):
		slog.Warn(
			"agent: checkpoint goroutine did not exit within 5s of stop signal",
			"session_id", cp.sessionID,
		)
	}
	cp.doneCh = nil
}
