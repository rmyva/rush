// SessionAgent and tool-slice construction, including worker sub-agent
// toolset layering. Extracted from coordinator.go — pure code move,
// bodies unchanged.

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/hooks"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/shell"
)

func (c *coordinator) buildAgent(ctx context.Context, prompt *prompt.Prompt, agent config.Agent, isSubAgent bool) (SessionAgent, error) {
	// ONE snapshot for the entire build: models, prompt, and tools. Before
	// this (task #576/P1-3), models came from buildAgentModels' own
	// Snapshot() while the provider options/system-prompt-prefix/Options
	// fields below and buildTools' many reads each took a fresh, separately
	// timed c.cfg.Config(). A reload landing anywhere in between could hand
	// back an agent whose model, prompt (WorkerAvailable) and toolset
	// (worker tool layering, hooks, grep/ls options, attribution, skills
	// paths) disagreed with each other -- an internally inconsistent agent
	// built across two config generations. Threading this one cfg through
	// closes that gap for all of the above; only explicitly-declared live
	// *policy* reads remain (see PeakHoursCheck below), plus MCP tools,
	// which are deliberately NOT pinned -- see the MCP block in buildTools
	// for why.
	cfg, _ := c.cfg.Snapshot()

	smart, fast, err := c.buildAgentModelsFromCfg(ctx, cfg, isSubAgent)
	if err != nil {
		return nil, err
	}

	var smartProviderCfg config.ProviderConfig
	if creds := callCredentialsFrom(ctx); creds != nil {
		if cred, ok := creds.credential(smart.ModelCfg.Provider); ok {
			smartProviderCfg = credentialProviderConfig(cred)
		}
	} else {
		smartProviderCfg, _ = cfg.Providers.Get(smart.ModelCfg.Provider)
	}
	opts := cfg.Options
	var streamIdleTimeout time.Duration
	if opts != nil && opts.StreamIdleTimeoutSeconds > 0 {
		streamIdleTimeout = time.Duration(opts.StreamIdleTimeoutSeconds) * time.Second
	}
	// Never-freeze backstop: bound the watchdog's tool-pause.
	var toolMaxDuration time.Duration
	if opts != nil && opts.StreamToolTimeoutSeconds > 0 {
		toolMaxDuration = time.Duration(opts.StreamToolTimeoutSeconds) * time.Second
	}
	// Fork patch: batch 8 — mid-stream checkpoint interval.
	var checkpointInterval time.Duration
	if opts != nil {
		switch {
		case opts.CheckpointIntervalSeconds > 0:
			checkpointInterval = time.Duration(opts.CheckpointIntervalSeconds) * time.Second
		case opts.CheckpointIntervalSeconds == -1:
			checkpointInterval = 0 // explicitly disabled
		default:
			checkpointInterval = defaultCheckpointInterval
		}
	}
	// DisableAutoSummarize/DataDirectory must also tolerate a nil Options —
	// the streamIdle/toolMax/checkpointInterval reads above already do.
	var disableAutoSummarize bool
	var dataDirectory string
	if opts != nil {
		disableAutoSummarize = opts.DisableAutoSummarize
		dataDirectory = opts.DataDirectory
	}
	// R3-4: the turn-level activation of per-call restricted-run policies
	// needs the optional permission extension. Nil-safe like c.permissions
	// everywhere else in this function (bare test-fixture coordinators).
	var runAllowlists permission.SessionRunAllowlistManager
	if c.permissions != nil {
		if mgr, ok := c.permissions.(permission.SessionRunAllowlistManager); ok {
			runAllowlists = mgr
		}
	}
	result := NewSessionAgent(SessionAgentOptions{
		// R12-H2b §2.4: the agent's durable enqueue producer holds
		// row-keyed Continuation owners through the coordinator's
		// capability manager (nil on bare fixtures, same as diskCaps).
		diskCaps:             c.diskCaps,
		SmartModel:           smart,
		FastModel:            fast,
		SystemPromptPrefix:   smartProviderCfg.SystemPromptPrefix,
		SystemPrompt:         "",
		IsSubAgent:           isSubAgent,
		DisableAutoSummarize: disableAutoSummarize,
		// permissions can be nil on bare test-fixture coordinators; treat
		// that as fail-closed (not YOLO) rather than panicking.
		IsYolo:            c.permissions != nil && c.permissions.SkipRequests(),
		Sessions:          c.sessions,
		Messages:          c.messages,
		Tools:             nil,
		Config:            c.cfg,
		Notify:            c.notify,
		StreamIdleTimeout: streamIdleTimeout,
		ToolMaxDuration:   toolMaxDuration,
		DataDirectory:     dataDirectory,
		RunAllowlists:     runAllowlists,
		Coord:             c,
		// Phase 3's by-construction release trigger (sessionAgent.
		// onSessionIdle's doc, docs/plans/2026-09-28-async-phase3-spec.md
		// orchestrator decision item 1) PLUS supervision's turn-end deadline
		// push (supervision.go's onSessionIdleHook) -- wired identically for
		// the root agent and every delegated child driver, since buildAgent
		// builds both; both callees are no-ops for a session with nothing to
		// react to.
		OnSessionIdle:      c.onSessionIdleHook,
		RestrictedRuns:     restrictedRunAuthorizer(c.permissions),
		CheckpointInterval: checkpointInterval, // Fork patch: batch 8
		// Fork patch: peak-hours mid-turn re-check. Deliberately LIVE, not
		// pinned to cfg above: this is a runtime *policy* check re-evaluated
		// on every mid-turn tick for the lifetime of the turn (see
		// checkLivePeakHours), not a one-shot construction-time read, so
		// pinning it to the build-time generation would defeat its purpose
		// (a peak_hours edit made by another process while this turn is
		// running must still take effect).
		PeakHoursCheck: func(providerID string) error {
			return c.checkLivePeakHours(providerID)
		},
	})

	// R1-1: resolve the per-call worker-active predicate SYNCHRONOUSLY,
	// before the async build goroutines below are registered — the value
	// they see is fixed at UpdateModels/buildAgent call time, never a lazy
	// read of shared state from inside an already-running goroutine (the
	// hooks/runner.go sync-capture pattern). With a CallOptions-carrying
	// context the value is this call's own ModelRole; without one it is
	// the legacy shared field, read here under its mutex rather than
	// later on a goroutine.
	workerActive := c.workerSubAgentActiveForCall(ctx, cfg)

	gate := readyGateFor(ctx, &c.readyWg)
	gate.Go(func() error {
		// Orchestrator block only ever applies to the top-level coder prompt
		// (isSubAgent false) — a sub-agent renders task.md.tpl via
		// taskPrompt, which doesn't reference WorkerAvailable, but guarding
		// on !isSubAgent here keeps this call's intent explicit rather than
		// relying on the template to ignore the field.
		// Same pinned cfg as the models above and buildTools below — see
		// this function's opening comment.
		systemPrompt, err := prompt.Build(ctx, smart.Model.Provider(), smart.Model.Model(), c.cfg, cfg, !isSubAgent && workerActive)
		if err != nil {
			return err
		}
		result.SetSystemPrompt(systemPrompt)
		return nil
	})

	gate.Go(func() error {
		tools, err := c.buildTools(ctx, cfg, agent, isSubAgent)
		if err != nil {
			return err
		}
		result.SetTools(tools)
		return nil
	})

	return result, nil
}

