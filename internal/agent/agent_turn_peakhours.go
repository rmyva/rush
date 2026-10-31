package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
)

// peakHoursWatcher polls a.peakHoursCheck for the duration of a turn and
// aborts it the moment the provider enters its peak-hours window. Split out
// of agent_turn.go's runTurn when the 1000-line limit landed. Like
// turnCheckpointWriter, it owns its own state outright (the abort-error box)
// and only READS the two things it shares with the rest of runTurn --
// sessionLock and the current assistant message -- never owns them.
//
// abortErr is stashed by whichever check (this watcher's background poll,
// or runTurn's own once-per-step re-check inside OnStepFinish -- both call
// setAbortErr) first detects the provider entered its window mid-turn. The
// checks must call the turn's cancel func to break fantasy's agent loop
// (returning an error alone doesn't stop it), but that makes fantasy return
// context.Canceled -- swallowing the specific *PeakHoursError. After
// agent.Stream() returns, runTurn's tail (via normalizeTurnError calling
// getAbortErr) replaces the generic context.Canceled with the real error so
// it reaches the coordinator and ultimately RunNonInteractive's stderr
// output.
type peakHoursWatcher struct {
	a          *sessionAgent
	sessionID  string
	providerID string
	ctx        context.Context
	genCtx     context.Context

	// sessionLock and assistant are runTurn's own -- shared with every other
	// streaming callback, never owned here. See turnCheckpointWriter's doc
	// for why a pointer-to-pointer keeps read semantics identical to a
	// closure capturing the local directly.
	sessionLock *sync.Mutex
	assistant   **message.Message

	// bypass is the call's --allow-peak-hours: the window was waived for
	// this call, so the mid-turn poll must not enforce it either.
	bypass bool

	mu       sync.Mutex
	abortErr error
}

// peakHoursBypassed reports whether the call waived the peak-hours window.
func peakHoursBypassed(call SessionAgentCall) bool {
	return call.CallOptions != nil && call.CallOptions.AllowPeakHours
}

func newPeakHoursWatcher(a *sessionAgent, sessionID, providerID string, ctx, genCtx context.Context, sessionLock *sync.Mutex, assistant **message.Message) *peakHoursWatcher {
	return &peakHoursWatcher{a: a, sessionID: sessionID, providerID: providerID, ctx: ctx, genCtx: genCtx, sessionLock: sessionLock, assistant: assistant}
}

func (w *peakHoursWatcher) setAbortErr(err error) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.abortErr != nil {
		return false
	}
	w.abortErr = err
	return true
}

func (w *peakHoursWatcher) getAbortErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.abortErr
}

// start launches the background poll goroutine when a.peakHoursCheck is
// configured and returns a channel that closes when the watcher exits --
// either because it fired and aborted the turn, or because genCtx was
// cancelled. When no check is configured the channel is already closed, so
// the caller can treat the return value as "done when readable" either way
// without a separate nil branch.
func (w *peakHoursWatcher) start() <-chan struct{} {
	done := make(chan struct{})
	if w.a.peakHoursCheck == nil || w.bypass {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(peakHoursPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-w.genCtx.Done():
				return
			case <-ticker.C:
				pErr := w.a.peakHoursCheck(w.providerID)
				if pErr == nil {
					continue
				}
				if !w.setAbortErr(pErr) {
					return
				}
				slog.Warn("agent: aborting — provider entered peak-hours mid-turn",
					"session_id", w.sessionID, "error", pErr)
				peakMsg, peakDetails := peakHoursStoppedFinishText(pErr)
				w.sessionLock.Lock()
				var snap message.Message
				haveSnap := *w.assistant != nil
				if haveSnap {
					(*w.assistant).AddFinish(message.FinishReasonError, peakMsg, peakDetails)
					snap = (*w.assistant).Clone()
				}
				w.sessionLock.Unlock()
				if haveSnap {
					flushCtx, flushCancel := context.WithTimeout(context.WithoutCancel(w.ctx), 15*time.Second)
					if uErr := w.a.messages.Update(flushCtx, snap); uErr != nil {
						slog.Warn("agent: failed to persist peak-hours finish message", "error", uErr)
					}
					flushCancel()
				}
				if cancelFn, ok := w.a.activeRequests.Get(w.sessionID); ok {
					cancelFn()
				}
				return
			}
		}
	}()
	return done
}
