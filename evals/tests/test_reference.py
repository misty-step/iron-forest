from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
REFERENCE = ROOT / "runtime" / "reference.py"


class ReferenceCriticDraftsTest(unittest.TestCase):
    def run_reference(self, hidden: Path, scenario: dict) -> None:
        (hidden / "scenario.json").write_text(json.dumps(scenario))
        state = {
            "base": "cafebabe",
            "master_before": "cafebabe",
            "candidate": None,
            "competitor": None,
            "branch": None,
        }
        (hidden / "state.json").write_text(json.dumps(state))
        env = os.environ.copy()
        env["FOREST_EVAL_HIDDEN"] = str(hidden)
        result = subprocess.run(
            [sys.executable, str(REFERENCE), str(hidden / "scenario.json")],
            cwd=ROOT,
            env=env,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        self.assertEqual(result.returncode, 0, msg=result.stderr)

    def test_critic_reference_creates_evidence_note_for_planted_hotspot(self):
        with tempfile.TemporaryDirectory() as root:
            hidden = Path(root) / "hidden"
            hidden.mkdir()
            scenario = {
                "id": "critic-sweep-draft-only",
                "role": "critic",
                "summary": "Sweep the repository and file drafts.",
                "check": "true",
                "effect": "critic_drafts",
                "planted_files": {"hotspot.go": "package main\n"},
            }
            self.run_reference(hidden, scenario)

            ops = [
                json.loads(line)
                for line in (hidden / "powder-ops.jsonl").read_text().splitlines()
                if line.strip()
            ]
            creates = [op for op in ops if op.get("op") == "create"]
            notes = [op for op in ops if op.get("op") == "note"]
            self.assertTrue(1 <= len(creates) <= 5)
            self.assertTrue(all(not op.get("spec") for op in creates))
            self.assertTrue(notes)
            self.assertTrue(
                all(re.search(r"\S+:\d+", str(note.get("text", ""))) for note in notes)
            )
            self.assertTrue(any("hotspot.go" in str(note.get("text", "")) for note in notes))

    def test_tester_reference_creates_test_work_note_for_planted_surface(self):
        with tempfile.TemporaryDirectory() as root:
            hidden = Path(root) / "hidden"
            hidden.mkdir()
            scenario = {
                "id": "tester-sweep-draft-only",
                "role": "tester",
                "summary": "Sweep the repository and file test-work drafts.",
                "check": "true",
                "effect": "tester_drafts",
                "planted_files": {"under-tested.go": "package main\n"},
            }
            self.run_reference(hidden, scenario)

            ops = [
                json.loads(line)
                for line in (hidden / "powder-ops.jsonl").read_text().splitlines()
                if line.strip()
            ]
            creates = [op for op in ops if op.get("op") == "create"]
            notes = [op for op in ops if op.get("op") == "note"]
            self.assertTrue(1 <= len(creates) <= 5)
            self.assertTrue(all(not op.get("spec") for op in creates))
            self.assertTrue(notes)
            note_texts = [str(note.get("text", "")) for note in notes]
            self.assertTrue(all(re.search(r"\S+:\d+", text) for text in note_texts))
            self.assertTrue(any("under-tested.go" in text for text in note_texts))
            self.assertTrue(any(re.search(r"(?i)surface", text) for text in note_texts))
            self.assertTrue(any(re.search(r"(?i)failing example", text) for text in note_texts))
            self.assertTrue(any(re.search(r"(?i)acceptance", text) for text in note_texts))


if __name__ == "__main__":
    unittest.main()