// workerToolNames are added on top of the sub-agent's existing (read-only)
// AllowedTools when it is acting as a worker (see workerSubAgentActive):
// enough hands to actually do delegated work, not just look around.
//
// Deliberately excludes "agent": a worker must not spawn sub-workers of its
// own — recursion guard, keeps the delegation tree exactly two levels deep
// (orchestrator -> worker).
//
// Includes "ask_question": runSubAgent now recognizes AwaitingAnswerError
// before its generic error branch and returns a question-shaped SUCCESSFUL
// tool response (see subAgentQuestionText in question_stop.go) instead of
// collapsing it into "Failed to generate response: ...". The orchestrator
// answers via AgentParams.ResumeSessionID, which continues the same
// sub-session rather than starting a fresh one. See
// docs/plans/2026-07-26-orchestrator-worker-e2e.md, phase 3, for the full
// round-trip design and why a synchronous blocking version is impossible.
//
// Includes "await_tasks" (and its post-await batch_output read): the worker's await_tasks BLOCKS
// inside the turn (no StopTurn) and after its wake the worker must read the
// finished jobs' outputs; and a worker that started a background shell had
// no way to read it at all before. await_tasks.md already forbids looping
// on job_output.
var workerToolNames = []string{"edit", "multiedit", "write", "bash", "todos", "download", "fetch", tools.AskQuestionToolName, tools.AwaitTasksToolName}

// orchestratorStrippedToolNames are REMOVED from the top-level coder's
// AllowedTools when workerSubAgentActive(cfg) is true (worker configured AND
// active role empty-or-smart). The orchestrator must delegate all file
// mutation to the worker: as long as edit/multiedit/write are present, rule
// 7 in the prompt is only advice, and three real `rush run --role smart`
// runs showed the smart model doing all the hands-on work itself (0 agent
// calls out of 50/51/24 tool calls) with a configured worker never used.
//
// bash and every read tool are deliberately KEPT: rule 7's zero-trust pass
// requires the orchestrator to re-read files and re-run tests itself, and
// delegating verification would mean trusting a worker's report about
// checking a worker's report. The resulting hole — bash can still write
// files via sed, cat >, redirection — is accepted KNOWINGLY: it turns an
// accidental edit into a deliberate act, it does not make the ban airtight.
var orchestratorStrippedToolNames = []string{"edit", "multiedit", "write"}

// buildToolsAgentConfig returns the config.Agent buildTools should use to
// resolve AllowedTools for this build. Two mutations, both gated on
// workerSubAgentActive (worker configured AND active role empty-or-smart):
//
//   - a sub-agent acting as a worker gets a copy of agent with the worker
//     toolset layered on top of whatever was already allowed (today's
//     read-only set, in practice, since this only affects the AgentTask
//     sub-agent);
//   - the top-level coder gets a copy of agent with the edit tools in
//     orchestratorStrippedToolNames removed, so the orchestrator cannot
//     mutate files directly and must go through the `agent` tool.
//
// In every other case — no Worker configured, or an explicit non-smart
// active role (fast/worker/reviewer) — it returns agent unchanged, so
// behavior is byte-identical to before orchestrator mode existed.
// cfg is the caller's pinned snapshot (task #576/P1-3) — buildTools no
// longer takes its own live c.cfg.Config() reads, so this decision now
// agrees with every other config-derived choice buildAgent makes for the
// same build (models, prompt's WorkerAvailable, hooks, MCP, etc.).
//
// R3-1: the gate is now the PER-CALL predicate workerSubAgentActiveForCall,
// so an explicit --role fast/worker/reviewer call's tool set agrees with
// the prompt/model chosen for the SAME call instead of with the shared
// activeModelRole field.
func (c *coordinator) buildToolsAgentConfigForCall(ctx context.Context, cfg *config.Config, agent config.Agent, isSubAgent bool) config.Agent {
	if !c.workerSubAgentActiveForCall(ctx, cfg) {
		return agent
	}

	if isSubAgent {
		allowed := make([]string, len(agent.AllowedTools), len(agent.AllowedTools)+len(workerToolNames))
		copy(allowed, agent.AllowedTools)
		for _, name := range workerLayeredToolNames() {
			if !slices.Contains(allowed, name) {
				allowed = append(allowed, name)
			}
		}
		agent.AllowedTools = allowed
		return agent
	}

	// Top-level coder (buildAgent is only ever called with isSubAgent=false
	// for the coder agent): strip the orchestrator's direct edit tools.
	allowed := make([]string, 0, len(agent.AllowedTools))
	for _, name := range agent.AllowedTools {
		if !slices.Contains(orchestratorStrippedToolNames, name) {
			allowed = append(allowed, name)
		}
	}
	agent.AllowedTools = allowed
	return agent
}

