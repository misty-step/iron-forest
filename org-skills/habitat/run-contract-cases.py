#!/usr/bin/env python3
"""Run the redacted Habitat CLI contract cases offline.

Each case replays a fixture through org-skills/habitat/bin/habitat and asserts the
documented exit code, output marker, and profile-owned repository/eligibility
rule from CONTRACT.md. No network and no live ledger.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
BIN = HERE / "bin" / "habitat"
FIXTURES = HERE / "fixtures"

SELF_REPO = "misty-step/iron-forest"


def profile_eligible(fixture: dict) -> bool:
    profile = fixture.get("profile") or {}
    return profile.get("eligibility") == "eligible" and profile.get("repository") == SELF_REPO


CASES = [
    {
        "name": "eligible-item",
        "fixture": "eligible-item.json",
        "exit": 0,
        "stdout_contains": '"IRON-101"',
        "stderr_contains": None,
        "eligible": True,
    },
    {
        "name": "null-eligibility",
        "fixture": "null-eligibility.json",
        "exit": 0,
        "stdout_contains": '"IRON-NULL"',
        "stderr_contains": None,
        "eligible": False,
    },
    {
        "name": "foreign-repository",
        "fixture": "foreign-repository.json",
        "exit": 0,
        "stdout_contains": '"IRON-FOREIGN"',
        "stderr_contains": None,
        "eligible": False,
    },
    {
        "name": "missing-repository",
        "fixture": "missing-repository.json",
        "exit": 0,
        "stdout_contains": '"IRON-NOREPO"',
        "stderr_contains": None,
        "eligible": False,
    },
    {
        "name": "malformed",
        "fixture": "malformed.json",
        "exit": 1,
        "stdout_contains": None,
        "stderr_contains": "Invalid JSON response",
        "eligible": None,
    },
    {
        "name": "auth",
        "fixture": "auth.json",
        "exit": 1,
        "stdout_contains": None,
        "stderr_contains": "HTTP 401",
        "eligible": None,
    },
    {
        "name": "not-found",
        "fixture": "not-found.json",
        "exit": 1,
        "stdout_contains": None,
        "stderr_contains": "not found",
        "eligible": None,
    },
    {
        "name": "list-page-1",
        "fixture": "list-page-1.json",
        "exit": 0,
        "stdout_contains": '"IRON-101"',
        "stderr_contains": None,
        "eligible": None,
    },
    {
        "name": "list-page-2",
        "fixture": "list-page-2.json",
        "exit": 0,
        "stdout_contains": '"IRON-103"',
        "stderr_contains": None,
        "eligible": None,
    },
]


def main() -> int:
    failures = 0
    for case in CASES:
        fixture_path = FIXTURES / case["fixture"]
        fixture = json.loads(fixture_path.read_text(encoding="utf-8"))

        proc = subprocess.run(
            [sys.executable, str(BIN)],
            env={"HABITAT_FIXTURE": str(fixture_path), "PATH": "/usr/bin:/bin"},
            capture_output=True,
            text=True,
            timeout=10,
        )

        problems = []
        if proc.returncode != case["exit"]:
            problems.append(f"exit {proc.returncode} != {case['exit']}")
        if case["stdout_contains"] is not None and case["stdout_contains"] not in proc.stdout:
            problems.append("stdout marker missing")
        if case["stderr_contains"] is not None and case["stderr_contains"] not in proc.stderr:
            problems.append("stderr marker missing")
        if case["eligible"] is not None and profile_eligible(fixture) is not case["eligible"]:
            problems.append(f"profile eligibility wrong ({profile_eligible(fixture)})")

        if problems:
            failures += 1
            print(f"FAIL {case['name']}: {', '.join(problems)}")
        else:
            print(f"PASS {case['name']}")

    if failures:
        print(f"{failures} case(s) failed", file=sys.stderr)
        return 1
    print(f"{len(CASES)} cases passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
