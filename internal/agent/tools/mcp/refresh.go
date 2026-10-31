package mcp

import (
	"context"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
)

type refreshKind uint8

const (
	refreshToolsKind refreshKind = iota
	refreshPromptsKind
	refreshResourcesKind
)

type refreshKey struct {
	name           string
	kind           refreshKind
	epoch          uint64
	candidateToken uint64
}

type refreshRequest struct {
	kind      refreshKind
	name      string
	cfg       *config.ConfigStore
	admission serverAdmission
	// deferUntilCommit retains a notification received before its candidate
	// session is published.
	deferUntilCommit bool
	// candidateToken keeps a deferred notification attached to the exact
	// candidate admission. It must not be inferred from the server name.
	candidateToken uint64
	// rawEvents counts notifications deferred with this refresh. They become
	// observable only after the candidate commits successfully.
	rawEvents uint
}

func (o *Owner) refreshLoop() {
	defer o.refreshWG.Done()
	defer close(o.refreshDone)
	for {
		select {
		case <-o.lifecycleCtx.Done():
			return
		case <-o.refreshCh:
			for {
				request, ok := o.nextRefresh()
				if !ok {
					break
				}
				o.runRefresh(request)
				select {
				case <-o.lifecycleCtx.Done():
					return
				default:
				}
			}
		}
	}
}

func (o *Owner) nextRefresh() (refreshRequest, bool) {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	for key, request := range o.refreshPending {
		if request.deferUntilCommit {
			if !request.admission.notificationsValidLocked() {
				delete(o.refreshPending, key)
				continue
			}
			// A deferred request is activated only by the exact candidate
			// admission that owns it. Seeing a session under the same name is
			// insufficient: during same-name replacement it is the old session.
			continue
		}
		if !request.admission.validLocked() {
			delete(o.refreshPending, key)
			continue
		}
		delete(o.refreshPending, key)
		o.refreshRunning[key] = struct{}{}
		return request, true
	}
	return refreshRequest{}, false
}

func (o *Owner) enqueueRefresh(request refreshRequest) {
	lifecycleMu.Lock()
	wake := o.enqueueRefreshLocked(request)
	lifecycleMu.Unlock()
	if wake {
		o.signalRefresh()
	}
}

// enqueueRefreshLocked admits one generation of refresh work. lifecycleMu
// must be held by the caller.
func (o *Owner) enqueueRefreshLocked(request refreshRequest) bool {
	if owner != o && !o.standalone || request.admission.owner != o ||
		!request.admission.notificationsValidLocked() {
		return false
	}
	key := refreshKeyForRequest(request)
	if existing, exists := o.refreshPending[key]; exists {
		if request.deferUntilCommit && existing.deferUntilCommit &&
			existing.candidateToken == request.candidateToken {
			existing.rawEvents += request.rawEvents
			o.refreshPending[key] = existing
		}
		return false
	}
	// If the same key is already running, retaining one pending request marks
	// it dirty. The worker will run it once more after the in-flight snapshot
	// returns, coalescing any further notifications into that rerun.
	o.refreshPending[key] = request
	return true
}

func (o *Owner) signalRefresh() {
	select {
	case o.refreshCh <- struct{}{}:
	default:
		// The pending map is the authoritative queue. The channel is only a
		// wake-up edge, so a saturated channel does not drop a refresh.
	}
}

