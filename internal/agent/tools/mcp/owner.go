package mcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
)

// ErrOwnerBusy reports that another application currently owns the process
// wide MCP registry. The SDK deliberately permits only one application-mode
// owner; library-mode Apps do not acquire this owner.
var ErrOwnerBusy = errors.New("mcp: application owner is already active")

// ErrMCPConfigStoreBusy reports that an active owner is already bound to a
// different config store.
var ErrMCPConfigStoreBusy = errors.New("mcp: application owner is bound to a different config store")

// Owner is the lifetime token for the process-wide MCP registry. The MCP
// package predates multiple App instances and its tool/state maps remain
// process-wide, so ownership is explicit rather than silently shared.
type Owner struct {
	implicit bool
	// Standalone marks an owner reserved by an embedder that needs its own
	// MCP lifecycle independent of the process-wide installed owner (the SDK
	// multi-App case). A standalone owner participates in the shared
	// name-keyed registry like any owner, but it is never installed in the
	// package owner slot, never reclaims, never resets the shared registry on
	// Close, and only cleans up its own entries. Two owners configuring the
	// same server name share one registry entry, which is unsupported;
	// disjoint configs are the contract.
	standalone          bool
	closing             bool
	closeRequested      atomic.Bool
	generation          uint64
	lifecycleCtx        context.Context
	lifecycleCancel     context.CancelFunc
	initCount           int
	initWG              sync.WaitGroup
	initStarted         bool
	fullInitCount       int
	initDone            chan struct{}
	serverEpochs        map[string]uint64
	serverCancels       map[string]map[uint64]serverCancel
	committedAdmissions map[string]*serverAdmission
	pendingGlobalAdds   map[string]*addTransaction
	nextCancelToken     uint64
	refreshCh           chan struct{}
	refreshPending      map[refreshKey]refreshRequest
	refreshRunning      map[refreshKey]struct{}
	config              *config.ConfigStore
	refreshWG           sync.WaitGroup
	closeOnce           sync.Once
	closeDoneOnce       sync.Once
	closeDone           chan struct{}
	refreshDone         chan struct{}
	closeErr            error
	trackedSessions     map[*ClientSession]struct{}
	trackedEmpty        chan struct{}
	closer              *sessionCloser
	fallbackWorker      *fallbackWorker
}

type serverCancel struct {
	cancel context.CancelFunc
	token  uint64
}

// trackSessionLocked keeps the owner fence alive until a published generation
// has completed its one transport close, including after detachment.
func (o *Owner) trackSessionLocked(session *ClientSession, name string) {
	if o == nil || session == nil {
		return
	}
	session.closeStateMu.Lock()
	if session.closeFinished || session.closeStarted {
		session.closeStateMu.Unlock()
		return
	}
	if o.trackedSessions == nil {
		o.trackedSessions = make(map[*ClientSession]struct{})
	}
	if _, ok := o.trackedSessions[session]; ok {
		session.closeStateMu.Unlock()
		return
	}
	if len(o.trackedSessions) == 0 {
		o.trackedEmpty = make(chan struct{})
	}
	o.trackedSessions[session] = struct{}{}
	session.owner = o
	session.closeName = name
	session.closeStateMu.Unlock()
}

func (o *Owner) untrackSession(session *ClientSession) {
	lifecycleMu.Lock()
	if _, ok := o.trackedSessions[session]; ok {
		delete(o.trackedSessions, session)
		if len(o.trackedSessions) == 0 {
			close(o.trackedEmpty)
		}
	}
	lifecycleMu.Unlock()
}

func (o *Owner) waitTrackedSessions() {
	for {
		lifecycleMu.Lock()
		if len(o.trackedSessions) == 0 {
			lifecycleMu.Unlock()
			return
		}
		done := o.trackedEmpty
		lifecycleMu.Unlock()
		<-done
	}
}

type fallbackRequest struct {
	ctx    context.Context
	cfg    *config.ConfigStore
	result config.MCPMutationResult
	owner  *Owner
}

type fallbackWorker struct {
	mu       sync.Mutex
	pending  []fallbackRequest
	wake     chan struct{}
	stop     chan struct{}
	stopped  bool
	work     atomic.Int32
	stopOnce sync.Once
	wg       sync.WaitGroup
	done     chan struct{}
}

