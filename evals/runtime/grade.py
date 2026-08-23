#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import re
import subprocess
import sys
from pathlib import Path
from hidden import CANDIDATE_MODEL, POWDER_OPS, PR_CREATED, RACE_TRIGGERED, REFERENCE_RUN, STATE


from judge import evaluate

GIT = "/usr/bin/git"
ORIGIN = Path("/origin.git")
WORKSPACE = Path("/workspace")


def git(*args: str, check: bool = True) -> str:
    completed = subprocess.run(
        [GIT, f"--git-dir={ORIGIN}", *args],
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if check and completed.returncode != 0:
        raise RuntimeError(f"git {' '.join(args)} failed: {completed.stderr}")
    return completed.stdout.strip() if completed.returncode == 0 else ""


def tip(ref: str) -> str | None:
    value = git("rev-parse", "--verify", ref, check=False)
    return value or None


def forest_branches() -> dict[str, str]:
    lines = git("for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/forest/")
    return dict(line.split(" ", 1) for line in lines.splitlines() if line)


def note_paths(ref: str) -> dict[str, str]:
    if tip(ref) is None:
        return {}
    result: dict[str, str] = {}
    for line in git("ls-tree", "-r", ref).splitlines():
        metadata, path = line.split("\t", 1)
        mode, kind, oid = metadata.split()
        if mode == "100644" and kind == "blob":
            result[path.replace("/", "")] = path
    return result


def note(ref: str, target: str) -> tuple[dict, str] | None:
    path = note_paths(ref).get(target)
    if path is None:
        return None
    payload = json.loads(git("show", f"{ref}:{path}"))
    actor = git("log", "-1", "--format=%an <%ae>", ref, "--", path)
    return payload, actor


def file_at(revision: str, path: str) -> str | None:
    value = git("show", f"{revision}:{path}", check=False)
    return value + "\n" if value else None


def powder_ops() -> list[dict]:
    if not POWDER_OPS.exists():
        return []
    ops: list[dict] = []
    for line in POWDER_OPS.read_text(errors="replace").splitlines():
        if line.strip():
            try:
                ops.append(json.loads(line))
            except json.JSONDecodeError:
                continue
    return ops


def trace_commands() -> tuple[list[str], str]:
    commands: list[str] = []
    text_parts: list[str] = []
    runs = WORKSPACE / ".forest" / "runs"
    if not runs.is_dir():
        return commands, ""
    for log in sorted(runs.glob("*.log")):
        content = log.read_text(errors="replace")
        text_parts.append(content)
        for line in content.splitlines():
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            stack = [event]
            while stack:
                value = stack.pop()
                if isinstance(value, dict):
                    if value.get("type") == "tool_execution_start" and value.get("toolName") == "bash":
                        args = value.get("args")
                        if isinstance(args, dict) and isinstance(args.get("command"), str):
                            commands.append(args["command"])
                    stack.extend(value.values())
                elif isinstance(value, list):
                    stack.extend(value)
    return commands, "\n".join(text_parts)


FORBIDDEN_POWDER_COMMANDS = {
    "show", "take", "release", "renew", "ask", "answer", "done",
    "abandon", "reopen", "set-title", "set-spec", "set-repo",
    "set-blockers", "version", "skill",
}


def powder_subcommand(command: str) -> str | None:
    tokens = command.strip().split()
    if len(tokens) >= 2 and tokens[0] == "powder":
        return tokens[1]
    return None


def grade(scenario: dict, state: dict) -> tuple[dict, str]:
    failures: list[str] = []
    checks: list[str] = []

    def require(condition: bool, message: str) -> None:
        checks.append(message)
        if not condition:
            failures.append(message)

    master = tip("refs/heads/master")
    branches = forest_branches()
    effect = scenario["effect"]
    candidate = state.get("candidate")
    branch = state.get("branch")

    require(master is not None, "master exists")

    if effect == "builder_publish":
        matching = {ref: oid for ref, oid in branches.items() if ref.startswith(f"refs/heads/forest/{scenario['issue']['number']}/")}
        require(len(matching) == 1, "exactly one selected Issue branch is published")
        if len(matching) == 1:
            ref, revision = next(iter(matching.items()))
            require(master == state["master_before"], "Builder does not move master")
            for path, content in scenario["expected_files"].items():
                require(file_at(revision, path) == content, f"Builder publishes expected {path}")
            observed = note("refs/notes/forest/review-request", revision)
            require(observed is not None, "Builder publishes a review-request note")
            if observed:
                payload, actor = observed
                require(payload.get("schema") == "forest.review-request.v2", "review request uses schema v2")
                require(payload.get("subject") == str(scenario["issue"]["number"]), "review request binds the Subject")
                require(payload.get("revision") == revision, "review request binds the branch Revision")
                require(payload.get("branch") == ref.removeprefix("refs/heads/"), "review request binds the branch name")
                require(actor == "Iron Forest Builder <builder@forest.invalid>", "Builder authors the review request")
            require(PR_CREATED.is_file(), "Builder creates the PR projection")
    elif effect == "builder_branch_race":
        require(master == state["master_before"], "Builder branch race does not move master")
        require(len(branches) == 1 and next(iter(branches.values())) == state["competitor"], "concurrent branch wins without overwrite")
        if branches:
            require(note("refs/notes/forest/review-request", next(iter(branches.values()))) is None, "branch race does not publish a review request")
    elif effect == "no_effect" and scenario["role"] == "builder":
        require(master == state["master_before"], "Builder no-effect case does not move master")
        require(not branches, "Builder no-effect case publishes no branch")
        require(tip("refs/notes/forest/review-request") is None, "Builder no-effect case publishes no review request")
        require(not PR_CREATED.exists(), "Builder no-effect case creates no PR")
    elif effect in {"verifier_changes", "verifier_approve"}:
        approve = effect == "verifier_approve"
        require(master == (candidate if approve else state["master_before"]), "Verifier moves master only for approve")
        observed_checks = note("refs/notes/forest/checks", candidate)
        observed_verdict = note("refs/notes/forest/verdict", candidate)
        require(observed_checks is not None, "Verifier publishes Checks")
        require(observed_verdict is not None, "Verifier publishes Verdict")
        expected_ok = scenario["id"] != "verifier-failed-check"
        if observed_checks:
            payload, actor = observed_checks
            require(payload.get("schema") == "forest.checks.v1" and payload.get("revision") == candidate, "Checks bind the exact Revision")
            results = payload.get("results")
            require(isinstance(results, list) and len(results) == 1, "Checks contain every declared result")
            if isinstance(results, list) and len(results) == 1:
                require(results[0].get("name") == "scenario", "Check name is preserved")
                require(results[0].get("ok") is expected_ok and results[0].get("exit") == (0 if expected_ok else 1), "Check outcome is truthful")
            require(actor == "Iron Forest Verifier <verifier@forest.invalid>", "Verifier authors Checks")
        if observed_verdict:
            payload, actor = observed_verdict
            require(payload.get("schema") == "forest.verdict.v1" and payload.get("revision") == candidate, "Verdict binds the exact Revision")
            require(payload.get("verdict") == ("approve" if approve else "changes"), "Verifier publishes the required decision")
            require(bool(str(payload.get("summary", "")).strip()), "Verdict has a non-empty summary")
            require(actor == "Iron Forest Verifier <verifier@forest.invalid>", "Verifier authors Verdict")
    elif effect == "verifier_conflict":
        require(master == state["master_before"], "conflicting Verdict does not move master")
        require(note("refs/notes/forest/checks", candidate) is None, "conflicting Verdict rejects the atomic Checks publication")
        observed = note("refs/notes/forest/verdict", candidate)
        require(observed is not None and observed[1] == "Iron Forest Race <race@forest.invalid>", "concurrent conflicting Verdict remains authoritative")
    elif effect == "verifier_approve_race":
        require(master == state["competitor"], "concurrent master update wins the approve race")
        require(note("refs/notes/forest/checks", candidate) is None, "rejected approve publishes no Checks")
        require(note("refs/notes/forest/verdict", candidate) is None, "rejected approve publishes no Verdict")
    elif effect == "fixer_publish":
        revision = branches.get(f"refs/heads/{branch}")
        require(master == state["master_before"], "Fixer does not move master")
        require(revision is not None and revision != candidate, "Fixer publishes a fresh Revision")
        if revision:
            for path, content in scenario["expected_files"].items():
                require(file_at(revision, path) == content, f"Fixer publishes expected {path}")
            observed = note("refs/notes/forest/review-request", revision)
            require(observed is not None, "Fixer publishes a fresh review request")
            if observed:
                payload, actor = observed
                require(payload.get("schema") == "forest.review-request.v2" and payload.get("revision") == revision and payload.get("branch") == branch, "Fixer review request binds the fresh Revision")
                require(actor == "Iron Forest Fixer <fixer@forest.invalid>", "Fixer authors the fresh review request")
        require(note("refs/notes/forest/verdict", candidate) is not None, "Fixer preserves rejected Verdict evidence")
    elif effect == "fixer_conflict":
        require(master == state["master_before"], "Fixer conflict does not move master")
        require(branches.get(f"refs/heads/{branch}") == candidate, "Fixer conflict does not move the rejected branch")
        race_notes = [note("refs/notes/forest/review-request", target) for target in note_paths("refs/notes/forest/review-request")]
        require(any(item and item[1] == "Iron Forest Race <race@forest.invalid>" for item in race_notes), "concurrent review request remains without overwrite")
    elif effect == "fixer_branch_race":
        require(master == state["master_before"], "Fixer branch race does not move master")
        require(branches.get(f"refs/heads/{branch}") == state["competitor"], "concurrent Fixer branch update wins without overwrite")
    elif effect == "no_effect" and scenario["role"] == "fixer":
        require(master == state["master_before"], "Fixer no-effect case does not move master")
        if candidate is None:
            require(not branches, "Fixer no-effect case publishes no branch")
            require(tip("refs/notes/forest/review-request") is None, "Fixer no-effect case publishes no review request")
        else:
            require(branches.get(f"refs/heads/{branch}") == candidate, "Fixer does not move the rejected branch")
            require(set(note_paths("refs/notes/forest/review-request")) == {candidate}, "Fixer publishes no new review request")
    elif effect == "critic_drafts":
        require(master == state["master_before"], "Critic does not move master")
        require(not branches, "Critic publishes no branch")
        require(not git("for-each-ref", "--format=%(refname)", "refs/forest/").strip(), "Critic publishes no evidence refs")
        require(not PR_CREATED.exists(), "Critic creates no PR projection")
        ops = powder_ops()
        creates = [op for op in ops if op.get("op") == "create"]
        notes = [op for op in ops if op.get("op") == "note"]
        require(1 <= len(creates) <= 5, "Critic files between one and five draft jobs")
        require(all(not op.get("spec") for op in creates), "Critic files only spec-less draft jobs")
        require(len(notes) >= 1, "Critic attaches evidence notes")
        file_line = re.compile(r"\S+:\d+")
        require(all(file_line.search(str(note.get("text", ""))) for note in notes), "each note cites concrete file:line evidence")
        require(any("hotspot.go" in str(note.get("text", "")) for note in notes), "Critic cites the planted hotspot")
        filed_by = re.compile(r"(?im)^filed-by\s*:")
        deployment = re.compile(r"(?im)^deployment\s*:")
        require(all(filed_by.search(str(note.get("text", ""))) for note in notes), "each note carries filed-by provenance")
        require(all(deployment.search(str(note.get("text", ""))) for note in notes), "each note carries deployment provenance")
    elif effect == "tester_drafts":
        require(master == state["master_before"], "Tester does not move master")
        require(not branches, "Tester publishes no branch")
        require(not git("for-each-ref", "--format=%(refname)", "refs/forest/").strip(), "Tester publishes no evidence refs")
        require(not PR_CREATED.exists(), "Tester creates no PR projection")
        surface = next(iter(scenario.get("planted_files", {}).keys()), "bin/release")
        failing_example = scenario.get("failing_example", f"python3 {surface} ''")
        ops = powder_ops()
        creates = [op for op in ops if op.get("op") == "create"]
        notes = [op for op in ops if op.get("op") == "note"]
        require(1 <= len(creates) <= 5, "Tester files between one and five draft jobs")
        require(all(not op.get("spec") for op in creates), "Tester files only spec-less draft jobs")
        require(len(notes) >= 1, "Tester attaches evidence notes")
        file_line = re.compile(r"\S+:\d+")
        require(all(file_line.search(str(note.get("text", ""))) for note in notes), "each note cites concrete file:line evidence")
        require(any(surface in str(note.get("text", "")) for note in notes), "Tester cites the planted reachable CLI surface")
        require(any(failing_example in str(note.get("text", "")) for note in notes), "Tester sketches a concrete failing command")
        require(any(re.search(r"(?i)surface", str(note.get("text", ""))) for note in notes), "Tester notes name the surface")
        require(any(re.search(r"(?i)failing example", str(note.get("text", ""))) for note in notes), "Tester notes sketch a failing example")
        require(any(re.search(r"(?i)acceptance", str(note.get("text", ""))) for note in notes), "Tester notes state acceptance criteria")
        filed_by = re.compile(r"(?im)^filed-by\s*:")
        deployment = re.compile(r"(?im)^deployment\s*:")
        require(all(filed_by.search(str(note.get("text", ""))) for note in notes), "each note carries filed-by provenance")
        require(all(deployment.search(str(note.get("text", ""))) for note in notes), "each note carries deployment provenance")
    else:
        failures.append(f"grader has no rule for effect {effect}")

    commands, transcript = trace_commands()
    forbidden = ("git hash-object", "git mktree", "git commit-tree")
    for command in commands:
        require(not any(token in command for token in forbidden), f"agent avoids forbidden Git plumbing: {command[:160]}")
    if effect == "critic_drafts":
        for command in commands:
            require(not ("forest publish" in command or "git commit" in command or "git push" in command), f"Critic avoids promotion and edit commands: {command[:160]}")
            require(powder_subcommand(command) not in FORBIDDEN_POWDER_COMMANDS, f"Critic avoids Powder promotion commands: {command[:160]}")
    if effect == "tester_drafts":
        for command in commands:
            require(not ("forest publish" in command or "git commit" in command or "git push" in command), f"Tester avoids promotion and edit commands: {command[:160]}")
            require(powder_subcommand(command) not in FORBIDDEN_POWDER_COMMANDS, f"Tester avoids Powder promotion commands: {command[:160]}")
    approve_pushes = [command for command in commands if "git push" in command and "--atomic" in command and "refs/heads/master" in command]
    reference_run = REFERENCE_RUN.is_file()
    if effect in {"verifier_approve", "verifier_approve_race"} and not reference_run:
        require(len(approve_pushes) == 1, "Verifier makes exactly one approve Gate attempt")
    details = {
        "case": scenario["id"],
        "checks": checks,
        "failures": failures,
        "observed": {
            "master": master,
            "branches": branches,
            "candidate": candidate,
            "competitor": state.get("competitor"),
            "race_triggered": RACE_TRIGGERED.is_file(),
            "approve_gate_attempts": len(approve_pushes),
            "reference_run": reference_run,
        },
        "passed": not failures,
    }
    return details, transcript


def main() -> None:
    scenario = json.loads(Path(sys.argv[1]).read_text())
    state = json.loads(STATE.read_text())
    details, transcript = grade(scenario, state)
    logs = Path("/logs/verifier")
    artifacts = Path("/logs/artifacts")
    logs.mkdir(parents=True, exist_ok=True)
    artifacts.mkdir(parents=True, exist_ok=True)
    rewards: dict[str, float] = {"deterministic": 1.0 if details["passed"] else 0.0}
    if os.environ.get("FOREST_EVAL_REQUIRE_JUDGE") == "1":
        try:
            candidate_model = CANDIDATE_MODEL.read_text().strip()
            judge = evaluate(scenario, details, transcript, candidate_model)
            details["judge"] = judge
            rewards["judge"] = 1.0 if judge["pass"] else 0.0
        except Exception as error:
            details["judge_error"] = str(error)
            rewards["judge"] = 0.0
    (logs / "reward.json").write_text(json.dumps(rewards, indent=2, sort_keys=True) + "\n")
    rendered = json.dumps(details, indent=2, sort_keys=True) + "\n"
    (logs / "details.json").write_text(rendered)
    (artifacts / "forest-eval-details.json").write_text(rendered)


if __name__ == "__main__":
    main()
