package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestTurnCheckpointWriterMarksChangedSuccessfulWriteAsProgress(t *testing.T) {
	env := testEnv(t)
	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "checkpoint successful progress accounting")
	require.NoError(t, err)
	msg, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: "assistant"})
	require.NoError(t, err)
	current := &msg
	var lock sync.Mutex
	stall := &stallClock{}
	const baseline = int64(123)
	stall.lastProgress.Store(baseline)
	cp := newTurnCheckpointWriter(&sessionAgent{messages: env.messages, checkpointInterval: 5 * time.Millisecond}, sess.ID, ctx, &lock, &current, stall)
	cp.start()
	defer cp.stop()

	lock.Lock()
	current.AppendContent("persisted progress")
	lock.Unlock()
	require.Eventually(t, func() bool { return stall.lastProgress.Load() != baseline }, time.Second, 5*time.Millisecond,
		"a changed checkpoint should mark progress only after its successful update")

	updated, err := env.messages.Get(ctx, msg.ID)
	require.NoError(t, err)
	require.Equal(t, "persisted progress", updated.FullText(), "successful progress marking must correspond to persisted content")
}

func TestTurnCheckpointWriterDoesNotMarkUnchangedOrFailedWritesAsProgress(t *testing.T) {
	// Revert-check: moving markProgress back to the ticker case makes the
	// unchanged-tick assertion fail; marking after a failed Update makes the
	// second assertion fail.
	env := testEnv(t)
	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "checkpoint progress accounting")
	require.NoError(t, err)
	msg, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: "assistant"})
	require.NoError(t, err)
	current := &msg
	var lock sync.Mutex
	stall := &stallClock{}
	const baseline = int64(123)
	stall.lastProgress.Store(baseline)
	messages := &checkpointFailingMessages{Service: env.messages}
	cp := newTurnCheckpointWriter(&sessionAgent{messages: messages, checkpointInterval: 5 * time.Millisecond}, sess.ID, ctx, &lock, &current, stall)
	cp.start()
	defer cp.stop()

	// An unchanged ticker tick has no snapshot to persist and is not progress.
	time.Sleep(30 * time.Millisecond)
	require.Equal(t, baseline, stall.lastProgress.Load())

	lock.Lock()
	current.AppendContent("not persisted")
	lock.Unlock()
	require.Eventually(t, func() bool { return messages.calls.Load() > 0 }, time.Second, 5*time.Millisecond)
	require.Equal(t, baseline, stall.lastProgress.Load(), "failed checkpoint writes must not extend the stall deadline")
}

type checkpointFailingMessages struct {
	message.Service
	calls atomic.Int64
}

func (m *checkpointFailingMessages) Update(context.Context, message.Message) error {
	m.calls.Add(1)
	return errors.New("injected checkpoint write failure")
}
