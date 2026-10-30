#!/usr/bin/env python3
"""Tiered CI plan adapter, deterministic gate policy, and scoped cancellation."""
from __future__ import annotations
import argparse
import json
import os
import re
import subprocess
import sys
from pathlib import Path

FULL_LABEL = "full-ci"
NO_FULL = "Full validation not requested for this revision."
SHA_RE = re.compile(r"^[0-9a-fA-F]{40}$")
OS_NAMES = ("linux", "darwin", "windows")
MODULE = "github.com/PHPCraftdream/rush"


def decide(event_name, base_ref, labels, mode, needed_job_results, validated_sha,
           current_sha, validated_base_sha=None, current_base_sha=None,
           expected_jobs=None, plan_valid=True, receipts_valid=True,
           jobs_not_needed=None, event_action=None, event_label=None, draft=False):
    """Pure policy: required results, revision identity, verdict, reason, not_run."""
    not_run = list(jobs_not_needed or [])
    labels = labels or []
    if event_name == "pull_request" and event_action in ("labeled", "unlabeled") and event_label not in (None, FULL_LABEL):
        return {"verdict": "pass", "reason": f"Ignored unrelated label event: {event_action} {event_label}.", "not_run": not_run}
    if event_name not in ("pull_request", "push", "workflow_dispatch"):
        return {"verdict": "fail", "reason": f"Unsupported event: {event_name}.", "not_run": not_run}
    main_pr = event_name == "pull_request" and base_ref == "main"
    main_push = event_name == "push" and base_ref == "main"
    if mode not in ("tier1", "full"):
        return {"verdict": "fail", "reason": "Malformed plan mode.", "not_run": not_run}
    if main_pr and FULL_LABEL in labels and mode != "full":
        return {"verdict": "fail", "reason": NO_FULL, "not_run": not_run}
    if main_push and mode != "full":
        return {"verdict": "fail", "reason": "Main push requires full validation.", "not_run": not_run}
    if event_name == "pull_request":
        if not SHA_RE.fullmatch(str(validated_sha or "")) or not SHA_RE.fullmatch(str(current_sha or "")):
            return {"verdict": "fail", "reason": "Missing or malformed PR head SHA.", "not_run": not_run}
        if validated_sha.lower() != current_sha.lower():
            return {"verdict": "fail", "reason": "stale revision: validated PR head differs from live head.", "not_run": not_run}
        if not SHA_RE.fullmatch(str(validated_base_sha or "")) or not SHA_RE.fullmatch(str(current_base_sha or "")) or validated_base_sha.lower() != current_base_sha.lower():
            return {"verdict": "fail", "reason": "stale revision: validated PR base differs from live base.", "not_run": not_run}
    if not plan_valid or not receipts_valid or not isinstance(needed_job_results, dict):
        return {"verdict": "fail", "reason": "Missing or malformed plan/receipts.", "not_run": not_run}
    expected = list(expected_jobs or [])
    if not expected:
        return {"verdict": "fail", "reason": "Malformed plan: expected job set is empty.", "not_run": not_run}
    missing = sorted(set(expected) - set(needed_job_results))
    unexpected = sorted(set(needed_job_results) - set(expected))
    failures = sorted(name for name, result in needed_job_results.items() if result != "success")
    if missing or unexpected or failures:
        issues = []
        if missing: issues.append("missing jobs: " + ", ".join(missing))
        if unexpected: issues.append("unexpected jobs: " + ", ".join(unexpected))
        if failures: issues.append("required jobs not successful: " + ", ".join(f"{n} ({needed_job_results[n]})" for n in failures))
        return {"verdict": "fail", "reason": "; ".join(issues), "not_run": not_run}
    return {"verdict": "pass", "reason": "Full validation passed." if mode == "full" else "Tier 1 validation passed.", "not_run": not_run}