// activateRefreshesLocked atomically transitions deferred refreshes for one
// exact committed candidate and returns raw notifications that may now be
// published. lifecycleMu must be held by the caller.
func (o *Owner) activateRefreshesLocked(admission *serverAdmission) ([]Event, bool) {
	if admission == nil || admission.serverCancelToken == 0 {
		return nil, false
	}
	wake := false
	var events []Event
	for key, request := range o.refreshPending {
		if !request.deferUntilCommit || request.candidateToken != admission.serverCancelToken {
			continue
		}
		delete(o.refreshPending, key)
		request.deferUntilCommit = false
		request.candidateToken = 0
		request.admission.committed = admission.committed
		request.admission.committedName = admission.committedName
		request.admission.committedEpoch = admission.committedEpoch
		request.admission.mcpRevision = admission.mcpRevision
		request.admission.resolverRevision = admission.resolverRevision
		request.admission.mcpAdmission = admission.mcpAdmission
		request.admission.configIdentity = admission.configIdentity
		request.admission.hasConfigIdentity = admission.hasConfigIdentity
		request.admission.publishedSession = admission.publishedSession
		if request.admission.committedName == "" {
			request.admission.committedName = admission.name
			request.admission.committedEpoch = admission.epoch
		}
		request.admission.committed = true
		requestKey := refreshKeyForRequest(request)
		if _, exists := o.refreshPending[requestKey]; !exists {
			o.refreshPending[requestKey] = request
			wake = true
		}
		for range request.rawEvents {
			events = append(events, Event{
				Type: listChangedEventType(request.kind),
				Name: request.name,
			})
		}
	}
	return events, wake
}

func publishListChangedEvents(events []Event) {
	for _, event := range events {
		broker.Publish(pubsub.UpdatedEvent, event)
	}
}

func publishListChangedEventsOn(brokerForEvent *pubsub.Broker[Event], events []Event) {
	for _, event := range events {
		brokerForEvent.Publish(pubsub.UpdatedEvent, event)
	}
}

func (o *Owner) discardDeferredRefreshesLocked(candidateToken uint64) {
	for key, request := range o.refreshPending {
		if request.deferUntilCommit && request.candidateToken == candidateToken {
			delete(o.refreshPending, key)
		}
	}
}

func refreshKeyForRequest(request refreshRequest) refreshKey {
	epoch := request.admission.epoch
	if request.admission.committedName != "" {
		epoch = request.admission.committedEpoch
	}
	return refreshKey{
		name:           request.name,
		kind:           request.kind,
		epoch:          epoch,
		candidateToken: request.candidateToken,
	}
}

const refreshRequestTimeout = 15 * time.Second

func (o *Owner) runRefresh(request refreshRequest) {
	epoch := request.admission.epoch
	if request.admission.committedName != "" {
		epoch = request.admission.committedEpoch
	}
	key := refreshKeyForRequest(request)
	defer func() {
		lifecycleMu.Lock()
		delete(o.refreshRunning, key)
		lifecycleMu.Unlock()
	}()
	if !request.admission.valid() {
		return
	}
	// Refresh owns a temporary lifecycle reference while it holds the server
	// read lease and performs the SDK list call.
	refreshTimeout := refreshRequestTimeout
	if mcpConfig, ok := request.cfg.MCPConfig(request.name); ok && mcpConfig.Timeout > 0 {
		refreshTimeout = mcpTimeout(mcpConfig)
	}
	refreshCtx, cancelRefresh := context.WithTimeout(o.lifecycleCtx, refreshTimeout)
	defer cancelRefresh()
	refreshAdmission, err := o.admitServer(refreshCtx, request.cfg, request.name, false)
	if err != nil || refreshAdmission.epoch != epoch {
		if err == nil {
			refreshAdmission.done()
		}
		return
	}
	defer refreshAdmission.done()
	refreshAdmission.committed = true
	refreshAdmission.committedName = request.admission.committedName
	refreshAdmission.committedEpoch = epoch
	refreshAdmission.publishedSession = request.admission.publishedSession
	refreshAdmission.configIdentity = request.admission.configIdentity
	refreshAdmission.hasConfigIdentity = request.admission.hasConfigIdentity
	refreshAdmission.epoch = epoch
	refreshAdmission.ctx = refreshCtx
	switch request.kind {
	case refreshToolsKind:
		refreshTools(refreshCtx, request.cfg, request.name, &refreshAdmission)
	case refreshPromptsKind:
		refreshPrompts(request.name, &refreshAdmission)
	case refreshResourcesKind:
		refreshResources(request.name, &refreshAdmission)
	}
}

