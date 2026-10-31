package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

// parsePeakHoursWindow parses a "HH:MM-HH:MM" window string (e.g.
// "09:00-18:00") into a *config.PeakHoursWindow, validating the HH:MM
// values via PeakHoursWindow.Validate. An empty value or the literal
// "off" returns nil (feature off). Returns an error for malformed input.
func parsePeakHoursWindow(raw string) (*config.PeakHoursWindow, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "off") {
		return nil, nil
	}
	parts := strings.SplitN(raw, "-", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid peak-hours %q: expected HH:MM-HH:MM", raw)
	}
	w := &config.PeakHoursWindow{
		Start: strings.TrimSpace(parts[0]),
		End:   strings.TrimSpace(parts[1]),
	}
	if err := w.Validate(); err != nil {
		return nil, err
	}
	return w, nil
}

var providersCmd = &cobra.Command{
	Use:   "providers",
	Short: "Inspect and edit LLM-provider configuration",
	Long: `Manage the provider entries rush will use for chat completions.

Provider config lives under "providers.<id>" in rush.json. Two scopes
exist and rush merges them at load time, workspace overriding global:

  --global   ~/.local/share/rush/rush.json   (or %LocalAppData%\rush on Windows)
  --local    ./.rush/rush.json               (next to the project)

If --global / --local is omitted the default is --global for write
operations and "both" for read operations.

Each provider entry holds: api_key, base_url, type, name, disable, and
an optional peak_hours refusal window (local-clock HH:MM-HH:MM; see
'rush providers set --help').`,
}