// buildToolsAgentConfig is the legacy, context-less form of
// buildToolsAgentConfigForCall, kept for direct tests: it behaves exactly
// like a caller whose context carries no CallOptions.
func (c *coordinator) buildToolsAgentConfig(cfg *config.Config, agent config.Agent, isSubAgent bool) config.Agent {
	return c.buildToolsAgentConfigForCall(context.Background(), cfg, agent, isSubAgent)
}

// applyCallDisableSubAgents implements the per-call `--agents single`
// sub-agent ban (R1-1). It replaces ExecuteRun's former mutation of the
// PUBLISHED coder AllowedTools (disableSubAgentToolsInConfig +
// UpdateAgentAllowedTools): that write raced every concurrent run's
// toolset build for the whole duration of the mutating run, so a
// DisableSubAgents:false call could observe the delegating call's
// stripped toolset until its defer restored the list. The per-call
// filter touches nothing shared: it applies to THIS build only, and only
// to the top-level coder (isSubAgent false) — exactly the agent config
// the old mutation targeted (cfg.Agents[AgentCoder]).
//
// Semantics mirror the app-side shouldBypassSubAgentBan: strip both
// delegation tools, except when the run explicitly runs the smart role
// AND a Worker model is configured — delegation IS the intended workflow
// there, so only agentic_fetch stays stripped. A context without
// CallOptions (legacy callers) changes nothing.
func (c *coordinator) applyCallDisableSubAgents(ctx context.Context, cfg *config.Config, agent config.Agent, isSubAgent bool) config.Agent {
	opts := callOptionsFrom(ctx)
	if opts == nil || !opts.DisableSubAgents || isSubAgent {
		return agent
	}

	bypass := opts.ModelRole == config.SelectedModelTypeSmart
	if bypass {
		workerModelCfg, ok := cfg.Models[config.SelectedModelTypeWorker]
		bypass = ok && workerModelCfg.Model != ""
	}

	stripped := make([]string, 0, len(agent.AllowedTools))
	for _, name := range agent.AllowedTools {
		if name == tools.AgenticFetchToolName {
			continue
		}
		if name == AgentToolName && !bypass {
			continue
		}
		stripped = append(stripped, name)
	}
	agent.AllowedTools = stripped
	return agent
}

// folderScopeLegacyFileTools are the single-target file tools a scoped
// call never sees: the fs_* family replaces all of them, with batch
// items in place of the single path parameter.
var folderScopeLegacyFileTools = map[string]struct{}{
	tools.ViewToolName: {}, tools.GlobToolName: {}, tools.GrepToolName: {},
	tools.LSToolName: {}, tools.WriteToolName: {}, tools.EditToolName: {},
	tools.MultiEditToolName: {},
}

// folderScopeEscapeHatchTools read or write outside any path policy a
// folder scope can express, so a scoped call never sees them: download
// writes arbitrary paths, git_read reads the whole repository, rush_logs
// reads the host log, the MCP resource tools would expose whatever the
// configured servers serve, and agentic_fetch builds its own unscooped
// view/glob/grep in a temp dir.
var folderScopeEscapeHatchTools = map[string]struct{}{
	tools.DownloadToolName: {}, tools.GitReadToolName: {},
	tools.AgenticFetchToolName: {}, tools.ListMCPResourcesToolName: {},
	tools.ReadMCPResourceToolName: {},
	tools.RushLogsToolName:        {},
}

// folderScopeOpForTool maps each fs_* tool to the single operation that
// gates its presence in a scoped toolset. fs_write is the one
// exception: it appears when EITHER create or overwrite is granted,
// because its per-item check picks the operation by path existence.
var folderScopeOpForTool = tools.FolderScopeToolOperations()

// withoutFolderScopeTools drops every fs_* tool from agent.AllowedTools.
func withoutFolderScopeTools(agent config.Agent) config.Agent {
	kept := make([]string, 0, len(agent.AllowedTools))
	for _, name := range agent.AllowedTools {
		if _, ok := folderScopeOpForTool[name]; ok || name == tools.FSWriteToolName {
			continue
		}
		kept = append(kept, name)
	}
	agent.AllowedTools = kept
	return agent
}

