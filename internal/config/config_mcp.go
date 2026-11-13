package config

// The MCP server definition as it appears in configuration: its types, origin and source kinds, and the variable resolution applied to commands, arguments, URLs and headers. Split out of config.go when the 1000-line file limit landed.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/invopop/jsonschema"
)

type MCPType string

const (
	MCPStdio MCPType = "stdio"
	MCPSSE   MCPType = "sse"
	MCPHttp  MCPType = "http"
)

// MCPSource identifies where an MCP server configuration originated.
type MCPSource string

const (
	// MCPSourceUser is the default: configured in rush.json by the user.
	MCPSourceUser MCPSource = ""
	// MCPSourceExternal means the server was loaded from a .mcp.json file.
	MCPSourceExternal MCPSource = "external"
)

// MCPOriginKind identifies the layer that supplied an effective MCP server.
// The distinction is intentionally retained separately from MCPSource:
// project and system rush.json files are user configuration, but neither is
// a writable MCP scope.
type MCPOriginKind string

const (
	MCPOriginSystem    MCPOriginKind = "system"
	MCPOriginProject   MCPOriginKind = "project"
	MCPOriginGlobal    MCPOriginKind = "global"
	MCPOriginWorkspace MCPOriginKind = "workspace"
	MCPOriginExternal  MCPOriginKind = "external"
)

// MCPOrigin describes the effective source of an MCP definition.
type MCPOrigin struct {
	Kind     MCPOriginKind
	Path     string
	Scope    Scope
	Writable bool
}

type MCPConfig struct {
	Command       string            `json:"command,omitempty" jsonschema:"description=Command to execute for stdio MCP servers,example=npx"`
	Env           map[string]string `json:"env,omitempty" jsonschema:"description=Environment variables to set for the MCP server"`
	Args          []string          `json:"args,omitempty" jsonschema:"description=Arguments to pass to the MCP server command"`
	Type          MCPType           `json:"type" jsonschema:"required,description=Type of MCP connection,enum=stdio,enum=sse,enum=http,default=stdio"`
	URL           string            `json:"url,omitempty" jsonschema:"description=URL for HTTP or SSE MCP servers,format=uri,example=http://localhost:3000/mcp"`
	Disabled      bool              `json:"disabled,omitempty" jsonschema:"description=Whether this MCP server is disabled,default=false"`
	DisabledTools []string          `json:"disabled_tools,omitempty" jsonschema:"description=List of tools from this MCP server to disable,example=get-library-doc"`
	EnabledTools  []string          `json:"enabled_tools,omitempty" jsonschema:"description=Allow list of tools from this MCP server,example=get-library-doc"`
	Timeout       int               `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds for MCP server connections,default=15,example=30,example=60,example=120"`

	// EnabledInCLI opts this server into non-interactive invocations
	// (`rush run` and every other CLI subcommand except the bare `rush`
	// that starts the web UI). Off by default: most MCP servers are
	// useful to a human chatting in the web UI but add nothing to an
	// unattended coding agent, while still paying real startup cost — a
	// stdio server spawns its own child process on every single
	// invocation regardless of whether the task ever calls it (measured:
	// an npx-launched Node MCP server commits ~90-100MB, more than
	// rush's own process). Interactive web/TUI sessions are NEVER
	// affected by this field — they always start every non-disabled
	// server, exactly as before. `rush run --all-mcp` overrides this
	// gate for a single invocation when that run genuinely needs it.
	EnabledInCLI bool `json:"enabled_in_cli,omitempty" jsonschema:"description=Whether this server starts during non-interactive CLI invocations (rush run and other CLI subcommands). Off by default -- interactive web/TUI sessions always start non-disabled servers regardless. Override for one run with --all-mcp.,default=false"`

	// Headers are HTTP headers for HTTP/SSE MCP servers. Values run
	// through shell expansion at MCP startup, so $VAR and $(cmd)
	// work. A header whose value resolves to the empty string (unset
	// bare $VAR under lenient nounset, $(echo), or literal "") is
	// omitted from the outgoing request rather than sent as
	// "Header:".
	Headers map[string]string `json:"headers,omitempty" jsonschema:"description=HTTP headers for HTTP/SSE MCP servers"`

	// Source tracks where this config came from (runtime only, not serialized).
	Source MCPSource `json:"-"`
}

