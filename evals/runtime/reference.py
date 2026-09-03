#!/usr/bin/env python3
from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

from hidden import PR_CREATED, REFERENCE_RUN, STATE

from setup import GIT, TIME, evidence_push, git, identity, note_add, run, write_files

WORKSPACE = Path("/workspace")
ORIGIN = Path("/origin.git")


def remote_tip(ref: str) -> str | None:
    value = git(WORKSPACE, "ls-remote", "origin", ref)
    return value.split()[0] if value else None


def prepare_private(canonical: str, private: str) -> None:
    tip = remote_tip(canonical)
    if tip:
        git(WORKSPACE, "update-ref", private, tip)
    else:
        subprocess.run([GIT, "update-ref", "-d", private], cwd=WORKSPACE, check=False)


def add_canonical(canonical: str, target: str, payload: dict, actor: str) -> str:
    private = canonical + "-reference"
    prepare_private(canonical, private)
    note_add(WORKSPACE, private, target, payload, actor)
    return private


def review_payload(subject: str, branch: str, revision: str, tracker: str) -> dict:
    return {
        "schema": "forest.review-request.v2",
        "subject": subject,
        "branch": branch,
        "revision": revision,
        "time": TIME,
        "tracker": tracker,
    }


def checks_payload(revision: str, ok: bool) -> dict:
    return {
        "schema": "forest.checks.v1",
        "revision": revision,
        "results": [{"name": "scenario", "ok": ok, "exit": 0 if ok else 1}],
        "time": TIME,
    }


def verdict_payload(revision: str, verdict: str, summary: str) -> dict:
    return {
        "schema": "forest.verdict.v1",
        "revision": revision,
        "verdict": verdict,
        "summary": summary,
        "time": TIME,
    }


def commit_files(start: str, branch: str, files: dict[str, str], actor: str, message: str) -> str:
    git(WORKSPACE, "checkout", "--detach", start)
    git(WORKSPACE, "checkout", "-B", branch)
    write_files(WORKSPACE, files)
    git(WORKSPACE, "add", ".")
    identity(WORKSPACE, actor)
    git(WORKSPACE, "commit", "-m", message)
    return git(WORKSPACE, "rev-parse", "HEAD")


def publish_builder(scenario: dict, state: dict) -> None:
    subject = str(scenario["issue"]["number"])
    publish_builder_subject(scenario, state, subject)


def publish_builder_subject(scenario: dict, state: dict, subject: str) -> None:
    branch = f"forest/{subject}/eval"
    revision = commit_files(state["master_before"], branch, scenario["expected_files"], "builder", "eval: reference implementation")
    if scenario.get("race") == "canonical_note":
        run(GIT, f"--git-dir={ORIGIN}", "update-ref", "refs/notes/forest/review-request", state["race_note_tip"])
    payload = review_payload(subject, branch, revision, "powder" if powder_job_exists(scenario, subject) else "github")
    private = add_canonical("refs/notes/forest/review-request", revision, payload, "builder")
    evidence_push(WORKSPACE, "request", revision, payload, "builder")
    git(WORKSPACE, "push", "--atomic", "origin", f"{private}:refs/notes/forest/review-request", f"{revision}:refs/heads/{branch}")
    PR_CREATED.write_text(json.dumps({"head": branch, "base": "master"}) + "\n")

def powder_job_exists(scenario: dict, subject: str) -> bool:
    return any(job.get("id") == subject for job in scenario.get("powder_jobs", []))


def powder_take(subject: str) -> None:
    powder_script = Path(__file__).resolve().parent / "powder"
    run("/usr/bin/python3", str(powder_script), "take", subject, "--agent", "forest-iron-forest")


def powder_done(subject: str, revision: str) -> None:
    powder_script = Path(__file__).resolve().parent / "powder"
    run(
        "/usr/bin/python3",
        str(powder_script),
        "done",
        subject,
        "--proof",
        revision,
        "--agent",
        "forest-iron-forest",
    )


def publish_verifier(scenario: dict, state: dict, approve: bool) -> None:
    revision = state["candidate"]
    check_ok = scenario["id"] != "verifier-failed-check"
    verdict = "approve" if approve else "changes"
    summary = scenario.get("verdict_summary", "The Revision satisfies the review contract." if approve else "The Revision requires changes.")
    checks = checks_payload(revision, check_ok)
    verdict_body = verdict_payload(revision, verdict, summary)
    checks_private = add_canonical("refs/notes/forest/checks", revision, checks, "verifier")
    verdict_private = add_canonical("refs/notes/forest/verdict", revision, verdict_body, "verifier")
    evidence_push(WORKSPACE, "checks", revision, checks, "verifier")
    evidence_push(WORKSPACE, "verdict", revision, verdict_body, "verifier")
    refspecs = [
        f"{checks_private}:refs/notes/forest/checks",
        f"{verdict_private}:refs/notes/forest/verdict",
    ]
    if approve:
        refspecs.append(f"{revision}:refs/heads/master")
    git(WORKSPACE, "push", "--atomic", "origin", *refspecs)
    if approve and powder_job_exists(scenario, "100"):
        powder_take("100")
        powder_done("100", revision)



def publish_fixer(scenario: dict, state: dict) -> str:
    branch = state["branch"]
    revision = commit_files(state["candidate"], branch, scenario["expected_files"], "fixer", "eval: reference repair")
    payload = review_payload("100", branch, revision, "powder" if powder_job_exists(scenario, "100") else "github")
    private = add_canonical("refs/notes/forest/review-request", revision, payload, "fixer")
    evidence_push(WORKSPACE, "request", revision, payload, "fixer")
    git(WORKSPACE, "push", "--atomic", "origin", f"{private}:refs/notes/forest/review-request", f"{revision}:refs/heads/{branch}")
    return revision



