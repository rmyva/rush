// Provider client construction, per-provider options, credential refresh,
// and peak-hours policy. Extracted from coordinator.go — pure code move,
// bodies unchanged.

package agent

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/codexprovider"
	"github.com/PHPCraftdream/rush/internal/agent/hyper"
	mcp "github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/discover"
	"github.com/PHPCraftdream/rush/internal/event"
	"github.com/PHPCraftdream/rush/internal/oauth/copilot"
	"github.com/PHPCraftdream/rush/internal/session"
	openaisdk "github.com/charmbracelet/openai-go/option"
	"github.com/qjebbs/go-jsons"
)

// Copilot models that use the Responses API instead of Chat Completions.
var copilotResponsesModels = map[string]bool{
	"gpt-5.2":       true,
	"gpt-5.2-codex": true,
	"gpt-5.3-codex": true,
	"gpt-5.4":       true,
	"gpt-5.4-mini":  true,
	"gpt-5.5":       true,
	"gpt-5-mini":    true,
}

// OpenCode models that use Anthropic Messages API instead of Chat Completions.
// Ported from upstream b7f4ad6c (#3040).
var opencodeMessagesModels = map[string]bool{
	"qwen3.7-max": true,
}

// zai53ModelIDs are the Z.AI model ids whose reasoning-effort contract
// matches zai53ReasoningLevels in internal/cmd/models_atoms.go (low/high/max,
// no off — see that var's comment for the verification). Registered here as
// a map rather than inline `||` comparisons so a future sibling id is a
// one-line addition; the `internal/cmd` atom registry can't be imported
// directly (it doesn't export atomRegistry), so this list is the other half
// of the three-way SYNC WARNING pairing this file already documents.
var zai53ModelIDs = map[string]bool{
	"glm-5.3":       true,
	"glm-5.3-flash": true,
}

// effectiveReasoningEffort returns the reasoning effort to apply for provider calls.
// It prefers the user-selected effort when valid, otherwise the model default when
// valid, and finally falls back to the first configured reasoning level.
func effectiveReasoningEffort(model Model) string {
	if !model.CatwalkCfg.CanReason {
		return ""
	}

	if effort := model.ModelCfg.ReasoningEffort; effort != "" && slices.Contains(model.CatwalkCfg.ReasoningLevels, effort) {
		return effort
	}
	if effort := model.CatwalkCfg.DefaultReasoningEffort; effort != "" && slices.Contains(model.CatwalkCfg.ReasoningLevels, effort) {
		return effort
	}
	if len(model.CatwalkCfg.ReasoningLevels) > 0 {
		return model.CatwalkCfg.ReasoningLevels[0]
	}
	return ""
}

// fantasyAcceptsReasoningEffort reports whether fantasy's OpenAI-compatible
// providers take effort as the top-level reasoning_effort value.
func fantasyAcceptsReasoningEffort(effort string) bool {
	switch openai.ReasoningEffort(effort) {
	case openai.ReasoningEffortNone, openai.ReasoningEffortMinimal, openai.ReasoningEffortLow,
		openai.ReasoningEffortMedium, openai.ReasoningEffortHigh, openai.ReasoningEffortXHigh:
		return true
	}
	return false
}

