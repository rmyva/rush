package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// R11F adapters: production implementations of every R11C/R11D/R11E seam
// over the real engine and the session store. Adapters only; nothing here is
// wired into a session or the App (flagged installation stays in M2/M3).

// batchRunnerAdapter adapts *jobbatch.Runner onto the batchRunnerHandle
// seam. Direct calls submit ONE-item task specs; the engine requires a batch
// root, so the adapter wraps the task with jobbatch.OneItem (NotifyAll, D10)
// and keeps the returned Batch for the done source. The durable reservation
// is written by the commit hook during engine Submit.
type batchRunnerAdapter struct {
	outputCache *batchOutputCache
	runner      *jobbatch.Runner
	credentials *batchCredentialEscrow
	// rootLeases is the per-root disk-binding lease store built at install
	// time; the runtime and the model control share this one instance.
	rootLeases *batchRootLeaseStore
	mu         sync.Mutex
	started    map[jobbatch.ID]*jobbatch.Batch
	owners     map[jobbatch.ID]string
}

func newBatchRunnerAdapter(runner *jobbatch.Runner) *batchRunnerAdapter {
	return &batchRunnerAdapter{runner: runner, credentials: newBatchCredentialEscrow(), started: make(map[jobbatch.ID]*jobbatch.Batch), owners: make(map[jobbatch.ID]string)}
}

var _ batchRunnerHandle = (*batchRunnerAdapter)(nil)

func (a *batchRunnerAdapter) OutputCache() *batchOutputCache { return a.outputCache }

// Submit starts the caller-supplied root. The returned ID is always the
// requested root (ingress contract: runner root must equal requested root).
func (a *batchRunnerAdapter) Submit(ctx context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, error) {
	batch, err := a.submitBatch(ctx, req)
	if err != nil {
		return "", err
	}
	_ = batch
	return req.Root, nil
}

func (a *batchRunnerAdapter) SubmitBound(ctx context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, *jobbatch.Batch, error) {
	batch, err := a.submitBatch(ctx, req)
	if err != nil {
		return "", nil, err
	}
	return req.Root, batch, nil
}

func (a *batchRunnerAdapter) submitBatch(ctx context.Context, req jobbatch.SubmitRequest) (*jobbatch.Batch, error) {
	// R11F P1-6: a deferred install binds the real Runner at Start. An
	// Invoke that races ahead of Start has no Runner and fails closed.
	if a.runner == nil {
		return nil, errors.New("batch runner adapter: runner not constructed yet (Start did not run)")
	}
	var err error
	req.Spec, err = stampBatchCallCredentials(ctx, req.Spec)
	if err != nil {
		return nil, err
	}
	if req.Spec.Task != nil {
		if req.Spec.Batch != nil {
			return nil, jobbatch.ErrInvalidSpec
		}
		req.Spec = jobbatch.OneItem(*req.Spec.Task)
	} else if req.Spec.Batch == nil {
		return nil, jobbatch.ErrInvalidSpec
	}
	registrations, err := a.credentialRegistrations(ctx, string(req.Root), req.Spec)
	if err != nil {
		return nil, err
	}
	if len(registrations) > 0 && a.credentials == nil {
		return nil, errBatchCredentialEscrowMissing
	}
	release := func() {}
	if a.credentials != nil {
		release, err = a.credentials.registerBatch(registrations)
		if err != nil {
			return nil, err
		}
	}
	batch, err := a.runner.Submit(ctx, jobbatch.SubmitRequest{Root: req.Root, Spec: req.Spec, Ref: req.Ref})
	if err != nil {
		release()
		return nil, err
	}
	a.mu.Lock()
	if a.started == nil {
		a.started = make(map[jobbatch.ID]*jobbatch.Batch)
	}
	a.started[req.Root] = batch
	a.mu.Unlock()
	return batch, nil
}

// Stop requests cancellation of one root.
func (a *batchRunnerAdapter) Stop(ctx context.Context, id jobbatch.ID) error {
	return a.runner.Stop(ctx, id)
}

// Release disposes a settled root's retained execution (the batchReleaseHandle
// seam): the ingress/model control adopt this as the automatic post-Done
// disposal trigger, so the adapter must surface the real Runner's Release.
func (a *batchRunnerAdapter) Release(ctx context.Context, id jobbatch.ID) error {
	err := a.runner.Release(ctx, id)
	if err == nil && a.credentials != nil {
		a.credentials.releaseRoot(string(id))
	}
	return err
}

