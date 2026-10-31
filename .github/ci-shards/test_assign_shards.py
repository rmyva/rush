#!/usr/bin/env python3
"""Unit tests for assign_shards.py. Run via:
    python -I -m unittest discover -s .github/ci-shards -v
All tests invoke the real CLI via subprocess so exit codes are covered.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import tomllib
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "assign_shards.py")

BASE_TOML = """\
module = "example.com/m"
[agent_isolated]
packages = ["internal/agent"]
[shards.A]
groups = [
  { name = "test: a", type = "go", packages = ["internal/app"] },
]
[shards.B]
groups = [
  { name = "test: b", type = "go", packages = ["internal/cmd", "internal/commands"] },
]
[shards.D]
groups = [
  { name = "test: d", type = "go", packages = ["internal/db"] },
  { name = "test: everything else (computed catch-all)", type = "go", catch_all = true, packages = [] },
]
"""


def run_cli(shards: str, packages: list[str], *extra: str) -> subprocess.CompletedProcess:
    with tempfile.TemporaryDirectory() as td:
        toml_path = os.path.join(td, "shards.toml")
        with open(toml_path, "w", encoding="utf-8") as f:
            f.write(shards)
        in_path = os.path.join(td, "packages.txt")
        with open(in_path, "w", encoding="utf-8") as f:
            f.write("\n".join(packages) + "\n")
        return subprocess.run(
            [sys.executable, SCRIPT, "--shards", toml_path, "--input", in_path, *extra],
            capture_output=True,
            text=True,
        )


def run_coverage(
    shards: str, workflow: str | None, assignment: dict
) -> subprocess.CompletedProcess:
    with tempfile.TemporaryDirectory() as td:
        toml_path = os.path.join(td, "shards.toml")
        with open(toml_path, "w", encoding="utf-8") as f:
            f.write(shards)
        assignment_path = os.path.join(td, "assignment.json")
        with open(assignment_path, "w", encoding="utf-8") as f:
            json.dump(assignment, f)
        args = [sys.executable, SCRIPT, "--shards", toml_path, "--check-coverage"]
        if workflow is not None:
            workflow_path = os.path.join(td, "ci-windows-shards.yml")
            with open(workflow_path, "w", encoding="utf-8") as f:
                f.write(workflow)
            args.extend(["--workflow", workflow_path])
        args.extend(["--assignment", assignment_path])
        return subprocess.run(args, capture_output=True, text=True)


def run_coverage_files(shards_path: str, workflow_path: str, assignment: dict) -> subprocess.CompletedProcess:
    with tempfile.TemporaryDirectory() as td:
        assignment_path = os.path.join(td, "assignment.json")
        with open(assignment_path, "w", encoding="utf-8") as f:
            json.dump(assignment, f)
        return subprocess.run(
            [sys.executable, SCRIPT, "--shards", shards_path, "--check-coverage",
             "--workflow", workflow_path, "--assignment", assignment_path],
            capture_output=True, text=True,
        )


COVERAGE_TOML = '''\
module = "example.com/m"
[shards.A]
groups = [
  { name = "test: a", type = "go", packages = ["internal/app"] },
  { name = "go mod tidy produces no diff", type = "tidy" },
  { name = "test: npm wrapper cache logic", type = "npm" },
]
[windows_leg]
"test: a" = "A/test: a"
"go mod tidy produces no diff" = "A/go mod tidy produces no diff"
"test: npm wrapper cache logic" = "A/test: npm wrapper cache logic"
"go test ./internal/agent/ split into sequential batches" = "agent"
'''

COVERAGE_ASSIGNMENT = {
    "shards": {
        "A": {
            "groups": [
                {"name": "test: a"},
                {"name": "go mod tidy produces no diff"},
                {"name": "test: npm wrapper cache logic"},
            ]
        }
    }
}


class AssignShardsTests(unittest.TestCase):
    def test_coverage_uses_repository_workflow_and_shard_groups(self):
        root = os.path.dirname(os.path.dirname(HERE))
        shards_path = os.path.join(HERE, "shards.toml")
        workflow_path = os.path.join(root, ".github", "workflows", "ci-windows-shards.yml")
        with open(shards_path, "rb") as f:
            config = tomllib.load(f)
        assignment = {"shards": {
            shard: {"groups": [{"name": group["name"]} for group in data.get("groups", [])]}
            for shard, data in config["shards"].items()
        }}
        p = run_coverage_files(shards_path, workflow_path, assignment)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("coverage OK", p.stdout)

    def test_coverage_missing_workflow_file_fails_clearly(self):
        missing = os.path.join(HERE, "nonexistent-ci-windows-shards.yml")
        p = run_coverage_files(os.path.join(HERE, "shards.toml"), missing, {"shards": {}})
        self.assertNotEqual(p.returncode, 0)
        self.assertIn(os.path.basename(missing), p.stderr)

    def test_coverage_empty_workflow_fails(self):
        assignment = COVERAGE_ASSIGNMENT
        p = run_coverage(COVERAGE_TOML, "# empty workflow\\n", assignment)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("no recognized Windows group steps", p.stderr)

        assignment = COVERAGE_ASSIGNMENT
        workflow = '''\
      - name: "test: a"
      - name: go mod tidy produces no diff
      - name: "test: npm wrapper cache logic"
      - name: go test ./internal/agent/ split into sequential batches
'''
        p = run_coverage(COVERAGE_TOML, workflow, assignment)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("coverage OK", p.stdout)

    def test_coverage_reads_active_workflow_group_names(self):
        assignment = COVERAGE_ASSIGNMENT
        workflow = '''\
      - name: "test: a"
      - name: go mod tidy produces no diff
      - name: "test: npm wrapper cache logic"
      - name: go test ./internal/agent/ split into sequential batches
'''
        p = run_coverage(COVERAGE_TOML, workflow, assignment)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("coverage OK", p.stdout)

        assignment = {"shards": {"A": {"groups": [{"name": "test: a"}]}}}
        workflow = '      - name: "test: newly added group"\n'
        p = run_coverage(COVERAGE_TOML, workflow, assignment)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("test: newly added group", p.stderr)

    def test_reverse_coverage_rejects_removed_workflow_step_with_retained_mapping(self):
        assignment = COVERAGE_ASSIGNMENT
        workflow = '''\
      - name: go mod tidy produces no diff
      - name: "test: npm wrapper cache logic"
      - name: go test ./internal/agent/ split into sequential batches
'''
        p = run_coverage(COVERAGE_TOML, workflow, assignment)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("test: a", p.stderr)
        self.assertIn("has no active workflow step", p.stderr)

    def test_coverage_requires_workflow_input(self):
        p = run_coverage(COVERAGE_TOML, None, {"shards": {}})
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("requires --workflow", p.stderr)

    def test_exact_once_assignment(self):
        pkgs = [
            "example.com/m",
            "example.com/m/internal/app",
            "example.com/m/internal/cmd/thing",
            "example.com/m/internal/commands",
            "example.com/m/internal/agent",
            "example.com/m/internal/agent/tools",
            "example.com/m/internal/db",
            "example.com/m/internal/other",
        ]
        p = run_cli(BASE_TOML, pkgs)
        self.assertEqual(p.returncode, 0, p.stderr)
        out = json.loads(p.stdout)
        self.assertEqual(out["counts"]["total"], len(pkgs))
        self.assertEqual(out["agent"], ["example.com/m/internal/agent"])
        self.assertEqual(out["unassigned"], [])
        flat = [x for gs in out["shards"].values() for g in gs["groups"] for x in g["packages"]]
        flat += out["agent"]
        self.assertEqual(sorted(flat), sorted(pkgs))
        self.assertEqual(len(flat), len(set(flat)))  # exactly once
        self.assertEqual(
            out["shards"]["D"]["groups"][1]["packages"],
            # no fixture group claims internal/agent/tools, so it lands in
            # the D catch-all together with the module root and internal/other
            [
                "example.com/m",
                "example.com/m/internal/agent/tools",
                "example.com/m/internal/other",
            ],
        )

    def test_unassigned_without_catch_all_exits_nonzero(self):
        toml = BASE_TOML.replace(
            '  { name = "test: everything else (computed catch-all)", type = "go", catch_all = true, packages = [] },\n',
            "",
        )
        p = run_cli(toml, ["example.com/m/internal/mystery"])
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("unassigned", p.stderr)

    def test_double_assignment_exits_nonzero(self):
        # Overlapping (non-identical) prefixes: internal/app matches both
        # "internal" (B) and "internal/app" (A) -> per-package error.
        toml = BASE_TOML.replace(
            'packages = ["internal/cmd", "internal/commands"]',
            'packages = ["internal"]',
        )
        p = run_cli(toml, ["example.com/m/internal/app"])
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("double-assigned", p.stderr)

    def test_duplicate_prefix_exits_nonzero(self):
        toml = BASE_TOML.replace(
            'packages = ["internal/cmd", "internal/commands"]',
            'packages = ["internal/cmd", "internal/app"]',  # dup with A
        )
        p = run_cli(toml, ["example.com/m/internal/app"])
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("duplicate", p.stderr)

    def test_agent_isolation(self):
        p = run_cli(BASE_TOML, ["example.com/m/internal/agent", "example.com/m/internal/agent/tools"])
        self.assertEqual(p.returncode, 0, p.stderr)
        out = json.loads(p.stdout)
        self.assertEqual(out["agent"], ["example.com/m/internal/agent"])
        all_shard = [x for gs in out["shards"].values() for g in gs["groups"] for x in g["packages"]]
        self.assertEqual(all_shard, ["example.com/m/internal/agent/tools"])
        # A bare "internal/agent" prefix in any shard group is rejected.
        bad = BASE_TOML.replace(
            'packages = ["internal/cmd", "internal/commands"]',
            'packages = ["internal/agent"]',
        )
        p = run_cli(bad, ["example.com/m/internal/agent"])
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("internal/agent", p.stderr)

    def test_new_package_falls_into_catch_all(self):
        p = run_cli(BASE_TOML, ["example.com/m/internal/newthing"])
        self.assertEqual(p.returncode, 0, p.stderr)
        out = json.loads(p.stdout)
        self.assertEqual(
            out["shards"]["D"]["groups"][1]["packages"],
            ["example.com/m/internal/newthing"],
        )

    def test_removed_package_warns_but_succeeds(self):
        p = run_cli(BASE_TOML, ["example.com/m/internal/cmd"])
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("internal/commands", p.stderr)  # stale-prefix warning
        out = json.loads(p.stdout)
        b = out["shards"]["B"]["groups"][0]
        self.assertEqual(b["packages"], ["example.com/m/internal/cmd"])
        self.assertEqual(b["name"], "test: b")  # group kept (not empty-dropped silently)

    def test_ordering_stability_byte_identical(self):
        pkgs = [
            "example.com/m/internal/app",
            "example.com/m/internal/commands",
            "example.com/m/internal/cmd",
            "example.com/m/internal/db",
            "example.com/m/internal/agent",
            "example.com/m/internal/zzz",
            "example.com/m",
        ]
        p1 = run_cli(BASE_TOML, pkgs)
        p2 = run_cli(BASE_TOML, list(reversed(pkgs)))
        self.assertEqual(p1.returncode, 0, p1.stderr)
        self.assertEqual(p2.returncode, 0, p2.stderr)
        self.assertEqual(p1.stdout, p2.stdout)


if __name__ == "__main__":
    unittest.main()
