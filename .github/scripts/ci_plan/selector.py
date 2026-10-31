"""Changed-package selection (design section 2, steps 1-15)."""

from __future__ import annotations

import os
import posixpath
from dataclasses import dataclass, field

from . import golist, policy as pol

TARGET_OSES = golist.TARGET_OSES


class SelectionError(Exception):
    pass


@dataclass
class PkgSelection:
    package: str
    reasons: list[str] = field(default_factory=list)


@dataclass
class Plan:
    mode: str = "tier1"
    base: str = ""
    head: str = ""
    merge_base: str = ""
    selected: dict[str, list[PkgSelection]] = field(default_factory=dict)
    race: list[str] = field(default_factory=list)
    native_required: dict = field(
        default_factory=lambda: {"required": False, "reasons": []}
    )
    fallbacks: list[str] = field(default_factory=list)
    not_run: list[dict] = field(default_factory=list)
    totals: dict = field(default_factory=dict)


@dataclass
class _ChangeOwner:
    dirs: set[str] = field(default_factory=set)  # owning package dirs
    production: bool = False
    test_only: bool = False
    unknown: bool = False
    files: set[str] = field(default_factory=set)


def _file_dir(path: str) -> str:
    d = posixpath.dirname(path.replace("\\", "/"))
    return d


def _dir_owner(graph: golist.Graph, fdir: str) -> str | None:
    """Longest package dir that is a prefix of fdir (membership by dir)."""
    cur = fdir
    while True:
        owner = graph.dirs.get(cur)
        if owner is not None:
            return owner
        if cur == "":
            return None
        cur = posixpath.dirname(cur)


def _go_native_evidence(fname: str, polobj: pol.Policy) -> bool:
    stem = fname[:-3] if fname.endswith(".go") else fname
    for suf in polobj.native_suffixes:
        if stem.endswith(suf):
            return True
    return False


def _longest_dir(dirs: set[str], fdir: str) -> str | None:
    """Longest key in `dirs` that is fdir or an ancestor of fdir."""
    cur = fdir
    while True:
        if cur in dirs:
            return cur
        if cur == "":
            return None
        cur = posixpath.dirname(cur)


def _owner_changes(repo, graphs: dict, polobj: pol.Policy) -> dict[str, _ChangeOwner]:
    """Map each changed package DIR to its change classification (per spec:
    test-only changes select the owning package; production/generated changes
    select owner + transitive reverse deps)."""
    owners: dict[str, _ChangeOwner] = {}
    union_dirs: set[str] = set()
    for g in graphs.values():
        union_dirs.update(g.dirs)
    base_go_dirs = {
        _file_dir(f)
        for f in repo.base_tree_files
        if f.endswith(".go") and not f.startswith("vendor/")
    }

    def entry(d: str) -> _ChangeOwner:
        return owners.setdefault(d, _ChangeOwner())

    for ch in repo.changes:
        paths = [ch.path]
        if ch.old_path:
            paths.append(ch.old_path)
        for path in paths:
            fdir = _file_dir(path)
            fname = posixpath.basename(path)
            if path.endswith(".go"):
                # Membership by directory prefix; tagged/generated variants
                # match by dir. Deleted/unknown dirs keep their own dir so the
                # base tree and consumer lookup can still run.
                d = _longest_dir(union_dirs, fdir)
                if d is None:
                    d = _longest_dir(base_go_dirs, fdir)
                owner_dir = d if d is not None else fdir
                e = entry(owner_dir)
                e.files.add(path)
                if fname.endswith("_test.go"):
                    e.test_only = True
                else:
                    e.production = True
            else:
                mapped: set[str] = set()
                for g in graphs.values():
                    for pkg in g.packages.values():
                        if path in pkg.embeds:
                            mapped.add(pkg.dir)
                if "/testdata/" in (fdir + "/"):
                    for g in graphs.values():
                        d = _longest_dir(set(g.dirs), fdir)
                        if d is not None:
                            mapped.add(d)
                mapped.update(pol.map_non_go(path, polobj.non_go_mappings))
                if mapped:
                    for d in mapped:
                        e = entry(d)
                        e.files.add(path)
                        e.production = True
                else:
                    e = entry(fdir)
                    e.unknown = True
                    e.files.add(path)
    for e in owners.values():
        if e.production or e.unknown:
            e.test_only = False
    return owners


