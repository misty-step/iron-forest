#!/usr/bin/env python3
from __future__ import annotations

import json
import shutil
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
TASKS = ROOT / "tasks"
PRODUCTION_MANIFEST = ROOT / "production-cases.json"
PRODUCTION_TASKS = ROOT / "tasks-production"


def task_toml(case: dict, suite: str) -> str:
    description = json.dumps(case["summary"])
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
network_mode = "allowlist"
allowed_hosts = ["openrouter.ai"]
env = {{ FOREST_EVAL_JUDGE_API_KEY = "${{FOREST_EVAL_JUDGE_API_KEY:-}}", FOREST_EVAL_JUDGE_MODEL = "${{FOREST_EVAL_JUDGE_MODEL:-}}", FOREST_EVAL_FORENSIC_JUDGE_MODEL = "${{FOREST_EVAL_FORENSIC_JUDGE_MODEL:-}}", FOREST_EVAL_REQUIRE_JUDGE = "${{FOREST_EVAL_REQUIRE_JUDGE:-0}}" }}
environment_mode = "separate"

[verifier.environment]
docker_image = "iron-forest-eval:local"
network_mode = "no-network"
workdir = "/tests"

[[verifier.collect]]
command = "python3 /opt/iron-forest-eval/collect.py"
user = "root"
timeout_sec = 120.0

[agent]
user = "forest"
network_mode = "allowlist"
allowed_hosts = ["openrouter.ai"]

[environment]
docker_image = "iron-forest-eval:local"
network_mode = "no-network"
env = {{ OPENROUTER_API_KEY = "${{OPENROUTER_API_KEY:-}}" }}
'''


def generate_tasks(manifest: dict, tasks_dir: Path, suite: str) -> None:
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
            "Use only the repository, Git remotes, and the normal gh interface.\n"
        )
        (task / "task.toml").write_text(task_toml(case, suite))
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
    generate_tasks(load_manifest(ROOT / "cases.json", "forest.evals.v1"), TASKS, "regression")
    if PRODUCTION_MANIFEST.exists():
        generate_tasks(load_manifest(PRODUCTION_MANIFEST, "forest.production-cases.v1"), PRODUCTION_TASKS, "production-replay")
    else:
        shutil.rmtree(PRODUCTION_TASKS, ignore_errors=True)


if __name__ == "__main__":
    main()