func mcpConfigHasDynamicResolution(m MCPConfig) bool {
	if hasCommandSubstitution(m.Command) || hasCommandSubstitution(m.URL) {
		return true
	}
	for _, value := range m.Env {
		if hasCommandSubstitution(value) {
			return true
		}
	}
	for _, value := range m.Args {
		if hasCommandSubstitution(value) {
			return true
		}
	}
	for _, value := range m.Headers {
		if hasCommandSubstitution(value) {
			return true
		}
	}
	return false
}

func configHasDynamicMCPResolution(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	for _, m := range cfg.MCP {
		if mcpConfigHasDynamicResolution(m) {
			return true
		}
	}
	return false
}

func hasCommandSubstitution(value string) bool {
	return shell.HasCommandSubstitution(value)
}

type TUIOptions struct {
	CompactMode bool   `json:"compact_mode,omitempty" jsonschema:"description=Enable compact mode for the TUI interface,default=false"`
	DiffMode    string `json:"diff_mode,omitempty" jsonschema:"description=Diff mode for the TUI interface,enum=unified,enum=split"`
	// Theme overrides automatic terminal background detection.
	// Valid values: "dark", "light", or "" (auto-detect).
	Theme string `json:"theme,omitempty" jsonschema:"description=Color theme override,enum=dark,enum=light"`

	Completions Completions `json:"completions,omitzero" jsonschema:"description=Completions UI options"`
	Transparent *bool       `json:"transparent,omitempty" jsonschema:"description=Enable transparent background for the TUI interface,default=false"`
}

// Completions defines options for the completions UI.
type Completions struct {
	MaxDepth *int `json:"max_depth,omitempty" jsonschema:"description=Maximum depth for the ls tool,default=0,example=10"`
	MaxItems *int `json:"max_items,omitempty" jsonschema:"description=Maximum number of items to return for the ls tool,default=1000,example=100"`
}

func (c Completions) Limits() (depth, items int) {
	return ptrValOr(c.MaxDepth, 0), ptrValOr(c.MaxItems, 0)
}

type Permissions struct {
	AllowedTools []string `json:"allowed_tools,omitempty" jsonschema:"description=List of tools that don't require permission prompts,example=bash,example=view"`
	// Run configures the permission model for non-interactive
	// `rush run` invocations. When nil (the default), `rush run` keeps
	// its current behaviour: every permission request is auto-approved
	// because no human is on the keyboard. When Run.Restrict is true,
	// auto-approval flips to deny-by-default and only the requests that
	// match the allowlists below are approved. Interactive sessions
	// (TUI / web) are never affected by this setting.
	Run          *RunPermissions `json:"run,omitempty" jsonschema:"description=Restricted permission model for non-interactive 'rush run'. Off by default; opt in with run.restrict=true."`
	SkipRequests bool            `json:"-"`
}

