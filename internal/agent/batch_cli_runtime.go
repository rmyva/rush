package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/google/uuid"
)

// SubmitCLI submits an explicitly translated tree through the installed Runner.
func (i *BatchInstall) SubmitCLI(ctx context.Context, owner string, spec jobbatch.Spec) (*jobbatch.Batch, error) {
	return i.SubmitCLIRef(ctx, owner, spec, nil)
}

// SubmitCLIRef is SubmitCLI with a caller-chosen submit reference. ref receives
// the new root id and must return a reference that keeps the "batch-cli:" family
// prefix (such roots do not wake the owner) and is unique per root; nil keeps
// the default "batch-cli:<root>".
func (i *BatchInstall) SubmitCLIRef(ctx context.Context, owner string, spec jobbatch.Spec, ref func(root string) string) (*jobbatch.Batch, error) {
	if i == nil || i.Runner == nil || i.RunnerHandle == nil || i.Runtime == nil || owner == "" {
		return nil, errors.New("batch CLI: installed runner, owner and batch root are required")
	}
	if spec.Batch == nil || spec.Task != nil {
		return nil, errors.New("batch CLI: installed runner, owner and batch root are required")
	}
	if err := validateBatchOwnerPayloads(owner, spec); err != nil {
		return nil, err
	}
	if err := validateBatchRolePayloads(spec); err != nil {
		return nil, err
	}
	_, _, leaves, err := batchSpecNodes([]jobbatch.Spec{spec})
	if err != nil {
		return nil, fmt.Errorf("batch CLI admission: %w", err)
	}
	if len(leaves) == 0 {
		return nil, errors.New("batch CLI: empty task tree is unsupported")
	}
	root := jobbatch.ID(uuid.NewString())
	submitRef := "batch-cli:" + string(root)
	if ref != nil {
		submitRef = ref(string(root))
		if !strings.HasPrefix(submitRef, "batch-cli:") || !strings.HasSuffix(submitRef, string(root)) || !jobbatch.ValidRef(submitRef) {
			return nil, errors.New("batch CLI: invalid submit reference")
		}
	}
	if err := validateBatch(i.Runtime.cfg.Profile, batchRankContext{Root: root}, nil, []jobbatch.Spec{spec}, i.Runtime.cfg.MaxDepth, i.Runtime.cfg.MaxNodes, i.Runtime.cfg.Resolver); err != nil {
		return nil, fmt.Errorf("batch CLI admission: %w", err)
	}
	_, batch, err := i.RunnerHandle.SubmitBound(ctx, jobbatch.SubmitRequest{Root: root, Spec: spec, Ref: submitRef})
	if err != nil {
		return nil, err
	}
	if batch == nil {
		return nil, errors.New("batch CLI: runner returned no batch")
	}
	i.RunnerHandle.mu.Lock()
	i.RunnerHandle.started[root] = batch
	i.RunnerHandle.owners[root] = owner
	i.RunnerHandle.mu.Unlock()
	// Record the CLI root's owning session in the lease ledger (owner-only
	// entry: no disk capability owner exists for CLI roots) and arm the Done
	// watcher so a settled root's owner entry does not linger — the watcher
	// releases the never-acquired caps key (a no-op) and forgets the entry.
	if i.RootLeases != nil {
		if err := i.RootLeases.acquire(root, owner, "", ""); err != nil {
			return nil, fmt.Errorf("batch CLI: record root owner: %w", err)
		}
		_ = i.RootLeases.adopt(root, batch.Done(), "", "", nil)
	}
	return batch, nil
}

func batchRankRoot() jobbatch.ID { return jobbatch.ID(uuid.NewString()) }

func validateBatchOwnerPayloads(owner string, spec jobbatch.Spec) error {
	if (spec.Task == nil) == (spec.Batch == nil) {
		return errors.New("batch CLI: every item must be exactly a task or batch")
	}
	if spec.Task != nil {
		var payload batchLeafPayload
		if err := json.Unmarshal(spec.Task.Payload, &payload); err != nil || payload.SessionID != owner {
			return errors.New("batch CLI: task owner identity does not match submit owner")
		}
		if payload.Tool == AgentToolName {
			var selected struct {
				Role RequestedModelRole `json:"role"`
			}
			if err := json.Unmarshal(payload.Input, &selected); err != nil {
				return err
			}
			if selected.Role != "" && !selected.Role.builtin() && payload.SelectedRoleContract == nil {
				return fmt.Errorf("%w: role snapshot was not admitted", ErrAgentRoleUnconfigured)
			}
			if selected.Role.builtin() && payload.SelectedRoleContract != nil {
				return fmt.Errorf("%w: custom snapshot does not match builtin role", ErrAgentRoleMismatch)
			}
		}
		return nil
	}
	for _, child := range spec.Batch.Items {
		if err := validateBatchOwnerPayloads(owner, child); err != nil {
			return err
		}
	}
	return nil
}

