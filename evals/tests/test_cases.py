from __future__ import annotations

import json
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


class CaseManifestTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.manifest = json.loads((ROOT / "cases.json").read_text())
        cls.cases = cls.manifest["cases"]

    def test_manifest_has_six_cases_per_role(self):
        counts = {role: sum(case["role"] == role for case in self.cases) for role in ("builder", "verifier", "fixer")}
        self.assertEqual(counts, {"builder": 6, "verifier": 6, "fixer": 6})

    def test_case_ids_are_unique(self):
        ids = [case["id"] for case in self.cases]
        self.assertEqual(len(ids), len(set(ids)))

    def test_generated_tasks_match_manifest(self):
        task_names = {path.name for path in (ROOT / "tasks").iterdir() if path.is_dir()}
        self.assertEqual(task_names, {case["id"] for case in self.cases})
        for case in self.cases:
            task = ROOT / "tasks" / case["id"]
            self.assertEqual(json.loads((task / "scenario.json").read_text()), case)
            self.assertEqual(json.loads((task / "tests" / "scenario.json").read_text()), case)
            self.assertEqual(json.loads((task / "solution" / "scenario.json").read_text()), case)
            self.assertEqual(json.loads((task / "environment" / "scenario.json").read_text()), case)
            self.assertTrue((task / "tests" / "test.sh").stat().st_mode & 0o111)
            self.assertTrue((task / "solution" / "solve.sh").stat().st_mode & 0o111)


if __name__ == "__main__":
    unittest.main()