// RunPermissions configures the restricted permission model for
// `rush run`. Short of wrapping the run in an OS-level sandbox, this is
// the way to scope what an unattended non-interactive run may do.
//
// Interaction with the global skip / YOLO override: enabling skip
// (permissions.skip / --yolo / SetSkipRequests) approves EVERY request
// before the restricted-run gate is even consulted, so skip and
// restrict are mutually exclusive intents and skip wins. Don't combine
// them: if you want a scoped run, leave skip off and use restrict; if
// you want a wide-open run, use skip and don't bother with restrict.
//
// Allowlist semantics (conservative — see internal/permission runallowlist.go):
//   - AllowTools uses the same "tool" / "tool:action" syntax as
//     permissions.allowed_tools and bypasses the run gate for matching
//     NON-BASH tool calls. Entries for "bash" or "bash:execute" are
//     intentionally ignored by the gate — bash is governed solely by
//     AllowBash below, so an operator can't accidentally authorise
//     arbitrary shell commands by listing the tool name. (The global
//     permissions.allowed_tools fast-path still wins over both and is
//     the documented escape hatch for a full bash bypass.)
//   - AllowBash matches the command string of a bash tool call. Each
//     entry is one of:
//     "cmd args"   → prefix match on the command string with a word
//     boundary after the pattern (e.g. "git diff" matches
//     "git diff HEAD~1" but not "git difftool"). Patterns
//     of this form never match a compound command: the
//     command is parsed with the shell grammar, and any
//     chaining (; newline | && ||), backgrounding (&),
//     substitution ($(...) / backticks), or subshell makes
//     it ineligible — so a permissive prefix like "ls"
//     cannot authorise "ls && rm -rf /".
//     "exact:cmd"  → whole-string match (after trimming whitespace).
//     Same compound guard as the prefix form.
//     "glob:pat"   → shell-style glob (only * and ? are special; *
//     matches any characters INCLUDING "/", so matching is
//     the same on every OS). Same compound guard as the
//     prefix form, so a glob cannot authorise a chained
//     command either (e.g. "glob:git *").
//     "regex:pat"  → regexp.MatchString against the raw command string.
//     No compound guard — the full-control escape hatch for
//     operators who need to match a compound command.
type RunPermissions struct {
	// Restrict enables the restricted permission model for `rush run`.
	// When false (the default) `rush run` auto-approves every permission
	// request as before. Must be true for the allowlists below to take
	// effect.
	Restrict bool `json:"restrict,omitempty" jsonschema:"description=Enable restricted permission mode for 'rush run'. When true\\, only allow_tools (non-bash) and allow_bash entries are approved; everything else is denied cleanly. Default false (current auto-approve behaviour).,default=false"`

	// AllowTools lists tool (and optionally tool:action) names that are
	// auto-approved even in restricted run mode. Entries for "bash" /
	// "bash:execute" are ignored — use allow_bash for command-level
	// control of the bash tool.
	AllowTools []string `json:"allow_tools,omitempty" jsonschema:"description=Non-bash tools (or tool:action pairs) auto-approved in restricted run mode. Entries for 'bash' / 'bash:execute' are ignored — use allow_bash for command-level bash control.,example=view,example=edit:write"`

	// AllowBash lists bash command patterns permitted in restricted run
	// mode. See the RunPermissions doc comment for the pattern syntax.
	AllowBash []string `json:"allow_bash,omitempty" jsonschema:"description=Bash command patterns permitted in restricted run mode. Forms: 'cmd args' (word-boundary prefix)\\, 'exact:cmd'\\, 'glob:pat' (only * and ? are special\\, cross-platform)\\, 'regex:pat'. Every form except regex rejects compound commands (chaining\\, backgrounding\\, or substitution)\\, so a permissive pattern cannot smuggle a second command.,example=git diff,example=glob:ls *,example=regex:^go (test|build)"`
}

type TrailerStyle string

const (
	TrailerStyleNone         TrailerStyle = "none"
	TrailerStyleCoAuthoredBy TrailerStyle = "co-authored-by"
	TrailerStyleAssistedBy   TrailerStyle = "assisted-by"
)

type Attribution struct {
	TrailerStyle  TrailerStyle `json:"trailer_style,omitempty" jsonschema:"description=Style of attribution trailer to add to commits,enum=none,enum=co-authored-by,enum=assisted-by,default=assisted-by"`
	CoAuthoredBy  *bool        `json:"co_authored_by,omitempty" jsonschema:"description=Deprecated: use trailer_style instead"`
	GeneratedWith bool         `json:"generated_with,omitempty" jsonschema:"description=Add Generated with Rush line to commit messages and issues and PRs,default=true"`
}