func (a *batchRunnerAdapter) Add(ctx context.Context, req jobbatch.AddRequest) ([]jobbatch.ID, error) {
	view, err := a.runner.View(ctx, req.Parent)
	if err != nil {
		return nil, err
	}
	root := string(view.Root)
	registrations := make([]batchCredentialRegistration, 0)
	stamped := make([]jobbatch.Spec, len(req.Items))
	for i, spec := range req.Items {
		var err error
		stamped[i], err = stampBatchCallCredentials(ctx, spec)
		if err != nil {
			return nil, err
		}
	}
	req.Items = stamped
	for _, spec := range req.Items {
		entries, collectErr := a.credentialRegistrations(ctx, root, spec)
		if collectErr != nil {
			return nil, collectErr
		}
		registrations = append(registrations, entries...)
	}
	if a.credentials == nil {
		if len(registrations) > 0 {
			return nil, errBatchCredentialEscrowMissing
		}
		ids, err := a.runner.Add(ctx, req)
		return ids, err
	}
	release, err := a.credentials.registerBatch(registrations)
	if err != nil {
		return nil, err
	}
	ids, err := a.runner.Add(ctx, req)
	if err != nil {
		release()
	}
	return ids, err
}

func (a *batchRunnerAdapter) credentialRegistrations(ctx context.Context, root string, spec jobbatch.Spec) ([]batchCredentialRegistration, error) {
	if spec.Task != nil {
		if spec.Task.Kind != batchKindAgent {
			return nil, nil
		}
		var p batchLeafPayload
		if err := json.Unmarshal(spec.Task.Payload, &p); err != nil {
			return nil, err
		}
		if p.Tool != "agent" && p.Tool != "agentic_fetch" {
			return nil, nil
		}
		if !p.CallCredentials {
			return nil, nil
		}
		if callCredentialsFrom(ctx) == nil {
			return nil, errBatchCredentialEscrowMissing
		}
		return []batchCredentialRegistration{{key: batchCredentialKeyFor(p), root: root, creds: callCredentialsFrom(ctx)}}, nil
	}
	if spec.Batch == nil {
		return nil, jobbatch.ErrInvalidSpec
	}
	var all []batchCredentialRegistration
	for _, item := range spec.Batch.Items {
		entries, err := a.credentialRegistrations(ctx, root, item)
		if err != nil {
			return nil, err
		}
		all = append(all, entries...)
	}
	return all, nil
}

func (a *batchRunnerAdapter) Snapshot(ctx context.Context, id jobbatch.ID) (jobbatch.BatchView, error) {
	return a.runner.Snapshot(ctx, id)
}

func (a *batchRunnerAdapter) View(ctx context.Context, id jobbatch.ID) (jobbatch.NodeView, error) {
	return a.runner.View(ctx, id)
}

func (a *batchRunnerAdapter) Output(ctx context.Context, id jobbatch.ID, cursor uint64) (jobbatch.OutputChunk, error) {
	return a.runner.Output(ctx, id, cursor)
}

// batchFor returns the in-process Batch of a root started by THIS adapter,
// or nil (a root attached after a restart is polled durably instead).
func (a *batchRunnerAdapter) batchFor(root jobbatch.ID) *jobbatch.Batch {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.started[root]
}

func (a *batchRunnerAdapter) rememberOwner(root jobbatch.ID, owner string) {
	a.mu.Lock()
	a.owners[root] = owner
	a.mu.Unlock()
}

func (a *batchRunnerAdapter) ownerFor(root jobbatch.ID) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.owners[root]
}

// batchAttachAdapter adapts session.BatchStore onto the R11B attach seam.
type batchAttachAdapter struct{ store *session.BatchStore }

var _ batchAttachStore = batchAttachAdapter{}

// ProbeLive is the read-only fence probe; reservations happen in the commit
// hook at Submit, never here.
func (a batchAttachAdapter) ProbeLive(ctx context.Context, p batchAttachProbe) (jobbatch.ID, bool, error) {
	root, ok, err := a.store.BatchProbeLive(ctx, session.BatchLiveProbe{
		Owner: p.Owner, Host: p.Host, RunnerID: p.RunnerID,
		ToolCallID: p.ToolCallID, InputHash: p.InputHash,
		SubmittedMessageID: p.SubmittedMessageID, SubmittedRegistryID: p.SubmittedRegistryID,
	})
	return jobbatch.ID(root), ok, err
}

// batchStoreDoneSource implements the R11D completion seam over the in-process
// Runner and the durable response envelope. It NEVER invents a result: every
// returned envelope is the executor's own ToolResponse, or the store's
// persisted projection of it.
type batchStoreDoneSource struct {
	store *session.BatchStore
	// batches resolves a root started in this process (nil-able lookup).
	batches func(jobbatch.ID) *jobbatch.Batch
	// poll is the durable re-read cadence for roots without a live Batch;
	// zero selects batchDonePollInterval.
	poll time.Duration
}

