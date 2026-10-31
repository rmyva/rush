// Usage and cost accounting shared by runTurn, the compaction paths, and
// title generation: openrouter cost overrides and session token/cost updates.
package agent

import (
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"

	"github.com/PHPCraftdream/rush/internal/session"
)

func (a *sessionAgent) openrouterCost(metadata fantasy.ProviderMetadata) *float64 {
	openrouterMetadata, ok := metadata[openrouter.Name]
	if !ok {
		return nil
	}

	opts, ok := openrouterMetadata.(*openrouter.ProviderMetadata)
	if !ok {
		return nil
	}
	return &opts.Usage.Cost
}

// updateSessionUsage computes the cost delta for this step, applies the
// new token snapshot to session in-place (token fields are last-snapshot
// overwrite semantics), and returns the cost delta. The caller MUST
// persist the cost delta via sessions.IncrementCost (race-safe additive
// UPDATE) rather than relying on Save, because Save no longer writes the
// cost column.
//
// Fork patch (concurrency): upstream version was void; we now return
// the delta and rely on the caller to drive IncrementCost. See
// CHANGELOG.fork.md (Section 4.I).
//
// Fork merge note (origin/main 6ed8852b / 2e9c6505 / 74e6e378 "fix(agent):
// estimate/harden fallback usage accounting"): adopted upstream's
// updateSessionTokenCounters helper so partial-zero usage chunks no longer
// overwrite accumulated counters with zero. Rejected their `estimated bool`
// parameter (drives session.EstimatedUsage marker — a TUI widget we do not
// ship, see CHANGELOG.fork.md Section 2) and their eventTokensUsed publish
// (no consumer in our WebSocket fan-out).
func (a *sessionAgent) updateSessionUsage(model Model, session *session.Session, usage fantasy.Usage, overrideCost *float64) float64 {
	modelConfig := model.CatwalkCfg
	cost := modelConfig.CostPer1MInCached/1e6*float64(usage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(usage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(usage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(usage.OutputTokens)

	// Use override cost if available (e.g., from OpenRouter).
	if overrideCost != nil {
		cost = *overrideCost
	}

	// Skip cost accumulation
	if model.FlatRate {
		cost = 0
	}

	// #1130: OwnCost mirrors the node's own ledger (cost_self) for the
	// in-memory snapshot; the persisted truth is written by the caller's
	// IncrementCost. Cap decisions read the subtree budget instead.
	session.OwnCost += cost
	updateSessionTokenCounters(session, usage)
	return cost
}

func addStepUsageTokens(total int64, usage fantasy.Usage) int64 {
	partsTotal := usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens + usage.OutputTokens
	if usage.TotalTokens > partsTotal {
		partsTotal = usage.TotalTokens
	}
	return total + partsTotal
}

// updateSessionTokenCounters writes a new usage snapshot into the session
// without overwriting accumulated counters with zero. Fork merge note: from
// origin/main 74e6e378 "fix(agent): harden fallback usage accounting".
//
// PromptTokens must be the FULL prompt: InputTokens, CacheReadTokens and
// CacheCreationTokens are three disjoint classes (see
// internal/agent/cliprovider/usage.go), so all three belong in the sum.
//
// This used to add only InputTokens + CacheReadTokens, silently dropping
// cache-WRITE tokens. That understated the prompt for every provider that
// reports the three separately — the Anthropic HTTP provider always did
// (fantasy anthropic.go maps input_tokens exclusive of both cache counters),
// and claude-cli joined it once its parser stopped folding cache into input.
// A real measured turn had input=5842 / cache_creation=16984 / cache_read=0:
// the prompt is 22826 tokens but was recorded as 5842, a 74% understatement.
//
// PromptTokens drives the auto-summarization trigger (the remaining-context
// checks against CatwalkCfg.ContextWindow), so understating it delays
// compaction and risks running the context window over instead of sliding it.
func updateSessionTokenCounters(session *session.Session, usage fantasy.Usage) {
	if usage.OutputTokens != 0 {
		session.CompletionTokens = usage.OutputTokens
	}
	promptTokens := usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens
	if promptTokens != 0 {
		session.PromptTokens = promptTokens
	}
}
