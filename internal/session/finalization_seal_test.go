package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// sealScopeReader is a stub scope reader returning fixed facts.
type sealScopeReader struct {
	facts AgentFinalizationScopeFacts
	err   error
}

func (r *sealScopeReader) FinalizationSealScopeFacts(context.Context, *sql.Tx) (AgentFinalizationScopeFacts, error) {
	return r.facts, r.err
}

// sealWriteProbe counts accepted-receipt writes and records whether the
// post-commit publish callback observed the finished row, mirroring agent's
// finalizationPublishProbe.
type sealWriteProbe struct {
	message.Service
	message.FinalizationReader
	updates      atomic.Int32
	publications atomic.Int32
	sawCommitted atomic.Bool
}

func (p *sealWriteProbe) UpdateTx(ctx context.Context, tx *sql.Tx, m message.Message) (func(), error) {
	publish, err := p.Service.UpdateTx(ctx, tx, m)
	if err != nil {
		return nil, err
	}
	p.updates.Add(1)
	return func() {
		stored, readErr := p.Get(ctx, m.ID)
		p.sawCommitted.Store(readErr == nil && stored.IsFinished())
		p.publications.Add(1)
		publish()
	}, nil
}

// sealWriter surfaces the capability TrySeal needs from the env's messages.
func (e candidateEnv) sealWriter(t *testing.T) FinalizationSealWriter {
	t.Helper()
	writer, ok := e.messages.(FinalizationSealWriter)
	require.True(t, ok)
	return writer
}

// sealStateDump renders the durable state TrySeal may touch as one canonical
// string, so any refusal can be asserted byte-identical across the call.
func sealStateDump(t *testing.T, conn *sql.DB) string {
	t.Helper()
	var b strings.Builder
	scan := func(query, order string, scanRow func(*sql.Rows)) {
		rows, err := conn.QueryContext(t.Context(), query+" ORDER BY "+order)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			scanRow(rows)
		}
		require.NoError(t, rows.Err())
	}
	scan(`SELECT id, session_id, parts, checkpoint_generation, finished_at FROM messages`, "id", func(rows *sql.Rows) {
		var id, sessionID, parts string
		var generation int64
		var finishedAt sql.NullInt64
		require.NoError(t, rows.Scan(&id, &sessionID, &parts, &generation, &finishedAt))
		fmt.Fprintf(&b, "message %s %s %d %v %s\n", id, sessionID, generation, finishedAt, parts)
	})
	scan(`SELECT session_id, logical_call_id, generation, revision, phase, node_id, node_token,
		candidate_message_id, candidate_hash, candidate_serial
		FROM session_agent_finalizations`, "session_id, logical_call_id, generation", func(rows *sql.Rows) {
		var sessionID, logicalID, phase, node, messageID, hash string
		var generation, revision, token, serial int64
		require.NoError(t, rows.Scan(&sessionID, &logicalID, &generation, &revision, &phase,
			&node, &token, &messageID, &hash, &serial))
		fmt.Fprintf(&b, "finalization %s %s %d %d %s %s %d %s %s %d\n",
			sessionID, logicalID, generation, revision, phase, node, token, messageID, hash, serial)
	})
	scan(`SELECT session_id, logical_call_id, generation, ticket_id, released
		FROM session_agent_finalization_tickets`, "session_id, logical_call_id, generation, ticket_id", func(rows *sql.Rows) {
		var sessionID, logicalID, ticket string
		var generation, released int64
		require.NoError(t, rows.Scan(&sessionID, &logicalID, &generation, &ticket, &released))
		fmt.Fprintf(&b, "ticket %s %s %d %s %d\n", sessionID, logicalID, generation, ticket, released)
	})
	return b.String()
}

// sealStoredCandidate stores a candidate message at the given step and returns
// the presented candidate exactly as the record holds it.
func sealStoredCandidate(t *testing.T, e candidateEnv, revision, serial uint64) (message.Message, AgentFinalizationCandidate) {
	t.Helper()
	m := e.candidate(t, "answer", e.step(revision, serial), 1)
	stored, err := e.storeCandidate(t, m, e.step(revision, serial))
	require.NoError(t, err)
	hash, err := stored.FinalizationContentHash()
	require.NoError(t, err)
	presented := AgentFinalizationCandidate{MessageID: stored.ID, Hash: hash, Revision: int64(revision), Serial: int64(serial)}
	return stored, presented
}