var _ batchDoneSource = (*batchStoreDoneSource)(nil)

// batchDonePollInterval is the fallback durable poll cadence.
const batchDonePollInterval = 250 * time.Millisecond

// AwaitDone waits for the root's REAL completion: a live Batch blocks on the
// engine (never a stop acknowledgement), then loads the persisted envelope;
// a foreign root polls the durable envelope only. ok=false means held
// termination or a caller-cancelled wait, never a fabricated settlement.
func (d *batchStoreDoneSource) AwaitDone(ctx context.Context, owner string, root jobbatch.ID) (fantasy.ToolResponse, bool, error) {
	if err := ctx.Err(); err != nil {
		return fantasy.ToolResponse{}, false, err
	}
	if b := d.batches(root); b != nil {
		if _, err := b.Wait(ctx); err != nil {
			return fantasy.ToolResponse{}, false, err
		}
		resp, ok, err := d.LoadDone(ctx, owner, root)
		return resp, ok, callerCancelOr(ctx, err)
	}
	tick := time.NewTicker(d.pollInterval())
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return fantasy.ToolResponse{}, false, err
		}
		resp, ok, err := d.LoadDone(ctx, owner, root)
		if err != nil || ok {
			return resp, ok, callerCancelOr(ctx, err)
		}
		select {
		case <-ctx.Done():
			return fantasy.ToolResponse{}, false, ctx.Err()
		case <-tick.C:
		}
	}
}

// LoadDone reads an already-persisted completion without waiting (R11F
// P1-2): the one-item completion read — the root envelope when present, else
// the settled single task child's envelope (the Settled commit hook writes
// the envelope on the settled NODE, root.1, not the root). Envelope
// details carry the executor's full ToolResponse (batchEnvelope); a settled
// row without details is projected honestly from its stored summary and
// outcome state (state failed => IsError), never fabricated.
func (d *batchStoreDoneSource) LoadDone(ctx context.Context, owner string, root jobbatch.ID) (fantasy.ToolResponse, bool, error) {
	e, ok, err := d.store.BatchDeliveryOneItemCompletion(ctx, owner, string(root))
	if err != nil || !ok {
		return fantasy.ToolResponse{}, false, err
	}
	if len(e.Details) > 0 {
		var resp fantasy.ToolResponse
		if err := json.Unmarshal(e.Details, &resp); err != nil {
			return fantasy.ToolResponse{}, false, fmt.Errorf("batch done source: envelope %s: %w", e.Ref, err)
		}
		return resp, true, nil
	}
	resp := fantasy.NewTextResponse(e.Summary)
	// interrupted is the recovery outcome of a lost host: an error-shaped result, never a success.
	resp.IsError = e.State == "failed" || e.State == "timed_out" || e.State == "cancelled" || e.State == "interrupted"
	return resp, true, nil
}

func (d *batchStoreDoneSource) pollInterval() time.Duration {
	if d.poll > 0 {
		return d.poll
	}
	return batchDonePollInterval
}

// batchStoreElector implements the R8 election seam over the typed store
// elections. CAS semantics: exactly one winner, losers report false and
// insert no history (session.ErrBatchAnnouncementLost maps to won=false).
type batchStoreElector struct {
	store    *session.BatchStore
	messages message.Service
}

var _ batchElector = batchStoreElector{}

// ElectStarted wins-or-loses the durable started history for the root.
func (e batchStoreElector) ElectStarted(ctx context.Context, owner string, root jobbatch.ID) (bool, error) {
	params := message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: batchStartedNoticeText(root)}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
	}
	_, won, err := e.store.BatchDeliveryElectStarted(ctx, e.messages, owner, string(root), params)
	return won, err
}

// ElectInline publishes the elected completion exactly once. The history
// message carries the envelope's content; a media-only response is serialised
// so no payload is lost.
func (e batchStoreElector) ElectInline(ctx context.Context, owner string, root jobbatch.ID, envelope fantasy.ToolResponse) (bool, error) {
	_, won, err := e.store.BatchDeliveryElectInline(ctx, e.messages, owner, string(root),
		batchCompletionMessageParams(batchEnvelopeText(envelope)))
	return won, err
}

func batchStartedNoticeText(root jobbatch.ID) string {
	return fmt.Sprintf("batch root %s started; completions arrive as selected notices. Call await_tasks to wait for it.", root)
}

func batchCompletionMessageParams(text string) message.CreateMessageParams {
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: text}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
	}
}

