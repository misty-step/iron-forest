#!/usr/bin/env python3
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import secrets
import shutil
import subprocess
import tempfile
from pathlib import Path

import pwd

from hidden import CANDIDATE_MODEL, POWDER_JOBS, RACE, ROLE, SCENARIO, STATE, ensure


GIT = "/usr/bin/git"
TIME = "2026-08-14T00:00:00Z"
POWDER_JOB_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")


def run(*args: str, cwd: Path | None = None, env: dict[str, str] | None = None, input: str | None = None) -> str:
    completed = subprocess.run(
        list(args),
        cwd=cwd,
        env=env,
        input=input,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if completed.returncode != 0:
        raise RuntimeError(f"command failed ({completed.returncode}): {' '.join(args)}\n{completed.stderr}")
    return completed.stdout.strip()


def git(workspace: Path, *args: str, input: str | None = None) -> str:
    return run(GIT, *args, cwd=workspace, input=input)


def identity(workspace: Path, role: str) -> None:
    name = "Iron Forest " + role.capitalize()
    git(workspace, "config", "user.name", name)
    git(workspace, "config", "user.email", f"{role}@forest.invalid")


def write_files(root: Path, files: dict[str, str]) -> None:
    for relative, content in files.items():
        path = root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)


def scope_config(scope: dict | None) -> str:
    if not scope:
        return ""
    lines = ["scope:"]
    for key, value in scope.items():
        if isinstance(value, list):
            lines.append(f"  {key}:")
            for item in value:
                lines.append(f"    - {item}")
        else:
            lines.append(f"  {key}: {value}")
    return "\n".join(lines) + "\n"


def note_add(workspace: Path, ref: str, target: str, payload: dict, actor: str, layout: str = "flat") -> None:
    identity(workspace, actor)
    with tempfile.NamedTemporaryFile("w", delete=False) as payload_file:
        json.dump(payload, payload_file, separators=(",", ":"), sort_keys=True)
        payload_file.write("\n")
        payload_path = payload_file.name
    try:
        git(workspace, "notes", f"--ref={ref}", "add", "-F", payload_path, target)
    finally:
        os.unlink(payload_path)
    if layout == "fanout":
        fanout_notes(workspace, ref)


def evidence_push(workspace: Path, kind: str, sha: str, payload: dict, actor: str) -> None:
    identity(workspace, actor)
    blob = git(workspace, "hash-object", "-w", "--stdin", input=json.dumps(payload, separators=(",", ":"), sort_keys=True) + "\n")
    tree = git(workspace, "mktree", input=f"100644 blob {blob}\t{kind}.json\n")
    commit = git(workspace, "commit-tree", tree, "-m", f"eval {kind} {sha}")
    git(workspace, "push", "origin", f"{commit}:refs/forest/v1/{kind}/{sha}")


def fanout_notes(workspace: Path, ref: str) -> None:
    old_tip = git(workspace, "rev-parse", ref)
    entries: dict[str, list[tuple[str, str]]] = {}
    for line in git(workspace, "ls-tree", "-r", ref).splitlines():
        metadata, path = line.split("\t", 1)
        mode, kind, oid = metadata.split()
        if mode != "100644" or kind != "blob":
            raise RuntimeError(f"unexpected note entry: {line}")
        target = path.replace("/", "")
        entries.setdefault(target[:2], []).append((target[2:], oid))
    root_lines: list[str] = []
    for prefix, children in sorted(entries.items()):
        subtree_input = "".join(f"100644 blob {oid}\t{suffix}\n" for suffix, oid in sorted(children))
        subtree = git(workspace, "mktree", input=subtree_input)
        root_lines.append(f"040000 tree {subtree}\t{prefix}\n")
    root_tree = git(workspace, "mktree", input="".join(root_lines))
    new_tip = git(workspace, "commit-tree", root_tree, "-p", old_tip, input="Normalize fixture notes to fanout layout\n")
    git(workspace, "update-ref", ref, new_tip, old_tip)


def push_note(workspace: Path, ref: str) -> None:
    git(workspace, "push", "origin", f"{ref}:{ref}")


def prepare_object(workspace: Path, oid: str) -> None:
    temporary = "refs/forest-eval/prepared"
    git(workspace, "push", "origin", f"{oid}:{temporary}")
    git(workspace, "push", "origin", f":{temporary}")


