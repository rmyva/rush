#!/usr/bin/env python3
"""Assign Go packages to CI shards (.github/ci-shards/shards.toml).

Stdlib-only, Python 3.11+ (tomllib).

Modes:
  assign (default)
      python assign_shards.py --shards shards.toml --input packages.txt
      Emits the assignment JSON to stdout. Input is one import path per line;
      `--input -` reads stdin. Nonzero exit = a package was not assigned
      exactly once (this IS the "every package exactly once" assertion).
  --check-coverage --workflow ci-windows-shards.yml --assignment assignment.json
      Verifies active group steps and manifest groups in both directions against
      shards.toml's [windows_leg] table and generated assignment JSON.

Assignment rules:
  * "internal/agent" (agent_isolated) is EXACT match only, and no shard
    group may use the bare prefix "internal/agent" (rejected outright).
  * Shards match by exact-or-subdirectory prefix, first matching group in
    shards.toml order. A package matching more than one group is an error.
  * Shard D's catch_all group gets the sorted remainder. A stale prefix that
    matches no input package is a stderr warning, not an error (removing a
    package must not break CI); a DUPLICATE prefix across groups is an error.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
import tomllib

WINDOWS_GROUP_STEP_RE = re.compile(r'^\s*- name:\s*"?((?:test: |go mod tidy|go test ./internal/agent/)[^"\r\n]*)(?:"\s*)?$')
SPECIAL_TARGETS = {"agent", "plan", "all"}


def die(msg: str) -> None:
    print(f"assign_shards: error: {msg}", file=sys.stderr)
    sys.exit(1)


def rel_path(module: str, pkg: str, lineno: int) -> str:
    if pkg == module:
        return ""
    if pkg.startswith(module + "/"):
        return pkg[len(module) + 1 :]
    die(f"input line {lineno}: package {pkg!r} is not under module {module!r}")


def prefix_matches(rel: str, prefix: str) -> bool:
    # "" (module root package) only matches the "" prefix exactly.
    return rel == prefix or (prefix != "" and rel.startswith(prefix + "/"))


def load_shards(path: str) -> dict:
    with open(path, "rb") as f:
        return tomllib.load(f)


def all_groups(cfg: dict) -> list[tuple[str, dict]]:
    """Flatten [(shard_letter, group_dict)] in TOML order; also validate."""
    out: list[tuple[str, dict]] = []
    seen_prefixes: dict[str, str] = {}
    for letter, shard in cfg.get("shards", {}).items():
        for g in shard.get("groups", []):
            if "name" not in g:
                die(f"shard {letter}: group without a name")
            for p in g.get("packages", []) or []:
                if p == "internal/agent":
                    die(
                        f"shard {letter} group {g['name']!r} uses bare prefix "
                        f"'internal/agent'; that package is agent-isolated "
                        f"(exact match only) and no shard group may claim it"
                    )
                if p in seen_prefixes:
                    die(
                        f"prefix {p!r} is listed in both shard "
                        f"{seen_prefixes[p]} and shard {letter} (duplicate)"
                    )
                seen_prefixes[p] = letter
            out.append((letter, g))
    return out


def assign(cfg: dict, packages: list[str]) -> dict:
    module = cfg.get("module")
    if not module:
        die("shards.toml: missing 'module'")
    agent_exact = set(cfg.get("agent_isolated", {}).get("packages", []))
    groups = all_groups(cfg)
    catch_all = [
        (s, g)
        for s, g in groups
        if g.get("catch_all")
    ]

    matched_prefixes: set[str] = set()
    group_pkgs: dict[int, list[str]] = {}
    agent_list: list[str] = []
    remainder: list[str] = []

    for lineno, pkg in enumerate(packages, 1):
        rel = rel_path(module, pkg, lineno)
        if rel in agent_exact:
            agent_list.append(pkg)
            continue
        hits = [
            (s, g)
            for s, g in groups
            if any(prefix_matches(rel, p) for p in g.get("packages", []) or [])
        ]
        if len(hits) > 1:
            die(
                f"package {pkg} double-assigned to groups: "
                + ", ".join(f"{s}/{g['name']}" for s, g in hits)
            )
        if not hits:
            if catch_all:
                remainder.append(pkg)
            else:
                die(f"package {pkg} is unassigned and no catch-all group exists")
            continue
        s, g = hits[0]
        for p in g.get("packages", []) or []:
            if prefix_matches(rel, p):
                matched_prefixes.add(p)
                break
        group_pkgs.setdefault(id(g), []).append(pkg)

    for s, g in groups:
        for p in g.get("packages", []) or []:
            if p not in matched_prefixes:
                print(
                    f"assign_shards: warning: prefix {p!r} "
                    f"(shard {s}, group {g['name']!r}) matches no input package",
                    file=sys.stderr,
                )

    out_shards: dict[str, dict] = {k: {"groups": []} for k in cfg.get("shards", {})}
    for s, g in groups:
        entry = {"name": g["name"], "type": g.get("type", "go")}
        if g.get("timeout"):
            entry["timeout"] = g["timeout"]
        pkgs = sorted(group_pkgs.get(id(g), []))
        if g.get("catch_all"):
            pkgs = sorted(remainder)
        entry["packages"] = pkgs
        out_shards[s]["groups"].append(entry)

    counts = {
        "agent": len(agent_list),
        **{
            s: sum(len(g["packages"]) for g in gs["groups"])
            for s, gs in out_shards.items()
        },
        "total": len(packages),
    }
    return {
        "module": module,
        "shards": out_shards,
        "agent": sorted(agent_list),
        "unassigned": [],
        "counts": counts,
    }


def check_coverage(cfg: dict, workflow: str, assignment_path: str) -> None:
    table = cfg.get("windows_leg")
    if not table:
        die("shards.toml: missing [windows_leg] coverage table")
    names: list[str] = []
    try:
        with open(workflow, encoding="utf-8") as f:
            for line in f:
                m = WINDOWS_GROUP_STEP_RE.match(line)
                if m:
                    names.append(m.group(1))
    except OSError as exc:
        die(f"cannot read workflow {workflow!r}: {exc.strerror or exc}")
    if not names:
        die(f"workflow {workflow!r} contains no recognized Windows group steps")

    with open(assignment_path, encoding="utf-8") as f:
        assignment = json.load(f)

    missing = [n for n in names if n not in table]
    if missing:
        die("ci-windows-shards.yml group steps missing from [windows_leg]: " + ", ".join(missing))

    stale = []
    for step, target in table.items():
        if target in SPECIAL_TARGETS:
            continue
        if "/" not in target:
            stale.append(f"{step!r} -> {target!r} (expected 'shard/group')")
            continue
        s, gname = target.split("/", 1)
        groups = assignment.get("shards", {}).get(s, {}).get("groups", [])
        if not any(g.get("name") == gname for g in groups):
            stale.append(f"{step!r} -> {target!r} (no such group in assignment)")
    if stale:
        die("[windows_leg] targets not present in assignment: " + "; ".join(stale))

    # Reverse validation follows [windows_leg] targets, not group-name equality:
    # workflow step labels may differ from manifest group names.
    workflow_names = set(names)
    reverse_missing = []
    assignment_shards = assignment.get("shards", {})
    for shard_name, shard in cfg.get("shards", {}).items():
        assigned_groups = {
            g.get("name") for g in assignment_shards.get(shard_name, {}).get("groups", [])
        }
        for group in shard.get("groups", []):
            group_name = group.get("name")
            if group_name not in assigned_groups:
                reverse_missing.append(f"{group_name!r} missing from assignment shard {shard_name}")

    # Each generated assignment group must resolve through its shard/group
    # target to at least one active workflow step.
    for shard_name, shard in assignment_shards.items():
        for group in shard.get("groups", []):
            group_name = group.get("name")
            target = f"{shard_name}/{group_name}"
            steps = [step for step, mapped in table.items() if mapped == target]
            if not steps:
                reverse_missing.append(f"{group_name!r} has no [windows_leg] target {target!r}")
            elif not any(step in workflow_names for step in steps):
                reverse_missing.append(
                    f"{group_name!r} target {target!r} has no active workflow step"
                )
    if reverse_missing:
        die("shards.toml groups not fully covered: " + "; ".join(sorted(reverse_missing)))

    print(
        f"assign_shards: coverage OK: {len(names)} ci-windows-shards.yml "
        f"group steps mapped, {len(table)} table entries all resolve"
    )


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--shards", required=True, help="path to shards.toml")
    ap.add_argument("--input", help="package list file, one per line; '-' for stdin")
    ap.add_argument("--check-coverage", action="store_true")
    ap.add_argument("--workflow", help="path to ci-windows-shards.yml (with --check-coverage)")
    ap.add_argument("--assignment", help="path to assignment JSON (with --check-coverage)")
    args = ap.parse_args()

    cfg = load_shards(args.shards)

    if args.check_coverage:
        if not args.workflow or not args.assignment:
            die("--check-coverage requires --workflow and --assignment")
        check_coverage(cfg, args.workflow, args.assignment)
        return

    if not args.input:
        die("--input is required (file path or '-')")
    if args.input == "-":
        lines = sys.stdin.read().splitlines()
    else:
        with open(args.input, encoding="utf-8") as f:
            lines = f.read().splitlines()
    packages = [ln.strip() for ln in lines if ln.strip()]

    json.dump(assign(cfg, packages), sys.stdout, sort_keys=True)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
