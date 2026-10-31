package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// fakePeakHoursMsgSvc implements message.Service via a nil embedded
// interface, overriding only Update -- the sole method the watcher calls.
// Any other method would nil-panic if the watcher ever called it, which is
// the point: it fails loudly rather than silently accepting a call this
// test never expected.
type fakePeakHoursMsgSvc struct {
	message.Service
	mu          sync.Mutex
	updateCount int
	lastUpdated message.Message
}

func (m *fakePeakHoursMsgSvc) Update(_ context.Context, msg message.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateCount++
	m.lastUpdated = msg
	return nil
}

// No existing test exercised peakHoursWatcher's background poll goroutine
// end-to-end before this split -- the other peak-hours tests cover the
// coordinator-level start-of-turn gate, not the mid-turn watcher that used
// to live as an anonymous goroutine inside runTurn. Naming it as its own
// type is what makes it independently testable at all; this closes that
// gap rather than just relying on it.
func TestPeakHoursWatcherAbortsTurnAndPersistsFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		msgSvc := &fakePeakHoursMsgSvc{}
		checkErr := &PeakHoursError{ProviderID: "acme", Start: "22:00", End: "06:00", ReopensAt: time.Now().Add(8 * time.Hour)}

		checkCalls := 0
		checkedProvider := make(chan string, 1)
		a := &sessionAgent{
			messages:       msgSvc,
			activeRequests: csync.NewMap[string, context.CancelFunc](),
			peakHoursCheck: func(providerID string) error {
				checkCalls++
				checkedProvider <- providerID
				return checkErr
			},
		}

		cancelCalled := false
		a.activeRequests.Set("sess-1", func() { cancelCalled = true })

		var sessionLock sync.Mutex
		currentAssistant := &message.Message{ID: "msg-1", SessionID: "sess-1", Role: message.Assistant}

		genCtx, genCancel := context.WithCancel(t.Context())
		defer genCancel()

		w := newPeakHoursWatcher(a, "sess-1", "pinned-provider", t.Context(), genCtx, &sessionLock, &currentAssistant)
		done := w.start()

		// peakHoursPollInterval is 10s; synctest.Wait advances virtual time
		// to the next durable block, so this returns once the watcher's
		// ticker has fired and the watcher has run to completion (it
		// returns after its first non-nil check, win or lose).
		synctest.Wait()
		<-done

		require.Equal(t, 1, checkCalls, "watcher must stop polling after the first abort, not keep firing")
		require.Equal(t, "pinned-provider", <-checkedProvider, "watcher must check the turn's pinned provider")
		require.True(t, cancelCalled, "watcher must cancel the turn via activeRequests once it detects the window")
		require.ErrorIs(t, w.getAbortErr(), checkErr, "getAbortErr must return the exact error the check produced")

		sessionLock.Lock()
		finishRecorded := len(currentAssistant.Parts) > 0
		sessionLock.Unlock()
		require.True(t, finishRecorded, "watcher must record a Finish part on the assistant message before persisting")

		require.Equal(t, 1, msgSvc.updateCount, "watcher must persist exactly one flush of the finish state")
	})
}

// TestPeakHoursWatcherSecondSetAbortErrLoses verifies the same fencing
// runTurn's own once-per-step re-check relies on: whichever caller sets the
// abort error FIRST wins, and a second caller (the OnStepFinish inline
// check, in production) sees setAbortErr return false and must not
// re-trigger the abort/persist/cancel sequence.
func TestPeakHoursWatcherSecondSetAbortErrLoses(t *testing.T) {
	a := &sessionAgent{activeRequests: csync.NewMap[string, context.CancelFunc]()}
	var sessionLock sync.Mutex
	currentAssistant := &message.Message{ID: "msg-1"}
	w := newPeakHoursWatcher(a, "sess-1", "pinned-provider", context.Background(), context.Background(), &sessionLock, &currentAssistant)

	first := &PeakHoursError{ProviderID: "first"}
	second := &PeakHoursError{ProviderID: "second"}

	require.True(t, w.setAbortErr(first), "the first caller to detect the window must win the fence")
	require.False(t, w.setAbortErr(second), "a second caller must lose the fence and not overwrite the recorded error")
	require.ErrorIs(t, w.getAbortErr(), first, "the recorded error must stay the first one, never a later caller's")
}

// TestPeakHoursWatcherStartWithNoCheckClosesImmediately verifies the nil-check
// fast path: when a.peakHoursCheck is nil (peak-hours disabled for this
// agent), start() must return an already-closed channel rather than block
// runTurn's own defer forever.
func TestPeakHoursWatcherStartWithNoCheckClosesImmediately(t *testing.T) {
	a := &sessionAgent{}
	var sessionLock sync.Mutex
	currentAssistant := &message.Message{}
	w := newPeakHoursWatcher(a, "sess-1", "pinned-provider", context.Background(), context.Background(), &sessionLock, &currentAssistant)

	done := w.start()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("start() with a nil peakHoursCheck must return an already-closed channel")
	}
}

func TestPeakHoursStepCheckUsesPinnedProvider(t *testing.T) {
	var gotProvider string
	a := &sessionAgent{peakHoursCheck: func(providerID string) error {
		gotProvider = providerID
		return nil
	}}
	ts := &turnStream{
		a:          a,
		call:       SessionAgentCall{SessionID: "sess-1"},
		smartModel: Model{ModelCfg: config.SelectedModel{Provider: "pinned-provider"}},
	}
	ts.setPeakHoursAbortErr = func(error) bool { return true }

	require.NoError(t, ts.recheckPeakHours())
	require.Equal(t, "pinned-provider", gotProvider)
}

// A call made with --allow-peak-hours waived the window at turn start; the
// mid-turn poll and the per-step re-check must not enforce it afterwards,
// or the turn is aborted right after its first step.
func TestPeakHoursMidTurnChecksHonorCallBypass(t *testing.T) {
	var checks atomic.Int32
	checkedProvider := make(chan string, 1)
	a := &sessionAgent{peakHoursCheck: func(providerID string) error {
		checks.Add(1)
		checkedProvider <- providerID
		return &PeakHoursError{ProviderID: "in-window"}
	}}
	call := SessionAgentCall{SessionID: "sess-1", CallOptions: &CallOptions{AllowPeakHours: true}}

	var sessionLock sync.Mutex
	currentAssistant := &message.Message{}
	w := newPeakHoursWatcher(a, call.SessionID, "pinned-provider", context.Background(), context.Background(), &sessionLock, &currentAssistant)
	w.bypass = peakHoursBypassed(call)
	select {
	case <-w.start():
	case <-time.After(time.Second):
		t.Fatal("a bypassed call must not start the peak-hours poll")
	}

	ts := &turnStream{a: a, call: call, smartModel: Model{ModelCfg: config.SelectedModel{Provider: "pinned-provider"}}}
	require.NoError(t, ts.recheckPeakHours())
	require.Zero(t, checks.Load(), "the in-window check must not run for a bypassed call")
	require.Empty(t, checkedProvider, "bypass must not invoke the check callback")

	require.False(t, peakHoursBypassed(SessionAgentCall{}), "no call options: no bypass")
}
