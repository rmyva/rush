package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
)

type batchModelDelivery interface {
	DeliverBatch(context.Context, jobbatch.ID, BatchInvocation) (fantasy.ToolResponse, error)
}

type installedBatchModelControl struct {
	outputCache        *batchOutputCache
	runner             batchModelRunner
	delivery           batchModelDelivery
	store              *session.BatchStore
	live               *session.BatchLiveReader
	sessions           session.Service
	host, runnerID     string
	profile            *BatchProfile
	resolver           batchMCPCapabilityResolver
	rootLeases         *batchRootLeaseStore
	runMu              *sync.Mutex
	maxDepth, maxNodes int
}

var batchModelRunFallbackMu sync.Mutex

var _ BatchModelControl = (*installedBatchModelControl)(nil)

func NewInstalledBatchModelControl(runner batchModelRunner, delivery batchModelDelivery, store *session.BatchStore, live *session.BatchLiveReader, sessions session.Service, host, runnerID string, profile *BatchProfile, resolver batchMCPCapabilityResolver, leases ...*batchRootLeaseStore) BatchModelControl {
	control := &installedBatchModelControl{runner: runner, delivery: delivery, store: store, live: live, sessions: sessions, host: host, runnerID: runnerID, profile: profile, resolver: resolver, runMu: &sync.Mutex{}, maxDepth: batchDefaultMaxDepth, maxNodes: batchDefaultMaxNodes}
	if adapter, ok := runner.(interface{ OutputCache() *batchOutputCache }); ok {
		control.outputCache = adapter.OutputCache()
	}
	if len(leases) > 0 {
		control.rootLeases = leases[0]
	}
	return control
}

func (c *installedBatchModelControl) InstallBatchModelControls(reg *BatchLeafRegistry, inv BatchInvocation) ([]fantasy.AgentTool, error) {
	if reg == nil {
		return nil, errors.New("batch controls require calling pinned registry")
	}
	return newBatchModelTools(c, inv, reg), nil
}