func newFallbackWorker() *fallbackWorker {
	return &fallbackWorker{
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

func (w *fallbackWorker) start() {
	w.wg.Add(1)
	go w.run()
}

func (w *fallbackWorker) enqueue(request fallbackRequest) {
	if request.owner == nil || request.ctx == nil || request.ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	w.pending = append(w.pending, request)
	w.work.Add(1)
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *fallbackWorker) take() (fallbackRequest, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return fallbackRequest{}, false
	}
	request := w.pending[0]
	w.pending = w.pending[1:]
	return request, true
}

func (w *fallbackWorker) run() {
	defer w.wg.Done()
	defer close(w.done)
	for {
		if request, ok := w.take(); ok {
			func() {
				defer w.work.Add(-1)
				if request.ctx.Err() == nil {
					startFallbackMutation(request.ctx, request.cfg, request.result, request.owner)
				}
			}()
			continue
		}
		select {
		case <-w.wake:
		case <-w.stop:
			return
		}
	}
}

func (w *fallbackWorker) stopAndWait() {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.stopped = true
		w.mu.Unlock()
		close(w.stop)
	})
	select {
	case w.wake <- struct{}{}:
	default:
	}
	w.wg.Wait()
}

func (o *Owner) enqueueSessionClose(session *ClientSession, shutdown bool) {
	if o == nil || o.closer == nil {
		return
	}
	lifecycleMu.Lock()
	if owner != o && !o.standalone {
		lifecycleMu.Unlock()
		return
	}
	session.closeStateMu.Lock()
	name := session.closeName
	session.closeStateMu.Unlock()
	o.closer.enqueue(closeRequest{session: session, name: name, shutdown: shutdown})
	lifecycleMu.Unlock()
}

func (o *Owner) enqueueFallback(cfg *config.ConfigStore, result config.MCPMutationResult) {
	if o == nil || o.fallbackWorker == nil {
		return
	}
	lifecycleMu.Lock()
	if owner != o && !o.standalone || o.closing {
		lifecycleMu.Unlock()
		return
	}
	o.fallbackWorker.enqueue(fallbackRequest{
		ctx:    o.lifecycleCtx,
		cfg:    cfg,
		result: result,
		owner:  o,
	})
	lifecycleMu.Unlock()
}

func adoptSessionForRetirement(name string, session *ClientSession) {
	if session == nil {
		return
	}
	lifecycleMu.Lock()
	session.closeStateMu.Lock()
	unowned := session.owner == nil
	session.closeStateMu.Unlock()
	if unowned && owner != nil && !owner.closing {
		owner.trackSessionLocked(session, name)
	}
	lifecycleMu.Unlock()
}

func closedChannel() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// Acquire reserves the process-wide MCP registry for an application.
func Acquire() (*Owner, error) {
	return acquire(false)
}

// AcquireStandalone reserves a standalone MCP owner for an embedder that
// needs its own MCP lifecycle independent of the process-wide installed
// owner (the SDK multi-App case). It never contends for the installed owner
// slot, so it never fails with ErrOwnerBusy because of another owner.
func AcquireStandalone() (*Owner, error) {
	return acquireStandalone()
}

// acquireStandalone mirrors acquire(false) without installing the owner in
// the package owner slot: a standalone owner never reclaims, never contends
// for the slot, and only cleans up its own registry entries on Close.
func acquireStandalone() (*Owner, error) {
	lifecycleMu.Lock()
	generation++
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	o := &Owner{
		standalone:          true,
		generation:          generation,
		lifecycleCtx:        lifecycleCtx,
		lifecycleCancel:     lifecycleCancel,
		closeDone:           make(chan struct{}),
		initDone:            closedChannel(),
		serverEpochs:        make(map[string]uint64),
		serverCancels:       make(map[string]map[uint64]serverCancel),
		committedAdmissions: make(map[string]*serverAdmission),
		pendingGlobalAdds:   make(map[string]*addTransaction),
		refreshCh:           make(chan struct{}, 1),
		refreshPending:      make(map[refreshKey]refreshRequest),
		refreshRunning:      make(map[refreshKey]struct{}),
		trackedSessions:     make(map[*ClientSession]struct{}),
		trackedEmpty:        closedChannel(),
		closer:              newSessionCloser(),
		fallbackWorker:      newFallbackWorker(),
		refreshDone:         make(chan struct{}),
	}
	o.closer.start()
	o.fallbackWorker.start()
	o.refreshWG.Add(1)
	go o.refreshLoop()
	standaloneOwners[o] = struct{}{}
	lifecycleMu.Unlock()
	return o, nil
}