var providersListCmd = &cobra.Command{
	Use:   "list",
	Args:  cobra.NoArgs,
	Short: "List configured providers across both scopes",
	Long: `Print the merged effective view of providers (workspace overriding
global). Use --json for one NDJSON object per provider. API keys are
always masked — only the last 4 chars are shown.`,
	Example: `
rush providers list
rush providers list --json | jq 'select(.api_key_present)'
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		providers := a.Config().Providers.Copy()
		// Stable order.
		ids := make([]string, 0, len(providers))
		for id := range providers {
			ids = append(ids, id)
		}
		sort.Strings(ids)

		grepPattern, _ := cmd.Flags().GetString("grep")
		grepLower := strings.ToLower(grepPattern)

		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			for _, id := range ids {
				p := providers[id]
				if grepPattern != "" {
					if !matchesGrep(id, p, grepLower) {
						continue
					}
				}
				if err := enc.Encode(makeProviderListItem(id, p)); err != nil {
					return err
				}
			}
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tTYPE\tSTATUS\tMODELS\tAPI_KEY\tBASE_URL\tPEAK")
		for _, id := range ids {
			p := providers[id]
			if grepPattern != "" {
				if !matchesGrep(id, p, grepLower) {
					continue
				}
			}
			status := "enabled"
			if p.Disable {
				status = "disabled"
			}
			modelCount := "—"
			if len(p.Models) > 0 {
				modelCount = fmt.Sprintf("%d", len(p.Models))
			}
			peak := "—"
			if p.PeakHours != nil {
				peak = p.PeakHours.Start + "-" + p.PeakHours.End
				if p.PeakHours.InPeakHours(time.Now()) {
					peak += " *"
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				id,
				dash(p.Name),
				dash(string(p.Type)),
				status,
				modelCount,
				maskKey(p.APIKey),
				dash(p.BaseURL),
				peak,
			)
		}
		return tw.Flush()
	},
}

var providersShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Print a provider's effective config (api_key masked)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		p, ok := a.Config().Providers.Get(args[0])
		if !ok {
			return fmt.Errorf("provider %q not configured", args[0])
		}
		item := makeProviderListItem(args[0], p)
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(item)
		}
		status := "enabled"
		if item.Disabled {
			status = "disabled"
		}
		fmt.Fprintf(os.Stdout, "id:          %s\n", item.ID)
		fmt.Fprintf(os.Stdout, "name:        %s\n", dash(item.Name))
		fmt.Fprintf(os.Stdout, "type:        %s\n", dash(item.Type))
		fmt.Fprintf(os.Stdout, "status:      %s\n", status)
		fmt.Fprintf(os.Stdout, "api_key:     %s (present: %t)\n", item.APIKey, item.APIKeyPresent)
		fmt.Fprintf(os.Stdout, "base_url:    %s\n", dash(item.BaseURL))
		fmt.Fprintf(os.Stdout, "models:      %d\n", item.Models)
		fmt.Fprintf(os.Stdout, "oauth:       %t\n", item.HasOAuth)
		if p.PeakHours != nil {
			state := "not in peak"
			if p.PeakHours.InPeakHours(time.Now()) {
				state = "in peak"
			}
			fmt.Fprintf(os.Stdout, "peak hours:  %s-%s (currently: %s)\n", p.PeakHours.Start, p.PeakHours.End, state)
			if p.PeakHours.Message != "" {
				// Show the configured refusal message (set via --peak-hours-message).
				fmt.Fprintf(os.Stdout, "peak hours message: %s\n", p.PeakHours.Message)
			}
		}
		return nil
	},
}

var providersSetCmd = &cobra.Command{
	Use:   "set <id>",
	Short: "Write provider fields to the chosen scope",
	Long: `Set one or more provider fields in --global (default) or --local
scope. Only the flags you pass are written — unset fields are left
untouched, so you can update just an API key without erasing the
base URL.

Pass --disabled=true to disable a provider without losing its
credentials; --disabled=false to re-enable.

--peak-hours sets the refusal window (HH:MM-HH:MM, local time);
--peak-hours-message sets the custom text shown when a request is
refused inside that window. A time-only --peak-hours update preserves
the existing message; --peak-hours-message "" clears the message and
keeps the window; --peak-hours off clears both. The message requires
a window — set --peak-hours first.`,
	Args: cobra.ExactArgs(1),
	Example: `
# Set api key in global config (default scope)
rush providers set openai --api-key=$OPENAI_API_KEY

# Pin a custom base URL just for this workspace
rush providers set openai --local --base-url=http://localhost:11434/v1

# Disable a provider without removing it
rush providers set hyper --disabled=true

# Refuse to run during business hours (local time); clear with "off"
rush providers set openai --peak-hours 09:00-18:00

# Customise the peak-hours refusal text (workspace scope only)
rush providers set openai --local --peak-hours-message "Ping #ops-oncall before retrying"

# Set window and message together
rush providers set openai --peak-hours 09:00-18:00 --peak-hours-message "Low quota until 18:00"
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		id := args[0]
		updates := map[string]any{}
		if cmd.Flags().Changed("api-key") {
			v, _ := cmd.Flags().GetString("api-key")
			updates["providers."+escapeSJSONPathComponent(id)+".api_key"] = v
		}
		if cmd.Flags().Changed("base-url") {
			v, _ := cmd.Flags().GetString("base-url")
			updates["providers."+escapeSJSONPathComponent(id)+".base_url"] = v
		}
		if cmd.Flags().Changed("type") {
			v, _ := cmd.Flags().GetString("type")
			updates["providers."+escapeSJSONPathComponent(id)+".type"] = v
		}
		if cmd.Flags().Changed("name") {
			v, _ := cmd.Flags().GetString("name")
			updates["providers."+escapeSJSONPathComponent(id)+".name"] = v
		}
		if cmd.Flags().Changed("disabled") {
			v, _ := cmd.Flags().GetBool("disabled")
			updates["providers."+escapeSJSONPathComponent(id)+".disable"] = v
		}
		clearPeakHours := false
		var peakHoursWindow *config.PeakHoursWindow
		peakHoursChanged := cmd.Flags().Changed("peak-hours")
		if peakHoursChanged {
			v, _ := cmd.Flags().GetString("peak-hours")
			w, err := parsePeakHoursWindow(v)
			if err != nil {
				return fmt.Errorf("invalid --peak-hours: %w", err)
			}
			if w == nil {
				clearPeakHours = true
			} else {
				peakHoursWindow = w
			}
		}
		messageChanged := cmd.Flags().Changed("peak-hours-message")
		peakHoursMessage := ""
		if messageChanged {
			peakHoursMessage, _ = cmd.Flags().GetString("peak-hours-message")
		}
		if len(updates) == 0 && !peakHoursChanged && !messageChanged {
			return fmt.Errorf("no fields to set — pass at least one of --api-key/--base-url/--type/--name/--disabled/--peak-hours/--peak-hours-message")
		}

		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		if peakHoursWindow != nil || (messageChanged && !clearPeakHours) {
			// The merged provider view this command already uses for
			// every other field (workspace overrides global).
			var existing *config.PeakHoursWindow
			if p, ok := a.Store().Config().Providers.Get(id); ok && p.PeakHours != nil {
				w := *p.PeakHours
				existing = &w
			}
			if peakHoursWindow != nil {
				// --peak-hours alone only carries HH:MM-HH:MM: preserve
				// the existing message; --peak-hours-message overrides it.
				if messageChanged {
					peakHoursWindow.Message = peakHoursMessage
				} else if existing != nil {
					peakHoursWindow.Message = existing.Message
				}
			} else {
				// Message-only update: rewrite the effective window with
				// the new message so the target scope keeps a complete,
				// valid peak_hours object, never a bare message fragment.
				if existing == nil {
					return fmt.Errorf("provider %q has no peak-hours window — set one first with `rush providers set %s --peak-hours HH:MM-HH:MM`", id, id)
				}
				peakHoursWindow = existing
				peakHoursWindow.Message = peakHoursMessage
			}
			updates["providers."+escapeSJSONPathComponent(id)+".peak_hours"] = peakHoursWindow
		}

		if clearPeakHours {
			if err := a.Store().RemoveConfigField(scope, "providers."+escapeSJSONPathComponent(id)+".peak_hours"); err != nil {
				return fmt.Errorf("failed to clear peak_hours: %w", err)
			}
			fmt.Fprintf(os.Stderr, "cleared peak_hours for provider %q in %s scope\n", id, scope)
		}
		if len(updates) > 0 {
			if err := a.Store().SetConfigFields(scope, updates); err != nil {
				return fmt.Errorf("failed to write provider config: %w", err)
			}
			fmt.Fprintf(os.Stderr, "wrote %d field(s) to %s scope for provider %q\n", len(updates), scope, id)
		}
		return nil
	},
}