// Revert-check: a seal that publishes the terminal event before its
// transaction commits, or that seals the record without persisting the message
// receipt (or vice versa), fails this test.
func TestTrySealSealsRecordedCandidateAtomically(t *testing.T) {
	e := newCandidateEnv(t)
	stored, presented := sealStoredCandidate(t, e, 0, 1)
	hash, err := stored.FinalizationContentHash()
	require.NoError(t, err)
	probe := &sealWriteProbe{Service: e.messages, FinalizationReader: e.reader}
	outcome, err := e.store.TrySeal(t.Context(), e.id, presented, probe, &sealScopeReader{facts: AgentFinalizationScopeFacts{Complete: true}})
	require.NoError(t, err)
	require.True(t, outcome.Sealed)
	require.False(t, outcome.Accepted)
	require.Equal(t, AgentFinalizationSealNone, outcome.Reason)
	require.Equal(t, "accepted", outcome.Record.Phase)
	require.Zero(t, outcome.Record.OpenTickets)
	require.True(t, outcome.Message.IsFinished())
	finish := outcome.Message.FinishPart()
	require.NotNil(t, finish)
	require.False(t, finish.Partial)
	require.Equal(t, message.FinishReasonEndTurn, finish.Reason)
	require.Equal(t, hash, finish.FinalizationHash)
	require.Equal(t, message.FinalizationIdentity{Key: "logical", Generation: 1, Revision: 0, Serial: 1}, finish.FinalizationIdentity())
	require.EqualValues(t, 1, probe.updates.Load(), "the accepted receipt is written exactly once")
	require.EqualValues(t, 1, probe.publications.Load(), "one terminal event, published only after commit")
	require.True(t, probe.sawCommitted.Load(), "terminal publication must observe the accepted row after commit")
	restarted := NewAgentFinalizationStore(e.conn)
	record, err := restarted.Snapshot(t.Context(), e.id)
	require.NoError(t, err)
	require.Equal(t, "accepted", record.Phase)
	require.Equal(t, stored.ID, record.CandidateMessageID)
	require.Equal(t, hash, record.CandidateHash)
	require.EqualValues(t, 1, record.CandidateSerial)
	require.Zero(t, record.Revision)
	persisted, err := e.messages.Get(t.Context(), stored.ID)
	require.NoError(t, err)
	require.True(t, persisted.IsFinished())
}

// Revert-check: a regression that re-accepts or re-publishes an already
// accepted candidate (double terminal event) or that misreads the accepted
// state as scope-changed forever, fails the write counter and determinism
// assertions.
func TestTrySealIdempotentRepeatIsDeterministic(t *testing.T) {
	e := newCandidateEnv(t)
	_, presented := sealStoredCandidate(t, e, 0, 1)
	probe := &sealWriteProbe{Service: e.messages, FinalizationReader: e.reader}
	reader := &sealScopeReader{facts: AgentFinalizationScopeFacts{Complete: true}}
	first, err := e.store.TrySeal(t.Context(), e.id, presented, probe, reader)
	require.NoError(t, err)
	require.True(t, first.Sealed)
	repeat, err := e.store.TrySeal(t.Context(), e.id, presented, probe, reader)
	require.NoError(t, err)
	require.True(t, repeat.Accepted)
	require.False(t, repeat.Sealed)
	require.Equal(t, AgentFinalizationSealNone, repeat.Reason)
	require.Equal(t, first.Record, repeat.Record)
	requireSameSealedMessage(t, first.Message, repeat.Message)
	again, err := NewAgentFinalizationStore(e.conn).TrySeal(t.Context(), e.id, presented, probe, reader)
	require.NoError(t, err)
	require.True(t, again.Accepted)
	require.Equal(t, first.Record, again.Record)
	requireSameSealedMessage(t, first.Message, again.Message)
	require.EqualValues(t, 1, probe.updates.Load(), "no second accepted-receipt write")
	require.EqualValues(t, 1, probe.publications.Load(), "no second terminal event")
	record, err := e.store.Snapshot(t.Context(), e.id)
	require.NoError(t, err)
	require.Equal(t, "accepted", record.Phase)
	require.Equal(t, first.Record, record)
}