func getProviderOptions(sessionID string, model Model, providerCfg config.ProviderConfig) fantasy.ProviderOptions {
	options := fantasy.ProviderOptions{}

	cfgOpts := []byte("{}")
	providerCfgOpts := []byte("{}")
	catwalkOpts := []byte("{}")

	if model.ModelCfg.ProviderOptions != nil {
		data, err := json.Marshal(model.ModelCfg.ProviderOptions)
		if err == nil {
			cfgOpts = data
		}
	}

	if providerCfg.ProviderOptions != nil {
		data, err := json.Marshal(providerCfg.ProviderOptions)
		if err == nil {
			providerCfgOpts = data
		}
	}

	if model.CatwalkCfg.Options.ProviderOptions != nil {
		data, err := json.Marshal(model.CatwalkCfg.Options.ProviderOptions)
		if err == nil {
			catwalkOpts = data
		}
	}

	readers := []io.Reader{
		bytes.NewReader(catwalkOpts),
		bytes.NewReader(providerCfgOpts),
		bytes.NewReader(cfgOpts),
	}

	got, err := jsons.Merge(readers)
	if err != nil {
		slog.Error("Could not merge call config", "err", err)
		return options
	}

	mergedOptions := make(map[string]any)

	err = json.Unmarshal([]byte(got), &mergedOptions)
	if err != nil {
		slog.Error("Could not create config for call", "err", err)
		return options
	}

	reasoningEffort := effectiveReasoningEffort(model)
	shouldSetEffort := model.CatwalkCfg.CanReason &&
		reasoningEffort != "" &&
		slices.Contains(model.CatwalkCfg.ReasoningLevels, reasoningEffort)
	if verified, ok := providerCfg.LiveEfforts[model.ModelCfg.Model]; ok &&
		model.ModelCfg.ReasoningEffort == "" && verified.Default == "" {
		shouldSetEffort = false
	}

	switch providerCfg.Type {
	case openai.Name, azure.Name:
		_, hasReasoningEffort := mergedOptions["reasoning_effort"]
		if !hasReasoningEffort && shouldSetEffort {
			mergedOptions["reasoning_effort"] = reasoningEffort
		}
		if openai.IsResponsesModel(model.CatwalkCfg.ID) {
			if openai.IsResponsesReasoningModel(model.CatwalkCfg.ID) {
				mergedOptions["reasoning_summary"] = "auto"
				mergedOptions["include"] = []openai.IncludeType{openai.IncludeReasoningEncryptedContent}
			}
			parsed, err := openai.ParseResponsesOptions(mergedOptions)
			if err == nil {
				// Route cache-affine requests to the same backend host (session-stable, opaque).
				if parsed.PromptCacheKey == nil && sessionID != "" {
					hash := session.HashID(sessionID)
					parsed.PromptCacheKey = &hash
				}
				options[openai.Name] = parsed
			}
		} else {
			parsed, err := openai.ParseOptions(mergedOptions)
			if err == nil {
				// Route cache-affine requests to the same backend host (session-stable, opaque).
				if parsed.PromptCacheKey == nil && sessionID != "" {
					hash := session.HashID(sessionID)
					parsed.PromptCacheKey = &hash
				}
				options[openai.Name] = parsed
			}
		}
	case anthropic.Name, bedrock.Name:
		var (
			_, hasEffort = mergedOptions["effort"]
			_, hasThink  = mergedOptions["thinking"]
			extraBody    = make(map[string]any)
		)

		switch providerCfg.ID {
		case string(catwalk.InferenceProviderAlibabaSingapore):
			switch {
			case !hasEffort && shouldSetEffort:
				extraBody["reasoning_effort"] = reasoningEffort
			case !hasThink && model.CatwalkCfg.CanReason:
				if model.ModelCfg.Think {
					extraBody["thinking"] = map[string]any{"type": "enabled"}
				} else {
					extraBody["thinking"] = map[string]any{"type": "disabled"}
				}
			}
			mergedOptions["extra_body"] = extraBody

		default:
			switch {
			case !hasEffort && shouldSetEffort:
				mergedOptions["effort"] = reasoningEffort
			case !hasThink && model.ModelCfg.Think:
				mergedOptions["thinking"] = map[string]any{"budget_tokens": 2000}
			}
		}

		parsed, err := anthropic.ParseOptions(mergedOptions)
		if err == nil {
			options[anthropic.Name] = parsed
		}

	case openrouter.Name:
		_, hasReasoning := mergedOptions["reasoning"]
		if !hasReasoning && shouldSetEffort {
			mergedOptions["reasoning"] = map[string]any{
				"enabled": true,
				"effort":  reasoningEffort,
			}
		}
		parsed, err := openrouter.ParseOptions(mergedOptions)
		if err == nil {
			options[openrouter.Name] = parsed
		}
	case vercel.Name:
		_, hasReasoning := mergedOptions["reasoning"]
		if !hasReasoning && shouldSetEffort {
			mergedOptions["reasoning"] = map[string]any{
				"enabled": true,
				"effort":  reasoningEffort,
			}
		}
		parsed, err := vercel.ParseOptions(mergedOptions)
		if err == nil {
			options[vercel.Name] = parsed
		}
	case google.Name, catwalk.TypeVertexAI:
		_, hasReasoning := mergedOptions["thinking_config"]
		if !hasReasoning {
			if strings.HasPrefix(model.CatwalkCfg.ID, "gemini-2") {
				mergedOptions["thinking_config"] = map[string]any{
					"thinking_budget":  2000,
					"include_thoughts": true,
				}
			} else {
				mergedOptions["thinking_config"] = map[string]any{
					"thinking_level":   reasoningEffort,
					"include_thoughts": true,
				}
			}
		}
		parsed, err := google.ParseOptions(mergedOptions)
		if err == nil {
			options[google.Name] = parsed
		}
	case openaicompat.Name, hyper.Name:
		extraBody := make(map[string]any)

		_, hasReasoningEffort := mergedOptions["reasoning_effort"]
		if !hasReasoningEffort && shouldSetEffort {
			switch providerCfg.ID {
			case string(catwalk.InferenceProviderIoNet):
				extraBody["reasoning"] = map[string]string{"effort": reasoningEffort}
			default:
				mergedOptions["reasoning_effort"] = reasoningEffort
			}
		}

		// "reasoning effort" is a standard OpenAI field, but "thinking" is not.
		// Setting it in the right way for each provider.
		// TODO: Abstract this in Fantasy somehow?
		// TODO: Allow custom providers to specify how to set this?
		//
		// SYNC WARNING: the per-provider effort/thinking mapping below (ZAI,
		// DeepSeek, io.net, Alibaba Singapore, hyper) is restated in prose for
		// users by `rush models efforts` (providerEffortDocs in
		// internal/cmd/models_efforts.go). That prose is NOT derived from this
		// switch — if you change a mapping here, update the matching entry
		// there too, or the CLI help will describe stale behavior.
		switch providerCfg.ID {
		case hyper.Name:
			extraBody["thinking"] = model.ModelCfg.Think
		case string(catwalk.InferenceProviderIoNet):
			if _, ok := extraBody["reasoning"]; !ok && model.CatwalkCfg.CanReason {
				if model.ModelCfg.Think {
					extraBody["reasoning"] = map[string]string{"effort": "medium"}
				} else {
					extraBody["reasoning"] = map[string]string{"effort": "none"}
				}
			}
		case string(catwalk.InferenceProviderZAI):
			effort := strings.ToLower(model.ModelCfg.ReasoningEffort)
			modelID := strings.ToLower(model.ModelCfg.Model)
			if verified, ok := providerCfg.LiveEfforts[model.ModelCfg.Model]; ok {
				extraBody["thinking"] = map[string]any{"type": "enabled"}
				if effort == "" || !slices.Contains(verified.Levels, effort) {
					break
				}
				if effort == "none" || effort == "minimal" {
					extraBody["thinking"] = map[string]any{"type": "disabled"}
					delete(mergedOptions, "reasoning_effort")
				} else {
					extraBody["reasoning_effort"] = effort
				}
				break
			}
			if zai53ModelIDs[modelID] {
				// GLM-5.3/5.3-Flash can't disable reasoning at all (unlike
				// every model in the branch below) and take low/high/max
				// instead of off/high/max — see zai53ReasoningLevels' comment
				// in models_atoms.go for the verification and SYNC WARNING.
				// "off" (raw-syntax only, rejected by `rush models use` for
				// these two atoms) degrades to "low"; unset stays "high" per
				// this fork's existing convention, not the doc's own "max".
				extraBody["thinking"] = map[string]any{
					"type": "enabled",
				}
				switch effort {
				case "off", "low":
					extraBody["reasoning_effort"] = "low"
				case "high":
					extraBody["reasoning_effort"] = "high"
				case "xhigh", "max", "ultracode":
					extraBody["reasoning_effort"] = "max"
				default:
					extraBody["reasoning_effort"] = "high"
				}
				break
			}
			// GLM-5.x (pre-5.3) exposes two thinking-effort levels
			// (high / max) and no "unset" state on z.ai's side — reasoning
			// is either on at some level or off. Z.AI recommends max for
			// hard coding/math tasks, so an unset effort defaults to
			// thinking ON at "high" rather than silently disabling
			// reasoning. Explicitly opt out with ReasoningEffort == "off"
			// (e.g. `rush models use zai/glm-5.2@off <fast>` — the raw
			// provider/model@effort syntax accepts any suffix, unvalidated
			// against ReasoningLevels).
			//
			// We forward via `extra_body.reasoning_effort` — the
			// OpenAI-compat field z.ai already accepts on its Anthropic-
			// compatible coding endpoint. Mapping mirrors z.ai's own
			// "Claude Code selected effort → GLM-5.2 actual mapped effort"
			// table from docs.z.ai/devpack/latest-model:
			//   (unset), low, medium, high (default) → high
			//   xhigh, max, ultracode                → max
			//   off                                  → thinking disabled
			// Older GLM-4.x ignore the field harmlessly.
			if effort == "off" {
				extraBody["thinking"] = map[string]any{
					"type": "disabled",
				}
			} else {
				extraBody["thinking"] = map[string]any{
					"type": "enabled",
				}
				switch effort {
				case "xhigh", "max", "ultracode":
					extraBody["reasoning_effort"] = "max"
				default:
					extraBody["reasoning_effort"] = "high"
				}
			}
		case string(catwalk.InferenceProviderDeepSeek):
			// DeepSeek keeps the fork's original "unset reasoning = thinking
			// off" default. Reasoning is only enabled when the user opts in
			// (via Think or an explicit ReasoningEffort); the ZAI-only default
			// of turning thinking ON at "high" for an unset effort deliberately
			// does NOT apply here.
			if model.ModelCfg.Think || model.ModelCfg.ReasoningEffort != "" {
				extraBody["thinking"] = map[string]any{
					"type": "enabled",
				}
			} else {
				extraBody["thinking"] = map[string]any{
					"type": "disabled",
				}
			}
			// When reasoning is enabled, map the selected effort onto the two
			// effort levels the endpoint exposes (high / max). Mirrors the ZAI
			// table above:
			//   low, medium, high (default) → high
			//   xhigh, max, ultracode       → max
			if model.ModelCfg.ReasoningEffort != "" {
				switch strings.ToLower(model.ModelCfg.ReasoningEffort) {
				case "xhigh", "max", "ultracode":
					extraBody["reasoning_effort"] = "max"
				default:
					extraBody["reasoning_effort"] = "high"
				}
			}
		case string(catwalk.InferenceProviderAlibabaSingapore):
			if model.CatwalkCfg.CanReason {
				extraBody["enable_thinking"] = model.ModelCfg.Think
			}
		}

		// z.ai and DeepSeek carry their effort in extra_body, in wire values
		// ("max") the OpenAI vocabulary does not have. Fantasy validates the
		// top-level reasoning_effort against that vocabulary and fails the
		// whole call ("reasoning model `max` not supported"), so a value it
		// would reject must not also ride there: it made every reviewer pass
		// at @max fail.
		switch providerCfg.ID {
		case string(catwalk.InferenceProviderZAI), string(catwalk.InferenceProviderDeepSeek):
			if effort, ok := mergedOptions["reasoning_effort"].(string); ok && !fantasyAcceptsReasoningEffort(effort) {
				delete(mergedOptions, "reasoning_effort")
			}
		}

		mergedOptions["extra_body"] = extraBody

		parsed, err := openaicompat.ParseOptions(mergedOptions)
		if err == nil {
			options[openaicompat.Name] = parsed
		}
	default:
		// Known custom providers (litellm, ollama, omlx, lmstudio) speak
		// openai-compat under the hood, so route their options through the
		// openai-compat parser too.
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			parsed, err := openaicompat.ParseOptions(mergedOptions)
			if err == nil {
				options[openaicompat.Name] = parsed
			}
		}
	}

	return options
}

