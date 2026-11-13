---
name: rush-config
description: Use when the user needs help configuring Rush — working with rush.ktav (or legacy rush.json), setting up providers, configuring LSPs, adding MCP servers, managing skills or permissions, or changing Rush behavior.
---

# Rush Configuration

Rush prefers Ktav configuration files; a legacy JSON file is still read
when no Ktav file exists (if both are present, the Ktav file wins and
Rush emits one warning). Priority (highest to lowest):

1. Project configs, discovered per directory walking up from the current
   working directory; at each directory the **visible** `rush.ktav` (or
   legacy `rush.json`) takes precedence over the hidden `.rush.ktav`
   (or `.rush.json`) at that same directory.
2. `$XDG_CONFIG_HOME/rush/rush.ktav` or `$HOME/.config/rush/rush.ktav` (global; legacy `rush.json`)

## Basic Structure

```ktav
models: {}
providers: {}
mcp: {}
lsp: {}
hooks: {}
options: {}
permissions: {}
tools: {}
```

Editor/JSON-Schema validation is not available for Ktav config files (see `docs/ktav-config.md`).

## Shell Expansion

Rush runs selected string fields through an embedded bash-compatible
shell at load time, so values can pull from env vars, files, or helper
commands.

Supported constructs (match the `bash` tool):

- `$VAR` and `${VAR}`
- `${VAR:-default}`, `${VAR:+alt}`, `${VAR:?message}`
- `$(command)` with full quoting and nesting
- Single- and double-quoted strings, escapes

Default semantics match bash: an unset variable expands to an empty
string, no error. A failing `$(command)` is always a hard error. For
required credentials, use `${VAR:?message}` so a missing variable
fails loudly at load time with your message.

```ktav
api_key: ${CODEBERG_TOKEN:?set CODEBERG_TOKEN}
```

### Which fields expand

| Surface                                             | Expansion |
| --------------------------------------------------- | --------- |
| Provider `api_key`, `base_url`, `api_endpoint`      | yes       |
| Provider `extra_headers`                            | yes       |
| Provider `extra_body`                               | **no**    |
| MCP `command`, `args`, `env`, `headers`, `url`      | yes       |
| LSP `command`, `args`, `env`                        | yes       |
| Hook `command`                                      | runs via `sh -c`, not the resolver |

`extra_body` is a JSON passthrough. If you need env-driven values in
a request body, put them in `extra_headers`, `api_key`, or
`base_url` instead.

### Empty-resolved headers are dropped

When a header value resolves to the empty string (unset variable,
`$(echo)`, or literal `""`), the header is omitted from the
outgoing request. This keeps optional env-gated headers like
`"OpenAI-Organization": "$OPENAI_ORG_ID"` working cleanly when the
var isn't set. Applies to MCP `headers` and provider `extra_headers`.

### Security note

`rush.ktav` (or legacy `rush.json`) is trusted code. Any `$(...)` in it
runs at load time
with the invoking user's shell privileges, before the UI appears.
Don't launch Rush in a directory whose `rush.ktav` you haven't
reviewed.

## Common Tasks

- Add a custom provider: add an entry under `providers` with `type`, `base_url`, `api_key`, and `models`.
- Disable a builtin or local skill: add the skill name to `options.disabled_skills`.
- Add an MCP server: add an entry under `mcp` with `type` and either `command` (stdio) or `url` (http/sse).

## Model Selection

```ktav
models: {
    smart: {
        model: claude-sonnet-4-20250514
        provider: anthropic
        max_tokens: 16384
    }
    fast: {
        model: claude-haiku-4-20250514
        provider: anthropic
    }
}
```

- `smart` is the primary coding model; `fast` is for summarization. (Two more optional slots exist: `worker` for cheap delegated sub-task work, `reviewer` for the strongest slot on explicit review — see `rush models --help`.)
- Only `model` and `provider` are required.
- Optional tuning: `reasoning_effort`, `think`, `max_tokens`, `temperature`, `top_p`, `top_k`, `frequency_penalty`, `presence_penalty`, `provider_options`.