func (i *BatchInstall) OwnsCLI(root jobbatch.ID) bool {
	return i != nil && i.RunnerHandle != nil && i.RunnerHandle.ownerFor(root) != ""
}

func (i *BatchInstall) CLIOwner(root jobbatch.ID) string {
	if i == nil || i.RunnerHandle == nil {
		return ""
	}
	return i.RunnerHandle.ownerFor(root)
}

func (i *BatchInstall) commitCLIControl(ctx context.Context, request db.BatchControlRequest) (session.BatchControlAccepted, error) {
	if i == nil || i.Runner == nil || i.Runtime == nil {
		return session.BatchControlAccepted{}, errors.New("batch CLI: runtime unavailable")
	}
	if request.HostID != i.Runtime.cfg.Host || request.RunnerID != i.Runtime.cfg.RunnerID || i.CLIOwner(jobbatch.ID(request.RootID)) != request.OwnerSessionID {
		return session.BatchControlAccepted{}, session.ErrBatchControlStale
	}
	if seq, ok := i.cliAccepted[request.RequestID]; ok {
		return session.BatchControlAccepted{Code: "accepted", Sequence: seq}, nil
	}
	snapshot, err := i.Runner.Snapshot(ctx, jobbatch.ID(request.RootID))
	if err != nil {
		return session.BatchControlAccepted{}, err
	}
	found := snapshot.Node.ID == jobbatch.ID(request.NodeID)
	terminal := found && isTerminalBatchState(snapshot.Node.State)
	for _, node := range snapshot.Descendants {
		if node.ID == jobbatch.ID(request.NodeID) {
			found = true
			terminal = isTerminalBatchState(node.State)
			break
		}
	}
	if !found {
		return session.BatchControlAccepted{}, session.ErrBatchControlStale
	}
	if terminal {
		return session.BatchControlAccepted{Code: "terminal", Sequence: int64(snapshot.Sequence)}, nil
	}
	if err = i.Runner.Stop(ctx, jobbatch.ID(request.NodeID)); err != nil {
		return session.BatchControlAccepted{}, err
	}
	snapshot, err = i.Runner.Snapshot(ctx, jobbatch.ID(request.RootID))
	if err != nil {
		return session.BatchControlAccepted{}, err
	}
	if snapshot.Sequence <= 0 {
		return session.BatchControlAccepted{}, errors.New("batch CLI: invalid committed sequence")
	}
	i.cliAccepted[request.RequestID] = int64(snapshot.Sequence)
	return session.BatchControlAccepted{Code: "accepted", Sequence: int64(snapshot.Sequence)}, nil
}

func isTerminalBatchState(state jobbatch.State) bool {
	switch state {
	case jobbatch.StateCompleted, jobbatch.StateFailed, jobbatch.StateCancelled, jobbatch.StateTimedOut, jobbatch.StateInterrupted, jobbatch.StateSkipped:
		return true
	}
	return false
}

func (i *BatchInstall) PumpCLIControls(ctx context.Context, owner string, root jobbatch.ID) error {
	if i == nil || i.CLIPump == nil {
		return errors.New("batch CLI: control pump unavailable")
	}
	_, err := i.CLIPump.Pump(ctx, owner, string(root))
	return err
}

func (i *BatchInstall) RetainCLIConstruction(root jobbatch.ID, release func()) error {
	if i == nil || i.RunnerHandle == nil || release == nil {
		return errors.New("batch CLI: construction retention unavailable")
	}
	batch := i.RunnerHandle.batchFor(root)
	if batch == nil {
		return errors.New("batch CLI: root is not owned by this installed Runner")
	}
	go func() {
		<-batch.Done()
		release()
	}()
	return nil
}

