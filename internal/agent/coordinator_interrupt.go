// Cancellation, cross-process interrupt-inject polling, detached durable
// enqueueing, and message injection. Extracted from coordinator.go — pure
// code move, bodies unchanged.

package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/google/uuid"
)

// interruptInjectTick is how often the interrupt-inject ticker polls
// pending_injects for interrupt=true rows during an active turn. 3s is a
// deliberate middle ground: fast enough that `rush sessions inject
// --interrupt` feels near-immediate to an operator (worst case one tick of
// latency), slow enough that the extra SELECT is negligible even across a
// long multi-step turn. The ticker only lives for the duration of a turn (see
// startInterruptTicker), so there is no idle-process polling. It is a var so
// tests can shrink it to drive the real loop at test timescale, restoring it
// with t.Cleanup (precedent: recheckPassIntervalNS in coordinator_recheck.go).
var interruptInjectTick = 3 * time.Second

// interruptTickOperationTimeout is the per-tick deadline for the
// handleInterruptTick operation. If a tick's DB operations or downstream
// calls block longer than this, the tick is abandoned with a timeout error
// and the goroutine returns to the select loop to observe ctx.Done().
// P1-3 fix: prevents a single blocking tick from permanently hanging
// coordinator shutdown when the parent ctx is cancelled.
// 10s is chosen as ~3x the tick interval — long enough that normal operation
// never times out (a healthy tick completes in <<1s), but short enough that
// a genuinely stuck tick doesn't block shutdown for an unreasonable duration.
const interruptTickOperationTimeout = 10 * time.Second

// Cancel is the full Stop: stopTree(sessionID).
func (c *coordinator) Cancel(sessionID string) {
	c.stopTree(sessionID)
}

// CancelTurn cancels only sessionID's live generation -- no job stop, tree walk,
// wake zeroing or auto-resume suspension. See Coordinator.CancelTurn.
func (c *coordinator) CancelTurn(sessionID string) {
	if ag := c.agentFor(sessionID); ag != nil {
		ag.Cancel(sessionID)
	}
}

// stopTree is the body of Stop for sessionID: jobs, delegation tree, mailboxes,
// auto-resume suspension and wake zeroing. Rerun also uses it for the child
// tree of a voided delegation.
func (c *coordinator) stopTree(sessionID string) {
	// Phase-4 step 4 (doc sec.3.8): Stop is transitive -- the batch
	// delegation-tree walk collects sessionID's live delegation child ids
	// from the batch facts and, for EACH id, cancels the owner scope the
	// same way (sessionID is usually the PARENT whose tool call started a
	// delegation, but Cancel is also called on a child session id
	// directly). A session with no running delegation children degenerates
	// to the single-id behavior. The root stays in the set even if the fact
	// read is incomplete.
	ids := []string{sessionID}
	{
		facts := coordinatorBatchCutoverFacts(c)
		readCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		nodes, err := facts.LiveDelegationNodes(readCtx, sessionID)
		cancel()
		seen := map[string]struct{}{sessionID: {}}
		for _, node := range nodes {
			if node.ChildSessionID != "" {
				if _, exists := seen[node.ChildSessionID]; !exists {
					seen[node.ChildSessionID] = struct{}{}
					ids = append(ids, node.ChildSessionID)
				}
			}
		}
		if err != nil {
			slog.Error("coordinator.Cancel: failed to read batch delegation owners", "session_id", sessionID, "err", err)
		}
	}
	c.stopBatchOwners(ids)
	for _, id := range ids {
		// Task #1054: a delegated child session's live generation runs on
		// its registered driver, never c.currentAgent (task #1049) --
		// routing through agentFor is the same choke point wakeSession
		// uses, so Cancel on a child id actually reaches the SessionAgent
		// that owns its mailbox instead of silently finding an untouched
		// one on c.currentAgent (agent_control.go's genCancel == nil
		// branch: no error, no log, just nothing happens).
		if ag := c.agentFor(id); ag != nil {
			ag.Cancel(id)
		}
		// Doc sec.3.4's web session policy: after a Stop, automatic turns
		// are suspended until the next human message. The suspension is a
		// per-session state of its own (the arbiter state's suspended
		// flag), separate from
		// the bg-shell cap counter; the human-message reset path
		// (ResetAutoResumeCounter) clears both.
		c.suspendAutoResume(id)
		// A Stop ends the await_tasks sleep too; its max_wait schedule is
		// cancelled so no uncancelled once schedule keeps the run open.
		c.clearSleepAll(id)
	}
	// Doc sec.3.4/3.8: zero the wake bit on every already-terminal
	// (pending/done) debt row across the whole stopped tree, closing the
	// race where work finished with wake=1 a moment before Stop but its
	// notice was not yet reacted to -- a stopped delegation's child must
	// never get a turn out of that race. Batch-only: every owner captured
	// from the union above.
	if store := batchCutoverReactionStore(c); store != nil {
		zeroCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := store.BatchReactionSetWakeZeroForOwners(zeroCtx, ids); err != nil {
			slog.Error("coordinator.Cancel: failed to zero batch wake for stopped tree", "session_id", sessionID, "err", err)
		}
		cancel()
	}
	// A stopped child is registered for the recheck pass (the retained
	// release-shaped retry): its own work is cancelled silently and the idle
	// mailbox may fire no release hook.
	for _, id := range ids {
		c.addToRecheckSet(id)
	}
}

