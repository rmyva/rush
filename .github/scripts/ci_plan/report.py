"""Plan JSON and markdown summary rendering."""

from __future__ import annotations

import json

from .selector import Plan

SCHEMA_VERSION = 1


def plan_json(p: Plan) -> str:
    doc = {
        "schema_version": SCHEMA_VERSION,
        "base": p.base,
        "head": p.head,
        "merge_base": p.merge_base,
        "mode": p.mode,
        "per_os": {
            os_name: [
                {"package": e.package, "reasons": e.reasons} for e in p.selected[os_name]
            ]
            for os_name in ("linux", "darwin", "windows")
        },
        "race_packages": p.race,
        "native_required": p.native_required,
        "fallbacks": p.fallbacks,
        "not_run": p.not_run,
    }
    return json.dumps(doc, indent=2, sort_keys=False)


def summary_md(p: Plan) -> str:
    lines: list[str] = []
    lines.append("# CI plan")
    lines.append(f"- mode: **{p.mode}**")
    lines.append(f"- base: `{p.base}`")
    lines.append(f"- head: `{p.head}`")
    lines.append(f"- merge_base: `{p.merge_base}`")
    lines.append(f"- native_required: **{p.native_required['required']}**")
    for r in p.native_required["reasons"][:10]:
        lines.append(f"  - {r}")
    if len(p.native_required["reasons"]) > 10:
        lines.append(
            f"  - …and {len(p.native_required['reasons']) - 10} more native reasons"
        )
    lines.append("")
    lines.append("## Selected packages per OS")
    for os_name in ("linux", "darwin", "windows"):
        lines.append(f"- {os_name}: {len(p.selected[os_name])} package(s)")
    lines.append(f"- race (linux): {len(p.race)} package(s)")
    if p.race:
        for pkg in p.race:
            lines.append(f"  - {pkg}")
    lines.append("")
    if p.fallbacks:
        lines.append("## Full-fallback reasons")
        for r in p.fallbacks:
            lines.append(f"- {r}")
        lines.append("")
    else:
        lines.append("## Full-fallback reasons\n- none\n")
    highlights = [
        e
        for e in p.not_run
        if e["reason"] != "not selected"
    ]
    lines.append("## Not run (highlights)")
    if not highlights:
        lines.append("- none beyond unselected packages")
    for e in highlights[:40]:
        lines.append(f"- {e['os']}/{e['package_or_suite']}: {e['reason']}")
    if len(highlights) > 40:
        lines.append(f"- …and {len(highlights) - 40} more")
    lines.append("")
    lines.append("## Unselected package counts")
    for os_name in ("linux", "darwin", "windows"):
        n = sum(1 for e in p.not_run if e["os"] == os_name and e["reason"] == "not selected")
        lines.append(f"- {os_name}: {n} package(s) not selected")
    lines.append("")
    return "\n".join(lines)