func (c *installedBatchModelControl) Run(ctx context.Context, inv BatchInvocation, reg *policyLeafRegistry, raw batchModelSpec) (jobbatch.ID, error) {
	if c.runner == nil || c.store == nil || c.delivery == nil {
		return "", errors.New("batch runtime unavailable")
	}
	if !inv.AgentDepthSet {
		inv.AgentDepth = DefaultAgentDepth
		inv.AgentDepthSet = true
	}
	if inv.roleConfig == nil {
		if cfg, ok := parentRoleConfigFrom(ctx); ok {
			inv.roleConfig = cfg
		}
	}
	if inv.roleConfig == nil && inv.Policy.RoleContract != nil {
		return "", ErrAgentRoleUnconfigured
	}
	if inv.roleConfig == nil && modelSpecHasCustomRole(raw) {
		return "", ErrAgentRoleUnconfigured
	}
	if inv.roleConfig == nil {
		inv.roleConfig = &config.Config{}
	}
	if err := inv.Validate(); err != nil {
		return "", err
	}
	if inv.CallID == "" || reg == nil || inv.RegistryID != inv.Policy.RegistryID || !reg.hasToolset(inv.Policy.RestrictedToolset) {
		return "", errBatchMissingCallID
	}
	spec, err := modelBatchSpecForAdmission(raw, inv, reg, c.resolver)
	if err != nil {
		return "", err
	}
	spec, err = admitBatchRoleContracts(inv.roleConfig, inv.Policy.RoleContract, spec)
	if err != nil {
		return "", err
	}
	if err := assignModelBatchCallIDs(&spec, inv.CallID, "run"); err != nil {
		return "", err
	}
	if err := ValidateSubmit(c.profile, batchRankContext{Rank: inv.WaiterRank}, nil, spec, c.maxDepth, c.maxNodes, c.resolver); err != nil {
		return "", err
	}
	// Hash the exact spec the adapter submits. Credential stamping adds a
	// CallCredentials marker to agent payloads, and the durable reservation
	// hashes that stamped spec; probing the unstamped form misses live retries.
	spec, err = stampBatchCallCredentials(ctx, spec)
	if err != nil {
		return "", err
	}
	inputHash := batchInvocationHash(spec)
	var runMu *sync.Mutex
	if c.runMu != nil {
		runMu = c.runMu
	} else {
		runMu = &batchModelRunFallbackMu
	}
	runMu.Lock()
	locked := true
	defer func() {
		if locked {
			runMu.Unlock()
		}
	}()
	liveRoot, attached, err := c.store.BatchProbeLive(ctx, session.BatchLiveProbe{
		Owner: inv.SessionID, Host: c.host, RunnerID: c.runnerID,
		ToolCallID: inv.CallID, InputHash: inputHash,
	})
	if err != nil {
		return "", fmt.Errorf("batch attach: %w", err)
	}
	if attached {
		if c.rootLeases == nil || !c.rootLeases.matches(jobbatch.ID(liveRoot), inv.Policy.DiskBindingID, inv.RegistryID) {
			return "", ErrDiskProviderUnavailable
		}
		locked = false
		runMu.Unlock()
		resp, err := c.delivery.DeliverBatch(ctx, jobbatch.ID(liveRoot), inv)
		if err != nil {
			return "", err
		}
		if resp.IsError {
			return "", fmt.Errorf("batch delivery failed: %s", resp.Content)
		}
		return jobbatch.ID(liveRoot), nil
	}
	root := batchRootID()
	// Reservation identity is the caller's tool-call ID, so matching retries
	// attach to the live root before attempting another Runner submission.
	request := jobbatch.SubmitRequest{Root: root, Spec: spec, Ref: inv.CallID}
	// Root owner: acquired BEFORE Submit with the already-minted root ID;
	// it covers accepted-but-not-started children. Released on definite
	// rejection below; a successful Submit keeps it and the Done watcher
	// releases it when no further child can start.
	rootOwned := false
	if inv.RegistryID != "" {
		if c.rootLeases == nil {
			return "", ErrDiskProviderUnavailable
		}
		if err := c.rootLeases.acquire(root, inv.SessionID, inv.Policy.DiskBindingID, inv.RegistryID); err != nil {
			return "", err
		}
		rootOwned = true
	}
	releaseRoot := func() {
		if rootOwned {
			c.rootLeases.release(root, inv.Policy.DiskBindingID, inv.RegistryID)
		}
	}
	var id jobbatch.ID
	var batch *jobbatch.Batch
	if rootOwned {
		bound, ok := c.runner.(batchBoundRunnerHandle)
		if !ok {
			releaseRoot()
			return "", ErrDiskProviderUnavailable
		}
		id, batch, err = bound.SubmitBound(ctx, request)
	} else {
		id, err = c.runner.Submit(ctx, request)
	}
	if err != nil {
		releaseRoot()
		return "", err
	}
	if id != root {
		releaseRoot()
		return "", fmt.Errorf("runner root %q differs from requested %q", id, root)
	}
	if rootOwned {
		if batch == nil {
			releaseRoot()
			return "", ErrDiskProviderUnavailable
		}
		if batch.ID() != root {
			releaseRoot()
			return "", fmt.Errorf("runner root %q differs from requested %q", batch.ID(), root)
		}
		release, ok := c.runner.(batchReleaseHandle)
		if !ok {
			return "", ErrDiskProviderUnavailable
		}
		if err := c.rootLeases.adopt(root, batch.Done(), inv.Policy.DiskBindingID, inv.RegistryID, release.Release); err != nil {
			return "", err
		}
	}
	locked = false
	runMu.Unlock()
	resp, err := c.delivery.DeliverBatch(ctx, root, inv)
	if err != nil {
		return "", err
	}
	if resp.IsError {
		return "", fmt.Errorf("batch delivery failed: %s", resp.Content)
	}
	return root, nil
}