func (c *coordinator) CancelAll() (stillBusy bool) {
	c.StopRecheckTicker()
	c.StopWakeScheduler()
	// Stop timer admission before closing callback admission. Detached timer
	// deliveries and recheck wakes remain separate lifecycle backlog.
	c.timeouts.close()
	// Close admission before cancelling agent work: releases during unwind
	// must not create new DB readers. Ownership cleanup already happened in
	// agent_ownership.go.
	c.callbackMu.Lock()
	c.callbackClosing = true
	c.callbackContextLocked()
	cancelCallbacks := c.callbackCancel
	c.callbackMu.Unlock()
	cancelCallbacks()
	stillBusy = c.currentAgent.CancelAll()
	// B18 fix: a delegated child's driver is a SEPARATE SessionAgent from
	// c.currentAgent (task #1049) -- CancelAll must reach it too, or its own
	// in-flight Drain turn (built on context.Background(), doc sec.3.4)
	// survives this process's shutdown entirely.
	for _, driver := range c.subAgentDrivers.allDriverAgents() {
		if driver.CancelAll() {
			stillBusy = true
		}
	}
	if c.joinShutdownCallbacks() {
		stillBusy = true
	}
	return stillBusy
}

// joinShutdownCallbacks shares one five-second grace across BOTH callback
// groups and the stopped recheck producer, before App can release its store.
// Admission is already closed, including for late natural completions.
func (c *coordinator) joinShutdownCallbacks() bool {
	done := make(chan struct{})
	go func() {
		if c.recheckDone != nil {
			<-c.recheckDone
		}
		c.releaseWG.Wait()
		c.notifyWG.Wait()
		if c.callbackAfterWaitHook != nil {
			c.callbackAfterWaitHook()
		}
		close(done)
	}()
	timer := time.NewTimer(DefaultCancelAllGrace)
	defer timer.Stop()
	expired := timer.C
	if c.callbackJoinTimeout != nil {
		expired = c.callbackJoinTimeout()
	}
	select {
	case <-done:
		return false
	case <-expired:
		return true
	}
}

func (c *coordinator) ClearQueue(sessionID string) {
	c.currentAgent.ClearQueue(sessionID)
}

// interruptTickStopsFor reports whether a tick error must STOP the
// interrupt-inject ticker (gh#11a): a permanently non-durable dependency
// and a lost process-bound binding can never succeed on a later tick. The
// peeked inject row stays in place for an explicit operator action or a
// newly admitted epoch instead of spinning refusals until shutdown.
func interruptTickStopsFor(err error) bool {
	return errors.Is(err, ErrDiskProviderNotDurable) || errors.Is(err, ErrDiskProviderUnavailable)
}

// startInterruptTicker launches a goroutine that polls pending_injects for an
// interrupt=true row for sessionID every interruptInjectTick, for as long as
// ctx is live (i.e. the duration of the owning turn). On each interrupt row
// it consumes it, requeues the already-persisted message via
// ConsumeInterruptInjectAndEnqueue, and cancels the running generation.
//
// CORRECTED (task #421/P0-1; the original text here claimed "a replacement
// turn runs under the same coordinator-level Run", which stopped being true
// once handleInterruptTick started marking calls FromDurableQueue=true —
// see mailbox.go's guard on mb.replacement): for a durable-queue-originated
// interrupt, cancelling the current generation does NOT hand this Run() call
// a replacement turn to keep running — sessionAgent.Run's turn loop simply
// ends (hasNext=false), and this ticker's own ctx (the owning Run call's)
// is cancelled right along with it, so the ticker exits too. The durable row
// is the only remaining owner of the interrupted work; it is executed
// SEPARATELY, in the same OS process but outside this Run call, by
// RunNonInteractive's DrainSessionNow call (internal/app/app.go) once this
// Run() returns — not by this ticker continuing to poll for it. The one
// case this ticker DOES keep ticking across is a NON-durable interrupt
// (InterruptAndSend, still sets mb.replacement): that replacement genuinely
// does run under this same Run call, and remains interruptible by a second
// cross-process interrupt via this same ticker, exactly as originally
// documented. The goroutine also exits when ctx is cancelled (turn finished/
// aborted), so it never outlives the turn either way.
//
// Returns a channel that is closed when the ticker goroutine exits. Callers
// should defer a receive from this channel to ensure the goroutine has fully
// joined before returning, avoiding in-flight handleInterruptTick execution
// after context cancellation and DB cleanup.
func (c *coordinator) startInterruptTicker(ctx context.Context, sessionID string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interruptInjectTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// P1-3 fix: give each tick a bounded operation deadline so that
				// even if handleInterruptTick blocks (e.g., on a DB call or
				// downstream dependency that ignores parent ctx cancellation),
				// the tick returns within interruptTickOperationTimeout and we
				// can observe ctx.Done() on the next loop iteration.
				tickCtx, tickCancel := context.WithTimeout(ctx, interruptTickOperationTimeout)
				_, err := c.handleInterruptTick(tickCtx, sessionID)
				tickCancel()

				if err != nil {
					if interruptTickStopsFor(err) {
						slog.Error("coordinator: interrupt-inject cannot cross the durable queue; the dependency is permanently unavailable for this epoch", "session_id", sessionID, "err", err)
						return
					}
					if errors.Is(err, context.DeadlineExceeded) {
						// This is a signal that some operation inside handleInterruptTick
						// blocked without respecting ctx cancellation. Log at warning level
						// so it's visible in production and warrants investigation, but don't
						// stop the ticker — subsequent ticks may succeed.
						slog.Warn("coordinator: interrupt-inject tick timed out",
							"session_id", sessionID, "timeout", interruptTickOperationTimeout)
					} else {
						slog.Warn("coordinator: interrupt-inject tick failed",
							"session_id", sessionID, "err", err)
					}
					continue
				}
				// Continue ticking. Whether a replacement turn "remains
				// interruptible by this same ticker" depends on which path
				// fired above — see startInterruptTicker's own doc for the
				// distinction (task #421/P0-1 correction): a durable-queue
				// interrupt has no replacement turn under THIS Run call at
				// all (the row is executed separately, after Run returns),
				// so this loop iteration is really just clearing the way for
				// the ctx-cancellation exit that follows shortly. A
				// non-durable interrupt's replacement genuinely does run
				// here and stays covered by this same ticker. Either way,
				// subsequent interrupts are handled correctly (the durable
				// queue serves FIFO ordering regardless of which path
				// consumes it).
			}
		}
	}()
	return done
}