var providersAddCmd = &cobra.Command{
	Use:   "add <id>",
	Short: "Add a new provider",
	Long: `Add a new provider to the chosen scope (default: global). Specify
the provider type, name, and optionally a base URL, API key,
peak-hours window, and peak-hours refusal message.`,
	Args: cobra.ExactArgs(1),
	Example: `
# Add a catwalk-known provider (Z.AI)
rush providers add zai --name "Z.AI" --type openai-compat --base-url https://api.z.ai --api-key $ZAI_API_KEY

# Add a custom OpenAI-compatible provider
rush providers add local-llm --name "Local LLM" --type openai-compat --base-url http://localhost:8000/v1 --api-key none

# Add with a peak-hours window and custom refusal message
rush providers add zai --name "Z.AI" --type openai-compat --peak-hours 09:00-18:00 --peak-hours-message "Low quota until 18:00"`,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]
		name, _ := cmd.Flags().GetString("name")
		typeStr, _ := cmd.Flags().GetString("type")
		baseURL, _ := cmd.Flags().GetString("base-url")
		apiKey, _ := cmd.Flags().GetString("api-key")
		enable, _ := cmd.Flags().GetBool("enable")

		var peakHours *config.PeakHoursWindow
		if cmd.Flags().Changed("peak-hours") {
			phRaw, _ := cmd.Flags().GetString("peak-hours")
			ph, err := parsePeakHoursWindow(phRaw)
			if err != nil {
				return fmt.Errorf("invalid --peak-hours: %w", err)
			}
			peakHours = ph
		}

		peakHoursMessage := ""
		messageSet := cmd.Flags().Changed("peak-hours-message")
		if messageSet {
			peakHoursMessage, _ = cmd.Flags().GetString("peak-hours-message")
			if peakHours == nil {
				// A refusal message is only shown inside a window; absent
				// --peak-hours (or "off") means there is none.
				return fmt.Errorf("--peak-hours-message requires --peak-hours HH:MM-HH:MM")
			}
			peakHours.Message = peakHoursMessage
		}

		if name == "" {
			return fmt.Errorf("--name is required")
		}
		if typeStr == "" {
			return fmt.Errorf("--type is required")
		}

		// Check if provider already exists
		if _, exists := a.Config().Providers.Get(id); exists {
			return fmt.Errorf("provider %q already exists", id)
		}

		provType := catwalk.Type(typeStr)

		if provType == "cli" {
			return fmt.Errorf("adding new CLI providers requires editing internal/agent/cliprovider/provider.go directly")
		}

		// Validate provider type
		validTypes := catwalk.KnownProviderTypes()
		validTypes = append(validTypes, "openai-compat")
		isValid := false
		for _, t := range validTypes {
			if t == provType {
				isValid = true
				break
			}
		}
		if !isValid {
			fmt.Fprintf(os.Stderr, "Supported provider types: %v\n", validTypes)
			return fmt.Errorf("unsupported provider type %q", typeStr)
		}

		// If base-url not provided for catwalk-known, try to get from catwalk
		if baseURL == "" {
			for _, known := range a.Store().KnownProviders() {
				if known.ID == catwalk.InferenceProvider(id) {
					baseURL = known.APIEndpoint
					break
				}
			}
		}

		pc := config.ProviderConfig{
			ID:      id,
			Name:    name,
			Type:    provType,
			BaseURL: baseURL,
			APIKey:  apiKey,
			Disable: !enable,
		}

		fields := map[string]any{
			"providers." + escapeSJSONPathComponent(id) + ".name":    name,
			"providers." + escapeSJSONPathComponent(id) + ".type":    typeStr,
			"providers." + escapeSJSONPathComponent(id) + ".disable": pc.Disable,
		}
		if baseURL != "" {
			fields["providers."+escapeSJSONPathComponent(id)+".base_url"] = baseURL
		}
		if apiKey != "" {
			fields["providers."+escapeSJSONPathComponent(id)+".api_key"] = apiKey
		}
		if peakHours != nil {
			fields["providers."+escapeSJSONPathComponent(id)+".peak_hours"] = peakHours
		}

		if err := a.Store().SetConfigFields(scope, fields); err != nil {
			return fmt.Errorf("failed to add provider: %w", err)
		}

		fmt.Fprintf(os.Stderr, "✓ %s created\n", id)

		// Test connection if API key provided
		if apiKey != "" && !strings.HasPrefix(apiKey, "$") {
			if err := pc.TestConnection(a.Store().Resolver()); err != nil {
				fmt.Fprintf(os.Stderr, "✗ connection failed: %v (but provider saved; fix and re-enable)\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "✓ connection verified\n")
			}
		}
		// Fetch models if enabled
		if enable {
			if err := updateSingleProvider(a, id, scope); err != nil {
				return fmt.Errorf("provider created, but model refresh failed: %w", err)
			}
		}

		return nil
	},
}