// Revert-check: any refusal path that mutates durable state (for example
// clearing the candidate the way RecoverCandidate does, or half-committing a
// receipt) fails the byte-identical dump; a seal that ignores any of these
// conditions fails the per-case Reason assertion.
func TestTrySealRefusalsLeaveDatabaseByteIdentical(t *testing.T) {
	type refusalCase struct {
		name    string
		setup   func(t *testing.T) (candidateEnv, AgentFinalizationCandidate, AgentFinalizationScopeReader)
		reason  AgentFinalizationSealReason
		wantErr error
	}
	complete := func() AgentFinalizationScopeReader {
		return &sealScopeReader{facts: AgentFinalizationScopeFacts{Complete: true}}
	}
	env := func(t *testing.T, revision, serial uint64) (candidateEnv, AgentFinalizationCandidate) {
		t.Helper()
		e := newCandidateEnv(t)
		_, presented := sealStoredCandidate(t, e, revision, serial)
		return e, presented
	}
	var cases []refusalCase
	cases = append(cases, refusalCase{
		name: "open ticket",
		setup: func(t *testing.T) (candidateEnv, AgentFinalizationCandidate, AgentFinalizationScopeReader) {
			e, presented := env(t, 0, 1)
			_, err := e.store.Admit(t.Context(), e.id, "work")
			require.NoError(t, err)
			return e, presented, complete()
		},
		reason: AgentFinalizationSealOpenTickets,
	}, refusalCase{
		name: "revision moved",
		setup: func(t *testing.T) (candidateEnv, AgentFinalizationCandidate, AgentFinalizationScopeReader) {
			e := newCandidateEnv(t)
			_, err := e.store.Admit(t.Context(), e.id, "before")
			require.NoError(t, err)
			require.NoError(t, e.store.Release(t.Context(), e.id, "before"))
			_, presented := sealStoredCandidate(t, e, 2, 3)
			require.EqualValues(t, 2, presented.Revision)
			_, err = e.store.Admit(t.Context(), e.id, "after")
			require.NoError(t, err)
			require.NoError(t, e.store.Release(t.Context(), e.id, "after"))
			return e, presented, complete()
		},
		reason: AgentFinalizationSealRevisionChanged,
	}, refusalCase{
		name: "no candidate",
		setup: func(t *testing.T) (candidateEnv, AgentFinalizationCandidate, AgentFinalizationScopeReader) {
			e := newCandidateEnv(t)
			return e, AgentFinalizationCandidate{MessageID: "fabricated", Hash: "hash", Revision: 0, Serial: 1}, complete()
		},
		reason: AgentFinalizationSealNoCandidate,
	})
	addStale := func(name string, mutate func(*AgentFinalizationCandidate)) {
		cases = append(cases, refusalCase{
			name: name,
			setup: func(t *testing.T) (candidateEnv, AgentFinalizationCandidate, AgentFinalizationScopeReader) {
				e, presented := env(t, 0, 2)
				mutate(&presented)
				return e, presented, complete()
			},
			reason: AgentFinalizationSealStaleCandidate,
		})
	}
	addStale("stale serial", func(c *AgentFinalizationCandidate) { c.Serial = 1 })
	addStale("stale message id", func(c *AgentFinalizationCandidate) { c.MessageID = "other" })
	addStale("stale hash", func(c *AgentFinalizationCandidate) { c.Hash = "other" })
	for _, phase := range []string{"failed", "cancelled", "interrupted"} {
		target := phase
		cases = append(cases, refusalCase{
			name: "wrong phase " + target,
			setup: func(t *testing.T) (candidateEnv, AgentFinalizationCandidate, AgentFinalizationScopeReader) {
				e, presented := env(t, 0, 1)
				_, err := e.conn.ExecContext(t.Context(),
					`UPDATE session_agent_finalizations SET phase=? WHERE session_id=?`, target, e.id.SessionID)
				require.NoError(t, err)
				return e, presented, complete()
			},
			reason: AgentFinalizationSealWrongPhase,
		})
	}
	cases = append(cases, refusalCase{
		name: "cancel requested",
		setup: func(t *testing.T) (candidateEnv, AgentFinalizationCandidate, AgentFinalizationScopeReader) {
			e, presented := env(t, 0, 1)
			require.NoError(t, NewService(db.New(e.conn), e.conn).RequestCancel(t.Context(), e.id.SessionID))
			return e, presented, complete()
		},
		reason: AgentFinalizationSealCancelled,
	}, refusalCase{
		name: "scope nil",
		setup: func(t *testing.T) (candidateEnv, AgentFinalizationCandidate, AgentFinalizationScopeReader) {
			e, presented := env(t, 0, 1)
			return e, presented, nil
		},
		reason: AgentFinalizationSealScopeIncomplete,
	}, refusalCase{
		name: "scope incomplete",
		setup: func(t *testing.T) (candidateEnv, AgentFinalizationCandidate, AgentFinalizationScopeReader) {
			e, presented := env(t, 0, 1)
			return e, presented, &sealScopeReader{}
		},
		reason: AgentFinalizationSealScopeIncomplete,
	}, refusalCase{
		name: "scope reader fails",
		setup: func(t *testing.T) (candidateEnv, AgentFinalizationCandidate, AgentFinalizationScopeReader) {
			e, presented := env(t, 0, 1)
			return e, presented, &sealScopeReader{err: sql.ErrConnDone}
		},
		wantErr: sql.ErrConnDone,
	})
	addBusy := func(name string, reason AgentFinalizationSealReason, facts AgentFinalizationScopeFacts) {
		facts.Complete = true
		cases = append(cases, refusalCase{
			name: name,
			setup: func(t *testing.T) (candidateEnv, AgentFinalizationCandidate, AgentFinalizationScopeReader) {
				e, presented := env(t, 0, 1)
				return e, presented, &sealScopeReader{facts: facts}
			},
			reason: reason,
		})
	}
	addBusy("descendants active", AgentFinalizationSealDescendantsActive, AgentFinalizationScopeFacts{ActiveDescendants: 1})
	addBusy("reaction debt", AgentFinalizationSealReactionDebt, AgentFinalizationScopeFacts{ReactionDebt: 1})
	addBusy("physical holds", AgentFinalizationSealPhysicalHolds, AgentFinalizationScopeFacts{PhysicalHolds: 1})
	addBusy("queued calls", AgentFinalizationSealQueuedCalls, AgentFinalizationScopeFacts{QueuedCalls: 1})
	addDamaged := func(name string, damage func(t *testing.T, e candidateEnv, stored message.Message)) {
		cases = append(cases, refusalCase{
			name: name,
			setup: func(t *testing.T) (candidateEnv, AgentFinalizationCandidate, AgentFinalizationScopeReader) {
				e := newCandidateEnv(t)
				stored, presented := sealStoredCandidate(t, e, 0, 1)
				damage(t, e, stored)
				return e, presented, complete()
			},
			reason: AgentFinalizationSealCandidateDamaged,
		})
	}
	addDamaged("stored parts mutated", func(t *testing.T, e candidateEnv, stored message.Message) {
		res, err := e.conn.ExecContext(t.Context(),
			`UPDATE messages SET parts=replace(parts, 'answer', 'edited') WHERE id=?`, stored.ID)
		require.NoError(t, err)
		n, err := res.RowsAffected()
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
	})
	addDamaged("message row deleted", func(t *testing.T, e candidateEnv, stored message.Message) {
		_, err := e.conn.ExecContext(t.Context(), `DELETE FROM messages WHERE id=?`, stored.ID)
		require.NoError(t, err)
	})
	addDamaged("footer generation mutated", func(t *testing.T, e candidateEnv, stored message.Message) {
		res, err := e.conn.ExecContext(t.Context(), `UPDATE messages SET parts=replace(parts,
			'"finalization_generation":1', '"finalization_generation":9') WHERE id=?`, stored.ID)
		require.NoError(t, err)
		n, err := res.RowsAffected()
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, presented, reader := tc.setup(t)
			before := sealStateDump(t, e.conn)
			outcome, err := e.store.TrySeal(t.Context(), e.id, presented, e.sealWriter(t), reader)
			if tc.wantErr != nil {
				require.Error(t, err)
				require.ErrorIs(t, err, tc.wantErr)
				require.Equal(t, AgentFinalizationSealOutcome{}, outcome)
			} else {
				require.NoError(t, err)
				require.False(t, outcome.Sealed)
				require.False(t, outcome.Accepted)
				require.Equal(t, tc.reason, outcome.Reason)
			}
			require.Equal(t, before, sealStateDump(t, e.conn), "a refusal must not mutate durable state")
		})
	}
}