func assignModelBatchCallIDs(spec *jobbatch.Spec, base, path string) error {
	if spec == nil {
		return errors.New("nil batch spec")
	}
	if spec.Task != nil {
		var payload batchLeafPayload
		if err := json.Unmarshal(spec.Task.Payload, &payload); err != nil {
			return err
		}
		payload.ToolCallID = fmt.Sprintf("%s-%s", base, path)
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		spec.Task.Payload = raw
		return nil
	}
	if spec.Batch != nil {
		for i := range spec.Batch.Items {
			if err := assignModelBatchCallIDs(&spec.Batch.Items[i], base, fmt.Sprintf("%s-%d", path, i)); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New("invalid empty batch spec")
}

func terminalBatchStoreState(s string) bool {
	switch s {
	case "completed", "failed", "cancelled", "timed_out", "interrupted", "skipped":
		return true
	}
	return false
}

func (c *installedBatchModelControl) readAuthorized(ctx context.Context, caller, root string) (session.BatchSnapshot, error) {
	if caller == "" || root == "" || c.store == nil || c.sessions == nil {
		return session.BatchSnapshot{}, errors.New("batch root access unavailable")
	}
	for current, steps := caller, 0; current != "" && steps < 128; steps++ {
		snap, err := c.store.Read(ctx, current, root)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return session.BatchSnapshot{}, err
			}
			sess, getErr := c.sessions.Get(ctx, current)
			if getErr != nil {
				return session.BatchSnapshot{}, getErr
			}
			current = sess.ParentSessionID
			continue
		}
		return snap, nil
	}
	return session.BatchSnapshot{}, errors.New("batch root not owned by caller ancestry")
}

func findBatchNode(nodes []db.BatchNode, id string) (db.BatchNode, bool) {
	for _, n := range nodes {
		if n.NodeID == id {
			return n, true
		}
	}
	return db.BatchNode{}, false
}

func batchModelNodeInSnapshot(nodes []db.BatchNode, id, kind, root string) bool {
	parents := make(map[string]string, len(nodes))
	kinds := make(map[string]string, len(nodes))
	for _, n := range nodes {
		kinds[n.NodeID] = n.Kind
		if n.ParentID.Valid {
			parents[n.NodeID] = n.ParentID.String
		}
	}
	if kind != "" && kinds[id] != kind {
		return false
	}
	for cur, steps := id, 0; cur != "" && steps <= len(nodes); steps++ {
		if cur == root {
			return true
		}
		cur = parents[cur]
	}
	return false
}

func batchModelSpecNodes(items []batchModelItem, inv BatchInvocation, reg *policyLeafRegistry, resolver batchMCPCapabilityResolver) ([]jobbatch.Spec, error) {
	out := make([]jobbatch.Spec, 0, len(items))
	for i, item := range items {
		var spec jobbatch.Spec
		var err error
		if item.Batch != nil {
			wrapped := inv
			if item.Batch.MaxAgentDepth != nil {
				if *item.Batch.MaxAgentDepth < 0 || *item.Batch.MaxAgentDepth > inv.AgentDepth {
					return nil, errors.New("batch_add max_agent_depth exceeds trusted caller cap")
				}
				wrapped.AgentDepth = *item.Batch.MaxAgentDepth
			}
			spec, err = modelBatchSpecForAdmission(*item.Batch, wrapped, reg, resolver)
		} else {
			var wrapper jobbatch.Spec
			wrapper, err = modelBatchSpecForAdmission(batchModelSpec{Mode: "parallel", Notify: "each", Items: []batchModelItem{item}}, inv, reg, resolver)
			if err == nil {
				spec = wrapper.Batch.Items[0]
			}
		}
		if err != nil {
			return nil, err
		}
		if spec.Task != nil {
			var payload batchLeafPayload
			if err = json.Unmarshal(spec.Task.Payload, &payload); err != nil {
				return nil, err
			}
			if (payload.Tool == AgentToolName || payload.Tool == "agentic_fetch") &&
				(payload.AgentDepth == nil || *payload.AgentDepth > inv.AgentDepth) {
				return nil, errors.New("batch_add agent leaf exceeds trusted depth")
			}
			payload.ToolCallID = fmt.Sprintf("%s-add-%d", inv.CallID, i)
			spec.Task.Payload, err = json.Marshal(payload)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, spec)
	}
	return out, nil
}