var providersUpdateCmd = &cobra.Command{
	Use:   "update [<id> | --all]",
	Short: "Refresh provider models from the API",
	Long: `Fetch the latest model list from a provider's API and update the local
configuration. Shows a summary of added/removed models.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		all, _ := cmd.Flags().GetBool("all")

		if all && len(args) > 0 {
			return fmt.Errorf("--all and <id> are mutually exclusive")
		}

		if !all && len(args) == 0 {
			return fmt.Errorf("either specify <id> or use --all")
		}

		if all {
			return updateAllProviders(a)
		}

		return updateSingleProvider(a, args[0], config.ScopeGlobal)
	},
}

func updateSingleProvider(a *app.App, id string, scopes ...config.Scope) error {
	writeScope := config.ScopeGlobal
	if len(scopes) > 0 {
		writeScope = scopes[0]
	}
	p, ok := a.Config().Providers.Get(id)
	if !ok {
		return fmt.Errorf("provider %q not found", id)
	}

	oldModels := p.Models
	newModels, warnings, err := fetchModels(a, p)
	if err != nil {
		return err
	}

	added, removed := computeDiff(oldModels, newModels)

	oldCount := len(oldModels)
	newCount := len(newModels)

	var diffStr strings.Builder
	if newCount > oldCount {
		fmt.Fprintf(&diffStr, " (+%d", newCount-oldCount)
		if len(added) > 0 && len(added) <= 3 {
			diffStr.WriteString(": ")
			for i, m := range added {
				if i > 0 {
					diffStr.WriteString(", ")
				}
				diffStr.WriteString(m.ID)
			}
		}
		diffStr.WriteString(")")
	} else if newCount < oldCount {
		fmt.Fprintf(&diffStr, " (-%d", oldCount-newCount)
		if len(removed) > 0 && len(removed) <= 3 {
			diffStr.WriteString(": ")
			for i, m := range removed {
				if i > 0 {
					diffStr.WriteString(", ")
				}
				diffStr.WriteString(m.ID)
			}
		}
		diffStr.WriteString(")")
	}

	fmt.Fprintf(os.Stderr, "%s: %d → %d models%s\n", id, oldCount, newCount, diffStr.String())

	// Check for orphaned preferred slots
	cfg := a.Config()
	for modelType, model := range cfg.Models {
		if model.Provider != id {
			continue
		}
		for _, rm := range removed {
			if rm.ID == model.Model {
				slotName := "smart"
				if modelType == config.SelectedModelTypeFast {
					slotName = "fast"
				}
				fmt.Fprintf(os.Stderr, "WARN: preferred %s = %s/%s no longer exists after update — your '%s' slot is broken. Run `rush models use <smart> <fast>` to fix.\n", slotName, id, model.Model, slotName)
			}
		}
	}

	// Save updated models to config
	modelsJSON, _ := json.Marshal(newModels)
	updates := map[string]any{
		"providers." + escapeSJSONPathComponent(id) + ".models": json.RawMessage(modelsJSON),
	}
	if err := a.Store().SetConfigFields(writeScope, updates); err != nil {
		return fmt.Errorf("failed to save updated models: %w", err)
	}
	// Print warnings
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "note: %s\n", w)
	}

	return nil
}

func updateAllProviders(a *app.App) error {
	providers := a.Config().EnabledProviders()
	count := 0
	var updateErrors []error
	for _, p := range providers {
		if err := updateSingleProvider(a, p.ID, config.ScopeGlobal); err != nil {
			fmt.Fprintf(os.Stderr, "error updating %s: %v\n", p.ID, err)
			updateErrors = append(updateErrors, fmt.Errorf("%s: %w", p.ID, err))
			continue
		}
		count++
	}
	fmt.Fprintf(os.Stderr, "Updated %d provider(s)\n", count)
	if len(updateErrors) > 0 {
		return fmt.Errorf("failed to update %d provider(s): %w", len(updateErrors), errors.Join(updateErrors...))
	}
	return nil
}

var providersEnableCmd = &cobra.Command{
	Use:   "enable <id>",
	Short: "Enable a provider and refresh its model list",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]
		p, ok := a.Config().Providers.Get(id)
		if !ok {
			return fmt.Errorf("provider %q not found, see `rush providers list`", id)
		}

		if !p.Disable {
			fmt.Fprintf(os.Stderr, "provider %q is already enabled\n", id)
			return nil
		}

		if err := a.Store().SetConfigField(scope, "providers."+escapeSJSONPathComponent(id)+".disable", false); err != nil {
			return fmt.Errorf("failed to enable provider: %w", err)
		}

		fmt.Fprintf(os.Stderr, "✓ %s enabled\n", id)
		return nil
	},
}

var providersDisableCmd = &cobra.Command{
	Use:   "disable <id>",
	Short: "Disable a provider",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]
		p, ok := a.Config().Providers.Get(id)
		if !ok {
			return fmt.Errorf("provider %q not found, see `rush providers list`", id)
		}

		if p.Disable {
			fmt.Fprintf(os.Stderr, "provider %q is already disabled\n", id)
			return nil
		}

		if err := a.Store().SetConfigField(scope, "providers."+escapeSJSONPathComponent(id)+".disable", true); err != nil {
			return fmt.Errorf("failed to disable provider: %w", err)
		}

		// Check if preferred slots use this provider
		cfg := a.Config()
		for modelType, model := range cfg.Models {
			if model.Provider == id {
				slotName := "smart"
				if modelType == config.SelectedModelTypeFast {
					slotName = "fast"
				}
				fmt.Fprintf(os.Stderr, "warning: %s slot was using %s/%s; that slot is now broken. Run `rush models use <smart> <fast>` to fix.\n", slotName, id, model.Model)
			}
		}

		fmt.Fprintf(os.Stderr, "%s disabled\n", id)
		return nil
	},
}

var providersUnsetCmd = &cobra.Command{
	Use:     "unset <id>",
	Aliases: []string{"remove", "rm"},
	Short:   "Remove a provider entry from the chosen scope",
	Long: `Delete the providers.<id> object from the targeted config file. The
provider may still appear in "providers list" if it is also defined in
the other scope (workspace fallback to global, or vice versa) — run
unset with the matching --global / --local to fully clear it. For a
default/catwalk-known provider (e.g. "anthropic", "openai") unset only
clears any override; the provider reappears from the built-in catalog on
the next config load — use "rush providers disable <id>" to
persistently suppress it.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]

		// Check if preferred slots use this provider and warn
		cfg := a.Config()
		for modelType, model := range cfg.Models {
			if model.Provider == id {
				slotName := "smart"
				if modelType == config.SelectedModelTypeFast {
					slotName = "fast"
				}
				fmt.Fprintf(os.Stderr, "warning: %s slot was using %s/%s; that slot is now broken. Run `rush models use <smart> <fast>` to fix.\n", slotName, id, model.Model)
			}
		}

		if err := a.Store().RemoveConfigField(scope, "providers."+escapeSJSONPathComponent(id)); err != nil {
			return fmt.Errorf("failed to remove provider from %s scope: %w", scope, err)
		}
		fmt.Fprintf(os.Stderr, "removed provider %q from %s scope\n", id, scope)
		return nil
	},
}