// JSONSchemaExtend marks the co_authored_by field as deprecated in the schema.
func (Attribution) JSONSchemaExtend(schema *jsonschema.Schema) {
	if schema.Properties != nil {
		if prop, ok := schema.Properties.Get("co_authored_by"); ok {
			prop.Deprecated = true
		}
	}
}

type Options struct {
	ContextPaths         []string    `json:"context_paths,omitempty" jsonschema:"description=Paths to files containing context information for the AI,example=.cursorrules,example=RUSH.md"`
	GlobalContextPaths   []string    `json:"global_context_paths,omitempty" jsonschema:"description=Paths to files containing global context information for the AI,default=~/.config/rush/RUSH.md,default=~/.config/AGENTS.md"`
	SkillsPaths          []string    `json:"skills_paths,omitempty" jsonschema:"description=Paths to directories containing Agent Skills (folders with SKILL.md files),example=~/.config/rush/skills,example=./skills"`
	TUI                  *TUIOptions `json:"tui,omitempty" jsonschema:"description=Terminal user interface options"`
	Debug                bool        `json:"debug,omitempty" jsonschema:"description=Enable debug logging,default=false"`
	DisableAutoSummarize bool        `json:"disable_auto_summarize,omitempty" jsonschema:"description=Disable automatic conversation summarization,default=false"`
	// DataDirectory is where Rush keeps per-project state such as
	// the SQLite database and workspace overrides. Relative paths are
	// resolved against the working directory; absolute paths are used
	// verbatim. After defaulting the stored value is always absolute.
	DataDirectory             string       `json:"data_directory,omitempty" jsonschema:"description=Directory for storing application data. Relative paths are resolved against the working directory; absolute paths are used as-is.,default=.rush,example=.rush"`
	DisabledTools             []string     `json:"disabled_tools,omitempty" jsonschema:"description=List of built-in tools to disable and hide from the agent,example=bash,example=sourcegraph"`
	DisableProviderAutoUpdate bool         `json:"disable_provider_auto_update,omitempty" jsonschema:"description=Disable providers auto-update,default=false"`
	DisableDefaultProviders   bool         `json:"disable_default_providers,omitempty" jsonschema:"description=Ignore all default/embedded providers. When enabled\\, providers must be fully specified in the config file with base_url\\, models\\, and api_key - no merging with defaults occurs,default=false"`
	Attribution               *Attribution `json:"attribution,omitempty" jsonschema:"description=Attribution settings for generated content"`
	DisableMetrics            bool         `json:"disable_metrics,omitempty" jsonschema:"description=Disable sending metrics,default=false"`
	InitializeAs              string       `json:"initialize_as,omitempty" jsonschema:"description=Name of the context file to create/update during project initialization,default=AGENTS.md,example=AGENTS.md,example=RUSH.md,example=CLAUDE.md,example=docs/LLMs.md"`
	Progress                  *bool        `json:"progress,omitempty" jsonschema:"description=Show indeterminate progress updates during long operations,default=true"`
	DisableNotifications      bool         `json:"disable_notifications,omitempty" jsonschema:"description=Disable desktop notifications,default=false"`
	DisabledSkills            []string     `json:"disabled_skills,omitempty" jsonschema:"description=List of skill names to disable and hide from the agent,example=rush-config"`
	// AllowPrivateNetworkFetch disables the SSRF guard (see
	// internal/agent/tools/ssrf_guard.go) on download/fetch/agentic_fetch/
	// web_fetch/web_search/sourcegraph, letting the model reach
	// loopback/private/link-local addresses (e.g. a local dev server on
	// 127.0.0.1 or an internal host). Off by default: the guard exists to
	// stop a prompt-injected or malicious model from exfiltrating cloud
	// instance-metadata (169.254.169.254 et al.) through final_text.
	// Enable only in trusted local-dev/self-hosted setups.
	AllowPrivateNetworkFetch bool               `json:"allow_private_network_fetch,omitempty" jsonschema:"description=Allow download/fetch/web tools to reach loopback/private/link-local network addresses. Off by default as an SSRF defense-in-depth measure; enable only for trusted local-dev/self-hosted use.,default=false"`
	ResponsesTransport       ResponsesTransport `json:"responses_transport,omitempty" jsonschema:"description=Transport for direct Codex and public OpenAI Responses,enum=auto,enum=http,default=auto"`
	// Network configures outbound network behavior (proxy and custom
	// DNS/DoH resolution) applied to all provider HTTP connections.
	// Each provider may override any field individually via its own
	// provider.network block.
	Network *NetworkConfig `json:"network,omitempty" jsonschema:"description=Global outbound network settings (proxy\\, custom DNS/DoH) for provider HTTP connections; providers may override per field; a provider proxy replaces the global one (proxy: none disables it for that provider)"`
	// StreamIdleTimeoutSeconds overrides the default 3-minute stream
	// watchdog timeout (see internal/agent/stream_watchdog.go). The
	// watchdog cancels the LLM streaming request if the provider stops
	// sending data for this many seconds — surfaces control as
	// FinishReasonError("Stream stalled") instead of an indefinite hang.
	// Set higher (e.g. 900 = 15 min) when using extended-thinking models
	// (Opus 4.7 / Sonnet 4.5 with large thinking budgets) where the
	// model may legitimately pause for minutes while reasoning. 0 (the
	// default when omitted) keeps the 10-minute built-in value.
	StreamIdleTimeoutSeconds int `json:"stream_idle_timeout_seconds,omitempty" jsonschema:"description=Override the stream watchdog idle timeout in seconds. Default 600 (10 min). Raise further for extreme extended-thinking models that pause longer than 10 min mid-reasoning; lower (e.g. 180) for the old aggressive behaviour. 0 = use default.,default=0,example=900"`
	// StreamToolTimeoutSeconds bounds the stream watchdog's tool-pause
	// (never-freeze backstop). The watchdog pauses its idle timer while a
	// tool executes (between OnToolCall and OnToolResult) so a long
	// `cargo build`/test isn't a false provider stall — but that pause was
	// previously unbounded, freezing the turn on a stuck tool (hung MCP
	// tool, blocking batch_output). Past this cap the watchdog fires
	// with a distinct "tool timeout" reason so the turn ends. 0 (default)
	// keeps the built-in 2700s (45m) backstop; raise it for very long
	// synchronous tools.
	//
	// Keep this number in sync with agent.toolExecutionMaxDefault, which is
	// what actually resolves the 0 case (see effectiveToolMaxDuration). It
	// said 900s/15m here for days after e9544a8f raised the real default to
	// 45m, and the jsonschema description below is user-facing — it reaches
	// generated docs and editor tooltips, so a stale number there is a
	// promise the binary does not keep.
	StreamToolTimeoutSeconds int `json:"stream_tool_timeout_seconds,omitempty" jsonschema:"description=Max seconds a single tool may run while the stream watchdog is paused before it force-cancels the turn (never-freeze backstop). Omit to use the built-in default (2700s = 45m). Raise for very long synchronous tools.,default=0,example=3600"`
	// TurnStallTimeoutSeconds bounds how long a turn may run with no
	// durable progress (tool call/result, step finish, checkpoint
	// persist) before the turn-stall detector's abort policy fires. 0
	// (the default) means "not set"; the 30-minute default is resolved
	// in Go code (see internal/app), not here.
	TurnStallTimeoutSeconds int `json:"turn_stall_timeout_seconds,omitempty" jsonschema:"description=Max seconds a turn may run with no durable progress before the turn-stall abort policy fires. 0 = not set (the 30-minute default applies in Go code).,default=0,example=1800"`
	// StreamStallRetries is the number of times to automatically retry a
	// turn that ended in a transient provider failure (stream stall, empty
	// stream, overload, 5xx, network). Embodies "solve it ourselves before
	// bothering the user": instead of surfacing a transient error on the
	// first occurrence, rush waits with exponential backoff (10s, 30s,
	// 90s) and retries. Operator-actionable failures (quota wall, auth,
	// context overflow, bad request, user cancel) are surfaced immediately
	// regardless of this setting. Pointer so we can distinguish nil ("use
	// built-in default") from 0 ("explicitly disable retries — surface on
	// first occurrence"). Built-in default is 2 retries (3 total attempts).
	StreamStallRetries *int `json:"stream_stall_retries,omitempty" jsonschema:"description=Number of automatic retries after a transient provider failure (stream stall\\, empty stream\\, overload\\, 5xx\\, network). Omit to use the default (2 retries\\, 3 total attempts). 0 = disabled. Operator-actionable failures (quota\\, auth\\, context overflow) always surface immediately. Exponential backoff: 10s\\, 30s\\, 90s.,example=3"`
	// Fork patch: batch 8 — mid-stream auto-checkpoint interval.
	// When > 0, the agent flushes in-progress streaming Parts to DB
	// at most once per this interval, bounding text lost to SIGTERM
	// during final composition. 0 = use the default (2s). -1 = disable.
	CheckpointIntervalSeconds int `json:"checkpoint_interval_seconds,omitempty" jsonschema:"description=Mid-stream auto-checkpoint interval in seconds. Flushes in-progress assistant text to DB so SIGTERM during final composition does not lose work. Default 2. 0 = use default. -1 = disable.,default=0,example=5"`
	// KeepAliveEnabled toggles the web UI's tiny WebAudio noise loop
	// that keeps the audio output device "audibly alive" — prevents
	// Bluetooth headphones from suspending the stream during long
	// agent-runs, which otherwise eats the first second of a real
	// notification sound. Pointer so nil == "not set, use default ON".
	// Persisted globally because BT-headphone preferences track the
	// operator's machine, not any single project's repo.
	KeepAliveEnabled *bool `json:"keep_alive_enabled,omitempty" jsonschema:"description=Web UI WebAudio keep-alive: emits an inaudible loop so Bluetooth headphones do not auto-suspend during long agent runs. Default true. Set false to disable.,default=true"`
	// NotifyOnBackgroundJobDone controls whether a finished background
	// command (one bash auto-backgrounded after AutoBackgroundAfter)
	// pushes a completion message into the owning session. Default
	// (nil) = enabled. Set false to disable the auto-notification.
	// Web/interactive only — rush run is single-turn and never
	// receives it.
	NotifyOnBackgroundJobDone *bool `json:"notify_on_background_job_done,omitempty" jsonschema:"description=Push a completion message into the session when a background command finishes (web/interactive). Default true. Set false to disable.,default=true"`
	// AutoResumeOnJobDone enables Phase 4 autonomous idle-resume: when a
	// background command finishes and the owning session is idle, the agent
	// starts a fresh turn on its own (bounded by maxConsecutiveAutoResumes,
	// reset by any human message). OPT-IN: default (nil) = DISABLED. This is
	// the opposite default from NotifyOnBackgroundJobDone (which defaults on)
	// because autonomy must be deliberately enabled. Web/interactive only —
	// never fires for rush run.
	AutoResumeOnJobDone *bool `json:"auto_resume_on_job_done,omitempty" jsonschema:"description=Autonomously resume an idle session when a background job finishes (web/interactive). Default false (opt-in). Bounded by an internal consecutive-resume cap, reset by any human message.,default=false"`
	// SupervisionEnabled toggles the periodic root-session supervision
	// check-in (design doc docs/plans/2026-09-27-async-structured-
	// concurrency.md §7): after SupervisionIntervalMinutes of silence in the
	// ROOT session's chat, while its scope still has open work (running
	// commands or sub-agent delegations) and it is not mid-turn, the root is
	// woken with a work summary. Never holds the run open by itself and never
	// fires for a session with no open work. Default (nil) = enabled.
	SupervisionEnabled *bool `json:"supervision_enabled,omitempty" jsonschema:"description=Periodically wake the root session with a work summary after chat silence while background jobs/delegations are still open. Default true. Set false to disable.,default=true"`
	// SupervisionIntervalMinutes is the initial (and post-progress-reset)
	// silence interval before a supervision check-in. Grows on consecutive
	// no-progress ticks (doubling, capped at 60 minutes) and resets to this
	// value once a real completion happens. 0 (default) = 5 minutes.
	SupervisionIntervalMinutes int `json:"supervision_interval_minutes,omitempty" jsonschema:"description=Minutes of chat silence before a supervision check-in while work is open. Default 5. 0 = use default.,default=0,example=10"`
	// BackgroundJobDefaultTimeoutSeconds is the opt-in default
	// terminate_and_wake deadline for background bash/run_command calls that
	// pass no explicit timeout. 0 (default) = off.
	BackgroundJobDefaultTimeoutSeconds int                  `json:"background_job_default_timeout_seconds,omitempty" jsonschema:"description=Default terminate_and_wake deadline in seconds for background bash/run_command calls that pass no explicit timeout. A per-call timeout always wins. Default 0 = off (opt-in).,default=0,example=120"`
	Finalization                       *FinalizationOptions `json:"finalization,omitempty" jsonschema:"description=Agent finalization (batch agents): hold the final answer until children finish\\, reaction debts settle\\, there is no cancellation and no open mutations. Default: false."`
	// NoRealWorkspace marks a config whose session has no real host
	// working directory (sdk.ModeLibrary with an empty Options.WorkingDir:
	// an ephemeral, in-memory session). It is the single authoritative
	// signal every no-real-disk enforcement point reads (SDK review
	// round 14): internal/app's ExecuteRun refuses per-call FolderScopes
	// that would resolve against the real OS disk before any provider
	// traffic (R14-2), and internal/agent's buildTools applies a final
	// tool floor no later layering (worker sub-agent toolset,
	// folder-scope re-adds) can punch through (R14-1). Deliberately
	// json:"-": it is derived once per client at Open time
	// (sdk.buildLibraryConfig) and must never be settable from
	// rush.json. Per-config state, so two concurrently-open clients
	// (one ephemeral, one not) never share it.
	NoRealWorkspace bool `json:"-"`
}

