// Startup update check for the web UI. This is the delivery path that
// replaced the one lost with the TUI: upstream published a pubsub message
// Bubble Tea rendered, the fork removed that UI, and for a while the check
// ran and discarded its answer (see the app.checkForUpdates removal).

package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/update"
	"github.com/PHPCraftdream/rush/internal/version"
)

// updateCheckTimeout bounds the startup check. It is best-effort
// decoration: a slow or unreachable release API must never delay the
// server becoming usable, and the check runs in its own goroutine anyway.
const updateCheckTimeout = 30 * time.Second

// broadcastUpdateNotice checks for a newer release once and, if one exists,
// tells every connected client. Thin production wrapper around
// broadcastUpdateNoticeWithClientVersion, always using the real GitHub
// client and the binary's own version.Version.
//
// Deliberately fire-and-forget with no retry and no schedule: the operator
// restarts rush often enough, and a background poller would be a network
// call nobody asked for on a machine that may be offline on purpose. A
// failed check is logged at debug — it is not a problem the user can act
// on, and at warn it would cry wolf on every offline start.
//
// Sends nothing when no update is available, so the client can render on
// receipt without deciding anything.
func broadcastUpdateNotice(ctx context.Context, h *Hub) {
	broadcastUpdateNoticeWithClientVersion(ctx, h, update.Default, version.Version)
}

// broadcastUpdateNoticeWithClientVersion is broadcastUpdateNotice's
// injectable core: current is the version string to compare against (the
// production caller always passes version.Version), and client is the
// update.Client used to fetch the latest release (the production caller
// always passes update.Default, the real GitHub API). Split out purely so
// a test can exercise this exact function — including the h.BroadcastSticky
// call at the bottom, the actual line that decides whether the update
// notice is sticky at all — with a fake Client instead of hitting the
// network or depending on this fork's actual current version string.
func broadcastUpdateNoticeWithClientVersion(ctx context.Context, h *Hub, client update.Client, current string) {
	checkCtx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()

	info, err := update.Check(checkCtx, current, client)
	if err != nil {
		slog.Debug("update check failed", "err", err)
		return
	}
	// A development build has nothing to compare against. Available() treats
	// any pre-release Current against a stable Latest as "an update exists",
	// so without this a locally built binary — "devel-<commit>", or the
	// current 0.2.0-alpha.19 — would raise the badge on EVERY start, forever.
	// IsDevelopment already knows every shape internal/version emits.
	if info.IsDevelopment() {
		slog.Debug("skipping update notice: development build", "version", info.Current)
		return
	}
	if !info.Available() {
		return
	}

	slog.Info("a newer rush is available", "current", info.Current, "latest", info.Latest)
	// Sticky, not a plain Broadcast: this is sent once at server start and
	// the replay ring evicts it within a single streaming turn, so an
	// operator who opens the browser after any real work would never see the
	// badge (task #547). Sticky delivery re-sends it on every client
	// register instead.
	h.BroadcastSticky(EventUpdateAvailable, UpdateAvailableWire{
		Current: info.Current,
		Latest:  info.Latest,
	})
}