def expected_jobs(plan, outputs):
    """Derive exact Actions jobs API names from validated plan and caller outputs."""
    if not isinstance(plan, dict) or plan.get("mode") not in ("tier1", "full"):
        raise ValueError("malformed plan mode")
    flags = outputs or {}
    def yes(key): return flags.get(key) is True or str(flags.get(key, "")).lower() == "true"
    jobs = ["plan", "checks"]
    skipped = []
    mode = plan["mode"]
    if yes("web_changed"): jobs.append("web")
    else: skipped.append({"job": "web", "reason": "web not selected"})
    if yes("ubuntu_needed"): jobs.append("tests / test-ubuntu")
    else: skipped.append({"job": "tests / test-ubuntu", "reason": "not selected by validated plan"})
    if yes("agent_selected"): jobs.append("tests / agent-ubuntu")
    else: skipped.append({"job": "tests / agent-ubuntu", "reason": "agent not selected"})
    if yes("race_needed"): jobs.append("tests / race-ubuntu")
    else: skipped.append({"job": "tests / race-ubuntu", "reason": "no race packages selected"})
    if yes("macos_needed"): jobs.append("tests / test-native (macos-latest)")
    else: skipped.append({"job": "tests / test-native (macos-latest)", "reason": "Darwin not selected"})
    if yes("windows_needed") and mode != "full": jobs.append("tests / test-native (windows-latest)")
    elif yes("windows_needed"): pass
    else: skipped.append({"job": "tests / test-native (windows-latest)", "reason": "Windows not selected"})
    if yes("windows_needed") and mode == "full":
        jobs += ["tests / windows-full-shards / plan-shards"]
        jobs += [f"tests / windows-full-shards / windows-shard ({letter})" for letter in "ABCD"]
        jobs += ["tests / windows-full-shards / windows-agent", "tests / windows-full-shards / shards-summary"]
    else: skipped.append({"job": "tests / windows-full-shards", "reason": "full Windows shard suite not selected"})
    if yes("security_full"): jobs.append("security / codeql-go")
    else: skipped.append({"job": "security / codeql-go", "reason": "security not selected"})
    if yes("security_actions"): jobs.append("security / codeql-actions")
    else: skipped.append({"job": "security / codeql-actions", "reason": "security not selected"})
    if yes("security_deps"): jobs.extend(["security / govulncheck", "security / grype"])
    else: skipped.extend([{"job": "security / govulncheck", "reason": "security not selected"}, {"job": "security / grype", "reason": "security not selected"}])
    if mode == "full":
        required = {"ubuntu_needed", "macos_needed", "windows_needed", "agent_selected", "race_needed", "web_changed", "security_full", "security_actions", "security_deps"}
        if not all(yes(k) for k in required): raise ValueError("full plan has incomplete coverage flags")
    return jobs, skipped


def promote_plan(plan, graphs, policy, force_full):
    """Expand full plans to complete per-platform inventory and configured race roots."""
    if not isinstance(plan, dict) or plan.get("schema_version") != 1 or plan.get("mode") not in ("tier1", "full"):
        raise ValueError("invalid selector plan schema/mode")
    if not isinstance(plan.get("not_run"), list) or not isinstance(plan.get("race_packages"), list) or not isinstance(plan.get("per_os"), dict):
        raise ValueError("invalid plan coverage fields")
    for os_name in OS_NAMES:
        if not isinstance(plan["per_os"].get(os_name), list): raise ValueError("missing OS plan inventory: " + os_name)
        if any(not isinstance(x, dict) or not isinstance(x.get("package"), str) for x in plan["per_os"][os_name]): raise ValueError("malformed OS plan inventory")
    for os_name in OS_NAMES:
        selected = {entry["package"] for entry in plan["per_os"][os_name]}
        plan["not_run"] = [item for item in plan["not_run"] if not (item.get("os") == os_name and item.get("package_or_suite") in selected)]
    if force_full or plan["mode"] == "full":
        if not isinstance(graphs, dict) or not isinstance(policy, dict): raise ValueError("full plan requires parsed graphs and policy")
        for os_name in OS_NAMES:
            graph = graphs.get(os_name)
            packages = getattr(graph, "packages", None)
            if not isinstance(packages, dict) or not packages: raise ValueError(f"missing full package graph: {os_name}")
            packages = [name for name in packages if name == MODULE or name.startswith(MODULE + "/")]
            if not packages: raise ValueError(f"full package graph has no module owners: {os_name}")
            plan.setdefault("per_os", {})[os_name] = [{"package": name, "reasons": ["full validation"]} for name in sorted(packages)]
        linux = graphs["linux"]
        roots = set(policy.get("race_roots", []))
        race = {name for name in linux.packages if (name == MODULE or name.startswith(MODULE + "/")) and any(name == MODULE + "/" + root or name.startswith(MODULE + "/" + root.rstrip("/") + "/") for root in roots)}
        agent = MODULE + "/internal/agent"
        plan["race_packages"] = sorted(name for name in race if name != agent)
        plan["mode"] = "full"
        plan["native_required"] = {"required": True, "reasons": ["full validation requires all native OS lanes"]}
        plan["full_promoted"] = True
        plan["not_run"] = [item for item in plan["not_run"] if not (item.get("reason") in ("not selected", "security suites run in full mode only"))]
    return plan