type MCPs map[string]MCPConfig

type MCP struct {
	Name string    `json:"name"`
	MCP  MCPConfig `json:"mcp"`
}

func (m MCPs) Sorted() []MCP {
	sorted := make([]MCP, 0, len(m))
	for k, v := range m {
		sorted = append(sorted, MCP{
			Name: k,
			MCP:  v,
		})
	}
	slices.SortFunc(sorted, func(a, b MCP) int {
		return strings.Compare(a.Name, b.Name)
	})
	return sorted
}

// ResolvedEnv returns m.Env with every value expanded through the
// given resolver. The returned slice is of the form "KEY=value" sorted
// by key so callers get deterministic output; the receiver's Env map is
// not mutated. On the first resolution failure it returns nil and an
// error that identifies the offending key; the inner resolver error is
// already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work. Callers are expected to surface it
// (for MCP, via StateError on the status card) rather than silently
// spawn the server with an empty credential.
//
// The resolver choice matters: in server mode pass the shell resolver
// so $VAR / $(cmd) expand; in client mode pass IdentityResolver so the
// template is forwarded verbatim and expansion happens on the server.
func (m MCPConfig) ResolvedEnv(r VariableResolver) ([]string, error) {
	return resolveEnvs(m.Env, r)
}

// ResolvedEnvContext expands m.Env with ctx supplied to every value.
func (m MCPConfig) ResolvedEnvContext(ctx context.Context, r VariableResolver) ([]string, error) {
	return resolveEnvsContext(ctx, m.Env, r)
}

