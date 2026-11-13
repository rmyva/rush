package server

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
)

const batchInterval = 16 * time.Millisecond

// configNotificationsProcessed counts fully processed notifications; enqueued
// counts snapshots accepted by the hub's coalescing config queue. Test hooks.
var (
	configNotificationsProcessed atomic.Int64
	configNotificationsEnqueued  atomic.Int64
)

func forwardConfigSnapshot(h *Hub, wire ConfigWire, ok bool) {
	if ok && h.BroadcastConfig(wire) {
		configNotificationsEnqueued.Add(1)
	}
	configNotificationsProcessed.Add(1)
}

// Fork merge note: upstream rewrote events.go as REST /v1/... protocol
// conversions (sessionToProto, messageToProto, etc.). Our events.go is a
// WebSocket subscriber that forwards pubsub events to our Hub. Different
// architectures — keep ours. CHANGELOG.fork.md section 4.A.

type eventForwarders struct {
	join       func()
	syncConfig func()
}

func (f eventForwarders) Wait()       { f.join() }
func (f eventForwarders) SyncConfig() { f.syncConfig() }

// subscribeAndBroadcast subscribes to app event sources and forwards them to the hub.
func subscribeAndBroadcast(ctx context.Context, a *appPkg.App, h *Hub) eventForwarders {
	var forwarders sync.WaitGroup
	forwarders.Add(4)
	// Live-work snapshots: the forwarders below only MARK sessions dirty;
	// this worker does the DB reads off the broadcast hot path (#1058) and
	// exits with ctx.
	liveWork := startLiveWorkPusher(ctx, a, h)

	// Fork patch (finalization): nil gate (flag off) leaves both forwarders
	// unchanged; a held terminal event waits for the seal. WS and web
	// clients inherit the holds through the hub.
	gate := finalizationTerminalGateFor(a)

	// Sessions
	go func() {
		defer forwarders.Done()
		ch := FinalizationGatedSessionStream(ctx, gate, a.Sessions.Subscribe(ctx))
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				switch ev.Type {
				case pubsub.CreatedEvent:
					sess := ev.Payload
					AnnotateSessionExternalOwnership(a, &sess)
					h.Broadcast(EventSessionCreated, sess)
				case pubsub.UpdatedEvent:
					sess := ev.Payload
					AnnotateSessionExternalOwnership(a, &sess)
					h.Broadcast(EventSessionUpdated, sess)
				case pubsub.DeletedEvent:
					h.Broadcast(EventSessionDeleted, ev.Payload)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Messages ΓÇö batch streaming updates to avoid flooding the client.
	go func() {
		defer forwarders.Done()
		ch := FinalizationGatedMessageStream(ctx, gate, a.Messages.Subscribe(ctx))
		ticker := time.NewTicker(batchInterval)
		defer ticker.Stop()

		// pending holds the latest update per message ID within the current batch.
		pending := make(map[string]message.Message)

		flush := func() {
			for _, msg := range pending {
				h.Broadcast(EventMessageUpdated, toMessageWire(msg))
			}
			clear(pending)
		}

		for {
			select {
			case <-ctx.Done():
				flush()
				return
			case ev, ok := <-ch:
				if !ok {
					flush()
					return
				}
				switch ev.Type {
				case pubsub.CreatedEvent:
					h.Broadcast(EventMessageCreated, toMessageWire(ev.Payload))
					if shouldPushLiveWork(ev.Payload) {
						liveWork.mark(ev.Payload.SessionID)
					}
				case pubsub.UpdatedEvent:
					// Deduplicate: keep only the latest update per message ID.
					pending[ev.Payload.ID] = ev.Payload
				case pubsub.DeletedEvent:
					delete(pending, ev.Payload.ID)
					h.Broadcast(EventMessageDeleted, ev.Payload)
					if shouldPushLiveWork(ev.Payload) {
						liveWork.mark(ev.Payload.SessionID)
					}
				}
			case <-ticker.C:
				flush()
			}
		}
	}()

	// MCP state changes — broadcast a full snapshot of all servers on each
	// event. Read/broker surfaces are process-wide by design (shared
	// name-keyed registry + shared event bus); isolation via IsConfigured
	// filtering in buildMCPSnapshot below.
	go func() {
		defer forwarders.Done()
		ch := mcp.SubscribeEvents(ctx)
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return
				}
				h.Broadcast(EventMCPState, buildMCPSnapshot(a.Config()))
			case <-ctx.Done():
				return
			}
		}
	}()

	// Poll-driven config changes (plan §2.4): re-broadcast the full config
	// snapshot so web sessions that inherit their models display the new
	// defaults. The channel is coalescing (buffer 1), so bursts of reloads
	// cannot flood clients. The subscription is registered SYNCHRONOUSLY
	// before the forwarder goroutine starts: once subscribeAndBroadcast
	// returns, a poll-driven publish is guaranteed to land in ch (buffer 1)
	// even if it fires before the goroutine's first receive.
	type configBarrier struct{ done chan struct{} }
	barriers := make(chan configBarrier)
	configForwarderDone := make(chan struct{})
	ch := a.SubscribeModelsChanged(ctx)
	go func() {
		defer close(configForwarderDone)
		defer forwarders.Done()
		process := func() {
			wire, ok := buildConfigWire(a)
			forwardConfigSnapshot(h, wire, ok)
		}
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return
				}
				process()
			case barrier := <-barriers:
				for {
					select {
					case _, ok := <-ch:
						if !ok {
							close(barrier.done)
							return
						}
						process()
					default:
						close(barrier.done)
						goto drained
					}
				}
			drained:
			}
		}
	}()

	slog.Debug("ws: event subscriptions started")

	// Unused imports guard — referenced through the generic channels above.
	_ = session.Session{}
	return eventForwarders{
		join: func() {
			forwarders.Wait()
			<-liveWork.stopped
		},
		syncConfig: func() {
			done := make(chan struct{})
			select {
			case barriers <- configBarrier{done: done}:
			case <-configForwarderDone:
				return
			}
			select {
			case <-done:
			case <-configForwarderDone:
			}
		},
	}
}

// buildMCPSnapshot returns the current state of all MCP servers in wire format.
func buildMCPSnapshot(cfg *config.Config) MCPSnapshot {
	all := mcp.GetStates()
	servers := make([]MCPServerInfo, 0, len(all))
	for name, info := range all {
		srv := MCPServerInfo{
			Name:      name,
			Status:    info.State.String(),
			Disabled:  info.State == mcp.StateDisabled,
			ToolCount: info.Counts.Tools,
			Tools:     mcp.GetServerToolNames(name),
		}
		if cfg != nil {
			if mcpCfg, ok := cfg.MCP[name]; ok {
				srv.ServerType = string(mcpCfg.Type)
				srv.Command = mcpCfg.Command
				srv.Args = mcpCfg.Args
				srv.URL = mcpCfg.URL
				srv.Env = mcpCfg.Env
				srv.Headers = mcpCfg.Headers
				srv.Source = string(mcpCfg.Source)
			}
		}
		servers = append(servers, srv)
	}
	return MCPSnapshot{Servers: servers}
}