// acquireImplicit supports the legacy package-level entry points. Unlike an
// App-owned token, an idle implicit owner may be reclaimed after its registry
// has been emptied by its caller.
func acquireImplicit() (*Owner, error) {
	return acquire(true)
}

// canReclaimLocked verifies that an implicit owner has no lifecycle work that
// could outlive the registry reset. lifecycleMu must be held by the caller.
func (o *Owner) canReclaimLocked() bool {
	if o == nil || !o.implicit || o.closing || o.closeRequested.Load() || o.initCount != 0 || o.fullInitCount != 0 {
		return false
	}
	if len(o.trackedSessions) != 0 || sessions.Len() != 0 || states.Len() != 0 ||
		allTools.Len() != 0 || allPrompts.Len() != 0 || allResources.Len() != 0 {
		return false
	}
	if len(o.serverCancels) != 0 || len(o.committedAdmissions) != 0 ||
		len(o.pendingGlobalAdds) != 0 || len(o.refreshPending) != 0 || len(o.refreshRunning) != 0 {
		return false
	}
	if o.closer != nil && o.closer.work.Load() != 0 {
		return false
	}
	return o.fallbackWorker == nil || o.fallbackWorker.work.Load() == 0
}

func acquire(implicit bool) (*Owner, error) {
	lifecycleMu.Lock()
	if owner != nil {
		oldOwner := owner
		if !oldOwner.canReclaimLocked() {
			lifecycleMu.Unlock()
			return nil, ErrOwnerBusy
		}
		lifecycleMu.Unlock()

		oldOwner.requestClose(true)
		<-oldOwner.closeDone

		lifecycleMu.Lock()
		if owner != oldOwner {
			lifecycleMu.Unlock()
			return nil, ErrOwnerBusy
		}
	}

	generation++
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	o := &Owner{
		implicit:            implicit,
		generation:          generation,
		lifecycleCtx:        lifecycleCtx,
		lifecycleCancel:     lifecycleCancel,
		closeDone:           make(chan struct{}),
		initDone:            closedChannel(),
		serverEpochs:        make(map[string]uint64),
		serverCancels:       make(map[string]map[uint64]serverCancel),
		committedAdmissions: make(map[string]*serverAdmission),
		pendingGlobalAdds:   make(map[string]*addTransaction),
		refreshCh:           make(chan struct{}, 1),
		refreshPending:      make(map[refreshKey]refreshRequest),
		refreshRunning:      make(map[refreshKey]struct{}),
		trackedSessions:     make(map[*ClientSession]struct{}),
		trackedEmpty:        closedChannel(),
		closer:              newSessionCloser(),
		fallbackWorker:      newFallbackWorker(),
		refreshDone:         make(chan struct{}),
	}
	o.closer.start()
	o.fallbackWorker.start()
	owner = o
	initDone = o.initDone
	o.refreshWG.Add(1)
	go o.refreshLoop()
	lifecycleMu.Unlock()
	return o, nil
}

func currentOwner() *Owner {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	return owner
}

// A standalone owner is self-current: it is never installed in the
// package owner slot, so only its own closing state gates admission.
func (o *Owner) isCurrentLocked() bool {
	return (o.standalone || owner == o) && !o.closing && !o.closeRequested.Load()
}

func (o *Owner) rememberConfig(cfg *config.ConfigStore) error {
	if o == nil || cfg == nil {
		return nil
	}
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if o.config != nil && o.config != cfg {
		return ErrMCPConfigStoreBusy
	}
	if o.isCurrentLocked() {
		o.config = cfg
	}
	return nil
}

func (o *Owner) acceptsSession() bool {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	return o.isCurrentLocked()
}