// batchEnvelopeText renders a completion envelope for history: the content
// when present, else the serialised envelope (media/metadata preserved).
func batchEnvelopeText(envelope fantasy.ToolResponse) string {
	if envelope.Content != "" {
		return envelope.Content
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return ""
	}
	return string(raw)
}

// batchStoreNoticePull implements the drain seam over the R8 outbox pull.
// Pull consumes only ALREADY-SELECTED notices; it never waits for running
// batch work.
type batchStoreNoticePull struct {
	store    *session.BatchStore
	messages message.Service
	owner    string
}

var _ batchNoticePull = batchStoreNoticePull{}

// Pull drains the owner's pending selected notices and reports the count
// actually delivered into history.
func (p batchStoreNoticePull) Pull(ctx context.Context) (int, error) {
	pulled, err := p.store.BatchPullNotices(ctx, p.messages, p.owner, batchPulledNoticeParams)
	if err != nil {
		return 0, err
	}
	return len(pulled), nil
}

// batchPulledNoticeParams formats one selected notice for history from the
// row's own facts; nothing is re-derived from live state.
func batchPulledNoticeParams(row session.BatchNoticeRow) message.CreateMessageParams {
	origin := message.OriginWeb
	if row.OriginCLI {
		origin = message.OriginCLI
	}
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: batchNoticeText(row)}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
		Origin:              origin,
	}
}

func batchNoticeText(row session.BatchNoticeRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "batch %s node %s: kind %q state %q", row.RootID, row.NodeID, row.Kind, row.State)
	if row.Cause != "" {
		fmt.Fprintf(&b, " cause %q", row.Cause)
	}
	if row.OutcomeCode != "" || row.OutcomeSummary != "" {
		fmt.Fprintf(&b, " outcome %s %s", row.OutcomeCode, row.OutcomeSummary)
	}
	fmt.Fprintf(&b, " counts pending=%d active=%d completed=%d failed=%d cancelled=%d timed_out=%d interrupted=%d skipped=%d",
		row.Pending, row.Active, row.Completed, row.Failed, row.Cancelled, row.TimedOut, row.Interrupted, row.Skipped)
	return b.String()
}

// BatchLeafRegistryStore is the pinned R1 registry resolver keyed by the
// invocation's registry identity. Registration is pinned-once: an identity
// can never be replaced, so a live call's leaf set cannot change under it.
// Lookup fails closed on an unknown identity.
type BatchLeafRegistryStore struct {
	mu          sync.RWMutex
	registries  map[string]*policyLeafRegistry
	credentials *batchCredentialEscrow
}

func (s *BatchLeafRegistryStore) credentialEscrowLocked() *batchCredentialEscrow {
	if s.credentials == nil {
		s.credentials = newBatchCredentialEscrow()
	}
	return s.credentials
}

func (s *BatchLeafRegistryStore) credentialEscrow() *batchCredentialEscrow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credentialEscrowLocked()
}

func NewBatchLeafRegistryStore() *BatchLeafRegistryStore {
	return &BatchLeafRegistryStore{registries: make(map[string]*policyLeafRegistry), credentials: newBatchCredentialEscrow()}
}

// Register pins one registry under its identity. Double registration of the
// same identity is refused, whether the registry differs or not.
func (s *BatchLeafRegistryStore) Register(registryID string, registry *policyLeafRegistry) error {
	if strings.TrimSpace(registryID) == "" {
		return errors.New("batch leaf registry: empty registry identity")
	}
	if registry == nil {
		return errors.New("batch leaf registry: nil registry")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.registries[registryID]; exists {
		return fmt.Errorf("batch leaf registry: %q is already pinned", registryID)
	}
	copyRegistry := &policyLeafRegistry{entries: make(map[string]policyLeafEntry, len(registry.entries))}
	for name, entry := range registry.entries {
		if name == "agent" || name == "agentic_fetch" {
			entry.tool = &batchCredentialLeaf{AgentTool: entry.tool, escrow: s.credentialEscrowLocked()}
		}
		copyRegistry.entries[name] = entry
	}
	s.registries[registryID] = copyRegistry
	return nil
}

// Resolve returns the pinned registry of the identity; an unknown identity
// fails closed (never an empty registry).
func (s *BatchLeafRegistryStore) Resolve(registryID string) (*policyLeafRegistry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	registry, ok := s.registries[registryID]
	if !ok {
		return nil, fmt.Errorf("batch leaf registry: unknown pinned registry identity %q", registryID)
	}
	return registry, nil
}

// Delete removes one registry identity from the index. It is invoked ONLY
// by the capability manager when a construction record dies (no owner
// names the registry any more); it is not an ownership operation.
func (s *BatchLeafRegistryStore) Delete(registryID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.registries, registryID)
}