// Revert-check: a seal that ignores open tickets or loses the conditional
// UPDATE's guard can double-seal or corrupt the finished message — the
// single-publication and fencing assertions fail.
func TestTrySealSealAndStoreRaceKeepsFencing(t *testing.T) {
	e := newCandidateEnv(t)
	stored, presented := sealStoredCandidate(t, e, 0, 1)
	hash, err := stored.FinalizationContentHash()
	require.NoError(t, err)
	ctx := t.Context()
	writer := e.sealWriter(t)
	reader := &sealScopeReader{facts: AgentFinalizationScopeFacts{Complete: true}}
	start := make(chan struct{})
	var sealers sync.WaitGroup
	sealers.Add(3)
	outcomes := make(chan AgentFinalizationSealOutcome, 3)
	failures := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			defer sealers.Done()
			<-start
			outcome, err := e.store.TrySeal(ctx, e.id, presented, writer, reader)
			outcomes <- outcome
			failures <- err
		}()
	}
	late := make(chan error, 1)
	go func() {
		<-start
		sealers.Wait()
		// The seal has committed; persisted admission must refuse for good.
		for attempt := 0; attempt < 8; attempt++ {
			if _, err := e.store.Admit(ctx, e.id, "late"); err != nil {
				late <- err
				return
			}
			if err := e.store.Release(ctx, e.id, "late"); err != nil {
				late <- err
				return
			}
		}
		late <- errors.New("admission after the seal never refused")
	}()
	close(start)
	var collected []AgentFinalizationSealOutcome
	for i := 0; i < 3; i++ {
		require.NoError(t, <-failures)
		collected = append(collected, <-outcomes)
	}
	require.ErrorIs(t, <-late, ErrAgentFinalizationChanged)
	sealed := 0
	for _, outcome := range collected {
		if !outcome.Sealed && !outcome.Accepted {
			require.NotEqual(t, AgentFinalizationSealNone, outcome.Reason, "a race loser cannot claim a bare success")
		}
		switch {
		case outcome.Sealed:
			sealed++
			require.False(t, outcome.Accepted)
		case outcome.Accepted:
			require.False(t, outcome.Sealed)
		default:
			require.False(t, outcome.Sealed)
			require.False(t, outcome.Accepted)
		}
	}
	require.Equal(t, 1, sealed, "the conditional seal guard admits exactly one sealer")
	record, err := e.store.Snapshot(ctx, e.id)
	require.NoError(t, err)
	require.Equal(t, "accepted", record.Phase)
	require.Zero(t, record.OpenTickets)
	persisted, err := e.messages.Get(ctx, stored.ID)
	require.NoError(t, err)
	finish := persisted.FinishPart()
	require.NotNil(t, finish)
	require.False(t, finish.Partial)
	require.Equal(t, message.FinishReasonEndTurn, finish.Reason)
	require.Equal(t, hash, finish.FinalizationHash)
	require.Equal(t, 1, strings.Count(e.rowParts(t, stored.ID), `"finalization_hash"`),
		"exactly one accepted footer may land in the stored parts")
}

