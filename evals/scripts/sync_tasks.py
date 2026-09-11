#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import shutil
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
TASKS = ROOT / "tasks"
PRODUCTION_MANIFEST = ROOT / "production-cases.json"
PRODUCTION_TASKS = ROOT / "tasks-production"


def task_toml(case: dict, suite: str, image: str = "iron-forest-eval:local", offline: bool = False) -> str:
    description = json.dumps(case["summary"])
    network = 'network_mode = "no-network"' if offline else 'network_mode = "allowlist"\nallowed_hosts = ["openrouter.ai"]'
    return f'''schema_version = "1.4"

artifacts = [{{ source = "/var/lib/forest-eval/bundle", destination = "forest-eval-bundle" }}]

[task]
name = "iron-forest/{case["id"]}"
version = "0.1.0"
description = {description}
authors = [{{ name = "Iron Forest", email = "forest@invalid" }}]
keywords = ["iron-forest", "{case["role"]}", "{suite}"]

[metadata]
role = "{case["role"]}"
case = "{case["id"]}"

[verifier]
timeout_sec = 1800.0
{network}
env = {{ FOREST_EVAL_JUDGE_API_KEY = "${{FOREST_EVAL_JUDGE_API_KEY:-}}", FOREST_EVAL_JUDGE_MODEL = "${{FOREST_EVAL_JUDGE_MODEL:-}}", FOREST_EVAL_FORENSIC_JUDGE_MODEL = "${{FOREST_EVAL_FORENSIC_JUDGE_MODEL:-}}", FOREST_EVAL_REQUIRE_JUDGE = "${{FOREST_EVAL_REQUIRE_JUDGE:-0}}" }}
environment_mode = "separate"

[verifier.environment]
docker_image = "{image}"
network_mode = "no-network"
workdir = "/tests"

[[verifier.collect]]
command = "python3 /opt/iron-forest-eval/collect.py"
user = "root"
timeout_sec = 120.0

[agent]
user = "forest"
{network}

[environment]
docker_image = "{image}"
network_mode = "no-network"
env = {{ OPENROUTER_API_KEY = "${{OPENROUTER_API_KEY:-}}" }}
'''


def generate_tasks(manifest: dict, tasks_dir: Path, suite: str, image: str = "iron-forest-eval:local", offline: bool = False) -> None:
    shutil.rmtree(tasks_dir, ignore_errors=True)
    for case in manifest["cases"]:
        task = tasks_dir / case["id"]
        (task / "tests").mkdir(parents=True)
        (task / "solution").mkdir(parents=True)
        (task / "environment").mkdir(parents=True)
        scenario = json.dumps(case, indent=2, sort_keys=True) + "\n"
        (task / "tests" / "scenario.json").write_text(scenario)
        (task / "solution" / "scenario.json").write_text(scenario)
        (task / "instruction.md").write_text(
            f"Run the production Iron Forest {case['role']} declaration once.\n"
            "Read the explicit delegation in /run/forest-eval/request.json. "
            "A null request authorizes no work; historical queues are not authority. "
            "Use the production publication CLI for a supplied GitHub Subject, "
            "or return requested read-only findings without tracker writes.\n"
        )
        (task / "task.toml").write_text(task_toml(case, suite, image, offline))
        test = task / "tests" / "test.sh"
        test.write_text(
            "#!/bin/sh\n"
            "set -eu\n"
            "export FOREST_EVAL_HIDDEN=/var/lib/forest-eval/bundle\n"
            "python3 /opt/iron-forest-eval/grade.py /tests/scenario.json\n"
        )
        test.chmod(0o755)
        solve = task / "solution" / "solve.sh"
        solve.write_text(
            "#!/bin/sh\nset -eu\n"
            "sudo -n /usr/bin/python3 /opt/iron-forest-eval/setup.py /solution/scenario.json\n"
            "cd /workspace\n"
            "sudo -n /usr/bin/python3 /opt/iron-forest-eval/reference.py /solution/scenario.json\n"
        )
        solve.chmod(0o755)


def load_manifest(path: Path, schema: str) -> dict:
    manifest = json.loads(path.read_text())
    if manifest.get("schema") != schema:
        raise RuntimeError(f"unsupported case manifest schema in {path}")
    return manifest


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--case", action="append", default=[], help="Generate only this regression case (repeatable)")
    parser.add_argument("--output-dir", type=Path, help="Isolated generated regression task directory")
    parser.add_argument("--image", default="iron-forest-eval:local")
    parser.add_argument("--offline", action="store_true", help="Deny agent and verifier network access for deterministic execution")
    args = parser.parse_args()
    manifest = load_manifest(ROOT / "cases.json", "forest.evals.v1")
    if args.output_dir is not None and args.output_dir.exists():
        parser.error("--output-dir must not exist; refusing to delete existing files")
    if args.case:
        selected = set(args.case)
        unknown = selected - {case["id"] for case in manifest["cases"]}
        if unknown:
            parser.error("unknown cases: " + ", ".join(sorted(unknown)))
        if args.output_dir is None:
            parser.error("--case requires --output-dir; never truncate the full corpus")
        manifest = {**manifest, "cases": [case for case in manifest["cases"] if case["id"] in selected]}
    generate_tasks(manifest, args.output_dir or TASKS, "regression", args.image, args.offline)
    if args.output_dir is None:
        if PRODUCTION_MANIFEST.exists():
            generate_tasks(load_manifest(PRODUCTION_MANIFEST, "forest.production-cases.v1"), PRODUCTION_TASKS, "production-replay")
        else:
            shutil.rmtree(PRODUCTION_TASKS, ignore_errors=True)


if __name__ == "__main__":
    main()