func (s *BatchLeafRegistryStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.registries)
}

// Runner's router: generic and MCP leaves through the pinned policy leaf,
// shell/argv with their retained handles, agent delegation via R7c.
func newBatchExecutorFamilies(agentDeps batchAgentDeps) map[string]batchLeafFamily {
	generic := newBatchGenericFamily()
	return map[string]batchLeafFamily{
		batchKindGeneric: generic,
		batchKindMCP:     generic,
		batchKindShell:   batchShellFamily{},
		batchKindArgv:    batchArgvFamily{},
		batchKindAgent:   newBatchAgentFamily(agentDeps),
	}
}

// BatchInstallDeps carries everything one flagged install needs. All fields
// are required except Executor, Limits, CommitTimeout, ControlTimeout and
// RecordRetention (zero selects the engine defaults). Construction makes no
// store or engine calls; only InstallBatchRuntime does.
type BatchInstallDeps struct {
	// Host, RunnerID and Owner are the commit-hook identity: one Runner, one
	// owner session (session.BatchCommitIdentity contract).
	Host, RunnerID, Owner string
	// Profile is the R11A closed capacity profile.
	Profile *BatchProfile
	// Registries is the pinned R1 registry resolver (identity index only:
	// retention is the capability manager's owner set, never refs).
	Registries *BatchLeafRegistryStore
	// Store and Messages are the persistence and history services.
	Store    *session.BatchStore
	Messages message.Service
	// Executor optionally replaces the default R7 router; nil builds the
	// default generic/MCP/shell/argv/agent registration with the AgentDeps
	// below (zero deps keep the M1 fail-closed agent family).
	Executor jobbatch.Executor
	// DecorateExecution is an optional per-install wrapper applied to each
	// successfully started default-family execution before capability leases.
	// Nil leaves the production execution path unchanged.
	DecorateExecution func(jobbatch.Execution) jobbatch.Execution
	// AgentDeps wires the R7c agent executor family (R11F/M3 coordinator
	// seams); zero keeps the M1 fail-closed registration.
	AgentDeps BatchAgentDeps
	// Resolver resolves MCP admission capabilities (R11A); nil rejects
	// every MCP leaf as unresolved.
	Resolver BatchMCPCapabilityResolver
	// Limits, CommitTimeout, ControlTimeout and RecordRetention mirror
	// jobbatch.RunnerConfig; zero values select the engine defaults.
	Limits                        jobbatch.Limits
	CommitTimeout, ControlTimeout time.Duration
	RecordRetention               int
	// DonePoll is the done source's durable poll cadence; zero selects the
	// default.
	DonePoll time.Duration
	// CompletionWake, when non-nil, receives the touched root after every
	// successful commit that selected completion notices (the ordinary
	// completion wake bridge; the App wires the bounded timeout-wake
	// dispatcher). Nil keeps the plain durable commit hook.
	CompletionWake func(context.Context, string)
	QuestionWake   func(context.Context, string)
	// ArmSupervision, when non-nil, arms the invoking session's supervision
	// timer when a batch delivery answers with the started response (psv
	// legacy-parity restoration). The app factory wires
	// PsvBatchSupervisionArmingLate over the late-bound coordinator. Nil
	// (older wiring, tests) arms nothing.
	ArmSupervision func(context.Context, string)
	Sessions       session.Service
	DiskBindings   *DiskProviderRegistry
}

// pwCompletionWakeHook wraps a durable batch commit hook with the ordinary
// selected-completion wake. After a SUCCESSFUL commit whose transition
// selected completion notices, it calls wake with the touched root; the
// receiving dispatcher resolves the owner from the durable row. Host-closing
// mirrors the wrapped hook's sticky closing flag: a HostClosing commit (and
// every later interrupted settlement, whose notices persist wake=0) never
// wakes, so shutdown and recovery launches nothing. The wake is fired
// AFTER the commit returned nil and is nonblocking; a failed commit never
// wakes.
func pwCompletionWakeHook(next func(context.Context, jobbatch.Transition) error, wake func(context.Context, string)) func(context.Context, jobbatch.Transition) error {
	closing := false
	return func(ctx context.Context, t jobbatch.Transition) error {
		err := next(ctx, t)
		if err == nil && t.Event.Type == jobbatch.EventHostClosing {
			closing = true
		}
		if err != nil || closing || wake == nil || len(t.Notices) == 0 {
			return err
		}
		// The touched roots come from the SELECTED NOTICES: Event.Root is
		// zero for a settled/control event (only submit and release carry
		// it), so it cannot identify the root of an ordinary completion.
		var woken map[jobbatch.ID]bool
		for _, n := range t.Notices {
			if n.Root == "" || woken[n.Root] {
				continue
			}
			if woken == nil {
				woken = make(map[jobbatch.ID]bool, len(t.Notices))
			}
			woken[n.Root] = true
			wake(ctx, string(n.Root))
		}
		return nil
	}
}

