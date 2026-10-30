# sdk — embed Rush as a Go library

`sdk` is the public, embeddable surface of Rush: the engine behind `rush
run` as a plain Go function call, no CLI process required. This document
is a practical guide for embedding it in your own Go program. Field-level
details live in the godoc comments on `sdk.go`/`library_mode.go` — this
file covers the scenarios and the things that aren't obvious from a
signature alone.

## Two ways to open a Client

### Application mode (the default)

```go
client, err := sdk.Open(ctx, sdk.Options{
    WorkingDir: "/path/to/project",
})
```

This is exactly what `rush run` does internally: `WorkingDir` is
required, and Rush discovers and loads `rush.ktav` (project, workspace,
global — legacy `rush.json` is still read when no `rush.ktav` exists)
and `.mcp.json` from it, the same way the CLI does. If
`WorkingDir` doesn't exist yet, `Open` creates it — it stays a required
parameter (there is no fallback to the process's own working directory
or a temp directory), but the caller doesn't have to `mkdir` a
freshly-provisioned workspace by hand first.

Use this mode when your host process manages a real, persistent project
directory — the credentials, models, and tool settings all come from
`rush.ktav` (or legacy `rush.json`) as usual. Opening an application-mode Client acquires an MCP
owner: the first client in the process takes the process-wide owner; a
second concurrent application-mode `Open` reserves a standalone owner
instead of failing, so two application-mode Clients coexist, each routing
its MCP operations through its own owner. The registry underneath is still
one name-keyed process table: keep each client's server names disjoint — a
server configured in the first client is never visible to the second's
surfaces, but two clients configuring the same server name would share one
registry entry.

### Library mode — explicit config, no persistence on disk

```go
client, err := sdk.Open(ctx, sdk.Options{
    Mode: sdk.ModeLibrary,
    LibraryConfig: &sdk.LibraryConfig{
        Credentials: []sdk.Credential{{
            Provider: "my-provider",
            Type:     sdk.ProviderTypeOpenAICompat,
            APIKey:   apiKey,
            BaseURL:  "https://api.example.com/v1",
            Models: []sdk.CredentialModel{
                {ID: "my-model", ContextWindow: 200000, DefaultMaxTokens: 4096},
            },
        }},
        Models: map[sdk.Role]sdk.ModelChoice{
            sdk.RoleSmart: {Provider: "my-provider", Model: "my-model"},
        },
    },
})
```

`Options.Mode` defaults to `ModeApplication` (the zero value) — every
existing caller that never sets `Mode` is completely unaffected.
`ModeLibrary` skips config-file discovery entirely: no `rush.ktav`
(or legacy `rush.json`), no `.mcp.json`, no global config. Library
mode never reads or migrates disk config — only application mode
discovers and loads `rush.ktav`/`rush.json`. Every provider and model
role comes from `LibraryConfig` instead. `LibraryConfig.Models` must define at least the
`smart` role; the others (`fast`, `worker`, `reviewer`) are optional.
Library mode also calls `app.SkipMCP()`: it starts no MCP servers and does
not acquire the process-wide MCP owner. It can therefore coexist with an
application-mode Client, and multiple library-mode Clients can coexist with
one another.

**`WorkingDir` is optional in this mode, and that choice matters:**