def run_json(argv, env=None):
    return json.loads(subprocess.run(argv, env=env, check=True, capture_output=True, text=True).stdout)


def load_graphs(graph_dir):
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    from ci_plan import golist
    return golist.build_graphs(".", {o: str(Path(graph_dir) / f"{o}.json") for o in OS_NAMES})


def prepare_cli(args):
    with open(args.plan, encoding="utf-8") as f: plan = json.load(f)
    graphs = load_graphs(args.graph_dir) if args.force_full or plan.get("mode") == "full" else None
    policy = None
    if graphs is not None:
        import tomllib
        with open(args.policy, "rb") as f: policy = tomllib.load(f)
    plan = promote_plan(plan, graphs, policy, args.force_full)
    changed = []
    if args.base and args.head:
        changed = subprocess.run(["git", "diff", "--name-only", args.base, args.head], check=True, capture_output=True, text=True).stdout.splitlines()
    full = plan["mode"] == "full"
    outputs = {
        "mode": plan["mode"], "ubuntu_needed": full or bool(plan.get("per_os", {}).get("linux")),
        "macos_needed": full or bool(plan.get("native_required", {}).get("required", False)) or bool(plan.get("per_os", {}).get("darwin")),
        "windows_needed": full or bool(plan.get("native_required", {}).get("required", False)) or bool(plan.get("per_os", {}).get("windows")),
        "linux_packages": " ".join(x["package"] for x in plan["per_os"].get("linux", [])),
        "darwin_packages": " ".join(x["package"] for x in plan["per_os"].get("darwin", [])),
        "windows_packages": " ".join(x["package"] for x in plan["per_os"].get("windows", [])),
        "race_packages": " ".join(x for x in plan.get("race_packages", []) if x != MODULE + "/internal/agent"),
        "race_needed": any(x != MODULE + "/internal/agent" for x in plan.get("race_packages", [])),
        "agent_selected": any(x["package"] == MODULE + "/internal/agent" for x in plan["per_os"].get("linux", [])),
        "heavy_allowed": True,
        "native_required": full or bool(plan.get("native_required", {}).get("required", False)),
        "validated_head": plan.get("head") or args.head, "validated_base": plan.get("base") or args.base,
        "web_changed": full or any(x.startswith("web/") for x in changed), "security_full": full,
        "security_deps": full or any(x in ("go.mod", "go.sum", "web/pnpm-lock.yaml") for x in changed),
        "security_actions": full or any(x.startswith(".github/workflows/") for x in changed),
    }
    plan["head"] = outputs["validated_head"]
    plan["base"] = outputs["validated_base"]
    plan["expected_outputs"] = outputs
    with open(args.output, "w", encoding="utf-8") as f: json.dump(plan, f, sort_keys=True, indent=2); f.write("\n")
    target = os.environ.get("GITHUB_OUTPUT")
    if target:
        with open(target, "a", encoding="utf-8") as f:
            for key, value in outputs.items(): f.write(f"{key}={str(value).lower() if isinstance(value, bool) else value}\n")


def commit_requests_full(messages: str):
    """Whether commit-message text contains an exact CI: full directive line."""
    return any(re.fullmatch(r"[ \t]*CI:[ \t]*full[ \t]*\r?", line, re.IGNORECASE) for line in messages.split("\n"))


def full_directive_cli(args):
    message_text = subprocess.run(["git", "log", "--format=%B", f"{args.base}..{args.head}"], check=True, capture_output=True, text=True).stdout
    full = commit_requests_full(message_text)
    value = str(full).lower()
    print(value)
    target = os.environ.get("GITHUB_OUTPUT")
    if target:
        with open(target, "a", encoding="utf-8") as f: f.write(f"full_directive={value}\n")


