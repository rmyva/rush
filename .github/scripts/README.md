# .github/scripts — active tiered CI selector and gate helpers

`ci_plan.py` implements package selection from `--base`/`--head` SHAs: it diffs merge-base..head, maps changed files to Go packages, builds reverse-dependency closures over per-OS `go list` graphs, and emits package selections, race selection, native-required prefixes, fallback reasons, and `not_run` entries. The selector and gate unit tests run in the `checks` job.

## Files

- `ci_plan.py` — selector CLI.
- `ci_plan/` — stdlib-only implementation package.
- `ci_gate_decide.py` — plan preparation, gate decisions and cancellation helpers.
- `../ci-policy.toml` — selector thresholds and path lists.

## Run

```sh
python -I -m unittest discover -s .github/scripts
python .github/scripts/ci_plan.py --base <sha> --head <sha> --dry-run
```

The active plan/check orchestration is `.github/workflows/ci.yml`: `plan` always selects work and `checks` runs helper unit tests. There is no separate active shadow planning workflow. Prepared plans always export `heavy_allowed=true`; there is no cheap/minimal-plan bypass. Full mode is requested by a main-bound PR's `full-ci` label, an exact `CI: full` commit-message line, a manual `Actions` → `ci` → `Run workflow` dispatch with `full` checked, or a push to `main`. Example: `git commit -m "fix: x" -m "CI: full"`. A directive anywhere in the PR's commit range adds full validation only and persists until that commit is removed or rewritten. Selector prefixes/fanout and guard files still choose automatic work; the run title remains quick for directive/selector escalations. CI cutover rollback is one revert of the eventual cutover commit; its exact hash is supplied by the orchestrator. Historical references to `ci-plan.yml` identify a retired workflow restored only by that rollback.