// ErrBatchCLIOutputExpired reports that a released leaf's output is no
// longer cached; its persisted outcome is still valid.
var ErrBatchCLIOutputExpired = errors.New("batch CLI: output expired or evicted")

func (i *BatchInstall) CLIOutput(ctx context.Context, owner string, root, node jobbatch.ID) (jobbatch.OutputChunk, error) {
	if i == nil || i.RunnerHandle == nil || i.Runner == nil || i.CLIOwner(root) != owner {
		return jobbatch.OutputChunk{}, errors.New("batch CLI: root is not owned by this session")
	}
	chunk, err := i.Runner.Output(ctx, node, 0)
	if err == nil {
		return chunk, nil
	}
	if !errors.Is(err, jobbatch.ErrReleased) {
		return jobbatch.OutputChunk{}, err
	}
	cached, _, _, _, ok := i.OutputCache.readWithMetadata(node, 0)
	if !ok {
		return jobbatch.OutputChunk{}, ErrBatchCLIOutputExpired
	}
	return cached, nil
}

func (i *BatchInstall) ReleaseCLIAfterResult(root jobbatch.ID) error {
	if i == nil || i.RunnerHandle == nil || i.Runner == nil {
		return errors.New("batch CLI: runtime unavailable")
	}
	batch := i.RunnerHandle.batchFor(root)
	if batch == nil {
		return errors.New("batch CLI: root is not owned by this installed Runner")
	}
	if _, err := batch.Wait(context.Background()); err != nil {
		return err
	}
	if err := i.Runner.Release(context.Background(), root); err != nil && !errors.Is(err, jobbatch.ErrReleased) {
		return err
	}
	return nil
}

func (i *BatchInstall) WaitCLI(ctx context.Context, root jobbatch.ID) (jobbatch.Summary, error) {
	if i == nil || i.RunnerHandle == nil {
		return jobbatch.Summary{}, errors.New("batch CLI: runtime unavailable")
	}
	batch := i.RunnerHandle.batchFor(root)
	if batch == nil {
		return jobbatch.Summary{}, errors.New("batch CLI: root is not owned by this installed Runner")
	}
	return batch.Wait(ctx)
}

// AddCLI admits additions only for roots created by this exact install.
func (i *BatchInstall) AddCLI(ctx context.Context, root, parent jobbatch.ID, items []jobbatch.Spec) error {
	if i == nil || i.Runner == nil || i.RunnerHandle == nil || i.Runtime == nil {
		return errors.New("batch CLI: runtime unavailable")
	}
	owner := i.RunnerHandle.ownerFor(root)
	if owner == "" {
		return errors.New("batch CLI: root is not owned by this installed runtime")
	}
	if len(items) == 0 {
		return errors.New("batch CLI: add requires items")
	}
	snapshot, err := i.Runner.Snapshot(ctx, root)
	if err != nil {
		return err
	}
	parentDepth := 0
	nodes := make(map[jobbatch.ID]jobbatch.NodeView, 1+len(snapshot.Descendants))
	nodes[snapshot.Node.ID] = snapshot.Node
	for _, node := range snapshot.Descendants {
		nodes[node.ID] = node
	}
	if _, ok := nodes[parent]; !ok {
		return errors.New("batch CLI: parent is not in the requested root")
	}
	for current := parent; current != ""; {
		node, ok := nodes[current]
		if !ok {
			return errors.New("batch CLI: parent ancestry is incomplete")
		}
		parentDepth++
		current = node.Parent
	}
	for _, item := range items {
		if err := validateBatchOwnerPayloads(owner, item); err != nil {
			return err
		}
		if err := validateBatchRolePayloads(item); err != nil {
			return err
		}
	}
	if err := validateBatch(i.Runtime.cfg.Profile, batchRankContext{Root: root}, nil, items, i.Runtime.cfg.MaxDepth-parentDepth, i.Runtime.cfg.MaxNodes-(1+len(snapshot.Descendants)), i.Runtime.cfg.Resolver); err != nil {
		return fmt.Errorf("batch CLI add admission: %w", err)
	}
	_, err = i.RunnerHandle.Add(ctx, jobbatch.AddRequest{Parent: parent, Items: items, Ref: "batch-cli-add:" + string(root)})
	return err
}
