package agent

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// FinalizationWiringDeps carries everything the composition root (internal/app)
// owns and the wiring needs. Enabled=false must produce a nil wiring and zero
// side effects.
type FinalizationWiringDeps struct {
	Enabled     bool
	Conn        *sql.DB
	Messages    message.Service
	NoticeStore *session.SessionNoticeStore
	DataDir     string
	Install     *BatchInstall
	// Authorities is the process-wide owner-to-authority registry the
	// composition root builds once when the flag is on and also hands to
	// the batch commit identity; the root bind registers the bound
	// authority here so runner-side commits can admit without a call
	// context. Optional for direct assembly, required by the root bind.
	Authorities *FinalizationAuthorityRegistry
	ResultGate  session.FinalizationTerminalGate
}

// finalizationWiring is the single production assembly of the finalization
// driver: durable stores, scope-facts readers, cancellation registry, and the
// L4/L5/L6 hooks. It is built once by the composition root and bound to the
// concrete coordinator.
type finalizationWiring struct {
	coord       *coordinator
	conn        *sql.DB
	store       *session.AgentFinalizationStore
	messages    message.Service
	noticeStore *session.SessionNoticeStore
	dataDir     string
	// resultGate is owned by the app-side batch gate; the wiring only carries
	// it and never reads it.
	resultGate  session.FinalizationTerminalGate
	descendants session.FinalizationDescendantsFacts
	queued      session.FinalizationQueuedCallsFacts
	holds       session.FinalizationPhysicalHoldsFacts
	debtFacts   session.FinalizationReactionDebtFacts
	settler     session.AgentFinalizationDebtSettler
	cancel      *finalizationCancelRegistry
	authorities *FinalizationAuthorityRegistry
	poll        time.Duration
	hooks       FinalizationDriverHooks
}

// newFinalizationWiring assembles the wiring. A disabled flag yields a nil
// wiring with no side effects; a partially installable configuration fails
// closed instead of running with missing collaborators.
func newFinalizationWiring(deps FinalizationWiringDeps, coord *coordinator) (*finalizationWiring, error) {
	if !deps.Enabled {
		return nil, nil
	}
	var missing []string
	if deps.Conn == nil {
		missing = append(missing, "conn")
	}
	if deps.Messages == nil {
		missing = append(missing, "messages")
	}
	if deps.NoticeStore == nil {
		missing = append(missing, "notice store")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("finalization wiring: missing collaborators: %s",
			fmt.Sprint(missing))
	}
	// Without a batch runtime there is no batch-root holds source; the hold
	// facts degrade to the host driver facts, which report zero holds for
	// batch roots.
	holds := session.NewFinalizationHostDriverHoldsFacts(deps.DataDir)
	if deps.Install != nil {
		assembled, err := newFinalizationPhysicalHoldsFacts(deps.Install, holds)
		if err != nil {
			return nil, err
		}
		holds = assembled
	}
	// The debt store serves both as the durable facts reader and the settler.
	ds := session.NewFinalizationDebtStore(deps.Conn)
	w := &finalizationWiring{
		coord:       coord,
		conn:        deps.Conn,
		store:       session.NewAgentFinalizationStore(deps.Conn),
		messages:    deps.Messages,
		noticeStore: deps.NoticeStore,
		dataDir:     deps.DataDir,
		resultGate:  deps.ResultGate,
		descendants: session.NewFinalizationDescendantsReader(),
		queued:      session.NewFinalizationQueuedCallsFacts(),
		holds:       holds,
		debtFacts:   ds,
		settler:     ds,
		cancel:      newFinalizationCancelRegistry(),
		authorities: deps.Authorities,
		// The wait loop re-reads durable facts at this cadence; changes made by
		// the batch runner never signal the in-process gate. The 5-minute
		// supervision window is far too coarse for a parent waiting on a child.
		poll: defaultFinalizationPollInterval,
	}
	w.hooks = FinalizationDriverHooks{
		BindScope:    w.bindScopeHook(),
		RecoverScope: NewFinalizationRecoverScopeHook(deps.Messages),
		FinalizeTurn: NewFinalizationFinalizeTurnHook(FinalizationWaitDeps{
			Settler:           ds,
			PollInterval:      w.poll,
			MaxRecompositions: -1,
		}),
		OnCancel: NewFinalizationOnCancelHook(w.cancel),
	}
	if err := w.hooks.Validate(); err != nil {
		return nil, err
	}
	return w, nil
}

