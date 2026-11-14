# `rush result` — reading a run's result envelope

`rush run --json` writes a JSON result envelope. `rush result <session-id|path>`
prints that envelope back in a form you can actually use, and
`rush run --findings-from <session-id|path>` feeds a previous run's review
findings into the next run without manual copy-paste.

## Argument resolution

The argument is either:

- a **session id**, looked up as `<main-checkout>/.rush/stdin/<id>.out` (the
  file `rush run --worktree` writes). The main checkout root is found via
  `git rev-parse --git-common-dir`, so id lookup also works from inside a
  linked git worktree; or
- a **direct path** to an envelope file. A direct path that exists always wins.

## Output modes

```
rush result s1-review                    # human-readable text
rush result s1-review --review           # verdict + review only
rush result s1-review --field final_text # raw final_text for scripting
rush result s1-review --json | jq .exit_reason
rush result /abs/path/envelope.out       # explicit file
```

- default: readable text — session, exit reason, error (when present), final
  text (with a `(empty)` body marker when empty), review verdict + review
  (when present), warnings, and brief usage/duration.
- `--json`: the envelope bytes written to stdout **exactly as stored** — no
  re-marshalling, no trailing-newline fixup.
- `--review`: only the reviewer verdict + review text. Empty output when the
  envelope has neither (still exit 0).
- `--field <name>`: one field's value. Strings are printed raw (no quotes,
  single trailing newline) so they can be piped or redirected without
  unescaping; other types are printed as JSON. An absent field is an error.

## UTF-8 on Windows

Output is always raw UTF-8 bytes on stdout regardless of the Windows console
code page: Go performs no ANSI/OEM transcoding on `os.Stdout`, so Cyrillic and
any other Unicode pass through byte-exact. This is the main reason to prefer
`rush result` over script-side encoding workarounds on the shell output.

## Exit codes (`rush result`)

| Code | Meaning |
| --- | --- |
| 0 | envelope read |
| non-zero | envelope not found, or the file is not a valid JSON object (clear message; trailing garbage is not tolerated) |

The command needs no database, session store or network, and writes nothing
to disk.

## Envelope field reference

| Field | Meaning |
| --- | --- |
| `session_id` | session the run executed in |
| `exit_reason` | why the run ended (`stop`, `end_turn`, `error`, `timeout`, `canceled`, `stalled`, `invalid_json`, `awaiting_answer`, `queued`, `max_tokens`, `provider_limit`, `peak_hours` (peak-hours pause, resumable), ...) |
| `final_text` | the run's final answer text |
| `review` | the automatic reviewer's review text (omitted when absent) |
| `review_verdict` | reviewer verdict: `pass`, `pass_with_notes`, `fail`, `unverified`, `unparsed` or `error`; empty when no reviewer pass ran |
| `assistant_notes` | original unstripped answer kept when the fence/preamble stripper rewrote it (omitted when absent) |
| `error` | error text when the run failed (omitted when absent) |
| `warnings` | non-fatal warnings collected during the run |
| `tool_calls` | per-tool call statistics |
| `usage` | token usage information |
| `duration_ms` | total run duration in milliseconds |

## Where envelope files live

- Shell-redirect convention: redirect `rush run --json` stdout to
  `.rush/stdin/<task>.out` yourself (set `RUSH_FORBID_WRITES` to the target
  first — see the README).
- `rush run --worktree` stores the envelope at
  `<main>/.rush/stdin/<session>.out` automatically (see the README for
  `--worktree` itself).

## `rush run --findings-from <session-id|path>`

Before sending the prompt, this flag appends a block titled
`## Findings from previous review` **after** the user's prompt, containing the
referenced envelope's `review_verdict` (as `verdict: ...`), the `review` text,
and the `warnings` (as `- ...` lines). When the envelope has none of the
three, the block says `(none recorded in the envelope).`

The value resolves exactly like a `rush result` argument. A missing or invalid
envelope **fails the run before it starts** (clear error, nothing executed).
Without the flag, `rush run` behavior is byte-identical to before.

### Typical loop

```sh
rush run --role smart --session t1 --json "review the diff"   # (with --worktree, the envelope lands in <main>/.rush/stdin/t1.out)
rush result t1 --review                                       # read the verdict
rush run --role smart --session t2 --findings-from t1 "fix every finding"
```

## `rush run --fail-on-review`

A `review_verdict: "fail"` still exits 0 by default (only a stderr line says
so), so background completion notifications look successful. The optional
`--fail-on-review` flag changes this: when the run itself completed
successfully and the automatic reviewer's verdict is exactly `"fail"`, the
process exits with the dedicated code **3**. A stderr message names the flag,
the verdict and the code, and points at `rush result <session> --review`.

- `pass` and `pass_with_notes` stay 0; other verdicts (`unverified`,
  `unparsed`, `error`) and a run with no reviewer pass stay 0.
- A run that itself failed keeps its own exit code — the flag never masks a
  real failure.
- The stdout envelope is byte-identical in every case; only the process exit
  code changes.
- Without the flag, `rush run` behavior is completely unchanged.

### `rush run` exit codes

| Code | Meaning |
| --- | --- |
| 0 | success (including review-fail without the flag, and `pass`/`pass_with_notes` with it) |
| 1 | generic run error |
| 3 | review verdict `"fail"` with `--fail-on-review` (public contract for wrapper scripts; `rush ping` uses its own separate code 3 for "degraded" — different subcommand) |
| 124 | timeout hard backstop force-kill |