func mergeCallOptions(sessionID string, model Model, cfg config.ProviderConfig) (fantasy.ProviderOptions, *float64, *float64, *int64, *float64, *float64) {
	modelOptions := getProviderOptions(sessionID, model, cfg)
	temp := cmp.Or(model.ModelCfg.Temperature, model.CatwalkCfg.Options.Temperature)
	topP := cmp.Or(model.ModelCfg.TopP, model.CatwalkCfg.Options.TopP)
	topK := cmp.Or(model.ModelCfg.TopK, model.CatwalkCfg.Options.TopK)
	freqPenalty := cmp.Or(model.ModelCfg.FrequencyPenalty, model.CatwalkCfg.Options.FrequencyPenalty)
	presPenalty := cmp.Or(model.ModelCfg.PresencePenalty, model.CatwalkCfg.Options.PresencePenalty)
	return modelOptions, temp, topP, topK, freqPenalty, presPenalty
}

// rebuildPinnedModel rebuilds only the provider client for a previously
// selected model. The selected model config and its catalog metadata stay
// pinned; refreshed provider state must not re-resolve the current defaults.
//
// cfg MUST be the SAME snapshot providerCfg was resolved from (round-9 F6
// residual): this rebuild used to receive providerCfg from the caller's
// fetch but take the global network/debug defaults from its own second,
// later c.cfg.Snapshot() — a reload landing between the two reads built
// buildProvider(cfgB, providerCfgA), a provider+network pairing that never
// existed in any single config generation (e.g. generation A's provider
// DNS with generation B's global proxy). Threading the caller's snapshot
// down — the same pattern buildProvider and resolveProviderHTTPClient
// already use — makes the pair atomic by construction. Callers get both
// values from one read via rebuildInputs.
func (c *coordinator) rebuildPinnedModel(ctx context.Context, cfg *config.Config, model Model, providerCfg config.ProviderConfig, isSubAgent bool) (Model, error) {
	if providerCfg.ID == "" {
		return Model{}, errModelProviderNotConfigured
	}

	provider, err := c.buildProvider(cfg, providerCfg, model.ModelCfg, isSubAgent)
	if err != nil {
		return Model{}, err
	}

	modelID := model.ModelCfg.Model
	if providerCfg.ID == openrouter.Name && isExactoSupported(modelID) {
		modelID += ":exacto"
	}
	languageModel, err := provider.LanguageModel(ctx, modelID)
	if err != nil {
		return Model{}, err
	}

	model.Model = languageModel
	model.FlatRate = providerCfg.FlatRate
	return model, nil
}