var providersFetchModelsCmd = &cobra.Command{
	Use:   "fetch-models <id>",
	Short: "Query the provider's API live and list available models (read-only)",
	Long: `Hit the provider's models endpoint (GET <base_url>/models for
openai-compat, GET /v1/models for anthropic) using the resolved API
key and print whatever the server actually returns. Unlike
'rush providers update', this does NOT write anything to rush.json —
use it to discover newly-released models before deciding whether to
update the local cache.

Superseded model families are hidden from the printed list, the same
filter the other model listings use: openai-codex shows gpt-6 and
newer, and glm models below 5.3 are hidden for every provider.`,
	Example: `
# Inspect z.ai's live model list (looks for newly-released GLM versions)
rush providers fetch-models zai

# JSON for scripts
rush providers fetch-models openai --json

# Diff against locally-cached models
rush providers fetch-models zai --diff
  `,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		showDiff, _ := cmd.Flags().GetBool("diff")

		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]
		p, ok := a.Config().Providers.Get(id)
		if !ok {
			return fmt.Errorf("provider %q not found", id)
		}

		models, warnings, err := fetchModels(a, p)
		if err != nil {
			return fmt.Errorf("fetch-models %s: %w", id, err)
		}

		if asJSON {
			out := struct {
				Provider string          `json:"provider"`
				Count    int             `json:"count"`
				Models   []catwalk.Model `json:"models"`
				Warnings []string        `json:"warnings,omitempty"`
			}{Provider: id, Count: len(models), Models: models, Warnings: warnings}
			return json.NewEncoder(os.Stdout).Encode(out)
		}

		fmt.Fprintf(os.Stdout, "provider:  %s (%s)\n", id, dash(string(p.Type)))
		fmt.Fprintf(os.Stdout, "endpoint:  %s\n", dash(p.BaseURL))
		fmt.Fprintf(os.Stdout, "models:    %d returned by server\n\n", len(models))

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tCONTEXT\tNAME")
		for _, m := range models {
			ctx := "—"
			if m.ContextWindow > 0 {
				ctx = fmt.Sprintf("%d", m.ContextWindow)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", m.ID, ctx, dash(m.Name))
		}
		if err := tw.Flush(); err != nil {
			return err
		}

		if showDiff {
			added, removed := computeDiff(p.Models, models)
			fmt.Fprintln(os.Stdout)
			fmt.Fprintf(os.Stdout, "diff vs cached (%d locally):\n", len(p.Models))
			if len(added) == 0 && len(removed) == 0 {
				fmt.Fprintln(os.Stdout, "  no changes")
			}
			for _, m := range added {
				fmt.Fprintf(os.Stdout, "  + %s\n", m.ID)
			}
			for _, m := range removed {
				fmt.Fprintf(os.Stdout, "  - %s\n", m.ID)
			}
		}

		for _, w := range warnings {
			fmt.Fprintf(os.Stderr, "note: %s\n", w)
		}
		return nil
	},
}