// BatchInstall is one fully adapted, still-dormant installation.
type BatchInstall struct {
	mu        sync.Mutex
	runnerCfg jobbatch.RunnerConfig

	Runner       *jobbatch.Runner
	CommitHook   func(context.Context, jobbatch.Transition) error
	Runtime      *batchRuntime
	RunnerHandle *batchRunnerAdapter
	Attach       batchAttachStore
	Delivery     batchDelivery
	Done         batchDoneSource
	Elector      batchElector
	NoticePull   batchNoticePull
	Sessions     session.Service
	CLIPump      *session.BatchControlPump
	DiskBindings *DiskProviderRegistry
	// RootLeases is the per-root capability identity store the model control
	// shares with the runtime; exported so app wiring can pass it through
	// (NewBatchModelControl) without a runtime accessor.
	RootLeases  *BatchRootLeaseStore
	OutputCache *batchOutputCache
	cliAccepted map[string]int64
}

// InstallBatchRuntime builds the real Runner, commit hook, attach store,
// done source, elector, delivery seam, notice pull and ingress runtime.
// The Runner's engine loop starts here (inert until Submit); the app fault
// watcher does NOT start until its own Start is called.
func InstallBatchRuntime(ctx context.Context, deps BatchInstallDeps) (*BatchInstall, error) {
	install, err := newBatchInstall(ctx, deps, true)
	if err != nil {
		return nil, err
	}
	runner, err := install.BuildRunner(ctx)
	if err != nil {
		return nil, err
	}
	install.Runner = runner
	return install, nil
}

// batchDormantInstallOwner is the dormant DEFAULT commit-hook/delivery owner
// used when an install carries no fixed owner (R11F P1-1: the flagged app
// install no longer pins the host UUID as owner). It is NEVER written as an
// owner_session_id: the commit hook resolves every Submit's owner from the
// spec payload (BatchCommitIdentity.OwnerFor) and every delivery fences by
// the invocation's session (batchDeliveryImpl.owner).
const batchDormantInstallOwner = "batch-install:per-invocation-owner"

// batchCommitOwnerFor is the P1-1 OwnerFor resolver: the INVOKING session
// carried in the one-item spec payload (batchLeafPayload.SessionID). The
// runner adapter submits jobbatch.OneItem(*req.Spec.Task) — a BATCH-root
// spec (OneItem: Mode parallel, NotifyAll, Items=[{Task}]) whose task rides
// in spec.Batch.Items[0].Task — so the resolver walks the batch items too.
// An unparseable spec (or a spec with neither shape) keeps the dormant
// default: the commit hook falls back and the fixed/dormant Owner applies.
func batchCommitOwnerFor(spec jobbatch.Spec) string {
	if spec.Task != nil {
		var p batchLeafPayload
		if err := json.Unmarshal(spec.Task.Payload, &p); err != nil {
			return ""
		}
		return p.SessionID
	}
	if spec.Batch == nil {
		return ""
	}
	for _, child := range spec.Batch.Items {
		if owner := batchCommitOwnerFor(child); owner != "" {
			return owner
		}
	}
	return ""
}

// InstallBatchRuntimeDeferred builds everything InstallBatchRuntime builds
// EXCEPT the Runner (R11F P1-6: no engine goroutines until BatchRuntime.Start
// constructs the Runner through BatchInstall.BuildRunner via the runtime's
// RunnerFactory). The runner handle adapter is wired into the runtime
// immediately; it fails closed on Submit until BuildRunner attaches the real
// Runner.
func InstallBatchRuntimeDeferred(ctx context.Context, deps BatchInstallDeps) (*BatchInstall, error) {
	return newBatchInstall(ctx, deps, false)
}