// scopeFacts builds the composite scope reader bound to one owner session; the
// interface itself carries no owner, so the owner is fixed here.
func (w *finalizationWiring) scopeFacts(ownerSessionID string) session.AgentFinalizationScopeReader {
	return session.NewAgentFinalizationScopeReader(ownerSessionID, w.descendants, w.queued, w.holds, w.debtFacts)
}

// readFacts is the production adapter from durable facts to the in-process
// finalizationFacts snapshot. The database runs with a single open connection,
// so the three reads below are strictly sequential and no read runs outside a
// transaction while one is open.
func (w *finalizationWiring) readFacts(ctx context.Context, identity session.AgentFinalizationIdentity) (finalizationFacts, error) {
	if w == nil || w.store == nil || w.conn == nil || w.noticeStore == nil {
		return finalizationFacts{}, ErrFinalizationFactsUnavailable
	}
	if err := ctx.Err(); err != nil {
		return finalizationFacts{}, err
	}
	if identity.SessionID == "" || identity.LogicalCallID == "" || identity.Generation <= 0 {
		return finalizationFacts{}, ErrFinalizationFactsUnavailable
	}
	questions, err := w.noticeStore.PendingChildQuestions(ctx, identity.SessionID)
	if err != nil {
		return finalizationFacts{}, fmt.Errorf("finalization facts: pending child questions: %w", err)
	}
	record, err := w.store.Snapshot(ctx, identity)
	if err != nil {
		return finalizationFacts{}, fmt.Errorf("finalization facts: durable snapshot: %w", err)
	}
	tx, err := w.conn.BeginTx(ctx, nil)
	if err != nil {
		return finalizationFacts{}, fmt.Errorf("finalization facts: begin scope transaction: %w", err)
	}
	facts, scopeErr := w.scopeFacts(identity.SessionID).FinalizationSealScopeFacts(ctx, tx)
	if rollbackErr := tx.Rollback(); rollbackErr != nil && scopeErr == nil {
		scopeErr = fmt.Errorf("finalization facts: rollback scope transaction: %w", rollbackErr)
	}
	if scopeErr != nil {
		return finalizationFacts{}, fmt.Errorf("finalization facts: scope facts: %w", scopeErr)
	}
	if !facts.Complete {
		// Fail closed: incomplete facts must never look quiet.
		return finalizationFacts{}, fmt.Errorf("%w: finalization scope facts incomplete",
			ErrFinalizationFactsUnavailable)
	}
	return finalizationFacts{
		durableRevision: uint64(record.Revision),
		complete:        true,
		own:             int(record.OpenTickets),
		descendants:     facts.ActiveDescendants,
		physicalHolds:   facts.PhysicalHolds,
		debt:            facts.ReactionDebt,
		queuedCalls:     facts.QueuedCalls,
		heldQuestion:    len(questions) > 0,
		cancelled:       record.Cancelled,
	}, nil
}

// newExecution builds the per-call execution when the call owns a finalization
// authority of its own. Calls without a requirement, or descendants that only
// inherit a foreign authority, get no execution and stay inert.
func (w *finalizationWiring) newExecution(call SessionAgentCall) (*finalizationExecution, error) {
	requirement := call.finalizationAuthorities.Requirement()
	if requirement == nil {
		return nil, nil
	}
	var identity session.AgentFinalizationIdentity
	found := false
	for _, candidate := range requirement.Authorities {
		if candidate.SessionID == call.SessionID {
			identity = candidate
			found = true
			break
		}
	}
	if !found {
		return nil, nil
	}
	execution := &finalizationExecution{
		sessionID:  identity.SessionID,
		logicalID:  identity.LogicalCallID,
		gate:       newFinalizationGate(uint64(identity.Generation)),
		scope:      finalizationPublicationScope{store: w.store, identity: identity},
		scopeFacts: w.scopeFacts(identity.SessionID),
	}
	execution.read = func(ctx context.Context) (finalizationFacts, error) {
		return w.readFacts(ctx, identity)
	}
	return execution, nil
}