// rebuildInputs returns the config snapshot and the named provider's config
// FROM THAT SAME snapshot, for the pinned-model rebuild paths (sub-agent
// refresh, summarize refresh). Fetching both from one store read is the
// whole point (round-9 F6 residual): the previous flow read providerCfg via
// currentProviderConfig's snapshot and let rebuildPinnedModel take a
// second, separately-timed snapshot for the globals, so a reload landing
// between the two reads handed the rebuild a mixed cross-generation pair.
func (c *coordinator) rebuildInputs(providerID string) (*config.Config, config.ProviderConfig, error) {
	cfg, _ := c.cfg.Snapshot()
	providerCfg, ok := cfg.Providers.Get(providerID)
	if !ok {
		return nil, config.ProviderConfig{}, errModelProviderNotConfigured
	}
	return cfg, providerCfg, nil
}

func (c *coordinator) currentProviderConfig(providerID string) (config.ProviderConfig, error) {
	cfg, _ := c.cfg.Snapshot()
	providerCfg, ok := cfg.Providers.Get(providerID)
	if !ok {
		return config.ProviderConfig{}, errModelProviderNotConfigured
	}
	return providerCfg, nil
}

func anthropicHeaderValue(headers map[string]string, name string) (string, bool) {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		if strings.EqualFold(key, name) {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return "", false
	}
	if value, ok := headers[name]; ok {
		return value, true
	}
	slices.Sort(keys)
	return headers[keys[0]], true
}