// ResolvedArgs returns m.Args with every element expanded through the
// given resolver. A fresh slice is allocated; m.Args is never mutated.
// On the first resolution failure it returns nil and an error
// identifying the offending positional index; the inner resolver error
// is already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work.
//
// See ResolvedEnv for guidance on picking a resolver.
func (m MCPConfig) ResolvedArgs(r VariableResolver) ([]string, error) {
	if len(m.Args) == 0 {
		return nil, nil
	}
	out := make([]string, len(m.Args))
	for i, a := range m.Args {
		v, err := r.ResolveValue(a)
		if err != nil {
			return nil, fmt.Errorf("arg %d: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

// ResolvedArgsContext expands m.Args with ctx supplied to every value.
func (m MCPConfig) ResolvedArgsContext(ctx context.Context, r VariableResolver) ([]string, error) {
	if len(m.Args) == 0 {
		return nil, nil
	}
	out := make([]string, len(m.Args))
	for i, a := range m.Args {
		v, err := ResolveValueContext(ctx, r, a)
		if err != nil {
			return nil, fmt.Errorf("arg %d: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

// ResolvedURL returns m.URL expanded through the given resolver. The
// receiver is not mutated. Errors from the resolver are already
// sanitized by ResolveValue and are wrapped with %w for errors.Is/As.
//
// URLs run through the same shell-expansion pipeline as the other
// fields, so a literal '$' (e.g. OData query strings containing
// $filter/$select) must be escaped as '\$' or '${DOLLAR:-$}' to avoid
// being interpreted as a variable reference. Same constraint already
// applies to command, args, env, and headers.
//
// See ResolvedEnv for guidance on picking a resolver.
func (m MCPConfig) ResolvedURL(r VariableResolver) (string, error) {
	if m.URL == "" {
		return "", nil
	}
	v, err := r.ResolveValue(m.URL)
	if err != nil {
		return "", fmt.Errorf("url: %w", err)
	}
	return v, nil
}

// ResolvedURLContext expands m.URL with ctx.
func (m MCPConfig) ResolvedURLContext(ctx context.Context, r VariableResolver) (string, error) {
	if m.URL == "" {
		return "", nil
	}
	v, err := ResolveValueContext(ctx, r, m.URL)
	if err != nil {
		return "", fmt.Errorf("url: %w", err)
	}
	return v, nil
}

// ResolvedHeaders returns m.Headers with every value expanded through
// the given resolver. A fresh map is allocated; m.Headers is never
// mutated. On the first resolution failure it returns nil and an error
// identifying the offending header name; the inner resolver error is
// already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work.
//
// A header whose value resolves to the empty string (unset bare $VAR
// under lenient nounset, $(echo), or literal "") is omitted from the
// returned map — sending "X-Auth:" with an empty value is rejected by
// some providers and the user's intent in "optional, env-gated
// header" is clearly "absent when the var isn't set."
//
// See ResolvedEnv for guidance on picking a resolver.
func (m MCPConfig) ResolvedHeaders(r VariableResolver) (map[string]string, error) {
	if len(m.Headers) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(m.Headers))
	keys := make([]string, 0, len(m.Headers))
	for k := range m.Headers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v, err := r.ResolveValue(m.Headers[k])
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", k, err)
		}
		if v == "" {
			continue
		}
		out[k] = v
	}
	return out, nil
}

// ResolvedHeadersContext expands every header value with ctx.
func (m MCPConfig) ResolvedHeadersContext(ctx context.Context, r VariableResolver) (map[string]string, error) {
	if len(m.Headers) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(m.Headers))
	// Sort keys so failures are reported deterministically when more
	// than one header would fail.
	keys := make([]string, 0, len(m.Headers))
	for k := range m.Headers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v, err := ResolveValueContext(ctx, r, m.Headers[k])
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", k, err)
		}
		if v == "" {
			continue
		}
		out[k] = v
	}
	return out, nil
}