def configure_declaration(
    agent_file: Path,
    *,
    model: str | None,
    thinking: str | None,
    tools: str | None,
    prompt_append: str | None,
) -> None:
    replacements = {
        "model": model,
        "thinking": thinking,
        "tools": tools,
    }
    lines = agent_file.read_text().splitlines()
    found: set[str] = set()
    for index, line in enumerate(lines):
        name, separator, _ = line.partition(":")
        if separator and name in replacements and replacements[name] is not None:
            lines[index] = f"{name}: {replacements[name]}"
            found.add(name)
    missing = {name for name, value in replacements.items() if value is not None and name not in found}
    if missing:
        raise RuntimeError(f"declaration has no fields: {', '.join(sorted(missing))}")
    text = "\n".join(lines) + "\n"
    if prompt_append:
        text += prompt_append
        if not text.endswith("\n"):
            text += "\n"
    agent_file.write_text(text)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("scenario", type=Path)
    parser.add_argument("--model")
    parser.add_argument("--thinking")
    parser.add_argument("--tools")
    parser.add_argument("--prompt-append")
    args = parser.parse_args()
    scenario = json.loads(args.scenario.read_text())

    workspace = Path("/workspace")
    origin = Path("/origin.git")
    hidden = ensure()
    shutil.rmtree(workspace, ignore_errors=True)
    shutil.rmtree(origin, ignore_errors=True)
    for stale in ("state.json", "race.json", "race-triggered", "forest-exit", "candidate-model", "reference-run", "scenario.json", "role", "pr-created.json", "issue-created.json", "powder-jobs.json", "powder-ops.jsonl"):
        (hidden / stale).unlink(missing_ok=True)
    shutil.rmtree(hidden / "claim-state", ignore_errors=True)
    SCENARIO.write_text(json.dumps(scenario, indent=2, sort_keys=True) + "\n")
    powder_jobs = scenario.get("powder_jobs", [])
    for job in powder_jobs:
        job_id = job.get("id")
        if not isinstance(job_id, str) or POWDER_JOB_ID.fullmatch(job_id) is None:
            raise ValueError(f"Powder job id {job_id!r} is not a slug")
        job.setdefault("repo", "local/eval")
        job.setdefault("lease", None)
        local_claim = job.pop("_local_claim", False)
        if local_claim:
            if job["lease"] is None:
                raise ValueError(f"Powder job {job['id']!r} cannot seed a claim without a live lease")
            claim = secrets.token_urlsafe(32)
            job["_claim_hash"] = hashlib.sha256(claim.encode()).hexdigest()
            claims_root = (hidden / "claim-state" / "powder" / "claims" / "eval-origin").resolve()
            claim_file = claims_root / job_id
            if claim_file.parent != claims_root:
                raise ValueError(f"Powder claim path escapes its state root: {job_id!r}")
            claim_file.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            os.chmod(claim_file.parent, 0o700)
            claim_file.write_text(claim)
            os.chmod(claim_file, 0o600)
    if powder_jobs:
        POWDER_JOBS.write_text(json.dumps(powder_jobs, indent=2, sort_keys=True) + "\n")
    run(GIT, "init", "--bare", "--initial-branch=master", str(origin))
    run(GIT, "init", "--initial-branch=master", str(workspace))
    git(workspace, "remote", "add", "origin", str(origin))
    identity(workspace, "builder")

    shutil.copytree("/opt/iron-forest/agents", workspace / "agents")
    if args.model or args.thinking or args.tools or args.prompt_append:
        configure_declaration(
            workspace / "agents" / scenario["role"] / "agent.md",
            model=args.model,
            thinking=args.thinking,
            tools=args.tools,
            prompt_append=args.prompt_append,
        )
    declaration_model = next(
        line.split(":", 1)[1].strip()
        for line in (workspace / "agents" / scenario["role"] / "agent.md").read_text().splitlines()
        if line.startswith("model:")
    )
    CANDIDATE_MODEL.write_text(declaration_model + "\n")
    base_files = {
        "value.txt": "old\n",
        "CONTRACT.md": "The requested final value is documented by the selected Issue or rejection evidence.\n",
    }
    base_files.update(scenario.get("planted_files", {}))
    write_files(workspace, base_files)
    check = json.dumps(scenario.get("check", "true"))
    config = (
        "repo: local/eval\n"
        + scope_config(scenario.get("scope"))
        + "agents:\n"
        "  builder: {poll: \"true\", interval: 1}\n"
        "  verifier: {poll: \"true\", interval: 1}\n"
        "  fixer: {poll: \"true\", interval: 1}\n"
        "  critic: {poll: \"true\", interval: 1}\n"
        "  tester: {poll: \"true\", interval: 1}\n"
        "checks:\n"
        f"  - name: scenario\n    run: {check}\n"
    )
    (workspace / "forest.yaml").write_text(config)
    git(workspace, "add", ".")
    git(workspace, "commit", "-m", "eval: initial repository")
    base_sha = git(workspace, "rev-parse", "HEAD")
    git(workspace, "push", "-u", "origin", "master")

    state: dict[str, str | None] = {
        "base": base_sha,
        "master_before": base_sha,
        "candidate": None,
        "competitor": None,
        "branch": None,
    }
    candidate_files = scenario.get("candidate_files")
    if candidate_files is not None:
        branch = "forest/100/candidate"
        git(workspace, "checkout", "-b", branch)
        write_files(workspace, candidate_files)
        git(workspace, "add", ".")
        git(workspace, "commit", "-m", "candidate: implement requested change")
        candidate = git(workspace, "rev-parse", "HEAD")
        git(workspace, "push", "-u", "origin", branch)
        state["candidate"] = candidate
        state["branch"] = branch

        request_actor = "fixer" if scenario["role"] == "fixer" and scenario.get("request_actor") == "fixer" else "builder"
        review_request = {
            "schema": "forest.review-request.v2",
            "subject": "100",
            "branch": branch,
            "revision": candidate,
            "time": TIME,
            "tracker": "powder" if scenario.get("powder_jobs") else "github",
        }
        note_add(workspace, "refs/notes/forest/review-request", candidate, review_request, request_actor, scenario.get("note_layout", "flat"))
        push_note(workspace, "refs/notes/forest/review-request")
        evidence_push(workspace, "request", candidate, review_request, request_actor)


        if scenario["role"] == "fixer":
            checks = {
                "schema": "forest.checks.v1",
                "revision": candidate,
                "results": [{"name": "scenario", "ok": False, "exit": 1}],
                "time": TIME,
            }
            verdict = {
                "schema": "forest.verdict.v1",
                "revision": candidate,
                "verdict": "changes",
                "summary": scenario["rejection"],
                "time": TIME,
            }
            note_add(workspace, "refs/notes/forest/checks", candidate, checks, "verifier", scenario.get("note_layout", "flat"))
            note_add(workspace, "refs/notes/forest/verdict", candidate, verdict, "verifier", scenario.get("note_layout", "flat"))
            push_note(workspace, "refs/notes/forest/checks")
            push_note(workspace, "refs/notes/forest/verdict")
            evidence_push(workspace, "checks", candidate, checks, "verifier")
            evidence_push(workspace, "verdict", candidate, verdict, "verifier")


        git(workspace, "checkout", "master")

    if scenario.get("stale"):
        (workspace / "master-advance.txt").write_text("advanced\n")
        git(workspace, "add", "master-advance.txt")
        git(workspace, "commit", "-m", "eval: advance master")
        advanced = git(workspace, "rev-parse", "HEAD")
        git(workspace, "push", "origin", "master")
        state["master_before"] = advanced

    if scenario.get("race") in {"branch", "approve_master"}:
        start = state["candidate"] if scenario["role"] == "fixer" else state["master_before"]
        git(workspace, "checkout", "--detach", str(start))
        (workspace / "competitor.txt").write_text(f"{scenario['id']}\n")
        git(workspace, "add", "competitor.txt")
        identity(workspace, "builder")
        git(workspace, "commit", "-m", "race: concurrent writer")
        competitor = git(workspace, "rev-parse", "HEAD")
        prepare_object(workspace, competitor)
        state["competitor"] = competitor
        git(workspace, "checkout", "master")

    if scenario.get("race") == "canonical_note":
        canonical = "refs/notes/forest/review-request"
        remote_tip = ""
        temporary = "refs/notes/forest/race-prepared"
        race_payload = {
            "schema": "forest.review-request.v2",
            "subject": "999",
            "branch": "forest/999/race",
            "revision": base_sha,
            "time": TIME,
        }
        note_add(workspace, temporary, base_sha, race_payload, "builder")
        state["race_note_tip"] = git(workspace, "rev-parse", temporary)
        prepare_object(workspace, str(state["race_note_tip"]))
        git(workspace, "update-ref", "-d", temporary)

    git(workspace, "checkout", "master")
    ROLE.write_text(scenario["role"] + "\n")
    run_dir = Path("/run/forest-eval")
    run_dir.mkdir(parents=True, exist_ok=True)
    (run_dir / "role").write_text(scenario["role"] + "\n")
    os.chmod(run_dir / "role", 0o644)
    STATE.write_text(json.dumps(state, indent=2, sort_keys=True) + "\n")
    if scenario.get("race"):
        RACE.write_text(json.dumps({"type": scenario["race"], **state}, indent=2, sort_keys=True) + "\n")

    try:
        forest = pwd.getpwnam("forest")
    except KeyError:
        forest = None
    if forest is not None:
        for root in (workspace, origin):
            for dirpath, dirnames, filenames in os.walk(root):
                os.chown(dirpath, forest.pw_uid, forest.pw_gid)
                for name in dirnames + filenames:
                    os.chown(os.path.join(dirpath, name), forest.pw_uid, forest.pw_gid)


if __name__ == "__main__":
    main()
