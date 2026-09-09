#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import pwd
import shutil
import signal
import subprocess
import sys
from pathlib import Path

from hidden import FOREST_EXIT, REFERENCE_RUN, SCENARIO, STATE
from setup import TIME

WORKSPACE = Path("/workspace")
ORACLE = Path("/run/forest-eval/oracle")


def run_reference(scenario: dict, state: dict) -> subprocess.CompletedProcess[str]:
    """Run a deterministic, model-free oracle through the production Runner and Pi."""
    if os.geteuid() != 0:
        raise PermissionError("reference orchestration must run as root; the Run executes as forest")
    role = scenario["role"]
    if role not in {"builder", "fixer", "verifier", "critic", "tester"}:
        raise ValueError(f"unsupported reference role: {role}")
    if "request" not in state:
        raise ValueError("setup must resolve state.request, including an explicit null for no authorization")
    forest = pwd.getpwnam("forest")
    runtime = Path(__file__).resolve().parent
    # The protected fixture/grade tree stays root-only. Only the oracle's necessary
    # inputs and entrypoints become readable, outside the managed checkout, for
    # this invocation. An existing directory is not silently removed mid-Run.
    ORACLE.mkdir(mode=0o755)
    previous_handlers = {}
    try:
        (ORACLE / "bin").mkdir(mode=0o755)
        (ORACLE / "results").mkdir(mode=0o700)
        os.chown(ORACLE / "results", forest.pw_uid, forest.pw_gid)
        for source, destination in (
            ("oracle.py", "oracle.py"),
            ("oracle-extension.ts", "oracle-extension.ts"),
            ("oracle-pi.py", "bin/pi"),
        ):
            target = ORACLE / destination
            shutil.copyfile(runtime / source, target)
            target.chmod(0o555 if destination == "bin/pi" else 0o444)
        data = {
            "role": role,
            "effect": scenario["effect"],
            "request": state["request"],
            "base": state["master_before"],
            "time": TIME,
            "files": scenario.get("attempt_files", scenario.get("expected_files")),
            "inspection_paths": list(scenario.get("planted_files", {})),
            "failing_example": scenario.get("failing_example"),
            "verdict_summary": scenario.get("verdict_summary"),
            "semantic_contract": scenario.get("semantic_contract"),
            "pause_before_publication": scenario.get("pause_before_publication"),
        }
        data_path = ORACLE / "input.json"
        data_path.write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")
        data_path.chmod(0o444)
        request_path = ORACLE.parent / "request.json"
        request_path.write_text(json.dumps(state["request"], indent=2, sort_keys=True) + "\n")
        request_path.chmod(0o444)
        environment = {str(key): str(value) for key, value in scenario.get("agent_env", {}).items()}
        # No inherited provider credentials or operator Pi state enter the oracle.
        if any("KEY" in key or "TOKEN" in key or "SECRET" in key for key in environment):
            raise ValueError("oracle agent_env must not supply credentials")
        environment.update({
            "HOME": forest.pw_dir,
            "USER": forest.pw_name,
            "LOGNAME": forest.pw_name,
            "LANG": "C.UTF-8",
            "PATH": f"{ORACLE / 'bin'}:/usr/local/bin:/usr/bin:/bin",
            "FOREST_EVAL_ORACLE_DATA": str(data_path),
            "FOREST_EVAL_ORACLE_RESULT": str(ORACLE / "results" / "input-hook.json"),
        })
        # Do not supply a Run ID, worktree, identity, or live marker. Runner owns
        # all of them, including cleanup on failed publication and no-work paths.
        with subprocess.Popen(
            [str(WORKSPACE / ".iron-forest/bin/forest"), "once", role, "--request", "/run/forest-eval/run-request.json"],
            cwd=WORKSPACE,
            env=environment,
            user=forest.pw_uid,
            group=forest.pw_gid,
            extra_groups=[],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        ) as child:
            def forward_signal(signum, _frame):
                # The child shares this process group; forwarding also covers a
                # caller that signals only the orchestrator. Keep waiting so
                # Runner can cancel its separate Pi group and clean live state.
                child.send_signal(signum)

            for signum in (signal.SIGTERM, signal.SIGINT):
                previous_handlers[signum] = signal.signal(signum, forward_signal)
            stdout, stderr = child.communicate()
            return subprocess.CompletedProcess(child.args, child.returncode, stdout, stderr)
    finally:
        try:
            shutil.rmtree(ORACLE)
        finally:
            for signum, handler in previous_handlers.items():
                signal.signal(signum, handler)


def main() -> int:
    scenario = json.loads(SCENARIO.read_text())
    state = json.loads(STATE.read_text())
    REFERENCE_RUN.write_text("deterministic-oracle\n")
    completed = run_reference(scenario, state)
    FOREST_EXIT.write_text(f"{completed.returncode}\n")
    sys.stdout.write(completed.stdout)
    sys.stderr.write(completed.stderr)
    return completed.returncode


if __name__ == "__main__":
    raise SystemExit(main())