// handleInterruptTick performs one poll of the interrupt-inject queue. It
// returns fired=true when it consumed an interrupt row and issued a
// cancel+requeue (the caller then stops ticking). Extracted from the ticker
// goroutine so it can be unit-tested directly with a real session.Service and
// message.Service, without a live provider. It is a no-op returning
// (false, nil) when no interrupt row is pending.
//
// P0-2 fix (atomic): uses ConsumeInterruptInjectAndEnqueue to delete and
// enqueue in a single transaction, eliminating the data loss window where
// a separate delete-then-enqueue sequence could lose the row if enqueue failed.
// This approach requires building the call data BEFORE the atomic transaction.
// Every fallible step (messages.Get, buildCall, marshal) runs BEFORE the
// atomic consume — PeekInterruptInject does not delete, so a failure there
// simply leaves the row in place for the next tick to retry naturally; no
// explicit recreation is needed. Once ConsumeInterruptInjectAndEnqueue
// commits, the call is durably enqueued. A generation fence loss then
// atomically removes that exact candidate and restores the source row before
// the tick reports no delivery.
func (c *coordinator) handleInterruptTick(ctx context.Context, sessionID string) (bool, error) {
	// Ownership is published before the first generation starts. If a
	// reservation exists but its call snapshot is not visible yet, retry the
	// tick: consuming now could rebuild with operator credentials or disk.
	activeCall, activeToken, owned, published, tokenAvailable := activeCallStateForSession(c.currentAgent, sessionID)
	if owned && !published {
		return false, nil
	}

	// First, peek at the row to get the message reference (SELECT only, no delete)
	pi, err := c.sessions.PeekInterruptInject(ctx, sessionID)
	if err != nil {
		return false, err
	}
	if pi == nil {
		return false, nil
	}

	// Load the message to build call data
	injMsg, getErr := c.messages.Get(ctx, pi.MessageID)
	if getErr != nil {
		return false, fmt.Errorf("interrupt inject references missing message %q: %w", pi.MessageID, getErr)
	}

	// Non-durable dependencies cannot cross the durable queue boundary. If the
	// active owner carries credentials or a DiskProvider, hand the persisted
	// message directly to that owner's mailbox so the replacement retains the
	// exact in-process execution identity.
	if published && callCarriesNonDurableDependency(activeCall) {
		return c.handleActiveNonDurableInterrupt(ctx, pi, injMsg, activeCall, activeToken, tokenAvailable)
	}

	var call SessionAgentCall
	// idlePrep is the fence resolution of the idle branch (R12-H2b §2.4):
	// kept held across a successful enqueue (the row-keyed holder then
	// covers the binding), released on every failure path so a refused
	// tick never leaks the Continuation owner.
	var idlePrep *preparedContinuation
	if owned && published {
		// The ticker context belongs to the dispatcher that started the active
		// turn. It is not the policy context of that turn, and may already be
		// stale after the dispatcher has moved to another queued call. Copy the
		// published call so every policy and pinned value comes from that turn.
		// callFromActive: the operator's message never inherits the active
		// call's kind or notice flags (a Drain, a wake turn).
		call = callFromActive(activeCall)
		call.Prompt = injMsg.FullText()
		call.Attachments = nil
	} else {
		// An idle session has no active policy snapshot. Resolve a fresh call
		// from the tick context, as the normal detached path does.
		// An independently owned scope is needed only while building the durable
		// record. No live tool registry crosses serialization, so it is released
		// on every exit after the scope is created.
		if callOptionsFrom(ctx) != nil {
			ownedCtx, ownedScope := ownDeferredCallScope(ctx)
			ctx = ownedCtx
			defer ownedScope.close()
		}
		pinned, resolveErr := c.resolveSessionModels(ctx, sessionID)
		if resolveErr != nil {
			return false, fmt.Errorf("failed to resolve session models for interrupt tick: %w", resolveErr)
		}
		call, err = c.buildCall(ctx, sessionID, injMsg.FullText(), pinned, nil)
		if err != nil {
			return false, err
		}
		// R12-H2b §2.4: an idle session's replacement must carry the
		// session's persisted fence. Peek did not consume the row, so a
		// resolution failure leaves it for the next tick untouched.
		prep, prepErr := c.PrepareContinuation(ctx, sessionID, nil)
		if prepErr != nil {
			return false, prepErr
		}
		prep.applyTo(&call)
		idlePrep = prep
	}

	// Layer 1 (T9 shape, design doc §7.3): refuse outright, BEFORE the
	// atomic consume below, a call carrying a caller-supplied
	// DiskProvider. The row PeekInterruptInject found above is left
	// untouched (peek never deletes), so the operator has a record
	// instead of losing the message with no trace — consuming it here
	// would durably enqueue a row that a restart could only replay onto
	// the REAL disk.
	// R12-H2b §2.4: only a live provider WITHOUT an execution requirement
	// envelope is refused here — an envelope-carrying call is durably
	// queued below with a row-keyed Continuation owner pinning its
	// binding until the pump rebuild releases it in either outcome.
	if callCarriesDiskProvider(call) && requirementFromCall(call) == nil {
		if call.CallOptions.DiskBindingID != "" {
			return false, fmt.Errorf("%w: provider-dependent session continuation cannot be durably queued", ErrDiskProviderUnavailable)
		}
		return false, fmt.Errorf("%w (session=%s)", ErrDiskProviderNotDurable, sessionID)
	}

	// Reference the existing row; the agent must not re-create it.
	call.ExistingMessageID = pi.MessageID
	call.InjectID = pi.ID
	// Each durable handoff is an attempt, not the source inject's identity.
	// The source ID remains in InjectID so restoring and retrying the source
	// cannot let stale cleanup delete a later attempt's queue row.
	call.LogicalCallID = uuid.NewString()
	call.OnUserMessageCreated = nil

	// Mark as originating from the durable queue so InterruptAndReplace skips
	// mb.replacement to avoid double-execution (P0-1 fix). The pump will execute
	// the durable row directly; we only cancel the in-flight generation here.
	call.FromDurableQueue = true

	// Generate idempotency key
	var idempotencyKey string
	if call.LogicalCallID != "" {
		idempotencyKey = fmt.Sprintf("%s-%s", call.SessionID, call.LogicalCallID)
	} else {
		idempotencyKey = fmt.Sprintf("%s-%s", call.SessionID, call.InjectID)
	}

	// R12-H2b §2.4: hold the row-keyed Continuation owner for the window
	// between this durable enqueue and the pump's rebuild (which releases
	// it in both outcomes). A lost binding means the row could never
	// succeed — refuse typed and leave the peeked row untouched.
	var holdRelease func()
	if req := requirementFromCall(call); req != nil {
		rel, ok := holdDurableContinuationOwner(c.diskCaps, req.BindingID, idempotencyKey)
		if !ok {
			return false, fmt.Errorf("%w: interrupt replacement's disk provider binding %s is already lost (session=%s)", ErrDiskProviderUnavailable, req.BindingID, sessionID)
		}
		holdRelease = rel
	}

	// Convert to SessionAgentCallData for serialization
	callData := ToSessionAgentCallData(call)
	callDataJSON, marshalErr := json.Marshal(callData)
	if marshalErr != nil {
		if holdRelease != nil {
			holdRelease()
		}
		if idlePrep != nil {
			idlePrep.release()
		}
		return false, fmt.Errorf("failed to serialize call data for interrupt inject: %w", marshalErr)
	}

	// Now atomically consume (delete) and enqueue in one transaction, matching
	// the exact row peeked above so a concurrent deletion of that row (rather
	// than a stale re-select of "the oldest row") can never cause us to
	// consume a different row than the one callData was built from.
	enqueuedPi, enqueueErr := c.sessions.ConsumeInterruptInjectAndEnqueue(ctx, sessionID, pi.ID, idempotencyKey, callDataJSON)
	if enqueueErr != nil {
		// Transaction rolled back, so row still exists for retry; the
		// row-keyed holder dies with it (a later tick re-holds).
		if holdRelease != nil {
			holdRelease()
		}
		if idlePrep != nil {
			idlePrep.release()
		}
		return false, fmt.Errorf("failed to enqueue interrupt inject: %w", enqueueErr)
	}
	if enqueuedPi == nil {
		// Row vanished between peek and enqueue — handled gracefully
		// (the transaction rolled back, so no durable row exists):
		// release the holders exactly as the enqueue-error branch
		// above, or the binding pin and the prep token leak.
		if holdRelease != nil {
			holdRelease()
		}
		if idlePrep != nil {
			idlePrep.release()
		}
		return false, nil
	}

	// InterruptAndReplace atomically records call and cancels only the
	// in-flight generation (design §4). Since we've already enqueued durably,
	// we just need to cancel the in-flight generation if there is one.
	delivered := interruptAndReplaceSnapshot(c.currentAgent, sessionID, call, activeToken, tokenAvailable)
	if !delivered && owned && published {
		// The generation fence lost after the durable transaction committed.
		// Remove only this candidate and restore the exact source row; a newer
		// generation's queue entry is never selected by this reconciliation.
		// The durable row is removed again, so the row-keyed holder must
		// go with it (R12-H2b §2.4); a later tick re-holds a fresh one.
		if req := requirementFromCall(call); req != nil {
			releaseDurableContinuationOwner(c.diskCaps, req.BindingID, idempotencyKey)
		}
		// The durable row is gone too, so the idle prep token's owner
		// has nothing left to cover; leaving it held pins the binding
		// until the next tick's re-resolution.
		if idlePrep != nil {
			idlePrep.release()
		}
		if reconcileErr := c.reconcileInterruptInjectEnqueue(ctx, *enqueuedPi, idempotencyKey); reconcileErr != nil {
			return false, fmt.Errorf("failed to reconcile undelivered interrupt inject: %w", reconcileErr)
		}
		return false, nil
	}
	if !delivered {
		// No owner — session is idle, the durable enqueue already handles it
		slog.Debug("coordinator: interrupt tick enqueued durable call for idle session",
			"session_id", sessionID, "idempotency_key", idempotencyKey)
	}

	// Notify only after ownership delivery succeeds, or after an idle durable
	// enqueue has become the authoritative owner of the message.
	// The row-keyed holder now owns the wait window; the idle prep
	// token's random owner must not pin the binding past dispatch.
	if idlePrep != nil {
		idlePrep.release()
	}
	c.messages.Notify(injMsg)
	return true, nil
}

