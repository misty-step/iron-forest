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
LIVE = ROOT / ".iron-forest/runtime/runs/live-verifier.json"
LEDGER = ROOT / ".iron-forest/runtime/runs.jsonl"


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
        log_path = ROOT / ".iron-forest/runtime/runs" / f"{row['run_id']}.log"
        log = log_path.read_text()
        row["log_sha256"] = hashlib.sha256(log.encode()).hexdigest()
        row["log"] = log
    require(len(observed) == 1, f"{name}: expected one retained real Run, observed {len(observed)}")
    row = observed[0]
    require(all(row[key] == 0 for key in ("tokens_in", "tokens_out", "cache_read", "cache_write", "reasoning")),
            f"{name}: deterministic input hook must not consume model tokens")
    require(not LIVE.exists(), f"{name}: live Verifier context survived completed Run cleanup")
    scratch = ROOT / ".iron-forest/runtime/checks"
    require(not scratch.exists() or not any(scratch.iterdir()),
            f"{name}: disposable Check scratch survived")
    if row["exit"] == 0:
        require(not (ROOT / ".iron-forest/runtime/worktrees" / row["run_id"]).exists(),
                f"{name}: successful disposable worktree survived")
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
        worktree = ROOT / ".iron-forest/runtime/worktrees" / paused_run["run_id"]
        def work_git(*args: str) -> bytes:
            return subprocess.check_output(["/usr/bin/git", "-C", str(worktree), *args],
                                           user=forest.pw_uid, group=forest.pw_gid, extra_groups=[])
        committed = b"unpublished committed source\x00\xfc\n"
        (worktree / "committed-source.bin").write_bytes(committed)
        os.chown(worktree / "committed-source.bin", forest.pw_uid, forest.pw_gid)
        work_git("add", "committed-source.bin")
        work_git("-c", "user.name=Fixture", "-c", "user.email=fixture@invalid", "commit", "-m", "unpublished recovery fixture")
        base = work_git("rev-parse", "HEAD").decode().strip()
        work_git("rm", "committed-source.bin")
        tracked = b"interrupted tracked source\x00\xff\n"
        staged = b"\x00\xffstaged\n" * (256 * 1024)
        untracked = b"interrupted untracked source\x00\xfe\n"
        ignored = b"ignored original source\x00\xfd\n"
        (worktree / "value.txt").write_bytes(staged)
        work_git("add", "--", "value.txt")
        (worktree / "value.txt").write_bytes(tracked)
        for name, data in {"interrupted-source.bin": untracked, "ignored-source.bin": ignored}.items():
            (worktree / name).write_bytes(data)
            os.chown(worktree / name, forest.pw_uid, forest.pw_gid)
        with (worktree / ".gitignore").open("a") as ignore:
            ignore.write("\nignored-source.bin\n")
        cancellation = subprocess.run(
            [str(ROOT / ".iron-forest/bin/forest"), "run", "cancel", paused_run["run_id"], "--root", str(ROOT), "--json"],
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
        require(row.get("outcome") == "cancelled", "cancellation outcome was not persisted")
        recovery = row.get("recovery", {})
        require(recovery.get("path") == str(worktree.relative_to(ROOT)), "cancelled source path does not identify its original worktree")
        require(worktree.parent.stat().st_mode & 0o777 == 0o700, "worktree custody directory is not private")
        require(recovery.get("base_revision") == base, "recovery base lost unpublished HEAD")
        work_git("reflog", "expire", "--expire=now", "--all")
        work_git("gc", "--prune=now")
        actual = {"worktree/value.txt": (worktree / "value.txt").read_bytes(),
                  "index/0/value.txt": work_git("show", ":value.txt"),
                  "worktree/interrupted-source.bin": (worktree / "interrupted-source.bin").read_bytes(),
                  "worktree/ignored-source.bin": (worktree / "ignored-source.bin").read_bytes(),
                  "HEAD/committed-source.bin": work_git("show", "HEAD:committed-source.bin")}
        expected = dict(zip(actual, [tracked, staged, untracked, ignored, committed]))
        require(actual == expected, "cancelled native Git source bytes changed")
        require(not (worktree / "committed-source.bin").exists(), "staged deletion was lost")
        require(work_git("rev-parse", "HEAD").decode().strip() == base, "native HEAD changed after GC")
        report["source_recovery"] = {"evidence": recovery,
                                     "members_sha256": {name: hashlib.sha256(data).hexdigest() for name, data in actual.items()},
                                     "members_bytes": {name: len(data) for name, data in actual.items()},
                                     "private_parent_mode": oct(worktree.parent.stat().st_mode & 0o777),
                                     "git_status": work_git("status", "--porcelain", "--ignored").decode(),
                                     "survived_gc": True}
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
        # pointing at its removed inode, or query Forest before .iron-forest/config.yaml exists.
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
        version = subprocess.run([str(ROOT / ".iron-forest/bin/forest"), "version", "--json"], text=True, capture_output=True, check=True)
        report["forest_version"] = json.loads(version.stdout)
        image = json.loads(Path("/run/forest-eval-image.json").read_text())
        actual_kernel = hashlib.sha256((ROOT / ".iron-forest/bin/forest").read_bytes()).hexdigest()
        require(actual_kernel == image["kernel_sha256"], "journey Kernel differs from the selected image")
        require(os.environ.get("FOREST_EVAL_IMAGE_ID") == image["image"], "journey image identity mismatch")
        require(report["forest_version"]["data"]["build_sha"] == image["build_sha"],
                "journey Kernel build revision mismatch")
        require(report["forest_version"]["data"]["dirty"] == image["dirty"], "journey Kernel dirty identity mismatch")
        report["image_inputs"] = {**image, "actual_kernel_sha256": actual_kernel}
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
        require(evidence("verdict", repaired).get("verifier_run_id") == resumed["run_id"],
                "approval must bind the actual recovered Verifier Run")
        landed = refs()
        # ADR 0022 (2026-09-12 amendment): a different Run cannot adopt an
        # existing Verdict, even when its agent-supplied payload is identical.
        retried = phase(report, "different-run-publication-conflict",
                        {**approval, "effect": "verifier_approve_conflict"}, state)
        require(retried["run_id"] != resumed["run_id"], "conflicting retry must use a fresh Verifier Run")
        require(refs() == landed, "different-Run refusal changed publication refs")
        projection = json.loads((SCENARIO.parent / "pr-created.json").read_text())
        require(projection["count"] == 1 and projection["head"] == branch, "journey must retain one human Projection")
        retained = ROOT / ".iron-forest/runtime/worktrees" / interrupted_id
        require(retained.is_dir(), "fresh independent review deleted interrupted source")
        forest = pwd.getpwnam("forest")
        subprocess.run(["/usr/bin/git", "-C", str(ROOT), "worktree", "remove", "--force", str(retained)],
                       user=forest.pw_uid, group=forest.pw_gid, extra_groups=[], check=True)
        require(not retained.exists(), "explicit fixture disposal failed")
        report["source_recovery"]["fixture_disposed_after_delivery"] = True
        report.update(passed=True, subject="100", rejected_revision=rejected, delivered_revision=repaired,
                      interrupted_run=interrupted_id, recovered_run=resumed["run_id"], final_refs=landed)
    except Exception as error:
        report["error"] = str(error)
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