func (o *Owner) acceptsGeneration(generation uint64) bool {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	return (o.standalone || (owner == o && o.generation == generation)) && !o.closing
}

func (o *Owner) operationContext(ctx context.Context) (context.Context, func()) {
	operationCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(o.lifecycleCtx, cancel)
	return operationCtx, func() {
		stop()
		cancel()
	}
}

// requestClose starts the one shutdown coordinator. The coordinator owns the
// lifecycle mutex acquisition, so a caller can observe context cancellation
// while another lifecycle turn is still active.
func (o *Owner) requestClose(reclaim bool) {
	if o == nil {
		return
	}
	o.closeRequested.Store(true)
	runBeforeCloseRequest(o)
	o.closeOnce.Do(func() { go o.coordinateClose(reclaim) })
}

// coordinateClose runs the single shutdown coordinator. A standalone owner
// is never installed in the package owner slot, so it always proceeds with
// its own shutdown instead of short-circuiting on the slot identity.
func (o *Owner) coordinateClose(reclaim bool) {
	lifecycleMu.Lock()
	if owner != o && !o.standalone {
		lifecycleMu.Unlock()
		o.closeDoneOnce.Do(func() { close(o.closeDone) })
		return
	}
	o.closing = true
	o.lifecycleCancel()
	lifecycleMu.Unlock()
	o.finishClose(reclaim)
}