func setAnthropicHeader(headers map[string]string, name, value string) {
	for key := range headers {
		if strings.EqualFold(key, name) {
			delete(headers, key)
		}
	}
	headers[name] = value
}

func configureAnthropicAuthHeaders(headers map[string]string, apiKey, providerID string) map[string]string {
	headers = maps.Clone(headers)
	if headers == nil {
		headers = make(map[string]string)
	}
	operatorAPIKey, hasOperatorAPIKey := anthropicHeaderValue(headers, "x-api-key")
	operatorAuthorization, hasOperatorAuthorization := anthropicHeaderValue(headers, "Authorization")

	// Remove all case variants before adding one canonical value. This makes
	// auth precedence independent of map iteration order.
	setAnthropicHeader(headers, "x-api-key", "")
	setAnthropicHeader(headers, "Authorization", "")

	switch {
	case strings.HasPrefix(apiKey, "Bearer "):
		setAnthropicHeader(headers, "x-api-key", "")
		setAnthropicHeader(headers, "Authorization", apiKey)
	case providerID == string(catwalk.InferenceProviderMiniMax) || providerID == string(catwalk.InferenceProviderMiniMaxChina):
		setAnthropicHeader(headers, "x-api-key", "")
		setAnthropicHeader(headers, "Authorization", "Bearer "+apiKey)
	case apiKey != "":
		setAnthropicHeader(headers, "x-api-key", apiKey)
		if hasOperatorAuthorization {
			setAnthropicHeader(headers, "Authorization", operatorAuthorization)
		}
	default:
		if hasOperatorAPIKey {
			setAnthropicHeader(headers, "x-api-key", operatorAPIKey)
		}
		if hasOperatorAuthorization {
			setAnthropicHeader(headers, "Authorization", operatorAuthorization)
		}
	}

	return headers
}

