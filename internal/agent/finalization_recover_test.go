package agent

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// errFinalizationRecoverReaderInjected is the injected candidate-read failure
// the recovery read-error test drives candidateIntact with.
var errFinalizationRecoverReaderInjected = errors.New("injected finalization candidate read failure")

// finalizationRecoverBrokenReader serves every transactional candidate read
// with one injected failure: the transient fault recovery must survive
// without clearing a good candidate.
type finalizationRecoverBrokenReader struct {
	message.Service
	message.FinalizationReader
	calls atomic.Int32
}

var _ message.FinalizationReader = (*finalizationRecoverBrokenReader)(nil)

func (r *finalizationRecoverBrokenReader) GetFinalizationCandidateTx(_ context.Context, _ *sql.Tx, _ string) (message.Message, error) {
	r.calls.Add(1)
	return message.Message{}, errFinalizationRecoverReaderInjected
}

// TestFinalizationRecoverScopeHookAdoptsIntactCandidate proves a restart
// adopts an intact recorded candidate and resumes the step counter above the
// durable high-water mark.
//
// Revert-check: dropping the recoverFinalizationCandidate call (or the
// resumeCandidateSerial it performs) fails both the verdict and the serial
// assertion.
func TestFinalizationRecoverScopeHookAdoptsIntactCandidate(t *testing.T) {
	env := newFinalizationDriverSeamEnv(t)
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 5}
	m := newFinalizationCandidateMessage(t, env.fakeEnv, id)
	_, err := persistFinalizationCandidate(t.Context(), env.sessions, env.messages, env.scope, id, m)
	require.NoError(t, err)
	hook := NewFinalizationRecoverScopeHook(env.messages)
	verdict, err := hook(env.ctx, env.execution)
	require.NoError(t, err)
	require.Equal(t, session.AgentFinalizationCandidateAdopted, verdict)
	stamp, err := env.execution.gate.beginCandidate(env.ctx, quietFinalizationTestFacts)
	require.NoError(t, err)
	require.EqualValues(t, 6, stamp.serial, "the restarted owner continues above the durable high-water mark")
}

// TestFinalizationRecoverScopeHookFailsStaleCandidate proves a candidate
// whose message vanished is failed and cleared while the serial high-water
// mark survives it, and the restarted counter still lands above that mark.
//
// Revert-check: a hook that reports none (or an error) for the stale
// candidate, or that lets the clearing lower CandidateSerial, fails here.
func TestFinalizationRecoverScopeHookFailsStaleCandidate(t *testing.T) {
	env := newFinalizationDriverSeamEnv(t)
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 7}
	m := newFinalizationCandidateMessage(t, env.fakeEnv, id)
	_, err := persistFinalizationCandidate(t.Context(), env.sessions, env.messages, env.scope, id, m)
	require.NoError(t, err)
	_, err = env.conn.ExecContext(t.Context(), `DELETE FROM messages WHERE id=?`, m.ID)
	require.NoError(t, err)
	hook := NewFinalizationRecoverScopeHook(env.messages)
	verdict, err := hook(env.ctx, env.execution)
	require.NoError(t, err)
	require.Equal(t, session.AgentFinalizationCandidateFailed, verdict)
	record, err := env.scope.store.Snapshot(env.ctx, env.scope.identity)
	require.NoError(t, err)
	require.Empty(t, record.CandidateMessageID, "the stale candidate is cleared")
	require.Empty(t, record.CandidateHash)
	require.EqualValues(t, 7, record.CandidateSerial, "the serial high-water mark survives the clearing")
	stamp, err := env.execution.gate.beginCandidate(env.ctx, quietFinalizationTestFacts)
	require.NoError(t, err)
	require.EqualValues(t, 8, stamp.serial)
	require.GreaterOrEqual(t, stamp.serial, uint64(record.CandidateSerial))
}

// TestFinalizationRecoverScopeHookReportsNoneWithoutCandidate proves an
// active record without a recorded candidate recovers as none and starts
// counting from one.
//
// Revert-check: a hook that invents adopted or failed verdicts for the empty
// record fails the verdict assertion.
func TestFinalizationRecoverScopeHookReportsNoneWithoutCandidate(t *testing.T) {
	env := newFinalizationDriverSeamEnv(t)
	hook := NewFinalizationRecoverScopeHook(env.messages)
	verdict, err := hook(env.ctx, env.execution)
	require.NoError(t, err)
	require.Equal(t, session.AgentFinalizationNoCandidate, verdict)
	stamp, err := env.execution.gate.beginCandidate(env.ctx, quietFinalizationTestFacts)
	require.NoError(t, err)
	require.EqualValues(t, 1, stamp.serial)
}

