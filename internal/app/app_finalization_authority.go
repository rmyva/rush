// W1c: the app-owned finalization authority registry. Built lazily, exactly
// once, when options.finalization.enabled is on: the batch runtime install
// reads it into the commit-hook identity and the agent finalization wiring
// registers each root run's bound authority into it. Flag off: nil, nothing
// allocated, no behavior change anywhere.

package app

import (
	"github.com/PHPCraftdream/rush/internal/agent"
)

// finalizationAuthorityRegistry returns the App's single owner-to-authority
// registry when the finalization flag is on, building it on first use. The
// batch install runs before the agent wiring, so the instance the commit
// hook sees is the same one the wiring binds authorities into. A disabled
// flag (or a config-less App) returns nil and allocates nothing.
func (app *App) finalizationAuthorityRegistry() *agent.FinalizationAuthorityRegistry {
	if app.finalizationAuthorities != nil {
		return app.finalizationAuthorities
	}
	if app.config == nil || !app.Config().Options.AgentFinalizationEnabled() {
		return nil
	}
	registry := agent.NewFinalizationAuthorityRegistry()
	app.finalizationAuthorities = registry
	return registry
}