// sealBatchRootReader reads one real owned-scope fact (live batch roots) with
// the transaction TrySeal is holding.
type sealBatchRootReader struct{ owner string }

func (r sealBatchRootReader) FinalizationSealScopeFacts(ctx context.Context, tx *sql.Tx) (AgentFinalizationScopeFacts, error) {
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM batch_nodes
		WHERE owner_session_id=? AND state IN ('pending','starting','running','cancelling')`, r.owner).Scan(&live); err != nil {
		return AgentFinalizationScopeFacts{}, err
	}
	return AgentFinalizationScopeFacts{Complete: true, ActiveDescendants: live}, nil
}

// newReservedScopeEnv binds the scope after one admitted live-root
// reservation: the S1 ingress fence refuses reservations for an owner that
// already has a bound finalization scope, so the root must exist first.
func newReservedScopeEnv(t *testing.T) candidateEnv {
	t.Helper()
	_, conn, ctx := batchStoreTestDB(t)
	messages := message.NewService(db.New(conn))
	store := NewAgentFinalizationStore(conn)
	id := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "logical", Generation: 1}
	batchStoreTestReserve(t, NewBatchStore(conn), ctx)
	require.NoError(t, store.Bind(ctx, id))
	writer, ok := messages.(message.FinalizationWriter)
	require.True(t, ok)
	reader, ok := messages.(message.FinalizationReader)
	require.True(t, ok)
	return candidateEnv{conn: conn, store: store, messages: messages, writer: writer, reader: reader, id: id}
}

// Revert-check: a fail-open default (missing reader or !Complete treated as
// "clear") lets the seal proceed, and a reader that reads outside the seal's
// transaction misses the admitted root — both fail this test's assertions.
func TestTrySealScopeReaderReadsInsideTransaction(t *testing.T) {
	e := newCandidateEnv(t)
	_, presented := sealStoredCandidate(t, e, 0, 1)
	outcome, err := e.store.TrySeal(t.Context(), e.id, presented, e.sealWriter(t), sealBatchRootReader{owner: e.id.SessionID})
	require.NoError(t, err)
	require.True(t, outcome.Sealed, "an established reader observing zero live roots must seal")

	busy := newReservedScopeEnv(t)
	_, busyPresented := sealStoredCandidate(t, busy, 0, 1)
	before := sealStateDump(t, busy.conn)
	outcome, err = busy.store.TrySeal(t.Context(), busy.id, busyPresented, busy.sealWriter(t), sealBatchRootReader{owner: busy.id.SessionID})
	require.NoError(t, err)
	require.False(t, outcome.Sealed)
	require.False(t, outcome.Accepted)
	require.Equal(t, AgentFinalizationSealDescendantsActive, outcome.Reason)
	require.Equal(t, before, sealStateDump(t, busy.conn))
}

// Revert-check: a TrySeal that proceeds without the message-writer capability
// cannot persist the accepted receipt and would produce a sealed record with
// an unfinished message.
func TestTrySealRequiresWriterCapability(t *testing.T) {
	e := newCandidateEnv(t)
	_, presented := sealStoredCandidate(t, e, 0, 1)
	before := sealStateDump(t, e.conn)
	outcome, err := e.store.TrySeal(t.Context(), e.id, presented, nil, &sealScopeReader{facts: AgentFinalizationScopeFacts{Complete: true}})
	require.ErrorIs(t, err, ErrAgentFinalizationChanged)
	require.Equal(t, AgentFinalizationSealOutcome{}, outcome)
	require.Equal(t, before, sealStateDump(t, e.conn))
}

// requireSameSealedMessage compares two views of the sealed message ignoring
// UpdatedAt: the seal outcome carries the accepted row as it was built before
// UpdateTx stamped its own `time.Now().Unix()` on a copy, so a repeat read from
// the store differs by one second whenever the seal crosses a second boundary.
func requireSameSealedMessage(t *testing.T, want, got message.Message) {
	t.Helper()
	want.UpdatedAt, got.UpdatedAt = 0, 0
	require.Equal(t, want, got)
}