type activeCallSnapshotter interface {
	ActiveCall(sessionID string) (SessionAgentCall, bool)
}

type activeCallInspector interface {
	ActiveCallState(sessionID string) (SessionAgentCall, bool, bool)
}

type activeCallTokenInspector interface {
	ActiveCallStateWithToken(sessionID string) (SessionAgentCall, activeCallToken, bool, bool)
}

type activeCallReplacer interface {
	InterruptAndReplaceIfCurrent(sessionID string, call SessionAgentCall, token activeCallToken) bool
}

func activeCallStateForSession(currentAgent SessionAgent, sessionID string) (SessionAgentCall, activeCallToken, bool, bool, bool) {
	if inspector, ok := currentAgent.(activeCallTokenInspector); ok {
		call, token, owned, published := inspector.ActiveCallStateWithToken(sessionID)
		return call, token, owned, published, true
	}
	if inspector, ok := currentAgent.(activeCallInspector); ok {
		call, owned, published := inspector.ActiveCallState(sessionID)
		return call, activeCallToken{}, owned, published, false
	}
	snapshotter, ok := currentAgent.(activeCallSnapshotter)
	if !ok {
		return SessionAgentCall{}, activeCallToken{}, false, true, false
	}
	call, published := snapshotter.ActiveCall(sessionID)
	return call, activeCallToken{}, published, published, false
}

