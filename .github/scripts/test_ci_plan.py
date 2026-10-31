"""Fixture tests for the selector (stdlib unittest)."""

from __future__ import annotations

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from ci_plan import golist, policy as pol, selector  # noqa: E402

MODULE = "github.com/PHPCraftdream/rush"


def _p(imp, imports=(), for_test=None, embeds=()):
    o = {
        "ImportPath": imp,
        "Dir": imp[len(MODULE):].lstrip("/") or ".",
        "Module": {"Path": MODULE, "Main": True},
    }
    if imports:
        o["Imports"] = list(imports)
    if for_test:
        o["ForTest"] = for_test
        o["ImportPath"] = f"{for_test} [{for_test}.test]"
    if embeds:
        o["EmbedFiles"] = list(embeds)
    return o


def _dep(imp, module):
    """Foreign-module dependency package (Module.Main absent)."""
    return {"ImportPath": imp, "Dir": "/go/pkg/mod/" + imp, "Module": {"Path": module}}


def stream(*objs):
    import json
    return "\n".join(json.dumps(o) for o in objs)


class GoListTests(unittest.TestCase):
    def test_parse_and_normalize(self):
        g = golist.parse_stream(
            stream(
                _p(MODULE, imports=[MODULE + "/internal/db"]),
                _p(MODULE + "/internal/db"),
                _p("unused", for_test=MODULE + "/internal/db"),
            ),
            "linux",
        )
        self.assertEqual(g.module, MODULE)
        self.assertIn(MODULE + "/internal/db", g.packages)
        # Synthetic variant normalized onto its owner; reverse edge built.
        self.assertEqual(
            g.reverse[MODULE + "/internal/db"], {MODULE}
        )

    def test_first_object_foreign_module_still_maps_main(self):
        foreign = "github.com/charmbracelet/x/ansi"
        g = golist.parse_stream(
            stream(
                _dep(foreign, foreign),
                _p(MODULE, imports=[MODULE + "/internal/shell"]),
                _p(MODULE + "/internal/shell"),
            ),
            "linux",
        )
        self.assertEqual(g.module, MODULE)
        self.assertIn(MODULE + "/internal/shell", g.packages)
        self.assertIn(MODULE + "/internal/shell", g.reverse)
        self.assertEqual(g.reverse[MODULE + "/internal/shell"], {MODULE})

    def test_embed_files_normalize_package_relative_and_root_paths(self):
        g = golist.parse_stream(
            stream(
                _p(MODULE + "/internal/skills", embeds=["templates/prompt.md", "internal/skills/assets/icon.svg", "./nested/./diagram.svg"]),
                _p(MODULE, embeds=["web/index.html"]),
            ),
            "linux",
        )
        self.assertEqual(
            g.packages[MODULE + "/internal/skills"].embeds,
            {"internal/skills/templates/prompt.md", "internal/skills/internal/skills/assets/icon.svg", "internal/skills/nested/diagram.svg"},
        )
        self.assertEqual(g.packages[MODULE].embeds, {"web/index.html"})

    def test_no_main_module_raises(self):
        foreign = "github.com/example/dep"
        with self.assertRaises(golist.GraphError):
            golist.parse_stream(stream(_dep(foreign, foreign)), "linux")

    def test_malformed_raises(self):
        with self.assertRaises(golist.GraphError):
            golist.parse_stream("{not json", "linux")

    def test_empty_raises(self):
        with self.assertRaises(golist.GraphError):
            golist.parse_stream("  \n ", "linux")


class PolicyTests(unittest.TestCase):
    def setUp(self):
        self.pol = pol.load_policy(
            os.path.join(
                os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                "ci-policy.toml",
            )
        )

    def test_load(self):
        self.assertIn("internal/csync/", self.pol.core_prefixes)
        self.assertIn("internal/agent/notify/", self.pol.native_required_prefixes)
        self.assertIn("cmd/tasklab", self.pol.race_roots)

    def test_glob_match_keeps_single_star_within_path_segment(self):
        self.assertTrue(pol.glob_match("internal/agent/templates/a.md", "internal/agent/templates/*.md"))
        self.assertFalse(pol.glob_match("internal/agent/templates/nested/a.md", "internal/agent/templates/*.md"))
        self.assertTrue(pol.glob_match("internal/skills/nested/a.md", "internal/skills/**/*.md"))
        self.assertTrue(pol.glob_match("internal/skills/a.md", "internal/skills/**/*.md"))
        self.assertFalse(pol.glob_match("docs/example.txt", "internal/skills/**/*"))

    def test_prefix(self):
        self.assertTrue(pol.under_prefix("internal/db/x.sql", ["internal/db/"]))
        self.assertTrue(pol.under_prefix("internal/csync/a.go", ["internal/csync/"]))
        self.assertFalse(pol.under_prefix("internal/dbx/a.go", ["internal/db/"]))
        self.assertFalse(pol.under_prefix("outside/internal/db/x.sql", ["internal/db/"]))