- **Omitted** → an *ephemeral* session backed by an in-memory SQLite
  connection: session/message persistence never touches disk. A
  graceful `Close()` releases the in-memory handles, and the session
  data is gone with them. On a forced `Close()` — work that was still
  busy after cancellation, which includes background agent work
  (session title generation, cache keep-alive replays) and a busy
  run-queue pump worker, not just the calls you had in flight — the
  handles are deliberately left open and the data survives under them
  until you reclaim it with `CloseEphemeralConnsForced()` (see
  "Shutting down" below). Either way, once the handles are closed there
  is no way to recover the session. Use this for stateless, one-shot
  embedding (a request handler that spins up a Client, runs one turn,
  and throws it away).

  **An ephemeral session also gets NO real-disk or command-execution
  tools by default**: `bash`, `run_command`, `download`,
  `agentic_fetch`, `edit`, `multiedit`, `glob`, `grep`, `ls`, `view`,
  `write`, and the whole `fs_*` family are all absent from the model's
  toolset unless a call opts in (below). Four of them — `bash`,
  `run_command`, `download`, and `agentic_fetch` — have no opt-in at
  all: none can be virtualized by a `DiskProvider`, so they stay
  hard-denied even for a call that opts into `FolderScopes`.
  (`agentic_fetch` is stripped for the same reason: it builds its
  sub-agent's file-tool workspace in a temp directory created under the
  real OS temp path, exactly what an ephemeral session promises not to
  touch.)
  There is no real working directory to point them at, so — unlike a
  missing config field silently falling back to a default — Rush
  refuses to hand out tools whose only fallback would be the real host
  filesystem. If you need file access from an ephemeral session, opt in
  **per call** with `RunOverrides.FolderScopes` **and**
  `RunOverrides.DiskProvider` set to your own virtual filesystem
  implementation — that combination is the only way an ephemeral
  session's model gets any file tool (`fs_*`) at all, and every path
  handed to your `DiskProvider` is guaranteed absolute on every OS. A
  `FolderScopes` override with no `DiskProvider` fails the call before
  any provider traffic, rather than silently falling back to the real
  disk.
- **Given** → the directory is created if missing (same as application
  mode), and session data persists under `<WorkingDir>/.rush` — but
  no disk config (`rush.ktav` or legacy `rush.json`) is ever read, and
  nothing is migrated. `LibraryConfig` remains the only
  config source; only the *persistence* differs from the ephemeral case.
  Because there **is** a real working directory here, the default
  toolset behaves exactly like application mode: file and command tools
  are enabled and operate on the real filesystem rooted at
  `WorkingDir`, same as `rush run`.

Known v1 limitation of library mode: MCP servers are not started (no
`.mcp.json` to read from), and agent context files (`AGENTS.md`/`RUSH.md`)
are not loaded.

## `Run` vs `RunWithCredentials`

`Client.Run` is the "normal" call: model and provider come from however
the Client was opened (`rush.ktav`, or legacy `rush.json`, in
application mode; `LibraryConfig`
in library mode).