// applyCallFolderScope implements the per-call scoped filesystem
// toolset: when ctx carries CallOptions.FolderScope, the agent's
// AllowedTools is replaced with the tools that scope allows and
// AllowedMCP becomes the "no MCPs" spelling, because an external
// filesystem MCP server would bypass the scope wholesale. It MUST run
// after buildToolsAgentConfigForCall: worker layering ADDS
// bash/edit/write to a worker sub-agent's list, and this filter has to
// see that final list to strip them again. A context without a
// FolderScope only loses the fs_* tools (they would deny every path).
//
// Every fs_* name is decided by the scope's Grants here and never left
// to fall through from the incoming list (which the post-T7 defaults DO
// contain): ungranted fs_* tools are removed, granted ones are
// re-added exactly once, and fs_write additionally requires create OR
// overwrite. Tools outside all four sets -- todos, ask_question, the
// agent delegation tool and so on -- are untouched: the scope narrows
// the filesystem surface, not the conversation.
func (c *coordinator) applyCallFolderScope(ctx context.Context, agent config.Agent) config.Agent {
	opts := callOptionsFrom(ctx)
	if opts == nil || opts.FolderScope == nil {
		// No scope: fs_* would be built with the zero scope and deny every
		// path, so the model burns turns on refusals. Hide them.
		return withoutFolderScopeTools(agent)
	}
	scope := *opts.FolderScope

	commandTools := folderScopeCommandToolSet()
	stripped := make([]string, 0, len(agent.AllowedTools))
	for _, name := range agent.AllowedTools {
		if _, ok := folderScopeLegacyFileTools[name]; ok {
			continue
		}
		if _, ok := folderScopeEscapeHatchTools[name]; ok {
			continue
		}
		if _, ok := commandTools[name]; ok && !scope.KeepsCommandTools() {
			continue
		}
		if _, ok := folderScopeOpForTool[name]; ok {
			// Re-decided by the Grants pass below; never fall through.
			continue
		}
		if name == tools.FSWriteToolName {
			// create OR overwrite, decided below.
			continue
		}
		stripped = append(stripped, name)
	}
	for toolName, op := range folderScopeOpForTool {
		if scope.Grants(op) {
			stripped = append(stripped, toolName)
		}
	}
	if scope.Grants(permission.FileOpCreate) || scope.Grants(permission.FileOpOverwrite) {
		stripped = append(stripped, tools.FSWriteToolName)
	}
	agent.AllowedTools = stripped
	agent.AllowedMCP = map[string][]string{}
	return agent
}

// noRealWorkspaceForbiddenTools are the tools that can never be offered
// on a config whose Options.NoRealWorkspace is set (sdk.ModeLibrary with
// no WorkingDir): every legacy tool that reads or writes the real host
// filesystem via c.cfg.WorkingDir() -- for such a session, the synthetic
// library sentinel, not a sandbox -- plus every command-executing and
// arbitrary-write tool. rush_logs belongs here too (R15-1, P0, SDK
// review round 15): its log path is filepath.Join(Options.DataDirectory,
// "logs", "rush.log"), which collapses to the RELATIVE "logs/rush.log"
// when DataDirectory is empty and is then resolved against the HOST
// process's own working directory by a plain os.Stat/os.Open -- no
// WorkingDir, DiskProvider, or FolderScope seam involved. git_read
// belongs here too (R15-2, P1, SDK review round 15): it runs
// the real `git` binary as an OS subprocess with cmd.Dir set to
// c.cfg.WorkingDir() (internal/agent/tools/git_read.go) -- for such a
// session, the synthetic library sentinel, a real OS-interpreted host
// path, so a git repository that happens to exist at that path would
// have its status/diff/log/show/blame data read and sent to the
// provider. It never goes through a DiskProvider at all, so the fs_*
// custom-provider opt-in does not apply to it. agentic_fetch is listed
// explicitly as well (R14-7, P3, round-15 review follow-up): it was
// previously only INCIDENTALLY safe -- absent from workerToolNames and
// folderScopeOpForTool, so no existing per-call layering re-added it --
// and floor membership makes that denial unconditional rather than
// dependent on two unrelated positive re-add lists. This is the
// floor behind Options.DisabledTools
// (sdk.libraryEphemeralDisabledTools): the disabled list only filters
// the INITIAL AllowedTools, but per-call layering appends afterwards --
// R14-1 (P0, SDK review round 14) found buildToolsAgentConfigForCall
// re-adding write/bash/download (workerToolNames) on an ephemeral
// worker sub-agent with no check against the disabled list at all.
var noRealWorkspaceForbiddenTools = map[string]struct{}{
	tools.BashToolName: {}, tools.RunCommandToolName: {}, tools.DownloadToolName: {},
	tools.EditToolName: {}, tools.MultiEditToolName: {}, tools.WriteToolName: {},
	tools.ViewToolName: {}, tools.GlobToolName: {}, tools.GrepToolName: {},
	tools.LSToolName:           {},
	tools.RushLogsToolName:     {},
	tools.GitReadToolName:      {},
	tools.AgenticFetchToolName: {},
}

// noRealWorkspaceScopedFSTools is the fs_* family's contribution to the
// same floor. Unlike the legacy tools above, the fs_* family has a
// legitimate opt-in on a no-real-workspace session: a call supplying a
// CUSTOM DiskProvider (not nil and not tools.OSDisk()) backs every fs_*
// operation with caller-supplied storage, and applyCallFolderScope has
// already scoped it. When the call carries no custom provider, fs_*
// normalizes to the real OS disk, so the floor strips the whole family
// too (internal/app's ExecuteRun already refuses such calls outright
// for sdk callers -- R14-2; this is the coordinator-side backstop for
// direct internal callers).
var noRealWorkspaceScopedFSTools = map[string]struct{}{
	tools.FSListToolName: {}, tools.FSFindToolName: {}, tools.FSGrepToolName: {},
	tools.FSReadToolName: {}, tools.FSWriteToolName: {}, tools.FSReplaceToolName: {},
	tools.FSWriteLinesToolName: {}, tools.FSDeleteToolName: {},
}

// applyNoRealWorkspaceToolFloor is buildTools' FINAL AllowedTools
// filter (R14-1, P0, SDK review round 14): it runs after
// buildToolsAgentConfigForCall (worker toolset layering) and after
// applyCallFolderScope (folder-scope re-adds), stripping every tool a
// no-real-workspace config must never offer regardless of who added
// it. This makes the ephemeral disabled-tools list a floor later
// layering cannot punch through, rather than a one-time initial
// filter. The call's resolved DiskProvider decides the fs_* half: a
// custom provider keeps the scoped family usable (the README's
// opt-in), nil or the real OSDisk strips it alongside the rest.
func (c *coordinator) applyNoRealWorkspaceToolFloor(cfg *config.Config, agent config.Agent, disk tools.DiskProvider) config.Agent {
	if cfg == nil || cfg.Options == nil || !cfg.Options.NoRealWorkspace {
		return agent
	}
	customDisk := tools.IsCustomDiskProvider(disk)
	stripped := make([]string, 0, len(agent.AllowedTools))
	for _, name := range agent.AllowedTools {
		if _, ok := noRealWorkspaceForbiddenTools[name]; ok {
			continue
		}
		if !customDisk {
			if _, ok := noRealWorkspaceScopedFSTools[name]; ok {
				continue
			}
		}
		stripped = append(stripped, name)
	}
	agent.AllowedTools = stripped
	return agent
}