// bindScopeHook defers agent resolution to hook time: the wiring may be
// installed before any agent exists on the coordinator.
func (w *finalizationWiring) bindScopeHook() func(context.Context, SessionAgentCall) error {
	return func(ctx context.Context, call SessionAgentCall) error {
		if call.finalizationAuthorities.Requirement() == nil {
			return nil
		}
		agent := w.rootAgent()
		if agent == nil {
			return session.ErrAgentFinalizationDriverUnavailable
		}
		return NewFinalizationBindScopeHook(agent)(ctx, call)
	}
}

// rootAgent resolves the current concrete session agent, if one is running.
func (w *finalizationWiring) rootAgent() *sessionAgent {
	if w == nil || w.coord == nil || w.coord.currentAgent == nil {
		return nil
	}
	agent, _ := w.coord.currentAgent.(*sessionAgent)
	return agent
}

// startExecution is the driver entry from Run: it binds the scope, builds the
// execution, recovers a persisted candidate, and registers cancellation. A nil
// wiring is the flag-off no-op.
func (w *finalizationWiring) startExecution(ctx context.Context, call SessionAgentCall) (context.Context, func(), error) {
	if w == nil {
		return ctx, func() {}, nil
	}
	if err := w.hooks.BindScope(ctx, call); err != nil {
		return nil, nil, err
	}
	execution, err := w.newExecution(call)
	if err != nil {
		return nil, nil, err
	}
	if execution == nil {
		return ctx, func() {}, nil
	}
	execCtx, err := withFinalizationExecution(ctx, execution)
	if err != nil {
		return nil, nil, err
	}
	if w.hooks.RecoverScope != nil {
		recovery, err := w.hooks.RecoverScope(execCtx, execution)
		if err != nil {
			return nil, nil, err
		}
		// The verdict itself is informational here; adopt/fail side effects
		// already happened inside the hook.
		slog.Debug("Finalization recovery verdict",
			"session_id", execution.sessionID,
			"logical_call_id", execution.logicalID,
			"recovery", uint8(recovery),
		)
	}
	w.cancel.register(execution)
	return execCtx, func() { w.cancel.unregister(execution) }, nil
}

// finalizeTurn is the driver exit from runTurn. A turn without an execution
// takes the legacy queue/drain/replacement path.
func (w *finalizationWiring) finalizeTurn(ctx context.Context, ts *turnStream) error {
	if w == nil || ts == nil || ts.finalization == nil || w.hooks.FinalizeTurn == nil {
		return nil
	}
	return w.hooks.FinalizeTurn(ctx, ts)
}

// finalizationOnCancel is the cancellation entry from the coordinator
// interrupt path; a nil wiring keeps it silent.
func (c *coordinator) finalizationOnCancel(sessionID string) {
	w := c.finalizationWiring()
	if w != nil && w.hooks.OnCancel != nil {
		w.hooks.OnCancel(sessionID)
	}
}

// finalizationWiring returns the wiring installed on the agent's coordinator.
func (a *sessionAgent) finalizationWiring() *finalizationWiring {
	if a == nil || a.coord == nil {
		return nil
	}
	return a.coord.finalization
}

// finalizationWiring returns the wiring installed on the coordinator.
func (c *coordinator) finalizationWiring() *finalizationWiring {
	if c == nil {
		return nil
	}
	return c.finalization
}

// SetCoordinatorFinalizationWiring builds the finalization wiring from deps
// and binds it to the concrete coordinator. A disabled flag builds nothing
// and binds nothing; a partially installable configuration fails closed.
func SetCoordinatorFinalizationWiring(c Coordinator, deps FinalizationWiringDeps) error {
	if !deps.Enabled {
		return nil
	}
	if c == nil {
		return fmt.Errorf("finalization wiring: nil coordinator")
	}
	co, ok := c.(*coordinator)
	if !ok {
		return fmt.Errorf("finalization wiring: unsupported coordinator type %T", c)
	}
	w, err := newFinalizationWiring(deps, co)
	if err != nil {
		return err
	}
	co.finalization = w
	slog.Info("Finalization wiring installed", "session_poll", w.poll.String())
	return nil
}
