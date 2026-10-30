// The Drain call kind (phase-4 step 3, docs/plans/2026-09-28-async-phase4-
// durable-core.md sec.3.4): a SessionAgentCall whose only job is the
// driver's notice pull (agent_notice_pull.go) at turn start, followed by an
// empty-prompt reaction turn if (and only if) debt that pull made visible is
// still owed and the ONE launch predicate (drainPermitted, see
// decideDrainTurn in agent_drain_decision.go) allows a turn. Submitted
// through the ordinary Run/submit path: idle -> becomes the owner like any
// call; busy -> queued, merging with whatever is already there
// (mailbox_ownership.go's submit).
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
)

// newDrainCall is the SINGLE constructor for a Drain call: the only place
// IsDrain is ever set to true. base is either a fresh
// SessionAgentCall{SessionID: sessionID} (a root/non-driver session) or a
// delegated child's driver.callFor("") template (doc sec.3.4: "the child's
// drain uses the driver's template via the single constructor"). Either
// way, every field a call kind or notice-flag could otherwise carry forward
// from an active call is reset HERE, not inherited: a Drain never carries
// another call's prompt, notice kind, existing-message reference, or
// queue/durable-persistence markers.
func newDrainCall(base SessionAgentCall) SessionAgentCall {
	base.IsDrain = true
	base.drainTurnCommitted = false
	base.Prompt = ""
	base.AutoResumed = true
	base.BackgroundJobNotice = true
	base.NoticeKind = ""
	base.ExistingMessageID = ""
	base.OnUserMessageCreated = nil
	base.OnAssistantMessageCreated = nil
	base.FromDurableQueue = false
	base.FailIfSessionBusy = false
	base.onQueueResolved = nil
	return base
}

// lastUserMessageOrigin returns the origin of the chronologically latest user
// message. Read failures and empty history fail closed (no web preference).
func lastUserMessageOrigin(ctx context.Context, messages message.Service, sessionID string) (message.Origin, bool) {
	if messages == nil {
		return message.OriginUnspecified, false
	}
	rows, err := messages.ListUserMessages(ctx, sessionID)
	if err != nil {
		slog.Warn("Drain: could not read last user-message origin; using configured models", "session_id", sessionID, "error", err)
		return message.OriginUnspecified, false
	}
	if len(rows) == 0 {
		return message.OriginUnspecified, false
	}
	return rows[0].Origin, true
}

// callFromActive is the single constructor for a call built from an active
// call or a template (the `sessions inject --interrupt` replacement,
// subAgentDriver.callFor): it keeps the source's model/sampling/policy shape
// but resets the call kind and notice flags, which are never inherited (doc
// sec.3.4). Without it, an operator's interrupt landing on a Drain turn
// would itself run as a Drain -- and, with nothing wake-worthy to pull,
// never reach the provider.
func callFromActive(active SessionAgentCall) SessionAgentCall {
	call := active
	call.IsDrain = false
	call.drainTurnCommitted = false
	call.AutoResumed = false
	call.BackgroundJobNotice = false
	call.NoticeKind = ""
	return call
}

func applyDrainModelOptions(call SessionAgentCall, model Model) SessionAgentCall {
	call.SmartModel = &model
	call.MaxOutputTokens = model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		call.MaxOutputTokens = model.ModelCfg.MaxTokens
	}
	call.Temperature = model.ModelCfg.Temperature
	call.TopP = model.ModelCfg.TopP
	call.TopK = model.ModelCfg.TopK
	call.FrequencyPenalty = model.ModelCfg.FrequencyPenalty
	call.PresencePenalty = model.ModelCfg.PresencePenalty
	return call
}

