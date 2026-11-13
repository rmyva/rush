package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// Invoke is the ONE ingress API (decomposition row C): validate the
// invocation fail-closed, refuse D1 control tools as generic targets, admit
// the one-item spec through the R11A validator, attach to a live root for a
// matching retry (exactly-once execution), otherwise Submit, then hand the
// started root to the R11D delivery seam. Hooks, restrictions and permissions
// run once, inside the pinned policy leaf the executor resolves; the old
// wrapper path is never taken.
//
// A nil delivery seam or runner handle is a typed not-wired error returned
// BEFORE any store probe or engine call: the dormant runtime never falls back
// to the legacy async path.
func (r *batchRuntime) Invoke(ctx context.Context, inv BatchInvocation, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	pctHandoff := pctHandoffFrom(ctx)
	if err := inv.Validate(); err != nil {
		if pctHandoff != nil {
			pctHandoff.pctRelease()
		}
		return fantasy.ToolResponse{}, err
	}
	if call.ID == "" {
		if pctHandoff != nil {
			pctHandoff.pctRelease()
		}
		return fantasy.ToolResponse{}, fmt.Errorf("%w", errBatchMissingCallID)
	}
	if leaf == nil {
		if pctHandoff != nil {
			pctHandoff.pctRelease()
		}
		return fantasy.ToolResponse{}, fmt.Errorf("%w: %q", errBatchNilLeaf, call.Name)
	}
	// D1: control-plane tools execute their pinned policy leaf directly;
	// they are never generic batch targets.
	if batchControlTarget(call.Name) {
		if pctHandoff != nil {
			pctHandoff.pctRelease()
		}
		return fantasy.ToolResponse{}, fmt.Errorf("%w: %q", errBatchControlPlaneLeaf, call.Name)
	}
	if err := ctx.Err(); err != nil {
		if pctHandoff != nil {
			pctHandoff.pctRelease()
		}
		return fantasy.ToolResponse{}, err
	}
	if call.Name == AgentToolName || call.Name == tools.AgenticFetchToolName {
		if !inv.AgentDepthSet {
			inv.AgentDepth = DefaultAgentDepth
			inv.AgentDepthSet = true
		}
		if inv.AgentDepth < 0 || inv.AgentDepth > MaxAgentDepthForResume {
			if pctHandoff != nil {
				pctHandoff.pctRelease()
			}
			return fantasy.ToolResponse{}, errors.New("agent depth must be between 0 and 32")
		}
		if opts := callOptionsFrom(ctx); opts != nil && opts.AgentDepthSet && opts.MaxAgentDepth < inv.AgentDepth {
			inv.AgentDepth = opts.MaxAgentDepth
		}
	}
	input, err := batchCanonicalInput(call.Input)
	if err != nil {
		if pctHandoff != nil {
			pctHandoff.pctRelease()
		}
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	spec, err := batchOneItemSpec(inv, call, input)
	if err != nil {
		if pctHandoff != nil {
			pctHandoff.pctRelease()
		}
		// A bad timeout is the model's own input: report it back so the turn
		// goes on, instead of a Go error that ends the run.
		var inputErr batchToolInputError
		if errors.As(err, &inputErr) {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		return fantasy.ToolResponse{}, err
	}
	spec, err = stampBatchCallCredentials(ctx, spec)
	if err != nil {
		if h := pctHandoffFrom(ctx); h != nil {
			h.pctReleasePending()
		}
		return fantasy.ToolResponse{}, err
	}
	hash := batchInvocationHash(spec)

	// Singleflight covers only IN-FLIGHT executions: finishCall removes the
	// entry before publishing, so a retry arriving after completion takes
	// the fresh durable path (design I5: a reused call id after a history
	// rerun is a new execution, never a cached replay). A waiter whose own
	// context is still alive but that received the first caller's
	// cancellation re-runs the fresh path as the new first caller; the loop
	// bound only guards against a pathological store that keeps returning
	// foreign cancellation errors.
	for attempts := 0; ; attempts++ {
		resp, err, retry := r.invokeOnce(ctx, inv, spec, hash, call.ID)
		if !retry || attempts >= 4 {
			return resp, err
		}
	}
}

// invokeOnce runs one singleflight step for a prepared invocation: join an
// in-flight execution or become its first caller.
func (r *batchRuntime) invokeOnce(ctx context.Context, inv BatchInvocation, spec jobbatch.Spec, hash, callID string) (fantasy.ToolResponse, error, bool) {
	r.mu.Lock()
	if r.cfg.Delivery == nil {
		r.mu.Unlock()
		return fantasy.ToolResponse{}, fmt.Errorf("%w", errBatchDeliveryNotWired), false
	}
	if r.cfg.Runner == nil {
		r.mu.Unlock()
		return fantasy.ToolResponse{}, fmt.Errorf("%w", errBatchRunnerNotWired), false
	}
	key := inv.SessionID + "\x00" + callID
	if e, ok := r.calls[key]; ok {
		r.mu.Unlock()
		if e.hash != hash {
			return fantasy.ToolResponse{}, fmt.Errorf("%w: call %q", errBatchInvocationMismatch, callID), false
		}
		select {
		case e.joined <- struct{}{}:
		default:
		}
		resp, err := waitCall(ctx, e)
		// Waiter poisoning: the published outcome is the FIRST caller's
		// cancellation, not this waiter's (its own ctx is still alive) and
		// the entry is already removed, so re-run the fresh path.
		if ctx.Err() == nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return fantasy.ToolResponse{}, nil, true
		}
		return resp, err, false
	}
	e := &batchCallEntry{hash: hash, done: make(chan struct{}), joined: make(chan struct{}, batchCallWaiterBuffer)}
	r.calls[key] = e
	r.mu.Unlock()

	resp, err := r.invokeFresh(ctx, inv, spec, hash, callID)
	r.finishCall(key, e, resp, err)
	return resp, err, false
}

// invokeFresh runs the first execution of one call identity: durable attach
// probe, admission, Submit, delivery.
func (r *batchRuntime) invokeFresh(ctx context.Context, inv BatchInvocation, spec jobbatch.Spec, hash, callID string) (fantasy.ToolResponse, error) {
	probe := batchAttachProbe{
		Owner: inv.SessionID, Host: r.cfg.Host, RunnerID: r.cfg.RunnerID,
		ToolCallID: callID, InputHash: hash,
		SubmittedMessageID: inv.MessageID, SubmittedRegistryID: inv.RegistryID,
	}
	root, attached, err := r.cfg.Attach.ProbeLive(ctx, probe)
	if err != nil {
		if handoff := pctHandoffFrom(ctx); handoff != nil {
			handoff.pctRelease()
		}
		return fantasy.ToolResponse{}, fmt.Errorf("batch attach: %w", err)
	}
	if attached && inv.Policy.DiskBindingID != "" {
		if r.cfg.RootLeases == nil || !r.cfg.RootLeases.matches(root, inv.Policy.DiskBindingID, inv.RegistryID) {
			if handoff := pctHandoffFrom(ctx); handoff != nil {
				handoff.pctRelease()
			}
			return fantasy.ToolResponse{}, ErrDiskProviderUnavailable
		}
	}
	if !attached {
		root, err = r.submit(ctx, inv, spec, hash, callID)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
	} else if handoff := pctHandoffFrom(ctx); handoff != nil {
		handoff.pctRelease()
	}
	resp, err := r.cfg.Delivery.DeliverBatch(ctx, root, inv)
	// Accepted physical work cannot be preempted by caller cancellation
	// (R12-H2 §1.2: physical I/O Stop cannot preempt). The first caller's
	// cancellation Stops the root once (deliverSync) and returns its ctx
	// error, but the call's owner scope must NOT unwind beneath the still
	// executing root: re-wait WITHOUT the caller's cancellation for the
	// root's real completion, so the call/construction owners are released
	// only after the physical settle — never by a waiter leaving early.
	if err != nil && ctx.Err() != nil &&
		(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		resp, err = r.cfg.Delivery.DeliverBatch(context.WithoutCancel(ctx), root, inv)
	}
	// Settled roots stay RETAINED by the engine by design (design §1.2): no
	// per-call release happens here — execution owners release only at
	// physical Dispose (Runner.Release/cleanup).
	return resp, err
}

// submit admits and starts the one-item root. Admission (ValidateSubmit) runs
// strictly before the Runner is called.
func (r *batchRuntime) submit(ctx context.Context, inv BatchInvocation, spec jobbatch.Spec, hash, callID string) (jobbatch.ID, error) {
	root := batchRootID()
	waiter := batchRankContext{Rank: inv.WaiterRank, Root: root}
	if err := ValidateSubmit(r.cfg.Profile, waiter, nil, spec, r.cfg.MaxDepth, r.cfg.MaxNodes, r.cfg.Resolver); err != nil {
		if handoff := pctHandoffFrom(ctx); handoff != nil {
			handoff.pctRelease()
		}
		return "", fmt.Errorf("%w: %w", errBatchSubmitBeforeInvalid, err)
	}
	// Root owner: acquired BEFORE Submit with the already-minted root ID;
	// it covers accepted-but-not-started children. Released on definite
	// rejection below; a successful Submit keeps it and the Done watcher
	// releases it when no further child can start.
	rootOwned := false
	pctHandoff := pctHandoffFrom(ctx)
	if pctHandoff != nil {
		if r.cfg.DiskBindings == nil || len(pctHandoff.releases) == 0 {
			pctHandoff.pctRelease()
			return "", ErrDiskProviderUnavailable
		}
	}
	if inv.RegistryID != "" {
		if r.cfg.RootLeases == nil {
			if pctHandoff != nil {
				pctHandoff.pctRelease()
			}
			return "", ErrDiskProviderUnavailable
		}
		if err := r.cfg.RootLeases.acquire(root, inv.SessionID, inv.Policy.DiskBindingID, inv.RegistryID); err != nil {
			if pctHandoff != nil {
				pctHandoff.pctRelease()
			}
			return "", err
		}
		rootOwned = true
	}
	releaseRoot := func() {
		if rootOwned {
			r.cfg.RootLeases.release(root, inv.Policy.DiskBindingID, inv.RegistryID)
		}
	}
	request := jobbatch.SubmitRequest{Root: root, Spec: spec, Ref: callID}
	var id jobbatch.ID
	var batch *jobbatch.Batch
	var err error
	if rootOwned {
		bound, ok := r.cfg.Runner.(batchBoundRunnerHandle)
		if !ok {
			releaseRoot()
			if pctHandoff != nil {
				pctHandoff.pctRelease()
			}
			return "", ErrDiskProviderUnavailable
		}
		id, batch, err = bound.SubmitBound(ctx, request)
	} else {
		id, err = r.cfg.Runner.Submit(ctx, request)
	}
	if err != nil {
		releaseRoot()
		if pctHandoff != nil {
			pctHandoff.pctRelease()
		}
		return "", fmt.Errorf("batch submit: %w", err)
	}
	if id != root {
		releaseRoot()
		if pctHandoff != nil {
			pctHandoff.pctRelease()
		}
		return "", fmt.Errorf("batch submit: runner root %q differs from requested %q", id, root)
	}
	if rootOwned {
		if batch == nil {
			releaseRoot()
			if pctHandoff != nil {
				pctHandoff.pctRelease()
			}
			return "", ErrDiskProviderUnavailable
		}
		if batch.ID() != root {
			releaseRoot()
			if pctHandoff != nil {
				pctHandoff.pctRelease()
			}
			return "", fmt.Errorf("batch submit: runner root %q differs from requested %q", batch.ID(), root)
		}
		release, ok := r.cfg.Runner.(batchReleaseHandle)
		if !ok {
			if pctHandoff != nil {
				pctHandoff.pctRelease()
			}
			return "", ErrDiskProviderUnavailable
		}
		if err := r.cfg.RootLeases.adopt(root, batch.Done(), inv.Policy.DiskBindingID, inv.RegistryID, release.Release); err != nil {
			releaseRoot()
			if pctHandoff != nil {
				pctHandoff.pctRelease()
			}
			return "", err
		}
	}
	if pctHandoff != nil {
		// pct: successful Submit transfers cleanup to the accepted-root Done watcher.
		pctHandoff.pctAccepted()
		if batch != nil {
			go func() {
				<-batch.Done()
				pctHandoff.pctReleaseUnstarted()
			}()
		}
	}
	_ = hash // The commit hook reserves (root, callID, hash) atomically in R11B.
	return root, nil
}

// batchToolInputError marks a direct call whose tool input itself is invalid.
type batchToolInputError struct{ err error }

func (e batchToolInputError) Error() string { return e.err.Error() }
func (e batchToolInputError) Unwrap() error { return e.err }

// batchOneItemSpec builds the direct-call spec: OneItem, NotifyAll, kind
// derived from the tool family, payload carrying the invocation's immutable
// policy transport and pinned registry identity. The call's timeout intent
// is normalized here: a terminate_and_wake intent latches TaskSpec.Timeout
// (the engine's termination deadline — previously never set on the direct
// path), a wake_only intent is carried durably in the payload for the
// App-owned check-in scanner. Explicit specs and Notify selection are
// represented by the batch model controls, not this direct-call adapter.
func batchOneItemSpec(inv BatchInvocation, call fantasy.ToolCall, input json.RawMessage) (jobbatch.Spec, error) {
	payload := batchLeafPayload{
		Tool:          call.Name,
		ToolCallID:    call.ID,
		SessionID:     inv.SessionID,
		Input:         input,
		RegistryID:    inv.RegistryID,
		Policy:        &inv.Policy,
		QuestionRoute: inv.QuestionRoute,
	}
	if call.Name == AgentToolName {
		if err := validateAgentRoleInput(input); err != nil {
			return jobbatch.Spec{}, batchToolInputError{err: err}
		}
		var selected struct {
			Role RequestedModelRole `json:"role"`
		}
		if err := json.Unmarshal(input, &selected); err != nil {
			return jobbatch.Spec{}, batchToolInputError{err: err}
		}
		if selected.Role != "" && !selected.Role.builtin() {
			cfg, err := configForBatchInvocation(inv)
			if err != nil {
				return jobbatch.Spec{}, err
			}
			payload.SelectedRoleContract, err = ResolveRoleContract(cfg, string(selected.Role), inv.Policy.RoleContract)
			if err != nil {
				return jobbatch.Spec{}, err
			}
		}
	}
	intent, err := batchParseTimeoutParam(call.Name, string(input))
	if err != nil {
		return jobbatch.Spec{}, batchToolInputError{err}
	}
	if call.Name == AgentToolName || call.Name == tools.AgenticFetchToolName {
		remaining := inv.AgentDepth
		payload.AgentDepth = &remaining
	}
	timeout := time.Duration(0)
	if intent != nil {
		if intent.Kind == batchTimeoutTerminateAndWake {
			timeout = intent.Duration
		} else {
			payload.Timeout = &batchLeafTimeoutSpec{Kind: batchTimeoutWakeOnly, Seconds: intent.Seconds}
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return jobbatch.Spec{}, fmt.Errorf("batch payload: %w", err)
	}
	return jobbatch.Spec{Task: &jobbatch.TaskSpec{
		Kind: batchLeafKind(call.Name), Payload: raw, Timeout: timeout,
	}}, nil
}

// batchLeafKind maps the tool family onto the executor routing kind. MCP
// leaves must declare their kind explicitly (R7a wiring); nothing is inferred
// from a name prefix.
func batchLeafKind(tool string) string {
	switch tool {
	case "bash":
		return batchKindShell
	case "run_command":
		return batchKindArgv
	case "agent", "agentic_fetch":
		return batchKindAgent
	}
	// R11F (M3) flag-on hook: MCP leaves registered by the binding resolve
	// through the MCP kind; nil (flag-off) keeps the generic default.
	if override := batchLeafKindOverride.Load(); override != nil {
		if kind, ok := (*override)(tool); ok {
			return kind
		}
	}
	return batchKindGeneric
}

// batchLeafKindOverride is set only by the flag-on coordinator binding. It is
// atomic: every App bind stores it while invocations of other Apps (and
// parallel tests) read it.
var batchLeafKindOverride atomic.Pointer[func(tool string) (kind string, ok bool)]

// setBatchLeafKindOverride installs fn (nil clears the override).
func setBatchLeafKindOverride(fn func(tool string) (kind string, ok bool)) {
	if fn == nil {
		batchLeafKindOverride.Store(nil)
		return
	}
	batchLeafKindOverride.Store(&fn)
}