// rejectScopedCallOnCLIProvider refuses a folder-scoped call whose
// RESOLVED provider for the given role is a CLI provider (Type "cli"):
// that provider executes the file tools inside a CLI subprocess (claude,
// codex, gemini, qwen) whose MCP tool implementations cannot see the
// FolderScope, so the scope would silently mean nothing. The refusal is a
// hard error so it fires BEFORE any provider traffic or subprocess spawn.
// Unscoped calls and non-CLI providers pass untouched.
func (c *coordinator) rejectScopedCallOnCLIProvider(ctx context.Context, role string, providerCfg config.ProviderConfig) error {
	opts := callOptionsFrom(ctx)
	if opts == nil || opts.FolderScope == nil {
		return nil
	}
	if providerCfg.Type != cliprovider.ProviderType {
		return nil
	}
	return fmt.Errorf(
		"folder-scoped call cannot run on the %s role's CLI provider %q (type %q): the subprocess's file tools cannot see the folder scope",
		role, providerCfg.ID, cliprovider.ProviderType)
}

// nestedAgentToolWarnOnce rate-limits the task #1050 nested-delegation
// warning below to once per process, since a misconfigured task agent
// would otherwise log on every buildTools call for every sub-agent turn.
var nestedAgentToolWarnOnce sync.Once

// buildTools builds the tool slice for agent. cfg is the pinned
// *config.Config the caller captured for this whole buildAgent call (task
// #576/P1-3) -- every config-derived choice below (worker tool layering,
// SSRF escape hatch, model metadata, hooks, background-job notify,
// attribution, grep/ls options, skills paths) now reads from this one
// generation instead of each taking its own, separately timed
// c.cfg.Config() call. c.cfg.WorkingDir() stays live: it is process-stable
// (does not change across a config reload), same precedent as
// prompt.Build's store.WorkingDir()/store.Resolver() reads.
//
// MCP tool schemas are the one deliberate live-data exception (task #591/P2-1):
// the tool set below comes from tools.GetMCPTools, which enumerates the
// package-level mcp.Tools() registry -- live MCP client connections and their
// current schemas, refreshed asynchronously by each server's own
// ToolListChangedHandler, EnableServer/DisableServer/RemoveServer, and startup
// Initialize. The registry cannot be pinned to cfg without
// either freezing it to a stale tool schema/connection set (breaking
// reconnection and live tool-list updates) or snapshotting live network
// state, which is not what config generations represent. So len(cfg.MCP)
// (the presence gate for ListMCPResourcesTool/ReadMCPResourceTool, the only
// MCP-derived value that IS plain config data) is pinned like everything
// else here, while GetMCPTools applies the consuming ConfigStore's name filter
// to the live schemas before the AllowedMCP filter. A reload landing mid-build
// can still pair this build's pinned prompt/options/allow-list with a newer
// schema for an owned server, which is the intended live-update behavior. See
// tools.GetMCPTools's doc for the registry side of this.
//
// R3-1: when ctx carries CallOptions, the returned slice is scoped to that
// ONE call (the DisableSubAgents filter, the per-call role gate and the folder-scope filter above
// read the context). Callers must pin it to the call — resolvedOverrides.tools
// / SessionAgentCall.Tools — and must NOT publish it onto the shared agent
// with SetTools; only UpdateModels (which strips CallOptions from its ctx
// first) may publish a global toolset.
func (c *coordinator) buildTools(ctx context.Context, cfg *config.Config, agent config.Agent, isSubAgent bool) ([]fantasy.AgentTool, error) {
	if opts := callOptionsFrom(ctx); opts != nil && callBuildScopeFrom(ctx) == nil {
		EnsureCallBuildScope(opts)
		defer opts.buildScope.close()
	}
	built, err := c.buildPolicyTools(ctx, cfg, agent, isSubAgent)
	return built.tools, err
}