func (c *coordinator) buildAnthropicProvider(baseURL, apiKey string, headers map[string]string, providerID string, httpClient *http.Client) (fantasy.Provider, error) {
	var opts []anthropic.Option
	headers = configureAnthropicAuthHeaders(headers, apiKey, providerID)

	if apiKey != "" &&
		!strings.HasPrefix(apiKey, "Bearer ") &&
		providerID != string(catwalk.InferenceProviderMiniMax) &&
		providerID != string(catwalk.InferenceProviderMiniMaxChina) {
		// X-Api-Key header.
		opts = append(opts, anthropic.WithAPIKey(apiKey))
	}

	if len(headers) > 0 {
		opts = append(opts, anthropic.WithHeaders(headers))
	}

	if baseURL != "" {
		opts = append(opts, anthropic.WithBaseURL(baseURL))
	}

	opts = append(opts, anthropic.WithHTTPClient(withoutEmptyAuthHeaders(httpClient)))
	return anthropic.New(opts...)
}

func (c *coordinator) buildOpenaiProvider(baseURL, apiKey string, headers map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
	opts := []openai.Option{
		openai.WithAPIKey(apiKey),
		openai.WithUseResponsesAPI(),
	}
	if httpClient != nil {
		opts = append(opts, openai.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, openai.WithHeaders(headers))
	}
	if baseURL != "" {
		opts = append(opts, openai.WithBaseURL(baseURL))
	}
	return openai.New(opts...)
}

func (c *coordinator) buildOpenrouterProvider(_ string, apiKey string, headers map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
	opts := []openrouter.Option{
		openrouter.WithAPIKey(apiKey),
	}
	if httpClient != nil {
		opts = append(opts, openrouter.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, openrouter.WithHeaders(headers))
	}
	return openrouter.New(opts...)
}

func (c *coordinator) buildVercelProvider(_ string, apiKey string, headers map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
	opts := []vercel.Option{
		vercel.WithAPIKey(apiKey),
	}
	if httpClient != nil {
		opts = append(opts, vercel.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, vercel.WithHeaders(headers))
	}
	return vercel.New(opts...)
}

// buildOpenaiCompatProvider takes the SAME pinned cfg snapshot (F6) the
// caller resolved providerCfg from: its Options.Debug flag shapes the
// Copilot transport and must not come from a separately-timed live read.
func (c *coordinator) buildOpenaiCompatProvider(cfg *config.Config, baseURL, apiKey string, headers map[string]string, extraBody map[string]any, providerID string, isSubAgent bool, httpClient *http.Client) (fantasy.Provider, error) {
	opts := []openaicompat.Option{
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(apiKey),
	}

	// Set HTTP client based on provider and debug mode. Copilot builds its
	// own initiator-transport client, layered on top of the resolved
	// network transport; everything else uses the resolved client as-is.
	switch providerID {
	case string(catwalk.InferenceProviderCopilot):
		opts = append(
			opts,
			openaicompat.WithUseResponsesAPI(),
			openaicompat.WithResponsesAPIFunc(func(modelID string) bool {
				return copilotResponsesModels[modelID]
			}),
		)
		opts = append(opts, openaicompat.WithHTTPClient(copilot.NewClientWithTransport(
			isSubAgent, cfg.Options.Debug, copilotBaseTransport(httpClient))))
	default:
		if httpClient != nil {
			opts = append(opts, openaicompat.WithHTTPClient(httpClient))
		}
	}

	if len(headers) > 0 {
		opts = append(opts, openaicompat.WithHeaders(headers))
	}

	for extraKey, extraValue := range extraBody {
		opts = append(opts, openaicompat.WithSDKOptions(openaisdk.WithJSONSet(extraKey, extraValue)))
	}

	return openaicompat.New(opts...)
}

func (c *coordinator) buildAzureProvider(baseURL, apiKey string, headers map[string]string, options map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
	opts := []azure.Option{
		azure.WithBaseURL(baseURL),
		azure.WithAPIKey(apiKey),
		azure.WithUseResponsesAPI(),
	}
	if httpClient != nil {
		opts = append(opts, azure.WithHTTPClient(httpClient))
	}
	if options == nil {
		options = make(map[string]string)
	}
	if apiVersion, ok := options["apiVersion"]; ok {
		opts = append(opts, azure.WithAPIVersion(apiVersion))
	}
	if len(headers) > 0 {
		opts = append(opts, azure.WithHeaders(headers))
	}

	return azure.New(opts...)
}

