package server

import (
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
)

// finalizationTerminalGateFor resolves the terminal gate for the server's
// pubsub and SSE wiring points. It is a variable only so flag-on tests can
// substitute a deterministic stub gate without building an App that carries
// the finalization config flag. A nil App or a disabled flag yields nil:
// every consumer treats nil as "no gate" and stays byte-for-byte inert.
var finalizationTerminalGateFor = func(a *appPkg.App) session.FinalizationTerminalGate {
	if a == nil {
		return nil
	}
	return a.FinalizationTerminalGate()
}
