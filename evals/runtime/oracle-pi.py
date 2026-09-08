#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path


def main() -> int:
    data = Path(os.environ["FOREST_EVAL_ORACLE_DATA"])
    result = Path(os.environ["FOREST_EVAL_ORACLE_RESULT"])
    extension = data.parent / "oracle-extension.ts"
    if os.geteuid() == 0 or not os.environ.get("FOREST_RUN_ID"):
        raise RuntimeError("oracle Pi must be launched by the forest Runner as its unprivileged user")
    for path in (data, extension, data.parent / "oracle.py"):
        metadata = path.stat()
        if metadata.st_uid != 0 or metadata.st_mode & 0o222:
            raise RuntimeError(f"oracle input must be root-owned and read-only: {path}")
    if result.exists():
        raise RuntimeError("oracle input hook already ran; refusing to reuse its completion receipt")
    # Preserve every Runner flag, including --no-extensions, model, tools, skills,
    # session identity, and prompts. Pi permits explicitly named extensions even
    # when discovery is disabled. This wrapper is never on a model trial's PATH.
    completed = subprocess.run(
        ["/usr/local/bin/pi", *sys.argv[1:], "-e", str(extension)], check=False
    )
    if completed.returncode != 0:
        return completed.returncode if completed.returncode > 0 else 128 - completed.returncode
    if not result.is_file():
        raise RuntimeError("real Pi exited without running the oracle input hook")
    receipt = json.loads(result.read_text())
    if receipt["handled_inputs"] != 1 or receipt["model_turns"] != 0:
        raise RuntimeError(f"oracle input interception failed: {receipt}")
    return receipt["exit_code"]


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as error:
        print(f"oracle Pi wrapper: {error}", file=sys.stderr)
        raise SystemExit(1)
