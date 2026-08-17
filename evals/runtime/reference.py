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


def review_payload(issue: int, branch: str, revision: str) -> dict:
    return {
        "schema": "forest.review-request.v1",
        "issue": issue,
        "branch": branch,
        "revision": revision,
        "time": TIME,
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
    issue = scenario["issue"]
    branch = f"forest/{issue['number']}-eval"
    revision = commit_files(state["master_before"], branch, scenario["expected_files"], "builder", "eval: reference implementation")
    if scenario.get("race") == "canonical_note":
        run(GIT, f"--git-dir={ORIGIN}", "update-ref", "refs/notes/forest/review-request", state["race_note_tip"])
    payload = review_payload(issue["number"], branch, revision)
    private = add_canonical("refs/notes/forest/review-request", revision, payload, "builder")
    evidence_push(WORKSPACE, "request", revision, payload, "builder")
    git(WORKSPACE, "push", "--atomic", "origin", f"{private}:refs/notes/forest/review-request", f"{revision}:refs/heads/{branch}")
    PR_CREATED.write_text(json.dumps({"head": branch, "base": "master"}) + "\n")


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



def publish_fixer(scenario: dict, state: dict) -> str:
    branch = state["branch"]
    revision = commit_files(state["candidate"], branch, scenario["expected_files"], "fixer", "eval: reference repair")
    payload = review_payload(100, branch, revision)
    private = add_canonical("refs/notes/forest/review-request", revision, payload, "fixer")
    evidence_push(WORKSPACE, "request", revision, payload, "fixer")
    git(WORKSPACE, "push", "--atomic", "origin", f"{private}:refs/notes/forest/review-request", f"{revision}:refs/heads/{branch}")
    return revision



def publish_conflicting_note(canonical: str, target: str, payload: dict) -> None:
    private = add_canonical(canonical, target, payload, "race")
    identity(WORKSPACE, "race")
    git(WORKSPACE, "push", "origin", f"{private}:{canonical}")


def main() -> None:
    scenario_path = Path(sys.argv[1])
    scenario = json.loads(scenario_path.read_text())
    state = json.loads(STATE.read_text())
    REFERENCE_RUN.write_text("oracle\n")
    effect = scenario["effect"]
    if effect == "builder_publish":
        publish_builder(scenario, state)
    elif effect == "builder_branch_race":
        branch = f"refs/heads/forest/{scenario['issue']['number']}-eval"
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
        publish_fixer(scenario, state)
    elif effect == "fixer_conflict":
        target = commit_files(state["candidate"], state["branch"], scenario["expected_files"], "fixer", "eval: unpublished reference repair")
        publish_conflicting_note(
            "refs/notes/forest/review-request",
            target,
            review_payload(100, state["branch"], target),
        )
        git(WORKSPACE, "reset", "--hard", state["candidate"])
    elif effect == "fixer_branch_race":
        run(GIT, f"--git-dir={ORIGIN}", "update-ref", f"refs/heads/{state['branch']}", state["competitor"])
    elif effect == "no_effect":
        pass
    else:
        raise RuntimeError(f"unknown reference effect: {effect}")


if __name__ == "__main__":
    main()
