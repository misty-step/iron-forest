#!/usr/bin/env python3
from __future__ import annotations

import json
import shutil
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
TASKS = ROOT / "tasks"


def task_toml(case: dict) -> str:
    description = json.dumps(case["summary"])
    return f'''schema_version = "1.4"

artifacts = [{{ source = "/var/lib/forest-eval/bundle", destination = "forest-eval-bundle" }}]

[task]
name = "iron-forest/{case["id"]}"
version = "0.1.0"
description = {description}
authors = [{{ name = "Iron Forest", email = "forest@invalid" }}]
keywords = ["iron-forest", "{case["role"]}", "regression"]

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


def main() -> None:
    manifest = json.loads((ROOT / "cases.json").read_text())
    if manifest.get("schema") != "forest.evals.v1":
        raise RuntimeError("unsupported case manifest schema")
    shutil.rmtree(TASKS, ignore_errors=True)
    for case in manifest["cases"]:
        task = TASKS / case["id"]
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
        (task / "task.toml").write_text(task_toml(case))
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


if __name__ == "__main__":
    main()