def plan(
    root: str,
    repo,
    graphs: dict[str, golist.Graph],
    polobj: pol.Policy,
    native_suffix_check=_go_native_evidence,
) -> Plan:
    p = Plan(
        base=repo.base,
        head=repo.head,
        merge_base=repo.merge_base,
    )
    linux_graph = graphs["linux"]
    module_packages = list(linux_graph.packages.values())

    owners = _owner_changes(repo, graphs, polobj)

    def resolve_owner(d: str) -> str | None:
        """Accept a repo dir or an import path (e.g. policy mappings)."""
        for g in graphs.values():
            cand = g.dirs.get(d)
            if cand is not None:
                return cand
            if d in g.packages:
                return d
        return None

    def repo_rel(imp: str) -> str:
        for g in graphs.values():
            d = golist._repo_dir(g.module, imp)
            if d is not None:
                return d
        return imp

    # Seed set: package owners; production changes add reverse deps later.
    seeds: set[str] = set()
    production_seeds: set[str] = set()
    unknown_paths: list[str] = []
    deleted_dirs: list[str] = []

    for d, ch in sorted(owners.items()):
        owner = resolve_owner(d)
        if owner is None:
            # Directory is not a package in any head graph (deleted package or
            # unknown ownership). Consult base tree: was it a Go package there?
            base_has_go = any(
                f.startswith((d + "/") if d else "")
                and f.endswith(".go")
                for f in repo.base_tree_files
            )
            if base_has_go or any(ch.files):
                deleted_dirs.append(d)
                # Surviving consumers: reverse closure from the deleted import
                # path, matched by dir across the union graph.
                for imp in {g.module + ("/" + d if d else "") for g in graphs.values()}:
                    production_seeds.add(imp)
                for f in sorted(ch.files):
                    p.not_run.append(
                        {
                            "os": "all",
                            "package_or_suite": d or ".",
                            "reason": f"deleted package (changed file {f}); consumers still selected",
                        }
                    )
            if ch.unknown:
                unknown_paths.extend(sorted(ch.files))
            continue
        seeds.add(owner)
        if ch.production:
            production_seeds.add(owner)
        if ch.unknown:
            unknown_paths.extend(sorted(ch.files))

    if unknown_paths:
        p.fallbacks.append(
            "unknown ownership: " + ", ".join(sorted(set(unknown_paths))[:10])
        )
    if len(deleted_dirs) > 1:
        p.fallbacks.append("deleted-owner ambiguity: " + ", ".join(sorted(deleted_dirs)[:10]))

    # Dependency changes force full.
    dep_files = [
        ch.path
        for ch in repo.changes
        if posixpath.basename(ch.path) in ("go.mod", "go.sum", "go.work.sum")
        or ch.path.startswith("go.work")
        or posixpath.basename(ch.path).startswith("go.work")
        or "toolchain" in posixpath.basename(ch.path)
    ]
    if dep_files:
        p.fallbacks.append(
            "dependency/toolchain changes: " + ", ".join(sorted(set(dep_files))[:10])
        )

    # Race roots: any change (production or test-only) in a package under
    # race_roots requires native lanes (concurrency-sensitive flakes).
    race_native: list[str] = []
    for d in owners:
        if pol.under_prefix((d + "/") if d else "internal/", polobj.race_roots):
            race_native.append(
                f"changed package {d} is under race_roots: concurrency-sensitive tests run on native lanes"
            )

    # Guard infrastructure: changes here always run guards (full mode).
    guard_hits = []
    for ch in repo.changes:
        for path in ([ch.path, ch.old_path] if ch.old_path else [ch.path]):
            if pol.under_prefix(path, polobj.always_run):
                guard_hits.append(path)
    if guard_hits:
        p.fallbacks.append(
            "always-run guard files changed: " + ", ".join(sorted(set(guard_hits))[:10])
        )

    # Selector policy can explicitly promote matching changes to full mode.
    selector_full_hits = sorted({
        path
        for ch in repo.changes
        for path in ([ch.path, ch.old_path] if ch.old_path else [ch.path])
        if any(pol.glob_match(path, pattern) for pattern in polobj.selector_full_patterns)
    })
    if selector_full_hits:
        p.fallbacks.append(
            "selector full patterns changed: " + ", ".join(selector_full_hits[:10])
        )

    # Core prefix production change -> full fallback
    # Core prefix blanket rule (production only).
    core_hits = []
    for d, ch in owners.items():
        if not ch.production:
            continue
        path = d + "/" if d else "internal/"
        if pol.under_prefix(path, polobj.core_prefixes):
            core_hits.append(d)
    if core_hits:
        p.fallbacks.append(
            "production changes under core prefixes: " + ", ".join(sorted(core_hits)[:10])
        )

    # Reverse closure over the union graph, then retain per-OS validity.
    closure_imports = golist.reverse_closure(list(graphs.values()), production_seeds)
    selected_imports = set(seeds) | closure_imports

    fanout_count = 0
    for os_name in TARGET_OSES:
        g = graphs[os_name]
        entries: list[PkgSelection] = []
        for imp in sorted(selected_imports):
            pkg = g.packages.get(imp)
            if pkg is None or imp not in selected_imports:
                continue
            reasons: list[str] = []
            d, ch = None, None
            for dd, cc in owners.items():
                if g.dirs.get(dd) == imp:
                    d, ch = dd, cc
                    break
            if ch is not None:
                reasons.append("changed package" + (" (test-only edit)" if ch.test_only else ""))
            else:
                reasons.append("reverse dependency of changed package")
            entries.append(PkgSelection(imp, reasons))
        p.selected[os_name] = entries

    fanout_count = len(p.selected["linux"])
    total = max(len(module_packages), 1)
    if fanout_count > polobj.fanout_max_packages:
        p.fallbacks.append(
            f"fan-out {fanout_count} > {polobj.fanout_max_packages} packages"
        )
    if fanout_count / total > polobj.fanout_max_ratio:
        p.fallbacks.append(
            f"fan-out ratio {fanout_count}/{total} > {polobj.fanout_max_ratio:.0%} of module packages"
        )

    p.mode = "full" if p.fallbacks else "tier1"
    if p.mode == "full":
        # Full Ubuntu: every module package on linux.
        p.selected["linux"] = [
            PkgSelection(pkg.owner, ["full fallback: " + p.fallbacks[0]])
            for pkg in sorted(module_packages, key=lambda x: x.owner)
        ]

    # Native requirement (independent of mode).
    nreq: list[str] = []
    for ch in repo.changes:
        for path in ([ch.path, ch.old_path] if ch.old_path else [ch.path]):
            if pol.under_prefix(path, polobj.native_required_prefixes):
                nreq.append(f"{path} under native_required_prefix")
            if path.endswith(".go"):
                fname = posixpath.basename(path)
                if native_suffix_check(fname, polobj):
                    nreq.append(f"{path} has GOOS/GOARCH filename constraint")
    # Package present on one OS but absent on another = platform-specific code.
    for imp in selected_imports:
        presence = {os_name: imp in graphs[os_name].packages for os_name in TARGET_OSES}
        if len(set(presence.values())) > 1:
            nreq.append(
                f"package {imp} differs between OS graphs ("
                + ", ".join(f"{k}:{'yes' if v else 'no'}" for k, v in presence.items())
                + ")"
            )
    nreq.extend(race_native)
    nreq = sorted(set(nreq))
    p.native_required = {"required": bool(nreq), "reasons": nreq}

    # Race selection: closure intersecting race_roots (whole packages).
    race: list[str] = []
    for entry in p.selected["linux"]:
        if pol.under_prefix(repo_rel(entry.package), polobj.race_roots):
            race.append(entry.package)
    p.race = race

    # Not-run coverage: deleted files/packages.
    seen_del: set[str] = set()
    for ch in repo.changes:
        if ch.status != "D":
            continue
        for path in [ch.path, ch.old_path or ""]:
            if not path or path in seen_del:
                continue
            seen_del.add(path)
            d = _file_dir(path)
            gone = resolve_owner(d) is None
            reason = (
                "deleted package" if gone and path.endswith(".go")
                else "deleted file"
            )
            p.not_run.append(
                {"os": "all", "package_or_suite": path, "reason": reason}
            )
    for os_name in TARGET_OSES:
        g = graphs[os_name]
        selected_names = {e.package for e in p.selected[os_name]}
        for owner in sorted(g.packages):
            pkg = g.packages[owner]
            if owner in selected_names:
                continue
            p.not_run.append(
                {"os": os_name, "package_or_suite": owner, "reason": "not selected"}
            )
        if race and os_name != "linux":
            p.not_run.append(
                {
                    "os": os_name,
                    "package_or_suite": "race",
                    "reason": "race suite runs on linux only",
                }
            )
    if p.mode == "tier1":
        for os_name in TARGET_OSES:
            p.not_run.append(
                {
                    "os": os_name,
                    "package_or_suite": "security",
                    "reason": "security suites run in full mode only",
                }
            )
    for d in deleted_dirs:
        p.not_run.append(
            {"os": "all", "package_or_suite": d or ".", "reason": "deleted package"}
        )
    return p
