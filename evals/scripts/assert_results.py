#!/usr/bin/env python3
from __future__ import annotations

import json
import sys
from pathlib import Path


def main() -> None:
    jobs = Path(sys.argv[1])
    results = []
    for path in jobs.rglob("result.json"):
        value = json.loads(path.read_text())
        if "task_name" in value:
            results.append((path, value))
    if not results:
        raise SystemExit("no Harbor trial results found")
    failures: list[str] = []
    for path, result in results:
        if result.get("exception_info") is not None:
            failures.append(f"{path.parent.name}: exception")
            continue
        verifier = result.get("verifier_result") or {}
        rewards = verifier.get("rewards") or {}
        if not rewards or any(value != 1 for value in rewards.values()):
            failures.append(f"{path.parent.name}: rewards={rewards}")
    if failures:
        raise SystemExit("Harbor regressions failed:\n" + "\n".join(failures))
    print(f"validated {len(results)} Harbor trials; every reward is 1")


if __name__ == "__main__":
    main()