func interruptAndReplaceSnapshot(currentAgent SessionAgent, sessionID string, call SessionAgentCall, token activeCallToken, tokenAvailable bool) bool {
	if tokenAvailable {
		replacer, ok := currentAgent.(activeCallReplacer)
		if !ok {
			return false
		}
		return replacer.InterruptAndReplaceIfCurrent(sessionID, call, token)
	}
	return currentAgent.InterruptAndReplace(sessionID, call)
}

func (c *coordinator) handleActiveNonDurableInterrupt(
	ctx context.Context,
	pi *session.PendingInject,
	msg message.Message,
	active SessionAgentCall,
	token activeCallToken,
	tokenAvailable bool,
) (bool, error) {
	call := callFromActive(active)
	call.Prompt = msg.FullText()
	call.ExistingMessageID = pi.MessageID
	call.InjectID = ""
	call.FromDurableQueue = false
	call.LogicalCallID = uuid.New().String()

	// Consume the signal before handing off the in-process replacement. If the
	// owner disappeared in the narrow window, recreate it so the next normal
	// run remains the durable owner rather than silently losing the inject.
	if err := c.sessions.DeleteInterruptInject(ctx, pi.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("failed to consume disk-provider interrupt for session %s: %w", call.SessionID, err)
	}
	if !interruptAndReplaceSnapshot(c.currentAgent, call.SessionID, call, token, tokenAvailable) {
		if err := c.restorePendingInjectRow(ctx, pi); err != nil {
			return false, fmt.Errorf("active non-durable run ended before interrupt delivery and recovery failed: %w", err)
		}
		return false, nil
	}
	c.messages.Notify(msg)
	return true, nil
}

