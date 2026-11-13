package agent

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestFinalizationSettleNilSettlerFailsClosed(t *testing.T) {
	f := newFinalizationWaitFixture(t)
	f.facts.set(func(x *finalizationFacts) { x.debt = 1 })
	hook := f.hook(func(d *FinalizationWaitDeps) { d.Settler = nil })
	err := hook(t.Context(), f.ts)
	require.ErrorIs(t, err, ErrFinalizationSettlerUnavailable)
	record, err := f.env.scope.store.Snapshot(t.Context(), f.env.scope.identity)
	require.NoError(t, err)
	require.Equal(t, "executing", record.Phase, "an unsettled debt must not seal")
}

func TestFinalizationSettleReceivesOwnerSessionID(t *testing.T) {
	f := newFinalizationWaitFixture(t)
	f.facts.set(func(x *finalizationFacts) { x.debt = 1 })
	settler := &finalizationSettlerStub{onCall: func() {
		f.facts.set(func(x *finalizationFacts) { x.debt = 0 })
	}}
	hook := f.hook(func(d *FinalizationWaitDeps) {
		d.Compose = f.composeStub(t)
		d.Settler = settler
	})
	require.NoError(t, hook(t.Context(), f.ts))
	settler.mu.Lock()
	defer settler.mu.Unlock()
	require.Equal(t, []string{f.env.execution.sessionID}, settler.owners)
	require.Equal(t, "session", settler.owners[0])
}

func TestFinalizationSettleErrorDoesNotAbortTheLoop(t *testing.T) {
	f := newFinalizationWaitFixture(t)
	// An open admission makes the facts non-quiet, so the loop enters the
	// wait branch and settles, even though the debt counter is zero.
	release, err := f.env.execution.gate.admitMutation(false)
	require.NoError(t, err)
	settler := &finalizationSettlerStub{err: errors.New("settler down")}
	var entered sync.Once
	enteredWait := make(chan struct{})
	hook := f.hook(func(d *FinalizationWaitDeps) {
		d.Compose = f.composeStub(t)
		d.Settler = settler
		d.PollInterval = time.Millisecond
		d.OnState = func(st FinalizationWaitStatus) {
			if st.Phase == FinalizationPhaseWaiting {
				entered.Do(func() { close(enteredWait) })
			}
		}
	})
	go func() {
		<-enteredWait
		release()
	}()
	require.NoError(t, hook(t.Context(), f.ts))
	recordAccepted(t, f)
	require.GreaterOrEqual(t, settler.count(), 1)
	var _ session.AgentFinalizationDebtSettler = settler
}
