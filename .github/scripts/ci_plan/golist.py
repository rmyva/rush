"""Per-OS `go list -deps -test -json` consumption and graph building."""

from __future__ import annotations

import json
import os
import posixpath
import subprocess
from dataclasses import dataclass, field

TARGET_OSES = ("linux", "darwin", "windows")
TARGET_ARCH = {"linux": "amd64", "darwin": "arm64", "windows": "amd64"}
# Mirror typical CI: native CGO default enabled on every target.
CGO_ENABLED = {"linux": "1", "darwin": "1", "windows": "1"}


class GraphError(Exception):
    pass


@dataclass
class Package:
    owner: str  # normalized import path of owning package
    dir: str  # normalized package directory (repo-relative, '' for root)
    imports: set[str] = field(default_factory=set)
    embeds: set[str] = field(default_factory=set)
    test_only_variant: bool = False


@dataclass
class Graph:
    os_name: str
    module: str = ""
    packages: dict[str, Package] = field(default_factory=dict)  # by owner
    dirs: dict[str, str] = field(default_factory=dict)  # dir -> owner
    reverse: dict[str, set[str]] = field(default_factory=dict)  # imp -> importers


def _owner_for(obj: dict) -> tuple[str | None, bool]:
    """Return (owner import path, is synthetic test variant)."""
    imp = obj.get("ImportPath", "")
    for_test = obj.get("ForTest")
    if for_test:
        return for_test, True
    if imp.endswith(".test"):  # synthesized test binary
        return None, True
    return imp, False


def _repo_dir(module: str, import_path: str) -> str | None:
    """Import path -> repo-relative dir, or None for external packages."""
    if import_path == module:
        return ""
    if import_path.startswith(module + "/"):
        return import_path[len(module) + 1:]
    return None


def parse_stream(text: str, os_name: str) -> Graph:
    """Parse a stream of concatenated `go list -json` objects."""
    dec = json.JSONDecoder()
    graph = Graph(os_name=os_name)
    idx = 0
    n = len(text)
    count = 0
    objs: list[dict] = []
    while idx < n:
        while idx < n and text[idx] in " \t\r\n":
            idx += 1
        if idx >= n:
            break
        try:
            obj, end = dec.raw_decode(text, idx)
        except json.JSONDecodeError as exc:
            raise GraphError(
                f"malformed go list JSON for {os_name} at byte {idx}: {exc}"
            ) from exc
        idx = end
        count += 1
        objs.append(obj)

    # The main module is the one with Module.Main == true, not whatever
    # happens to come first in the stream (go list -deps often starts with
    # foreign dependency packages).
    main_module = ""
    for obj in objs:
        mod = obj.get("Module") or {}
        if mod.get("Main") and mod.get("Path"):
            main_module = mod["Path"]
            if obj.get("ImportPath") == main_module:
                break
    if main_module == "":
        raise GraphError(
            f"no main module (Module.Main) found in {os_name} go list output"
        )
    graph.module = main_module

    for obj in objs:
        owner, synthetic = _owner_for(obj)
        if owner is None:
            continue
        if graph.module == "":
            continue
        rdir = _repo_dir(graph.module, owner)
        if rdir is None:
            continue
        imp_path = obj.get("ImportPath", "")
        imports: set[str] = set()
        for key in ("Imports", "TestImports", "XTestImports"):
            for imp in obj.get(key) or []:
                if imp.endswith(".test"):
                    continue
                if _repo_dir(graph.module, imp) is not None:
                    imports.add(imp)
        embeds: set[str] = set()
        rdir_prefix = (rdir + "/") if rdir else ""
        for key in ("EmbedFiles", "TestEmbedFiles", "XTestEmbedFiles"):
            for f in obj.get(key) or []:
                f = f.replace("\\", "/")
                if f.startswith("./"):
                    f = f[2:]
                f = posixpath.normpath(f)
                # go list reports embed files relative to the package directory.
                # Normalize to repo-root-relative paths for changed-file lookup.
                if rdir:
                    f = rdir_prefix + f
                embeds.add(f)
        pkg = graph.packages.get(owner)
        if pkg is None:
            pkg = Package(owner=owner, dir=rdir)
            graph.packages[owner] = pkg
            graph.dirs[rdir] = owner
        pkg.imports |= imports
        pkg.embeds |= embeds
        _ = synthetic, imp_path  # variants normalize onto the owner above
    if count == 0:
        raise GraphError(f"go list output for {os_name} was empty")
    # Reverse edges by import path.
    for owner, pkg in graph.packages.items():
        for imp in pkg.imports:
            graph.reverse.setdefault(imp, set()).add(owner)
    return graph


def load_from_file(path: str, os_name: str) -> Graph:
    try:
        with open(path, "r", encoding="utf-8", errors="surrogateescape") as fh:
            text = fh.read()
    except OSError as exc:
        raise GraphError(f"cannot read go list JSON {path}: {exc}") from exc
    return parse_stream(text, os_name)


def enumerate_live(root: str, os_name: str) -> Graph:
    """Run exactly ONE `go list -deps -test -json ./...` for this OS."""
    env = dict(os.environ)
    env["GOOS"] = os_name
    env["GOARCH"] = TARGET_ARCH[os_name]
    env["CGO_ENABLED"] = CGO_ENABLED[os_name]
    try:
        proc = subprocess.run(
            ["go", "list", "-deps", "-test", "-json", "./..."],
            cwd=root,
            capture_output=True,
            text=False,
            env=env,
            check=False,
        )
    except OSError as exc:
        raise GraphError(f"failed to run go list for {os_name}: {exc}") from exc
    if proc.returncode != 0:
        raise GraphError(
            f"go list failed for {os_name}: "
            + proc.stderr.decode("utf-8", "replace").strip()[:2000]
        )
    return parse_stream(proc.stdout.decode("utf-8", "surrogateescape"), os_name)


def build_graphs(
    root: str,
    files: dict[str, str | None],
) -> dict[str, Graph]:
    graphs: dict[str, Graph] = {}
    for os_name in TARGET_OSES:
        f = files.get(os_name)
        if f:
            graphs[os_name] = load_from_file(f, os_name)
        else:
            graphs[os_name] = enumerate_live(root, os_name)
    return graphs


def reverse_closure(graphs: list[Graph], seeds: set[str]) -> set[str]:
    """Transitive reverse deps over the union of the given graphs."""
    merged: dict[str, set[str]] = {}
    for g in graphs:
        for imp, importers in g.reverse.items():
            merged.setdefault(imp, set()).update(importers)
    seen: set[str] = set()
    stack = list(seeds)
    while stack:
        cur = stack.pop()
        if cur in seen:
            continue
        seen.add(cur)
        stack.extend(merged.get(cur, ()))
    return seen
