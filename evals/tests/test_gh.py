from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
GH = ROOT / "runtime" / "gh"


class GhFixtureTest(unittest.TestCase):
    def run_gh(self, hidden: Path, *args: str) -> subprocess.CompletedProcess[str]:
        (hidden / "scenario.json").write_text('{"id":"builder-no-eligible-issue"}\n')
        env = os.environ.copy()
        env["FOREST_EVAL_HIDDEN"] = str(hidden)
        env["PYTHONPATH"] = str(ROOT / "runtime")
        return subprocess.run(
            [sys.executable, str(GH), *args],
            cwd=ROOT,
            env=env,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )

    def test_help_for_pr_create_does_not_record_a_pr(self):
        with tempfile.TemporaryDirectory() as root:
            hidden = Path(root)
            result = self.run_gh(hidden, "pr", "create", "--help")
            self.assertEqual(result.returncode, 2)
            self.assertFalse((hidden / "pr-created.json").exists())

    def test_pr_create_records_a_pr(self):
        with tempfile.TemporaryDirectory() as root:
            hidden = Path(root)
            result = self.run_gh(hidden, "pr", "create", "--head", "forest/1-ready")
            self.assertEqual(result.returncode, 0)
            payload = json.loads((hidden / "pr-created.json").read_text())
            self.assertEqual(payload["head"], "forest/1-ready")


if __name__ == "__main__":
    unittest.main()
