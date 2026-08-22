from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
POWDER = ROOT / "runtime" / "powder"


class PowderIntakeTest(unittest.TestCase):
    def run_powder(self, hidden: Path, *args: str) -> subprocess.CompletedProcess[str]:
        env = os.environ.copy()
        env["FOREST_EVAL_HIDDEN"] = str(hidden)
        return subprocess.run(
            [sys.executable, str(POWDER), *args],
            cwd=ROOT,
            env=env,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )

    def test_spec_less_external_filing_is_not_takeable(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            hidden = Path(root) / "hidden"
            hidden.mkdir()
            repo = "misty-step/iron-forest"

            external = self.run_powder(
                hidden, "create", "--id", "if-critic-external", "--title",
                "External finding", "--repo", repo,
            )
            self.assertEqual(external.returncode, 0, msg=external.stderr)
            promoted = self.run_powder(
                hidden, "create", "--id", "if-ready-work", "--title",
                "Ready work", "--repo", repo, "--spec", "## Problem\nConcrete need.\n",
            )
            self.assertEqual(promoted.returncode, 0, msg=promoted.stderr)

            takeable = self.run_powder(hidden, "list", "--takeable", "--repo", repo)
            self.assertEqual(takeable.returncode, 0, msg=takeable.stderr)
            takeable_ids = [job["id"] for job in json.loads(takeable.stdout)]
            self.assertIn("if-ready-work", takeable_ids)
            self.assertNotIn("if-critic-external", takeable_ids)

            all_ = self.run_powder(hidden, "list", "--repo", repo)
            self.assertEqual(all_.returncode, 0, msg=all_.stderr)
            all_ids = [job["id"] for job in json.loads(all_.stdout)]
            self.assertIn("if-critic-external", all_ids)


if __name__ == "__main__":
    unittest.main()