def _page_payloads(value):
    if isinstance(value, list):
        for part in value: yield from _page_payloads(part)
    elif isinstance(value, dict) and "jobs" in value:
        yield from value["jobs"]
    elif isinstance(value, dict) and "workflow_runs" in value:
        yield from value["workflow_runs"]
    elif isinstance(value, dict):
        yield value


def validate_assignment(assignment, expected_packages=None):
    if not isinstance(assignment, dict) or assignment.get("module") != MODULE or not isinstance(assignment.get("shards"), dict) or not isinstance(assignment.get("agent"), list): raise ValueError("malformed Windows shard assignment")
    seen = list(assignment["agent"])
    agent_package = MODULE + "/internal/agent"
    if assignment["agent"] != [agent_package]: raise ValueError("Windows assignment agent package mismatch")
    counts = assignment.get("counts")
    if not isinstance(counts, dict): raise ValueError("malformed Windows assignment counts")
    for letter in "ABCD":
        shard = assignment["shards"].get(letter)
        if not isinstance(shard, dict) or not isinstance(shard.get("groups"), list): raise ValueError("missing shard " + letter)
        shard_total = 0
        for group in shard["groups"]:
            if not isinstance(group, dict) or not isinstance(group.get("packages"), list): raise ValueError("malformed shard group")
            packages = group["packages"]
            if any(not isinstance(x, str) or not (x == MODULE or x.startswith(MODULE + "/")) for x in packages): raise ValueError("invalid shard package owner")
            seen.extend(packages); shard_total += len(packages)
        if counts.get(letter) != shard_total: raise ValueError("shard count mismatch: " + letter)
    if any(not isinstance(x, str) for x in assignment["agent"]) or len(seen) != len(set(seen)) or assignment.get("unassigned") != []: raise ValueError("duplicate or unassigned shard package")
    if counts.get("agent") != 1 or counts.get("total") != len(seen): raise ValueError("shard assignment total mismatch")
    if expected_packages is not None and set(seen) != set(expected_packages): raise ValueError("shard assignment does not cover full Windows plan inventory")
    return True


def _append_summary(output):
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if not summary: return
    safe = lambda value: str(value).replace("|", "\\|").replace("\r", " ").replace("\n", " ")
    with open(summary, "a", encoding="utf-8") as f:
        f.write(f"## ci-gate: {safe(output['verdict']).upper()}\n\n{safe(output['reason'])}\n\n| Job | Result |\n|---|---|\n")
        for name, status in sorted(output.get("jobs", {}).items()): f.write(f"| {safe(name)} | {safe(status)} |\n")
        f.write("\n## not_run\n| OS / package or job | Reason |\n|---|---|\n")
        for item in output.get("not_run", []):
            if isinstance(item, dict):
                who = item.get("job") or f"{item.get('os', 'suite')}/{item.get('package_or_suite', 'suite')}"
                f.write(f"| {safe(who)} | {safe(item.get('reason','not selected'))} |\n")
            else: f.write(f"| {safe(item)} | declared by plan |\n")


