#!/usr/bin/env python3
"""Classify changed paths as one Forest role or a shared boundary."""

from __future__ import annotations

import argparse
import subprocess
from pathlib import Path

ROLES = {"builder", "verifier", "fixer", "critic", "tester"}


def classify(paths: list[str]) -> str:
    roles: set[str] = set()
    shared = False
    for raw in paths:
        path = Path(raw)
        parts = path.parts
        if len(parts) >= 2 and parts[0] == "agents" and parts[1] in ROLES:
            roles.add(parts[1])
        else:
            shared = True
    if not shared and len(roles) == 1:
        return next(iter(roles))
    return "shared"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", required=True)
    parser.add_argument("--head", required=True)
    args = parser.parse_args()
    changed = subprocess.run(
        ["git", "diff", "--name-only", f"{args.base}...{args.head}"],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.splitlines()
    print(classify(changed))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