var providersGrepCmd = &cobra.Command{
	Use:   "grep <pattern>",
	Short: "Filter providers by id, name, or type (sugar for `list --grep <pattern>`)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()
		needle := strings.ToLower(args[0])
		providers := a.Config().Providers.Copy()
		ids := make([]string, 0, len(providers))
		for id := range providers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID	NAME	TYPE	STATUS	MODELS	API_KEY	BASE_URL")
		matched := 0
		for _, id := range ids {
			p := providers[id]
			if !matchesGrep(id, p, needle) {
				continue
			}
			status := "enabled"
			if p.Disable {
				status = "disabled"
			}
			modelCount := "—"
			if len(p.Models) > 0 {
				modelCount = fmt.Sprintf("%d", len(p.Models))
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", id, dash(p.Name), dash(string(p.Type)), status, modelCount, maskKey(p.APIKey), dash(p.BaseURL))
			matched++
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		if matched == 0 {
			fmt.Fprintf(os.Stderr, "no providers matched %q\n", args[0])
		}
		return nil
	},
}

func init() {
	providersListCmd.Flags().Bool("json", false, "Emit one JSON object per line instead of a table")
	providersListCmd.Flags().String("grep", "", "Filter providers by id, name, or type (case-insensitive substring match)")
	providersShowCmd.Flags().Bool("json", false, "Emit a JSON object instead of human-readable lines")

	for _, c := range []*cobra.Command{providersSetCmd, providersUnsetCmd} {
		c.Flags().Bool("global", false, "Target the global config (~/.local/share/rush/rush.json). Default when neither --global nor --local is given.")
		c.Flags().Bool("local", false, "Target the workspace config (./.rush/rush.json).")
		c.MarkFlagsMutuallyExclusive("global", "local")
	}
	providersSetCmd.Flags().String("api-key", "", "API key for the provider (literal or $VAR — the latter expands at config-load time)")
	providersSetCmd.Flags().String("base-url", "", "Base URL for the provider's API")
	providersSetCmd.Flags().String("type", "", "Provider type: openai|openai-compat|anthropic|gemini|azure|vertexai")
	providersSetCmd.Flags().String("name", "", "Human-readable display name shown in the WUI")
	providersSetCmd.Flags().Bool("disabled", false, "Mark provider as disabled (kept in config, ignored at runtime)")
	providersSetCmd.Flags().String("peak-hours", "", "Peak-hours window as HH:MM-HH:MM (local time). Pass 'off' or empty to clear.")
	providersSetCmd.Flags().String("peak-hours-message", "", "Custom text shown when a request is refused during the peak-hours window. Empty string clears it. Requires an existing --peak-hours window.")

	for _, c := range []*cobra.Command{providersEnableCmd, providersDisableCmd, providersAddCmd} {
		c.Flags().Bool("global", false, "Target the global config (~/.local/share/rush/rush.json). Default when neither --global nor --local is given.")
		c.Flags().Bool("local", false, "Target the workspace config (./.rush/rush.json).")
		c.MarkFlagsMutuallyExclusive("global", "local")
	}

	providersAddCmd.Flags().String("name", "", "Human-readable name for the provider (required)")
	providersAddCmd.Flags().String("type", "", "Provider type: openai|openai-compat|anthropic|gemini|azure|vertexai|bedrock|xai|zai|groq|openrouter|synthetic|huggingface|copilot|vercel (required)")
	providersAddCmd.Flags().String("base-url", "", "Base URL for the provider's API (optional for catwalk-known providers)")
	providersAddCmd.Flags().String("api-key", "", "API key for the provider (optional, can be set later)")
	providersAddCmd.Flags().Bool("enable", true, "Enable the provider after creation (default: true)")
	providersAddCmd.Flags().String("peak-hours", "", "Optional peak-hours window as HH:MM-HH:MM (local time). Provider is refused during this window.")
	providersAddCmd.Flags().String("peak-hours-message", "", "Optional custom text shown when a request is refused during the peak-hours window. Requires --peak-hours.")

	providersUpdateCmd.Flags().Bool("all", false, "Update all enabled providers")

	providersFetchModelsCmd.Flags().Bool("json", false, "Emit a JSON object instead of a table")
	providersFetchModelsCmd.Flags().Bool("diff", false, "Also show added/removed vs the locally-cached model list")

	providersCmd.AddCommand(providersListCmd, providersShowCmd, providersSetCmd, providersAddCmd, providersEnableCmd, providersDisableCmd, providersUnsetCmd, providersUpdateCmd, providersGrepCmd, providersFetchModelsCmd)
	rootCmd.AddCommand(providersCmd)
}

// providerListItem is the JSON shape of providers list / show. Kept
// separate from config.ProviderConfig so api_key is never serialised as
// plaintext by accident.
type providerListItem struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	Type          string `json:"type,omitempty"`
	APIKey        string `json:"api_key"` // masked
	APIKeyPresent bool   `json:"api_key_present"`
	BaseURL       string `json:"base_url,omitempty"`
	Disabled      bool   `json:"disabled"`
	Models        int    `json:"models"`
	HasOAuth      bool   `json:"oauth"`
}

func makeProviderListItem(id string, p config.ProviderConfig) providerListItem {
	return providerListItem{
		ID:            id,
		Name:          p.Name,
		Type:          string(p.Type),
		APIKey:        maskKey(p.APIKey),
		APIKeyPresent: p.APIKey != "",
		BaseURL:       p.BaseURL,
		Disabled:      p.Disable,
		Models:        len(p.Models),
		HasOAuth:      p.OAuthToken != nil,
	}
}