// TestFinalizationRecoverScopeHookRestartAfterSealIsIdempotent proves a
// restart against an already accepted record reports none without an error,
// leaves the accepted message untouched (no re-publication) and still
// resumes above the durable serial.
//
// Revert-check: a hook that runs the frozen recovery against the terminal
// record returns ErrAgentFinalizationChanged here; a hook that re-publishes
// or reverts the message fails the IsFinished assertions.
func TestFinalizationRecoverScopeHookRestartAfterSealIsIdempotent(t *testing.T) {
	env := newFinalizationDriverSeamEnv(t)
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 1}
	m := newFinalizationCandidateMessage(t, env.fakeEnv, id)
	persisted, err := persistFinalizationCandidate(t.Context(), env.sessions, env.messages, env.scope, id, m)
	require.NoError(t, err)
	accepted, err := publishFinalizationCandidate(t.Context(), env.sessions, env.messages, env.scope, id, persisted.finalizationCandidate)
	require.NoError(t, err)
	require.True(t, accepted.IsFinished())
	hook := NewFinalizationRecoverScopeHook(env.messages)
	verdict, err := hook(env.ctx, env.execution)
	require.NoError(t, err, "a restart after a seal is idempotent, not a recovery failure")
	require.Equal(t, session.AgentFinalizationNoCandidate, verdict)
	stored, err := env.messages.Get(t.Context(), accepted.ID)
	require.NoError(t, err)
	require.True(t, stored.IsFinished(), "recovery must not re-publish or revert the accepted message")
	record, err := env.scope.store.Snapshot(env.ctx, env.scope.identity)
	require.NoError(t, err)
	require.Equal(t, "accepted", record.Phase)
	stamp, err := env.execution.gate.beginCandidate(env.ctx, quietFinalizationTestFacts)
	require.NoError(t, err)
	require.EqualValues(t, 2, stamp.serial, "even the terminal path resumes above the durable serial")
}

// TestFinalizationRecoverScopeHookReadErrorKeepsCandidate proves a transient
// candidate-read failure surfaces as an error and never clears the recorded
// candidate (fail-closed).
//
// Revert-check: a hook (or frozen recovery) that maps the read failure to a
// failed verdict clears CandidateMessageID here and fails the snapshot
// assertions.
func TestFinalizationRecoverScopeHookReadErrorKeepsCandidate(t *testing.T) {
	env := newFinalizationDriverSeamEnv(t)
	id := message.FinalizationIdentity{Key: "logical", Generation: 1, Serial: 3}
	m := newFinalizationCandidateMessage(t, env.fakeEnv, id)
	persisted, err := persistFinalizationCandidate(t.Context(), env.sessions, env.messages, env.scope, id, m)
	require.NoError(t, err)
	broken := &finalizationRecoverBrokenReader{Service: env.messages, FinalizationReader: env.messages.(message.FinalizationReader)}
	hook := NewFinalizationRecoverScopeHook(broken)
	verdict, err := hook(env.ctx, env.execution)
	require.ErrorIs(t, err, errFinalizationRecoverReaderInjected)
	require.Equal(t, session.AgentFinalizationNoCandidate, verdict)
	require.EqualValues(t, 1, broken.calls.Load(), "the injected failure is the candidate read itself")
	record, err := env.scope.store.Snapshot(env.ctx, env.scope.identity)
	require.NoError(t, err)
	require.Equal(t, m.ID, record.CandidateMessageID, "a transient read failure must not clear the recorded candidate")
	require.Equal(t, persisted.hash, record.CandidateHash)
	require.EqualValues(t, 3, record.CandidateSerial)
	require.Equal(t, "executing", record.Phase)
}

// TestFinalizationRecoverScopeHookRefusesMissingExecution pins the fail-closed
// guard for a hook invoked without an execution.
//
// Revert-check: removing the execution guard makes a nil execution panic on
// the gate or scope dereference instead of returning this error.
func TestFinalizationRecoverScopeHookRefusesMissingExecution(t *testing.T) {
	env := newFinalizationDriverSeamEnv(t)
	hook := NewFinalizationRecoverScopeHook(env.messages)
	_, err := hook(env.ctx, nil)
	require.ErrorIs(t, err, ErrFinalizationFactsUnavailable)
}
