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

    def test_forbidden_commands_fail_closed(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            hidden = Path(root) / "hidden"
            hidden.mkdir()
            forbidden = (
                "renew", "answer", "done", "abandon", "reopen",
                "set-title", "set-spec", "set-repo", "set-blockers",
                "version", "skill",
            )
            for command in forbidden:
                result = self.run_powder(hidden, command, "if-any")
                self.assertNotEqual(result.returncode, 0, msg=command)
                self.assertIn("forbidden command", result.stderr, msg=command)
            self.assertFalse((hidden / "powder-ops.jsonl").exists())
            self.assertFalse((hidden / "powder-jobs.json").exists())

    def test_allowed_commands_stay_deterministic_after_forbidden_attempt(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            hidden = Path(root) / "hidden"
            hidden.mkdir()
            repo = "misty-step/iron-forest"

            forbidden = self.run_powder(
                hidden, "set-spec", "if-draft", "--spec", "## Problem\nPromoted improperly.\n"
            )
            self.assertNotEqual(forbidden.returncode, 0, msg=forbidden.stderr)
            self.assertFalse((hidden / "powder-jobs.json").exists())

            created = self.run_powder(
                hidden, "create", "--id", "if-draft", "--title", "Draft", "--repo", repo
            )
            self.assertEqual(created.returncode, 0, msg=created.stderr)
            takeable = self.run_powder(hidden, "list", "--takeable", "--repo", repo)
            self.assertEqual(takeable.returncode, 0, msg=takeable.stderr)
            self.assertEqual(json.loads(takeable.stdout), [])
            all_ = self.run_powder(hidden, "list", "--repo", repo)
            self.assertEqual(all_.returncode, 0, msg=all_.stderr)
            self.assertEqual(
                [job["id"] for job in json.loads(all_.stdout)], ["if-draft"]
            )

    def test_lease_commands_are_deterministic(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            hidden = Path(root) / "hidden"
            hidden.mkdir()
            repo = "misty-step/iron-forest"
            agent = "forest-iron-forest"

            ready = self.run_powder(
                hidden, "create", "--id", "if-ready", "--title", "Ready",
                "--repo", repo, "--spec", "## Problem\nConcrete need.\n",
            )
            self.assertEqual(ready.returncode, 0, msg=ready.stderr)
            held = self.run_powder(
                hidden, "create", "--id", "if-held", "--title", "Held",
                "--repo", repo, "--spec", "## Problem\nConcrete need.\n",
            )
            self.assertEqual(held.returncode, 0, msg=held.stderr)

            taken = self.run_powder(hidden, "take", "if-held")
            self.assertEqual(taken.returncode, 0, msg=taken.stderr)
            self.assertEqual(json.loads(taken.stdout)["lease"]["agent"], agent)

            mine = self.run_powder(hidden, "list", "--mine", agent, "--repo", repo)
            self.assertEqual(
                [job["id"] for job in json.loads(mine.stdout)], ["if-held"]
            )
            takeable = self.run_powder(hidden, "list", "--takeable", "--repo", repo)
            self.assertEqual(
                [job["id"] for job in json.loads(takeable.stdout)], ["if-ready"]
            )

            already = self.run_powder(hidden, "take", "if-held")
            self.assertNotEqual(already.returncode, 0, msg=already.stderr)

            released = self.run_powder(hidden, "release", "if-held")
            self.assertEqual(released.returncode, 0, msg=released.stderr)
            self.assertIsNone(json.loads(released.stdout)["lease"])
            mine_after = self.run_powder(hidden, "list", "--mine", agent, "--repo", repo)
            self.assertEqual(json.loads(mine_after.stdout), [])

            asked = self.run_powder(
                hidden, "ask", "if-held", "--question", "is this released?"
            )
            self.assertEqual(asked.returncode, 0, msg=asked.stderr)
            shown = self.run_powder(hidden, "show", "if-held")
            self.assertEqual(shown.returncode, 0, msg=shown.stderr)
            self.assertEqual(json.loads(shown.stdout)["id"], "if-held")

            ops = [
                json.loads(line)
                for line in (hidden / "powder-ops.jsonl").read_text().splitlines()
                if line.strip()
            ]
            self.assertEqual(
                [op["op"] for op in ops if op["id"] == "if-held"],
                ["create", "take", "release", "ask", "show"],
            )


if __name__ == "__main__":
    unittest.main()