func modelSpecHasCustomRole(spec batchModelSpec) bool {
	for _, item := range spec.Items {
		if batchModelItemHasCustomRole(item) {
			return true
		}
	}
	return false
}

func batchModelItemHasCustomRole(item batchModelItem) bool {
	if item.Batch != nil {
		for _, child := range item.Batch.Items {
			if batchModelItemHasCustomRole(child) {
				return true
			}
		}
		return false
	}
	if item.Tool != AgentToolName {
		return false
	}
	var input struct {
		Role RequestedModelRole `json:"role"`
	}
	if json.Unmarshal(item.Input, &input) != nil {
		return false
	}
	return input.Role != "" && !input.Role.builtin()
}

func (c *installedBatchModelControl) Add(ctx context.Context, caller, root, parent string, inv BatchInvocation, reg *policyLeafRegistry, items []batchModelItem) ([]jobbatch.ID, error) {
	if c.runner == nil || c.store == nil {
		return nil, errors.New("batch runtime unavailable")
	}
	if caller == "" || caller != inv.SessionID || root == "" || len(items) == 0 {
		return nil, errors.New("batch_add requires caller, root and items")
	}
	if !inv.AgentDepthSet {
		inv.AgentDepth, inv.AgentDepthSet = DefaultAgentDepth, true
	}
	if inv.roleConfig == nil {
		if cfg, ok := parentRoleConfigFrom(ctx); ok {
			inv.roleConfig = cfg
		}
	}
	if inv.roleConfig == nil && slices.ContainsFunc(items, batchModelItemHasCustomRole) {
		return nil, ErrAgentRoleUnconfigured
	}
	if inv.roleConfig == nil && inv.Policy.RoleContract != nil {
		return nil, ErrAgentRoleUnconfigured
	}
	if inv.roleConfig == nil {
		inv.roleConfig = &config.Config{}
	}
	if err := inv.Validate(); err != nil {
		return nil, err
	}
	s, err := c.readAuthorized(ctx, caller, root)
	if err != nil {
		return nil, err
	}
	if s.Root.RootID != root || s.Root.Kind != "batch" || s.Root.HostID != c.host || terminalBatchStoreState(s.Root.State) {
		return nil, errors.New("batch_add requires live local owned root")
	}
	if c.rootLeases == nil || !c.rootLeases.matches(jobbatch.ID(root), inv.Policy.DiskBindingID, inv.RegistryID) {
		return nil, ErrDiskProviderUnavailable
	}
	if parent == "" {
		parent = root
	}
	depths := map[string]int{root: 1}
	for progress := true; progress; {
		progress = false
		for _, n := range s.Nodes {
			if depths[n.NodeID] == 0 && n.ParentID.Valid {
				if d := depths[n.ParentID.String]; d > 0 {
					depths[n.NodeID] = d + 1
					progress = true
				}
			}
		}
	}
	depth := depths[parent]
	if depth == 0 {
		return nil, errors.New("batch_add parent not in root")
	}
	pn, ok := findBatchNode(s.Nodes, parent)
	if !ok || pn.Kind != "batch" {
		return nil, errors.New("batch_add parent must be batch")
	}
	specs, err := batchModelSpecNodes(items, inv, reg, c.resolver)
	if err != nil {
		return nil, err
	}
	for i := range specs {
		specs[i], err = admitBatchRoleContracts(inv.roleConfig, inv.Policy.RoleContract, specs[i])
		if err != nil {
			return nil, err
		}
	}
	used := len(s.Nodes)
	for i := range specs {
		spec := specs[i]
		if err := assignModelBatchCallIDs(&spec, inv.CallID, fmt.Sprintf("add-%d", i)); err != nil {
			return nil, err
		}
		if err := ValidateAdd(c.profile, batchRankContext{Rank: inv.WaiterRank, Root: jobbatch.ID(root)}, nil, spec, depth, used, c.maxDepth, c.maxNodes, c.resolver); err != nil {
			return nil, err
		}
		_, added, _, err := batchSpecNodes([]jobbatch.Spec{spec})
		if err != nil {
			return nil, err
		}
		used += added
		specs[i] = spec
	}
	return c.runner.Add(ctx, jobbatch.AddRequest{Parent: jobbatch.ID(parent), Items: specs, Ref: inv.CallID})
}