class SelectionTests(unittest.TestCase):
    def setUp(self):
        self.pol = pol.Policy(
            native_required_prefixes=["internal/shell/"],
            core_prefixes=["internal/db/"],
            race_roots=["internal/agent"],
            fanout_max_packages=25,
            fanout_max_ratio=0.40,
        )
        objs = [
            _p(MODULE),
            _p(MODULE + "/internal/agent"),
            _p(MODULE + "/internal/db"),
            _p(MODULE + "/internal/ui", imports=[MODULE + "/internal/db"]),
        ]
        self.graphs = {
            os_name: golist.parse_stream(stream(*objs), os_name)
            for os_name in golist.TARGET_OSES
        }

    def test_report_heading_identifies_active_ci_plan(self):
        from ci_plan import report
        p = selector.Plan()
        p.selected = {os_name: [] for os_name in golist.TARGET_OSES}
        p.native_required = {"required": False, "reasons": []}
        md = report.summary_md(p)
        self.assertEqual("# CI plan", md.splitlines()[0])
        self.assertNotIn("shadow mode", md)
        self.assertNotIn("nothing runs", md)
        self.assertIn("## Unselected package counts", md)
        self.assertNotIn("Not run (unselected", md)

    def test_test_only_change_selects_owner_only(self):
        class R:
            base = "b"
            head = "h"
            merge_base = "b"
            base_tree_files = []
            changes = [
                type("C", (), {"status": "M", "path": "internal/agent/x_test.go", "old_path": None})()
            ]
        p = selector.plan(".", R(), self.graphs, self.pol)
        self.assertEqual(p.mode, "tier1")
        self.assertEqual(
            [e.package for e in p.selected["linux"]],
            [MODULE + "/internal/agent"],
        )
        self.assertIn(MODULE + "/internal/agent", p.race)

    def test_production_change_pulls_reverse_deps(self):
        class R:
            base = "b"
            head = "h"
            merge_base = "b"
            base_tree_files = []
            changes = [
                type("C", (), {"status": "M", "path": "internal/db/db.go", "old_path": None})()
            ]
        p = selector.plan(".", R(), self.graphs, self.pol)
        names = [e.package for e in p.selected["linux"]]
        self.assertIn(MODULE + "/internal/ui", names)
        # core prefix production change -> full fallback
        self.assertEqual(p.mode, "full")
        self.assertTrue(any("core prefixes" in f for f in p.fallbacks))
        # native decision independent
        self.assertFalse(p.native_required["required"])

    def test_native_prefix_and_suffix(self):
        class R:
            base = "b"
            head = "h"
            merge_base = "b"
            base_tree_files = []
            changes = [
                type("C", (), {"status": "M", "path": "internal/shell/shell_windows.go", "old_path": None})()
            ]
        p = selector.plan(".", R(), self.graphs, self.pol)
        self.assertTrue(p.native_required["required"])
        self.assertEqual(p.mode, "tier1")

    def test_unknown_ownership_falls_back(self):
        class R:
            base = "b"
            head = "h"
            merge_base = "b"
            base_tree_files = []
            changes = [
                type("C", (), {"status": "M", "path": "docs/mystery.bin", "old_path": None})()
            ]
        p = selector.plan(".", R(), self.graphs, self.pol)
        self.assertEqual(p.mode, "full")
        self.assertTrue(any("unknown ownership" in f for f in p.fallbacks))

    def test_sql_mapping_selects_db(self):
        self.pol.non_go_mappings = [
            pol.NonGoMapping(["internal/db/sql/*.sql"], ["internal/db"])
        ]
        class R:
            base = "b"
            head = "h"
            merge_base = "b"
            base_tree_files = []
            changes = [
                type("C", (), {"status": "M", "path": "internal/db/sql/stats.sql", "old_path": None})()
            ]
        p = selector.plan(".", R(), self.graphs, self.pol)
        names = [e.package for e in p.selected["linux"]]
        self.assertIn(MODULE + "/internal/ui", names)  # reverse dep of db
        self.assertTrue(any("core prefixes" in f for f in p.fallbacks))

    def test_deleted_package_consumers(self):
        class R:
            base = "b"
            head = "h"
            merge_base = "b"
            base_tree_files = [
                "internal/db/dead.go",
                "internal/agent/a.go",
            ]
            changes = [
                type("C", (), {"status": "D", "path": "internal/db/dead.go", "old_path": None})()
            ]
        p = selector.plan(".", R(), self.graphs, self.pol)
        names = [e.package for e in p.selected["linux"]]
        self.assertIn(MODULE + "/internal/ui", names)
        self.assertTrue(any(e["reason"].startswith("deleted") for e in p.not_run))

    def test_race_root_change_requires_native_lanes(self):
        class R:
            base = "b"
            head = "h"
            merge_base = "b"
            base_tree_files = []
            changes = [
                type("C", (), {"status": "M", "path": "internal/agent/x_test.go", "old_path": None})()
            ]
        p = selector.plan(".", R(), self.graphs, self.pol)
        self.assertTrue(p.native_required["required"])
        self.assertTrue(any("race_roots" in r for r in p.native_required["reasons"]))

    def test_race_root_change_runs_native_reverse_deps(self):
        objs = [
            _p(MODULE),
            _p(MODULE + "/internal/agent"),
            _p(MODULE + "/internal/app", imports=[MODULE + "/internal/agent"]),
            _p(MODULE + "/internal/db"),
            _p(MODULE + "/internal/ui", imports=[MODULE + "/internal/db"]),
        ]
        graphs = {
            os_name: golist.parse_stream(stream(*objs), os_name)
            for os_name in golist.TARGET_OSES
        }
        class R:
            base = "b"
            head = "h"
            merge_base = "b"
            base_tree_files = []
            changes = [
                type("C", (), {"status": "M", "path": "internal/agent/supervision.go", "old_path": None})()
            ]
        p = selector.plan(".", R(), graphs, self.pol)
        self.assertTrue(p.native_required["required"])
        darwin = [e.package for e in p.selected["darwin"]]
        windows = [e.package for e in p.selected["windows"]]
        self.assertIn(MODULE + "/internal/app", darwin)
        self.assertIn(MODULE + "/internal/app", windows)

    def test_windows_file_patterns_promote_to_full(self):
        self.pol.selector_full_patterns = [
            "internal/agent/**/*_windows.go",
            "internal/agent/**/*_windows_test.go",
        ]

        def plan_for(path):
            class R:
                base = "b"
                head = "h"
                merge_base = "b"
                base_tree_files = []
                changes = [type("C", (), {"status": "M", "path": path, "old_path": None})()]

            return selector.plan(".", R(), self.graphs, self.pol)

        for path in (
            "internal/agent/platform_windows.go",
            "internal/agent/platform_windows_test.go",
        ):
            with self.subTest(path=path):
                p = plan_for(path)
                self.assertEqual(p.mode, "full")
                self.assertTrue(any("selector full patterns" in f for f in p.fallbacks))

        # The linux path belongs to a package present in the fixture graph, so
        # this checks the suffix pattern rather than an unknown-owner fallback.
        self.assertNotEqual(plan_for("internal/ui/platform_linux.go").mode, "full")

    def test_always_run_guard_change_forces_full(self):
        self.pol.always_run = [".githooks/"]
        class R:
            base = "b"
            head = "h"
            merge_base = "b"
            base_tree_files = []
            changes = [
                type("C", (), {"status": "M", "path": ".githooks/check_file_size.sh", "old_path": None})()
            ]
        p = selector.plan(".", R(), self.graphs, self.pol)
        self.assertEqual(p.mode, "full")
        self.assertTrue(any("always-run" in f for f in p.fallbacks))

    def test_rename_both_paths_considered(self):
        class R:
            base = "b"
            head = "h"
            merge_base = "b"
            base_tree_files = []
            changes = [
                type("C", (), {"status": "R100", "path": "internal/agent/new_test.go", "old_path": "internal/agent/old_test.go"})()
            ]
        p = selector.plan(".", R(), self.graphs, self.pol)
        self.assertEqual(
            [e.package for e in p.selected["linux"]],
            [MODULE + "/internal/agent"],
        )


if __name__ == "__main__":
    unittest.main()