func notifyListChanged(admission *serverAdmission, name string, kind refreshKind) {
	eventType := listChangedEventType(kind)
	if admission == nil || admission.owner == nil {
		lifecycleMu.Lock()
		broker.Publish(pubsub.UpdatedEvent, Event{Type: eventType, Name: name})
		lifecycleMu.Unlock()
		return
	}

	// Admission, queueing, and the raw notification form one lifecycle
	// transition. A delete or disable that wins the lifecycle lock first will
	// invalidate the generation and suppress both the refresh and its event.
	lifecycleMu.Lock()
	if !admission.notificationsValidLocked() {
		committed := admission.committed
		lifecycleMu.Unlock()
		if committed {
			fenceInvalidCommittedNotification(admission)
		}
		return
	}
	request := refreshRequest{
		kind:      kind,
		name:      name,
		cfg:       admission.cfg,
		admission: *admission,
	}
	if !admission.committed && admission.candidate && admission.serverCancelToken != 0 {
		// Any uncommitted admission is a candidate, including InitializeSingle
		// when it is replacing an already published same-name session. Its
		// notification must remain attached to this exact token until commit.
		request.deferUntilCommit = true
		request.candidateToken = admission.serverCancelToken
		request.rawEvents = 1
	}
	wake := admission.owner.enqueueRefreshLocked(request)
	if request.deferUntilCommit {
		lifecycleMu.Unlock()
		if wake {
			admission.owner.signalRefresh()
		}
		return
	}
	if !wake {
		// A duplicate pending/running request is still a valid notification;
		// only generation admission decides whether its raw event is published.
		if owner != admission.owner || admission.owner.closing ||
			!admission.notificationsValidLocked() {
			lifecycleMu.Unlock()
			return
		}
	}
	broker.Publish(pubsub.UpdatedEvent, Event{Type: eventType, Name: name})
	lifecycleMu.Unlock()
	if wake {
		admission.owner.signalRefresh()
	}
}

func fenceInvalidCommittedNotification(admission *serverAdmission) {
	if admission == nil || admission.owner == nil {
		return
	}
	name := admission.committedName
	if name == "" {
		name = admission.name
	}
	lease := serverLeaseFor(name)
	lease.Lock()
	lifecycleMu.Lock()
	current, exists := sessions.Get(name)
	registered := admission.owner.committedAdmissions[name] == admission ||
		(admission.owner.committedAdmissions[name] == nil && current == admission.publishedSession)
	if registered && exists && current == admission.publishedSession && !admission.committedValidLocked() {
		cancels := admission.owner.invalidateServerLocked(name)
		detached := detachSessionLifecycleLocked(name)
		clearAdvertised(name)
		setState(name, StateDisabled, nil, nil, Counts{})
		lifecycleMu.Unlock()
		lease.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		retireMCPClient(name, detached)
		publishStateEvent(name, StateDisabled, nil, Counts{})
		return
	}
	lifecycleMu.Unlock()
	lease.Unlock()
}

// detachInvalidCommittedSessionLocked removes only the exact published
// session represented by admission. The caller holds the server lease and
// lifecycleMu, in that order.
func (o *Owner) detachInvalidCommittedSessionLocked(name string, session *ClientSession, cfg *config.ConfigStore) ([]context.CancelFunc, *ClientSession, bool) {
	if o == nil || session == nil || cfg == nil {
		return nil, nil, false
	}
	admission := o.committedAdmissions[name]
	if admission == nil || admission.cfg != cfg || admission.publishedSession != session || admission.committedValidLocked() {
		return nil, nil, false
	}
	cancels := o.invalidateServerLocked(name)
	detached := detachSessionLifecycleLocked(name)
	clearAdvertised(name)
	setState(name, StateDisabled, nil, nil, Counts{})
	return cancels, detached, true
}

func listChangedEventType(kind refreshKind) EventType {
	switch kind {
	case refreshPromptsKind:
		return EventPromptsListChanged
	case refreshResourcesKind:
		return EventResourcesListChanged
	default:
		return EventToolsListChanged
	}
}

// maybeStdioErr if a stdio mcp prints an error in non-json format, it'll fail
// to parse, and the cli will then close it, causing the EOF error.