func incompleteBatchStatus(root, node string) map[string]any {
	m := map[string]any{"root_id": root, "state": "unknown", "terminal": false, "complete": false}
	if node != "" {
		m["node_id"] = node
	}
	return m
}

func (c *installedBatchModelControl) StatusNode(ctx context.Context, caller, root, node string) (any, error) {
	if node == "" || node == root {
		return c.Status(ctx, caller, root)
	}
	if c.live == nil {
		return incompleteBatchStatus(root, node), nil
	}
	s, err := c.readAuthorized(ctx, caller, root)
	if err != nil {
		return incompleteBatchStatus(root, node), nil
	}
	if s.Root.RootID != root || s.Root.Kind != "batch" || !batchModelNodeInSnapshot(s.Nodes, node, "", root) {
		return incompleteBatchStatus(root, node), nil
	}
	fact := c.live.BatchLevel(ctx, session.BatchLiveRef{OwnerSessionID: s.Root.OwnerSessionID, RootID: root}, node)
	if !fact.Complete || fact.Disappeared {
		return incompleteBatchStatus(root, node), nil
	}
	return fact, nil
}

func (c *installedBatchModelControl) Status(ctx context.Context, caller, root string) (any, error) {
	if caller == "" || root == "" {
		return nil, errors.New("batch_status requires root id")
	}
	if c.live == nil || c.runner == nil {
		return incompleteBatchStatus(root, ""), nil
	}
	s, err := c.readAuthorized(ctx, caller, root)
	if err != nil {
		return incompleteBatchStatus(root, ""), nil
	}
	if s.Root.RootID != root || s.Root.Kind != "batch" {
		return incompleteBatchStatus(root, ""), nil
	}
	fact := c.live.BatchLevel(ctx, session.BatchLiveRef{OwnerSessionID: s.Root.OwnerSessionID, RootID: root}, root)
	if !fact.Complete || fact.Disappeared {
		return incompleteBatchStatus(root, ""), nil
	}
	if fact.Finished {
		return map[string]any{"root_id": root, "state": fact.Node.State, "terminal": true, "complete": true}, nil
	}
	view, err := c.runner.Snapshot(ctx, jobbatch.ID(root))
	if err != nil {
		return incompleteBatchStatus(root, ""), nil
	}
	return view, nil
}

func (c *installedBatchModelControl) Output(ctx context.Context, caller, root, node string, cursor uint64) (any, error) {
	if c.runner == nil || c.store == nil {
		return nil, errors.New("batch runtime unavailable")
	}
	s, err := c.readAuthorized(ctx, caller, root)
	if err != nil {
		return nil, err
	}
	if s.Root.RootID != root || s.Root.HostID != c.host || s.Root.Kind != "batch" || !batchModelNodeInSnapshot(s.Nodes, node, "task", root) {
		return nil, errors.New("batch_output owner/runtime or descendant mismatch")
	}
	chunk, err := c.runner.Output(ctx, jobbatch.ID(node), cursor)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !errors.Is(err, jobbatch.ErrReleased) {
			return nil, err
		}
		chunk, totalBytes, retainedBytes, truncated, found := c.outputCache.readWithMetadata(jobbatch.ID(node), cursor)
		if found {
			return cachedBatchOutput(chunk, totalBytes, retainedBytes, truncated), nil
		}
		return expiredBatchOutput(cursor), nil
	}
	return liveBatchOutput(chunk), nil
}

func liveBatchOutput(chunk jobbatch.OutputChunk) any {
	// Retained bytes are the live window size, not the end cursor: the
	// difference Next-Oldest, clamped at zero.
	var retained uint64
	if chunk.Next > chunk.Oldest {
		retained = chunk.Next - chunk.Oldest
	}
	return outputResponse(chunk, chunk.Next, int(retained), chunk.Truncated)
}