// drainCallFor builds sessionID's Drain call. Slot-derived child drivers
// resolve their live role slot per new automatic turn; explicit/non-slot
// drivers keep the frozen model. Root sessions resolve their own pins as usual.
// R12-H2b §2.3: both branches resolve the session's execution fence
// FIRST — the returned preparedContinuation holds the Continuation owner
// across the dispatch window (the caller releases it after Run returns),
// and its requirement is stamped onto the drain call (and, on the root
// branch, onto the ctx so the pinned toolset is provider-backed). The root
// branch cleans up after itself on every failure: a defer releases the
// Continuation owner and closes the build scope unless the ownership was
// transferred to the successful return. A lost binding fails closed typed
// before anything runs.
func (c *coordinator) drainCallFor(ctx context.Context, sessionID string) (SessionAgentCall, *preparedContinuation, error) {
	if _, ok := c.subAgentDrivers.get(sessionID); !ok && c.isCredentialedRoot(sessionID) {
		return SessionAgentCall{}, nil, fmt.Errorf("%w: session %s", ErrCredentialBindingUnavailable, sessionID)
	}
	if driver, ok := c.subAgentDrivers.get(sessionID); ok {
		// §6.2 (preserved from the old wakeNoticeCall driver branch): every
		// wake re-inherits the driver's parentSessionID's allowlist baseline
		// onto the child BEFORE waking it -- the child's own turn no longer
		// clears its allowlist entry on return, so re-arming here keeps a
		// woken turn judged by the delegation's policy instead of falling
		// back to the process-wide gate.
		if mgr, ok := c.permissions.(permission.SessionRunAllowlistManager); ok && driver.parentSessionID != "" {
			mgr.InheritSessionRunAllowlistForGeneration(driver.parentSessionID, sessionID, driver.generation)
		}
		call := newDrainCall(driver.callFor(""))
		if driver.slotRole != "" && call.Credentials == nil {
			if refreshed, providerCfg, ok := c.refreshDrainModel(ctx, driver.slotRole); ok {
				call = applyDrainModelOptions(call, refreshed)
				call.ProviderOptions = getProviderOptions(sessionID, refreshed, providerCfg)
			}
		}
		c.freshenDrainClient(ctx, &call)
		prep, prepErr := c.PrepareContinuation(ctx, sessionID, requirementFromCall(call))
		if prepErr != nil {
			return SessionAgentCall{}, nil, prepErr
		}
		prep.applyTo(&call)
		return call, prep, nil
	}
	// Root branch: resolve the fence BEFORE model resolution so the pinned
	// toolset is built with the provider already on the ctx (R12-H2b §2.3).
	// Failure cleanup (gh#10b): every error return below that does not hand
	// the Continuation owner to a caller must release it (and close the
	// build scope it registered pins on) — the deferred abandon below runs
	// unless the ownership is transferred at the final return.
	prep, prepErr := c.PrepareContinuation(ctx, sessionID, nil)
	if prepErr != nil {
		return SessionAgentCall{}, nil, prepErr
	}
	var capOpts *CallOptions
	transferred := false
	defer func() {
		if !transferred && prep != nil {
			if capOpts != nil {
				capOpts.buildScope.close()
			}
			prep.release()
		}
	}()
	if prep != nil {
		capOpts = &CallOptions{}
		prep.applyToOptions(capOpts)
		runSpec, policyErr := prep.restorePolicyBaseline(ctx, sessionID, capOpts)
		if policyErr != nil {
			return SessionAgentCall{}, nil, policyErr
		}
		ctx = WithCallOptions(ctx, capOpts)
		// CallOptions remain the source of call policy; the marker now reflects
		// the latest persisted user-message origin rather than inherited setup.
		origin, hasUserMessage := lastUserMessageOrigin(ctx, c.messages, sessionID)
		capOpts.WebTurnOptIn = hasUserMessage && origin == message.OriginWeb && capOpts.ModelRole != config.SelectedModelTypeReviewer
		ctx = WithCallOptions(ctx, capOpts)
		// Carry the fence's folder-scope spec on the ctx too, mirroring
		// ExecuteRun's per-call arming: its presence is what lets the
		// durable queue re-serialize the scope.
		if prep.requirement.FolderScopeSpec != nil {
			ctx = WithFolderScopeSpec(ctx, fromSessionFolderScopeSpec(prep.requirement.FolderScopeSpec))
		}
		// Arm the fence's restricted-run policy in both forms: compiled for
		// this turn's enforcement, spec for the durable re-serialization.
		// Degrade-on-pattern-error, like the rebuild (T12): invalid patterns
		// are dropped with a warning and the compiled allowlist stays
		// restricted — the spec itself is still armed verbatim.
		if runSpec != nil {
			compiled, compileErr := permission.BuildRunAllowlist(*runSpec)
			if compileErr != nil {
				slog.Warn("drainCallFor: dropped invalid restricted-run patterns from the fence policy spec", "session_id", sessionID, "err", compileErr)
			}
			ctx = WithRunAllowlist(ctx, &compiled)
			ctx = WithRunAllowlistSpec(ctx, runSpec)
		}
	}
	if prep == nil {
		// With no execution fence, use CallOptions to carry the web-turn policy
		// into model resolution without replacing unrelated context options.
		origin, hasUserMessage := lastUserMessageOrigin(ctx, c.messages, sessionID)
		if existing := callOptionsFrom(ctx); existing != nil || hasUserMessage && origin == message.OriginWeb {
			var opts CallOptions
			if existing != nil {
				opts = *existing
			}
			opts.WebTurnOptIn = hasUserMessage && origin == message.OriginWeb && opts.ModelRole != config.SelectedModelTypeReviewer
			ctx = WithCallOptions(ctx, &opts)
		}
	}
	pinned, err := c.resolveSessionModels(ctx, sessionID)
	if err != nil {
		return SessionAgentCall{}, nil, err
	}
	// A reaction after a long job may find the OAuth token expired: refresh it
	// BEFORE the call is built (the web and child Drains bypass runInternal,
	// where the CLI refreshes), and resolve the models again so the call
	// carries the new client. A failed refresh proceeds with the token we have.
	if refreshed, refreshErr := c.refreshExpiredToken(ctx, pinned.providerCfg); refreshErr != nil {
		slog.Warn("Drain: OAuth token refresh failed; proceeding with the existing token", "session_id", sessionID, "error", refreshErr)
	} else if refreshed {
		if pinned, err = c.resolveSessionModels(ctx, sessionID); err != nil {
			return SessionAgentCall{}, nil, err
		}
	}
	call, err := c.buildCall(ctx, sessionID, "", pinned, nil)
	if err != nil {
		return SessionAgentCall{}, nil, err
	}
	call = newDrainCall(call)
	// Idempotent stamping: keeps DiskRequirement on the struct itself even
	// though the ctx-carried options already carry it (R12-H2b §2.3).
	prep.applyTo(&call)
	// The Continuation owner now belongs to the caller, which releases it
	// after Run returns; the deferred abandon must not fire.
	transferred = true
	return call, prep, nil
}