// InterruptAndSend queues a user message and cancels the running turn.
// agent.Run()'s cancel-handling branch drains the queue and the queued
// message becomes the next Run() — with all assistant content produced so
// far preserved in the DB (the cancel path writes a FinishReasonCanceled
// to the in-flight assistant message before unwinding).
func (c *coordinator) InterruptAndSend(ctx context.Context, sessionID, prompt string, smart, fast *ModelOverride, attachments ...message.Attachment) error {
	if err := c.readyWg.Wait(); err != nil {
		return err
	}
	var scopeTransferred bool
	ctx, ownedScope := ownDeferredCallScope(ctx)
	if ownedScope != nil {
		defer func() {
			if !scopeTransferred {
				ownedScope.close()
			}
		}()
	}
	var pinned *resolvedOverrides
	if smart != nil || fast != nil {
		resolved, applyErr := c.applyModelOverrides(ctx, sessionID, smart, fast)
		if applyErr != nil {
			return applyErr
		}
		pinned = resolved
	} else {
		// No explicit overrides: resolve from session DB or config defaults.
		// This ensures that an interrupt respects the session's persisted model
		// override (if any) rather than falling back to the shared/global model.
		resolved, resolveErr := c.resolveSessionModels(ctx, sessionID)
		if resolveErr != nil {
			return fmt.Errorf("failed to resolve session models for interrupt: %w", resolveErr)
		}
		pinned = resolved
	}
	call, err := c.buildCall(ctx, sessionID, prompt, pinned, attachments)
	if err != nil {
		return err
	}
	if ownedScope != nil {
		call.onQueueResolved = func(_ *fantasy.AgentResult, _ error) { ownedScope.close() }
	}
	// The owned scope was established before resolution and is carried through
	// buildCall; do not replace it with a fresh, empty scope here.
	// InterruptAndReplace atomically records call as the replacement the
	// current owner runs next and cancels only the in-flight generation
	// (design §4) — replacing the QueueMessage+Cancel two-step that P0-2
	// made self-defeating (Cancel deterministically wiped what QueueMessage
	// just queued). When the session is idle there is nothing to interrupt,
	// and nobody is running who would ever drain a queued call — so we must
	// start the run ourselves (P0-B).
	if !c.agentFor(sessionID).InterruptAndReplace(sessionID, call) {
		if err := c.startDetachedRun(ctx, call); err != nil {
			return fmt.Errorf("failed to enqueue interrupt for idle session %s: %w", sessionID, err)
		}
	} else {
		scopeTransferred = true
	}
	return nil
}