// Close starts shutdown and waits until it finishes or ctx expires. If ctx
// expires, the process-wide owner remains fenced in closing state and rejects
// Acquire until its single cleanup goroutine has joined every admitted
// initialization or renewal and closed every session. A non-cooperative
// session close can therefore retain the fence indefinitely; releasing it
// would allow callbacks from that old session to mutate the next owner's
// process-wide registry.
func (o *Owner) Close(ctx context.Context) error {
	if o == nil {
		return Close(ctx)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	o.requestClose(false)

	select {
	case <-o.closeDone:
		return o.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *Owner) finishClose(reclaim bool) {
	// A standalone owner shuts down only its own lifecycle; shared registry
	// cleanup and slot teardown belong to the installed owner.
	if o.fallbackWorker != nil {
		o.fallbackWorker.stopAndWait()
	}
	// Do not abandon this wait when a caller's cleanup context expires. The
	// owner remains the lifecycle fence until every admitted operation exits.
	o.initWG.Wait()
	// The notification worker is independently joined so no callback from
	// this owner can publish into a later owner after the registry is reset.
	o.refreshWG.Wait()

	type namedSession struct {
		name    string
		session *ClientSession
	}
	snapshotBySession := make(map[*ClientSession]string)
	lifecycleMu.Lock()
	for name, session := range sessions.Seq2() {
		session.closeStateMu.Lock()
		sessionOwner := session.owner
		session.closeStateMu.Unlock()
		if sessionOwner == o || (!o.standalone && sessionOwner == nil) {
			if sessionOwner == nil {
				o.trackSessionLocked(session, name)
			}
			snapshotBySession[session] = name
		}
	}
	for session := range o.trackedSessions {
		if _, ok := snapshotBySession[session]; !ok {
			snapshotBySession[session] = ""
		}
	}
	lifecycleMu.Unlock()
	snapshot := make([]namedSession, 0, len(snapshotBySession))
	for session, name := range snapshotBySession {
		snapshot = append(snapshot, namedSession{name: name, session: session})
	}

	// Cancel every transport before entering any potentially non-cooperative
	// SDK Close. The owner closer then runs those closes sequentially.
	for _, item := range snapshot {
		item.session.cancelContext()
	}
	for _, item := range snapshot {
		item.session.queueCloseWithMode(true)
	}
	o.waitTrackedSessions()
	if o.closer != nil {
		o.closer.stopAndWait()
	}

	lifecycleMu.Lock()
	if owner == o {
		o.clearRegistryEntriesLocked()
		if !reclaim {
			owner = nil
			initDone = closedChannel()
		}
		o.closeInitBarrierLocked()
	} else if o.standalone {
		o.clearRegistryEntriesLocked()
	}
	if o.standalone {
		delete(standaloneOwners, o)
	}
	if (owner == nil || (owner == o && reclaim)) && len(standaloneOwners) == 0 {
		resetBrokerLocked()
	}
	o.closeDoneOnce.Do(func() { close(o.closeDone) })
	lifecycleMu.Unlock()
}

// clearRegistryEntriesLocked removes only this owner's entries from the
// shared name-keyed maps. Callers hold lifecycleMu.
func (o *Owner) clearRegistryEntriesLocked() {
	names := make(map[string]struct{})
	for name := range o.committedAdmissions {
		names[name] = struct{}{}
	}
	for name := range o.serverEpochs {
		names[name] = struct{}{}
	}
	if !o.standalone {
		for name := range states.Seq2() {
			names[name] = struct{}{}
		}
		for name := range allTools.Seq2() {
			names[name] = struct{}{}
		}
		for name := range allPrompts.Seq2() {
			names[name] = struct{}{}
		}
		for name := range allResources.Seq2() {
			names[name] = struct{}{}
		}
	}
	for name := range names {
		if session, ok := sessions.Get(name); ok {
			session.closeStateMu.Lock()
			sessionOwner := session.owner
			session.closeStateMu.Unlock()
			if sessionOwner != o {
				continue
			}
		}
		if admission, ok := stateOwners.Get(name); ok && admission.owner != o {
			continue
		}
		if hasForeignServerClaimLocked(o, name) {
			continue
		}
		sessions.Del(name)
		states.Del(name)
		stateOwners.Del(name)
		allTools.Del(name)
		allPrompts.Del(name)
		allResources.Del(name)
		leases.deleteIfUnused(name)
	}
	clear(o.committedAdmissions)
	clear(o.serverCancels)
	clear(o.pendingGlobalAdds)
	clear(o.serverEpochs)
	clear(o.refreshPending)
	clear(o.refreshRunning)
}

func hasForeignServerClaimLocked(o *Owner, name string) bool {
	if current := owner; current != nil && current != o {
		if _, ok := current.serverEpochs[name]; ok {
			return true
		}
	}
	for other := range standaloneOwners {
		if other != o {
			if _, ok := other.serverEpochs[name]; ok {
				return true
			}
		}
	}
	return false
}

func resetBrokerLocked() {
	broker.Shutdown()
	broker = pubsub.NewBroker[Event]()
}

// State represents the current state of an MCP client
type State int

const (
	StateDisabled State = iota
	StateStarting
	StateConnected
	StateError
)

func (s State) String() string {
	switch s {
	case StateDisabled:
		return "disabled"
	case StateStarting:
		return "starting"
	case StateConnected:
		return "connected"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

// EventType represents the type of MCP event
type EventType uint

const (
	EventStateChanged EventType = iota
	EventToolsListChanged
	EventPromptsListChanged
	EventResourcesListChanged
)

// Event represents an event in the MCP system
type Event struct {
	Type   EventType
	Name   string
	State  State
	Error  error
	Counts Counts
}

// Counts number of available tools, prompts, etc.
type Counts struct {
	Tools     int
	Prompts   int
	Resources int
}

// ClientInfo holds information about an MCP client's state
type ClientInfo struct {
	Name        string
	State       State
	Error       error
	Client      *ClientSession
	Counts      Counts
	ConnectedAt time.Time
}

// SubscribeEvents returns a channel for MCP events
func SubscribeEvents(ctx context.Context) <-chan pubsub.Event[Event] {
	return currentBroker().Subscribe(ctx)
}

// SubscribeEvents returns a channel for MCP events.
// A nil receiver resolves the process-current owner exactly like the
// package-level SubscribeEvents function, so callers holding an optional
// owner can call the method unconditionally.
func (o *Owner) SubscribeEvents(ctx context.Context) <-chan pubsub.Event[Event] {
	if o == nil {
		return SubscribeEvents(ctx)
	}
	// The event broker is shared process-wide name-keyed state; events are
	// not filtered per owner here.
	return SubscribeEvents(ctx)
}

func currentBroker() *pubsub.Broker[Event] {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	return broker
}

func publishEvent(t pubsub.EventType, event Event) {
	currentBroker().Publish(t, event)
}

// GetStates returns the current state of all MCP clients
func GetStates() map[string]ClientInfo {
	return states.Copy()
}

// GetStates returns the current state of all MCP clients.
// A nil receiver resolves the process-current owner exactly like the
// package-level GetStates function, so callers holding an optional owner can
// call the method unconditionally.
func (o *Owner) GetStates() map[string]ClientInfo {
	if o == nil {
		return GetStates()
	}
	// The registry data is shared name-keyed state; owner-scoping of reads
	// happens through IsConfigured filtering at consumers.
	return GetStates()
}

// IsConfigured reports whether name belongs to the consuming config store.
// MCP runtime state is process-wide, so callers that serve more than one
// ConfigStore must apply this ownership check before exposing registry data.
// A nil store preserves the legacy package-level behavior used by internal
// test fixtures that do not have a consuming configuration.
func IsConfigured(cfg *config.ConfigStore, name string) bool {
	if cfg == nil {
		return true
	}
	_, ok := cfg.MCPConfig(name)
	return ok
}

// IsConfigured reports whether name belongs to the consuming config store.
// A nil receiver resolves the process-current owner exactly like the
// package-level IsConfigured function, so callers holding an optional owner
// can call the method unconditionally.
func (o *Owner) IsConfigured(cfg *config.ConfigStore, name string) bool {
	if o == nil {
		return IsConfigured(cfg, name)
	}
	// The registry data is shared name-keyed state; owner-scoping of reads
	// happens through IsConfigured filtering at consumers.
	return IsConfigured(cfg, name)
}

// GetState returns the state of a specific MCP client
func GetState(name string) (ClientInfo, bool) {
	return states.Get(name)
}

// GetState returns the state of a specific MCP client.
// A nil receiver resolves the process-current owner exactly like the
// package-level GetState function, so callers holding an optional owner can
// call the method unconditionally.
func (o *Owner) GetState(name string) (ClientInfo, bool) {
	if o == nil {
		return GetState(name)
	}
	// The registry data is shared name-keyed state; owner-scoping of reads
	// happens through IsConfigured filtering at consumers.
	return GetState(name)
}

// Close closes all MCP clients. This should be called during application shutdown.
func Close(ctx context.Context) error {
	o := currentOwner()
	if o == nil {
		return nil
	}
	return o.Close(ctx)
}

// Initialize initializes MCP clients based on the provided configuration.
//
// restrictToCLIEnabled, when true, additionally skips every server whose
// config does not set EnabledInCLI — set by internal/app.New's
func ensureOwner() (*Owner, error) {
	if o := currentOwner(); o != nil {
		return o, nil
	}
	return acquireImplicit()
}

// detachSessionLocked removes the published session while the caller holds
// the server write lease. It takes lifecycleMu before touching owner state.
func detachSessionLocked(name string) *ClientSession {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	return detachSessionLifecycleLocked(name)
}

// detachSessionLifecycleLocked is the lifecycle-locked detach primitive. The
// caller holds the server lease and lifecycleMu, in that order.
func detachSessionLifecycleLocked(name string) *ClientSession {
	session, ok := sessions.Get(name)
	admissionOwner := owner
	if ok && session.owner != nil {
		admissionOwner = session.owner
	}
	if admissionOwner != nil {
		delete(admissionOwner.committedAdmissions, name)
	}
	stateOwners.Del(name)
	if ok {
		sessions.Del(name)
		return session
	}
	return nil
}

func closeMCPClient(name string, session *ClientSession) {
	if session == nil {
		return
	}
	if err := session.Close(); err != nil {
		logMCPCloseError(name, err)
	}
}

func logMCPCloseError(name string, err error) {
	if err != nil && !errors.Is(err, io.EOF) &&
		!errors.Is(err, context.Canceled) && err.Error() != "signal: killed" {
		slog.Warn("Error closing MCP session", "name", name, "error", err)
	}
}

func logMCPShutdownError(name string, err error) {
	if err != nil && !errors.Is(err, io.EOF) &&
		!errors.Is(err, context.Canceled) && err.Error() != "signal: killed" {
		slog.Warn("Failed to shutdown MCP client", "name", name, "error", err)
	}
}

func retireMCPClient(name string, session *ClientSession) {
	if session != nil {
		adoptSessionForRetirement(name, session)
		session.retire()
	}
}
