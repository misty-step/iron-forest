#!/usr/bin/env python3
"""Exercise one explicit-request delivery through real Forest/Pi, without a model."""
from __future__ import annotations

import hashlib
import json
import os
import pwd
import signal
import subprocess
import time
from pathlib import Path

from hidden import REFERENCE_RUN, SCENARIO, STATE
from reference import run_reference

ROOT = Path("/workspace")
ORIGIN = Path("/origin.git")
RUNTIME = Path(__file__).resolve().parent
LIVE = ROOT / ".forest/runs/live-verifier.json"
LEDGER = ROOT / ".forest/runs.jsonl"


def require(condition: bool, message: str) -> None:
    if not condition:
        raise RuntimeError(message)


def origin_git(*args: str) -> str:
    completed = subprocess.run(
        ["/usr/bin/git", "-c", f"safe.directory={ORIGIN}", f"--git-dir={ORIGIN}", *args],
        text=True, capture_output=True, check=False,
    )
    if completed.returncode:
        raise RuntimeError(f"origin Git read failed: {completed.stderr}")
    return completed.stdout.strip()


def refs() -> dict[str, str]:
    return dict(
        line.split() for line in origin_git("for-each-ref", "--format=%(refname) %(objectname)").splitlines()
    )


def evidence(kind: str, revision: str) -> dict:
    return json.loads(origin_git("show", f"refs/forest/v1/{kind}/{revision}:{kind}.json"))


def records() -> list[dict]:
    return [json.loads(line) for line in LEDGER.read_text().splitlines() if line.strip()] if LEDGER.exists() else []


def record_phase(report: dict, name: str, before: int, completed: subprocess.CompletedProcess[str]) -> dict:
    observed = records()[before:]
    phase = {"name": name, "forest_exit": completed.returncode, "runs": observed,
             "stdout": completed.stdout, "stderr": completed.stderr, "refs": refs()}
    report["phases"].append(phase)
    for row in observed:
        log_path = ROOT / ".forest/runs" / f"{row['run_id']}.log"
        log = log_path.read_text()
        row["log_sha256"] = hashlib.sha256(log.encode()).hexdigest()
        row["log"] = log
    require(len(observed) == 1, f"{name}: expected one retained real Run, observed {len(observed)}")
    row = observed[0]
    require(all(row[key] == 0 for key in ("tokens_in", "tokens_out", "cache_read", "cache_write", "reasoning")),
            f"{name}: deterministic input hook must not consume model tokens")
    require(not LIVE.exists(), f"{name}: live Verifier context survived completed Run cleanup")
    return row


def phase(report: dict, name: str, scenario: dict, state: dict) -> dict:
    before = len(records())
    completed = run_reference(scenario, state)
    row = record_phase(report, name, before, completed)
    require(completed.returncode == 0 and row["exit"] == 0, f"{name}: real Forest Run failed; see retained output and log")
    return row


def interrupt_verifier(report: dict, scenario: dict, state: dict) -> str:
    marker = Path("/tmp/forest-eval-before-publication.json")
    marker.unlink(missing_ok=True)
    paused = {**scenario, "pause_before_publication": str(marker)}
    SCENARIO.write_text(json.dumps(paused) + "\n")
    STATE.write_text(json.dumps(state) + "\n")
    before = len(records())
    before_refs = refs()
    process = subprocess.Popen(
        ["/usr/bin/python3", str(RUNTIME / "reference.py")],
        text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True,
    )
    try:
        deadline = time.monotonic() + 120
        while not marker.exists():
            if process.poll() is not None:
                stdout, stderr = process.communicate()
                record_phase(report, "interrupted-review", before, subprocess.CompletedProcess(process.args, process.returncode, stdout, stderr))
                raise RuntimeError("Verifier ended before reaching the publication interruption point")
            require(time.monotonic() < deadline, "Verifier did not reach the publication interruption point")
            time.sleep(0.05)
        paused_run = json.loads(marker.read_text())
        live = json.loads(LIVE.read_text())
        require(live["run_id"] == paused_run["run_id"], "pause receipt must identify the actual live Verifier")
        require(refs() == before_refs, "paused Verifier changed refs before publication")
        forest = pwd.getpwnam("forest")
        cancellation = subprocess.run(
            ["/usr/local/bin/forest", "run", "cancel", paused_run["run_id"], "--root", str(ROOT), "--json"],
            user=forest.pw_uid, group=forest.pw_gid, extra_groups=[],
            text=True, capture_output=True, check=False,
        )
        require(cancellation.returncode == 0, f"Run cancellation failed: {cancellation.stdout}{cancellation.stderr}")
        report["cancellation"] = json.loads(cancellation.stdout)
        require(report["cancellation"]["data"]["state"] == "cancelled", "paused Run was not cancelled by the public CLI")
        stdout, stderr = process.communicate(timeout=30)
        completed = subprocess.CompletedProcess(process.args, process.returncode, stdout, stderr)
        row = record_phase(report, "interrupted-review", before, completed)
        require(row["run_id"] == paused_run["run_id"], "interrupted Run must remain identifiable in the Ledger")
        require(row["exit"] == 130, "cancelled Run must retain the public cancellation exit in the Ledger")
        require(refs() == before_refs, "interrupted review published a partial Effect")
        return row["run_id"]
    finally:
        if process.poll() is None:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.communicate(timeout=30)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.communicate()
        marker.unlink(missing_ok=True)