func (c *coordinator) buildPolicyTools(ctx context.Context, cfg *config.Config, agent config.Agent, isSubAgent bool) (policyToolBuild, error) {
	agent = c.applyCallDisableSubAgents(ctx, cfg, agent, isSubAgent)
	agent = c.buildToolsAgentConfigForCall(ctx, cfg, agent, isSubAgent)
	agent = c.applyCallFolderScope(ctx, agent)
	agent = applyCallReviewerReadOnly(ctx, agent, isSubAgent)

	// The scope and disk provider the fs_* tools constructed below are
	// built with: THIS call's FolderScope/DiskProvider, or their zero
	// values for a call with no CallOptions (or a CallOptions that sets
	// neither). The zero scope grants nothing, so the constructions
	// themselves are inert; which fs_* tools reach the returned slice is
	// decided by agent.AllowedTools membership -- applyCallFolderScope
	// for a scoped call, the config defaults for an unscoped one. The
	// zero (nil) disk provider means each constructor normalises to the
	// real filesystem (diskOrOS) -- see CallOptions.DiskProvider's doc
	// comment for why it is deliberately not part of that same
	// zero-scope-denies-everything story.
	var scope permission.FolderScope
	var disk tools.DiskProvider
	if callOpts := callOptionsFrom(ctx); callOpts != nil {
		if callOpts.FolderScope != nil {
			scope = *callOpts.FolderScope
		}
		disk = callOpts.DiskProvider
	}

	// R14-1 (P0, SDK review round 14): final capability floor. Must run
	// LAST -- after worker layering and the folder-scope re-adds above
	// -- so the disabled-tools list is a floor no layering can punch
	// through (see applyNoRealWorkspaceToolFloor's doc).
	agent = c.applyNoRealWorkspaceToolFloor(cfg, agent, disk)

	// SSRF guard escape hatch (Options.AllowPrivateNetworkFetch, off by
	// default): when enabled, every model-facing HTTP tool below gets an
	// explicit allowPrivate=true client instead of letting its own nil
	// fallback build the guarded default. See ssrf_guard.go.
	//
	// Options can be nil on bare test fixtures; every Options read below
	// tolerates that.
	var allowPrivateNetworkFetch bool
	var attribution *config.Attribution
	var skillsPaths []string
	var logFile string
	if cfg.Options != nil {
		allowPrivateNetworkFetch = cfg.Options.AllowPrivateNetworkFetch
		attribution = cfg.Options.Attribution
		skillsPaths = cfg.Options.SkillsPaths
		if cfg.Options.DataDirectory != "" {
			logFile = filepath.Join(cfg.Options.DataDirectory, "logs", "rush.log")
		}
	}
	fetchClient := func(timeout time.Duration) *http.Client {
		if !allowPrivateNetworkFetch {
			return nil
		}
		return tools.NewSSRFGuardedClient(timeout, true)
	}

	var allTools []fantasy.AgentTool
	if slices.Contains(agent.AllowedTools, AgentToolName) {
		if isSubAgent {
			// Recursion guard (task #1050): this build is for the task
			// agent itself -- the only isSubAgent=true build in production
			// (see agent_tool.go's c.buildAgent call). Handing it the
			// "agent" tool would let c.agentTool build a fresh copy of the
			// same task agent, whose buildTools would see AgentToolName
			// again and recurse forever, each level registering two more
			// goroutines on the shared c.readyWg that every run entry point
			// blocks on via readyWg.Wait(). Deny unconditionally, no matter
			// what AllowedTools says.
			nestedAgentToolWarnOnce.Do(func() {
				slog.Warn("task agent AllowedTools includes the agent tool; denying it to prevent nested delegation recursion", "agent", agent.Name)
			})
		} else {
			agentTool, err := c.agentTool(ctx)
			if err != nil {
				return policyToolBuild{}, err
			}
			allTools = append(allTools, agentTool)
		}
	}

	if slices.Contains(agent.AllowedTools, tools.AgenticFetchToolName) {
		agenticFetchTool, err := c.agenticFetchTool(ctx, fetchClient(30*time.Second))
		if err != nil {
			return policyToolBuild{}, err
		}
		allTools = append(allTools, agenticFetchTool)
	}

	// Get the model name for the agent
	modelID := ""
	if modelCfg, ok := cfg.Models[agent.Model]; ok {
		if model := cfg.GetModel(modelCfg.Provider, modelCfg.Model); model != nil {
			modelID = model.ID
		}
	}

	// Build hook runner if PreToolUse hooks are configured.
	var hookRunner *hooks.Runner
	if preToolHooks := cfg.Hooks[hooks.EventPreToolUse]; len(preToolHooks) > 0 {
		hookRunner = hooks.NewRunner(preToolHooks, c.cfg.WorkingDir(), c.cfg.WorkingDir())
	}

	// Background-job completion notification. When a bash command
	// auto-backgrounds and later finishes, notifyBackgroundJobDone persists a
	// bg_shell_done session_notices row (wake=1); the owning session's next
	// turn pulls it into history (a running turn at a step boundary). A turn
	// is started for it by the web process (AutoResumeOnJobDone, at most
	// maxConsecutiveAutoResumes per human message) or by a `rush run` loop's
	// own reaction turn (the same policy: with AutoResumeOnJobDone off a
	// bg-shell-only notice waits for the next natural turn; the loop has no
	// cap). Kill-switch defaults to ON. A `rush run` process registers this
	// callback too, so the row can also appear while its loop waits.
	opts := cfg.Options
	notifyDone := opts == nil || opts.NotifyOnBackgroundJobDone == nil || *opts.NotifyOnBackgroundJobDone
	backgroundManager := c.background
	if backgroundManager == nil {
		backgroundManager = shell.NewBackgroundShellManager()
	}
	var onBgDone func(string, *shell.BackgroundShell)
	if notifyDone {
		onBgDone = func(sessionID string, sh *shell.BackgroundShell) {
			c.notifyBackgroundJobDone(sessionID, sh)
		}
	}

	allTools = append(
		allTools,
		tools.NewAskQuestionTool(),
		tools.NewBashTool(c.permissions, c.cfg.WorkingDir(), attribution, modelID, onBgDone, backgroundManager),
		tools.NewRushInfoTool(c.cfg, c.allSkills, c.activeSkills, c.skillTracker),
		tools.NewInspectAgentTool(c),
		tools.NewInjectAgentTool(c),
		tools.NewStopAgentTool(c),
		tools.NewWakeinTool(c),
		tools.NewAwaitTasksTool(c),
		tools.NewWakeonTool(c),
		tools.NewWakeLoopTool(c),
		tools.NewWakeListTool(c),
		tools.NewWakeCancelTool(c),
		tools.NewDownloadTool(c.permissions, c.cfg.WorkingDir(), fetchClient(5*time.Minute)),
		tools.NewEditTool(c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewMultiEditTool(c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewFetchTool(c.permissions, c.cfg.WorkingDir(), fetchClient(30*time.Second)),
		tools.NewGitReadTool(c.cfg.WorkingDir()),
		tools.NewGlobTool(c.cfg.WorkingDir(), c.permissions),
		tools.NewGrepTool(c.cfg.WorkingDir(), cfg.Tools.Grep, c.permissions),
		tools.NewLsTool(c.permissions, c.cfg.WorkingDir(), cfg.Tools.Ls),
		tools.NewReadDelegationTranscriptTool(c.sessions, c.messages),
		tools.NewRunCommandTool(c.permissions, c.cfg.WorkingDir()),
		tools.NewSourcegraphTool(fetchClient(30*time.Second)),
		tools.NewTodosTool(c.sessions),
		tools.NewViewTool(c.permissions, c.filetracker, c.skillTracker, c.cfg.WorkingDir(), skillsPaths...),
		tools.NewWriteTool(c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),

		// Scoped, batch-capable fs_* family, constructed with THIS
		// call's scope (see the extraction above; the zero value denies
		// everything) and THIS call's disk provider (nil normalises to
		// the real disk inside each constructor). NewFSGrepTool takes
		// workingDir first and scope second; the other seven take scope
		// first. Every constructor's LAST parameter is the DiskProvider
		// precisely because of that reversed-order outlier: a trailing
		// parameter needs no per-constructor special case.
		tools.NewFSListTool(scope, c.cfg.WorkingDir(), cfg.Tools.Ls, disk),
		tools.NewFSFindTool(scope, c.cfg.WorkingDir(), disk),
		tools.NewFSReadTool(scope, c.filetracker, c.cfg.WorkingDir(), disk),
		tools.NewFSGrepTool(c.cfg.WorkingDir(), scope, cfg.Tools.Grep, disk),
		tools.NewFSWriteTool(scope, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir(), disk),
		tools.NewFSReplaceTool(scope, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir(), disk),
		tools.NewFSWriteLinesTool(scope, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir(), disk),
		tools.NewFSDeleteTool(scope, c.permissions, c.cfg.WorkingDir(), disk),
	)

	// R15-1 (P0, SDK review round 15): rush_logs reads a real host
	// file -- Options.DataDirectory's logs/rush.log. With no data
	// directory (an ephemeral SDK session) there is no log file to
	// read, and falling back to the RELATIVE logs/rush.log would
	// resolve against the host process's own working directory, so
	// the tool is simply not constructed or offered at all.
	if logFile != "" {
		allTools = append(allTools, tools.NewRushLogsTool(logFile))
	}

	// cfg.MCP presence is pinned config data (any MCP server configured at
	// all, enabled or not), unlike the tool set itself below -- see this
	// function's doc comment.
	if len(cfg.MCP) > 0 {
		allTools = append(
			allTools,
			tools.NewListMCPResourcesTool(c.cfg, c.permissions, c.mcpOwner),
			tools.NewReadMCPResourceTool(c.cfg, c.permissions, c.mcpOwner),
		)
	}

	filteredTools := make([]fantasy.AgentTool, 0, len(allTools))
	for _, tool := range allTools {
		if slices.Contains(agent.AllowedTools, tool.Info().Name) {
			filteredTools = append(filteredTools, tool)
		}
	}

	allowedControls := batchControlAvailability(filteredTools, agent.AllowedTools)
	{
		if bind := coordinatorBatchBindingFor(c); bind != nil && bind.controls != nil {
			for _, name := range allowedControls {
				if name == tools.AwaitTasksToolName {
					for i, tool := range filteredTools {
						if tool.Info().Name == name {
							filteredTools[i] = tools.NewBatchAwaitTasksTool(c)
						}
					}
					continue
				}
				filteredTools = append(filteredTools, newBatchControlPlaceholder(name))
			}
		}
	}

	// GetMCPTools reads the live MCP tool registry; apply the configured MCP allowlist below.
	for _, tool := range tools.GetMCPTools(c.permissions, c.cfg, c.cfg.WorkingDir(), c.mcpOwner) {
		if agent.AllowedMCP == nil {
			// No MCP restrictions
			filteredTools = append(filteredTools, tool)
			continue
		}
		if len(agent.AllowedMCP) == 0 {
			// No MCPs allowed
			slog.Debug("No MCPs allowed", "tool", tool.Name(), "agent", agent.Name)
			break
		}

		for mcp, tools := range agent.AllowedMCP {
			if mcp != tool.MCP() {
				continue
			}
			if len(tools) == 0 || slices.Contains(tools, tool.MCPToolName()) {
				filteredTools = append(filteredTools, tool)
				break
			}
			slog.Debug("MCP not allowed", "tool", tool.Name(), "agent", agent.Name)
		}
	}
	slices.SortFunc(filteredTools, func(a, b fantasy.AgentTool) int {
		return strings.Compare(a.Info().Name, b.Info().Name)
	})
	return c.finishToolAssembly(ctx, filteredTools, hookRunner, isSubAgent, allowedControls)
}

// sharedBuildScopeFor lazily creates the coordinator-level build scope for
// tool assemblies without trusted call state (the shared session toolset).
func (c *coordinator) sharedBuildScopeFor() *buildResourceScope {
	c.sharedBuildScopeMu.Lock()
	defer c.sharedBuildScopeMu.Unlock()
	if c.sharedBuildScope == nil {
		c.sharedBuildScope = newBuildResourceScope()
	}
	return c.sharedBuildScope
}

// finishToolAssembly is the final construction boundary. Flag OFF (no batch
// binding): the historical branch, byte-for-byte — async wrappers, then
// hooks/restrictions, then the stall-detach wrapper. Flag ON: hooks and
// restrictions run once here, then every batch-admissible work tool is
// re-armed through the ONE authority (batchRuntime.Invoke) and the old
// async/stall wrappers are NOT installed. allowed is the build's control
// grant list: batchControlAvailability over the final filtered tools and
// AllowedTools, computed once in buildPolicyTools.
func (c *coordinator) finishToolAssembly(ctx context.Context, filteredTools []fantasy.AgentTool, hookRunner *hooks.Runner, isSubAgent bool, allowed []string) (policyToolBuild, error) {
	if bind := coordinatorBatchBindingFor(c); bind != nil {
		allowedControls := allowed
		filteredTools = withoutBatchControlPlaceholders(filteredTools)
		filteredTools = decoratePolicyTools(filteredTools, restrictedRunAuthorizer(c.permissions), hookRunner, isSubAgent)
		// Construction ownership (R12-H2 §1.2): the build's pins register
		// in the trusted call state's build-resource scope. A build WITHOUT
		// trusted call state (the coordinator's shared toolset) uses the
		// coordinator's shared scope; a custom provider whose options carry
		// no scope has NO fallback and fails closed.
		scope := callBuildScopeFrom(ctx)
		if scope == nil {
			if opts := callOptionsFrom(ctx); opts != nil && tools.IsCustomDiskProvider(opts.DiskProvider) {
				return policyToolBuild{}, fmt.Errorf(
					"%w: a custom disk provider must carry its binding handle before the call options are shared",
					ErrDiskProviderUnavailable)
			}
		}
		// shared tracks whether the build carries NO trusted call state
		// (the coordinator's eager default toolset). Design §1.2: bare
		// builders create AND close a scope — no process-lifetime
		// fallback. The shared build therefore runs under an EPHEMERAL
		// scope (closed at assembly end, pinning nothing) and DEFERS its
		// registry publication + Construction ownership to first dispatch
		// (batchIngressTool.ensurePinned).
		shared := scope == nil
		deferred := shared
		if shared {
			scope = newBuildResourceScope()
			defer scope.close()
		}
		// failBuildScope closes the CALL's scope on a build-then-error
		// sequence (R12-H2a P1-1): newBatchWorkToolBuild succeeded and pinned
		// construction owners into it, so a later failure (registry resolve,
		// custom-provider handle, InstallBatchModelControls) must not leak
		// them past this assembly boundary.
		failBuildScope := func(err error) (policyToolBuild, error) {
			if !shared {
				scope.close()
			}
			return policyToolBuild{}, err
		}
		built, err := newBatchWorkToolBuild(bind, filteredTools, scope, deferred)
		if err != nil {
			return failBuildScope(err)
		}
		// A deferred (shared default toolset) build has no published
		// registry identity until first dispatch, so the model controls
		// install per-call, never in the shared build.
		if bind.controls != nil && !deferred {
			allowed := allowedControls
			if len(allowed) > 0 {
				registry, err := bind.ingress.Registry(built.registryID)
				if err != nil {
					return failBuildScope(err)
				}
				inv := BatchInvocation{
					RegistryID: built.registryID, Origin: "coordinator", Delivery: batchDeliveryChoice{Mode: batchDeliveryDetached},
					BackgroundEscape: batchEscapeNever,
					WorkerStopScope:  isSubAgent,
					Policy:           batchLeafPolicyContext{RegistryID: built.registryID, RestrictedToolset: batchToolsetFingerprint(filteredTools)},
				}
				inv.SessionID = tools.GetSessionFromContext(ctx)
				inv.MessageID = tools.GetMessageFromContext(ctx)
				inv.CallID = ""
				inv.Policy.MessageID = inv.MessageID
				inv.Policy.SupportsImages = tools.GetSupportsImagesFromContext(ctx)
				inv.Policy.ModelName = tools.GetModelNameFromContext(ctx)
				if inv.WaiterRank == 0 {
					inv.WaiterRank = batchGroupRanks[batchGroupDefault]
				}
				inv.Policy.FolderScope = folderScopeSpecFrom(ctx)
				inv.Policy.RunAllowlist = runAllowlistSpecFrom(ctx)
				// A custom (non-OS) call provider carries its IMMUTABLE
				// binding handle, minted when the trusted call state was
				// created (BindCallDiskProvider) — never lazily here: a
				// copied options object must not re-mint a binding, and a
				// custom provider without one fails closed.
				if opts := callOptionsFrom(ctx); opts != nil {
					if bindingID := opts.DiskBindingID; bindingID != "" {
						inv.Policy.DiskBindingID = bindingID
						inv.Policy.DiskProvider = true
					} else if tools.IsCustomDiskProvider(opts.DiskProvider) {
						return failBuildScope(fmt.Errorf(
							"%w: a custom disk provider must carry its binding handle before the call options are shared",
							ErrDiskProviderUnavailable))
					}
				}
				controls, err := bind.controls.InstallBatchModelControls(registry, inv)
				if err != nil {
					return failBuildScope(err)
				}
				for _, control := range controls {
					if control.Info().Name == tools.AwaitTasksToolName {
						continue
					}
					if slices.Contains(allowed, control.Info().Name) {
						built.tools = append(built.tools, wrapToolsWithHooks(wrapToolsWithRestrictedRun([]fantasy.AgentTool{control}, restrictedRunAuthorizer(c.permissions)), hookRunner, isSubAgent)[0])
					}
				}
			}
		}
		return built, nil
	}
	// Per-call pinned tool slices bypass sessionAgent.SetTools, so apply the
	// restricted-run gate at this assembly boundary as well as at the agent
	// boundary. This keeps every actual dispatch path fail-closed. Hooks are
	// applied outside this wrapper below so PreToolUse runs first.
	filteredTools = decoratePolicyTools(filteredTools, restrictedRunAuthorizer(c.permissions), hookRunner, isSubAgent)
	// Wrap tools with hook interception for the top-level agent only.
	// Sub-agents (the `agent` task tool, `agentic_fetch`, etc.) run
	// without hook interception to avoid firing the user's hook N times
	// per delegated turn. The top-level invocation of the sub-agent tool
	// itself is still wrapped from the coder's side.
	return newPolicyToolBuild(filteredTools), nil
}
