from __future__ import annotations

import json
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]

SCHEMA = "forest.capability-inventory.v1"
SUITES = {"capability", "whole-forest", "adversarial-security", "eval-integrity", "production-replay"}
SUBJECTS = {"builder", "verifier", "fixer", "critic", "tester", "forest", "eval"}
CLASSES = {"positive", "negative", "boundary", "adversarial", "recovery"}
SCENARIO_KEYS = {"setup", "action", "reference_outcome"}


class CapabilityInventoryTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.inventory = json.loads((ROOT / "capability-inventory.json").read_text())
        cls.tasks = cls.inventory["tasks"]

    def test_schema(self):
        self.assertEqual(self.inventory["schema"], SCHEMA)

    def test_task_count_is_first_expansion_bounds(self):
        self.assertGreaterEqual(len(self.tasks), 20)
        self.assertLessEqual(len(self.tasks), 50)

    def test_task_ids_are_unique(self):
        ids = [task["id"] for task in self.tasks]
        self.assertEqual(len(ids), len(set(ids)))

    def test_required_fields_are_nonempty(self):
        for task in self.tasks:
            with self.subTest(task=task["id"]):
                for field in ("id", "suite", "subject", "behavior", "class", "sources", "incident", "positive", "counterexample"):
                    self.assertIn(field, task, f"{field} missing")
                    if field != "incident":
                        self.assertTrue(task[field], f"{field} empty")

    def test_enums_are_known(self):
        for task in self.tasks:
            with self.subTest(task=task["id"]):
                self.assertIn(task["suite"], SUITES)
                self.assertIn(task["subject"], SUBJECTS)
                self.assertIn(task["class"], CLASSES)

    def test_paired_scenarios_have_reference_outcomes(self):
        for task in self.tasks:
            with self.subTest(task=task["id"]):
                for scenario_name in ("positive", "counterexample"):
                    scenario = task[scenario_name]
                    self.assertEqual(set(scenario), SCENARIO_KEYS)
                    for key in SCENARIO_KEYS:
                        self.assertTrue(scenario[key].strip(), f"{scenario_name}.{key} empty")

    def test_all_subjects_classes_and_suites_covered(self):
        self.assertEqual({task["subject"] for task in self.tasks}, SUBJECTS)
        self.assertEqual({task["class"] for task in self.tasks}, CLASSES)
        self.assertEqual({task["suite"] for task in self.tasks}, SUITES)

    def test_sources_resolve_to_repository_files(self):
        for task in self.tasks:
            with self.subTest(task=task["id"]):
                self.assertTrue(task["sources"], "sources must not be empty")
                for source in task["sources"]:
                    self.assertIn("path", source)
                    self.assertIn("note", source)
                    self.assertTrue(source["note"].strip())
                    path = Path(source["path"])
                    self.assertFalse(path.is_absolute(), f"source path must be repository-relative: {path}")
                    self.assertTrue((ROOT.parent / path).exists(), f"source path does not exist: {path}")


if __name__ == "__main__":
    unittest.main()
