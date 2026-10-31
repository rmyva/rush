package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

var providersPatchCmd = &cobra.Command{
	Use:   "patch <id>",
	Short: "Shallow-merge a JSON object into a provider's config",
	Long: `Apply a partial provider config by JSON, which lets non-flag fields
(extra_headers, extra_body, provider_options, etc.) be set from the
CLI without opening an editor.

The JSON object you pass is a partial config.ProviderConfig — the
fields you include are written, fields you omit are left alone.
Top-level keys are merged shallowly into the existing providers.<id>
object in the chosen scope; nested values are replaced as a whole
(merging maps shallowly works for the common case of "add one header",
"swap extra_body").

JSON source priority: --json (literal) > --json-file > stdin.

If the provider doesn't exist yet it is created with the supplied
fields.`,
	Args: cobra.ExactArgs(1),
	Example: `
# Add an extra header to openai globally
rush providers patch openai --json '{"extra_headers":{"OpenAI-Organization":"org-XXX"}}'

# Swap extra_body from a file (workspace scope)
rush providers patch openai --local --json-file ./openai-extra-body.json

# Pipe JSON in
echo '{"disable": true}' | rush providers patch hyper
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}

		raw, err := readPatchJSON(cmd)
		if err != nil {
			return err
		}
		// Validate shape: must be an object that unmarshals into a
		// partial ProviderConfig. We don't apply the validated struct —
		// we use the raw object so unknown-to-Go fields a future schema
		// adds don't get dropped silently. The unmarshal is a tripwire,
		// not a transform.
		var typed config.ProviderConfig
		if err := json.Unmarshal(raw, &typed); err != nil {
			return fmt.Errorf("--json is not a valid ProviderConfig fragment: %w", err)
		}

		// Unmarshal into a map so we can iterate top-level keys for the
		// shallow merge into "providers.<id>.<key>".
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			return fmt.Errorf("--json must be a JSON object: %w", err)
		}
		if len(fields) == 0 {
			return fmt.Errorf("--json: empty object — nothing to patch")
		}

		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		updates := make(map[string]any, len(fields))
		for k, v := range fields {
			updates["providers."+escapeSJSONPathComponent(args[0])+"."+escapeSJSONPathComponent(k)] = v
		}
		if err := a.Store().SetConfigFields(scope, updates); err != nil {
			return fmt.Errorf("failed to write patch: %w", err)
		}
		// Show keys we wrote so the caller can audit.
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		fmt.Fprintf(os.Stderr, "patched %d field(s) [%s] on provider %q in %s scope\n", len(keys), joinComma(keys), args[0], scope)
		return nil
	},
}

func init() {
	providersPatchCmd.Flags().String("json", "", "Literal JSON object to merge into providers.<id>")
	providersPatchCmd.Flags().String("json-file", "", "Path to a file containing the JSON object")
	providersPatchCmd.Flags().Bool("global", false, "Target the global config (default)")
	providersPatchCmd.Flags().Bool("local", false, "Target the workspace config")
	providersPatchCmd.MarkFlagsMutuallyExclusive("global", "local")
	providersPatchCmd.MarkFlagsMutuallyExclusive("json", "json-file")
	providersCmd.AddCommand(providersPatchCmd)
}

// readPatchJSON returns the JSON bytes from --json, --json-file, or stdin.
func readPatchJSON(cmd *cobra.Command) ([]byte, error) {
	if literal, _ := cmd.Flags().GetString("json"); literal != "" {
		return []byte(literal), nil
	}
	if path, _ := cmd.Flags().GetString("json-file"); path != "" {
		return os.ReadFile(path)
	}
	// Fall back to stdin if anything was piped in.
	fi, _ := os.Stdin.Stat()
	if fi != nil && (fi.Mode()&os.ModeNamedPipe != 0 || fi.Mode().IsRegular()) {
		return io.ReadAll(os.Stdin)
	}
	return nil, fmt.Errorf("no JSON source: pass --json, --json-file, or pipe JSON on stdin")
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

func scopeFromFlags(cmd *cobra.Command, def config.Scope) (config.Scope, error) {
	global, _ := cmd.Flags().GetBool("global")
	local, _ := cmd.Flags().GetBool("local")
	switch {
	case global && local:
		return def, fmt.Errorf("--global and --local are mutually exclusive")
	case global:
		return config.ScopeGlobal, nil
	case local:
		return config.ScopeWorkspace, nil
	default:
		return def, nil
	}
}

func maskKey(k string) string {
	if k == "" {
		return "-"
	}
	if strings.HasPrefix(k, "$") {
		// Env-template still unresolved — show the template literally so
		// it's obvious where the key comes from.
		return k
	}
	if len(k) <= 4 {
		return "****"
	}
	return strings.Repeat("*", 4) + k[len(k)-4:]
}

func matchesGrep(id string, p config.ProviderConfig, patternLower string) bool {
	fields := []string{
		strings.ToLower(id),
		strings.ToLower(p.Name),
		strings.ToLower(string(p.Type)),
	}
	for _, field := range fields {
		if strings.Contains(field, patternLower) {
			return true
		}
	}
	return false
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func escapeSJSONPathComponent(s string) string {
	return strings.NewReplacer(`\`, `\\`, ".", `\.`, "*", `\*`, "?", `\?`, "#", `\#`, "|", `\|`, "@", `\@`).Replace(s)
}