def main() -> int:
    report = {
        "schema": "forest.eval.journey.v1",
        "execution": {"kind": "deterministic-oracle", "agent_quality_evidence": False},
        "passed": False,
        "phases": [],
    }
    try:
        require(os.geteuid() == 0 and RUNTIME == Path("/opt/iron-forest-eval") and Path("/.dockerenv").is_file(),
                "journey must run as root inside the evaluation Docker image")
        # Fixture setup replaces /workspace; do not leave our current directory
        # pointing at its removed inode, or query Forest before forest.yaml exists.
        os.chdir("/")
        scenario = {
            "id": "explicit-request-interrupted-delivery",
            "role": "builder",
            "effect": "builder_publish",
            "issue": {"number": 100, "title": "Deliver the ready value", "body": "value.txt must contain ready followed by a newline."},
            "request": {"subject": "100", "instructions": "Deliver GitHub Subject 100: value.txt must contain ready followed by a newline."},
            # Deliberate semantic defect: the declared Check is insufficient.
            # The deterministic Verifier rejects it; this does not test judgment.
            "check": "test -f value.txt",
            "expected_files": {"value.txt": "wrong\n"},
        }
        SCENARIO.parent.mkdir(parents=True, exist_ok=True)
        SCENARIO.write_text(json.dumps(scenario) + "\n")
        setup = subprocess.run(["/usr/bin/python3", str(RUNTIME / "setup.py"), str(SCENARIO)], text=True, capture_output=True, check=False)
        require(setup.returncode == 0, f"journey fixture setup failed: {setup.stderr}")
        os.chdir(ROOT)
        state = json.loads(STATE.read_text())
        version = subprocess.run(["/usr/local/bin/forest", "version", "--json"], text=True, capture_output=True, check=True)
        report["forest_version"] = json.loads(version.stdout)
        report["pi_version"] = subprocess.run(["/usr/local/bin/pi", "--version"], text=True, capture_output=True, check=True).stdout.strip()
        scanner = subprocess.run(["/usr/local/bin/trufflehog", "--version"], text=True, capture_output=True, check=True)
        report["scanner_version"] = (scanner.stdout + scanner.stderr).strip()
        REFERENCE_RUN.write_text("deterministic-oracle\n")
        primary_before = state["master_before"]
        branch = "forest/100/eval"
        phase(report, "implementation-with-planted-defect", scenario, state)
        rejected = refs()[f"refs/heads/{branch}"]
        require(origin_git("show", f"{rejected}:value.txt") == "wrong", "Builder did not publish the planted implementation")
        require(evidence("request", rejected)["subject"] == "100", "initial request must bind the supplied Subject")
        require(refs()["refs/heads/master"] == primary_before, "Builder moved primary")

        request = {**state["request"], "branch": branch, "revision": rejected,
                   "instructions": "Review only this requested Revision against the required ready value and publish the supported decision."}
        state.update(candidate=rejected, branch=branch, request=request)
        review = {**scenario, "role": "verifier", "effect": "verifier_changes",
                  "verdict_summary": "value.txt contains wrong; the requested value is ready."}
        phase(report, "review-rejection", review, state)
        require(evidence("verdict", rejected)["verdict"] == "changes", "Verifier did not publish the rejection")
        require(evidence("checks", rejected)["results"] == [{"name": "scenario", "ok": True, "exit": 0}], "semantic rejection must retain truthful passing declared Checks")
        require(refs()["refs/heads/master"] == primary_before, "changes Verdict moved primary")
        preserved = refs()

        repair = {**scenario, "role": "fixer", "effect": "fixer_publish", "expected_files": {"value.txt": "ready\n"}}
        state["request"] = {**request, "instructions": "Repair the requested rejected Revision: change value.txt from wrong to ready."}
        phase(report, "repair", repair, state)
        after_repair = refs()
        repaired = after_repair[f"refs/heads/{branch}"]
        require(repaired != rejected, "Fixer must publish a fresh Revision")
        require(origin_git("show", f"{repaired}:value.txt") == "ready", "repaired Revision does not satisfy the request")
        require(evidence("request", repaired)["revision"] == repaired, "fresh request does not bind the repaired Revision")
        require(after_repair["refs/heads/master"] == primary_before, "Fixer moved primary")
        for ref, oid in preserved.items():
            if ref != f"refs/heads/{branch}":
                require(after_repair.get(ref) == oid, f"repair changed historical evidence {ref}")

        state.update(candidate=repaired, request={**request, "revision": repaired,
                     "instructions": "Review and deliver only this repaired Revision; value.txt now contains ready."})
        approval = {**review, "effect": "verifier_approve", "verdict_summary": "The requested ready value is present; declared Checks pass."}
        interrupted_id = interrupt_verifier(report, approval, state)
        resumed = phase(report, "fresh-review-after-interruption", approval, state)
        require(resumed["run_id"] != interrupted_id, "recovery must use a fresh supervised Run, not a fabricated session resume")
        require(refs()["refs/heads/master"] == repaired, "recovered approval did not deliver the repaired Revision")
        require(evidence("verdict", repaired)["verdict"] == "approve", "recovered review did not publish approval")
        landed = refs()
        phase(report, "identical-publication-retry", approval, state)
        require(refs() == landed, "identical retry changed publication refs")
        projection = json.loads((SCENARIO.parent / "pr-created.json").read_text())
        require(projection["count"] == 1 and projection["head"] == branch, "journey must retain one human Projection")
        report.update(passed=True, subject="100", rejected_revision=rejected, delivered_revision=repaired,
                      interrupted_run=interrupted_id, recovered_run=resumed["run_id"], final_refs=landed)
    except Exception as error:
        report["error"] = str(error)
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
