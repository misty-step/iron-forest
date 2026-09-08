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


    def test_case_ids_are_unique(self):
        ids = [case["id"] for case in self.cases]
        self.assertEqual(len(ids), len(set(ids)))



    def test_generated_tasks_keep_case_data_outside_the_agent_environment(self):
        task_names = {path.name for path in (ROOT / "tasks").iterdir() if path.is_dir()}
        self.assertEqual(task_names, {case["id"] for case in self.cases})
        for case in self.cases:
            task = ROOT / "tasks" / case["id"]
            self.assertFalse((task / "scenario.json").exists())
            self.assertFalse((task / "environment" / "scenario.json").exists())


if __name__ == "__main__":
    unittest.main()