func (c *coordinator) buildBedrockProvider(apiKey string, headers map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
	var opts []bedrock.Option
	if httpClient != nil {
		opts = append(opts, bedrock.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, bedrock.WithHeaders(headers))
	}
	switch {
	case apiKey != "":
		opts = append(opts, bedrock.WithAPIKey(apiKey))
	case os.Getenv("AWS_BEARER_TOKEN_BEDROCK") != "":
		opts = append(opts, bedrock.WithAPIKey(os.Getenv("AWS_BEARER_TOKEN_BEDROCK")))
	default:
		// Skip, let the SDK do authentication.
	}
	return bedrock.New(opts...)
}

func (c *coordinator) buildGoogleProvider(baseURL, apiKey string, headers map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
	opts := []google.Option{
		google.WithBaseURL(baseURL),
		google.WithGeminiAPIKey(apiKey),
	}
	if httpClient != nil {
		opts = append(opts, google.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}
	return google.New(opts...)
}

func (c *coordinator) buildGoogleVertexProvider(headers map[string]string, options map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
	opts := []google.Option{}
	if httpClient != nil {
		opts = append(opts, google.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}

	project := options["project"]
	location := options["location"]

	opts = append(opts, google.WithVertex(project, location))

	return google.New(opts...)
}

func (c *coordinator) isAnthropicThinking(model config.SelectedModel) bool {
	if model.Think {
		return true
	}
	opts, err := anthropic.ParseOptions(model.ProviderOptions)
	return err == nil && opts.Thinking != nil
}

// buildProvider takes the pinned cfg snapshot (F6) the caller resolved
// providerCfg from; that SAME snapshot supplies the global network
// defaults resolveProviderHTTPClient composes the HTTP client from.
func (c *coordinator) buildProvider(cfg *config.Config, providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool) (fantasy.Provider, error) {
	if providerCfg.ID == "openai-codex" {
		return c.buildProviderWithValues(cfg, providerCfg, model, isSubAgent, "", "")
	}
	apiKey, _ := c.cfg.Resolve(providerCfg.APIKey)
	baseURL, _ := c.cfg.Resolve(providerCfg.BaseURL)
	return c.buildProviderWithValues(cfg, providerCfg, model, isSubAgent, apiKey, baseURL)
}

// buildProviderWithLiteralCredentials builds a provider from credentials that
// are already resolved by the caller. Per-call tenant credentials are opaque
// values, so this boundary deliberately does not consult the config resolver.
// cfg is the caller's pinned config snapshot (F6) and only supplies the
// global network defaults for the HTTP client — it never feeds key/URL
// resolution.
func (c *coordinator) buildProviderWithLiteralCredentials(cfg *config.Config, providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool) (fantasy.Provider, error) {
	return c.buildProviderWithValues(cfg, providerCfg, model, isSubAgent, providerCfg.APIKey, providerCfg.BaseURL)
}

// buildProviderWithValues wraps every provider built for agent sessions so
// its model calls land in the heartbeat snapshot.
func (c *coordinator) buildProviderWithValues(cfg *config.Config, providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool, apiKey, baseURL string) (fantasy.Provider, error) {
	p, err := c.buildProviderWithValuesInner(cfg, providerCfg, model, isSubAgent, apiKey, baseURL)
	if err != nil {
		return nil, err
	}
	return NewHeartbeatProvider(p), nil
}

func (c *coordinator) buildProviderWithValuesInner(cfg *config.Config, providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool, apiKey, baseURL string) (fantasy.Provider, error) {
	headers := maps.Clone(providerCfg.ExtraHeaders)
	if headers == nil {
		headers = make(map[string]string)
	}

	// handle special headers for anthropic
	if providerCfg.Type == anthropic.Name && c.isAnthropicThinking(model) {
		if v, ok := headers["anthropic-beta"]; ok {
			headers["anthropic-beta"] = v + ",interleaved-thinking-2025-05-14"
		} else {
			headers["anthropic-beta"] = "interleaved-thinking-2025-05-14"
		}
	}

	httpClient, err := c.resolveProviderHTTPClient(cfg, providerCfg)
	if err != nil {
		return nil, err
	}

	switch providerCfg.ID {
	case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
		if opencodeMessagesModels[model.Model] {
			baseURL = strings.TrimSuffix(baseURL, "/v1")
			return c.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID, httpClient)
		}
	}
	if providerCfg.ID == "openai-codex" {
		if c != nil && c.cfg != nil {
			return newCodexTokenProvider(httpClient, &codexTokenSource{store: c.cfg, resolve: c.resolveProviderHTTPClient})
		}
		return codexprovider.New(httpClient, providerCfg.OAuthToken)
	}
	httpClient = c.withOAuthBearerClient(providerCfg, httpClient)

	switch providerCfg.Type {
	case openai.Name:
		return c.buildOpenaiProvider(baseURL, apiKey, headers, httpClient)
	case anthropic.Name:
		return c.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID, httpClient)
	case openrouter.Name:
		return c.buildOpenrouterProvider(baseURL, apiKey, headers, httpClient)
	case vercel.Name:
		return c.buildVercelProvider(baseURL, apiKey, headers, httpClient)
	case azure.Name:
		return c.buildAzureProvider(baseURL, apiKey, headers, providerCfg.ExtraParams, httpClient)
	case bedrock.Name:
		return c.buildBedrockProvider(apiKey, headers, httpClient)
	case google.Name:
		return c.buildGoogleProvider(baseURL, apiKey, headers, httpClient)
	case "google-vertex":
		return c.buildGoogleVertexProvider(headers, providerCfg.ExtraParams, httpClient)
	case openaicompat.Name, hyper.Name:
		switch providerCfg.ID {
		case hyper.Name:
			baseURL = hyper.BaseURL() + "/v1"
			headers["x-rush-id"] = event.GetID()
		case string(catwalk.InferenceProviderZAI):
			extraBody := maps.Clone(providerCfg.ExtraBody)
			if extraBody == nil {
				extraBody = make(map[string]any)
			}
			extraBody["tool_stream"] = true
			return c.buildOpenaiCompatProvider(cfg, baseURL, apiKey, headers, extraBody, providerCfg.ID, isSubAgent, httpClient)
		}
		return c.buildOpenaiCompatProvider(cfg, baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent, httpClient)
	case cliprovider.ProviderType:
		return cliprovider.New(c.cfg.WorkingDir(), c.cfg.Config().Options.DataDirectory, c.permissions.SkipRequests, c.permissions, c.sessions, &externalMCPProxy{cfg: c.cfg, owner: c.mcpOwner}), nil
	default:
		// Known custom providers (litellm, ollama, omlx, lmstudio) are
		// openai-compat under the hood.
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			return c.buildOpenaiCompatProvider(cfg, baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent, httpClient)
		}
		return nil, fmt.Errorf("provider type not supported: %q", providerCfg.Type)
	}
}