## Custom Providers

```ktav
providers: {
    deepseek: {
        type: openai-compat
        base_url: https://api.deepseek.com/v1
        api_key: $DEEPSEEK_API_KEY
        models: [
            {
                id: deepseek-chat
                name: Deepseek V3
                context_window: 64000
            }
        ]
    }
}
```

- `type` (required): `openai`, `openai-compat`, or `anthropic`
- `api_key`, `base_url`, `api_endpoint`, and `extra_headers` are shell-expanded (see [Shell Expansion](#shell-expansion)).
- `extra_body` is a JSON passthrough and is **not** expanded.
- Additional fields: `disable`, `system_prompt_prefix`, `extra_headers`, `extra_body`, `provider_options`.

## LSP Configuration

```ktav
lsp: {
    go: {
        command: gopls
        env: {
            GOPATH: $HOME/go
        }
    }
    typescript: {
        command: typescript-language-server
        args: [
            --stdio
        ]
    }
}
```

- `command` (required), `args`, `env` cover most setups.
- `command`, `args`, and `env` values are shell-expanded (see [Shell Expansion](#shell-expansion)).
- Additional fields: `disabled`, `filetypes`, `root_markers`, `init_options`, `options`, `timeout`.

## MCP Servers

```ktav
mcp: {
    filesystem: {
        type: stdio
        command: node
        args: [
            /path/to/mcp-server.js
        ]
    }
    github: {
        type: http
        url: https://api.githubcopilot.com/mcp/
        headers: {
            Authorization: Bearer $GH_PAT
        }
    }
}
```

- `type` (required): `stdio`, `sse`, or `http`
- `command`, `args`, `env`, `headers`, and `url` are shell-expanded (see [Shell Expansion](#shell-expansion)).
- Additional fields: `env`, `disabled`, `disabled_tools`, `enabled_tools`, `timeout`.

### CLI mode (`rush run` and other non-interactive commands)

A server only starts during `rush run` (and every other CLI subcommand
except the bare `rush` that starts the web UI) if its config sets
`enabled_in_cli: true`. It is off by default, because most MCP servers
help a human chatting in the web UI but add nothing to an unattended
coding agent, while still paying real startup cost — a `stdio` server
spawns its own child process on every single invocation whether or not
the task ever calls it.

```ktav
mcp: {
    github: {
        type: http
        url: https://api.githubcopilot.com/mcp/
        headers: {
            Authorization: Bearer $GH_PAT
        }
        enabled_in_cli: true
    }
}
```

The interactive web UI is never affected by this field — it always
starts every non-disabled server. `rush run --all-mcp` overrides the
gate for a single invocation that genuinely needs a server rush would
otherwise skip. `rush mcp add`/`rush mcp set` accept `--enabled-in-cli`
to set this without hand-editing the config file; `rush mcp list` shows
a `CLI` column for the effective value.

## Options

```ktav
options: {
    skills_paths: [
        ./skills
    ]
    disabled_tools: [
        bash
        sourcegraph
    ]
    disabled_skills: [
        rush-config
    ]
    tui: {
        compact_mode: false
        diff_mode: unified
        transparent: false
    }
    auto_lsp: true
    debug: false
    debug_lsp: false
    attribution: {
        trailer_style: assisted-by
        generated_with: true
    }
}
```

> [!IMPORTANT]
> The following skill paths are loaded by default and DO NOT NEED to be added to `skills_paths`:
> `.agents/skills`, `.rush/skills`, `.claude/skills`, `.cursor/skills`

Other options: `context_paths`, `progress`, `disable_notifications`, `disable_auto_summarize`, `disable_metrics`, `disable_provider_auto_update`, `disable_default_providers`, `data_directory`, `initialize_as`.

- Agent finalization for batch agents: `options.finalization.enabled` (default false) — see `docs/agent-finalization.md`.

## User-Invocable Skills

Skills can be made invocable as commands from the commands palette. Add `user-invocable: true` to the skill's YAML frontmatter:

```yaml
---
name: my-skill
description: A skill that can be invoked as a command.
user-invocable: true
---
```

User-invocable skills appear in the commands palette with a prefix:
- Skills from global directories: `user:skill-name`
- Skills from project directories: `project:skill-name`

When invoked, the skill's instructions are loaded into the conversation context.

To prevent the model from auto-triggering a skill (while still allowing user invocation), add `disable-model-invocation: true`:

```yaml
---
name: my-skill
description: Only invocable by users, not the model.
user-invocable: true
disable-model-invocation: true
---
```

Skills with `disable-model-invocation` won't appear in the model's available skills list but can still be invoked manually by users.

## Hooks

Hook configuration lives in `rush.ktav` (or legacy `rush.json`); the hook
stdin/stdout wire protocol remains JSON. Hooks are user-defined shell
commands that fire on agent events. Currently only `PreToolUse` is supported, which runs before a tool is executed.

```ktav
hooks: {
    PreToolUse: [
        {
            matcher: ^(edit|write|multiedit)$
            command: .rush/hooks/protect-files.sh
        }
        {
            matcher: ^bash$
            command: .rush/hooks/no-haskell.sh
        }
    ]
}
```

### Hook Properties

- `command` (required): Shell command to execute. Runs via `sh -c`.
- `matcher` (optional): Regex pattern tested against the tool name. Empty or absent means match all tools.
- `timeout` (optional): Timeout in seconds. Defaults to 30.

### Event Name Normalization

Event names are case-insensitive and accept snake_case variants: `PreToolUse`, `pretooluse`, `pre_tool_use`, and `PRE_TOOL_USE` all work.

### How Hooks Work

1. When a tool is about to be called, all `PreToolUse` hooks with a matching `matcher` (or no matcher) run in parallel.
2. Duplicate commands are deduplicated — each unique command runs at most once.
3. The hook receives JSON on **stdin** and hook-specific **environment variables**.

### Hook Input (stdin)

A JSON payload is piped to the hook command:

```json
{
  "event": "PreToolUse",
  "session_id": "abc-123",
  "cwd": "/path/to/project",
  "tool_name": "bash",
  "tool_input": {"command": "ls -la"}
}
```

### Hook Environment Variables

| Variable | Description |
|---|---|
| `RUSH_EVENT` | Event name (e.g. `PreToolUse`) |
| `RUSH_TOOL_NAME` | Name of the tool being called |
| `RUSH_SESSION_ID` | Current session ID |
| `RUSH_CWD` | Current working directory |
| `RUSH_PROJECT_DIR` | Project root directory |
| `RUSH_TOOL_INPUT_COMMAND` | Value of `command` from tool input (if present) |
| `RUSH_TOOL_INPUT_FILE_PATH` | Value of `file_path` from tool input (if present) |

### Hook Output

**Exit code 0** — the hook succeeded. Stdout is parsed as JSON:

```json
{"decision": "allow", "context": "optional context appended to tool result"}
```

- `decision`: `allow` to explicitly allow, `deny` to block, `none` (or omit) for no opinion.
- `reason`: Explanation text (used when denying).
- `context`: Extra context appended to the tool result.
- `updated_input`: Replacement JSON for the tool input. Last non-empty value wins.

**Exit code 2** — the tool call is blocked. Stderr is used as the deny reason.

```bash
echo "No Haskell allowed" >&2
exit 2
```

**Any other exit code** — non-blocking error. The tool call proceeds as normal.

### Claude Code Compatibility

Rush also supports the Claude Code hook output format:

```json
{
  "hookSpecificOutput": {
    "permissionDecision": "allow",
    "permissionDecisionReason": "Auto-approved",
    "updatedInput": {"command": "echo rewritten"}
  }
}
```

Existing Claude Code hooks should work without modification.

### Decision Aggregation

When multiple hooks match, their decisions are aggregated:

- **Deny wins over allow** — if any hook denies, the tool call is blocked.
- **Allow wins over none** — if no hook denies but at least one allows, the call proceeds.
- All deny reasons are concatenated (newline-separated).
- All context strings are concatenated (newline-separated).
- For `updated_input`, the last non-empty value wins.

## Tool Permissions

```ktav
permissions: {
    allowed_tools: [
        view
        ls
        grep
        edit
    ]
}
```

### Restricted `rush run` allowlist

By default `rush run` is non-interactive and auto-approves every
permission request (no one is at the keyboard). For unattended / CI use
you can flip to a deny-by-default model and carve out exactly what the
run may do. This only affects `rush run`; interactive TUI / web
sessions keep the normal permission flow.

Rush-style config can also be written as rush.json-style JSON when a
`rush.ktav` does not exist — the next example mixes `\`-escapes (a `\s` in a
regex), so it is shown in JSON to stay unambiguous:

```jsonc
{
  "permissions": {
    "allowed_tools": ["view"],
    "run": {
      "restrict": true,
      "allow_tools": ["view", "edit:write"],
      "allow_bash": [
        "git diff",
        "glob:ls *",
        "regex:^go (test|build)(\\s|$)"
      ]
    }
  }
}
```

- `run.restrict` arms the gate. Without it (the default), `rush run`
  keeps auto-approving everything.
- `run.allow_tools` uses the same `tool` / `tool:action` syntax as
  `allowed_tools` and bypasses the run gate for matching **non-bash**
  tool calls. Entries for `bash` / `bash:execute` are intentionally
  ignored — bash is governed solely by `run.allow_bash` so an operator
  can't accidentally authorise arbitrary shell commands by listing the
  tool name.
- `run.allow_bash` matches the bash command string. Each entry is one of:
  - `"cmd args"` — word-boundary prefix match (e.g. `"git diff"` matches
    `"git diff HEAD~1"` but not `"git difftool"`). This form refuses to
    match commands that contain shell-chaining metacharacters
    (`;`, `|`, `&&`, `$(`, `` ` ``), so `"ls"` cannot authorise
    `"ls && rm -rf /"`.
  - `"exact:cmd"` — whole-string equality after trimming whitespace.
    Same chaining guard.
  - `"glob:pat"` — `filepath.Match` against the raw command. No
    chaining guard (the wildcard is the user's explicit choice).
  - `"regex:pat"` — `regexp.MatchString` against the raw command. No
    chaining guard.

Precedence: `permissions.allowed_tools` is a global bypass that is
checked **before** the run gate and still wins — if `bash` is listed
there it is allowed unconditionally even in restricted mode. That is the
documented escape hatch for a full bash bypass. For command-level
control, leave `bash` out of `allowed_tools` and use `run.allow_bash`
instead.

CLI equivalents were removed: `rush run` no longer takes
`--restrict-run` / `--allow-bash` / `--allow-tool`. Scoping is
configured only through the `permissions.run` block above, and further
run-permission management is planned through roles, not CLI flags.

Related run/session surface (independent of this gate): `rush run
--worktree <slug>` runs inside an isolated git worktree managed with
`rush worktree list` / `rush worktree remove`; `rush result
<session|path>` (with `--text`/`--json`/`--review`/`--field`) reads a
run's JSON envelope back, `rush run --findings-from <session|path>`
feeds a previous run's review findings into the next run, and
`rush run --fail-on-review` turns a `review_verdict: "fail"` into exit
code 3; `rush sessions status <id> [--json]` prints a one-shot
machine-readable session/batch snapshot (unestablishable facts are
reported as `"unknown"`, never guessed).

## Environment Variables

- `RUSH_GLOBAL_CONFIG` - Override global config location
- `RUSH_GLOBAL_DATA` - Override data directory location
- `RUSH_SKILLS_DIR` - Override default skills directory
