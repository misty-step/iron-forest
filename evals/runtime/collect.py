#!/usr/bin/env python3
"""Root-owned post-agent collector for Iron Forest Harbor trials.

Harbor runs this command in the main service *after* the agent phase and
*before* artifact collection. It snapshots every input the separate verifier
needs into one content-addressed, hashed bundle. The verifier consumes only
this bundle and re-checks its manifest, so grader changes can be regraded
against a recorded trial without re-running the agent and a missing or
modified artifact fails closed.
"""

from __future__ import annotations

import hashlib
import json
import os
import platform
import shutil
import sys
from pathlib import Path

BUNDLE = Path("/var/lib/forest-eval/bundle")
MANIFEST = BUNDLE / "manifest.json"

HIDDEN = Path("/hidden")
WORKSPACE = Path("/workspace")
AGENT_LOGS = Path("/logs/agent")
ORIGIN = Path("/origin.git")

HIDDEN_FILES = (
    "scenario.json",
    "state.json",
    "race.json",
    "race-triggered",
    "pr-created.json",
    "issue-created.json",
    "powder-jobs.json",
    "powder-ops.jsonl",
    "candidate-model",
    "reference-run",
    "forest-exit",
    "role",
)

SENSITIVE_ENV_MARKERS = ("KEY", "TOKEN", "SECRET", "PASSWORD")


def is_sensitive_env_name(name: str) -> bool:
    upper = name.upper()
    return any(marker in upper for marker in SENSITIVE_ENV_MARKERS)


def fail(message: str) -> int:
    print(json.dumps({"ok": False, "error": message}, separators=(",", ":")), file=sys.stderr)
    return 1


def copy_path(source: Path, target: Path) -> None:
    if not source.exists():
        return
    target.parent.mkdir(parents=True, exist_ok=True)
    if source.is_dir():
        shutil.copytree(source, target, dirs_exist_ok=True, symlinks=False)
    else:
        shutil.copy2(source, target)


def sanitize_environment() -> dict[str, str]:
    sanitized: dict[str, str] = {}
    for name, value in os.environ.items():
        if value and is_sensitive_env_name(name):
            sanitized[name] = f"<redacted:{name}>"
        else:
            sanitized[name] = value
    return sanitized


def write_environment_metadata() -> None:
    payload = {
        "schema": "forest.eval.environment.v1",
        "platform": platform.platform(),
        "machine": platform.machine(),
        "python": platform.python_version(),
        "cwd": os.getcwd(),
        "environment": sanitize_environment(),
    }
    (BUNDLE / "environment.json").write_text(
        json.dumps(payload, indent=2, sort_keys=True) + "\n"
    )


def hash_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(65536), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_manifest() -> None:
    files: dict[str, dict[str, object]] = {}
    for path in sorted(BUNDLE.rglob("*")):
        if not path.is_file() or path.is_symlink():
            continue
        if path == MANIFEST:
            continue
        relative = path.relative_to(BUNDLE).as_posix()
        files[relative] = {
            "sha256": hash_file(path),
            "size": path.stat().st_size,
        }
    manifest = {
        "schema": "forest.eval.artifact-bundle.v1",
        "files": files,
    }
    MANIFEST.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")


def main() -> int:
    try:
        shutil.rmtree(BUNDLE, ignore_errors=True)
        BUNDLE.mkdir(parents=True, exist_ok=False)
    except OSError as error:
        return fail(f"could not prepare bundle directory: {error}")

    for name in HIDDEN_FILES:
        copy_path(HIDDEN / name, BUNDLE / name)

    copy_path(WORKSPACE / "forest.yaml", BUNDLE / "forest.yaml")
    copy_path(WORKSPACE / ".forest" / "runs", BUNDLE / "workspace" / ".forest" / "runs")
    copy_path(AGENT_LOGS, BUNDLE / "agent")
    copy_path(ORIGIN, BUNDLE / "origin.git")

    try:
        write_environment_metadata()
        write_manifest()
    except Exception as error:  # noqa: BLE001 - collector must report and exit
        return fail(f"bundle finalization failed: {error}")

    print(
        json.dumps(
            {"ok": True, "bundle": str(BUNDLE), "manifest": str(MANIFEST)},
            separators=(",", ":"),
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