`Client.RunWithCredentials(ctx, req, creds)` is for genuine multi-tenant
use: **one Client, many concurrent calls, each with its own provider
credentials.** Every field `creds.Models` covers gets resolved fresh for
that one call — a new provider client is built per call, never cached,
and nothing is read from or merged with the Client's own configured
providers. Strict isolation is the default: the `smart` role is **required** in
`creds.Models`, and a `smart`/`fast` role the set doesn't cover is a
hard **error** before any provider traffic — Rush will not quietly
serve a missing role from the Client's configured providers. If you
genuinely want an uncovered role to fall back to the Client's own
configured model (e.g. `fast` for background title generation), set
`AllowConfiguredRoleFallback: true` on the `CredentialSet`. That is a
deliberate crossing of the tenant-credential boundary: the fallback
role runs on **your** (the operator's) provider, with whatever tenant
data that role carries.

```go
result, err := client.RunWithCredentials(ctx, sdk.RunRequest{
    Prompt:            "summarise the attached diff",
    ContinueSessionID: sessionIDForTenant(tenantID), // opaque, unguessable -- see the trust-model section below
}, sdk.CredentialSet{
    Credentials: []sdk.Credential{{
        Provider: "tenant-provider",
        Type:     sdk.ProviderTypeAnthropic,
        APIKey:   tenantAPIKey,
    }},
    Models: map[sdk.Role]sdk.ModelChoice{
        sdk.RoleSmart: {Provider: "tenant-provider", Model: "claude-opus-4-8"},
        sdk.RoleFast:  {Provider: "tenant-provider", Model: "claude-haiku-4-5"},
    },
})
```

Two independent, concurrently-safe calls to `RunWithCredentials` (each
with its own `ContinueSessionID` and its own `CredentialSet`) on the
**same** `Client` never interfere with each other — this was verified
under `-race` specifically because the underlying `sessionAgent`/tool
machinery is shared coordinator-wide by default; per-call credentials
route around that sharing rather than depending on it.

If `Options.Stdout` or `Options.Stderr` is used as the default by concurrent
runs, the SDK serializes each write for that Client, making ordinary writers
such as `*bytes.Buffer` safe to share. Output from separate runs is still
allowed to interleave. A non-nil `RunRequest.Stdout` or `RunRequest.Stderr`
is passed through unchanged; a request-level writer shared across calls is
the caller's responsibility to synchronize.

`ModelChoice.Model` is **not** validated against `Credential.Models` —
an unrecognised model id fails on the first real provider call, exactly
like `rush run --model` does today. OAuth-based providers (e.g. GitHub
Copilot) are out of scope for `CredentialSet` — only a literal API key.

Credentialed roots are marked in memory after admission; that marker lives
for the lifetime of the Client. Automatic background follow-ups are skipped
rather than sent to the Client's configured provider. Any completed work is
delivered on the next explicit call. Delegated child sessions keep their
per-call credential snapshot and are not affected by this root-only refusal.

## Sessions and credentials are independent axes

`RunRequest.ContinueSessionID` identifies a conversation with
get-or-create semantics — an unknown id creates a new session with that
exact id, an existing one continues it. This is completely orthogonal to
credentials: a session doesn't "belong" to a `CredentialSet`. A tenant
just picks its own `ContinueSessionID` and passes its own
`CredentialSet` on each call — no separate directory or sub-Client per
tenant is needed.

Pick the id as an unguessable value whose mapping to your callers *you*
own — the next section explains why.

## Trust model: there is no tenant authorization

Stated as plainly as possible because it matters: **Rush performs no
authorization and no ownership checks whatsoever.** Not on
`ContinueSessionID`, not on `Messages`, not on `Session`, not on the
subscribe streams.

- `RunRequest.ContinueSessionID` has get-or-create semantics keyed on
  the literal id. Any caller who knows — or guesses — an existing
  session id can continue that session, reading and writing its full
  history, with **any** credentials they pass. A per-call
  `CredentialSet` isolates *which provider serves the turn*; it is not
  an authentication mechanism and grants no exclusivity over the
  session.
- `Client.Messages(ctx, id)` and `Client.Session(ctx, id)` return any
  session's full history / metadata to whoever asks, no questions
  asked.
- `SubscribeMessages` / `SubscribeSessions` stream
  create/update/delete events for **every** session the Client knows
  about, with no tenant filtering. If you forward events to
  per-tenant consumers, filter the stream yourself on
  `ev.Payload.SessionID` before it leaves your process.

The host owns this boundary. A multi-tenant host should:

1. Generate session ids that are opaque and unguessable (a UUID or
   equivalent) — never predictable ids like `"tenant-42"`.
2. Keep its own ownership map (session id → caller) and consult it
   *before* handing a `ContinueSessionID` to Rush.
3. Filter subscribe streams per tenant before forwarding anything.

If you need enforced in-process tenant isolation today, put it in your
own authorization layer and only hand Rush ids that the caller is allowed
to touch.

## Scoped filesystem access: `RunOverrides.FolderScopes`

By default a run gets the standard coder toolset. Set `FolderScopes` on
`RunOverrides` to hand one turn a per-folder file-operation ACL instead.
`Dir` is absolute or relative to the client's working directory; `Ops`
lists the granted `sdk.FileOp` values, and an entry with no ops is a
**deny carve-out** that excludes that subtree from every enclosing grant
(the deepest matching entry wins):

```go
res, err := client.Run(ctx, sdk.RunRequest{
    Overrides: sdk.RunOverrides{
        FolderScopes: []sdk.FolderScope{
            {Dir: "src", Ops: []sdk.FileOp{sdk.FileOpRead, sdk.FileOpGrep, sdk.FileOpReplace}},
            {Dir: "src/vendor", Ops: nil}, // deny carve-out
        },
    },
})
```

A scoped run replaces the single-target file tools (`view`, `glob`,
`grep`, `ls`, `write`, `edit`, `multiedit`) with the batch-capable
`fs_*` family (`fs_read`, `fs_list`, `fs_find`, `fs_grep`, `fs_write`,
`fs_replace`, `fs_write_lines`, `fs_delete`): every item carries its own
path, an out-of-scope item is denied **per item** inside the batch
response (the turn continues; only a batch where zero items succeed is a
tool error), and escape hatches (`download`, `git_read`, MCP resources)
and MCP tools are removed for that call. Conversation tools (todos,
ask_question, sub-agents) are untouched. Command-executing tools stay in
a scoped toolset only when the restricted-run gate is armed AND the run
grants actual bash patterns — keeping them is a deliberate grant, and
their commands remain subject to those patterns.

Three boundaries to know:

- **Per-call only, never persisted.** Unlike `SmartModel` /
  `SystemPrompt`, scoping is not written to the session: a later run on
  the same `ContinueSessionID` without `FolderScopes` is an ordinary
  unscoped run with the default toolset.
- **CLI providers are refused.** A folder-scoped call whose resolved
  provider is a CLI provider (`claude` / `codex` / `gemini` / `qwen` —
  they run file tools inside a subprocess that cannot see the scope)
  fails with a hard error before any provider traffic.
- **Durable restart preserves the scope.** A scoped SDK turn orphaned
  into the durable run queue restarts with the SAME scope: the
  uncompiled spec travels on the durable row and is recompiled and
  rebound before the restarted turn runs (the same fix already shipped
  for the run allowlist, finding F2/R4-1). You do not need to re-supply
  `FolderScopes` for a restart to stay scoped.
- **Ephemeral library-mode sessions never get command tools back,
  scope or no scope.** The "command tools stay when the restricted-run
  gate grants bash patterns" behaviour above assumes a real
  filesystem/subprocess for `bash` to run against. An ephemeral
  `ModeLibrary` session (no `WorkingDir`) has none, and shell execution
  cannot be virtualized by a `DiskProvider` the way `fs_*` can, so
  `bash`/`run_command` stay hard-denied there regardless of
  `RestrictedRun`/`AllowBash`. Only the `fs_*` family is reachable in
  that mode, and only when you also supply a `DiskProvider` (see
  "Substituting the filesystem" below).

## Substituting the filesystem: `RunOverrides.DiskProvider`

Set `DiskProvider` on `RunOverrides` to redirect one run's `fs_*` tools
(`fs_read`, `fs_list`, `fs_find`, `fs_grep`, `fs_write`, `fs_replace`,
`fs_write_lines`, `fs_delete`) — including the path resolution their
folder-scope checks run on — away from the real filesystem and onto your
own implementation of the `sdk.DiskProvider` interface: stat, symlink
resolution, streaming read, whole-file read, `MkdirAll`, atomic write,
remove, directory listing, name glob (`Find`) and content search
(`Search`). A virtual path is scope-checked exactly like a real one.

```go
res, err := client.Run(ctx, sdk.RunRequest{
    Overrides: sdk.RunOverrides{
        FolderScopes: []sdk.FolderScope{
            {Dir: "src", Ops: []sdk.FileOp{sdk.FileOpRead, sdk.FileOpCreate}},
        },
        DiskProvider: myVirtualFS,
    },
})
```

`sdk.OSDiskProvider()` returns the real-filesystem implementation. `nil` falls back to it; wrap it (e.g. to log every write) instead of
reimplementing ripgrep dispatch and gitignore-aware directory walking
from scratch.

**What it does NOT affect** (the deliberate non-retrofit): `view`,
`write`, `edit`, `multiedit`, `glob`, `grep`, `ls`, `download`,
`git_read`, `agentic_fetch`, MCP tools, and `bash`/`run_command` all keep
hitting the real filesystem regardless of this field. This is why a
`DiskProvider` requires a `FolderScope` (see below) — a scoped run is
what actually removes the legacy tools that would otherwise still touch
the real disk.

Boundaries to know:

- **Requires a `FolderScope`, hard error otherwise.** Without a scope,
  the legacy single-target file tools stay in the toolset (only a scope
  strips them), so the model could read the virtual file with `fs_read`
  and overwrite the REAL one with `write` in the same turn. `Run` fails
  before any provider traffic when `DiskProvider` is set with an empty
  `FolderScopes`.
- **Cannot combine with a scope that keeps command tools, hard error
  otherwise.** A scope that keeps `bash` (`RestrictedRun` with bash
  patterns granted) would let `bash` see the real disk while `fs_*` sees
  the virtual one in the same turn — a split the model has no way to
  know about, since the prompt does not change for a disk-provider call.
- **The trust boundary.** The provider is YOUR own in-process Go code,
  running in the same address space with the same OS privileges as
  Rush. It is a *redirection* mechanism, not a sandbox: it does not
  confine `bash`, does not confine anything the provider itself chooses
  to do, and is not a defence against a hostile model that has other
  tools. The `FolderScope` ACL is still what limits which paths the
  model may name.
- **Per-call provider identity.** A run without an explicit provider is
  never assumed safe to continue an earlier provider-backed session. A
  provider-backed SDK run persists a session execution requirement (the
  fence) at admission; same-process continuations (durable queued turns,
  drains/wakes, reviewer follow-ups) restore the exact binding, never
  "the latest provider for the session". The same-process root
  drain/wake continuation ALSO restores the persisted folder-scope/command-tool
  and restricted-run baseline from the fence, so the continued turn is judged
  by the same policy the fenced run was admitted under.
- By default, SDK runs use the batch execution runtime. Rush borrows the provider and does not close it. Provider-backed runs retain a process-bound provider binding until associated work terminates; a process restart cannot restore that in-memory capability, so provider-dependent continuations fail closed with `sdk.ErrDiskProviderUnavailable` rather than falling back to the OS filesystem. Keep it usable until
  associated executions—including detached work and held delegations—
  have actually terminated and disposed. Cancellation requests are not
  proof that provider operations stopped; SDK Run/wait returning does
  not end detached work. Concurrent calls may overlap, so providers
  need concurrency safety. Crashes never resume old provider-backed
  work: after a restart, provider-dependent continuations fail closed
  with `sdk.ErrDiskProviderUnavailable` — terminally for durable rows,
  never replayed onto the OS disk — and a new SDK run supplying a
  provider establishes a new epoch. Ordinary turns and unmarked
  continuations of a fenced session are refused, never downgraded to
  the real disk. FolderScope and command-tool safety floors above
  remain in force.
- **An ephemeral library-mode session (see "Library mode" above) has no
  "real disk" to fall back to at all.** With no `WorkingDir`, every
  legacy file/command tool listed above as "NOT affected" is simply
  absent from the toolset by default — there is nothing for them to
  touch — and no later layering (a worker sub-agent's toolset, a folder
  scope's re-adds) can bring them back: the session's config carries an
  immutable no-real-workspace capability that `internal/app` and
  `internal/agent` both enforce as a floor (SDK review round 14). A
  `FolderScopes` override with no `DiskProvider` (or with the real
  `OSDiskProvider()`) is a hard error before any provider traffic —
  relative and absolute scope entries alike: an ephemeral session has
  no real filesystem to resolve a scope against. The internal
  `sdk.LibraryVirtualRoot` sentinel guard remains as defense-in-depth
  for direct internal callers only. Note `sdk.LibraryVirtualRoot` is a
  FUNCTION since round 14 (R14-3) — it was previously an exported
  `var`, a plain copy of the internal value an embedder could reassign,
  diverging it from the guard's copy; assigning to it is now a compile
  error. Supplying your own `DiskProvider` alongside `FolderScopes` is
  still the only way an ephemeral session's model gets file access at
  all.

## Session-busy behaviour is different from `rush run`

`rush run` and the web server intentionally **queue** a second message
sent to a session that already has a turn in flight (this is what makes
"type a follow-up while the agent is still answering" work in the web
UI). `Client.Run` and `RunWithCredentials` do **not** queue: a second
concurrent call on the *same* `ContinueSessionID` fails immediately with
an error wrapping `sdk.ErrSessionBusy` (check with `errors.Is`). Two
calls on *different* session ids remain fully concurrent and unaffected.

## Reading what the agent wrote

- **`RunResult.FinalText`** — the turn's last assistant message only.
- **`Client.SubscribeMessages(ctx)`** / **`SubscribeSessions(ctx)`** —
  live event streams (create/update/delete) across every session the
  Client knows about. Subscribe *before* starting the call you care
  about: the channel carries no backlog, so events that happened before
  you subscribed are gone.
- **`Client.Messages(ctx, sessionID)`** — the full message history of a
  session, in chronological order, on demand, *after* a call has already
  returned. This is the only way to retrieve history if you didn't
  subscribe in advance — and for an ephemeral in-memory session (see
  above), it is the *only* way to see history at all, since nothing
  persists to query later. Call it before `Close()`: once a graceful
  `Close()` has released the in-memory handles, an ephemeral session's
  data is gone for good (a forced close keeps the data alive until you
  reclaim the handles — see "Shutting down").
- **`Client.Session(ctx, sessionID)`** — a session's metadata (title,
  token/cost counters), not its messages.

## Origin — where a session or message came from

Every session and every individual user message carries an `Origin`
(`sdk.OriginCLI` / `sdk.OriginWeb` / `sdk.OriginSDK`), set once at
creation. `Client.Run` and `RunWithCredentials` tag both with
`OriginSDK` by default — set `RunRequest.Origin` explicitly before
calling if you need something else. Session origin and message origin
are independent: a session opened through the SDK can still receive a
message injected later through `rush sessions inject` (CLI), and each
message keeps the origin of whatever actually created it.

## Shutting down

```go
defer client.Close()
```

`Close()` is idempotent — calling it more than once is safe and always
returns the first call's result. It runs in three ordered phases:

The exported zero-value `Client` is also safe to close. After `Close()` it
rejects all error-returning methods with `sdk.ErrClientClosed` and returns
already-closed channels from the subscription methods.

1. **Admission closes.** From the instant `Close()` is called, new
   `Run`, `RunWithCredentials`, `Messages`, and `Session` calls return
   `sdk.ErrClientClosed`, and the Subscribe methods return an
   already-closed channel.
2. **Drain, cancelling on stall.** The calls admitted before that
   instant get one grace period (`agent.DefaultCancelAllGrace`, five
   seconds) to finish against the fully live App — a call that finishes
   inside the window is never cancelled. Work still running when the
   window expires is cancelled immediately, while every resource is
   still open: a run stuck on a non-cancellable provider or tool call
   unwinds here instead of blocking `Close()` forever. A run that
   unwinds once cancelled keeps the shutdown graceful; work that
   ignores cancellation entirely makes the shutdown forced, and
   `Close()` stops waiting — it returns within a bounded time (at most
   a couple of grace windows) instead of hanging. "Work" here is
   broader than the calls you admitted: even a drain that finishes
   cooperatively still runs one round of cancellation to join
   background agent work (session title generation, cache keep-alive
   replays) that no call was blocked on, and the run-queue pump is
   stopped with its own grace — either of those still busy after its
   grace period forces the shutdown too. After agent work has
   fully unwound, the residual wait for any remaining non-agent calls
   (a `Messages` read, which cancellation cannot reach) is unbounded: a
   host that needs a total bound should cancel the contexts it handed
   to its own in-flight calls.
3. **Release.** Bounded parallel cleanup always; the database — and, on
   an ephemeral client, the in-memory handles — only if the shutdown
   was graceful.

`CloseResult.Forced` is therefore `true` only when something was
*still busy after cancellation* — and that something is not limited to
the calls you had in flight: background agent work (session title
generation, cache keep-alive replays) that joined too late, or a
run-queue pump worker that ignored its own grace, forces the shutdown
with every admitted call long gone. A stuck-but-cooperative run produces a
graceful result: it is cancelled, it unwinds, and its state is flushed
before anything is released. A forced result means the database was
deliberately **not** released (closing it under a live writer risks
corruption), and on an ephemeral client the in-memory handles were
deliberately left open too: the session data is still alive under
those handles, held by the writer that refused to die. Once the host
knows every writer has finished, it reclaims the handles — and with
them the held memory — with `CloseEphemeralConnsForced()`; from that
call on, the ephemeral data is gone for good.
`CloseEphemeralConnsForced()` refuses to run until `Close()` has
*finished* — not merely started: while `Close()` is still draining,
calls it admitted earlier are still executing against the in-memory
handles, and a reclaim inside that window would close the database
under them. Before `Close()` is called at all the same refusal
applies, because the still-open Client would admit new calls against
closed handles. It is a no-op after a graceful close. `CloseResult.CleanupErrors` collects any non-fatal errors from
cleanup (also logged internally).

After `Close()` returns, the Client is permanently closed: `Run`,
`RunWithCredentials`, `Messages`, and `Session` return
`sdk.ErrClientClosed` (check with `errors.Is`), and `SubscribeMessages` /
`SubscribeSessions` return an already-closed channel. `Close()` itself
stays idempotent — the second call just returns the first call's result.

## Minimal example

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/PHPCraftdream/rush/sdk"
)