// startDetachedRun durably enqueues call for the idle-session paths of
// InterruptAndSend (P0-B). Despite the name (kept
// for git-blame continuity with the pre-#340 version), it no longer runs
// call itself, in a goroutine or otherwise — see the task #340 paragraph
// below for what changed and why.
//
// Those paths used to call QueueMessage(call) when InterruptAndReplace
// reported no owner. That is a runnerless queue: with the session idle
// there is no turn loop left to drain it, so the call sat there until
// some unrelated future Run() happened to come along — while the caller
// (and, through it, the web client) had already been told the message was
// "queued". For a user pressing interrupt on a session that had just
// finished, or landing in the race right after a release, the message
// simply never ran. Durably enqueuing here is what the mailbox's own
// contract says the caller must do when it is handed "no owner" (see
// mailbox.interruptAndReplace's doc).
//
// Task #340, ROUND 3 migration: durably enqueues the call to the
// session_run_queue table (session.EnqueueRunQueueEntry) synchronously, in
// the CALLER's own goroutine and ctx — no longer spawns its own goroutine
// and no longer wraps ctx in context.WithoutCancel. The independent
// RunQueuePump is what actually executes the call later; this function's
// job ends once the durable-enqueue write has committed (or, on enqueue
// failure, once the pending_injects row has been recreated below). This
// eliminates data loss risks from the previous bounded-retry-then-log
// approach and ensures every accepted call gets a guaranteed runner (or an
// explicit terminal failure recorded in the queue), even across process
// restarts.
//
// For the interrupt inject path (InjectID non-empty), we still delete the
// pending_injects row at START to prevent duplicate detached runs if the
// pump picks up the same call before we return. If durable enqueue fails, we
// recreate the row so a future tick can retry (P0-2).
func (c *coordinator) startDetachedRun(ctx context.Context, call SessionAgentCall) error {
	// Phase-4 step 3 (doc sec.3.4): a Drain call is NEVER durably enqueued.
	// Unreachable in practice today (a Drain never goes through
	// InterruptAndReplace/QueueExistingMessage), but this guard documents
	// the invariant at every EnqueueRunQueueEntry call site, not just the
	// one on the ordinary orphan-restart path.
	if call.IsDrain {
		return nil
	}
	// Layer 1 (T9 shape, design doc §7.3): refuse outright, before
	// touching anything (including the pending_injects row below), a
	// call carrying a caller-supplied DiskProvider. It has no
	// serializable form, so a durable row rebuilt from it would silently
	// restart on the REAL disk instead of the host's. Unlike
	// restartOrphanedWithRetry's finalizer, this error DOES reach the
	// original caller (InterruptAndSend wraps and returns it), so "Run
	// reports the refusal" holds for this producer.
	// R12-H2b §2.4: keep the legacy refusal (before ANY destructive step)
	// only for a live provider WITHOUT an execution requirement envelope;
	// an envelope-carrying call is durably queued below with a row-keyed
	// Continuation owner the pump rebuild releases in both outcomes.
	if callCarriesDiskProvider(call) && requirementFromCall(call) == nil {
		slog.Error("coordinator: refusing to durably enqueue a call carrying a caller-supplied disk provider with no execution requirement envelope",
			"session_id", call.SessionID, "logical_call_id", call.LogicalCallID)
		return fmt.Errorf("%w (session=%s)", ErrDiskProviderNotDurable, call.SessionID)
	}
	if call.Credentials != nil {
		slog.Error("coordinator: refusing to durably enqueue a call carrying per-call credentials",
			"session_id", call.SessionID, "logical_call_id", call.LogicalCallID)
		return fmt.Errorf("%w (session=%s)", ErrCredentialSetNotDurable, call.SessionID)
	}

	// P2-1: Generate idempotency key from LogicalCallID (stable per logical
	// request) instead of timestamp (which changes on every retry). Fallback
	// to timestamp with warning if LogicalCallID is empty (should not happen
	// in normal flow). For the interrupt inject path, the InjectID joins the
	// key. Computed BEFORE the pending_injects delete (R12-H2b §2.4): the
	// row-keyed Continuation holder below must be taken before any
	// destructive step, and released on EVERY failure path after it.
	var idempotencyKey string
	if call.LogicalCallID != "" {
		idempotencyKey = fmt.Sprintf("%s-%s", call.SessionID, call.LogicalCallID)
	} else if call.InjectID != "" {
		idempotencyKey = fmt.Sprintf("%s-%s", call.SessionID, call.InjectID)
	} else {
		slog.Warn("coordinator: LogicalCallID is empty, falling back to timestamp-based idempotency key (non-idempotent retries)",
			"session_id", call.SessionID)
		idempotencyKey = fmt.Sprintf("%s-%d", call.SessionID, time.Now().UnixNano())
	}

	// R12-H2b §2.4: hold the row-keyed Continuation owner for the wait
	// window; the pump rebuild releases it in both outcomes. A lost
	// binding refuses typed, before the delete below.
	var holdRelease func()
	if req := requirementFromCall(call); req != nil {
		rel, ok := holdDurableContinuationOwner(c.diskCaps, req.BindingID, idempotencyKey)
		if !ok {
			return fmt.Errorf("%w: detached replacement's disk provider binding %s is already lost (session=%s)", ErrDiskProviderUnavailable, req.BindingID, call.SessionID)
		}
		holdRelease = rel
	}

	// P0-2 fix: delete the pending_injects row at the START to prevent
	// duplicate detached runs. If durable enqueue fails, we'll recreate it.
	if call.InjectID != "" {
		slog.Debug("coordinator: detached run deleting pending_injects row at start",
			"inject_id", call.InjectID)
		if delErr := c.sessions.DeleteInterruptInject(ctx, call.InjectID); delErr != nil {
			if errors.Is(delErr, sql.ErrNoRows) {
				// Another detached consumer won ownership of this inject. Its
				// durable handoff is authoritative; do not enqueue a duplicate.
				slog.Debug("coordinator: detached run inject was already consumed; skipping duplicate",
					"inject_id", call.InjectID)
				// gh#10c: another detached consumer won ownership of this inject and THIS
				// consumer enqueued nothing — its row-keyed holder must die with the
				// abandoned attempt, or the binding stays pinned until process exit.
				if holdRelease != nil {
					holdRelease()
				}
				return nil
			}
			slog.Error("coordinator: detached run failed to delete pending_injects row at start",
				"inject_id", call.InjectID, "err", delErr)
			// A genuine DB error leaves ownership uncertain. Continue to the
			// durable enqueue so the call remains recoverable; enqueue failure
			// below recreates the inject row.
		} else {
			slog.Debug("coordinator: detached run deleted pending_injects row at start",
				"inject_id", call.InjectID)
		}
	}

	// Convert to SessionAgentCallData for serialization
	callData := ToSessionAgentCallData(call)
	callDataJSON, err := json.Marshal(callData)
	if err != nil {
		slog.Error("coordinator: failed to serialize call data for durable enqueue",
			"session_id", call.SessionID, "err", err)
		// For interrupt inject path, recreate the row to prevent data loss
		if call.InjectID != "" && call.ExistingMessageID != "" {
			if recreateErr := c.recreatePendingInjectRow(ctx, call); recreateErr != nil {
				slog.Error("coordinator: also failed to recreate pending_injects row during marshal recovery",
					"inject_id", call.InjectID, "recreate_err", recreateErr, "marshal_err", err)
			}
		}
		if holdRelease != nil {
			holdRelease()
		}
		return fmt.Errorf("failed to serialize call data for durable enqueue: %w", err)
	}

	// Durably enqueue the call BEFORE returning control (P0-2 requirement)
	// This ensures the call will eventually be executed even if this goroutine exits
	if enqueueErr := c.sessions.EnqueueRunQueueEntry(ctx, idempotencyKey, call.SessionID, callDataJSON); enqueueErr != nil {
		slog.Error("coordinator: failed to durably enqueue call for recovery",
			"session_id", call.SessionID, "err", enqueueErr)
		// For interrupt inject path, recreate the row so a future tick can retry
		if call.InjectID != "" && call.ExistingMessageID != "" {
			if recreateErr := c.recreatePendingInjectRow(ctx, call); recreateErr != nil {
				slog.Error("coordinator: also failed to recreate pending_injects row during enqueue recovery",
					"inject_id", call.InjectID, "recreate_err", recreateErr, "enqueue_err", enqueueErr)
			}
		}
		// For non-inject interrupts, there is no fallback — the call is lost.
		// Return error so the caller can handle it (e.g., surface to HTTP response).
		// For inject interrupts, we've recreated the row so a future tick will retry.
		if holdRelease != nil {
			holdRelease()
		}
		return fmt.Errorf("failed to durably enqueue call for recovery: %w", enqueueErr)
	}

	slog.Debug("coordinator: durably enqueued call for pump recovery",
		"session_id", call.SessionID, "idempotency_key", idempotencyKey, "inject_id", call.InjectID)
	return nil
}

