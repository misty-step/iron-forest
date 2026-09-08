#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import subprocess
import sys
import re
from pathlib import Path
from hidden import RACE, RACE_TRIGGERED


GIT = "/usr/bin/git"
ORIGIN = "/origin.git"


def run(*args: str, cwd: Path, env: dict[str, str] | None = None, input: str | None = None) -> str:
    if args[0] == GIT:
        args = (GIT, "-c", f"safe.directory={cwd}", "-c", f"safe.directory={ORIGIN}", *args[1:])
    completed = subprocess.run(
        list(args), cwd=cwd, env=env, input=input, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False
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
        if arg.startswith("-") or ":" not in arg:
            continue
        source, target = arg.rsplit(":", 1)
        if target.startswith(prefix):
            return source.removeprefix("+"), target
    return None


def publish_conflict(cwd: Path, args: list[str], kind: str) -> None:
    prefix = f"refs/forest/v1/{kind}/"
    refspec = destination(args, prefix)
    if refspec is None:
        raise RuntimeError(f"race could not find {prefix} refspec")
    source, target_ref = refspec
    revision = target_ref.removeprefix(prefix)
    if re.fullmatch(r"[0-9a-f]{40}", revision) is None:
        raise RuntimeError("race requires a full revision-scoped evidence ref")
    payload = json.loads(run(GIT, "show", f"{source}:{kind}.json", cwd=cwd))
    if not isinstance(payload, dict) or payload.get("revision") != revision:
        raise RuntimeError("race evidence does not bind the publication revision")
    if kind == "verdict":
        payload["verdict"] = "approve" if payload.get("verdict") == "changes" else "changes"
        payload["summary"] = "Concurrent writer publishes a different decision."
    elif kind == "request":
        # Keep the exact request binding but make its bytes differ, so neither
        # a lease retry nor the CLI's identical-publication path can accept it.
        time = "2026-08-14T00:00:00Z"
        payload["time"] = "2026-08-14T00:00:01Z" if payload.get("time") == time else time
    else:
        raise RuntimeError(f"unsupported conflicting evidence kind: {kind}")
    origin_git = (GIT, f"--git-dir={ORIGIN}")
    blob = run(
        *origin_git, "hash-object", "-w", "--stdin", cwd=cwd,
        input=json.dumps(payload, separators=(",", ":"), sort_keys=True) + "\n",
    )
    tree = run(*origin_git, "mktree", cwd=cwd, input=f"100644 blob {blob}\t{kind}.json\n")
    commit = run(
        *origin_git, "commit-tree", tree, "-m", f"eval concurrent {kind} {revision}",
        cwd=cwd, env=race_env(),
    )
    run(
        *origin_git, "update-ref", target_ref, commit, "0" * 40, cwd=cwd,
    )


def main() -> None:
    if not RACE.is_file() or RACE_TRIGGERED.exists():
        return
    cwd = Path(sys.argv[1])
    args = json.loads(sys.argv[2])
    race = json.loads(RACE.read_text())
    kind = race["type"]
    if kind == "unrelated_evidence":
        refspec = destination(args, "refs/forest/v1/request/")
        target = f"refs/forest/v1/request/{race['base']}"
        if refspec is None or refspec[1] == target:
            raise RuntimeError("unrelated evidence must not target the candidate request")
        run(
            GIT, f"--git-dir={ORIGIN}", "update-ref", target,
            race["race_request_commit"], "0" * 40, cwd=cwd,
        )
    elif kind == "branch":
        refspec = destination(args, "refs/heads/")
        if refspec is None:
            raise RuntimeError("race could not find branch refspec")
        _, target = refspec
        run(GIT, f"--git-dir={ORIGIN}", "update-ref", target, race["competitor"], cwd=cwd)
    elif kind == "approve_master":
        run(GIT, f"--git-dir={ORIGIN}", "update-ref", "refs/heads/master", race["competitor"], cwd=cwd)
    elif kind == "conflicting_verdict":
        publish_conflict(cwd, args, "verdict")
    elif kind == "conflicting_review_request":
        publish_conflict(cwd, args, "request")
    else:
        raise RuntimeError(f"unknown race: {kind}")
    restore_origin_owner()
    RACE_TRIGGERED.write_text(kind + "\n")


if __name__ == "__main__":
    main()