// externalMCPProxy implements cliprovider.ExternalMCPProxy by delegating to
// the internal mcp package for tool listing and execution.
type externalMCPProxy struct {
	cfg   *config.ConfigStore
	owner *mcp.Owner
}

func (p *externalMCPProxy) ListTools() []cliprovider.ExternalMCPTool {
	var result []cliprovider.ExternalMCPTool
	// Read path stays on the package wrappers: the registry maps are shared
	// name-keyed state; the IsConfigured cfg filter IS the isolation mechanism.
	for serverName, tools := range mcp.Tools() {
		if !mcp.IsConfigured(p.cfg, serverName) {
			continue
		}
		for _, t := range tools {
			result = append(result, cliprovider.ExternalMCPTool{
				ServerName:  serverName,
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			})
		}
	}
	return result
}

func (p *externalMCPProxy) CallTool(ctx context.Context, serverName, toolName, inputJSON string) (string, error) {
	result, err := p.owner.RunTool(ctx, p.cfg, serverName, toolName, inputJSON)
	if err != nil {
		return "", err
	}
	return result.Content, nil
}

func isExactoSupported(modelID string) bool {
	supportedModels := []string{
		"moonshotai/kimi-k2-0905",
		"deepseek/deepseek-v3.1-terminus",
		"z-ai/glm-4.6",
		"openai/gpt-oss-120b",
		"qwen/qwen3-coder",
	}
	return slices.Contains(supportedModels, modelID)
}