// newBatchInstall is the shared construction core. withRunner=false keeps
// the Runner unconstructed (dormant install).
func newBatchInstall(_ context.Context, deps BatchInstallDeps, withRunner bool) (*BatchInstall, error) {
	// R11F P1-1: Owner is no longer required — the flagged install resolves
	// the per-invocation owner from the spec payload; a fixed Owner is only
	// the dormant default.
	switch {
	case deps.Host == "" || deps.RunnerID == "":
		return nil, errors.New("batch install: host and runner id are required")
	case deps.Profile == nil:
		return nil, errors.New("batch install: profile is required")
	case deps.Registries == nil:
		return nil, errors.New("batch install: pinned registry store is required")
	case deps.Store == nil:
		return nil, errors.New("batch install: batch store is required")
	case deps.Messages == nil:
		return nil, errors.New("batch install: message service is required")
	}
	owner := deps.Owner
	if owner == "" || owner == batchDormantInstallOwner {
		owner = batchDormantInstallOwner
	}
	cache := newBatchOutputCache()
	runnerAdapter := &batchRunnerAdapter{outputCache: cache, credentials: deps.Registries.credentialEscrow(), started: make(map[jobbatch.ID]*jobbatch.Batch), owners: make(map[jobbatch.ID]string)}
	done := &batchStoreDoneSource{store: deps.Store, batches: runnerAdapter.batchFor, poll: deps.DonePoll}
	elector := batchStoreElector{store: deps.Store, messages: deps.Messages}
	delivery, err := newBatchDelivery(batchDeliveryDeps{
		Owner: owner, Done: done, Elect: elector, Runner: runnerAdapter, ArmSupervision: deps.ArmSupervision,
		ToolResultReceipt: func(ctx context.Context, owner string, root jobbatch.ID, callID string, sink *toolResultReceiptSink) error {
			if sink == nil {
				return nil
			}
			snapshot, err := deps.Store.Read(ctx, owner, string(root))
			if err != nil {
				return err
			}
			n := snapshot.Root
			if !n.ToolCallID.Valid || n.ToolCallID.String == "" || n.ToolCallID.String != callID || n.OwnerSessionID != owner || n.RootID != string(root) || n.NodeID != n.RootID || n.HostID == "" || n.RunnerID == "" {
				return errors.New("batch tool result: root identity mismatch")
			}
			return sink.add(toolResultReceipt{
				identity: session.BatchToolResultIdentity{Owner: owner, RootID: n.RootID, NodeID: n.NodeID, HostID: n.HostID, RunnerID: n.RunnerID, ToolCallID: n.ToolCallID.String},
				persist: func(ctx context.Context, params message.CreateMessageParams) (message.Message, bool, error) {
					return deps.Store.BatchConsumeToolResult(ctx, deps.Messages, owner,
						session.BatchToolResultIdentity{Owner: owner, RootID: n.RootID, NodeID: n.NodeID, HostID: n.HostID, RunnerID: n.RunnerID, ToolCallID: n.ToolCallID.String},
						params)
				},
			})
		},
	})
	if err != nil {
		return nil, err
	}
	if deps.DiskBindings == nil {
		return nil, errors.New("batch install: app-owned disk provider registry is required")
	}
	// The capability manager drops identity-index entries when a construction
	// record dies — the index is a lookup, never an ownership counter.
	deps.DiskBindings.setRegistryDrop(deps.Registries.Delete)
	runnerAdapter.rootLeases = newBatchRootLeaseStore(deps.DiskBindings)
	// R12-H2 3c: the runtime is constructed BEFORE the executor env so the
	// env can carry a lazy ingress accessor — the App binds the ingress only
	// AFTER install (BindRuntimeIngress), so an eager value copy would stay
	// nil forever and the agent driver would fail closed for every
	// provider-bound agent incarnation.
	runtime, err := newBatchRuntime(batchRuntimeConfig{
		Host: deps.Host, RunnerID: deps.RunnerID, Profile: deps.Profile,
		Resolver: deps.Resolver, Leaves: deps.Registries.Resolve,
		RegistryCount: deps.Registries.Count,
		DiskBindings:  deps.DiskBindings,
		RootLeases:    runnerAdapter.rootLeases,
		Attach:        batchAttachAdapter{store: deps.Store}, Delivery: delivery, Runner: runnerAdapter,
	})
	if err != nil {
		return nil, err
	}
	env := batchLeafEnv{OutputCache: cache, Leaves: deps.Registries.Resolve, DiskBindings: deps.DiskBindings, Ingress: runtime.ingress}
	if deps.AgentDeps.Runs != nil {
		deps.AgentDeps.QuestionPublisher = batchCoordinatorQuestionPublisher{store: deps.Store, wake: deps.QuestionWake}
		// The store is the durable half of the question lifecycle: accepted
		// answers and cancelled asks close the notices they advertised.
		deps.AgentDeps.QuestionResolver = batchStoreQuestionResolver{store: deps.Store}
		deps.AgentDeps.Host = deps.Host
		deps.AgentDeps.RunnerID = deps.RunnerID
		deps.AgentDeps.Bind = batchStoreChildBind(deps.Store, deps.Host, deps.RunnerID)
	}
	executor := deps.Executor
	if executor == nil {
		built := newBatchExecutor(env, newBatchExecutorFamilies(deps.AgentDeps))
		built.decorateExecution = deps.DecorateExecution
		executor = built
	}
	// R11F P1-1: the commit hook reserves every Submit under the INVOKING
	// session (OwnerFor); the fixed Owner stays the dormant default.
	commitIdentity := session.BatchCommitIdentity{
		Host: deps.Host, Runner: deps.RunnerID, Owner: owner, OwnerFor: batchCommitOwnerFor,
	}
	if authorityService, ok := deps.Sessions.(session.AgentFinalizationService); ok {
		commitIdentity, err = authorityService.WithAgentFinalizationAuthorities(commitIdentity)
		if err != nil {
			return nil, fmt.Errorf("batch install: finalization authority: %w", err)
		}
	}
	commitHook := session.NewBatchCommitHook(deps.Store, commitIdentity)
	// Ordinary-completion wake bridge: after a SUCCESSFUL durable commit
	// that selected completion notices, notify the owning session so the
	// coordinator can run the reaction through its existing gates. The
	// wrapper only ENQUEUES the touched root (nonblocking, bounded on the
	// receiver side); it never runs a provider turn on the Runner commit
	// stack and never adds a second delivery authority. Nil (flag-OFF
	// installs, dormant tests) keeps the hook byte-identical.
	if deps.CompletionWake != nil {
		commitHook = pwCompletionWakeHook(commitHook, deps.CompletionWake)
	}
	runnerCfg := jobbatch.RunnerConfig{
		CommitHook: commitHook, CommitTimeout: deps.CommitTimeout,
		Limits: deps.Limits, Executor: executor,
		ControlTimeout: deps.ControlTimeout, RecordRetention: deps.RecordRetention,
	}
	if withRunner {
		runner, err := jobbatch.NewRunner(context.Background(), runnerCfg)
		if err != nil {
			return nil, err
		}
		runnerAdapter.runner = runner
	}
	install := &BatchInstall{
		CommitHook: commitHook, Runtime: runtime,
		RunnerHandle: runnerAdapter, Attach: batchAttachAdapter{store: deps.Store},
		Delivery: delivery, Done: done, Elector: elector,
		cliAccepted: make(map[string]int64),
		// R11F P1-5(d): the notice-pull drain adapter has NO production
		// consumer yet — it is consumed by the web drain surface in
		// R12/R15b; wired inertly until then. Do not invent a consumer.
		NoticePull:   batchStoreNoticePull{store: deps.Store, messages: deps.Messages, owner: owner},
		Sessions:     deps.Sessions,
		DiskBindings: deps.DiskBindings,
		RootLeases:   runnerAdapter.rootLeases,
		OutputCache:  cache,
		runnerCfg:    runnerCfg,
	}
	if withRunner {
		install.Runner = runnerAdapter.runner
	}
	install.CLIPump = session.NewBatchControlPump(deps.Store, deps.Host, install.commitCLIControl)
	return install, nil
}

