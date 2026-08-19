#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path
from hidden import RACE, RACE_TRIGGERED


GIT = "/usr/bin/git"
ORIGIN = "/origin.git"
TIME = "2026-08-14T00:00:00Z"


def run(*args: str, cwd: Path, env: dict[str, str] | None = None) -> str:
    completed = subprocess.run(
        list(args), cwd=cwd, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False
    )
    if completed.returncode != 0:
        raise RuntimeError(f"race command failed: {' '.join(args)}\n{completed.stderr}")
    return completed.stdout.strip()


def restore_origin_owner() -> None:
    stat = os.stat(ORIGIN)
    for path, dirs, files in os.walk(ORIGIN):
        os.chown(path, stat.st_uid, stat.st_gid)
        for name in dirs + files:
            os.chown(os.path.join(path, name), stat.st_uid, stat.st_gid)


def race_env() -> dict[str, str]:
    env = os.environ.copy()
    env.update({
        "GIT_AUTHOR_NAME": "Iron Forest Race",
        "GIT_AUTHOR_EMAIL": "race@forest.invalid",
        "GIT_COMMITTER_NAME": "Iron Forest Race",
        "GIT_COMMITTER_EMAIL": "race@forest.invalid",
    })
    return env


def destination(args: list[str], prefix: str) -> tuple[str, str] | None:
    for arg in args:
        if ":" not in arg:
            continue
        source, target = arg.rsplit(":", 1)
        if target.startswith(prefix):
            return source, target
    return None


def canonical_targets(cwd: Path, ref: str) -> set[str]:
    completed = subprocess.run(
        [GIT, f"--git-dir={ORIGIN}", "ls-tree", "-r", "--name-only", ref],
        text=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, check=False
    )
    if completed.returncode != 0:
        return set()
    return {line.replace("/", "") for line in completed.stdout.splitlines() if line}


def publish_conflict(cwd: Path, args: list[str], canonical: str, schema: str) -> None:
    refspec = destination(args, canonical)
    if refspec is None:
        raise RuntimeError(f"race could not find {canonical} refspec")
    source, _ = refspec
    private_targets = {
        line.split()[-1]
        for line in run(GIT, "notes", f"--ref={source}", "list", cwd=cwd).splitlines()
        if line.strip()
    }
    targets = private_targets - canonical_targets(cwd, canonical)
    if len(targets) != 1:
        raise RuntimeError(f"race expected one new note target, got {sorted(targets)}")
    target = next(iter(targets))
    temporary = "refs/notes/forest/race-conflict"
    remote = subprocess.run(
        [GIT, f"--git-dir={ORIGIN}", "rev-parse", "--verify", canonical],
        text=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, check=False
    )
    if remote.returncode == 0:
        run(GIT, "update-ref", temporary, remote.stdout.strip(), cwd=cwd)
    else:
        subprocess.run([GIT, "update-ref", "-d", temporary], cwd=cwd, check=False)
    if schema == "verdict":
        payload = {
            "schema": "forest.verdict.v1", "revision": target, "verdict": "approve",
            "summary": "concurrent conflicting verdict", "time": TIME,
        }
    else:
        payload = {
            "schema": "forest.review-request.v2", "subject": "100",
            "branch": "forest/100/candidate", "revision": target, "time": TIME,
        }
    with tempfile.NamedTemporaryFile("w", delete=False) as handle:
        json.dump(payload, handle, separators=(",", ":"), sort_keys=True)
        handle.write("\n")
        payload_path = handle.name
    try:
        run(GIT, "notes", f"--ref={temporary}", "add", "-F", payload_path, target, cwd=cwd, env=race_env())
        run(GIT, "push", "origin", f"{temporary}:{canonical}", cwd=cwd)
    finally:
        os.unlink(payload_path)
        subprocess.run([GIT, "update-ref", "-d", temporary], cwd=cwd, check=False)


def main() -> None:
    if not RACE.is_file() or RACE_TRIGGERED.exists():
        return
    cwd = Path(sys.argv[1])
    args = json.loads(sys.argv[2])
    race = json.loads(RACE.read_text())
    kind = race["type"]
    if kind == "canonical_note":
        run(GIT, f"--git-dir={ORIGIN}", "update-ref", "refs/notes/forest/review-request", race["race_note_tip"], cwd=cwd)
    elif kind == "branch":
        refspec = destination(args, "refs/heads/")
        if refspec is None:
            raise RuntimeError("race could not find branch refspec")
        _, target = refspec
        run(GIT, f"--git-dir={ORIGIN}", "update-ref", target, race["competitor"], cwd=cwd)
    elif kind == "approve_master":
        run(GIT, f"--git-dir={ORIGIN}", "update-ref", "refs/heads/master", race["competitor"], cwd=cwd)
    elif kind == "conflicting_verdict":
        publish_conflict(cwd, args, "refs/notes/forest/verdict", "verdict")
    elif kind == "conflicting_review_request":
        publish_conflict(cwd, args, "refs/notes/forest/review-request", "review-request")
    else:
        raise RuntimeError(f"unknown race: {kind}")
    restore_origin_owner()
    RACE_TRIGGERED.write_text(kind + "\n")


if __name__ == "__main__":
    main()