// reminderBeforeTail moves preparePrompt's trailing todo reminder in front
// of the last k messages -- the notices a Drain turn's own turn-start pull
// appended -- so they stay the final user messages the provider sees (a
// Drain has no prompt of its own to follow the reminder). No-op unless the
// last message is that reminder and the k before it are user-role.
func reminderBeforeTail(history []fantasy.Message, k int) []fantasy.Message {
	n := len(history)
	if k <= 0 || n < k+1 || !isTodoReminder(history[n-1]) {
		return history
	}
	for _, m := range history[n-1-k : n-1] {
		if m.Role != fantasy.MessageRoleUser {
			return history
		}
	}
	out := make([]fantasy.Message, 0, n)
	out = append(out, history[:n-1-k]...)
	out = append(out, history[n-1])
	return append(out, history[n-1-k:n-1]...)
}

// isTodoReminder reports whether m is preparePrompt's <system_reminder>
// todo message.
func isTodoReminder(m fantasy.Message) bool {
	if m.Role != fantasy.MessageRoleUser || len(m.Content) != 1 {
		return false
	}
	part, ok := fantasy.AsMessagePart[fantasy.TextPart](m.Content[0])
	return ok && strings.HasPrefix(part.Text, "<system_reminder>")
}

// freshenDrainClient brings a delegated child's Drain call onto the current
// refreshable credentials (an OAuth token or an API-key template). The child's
// driver template froze its model client when the delegation started, so after
// a long job (or a refresh by any other caller: the 401 accounting refreshes an
// API-key template and publishes the new key) it carries an old token or key:
// refresh an expired OAuth token, then rebuild the pinned model's client from
// the current config (same model, new client). Best effort: a failure leaves the
// template's client, and a 401 then takes the counted, paced path of the
// accounting. A provider with no refreshable credential keeps its client, and
// per-call credentials (tenant keys) are never replaced with the shared
// config's.
func (c *coordinator) freshenDrainClient(ctx context.Context, call *SessionAgentCall) {
	if c.cfg == nil || call.SmartModel == nil || call.Credentials != nil {
		return
	}
	providerID := call.SmartModel.ModelCfg.Provider
	_, providerCfg, err := c.rebuildInputs(providerID)
	if err != nil || !hasRefreshableCredential(providerCfg) {
		return
	}
	if _, err := c.refreshExpiredToken(ctx, providerCfg); err != nil {
		slog.Warn("Drain: OAuth token refresh failed; proceeding with the existing token", "session_id", call.SessionID, "provider", providerID, "error", err)
		return
	}
	fresh, err := c.rebuildOnCurrentCredentials(ctx, *call.SmartModel, true)
	if err != nil {
		slog.Warn("Drain: could not rebuild the child's model client", "session_id", call.SessionID, "provider", providerID, "error", err)
		return
	}
	call.SmartModel = &fresh
}