def gate_cli(args):
    event = os.environ.get("GITHUB_EVENT_NAME", "")
    action = os.environ.get("GITHUB_EVENT_ACTION")
    event_label = os.environ.get("GITHUB_EVENT_LABEL")
    if event == "pull_request" and action in ("labeled", "unlabeled") and event_label not in (None, FULL_LABEL):
        result = decide(event, os.environ.get("GITHUB_BASE_REF", ""), [], "tier1", {}, None, None, expected_jobs=[], event_action=action, event_label=event_label)
        result["not_run"] = [{"job": "all heavy work", "reason": "unrelated label event"}]
        with open(args.output, "w", encoding="utf-8") as f: json.dump(result, f, sort_keys=True, indent=2); f.write("\n")
        _append_summary(result)
        return result
    try:
        event = os.environ["GITHUB_EVENT_NAME"]
        repo = os.environ.get("GITHUB_REPOSITORY", "")
        labels, live_head, live_base, base_ref = [], os.environ.get("GITHUB_SHA", ""), os.environ.get("GITHUB_BASE_REF", ""), args.base_ref
        if event == "pull_request":
            number = os.environ["PR_NUMBER"]
            pr = run_json(["gh", "api", f"repos/{repo}/pulls/{number}"], os.environ.copy())
            labels = [x["name"] for x in pr["labels"]]
            live_head, live_base, base_ref = pr["head"]["sha"], pr["base"]["sha"], pr["base"]["ref"]
        with open(args.plan, encoding="utf-8") as f: plan = json.load(f)
        outputs = json.loads(os.environ["PLAN_OUTPUTS_JSON"]); needs = json.loads(os.environ["NEEDS_JSON"])
        if not isinstance(needs, dict) or not all(needs.get(k) == "success" for k in ("plan", "checks")): raise ValueError("plan/checks dependency did not succeed")
        for name,status in needs.items():
            if name not in ("tests","web","security") and status not in ("success","skipped"): raise ValueError(f"unexpected top-level need {name}: {status}")
        if plan.get("schema_version") != 1 or plan.get("mode") not in ("tier1", "full") or not isinstance(plan.get("per_os"), dict) or not isinstance(plan.get("race_packages"), list) or not isinstance(plan.get("not_run"), list): raise ValueError("malformed plan schema")
        if any(not isinstance(x, dict) or not all(k in x for k in ("os","package_or_suite","reason")) for x in plan["not_run"]): raise ValueError("malformed not_run table")
        for os_name in OS_NAMES:
            if not isinstance(plan["per_os"].get(os_name), list): raise ValueError("malformed per-OS plan inventory")
        expected_outputs = plan.get("expected_outputs")
        if not isinstance(expected_outputs, dict): raise ValueError("plan missing expected output contract")
        for key, expected in expected_outputs.items():
            actual = outputs.get(key)
            if isinstance(expected, bool): actual = actual is True or str(actual).lower() == "true"
            elif isinstance(expected, str) and key in ("mode", "linux_packages", "darwin_packages", "windows_packages", "race_packages", "validated_head", "validated_base"): actual = str(actual)
            if actual != expected: raise ValueError("plan output drift: " + key)
        if plan.get("mode") != outputs.get("mode"): raise ValueError("mode differs from plan outputs")
        validated_head, validated_base = os.environ.get("VALIDATED_HEAD", ""), os.environ.get("VALIDATED_BASE", "")
        if validated_head != plan.get("head") or validated_base != plan.get("base"): raise ValueError("validated SHA inputs disagree with plan")
        needed, not_needed = expected_jobs(plan, outputs)
        for name, status in needs.items():
            if name in ("tests", "web", "security"):
                flag = {"tests": any(str(outputs.get(k, "false")).lower() == "true" for k in ("ubuntu_needed","macos_needed","windows_needed","agent_selected","race_needed")), "web": str(outputs.get("web_changed")).lower()=="true", "security": any(str(outputs.get(k)).lower()=="true" for k in ("security_full","security_deps","security_actions"))}[name]
                if flag and status != "success": raise ValueError(f"required top-level {name} result: {status}")
                if not flag and status not in ("success", "skipped"): raise ValueError(f"unexpected top-level {name} result: {status}")
        runid, attempt = os.environ["GITHUB_RUN_ID"], os.environ.get("GITHUB_RUN_ATTEMPT", "1")
        response = run_json(["gh", "api", "--paginate", "--slurp", f"repos/{repo}/actions/runs/{runid}/attempts/{attempt}/jobs?per_page=100"], os.environ.copy())
        current = {}
        for row in _page_payloads(response):
            if int(row.get("run_attempt", attempt)) != int(attempt): continue
            name = row.get("name")
            if not isinstance(name, str): raise ValueError("job receipt missing name")
            if name in current: raise ValueError(f"ambiguous job receipt: {name}")
            current[name] = row.get("conclusion") or row.get("status", "missing")
        result_map = {name: current[name] for name in needed if name in current}
        for item in not_needed:
            status = current.get(item["job"])
            if status not in (None, "success", "skipped"): raise ValueError(f"unexpected non-needed job result {item['job']}: {status}")
        not_run = list(plan["not_run"]) + [item for item in not_needed if current.get(item["job"]) != "success"]
        if plan["mode"] == "full" and outputs.get("windows_needed"):
            if not args.assignment: raise ValueError("full Windows plan missing assignment file")
            with open(args.assignment, encoding="utf-8") as f: validate_assignment(json.load(f), [x["package"] for x in plan["per_os"]["windows"]])
        output = decide(event, base_ref, labels, plan.get("mode"), result_map, validated_head, live_head, validated_base, live_base, needed, plan_valid=True, jobs_not_needed=not_run, event_action=action, event_label=event_label, draft=os.environ.get("PR_DRAFT", "false").lower() == "true")
        output["jobs"] = current; output["not_run"] = not_run
    except Exception as exc:
        output = {"verdict": "fail", "reason": f"Gate API/plan failure: {exc}", "jobs": {}, "not_run": []}
    with open(args.output, "w", encoding="utf-8") as f: json.dump(output, f, sort_keys=True, indent=2); f.write("\n")
    _append_summary(output)
    return output