// recreatePendingInjectRow recreates a pending_injects row for future retry
// (helper for startDetachedRun error path, P0-2 fix). Uses a bounded context
// (WithoutCancel + timeout) to ensure recovery writes have a chance even if
// the calling context is being canceled.
//
// Returns the error from recreation (or nil on success) so the caller can surface it.
func (c *coordinator) recreatePendingInjectRow(originalCtx context.Context, call SessionAgentCall) error {
	// Bounded context: disconnect from caller cancellation but enforce a timeout
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(originalCtx), 10*time.Second)
	defer cancel()

	slog.Debug("coordinator: attempting to recreate pending_injects row",
		"inject_id", call.InjectID, "session_id", call.SessionID, "message_id", call.ExistingMessageID)
	// Get the message content to recreate the row.
	msg, getErr := c.messages.Get(recoveryCtx, call.ExistingMessageID)
	if getErr != nil {
		slog.Error("coordinator: failed to recreate pending_injects row (could not get message)",
			"inject_id", call.InjectID, "message_id", call.ExistingMessageID, "err", getErr)
		return fmt.Errorf("could not get message for recreation: %w", getErr)
	}
	inject := session.PendingInject{
		ID:        uuid.New().String(), // Generate new ID to avoid UNIQUE constraint
		SessionID: call.SessionID,
		MessageID: call.ExistingMessageID,
		Content:   msg.FullText(),
		Interrupt: true,
	}
	createErr := c.sessions.CreatePendingInject(recoveryCtx, inject)
	if createErr != nil {
		slog.Error("coordinator: failed to recreate pending_injects row",
			"inject_id", call.InjectID, "err", createErr)
		return fmt.Errorf("failed to recreate pending_injects row: %w", createErr)
	}
	slog.Info("coordinator: successfully recreated pending_injects row for future retry",
		"new_inject_id", inject.ID, "old_inject_id", call.InjectID)
	return nil
}

// restorePendingInjectRow puts back the exact row consumed before a stale
// snapshot was rejected. Retaining its ID and creation time makes a retry
// idempotent and preserves FIFO ordering; a competing consumer is detected by
// DeleteInterruptInject returning sql.ErrNoRows before this function runs.
func (c *coordinator) restorePendingInjectRow(originalCtx context.Context, pi *session.PendingInject) error {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(originalCtx), 10*time.Second)
	defer cancel()
	if err := c.sessions.CreatePendingInject(recoveryCtx, *pi); err != nil {
		return fmt.Errorf("failed to restore pending_injects row %q: %w", pi.ID, err)
	}
	return nil
}

// reconcileInterruptInjectEnqueue uses a bounded recovery context because the
// ticker's context can be canceled immediately after a generation loses the
// delivery race. The candidate and source row must still be reconciled then.
func (c *coordinator) reconcileInterruptInjectEnqueue(originalCtx context.Context, pi session.PendingInject, idempotencyKey string) error {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(originalCtx), 10*time.Second)
	defer cancel()
	if err := c.sessions.ReconcileInterruptInjectEnqueue(recoveryCtx, pi, idempotencyKey); err != nil {
		return err
	}
	return nil
}

// InjectMessage — see Coordinator interface.
func (c *coordinator) InjectMessage(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (message.Message, error) {
	if err := c.readyWg.Wait(); err != nil {
		return message.Message{}, err
	}

	// Resolve the session's model configuration before building the call.
	pinned, err := c.resolveSessionModels(ctx, sessionID)
	if err != nil {
		return message.Message{}, fmt.Errorf("failed to resolve session models for inject: %w", err)
	}

	call, err := c.buildCall(ctx, sessionID, prompt, pinned, attachments)
	if err != nil {
		return message.Message{}, err
	}
	// Task #1054: see Cancel's identical reasoning above -- a delegated
	// child session's injectIfBusy merge must land on the mailbox its own
	// driver actually owns.
	return c.agentFor(sessionID).InjectMessage(ctx, call)
}