def publish_conflicting_note(canonical: str, target: str, payload: dict) -> None:
    private = add_canonical(canonical, target, payload, "race")
    identity(WORKSPACE, "race")
    git(WORKSPACE, "push", "origin", f"{private}:{canonical}")


def publish_critic_drafts(scenario: dict) -> None:
    repo = "local/eval"
    forest_yaml = WORKSPACE / "forest.yaml"
    if forest_yaml.exists():
        for line in forest_yaml.read_text().splitlines():
            stripped = line.strip()
            if stripped.startswith("repo:"):
                repo = stripped.split(":", 1)[1].strip()
                break
    planted_path = next(iter(scenario.get("planted_files", {}).keys()), "hotspot.go")
    powder_script = Path(__file__).resolve().parent / "powder"
    run("/usr/bin/python3", str(powder_script), "list", "--repo", repo)
    run(
        "/usr/bin/python3",
        str(powder_script),
        "create",
        "--id",
        "if-critic-hotspot",
        "--title",
        f"Dead weight: unused {planted_path} helper",
        "--repo",
        repo,
    )
    run(
        "/usr/bin/python3",
        str(powder_script),
        "note",
        "if-critic-hotspot",
        "--text",
        f"filed-by: critic @ {repo}\ndeployment: eval unknown\nObserved: {planted_path}:5 DeadWeight is unused exported surface. Required: remove it or add a test/use. Proposed spec direction: delete DeadWeight or cover it.",
        "--agent",
        "critic",
    )


def publish_tester_drafts(scenario: dict) -> None:
    repo = "local/eval"
    forest_yaml = WORKSPACE / "forest.yaml"
    if forest_yaml.exists():
        for line in forest_yaml.read_text().splitlines():
            stripped = line.strip()
            if stripped.startswith("repo:"):
                repo = stripped.split(":", 1)[1].strip()
                break
    surface = next(iter(scenario.get("planted_files", {}).keys()), "bin/release")
    failing_example = scenario.get("failing_example", f"python3 {surface} ''")
    powder_script = Path(__file__).resolve().parent / "powder"
    run("/usr/bin/python3", str(powder_script), "list", "--repo", repo)
    run(
        "/usr/bin/python3",
        str(powder_script),
        "create",
        "--id",
        "if-tester-release-channel-boundary",
        "--title",
        f"Under-tested CLI boundary: {surface}",
        "--repo",
        repo,
    )
    run(
        "/usr/bin/python3",
        str(powder_script),
        "note",
        "if-tester-release-channel-boundary",
        "--text",
        f"filed-by: tester @ {repo}\ndeployment: eval unknown\nSurface: {surface}:5 empty-channel boundary. Behaviors: empty channel prints 'channel unset'; non-empty channel prints 'channel: <value>'. Failing example: {failing_example} prints 'channel unset' with no regression test. Acceptance: add a test asserting the empty-channel boundary via {failing_example} and one non-empty case via python3 {surface} canary, both passing in the build. Observed: {surface}:5 has no test covering the empty-channel branch. Required: cover the boundary with a failing-example first test. Proposed test-work: add tests for {surface} empty-channel and non-empty-channel inputs.",
        "--agent",
        "tester",
    )


def main() -> None:
    scenario_path = Path(sys.argv[1])
    scenario = json.loads(scenario_path.read_text())
    state = json.loads(STATE.read_text())
    REFERENCE_RUN.write_text("oracle\n")
    effect = scenario["effect"]
    if effect == "builder_publish":
        publish_builder(scenario, state)
    elif effect == "builder_scope_publish":
        subject = scenario["subject"]
        powder_take(subject)
        publish_builder_subject(scenario, state, subject)
    elif effect == "builder_scope_held_outside":
        pass
    elif effect == "builder_scope_branch_no_match":
        pass
    elif effect == "builder_branch_race":
        branch = f"refs/heads/forest/{scenario['issue']['number']}/eval"
        run(GIT, f"--git-dir={ORIGIN}", "update-ref", branch, state["competitor"])
    elif effect == "verifier_changes":
        publish_verifier(scenario, state, approve=False)
    elif effect == "verifier_approve":
        publish_verifier(scenario, state, approve=True)
    elif effect == "verifier_conflict":
        target = state["candidate"]
        publish_conflicting_note(
            "refs/notes/forest/verdict",
            target,
            verdict_payload(target, "approve", "concurrent conflicting verdict"),
        )
    elif effect == "verifier_approve_race":
        run(GIT, f"--git-dir={ORIGIN}", "update-ref", "refs/heads/master", state["competitor"])
    elif effect == "fixer_publish":
        if powder_job_exists(scenario, "100"):
            powder_take("100")
        publish_fixer(scenario, state)
    elif effect == "fixer_conflict":
        target = commit_files(state["candidate"], state["branch"], scenario["expected_files"], "fixer", "eval: unpublished reference repair")
        publish_conflicting_note(
            "refs/notes/forest/review-request",
            target,
            review_payload("100", state["branch"], target, "powder" if powder_job_exists(scenario, "100") else "github"),
        )
        git(WORKSPACE, "reset", "--hard", state["candidate"])
    elif effect == "fixer_branch_race":
        run(GIT, f"--git-dir={ORIGIN}", "update-ref", f"refs/heads/{state['branch']}", state["competitor"])
    elif effect == "no_effect":
        pass
    elif effect == "critic_drafts":
        publish_critic_drafts(scenario)
    elif effect == "tester_drafts":
        publish_tester_drafts(scenario)
    else:
        raise RuntimeError(f"unknown reference effect: {effect}")


if __name__ == "__main__":
    main()