func main() {
    ctx := context.Background()

    client, err := sdk.Open(ctx, sdk.Options{WorkingDir: "./my-project"})
    if err != nil {
        log.Fatal(err)
    }
    defer client.Close()

    result, err := client.Run(ctx, sdk.RunRequest{
        Prompt: "list the TODO comments in this repo",
    })
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(result.FinalText)
}
```

## v1 boundaries

A few embedding limitations are stated honestly in `sdk.go`'s package
doc comment and worth repeating here:

- **Application-mode MCP ownership is two-tier.** The first
  application-mode `Open` in a process takes the process-wide MCP owner
  exactly as before; a second application-mode `Open` reserves a standalone
  owner instead of failing with `mcp.ErrOwnerBusy`, so two application-mode
  Clients coexist — each routing its MCP operations through its own owner,
  with independent initialization barriers and shutdown. The registry
  underneath is still one name-keyed process table, so the Clients must keep
  their server configs disjoint: a server configured in the first client is
  never visible to the second's surfaces, but the same server name in both
  clients would share one registry entry. Library mode never starts MCP
  servers or acquires an owner, so it can coexist with any application-mode
  Client; multiple simultaneous library-mode Clients are supported and
  tested — each ephemeral client gets its own isolated in-memory database.
- **The host's logger is untouched unless you opt in** via
  `Options.SetupLogging` — and that call is itself a process-wide
  singleton, so only the first `Open` with `SetupLogging: true` in a
  process wins.
- Rush's own internal code still logs through package-level
  `slog.Info`/`Warn`/`Error` in many places; those lines go wherever the
  host process's current `slog.Default()` points, regardless of
  `SetupLogging`.