func cachedBatchOutput(chunk jobbatch.OutputChunk, totalBytes uint64, retainedBytes int, truncated bool) any {
	return outputResponse(chunk, totalBytes, retainedBytes, truncated)
}

func outputResponse(chunk jobbatch.OutputChunk, totalBytes uint64, retainedBytes int, truncated bool) any {
	oldest := chunk.Oldest
	start := chunk.Next - uint64(len(chunk.Data))
	if start < oldest {
		start = oldest
	}
	if start > chunk.Next {
		start = chunk.Next
	}
	data := chunk.Data
	if uint64(len(data)) > batchModelOutputLimit {
		data = data[:batchModelOutputLimit]
		chunk.Next = start + uint64(len(data))
		chunk.EOF = false
		truncated = true
	}
	if chunk.Oldest > start {
		truncated = true
	}
	status := "available"
	if truncated {
		status = "output_truncated"
	}
	return map[string]any{"output_status": status, "data": string(data), "next_cursor": chunk.Next, "oldest_cursor": chunk.Oldest, "eof": chunk.EOF, "truncated": truncated, "total_bytes": totalBytes, "retained_bytes": retainedBytes}
}

func expiredBatchOutput(cursor uint64) any {
	return map[string]any{"output_status": "output_expired", "reason": "expired_or_evicted", "data": "", "next_cursor": cursor, "oldest_cursor": uint64(0), "eof": true, "truncated": false, "total_bytes": uint64(0), "retained_bytes": 0}
}

type batchWorkerStopScopeKey struct{}

func stopBatchWorkerScopeFromContext(ctx context.Context) bool {
	scope, _ := ctx.Value(batchWorkerStopScopeKey{}).(bool)
	return scope
}

func (c *installedBatchModelControl) isSessionDescendant(ctx context.Context, owner, ancestor string) bool {
	if owner == "" || ancestor == "" || c.sessions == nil {
		return false
	}
	ancestorSession, err := c.sessions.Get(ctx, ancestor)
	if err != nil || ancestorSession.ID != ancestor {
		return false
	}
	seen := make(map[string]struct{})
	for current, steps := owner, 0; current != "" && steps < 128; steps++ {
		if current == ancestor {
			return true
		}
		if _, exists := seen[current]; exists {
			return false
		}
		seen[current] = struct{}{}
		sess, err := c.sessions.Get(ctx, current)
		if err != nil || sess.ID != current {
			return false
		}
		current = sess.ParentSessionID
	}
	return false
}

func (c *installedBatchModelControl) readStopRoot(ctx context.Context, caller, root string) (session.BatchSnapshot, error) {
	if c.store == nil || root == "" || c.host == "" {
		return session.BatchSnapshot{}, errors.New("batch_stop root access unavailable")
	}
	roots, err := c.store.LiveRootsForHost(ctx, c.host)
	if err != nil {
		return session.BatchSnapshot{}, err
	}
	for _, candidate := range roots {
		if candidate.RootID != root {
			continue
		}
		return c.readStopCandidate(ctx, candidate, root)
	}
	// Terminal roots are excluded by LiveRootsForHost. Retain idempotence
	// for the caller's existing owned-root ancestry without scanning all sessions.
	if snapshot, readErr := c.readAuthorized(ctx, caller, root); readErr == nil {
		return c.readStopCandidate(ctx, snapshot.Root, root)
	} else if !errors.Is(readErr, sql.ErrNoRows) {
		return session.BatchSnapshot{}, readErr
	}
	return session.BatchSnapshot{}, errors.New("batch_stop root not found among live local batches")
}

func (c *installedBatchModelControl) readStopCandidate(ctx context.Context, candidate db.BatchNode, root string) (session.BatchSnapshot, error) {
	if candidate.OwnerSessionID == "" || candidate.HostID != c.host || candidate.RunnerID == "" || candidate.RunnerID != c.runnerID {
		return session.BatchSnapshot{}, errors.New("batch_stop root has unknown or stale runner incarnation")
	}
	if c.sessions == nil {
		return session.BatchSnapshot{}, errors.New("batch_stop owner lookup unavailable")
	}
	owner, err := c.sessions.Get(ctx, candidate.OwnerSessionID)
	if err != nil || owner.ID != candidate.OwnerSessionID {
		return session.BatchSnapshot{}, errors.New("batch_stop root owner is unknown")
	}
	return c.store.Read(ctx, candidate.OwnerSessionID, root)
}

