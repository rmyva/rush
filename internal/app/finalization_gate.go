package app

import (
	"sync"

	"github.com/PHPCraftdream/rush/internal/session"
)

// appFinalizationGateEntry caches one App's terminal gate behind a sync.Once
// so repeated subscription wiring shares one gate without re-reading the
// config or rebuilding the store. Entries are keyed per App because tests
// assemble many App values in one process.
type appFinalizationGateEntry struct {
	once sync.Once
	gate session.FinalizationTerminalGate
}

// appFinalizationGates holds the per-App gate cache entries.
var appFinalizationGates sync.Map // *App -> *appFinalizationGateEntry

// FinalizationTerminalGate returns the durable terminal gate used to delay
// terminal deliveries (CLI final output, WS session events, SSE run events)
// until the owner's finalization record is sealed. With
// options.finalization.enabled off — or without a usable config or database
// writer — it returns nil: every consumer treats nil as "no gate" and keeps
// the legacy byte-for-byte behavior. With the flag on it polls the App's
// agent finalization store; the poll interval 0 selects the gate's built-in
// 500ms default, because the supervision interval is a minutes-scale silence
// window, far too coarse for a terminal-delivery poll. The result is cached
// per App; no goroutines are created.
func (a *App) FinalizationTerminalGate() session.FinalizationTerminalGate {
	if a == nil {
		return nil
	}
	entryAny, _ := appFinalizationGates.LoadOrStore(a, &appFinalizationGateEntry{})
	entry := entryAny.(*appFinalizationGateEntry)
	entry.once.Do(func() { entry.gate = a.newFinalizationTerminalGate() })
	return entry.gate
}

// newFinalizationTerminalGate builds the gate once per App: nil unless the
// finalization flag is on and a database writer is available.
func (a *App) newFinalizationTerminalGate() session.FinalizationTerminalGate {
	if a.config == nil {
		return nil
	}
	cfg := a.Config()
	if cfg == nil || !cfg.Options.AgentFinalizationEnabled() {
		return nil
	}
	if a.DB == nil {
		return nil
	}
	conn := a.DB()
	if conn == nil {
		return nil
	}
	return session.NewPollingFinalizationTerminalGate(session.NewAgentFinalizationStore(conn), 0)
}