def cancel_full_cli():
    env = os.environ.copy()
    try:
        if env.get("GITHUB_EVENT_NAME") != "pull_request" or env.get("GITHUB_EVENT_ACTION") != "unlabeled" or env.get("GITHUB_EVENT_LABEL") != FULL_LABEL:
            return "No full-run cancellation: unrelated event/label."
        repo, number, own = env["GITHUB_REPOSITORY"], env["PR_NUMBER"], env["GITHUB_RUN_ID"]
        pr = run_json(["gh", "api", f"repos/{repo}/pulls/{number}"], env)
        if FULL_LABEL in [x.get("name") for x in pr.get("labels", [])]: return "No cancellation: full-ci label remains live."
        runs = run_json(["gh", "api", "--paginate", "--slurp", f"repos/{repo}/actions/workflows/ci.yml/runs?event=pull_request&per_page=100"], env)
        count = 0
        for run in _page_payloads(runs):
            rid = str(run.get("id"))
            if rid == own or run.get("status") not in ("queued", "in_progress"): continue
            if run.get("workflow_id") != int(env.get("CI_WORKFLOW_ID", run.get("workflow_id", 0))): continue
            if not str(run.get("display_title", "")).startswith("ci [full]"): continue
            pulls = run.get("pull_requests", [])
            if not pulls:
                return "WARNING: run has no unambiguous pull-request identity; operator must cancel manually."
            if not any(str(p.get("number")) == number for p in pulls): continue
            subprocess.run(["gh", "run", "cancel", rid, "--repo", repo], env=env, check=True); count += 1
        return f"Cancelled {count} matching full run(s)."
    except Exception as exc:
        return f"WARNING: unable to cancel full CI run(s) ({exc}); operator must cancel manually."


def main():
    parser = argparse.ArgumentParser(); sub = parser.add_subparsers(dest="command", required=True)
    p = sub.add_parser("prepare-plan"); p.add_argument("--plan", required=True); p.add_argument("--graph-dir", required=True); p.add_argument("--policy", default=".github/ci-policy.toml"); p.add_argument("--output", required=True); p.add_argument("--force-full", action="store_true"); p.add_argument("--base", default=os.environ.get("BASE", "")); p.add_argument("--head", default=os.environ.get("HEAD", ""))
    p = sub.add_parser("gate"); p.add_argument("--plan", required=True); p.add_argument("--output", required=True); p.add_argument("--base-ref", default=os.environ.get("PR_BASE_REF", "")); p.add_argument("--assignment")
    p = sub.add_parser("full-directive"); p.add_argument("--base", required=True); p.add_argument("--head", required=True)
    sub.add_parser("cancel-full"); args = parser.parse_args()
    if args.command == "prepare-plan": prepare_cli(args); return 0
    if args.command == "full-directive": full_directive_cli(args); return 0
    if args.command == "cancel-full":
        message = cancel_full_cli()
        print(message)
        if message.startswith("WARNING:"):
            print(f"::warning::{message}")
            summary = os.environ.get("GITHUB_STEP_SUMMARY")
            if summary:
                with open(summary, "a", encoding="utf-8") as f: f.write(f"\n> {message}\n")
        return 0
    output = gate_cli(args); return 0 if output["verdict"] == "pass" else 1

if __name__ == "__main__": raise SystemExit(main())