func (c *installedBatchModelControl) Stop(ctx context.Context, caller, root, node string) (any, error) {
	if c.runner == nil || c.store == nil {
		return nil, errors.New("batch runtime unavailable")
	}
	if caller == "" || root == "" {
		return nil, errors.New("batch_stop requires caller and root id")
	}
	if node == "" {
		node = root
	}
	s, err := c.readStopRoot(ctx, caller, root)
	if err != nil {
		return nil, err
	}
	if stopBatchWorkerScopeFromContext(ctx) {
		if !c.isSessionDescendant(ctx, caller, caller) || !c.isSessionDescendant(ctx, s.Root.OwnerSessionID, caller) {
			return nil, errors.New("batch_stop is limited to batches owned by this worker or its descendants")
		}
	} else if !c.isSessionDescendant(ctx, caller, caller) || (!c.isSessionDescendant(ctx, caller, s.Root.OwnerSessionID) && !c.isSessionDescendant(ctx, s.Root.OwnerSessionID, caller)) {
		return nil, errors.New("batch_stop caller must own the root or be related to its owner")
	}
	if s.Root.RootID != root || s.Root.Kind != "batch" || s.Root.OwnerSessionID == "" || s.Root.HostID != c.host || s.Root.RunnerID == "" || s.Root.RunnerID != c.runnerID || !batchModelNodeInSnapshot(s.Nodes, node, "", root) {
		return nil, errors.New("batch_stop requires an owned local root or descendant in the current runner incarnation")
	}
	if c.sessions == nil {
		return nil, errors.New("batch_stop owner lookup unavailable")
	}
	ownerSession, ownerErr := c.sessions.Get(ctx, s.Root.OwnerSessionID)
	if ownerErr != nil || ownerSession.ID != s.Root.OwnerSessionID {
		return nil, errors.New("batch_stop root owner is unknown")
	}
	if terminalBatchStoreState(s.Root.State) {
		return map[string]any{"accepted": false, "terminal": true, "state": s.Root.State}, nil
	}
	before, found := findBatchNode(s.Nodes, node)
	if !found || before.HostID != c.host || before.RunnerID == "" || before.RunnerID != c.runnerID {
		return nil, errors.New("batch_stop target has unknown or stale runner incarnation")
	}
	req, err := c.store.CreateControl(ctx, s.Root.OwnerSessionID, c.host, root, node)
	if err != nil {
		return nil, err
	}
	if err := c.runner.Stop(ctx, jobbatch.ID(node)); err != nil {
		return nil, err
	}
	latest, err := c.store.Read(ctx, s.Root.OwnerSessionID, root)
	if err != nil {
		return nil, err
	}
	stopped, ok := findBatchNode(latest.Nodes, node)
	if !ok || stopped.ProjectionSequence <= before.ProjectionSequence || stopped.State == "pending" || stopped.State == "starting" || stopped.State == "running" {
		return nil, errors.New("batch_stop runner transition was not durably projected")
	}
	if err := c.store.AnswerControl(ctx, db.AnswerBatchControlRequestParams{
		RequestID: req.RequestID, OwnerSessionID: s.Root.OwnerSessionID, HostID: c.host,
		RootID: root, NodeID: node, RunnerID: c.runnerID, AnswerCode: "accepted",
		AnswerSequence: sql.NullInt64{Int64: stopped.ProjectionSequence, Valid: true},
		AnsweredAt:     sql.NullInt64{Int64: time.Now().Unix(), Valid: true},
	}); err != nil {
		return nil, err
	}
	// The answer acknowledges the committed stop transition; settlement is
	// still reported only by the runner's subsequent terminal projection.
	return map[string]any{"request_id": req.RequestID, "accepted": true, "terminal": false, "state": "cancelling"}, nil
}
