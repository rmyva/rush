// Agent finalization wiring (W1a): builds the finalization driver wiring from
// this App's configuration and database and binds it to the agent
// coordinator. Everything is flag-gated: with options.finalization.enabled
// off this is a no-op that neither allocates, nor opens connections, nor
// touches the terminal gate.

package app

import (
	"errors"

	"github.com/PHPCraftdream/rush/internal/agent"
)

// installAgentFinalizationWiring wires the finalization driver when the flag
// is on. It runs once from InitCoderAgent, after the batch runtime install,
// so the batch install and the notice store are already available. Flag off:
// immediate nil, zero side effects. Flag on: any missing collaborator fails
// closed and aborts agent setup.
func (app *App) installAgentFinalizationWiring() error {
	if app.config == nil || app.AgentCoordinator == nil {
		return nil
	}
	if !app.Config().Options.AgentFinalizationEnabled() {
		return nil
	}
	if app.DB == nil {
		return errors.New("finalization wiring: database accessor is unavailable")
	}
	conn := app.DB()
	if conn == nil {
		return errors.New("finalization wiring: database is unavailable")
	}
	noticeStore := app.batchNoticeStore()
	if noticeStore == nil {
		return errors.New("finalization wiring: session notice store is unavailable")
	}
	return agent.SetCoordinatorFinalizationWiring(app.AgentCoordinator, agent.FinalizationWiringDeps{
		Enabled:     true,
		Conn:        conn,
		Messages:    app.Messages,
		NoticeStore: noticeStore,
		DataDir:     app.dataDir,
		Install:     app.batchInstall,
		ResultGate:  app.FinalizationTerminalGate(),
	})
}