// DrainDiskBindings closes residual construction scopes on the GRACEFUL
// shutdown path (after calls joined and disposal drained): construction
// records whose only remaining owners are their own Construction pins are
// released, so unused toolsets never outlive the calls that built them.
// R12-H2b §2.3: residual Continuation owners are dropped alongside —
// durable rows are picked up by the next process against ITS OWN store,
// so an in-process Continuation owner must not pin a binding forever.
// Forced shutdown never calls this — the manager, its remaining owners and
// the writer are retained until process exit; owners are never
// bulk-decremented.
func (i *BatchInstall) DrainDiskBindings() {
	if i == nil || i.DiskBindings == nil {
		return
	}
	i.DiskBindings.releaseResidualConstructions()
	i.DiskBindings.releaseResidualContinuations()
}

// BuildRunner constructs the real Runner for a deferred install (R11F P1-6)
// and attaches it to the runner handle (making the ingress live). Idempotent
// per install: the first construction wins; later calls return the same
// Runner.
func (i *BatchInstall) BuildRunner(ctx context.Context) (*jobbatch.Runner, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.Runner != nil {
		return i.Runner, nil
	}
	runner, err := jobbatch.NewRunner(ctx, i.runnerCfg)
	if err != nil {
		return nil, err
	}
	i.Runner = runner
	i.RunnerHandle.runner = runner
	return runner, nil
}

// batchStoreChildBind adapts the store's durable child-link write onto the
// agent family's bind seam; the install's host and runner identity close
// over the returned closure.
func batchStoreChildBind(store *session.BatchStore, host, runner string) func(context.Context, string, string, string, uint64, string) error {
	return func(ctx context.Context, owner, root, node string, token uint64, child string) error {
		return store.BatchBindChildSession(ctx, owner, host, runner, root, node, token, child)
	}
}
