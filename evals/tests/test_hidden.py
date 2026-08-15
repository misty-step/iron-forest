from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from runtime import hidden


class HiddenStateTest(unittest.TestCase):
    def test_default_hidden_root_is_not_workspace(self):
        self.assertEqual(hidden.HIDDEN, Path("/hidden"))
        self.assertEqual(hidden.SCENARIO, Path("/hidden/scenario.json"))
        self.assertNotEqual(hidden.HIDDEN, Path("/workspace"))
        self.assertNotEqual(hidden.HIDDEN, Path("/eval"))

    def test_hidden_override_stays_outside_workspace(self):
        with tempfile.TemporaryDirectory() as root:
            override = Path(root) / "hidden"
            with patch.dict("os.environ", {"FOREST_EVAL_HIDDEN": str(override)}, clear=False):
                import importlib

                reloaded = importlib.reload(hidden)
                self.assertEqual(reloaded.HIDDEN, override)
                self.assertTrue(str(reloaded.SCENARIO).startswith(str(override)))
            importlib.reload(hidden)


class GeneratedTaskVisibilityTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tasks = Path(__file__).resolve().parents[1] / "tasks"
        cls.manifest = json.loads((Path(__file__).resolve().parents[1] / "cases.json").read_text())

    def test_generated_tasks_do_not_publish_scenario_to_the_candidate(self):
        for case in self.manifest["cases"]:
            task = self.tasks / case["id"]
            self.assertFalse((task / "scenario.json").exists())
            self.assertFalse((task / "environment" / "scenario.json").exists())
            instruction = (task / "instruction.md").read_text()
            self.assertNotIn(case["summary"], instruction)
            self.assertNotIn("Case:", instruction)
            self.assertEqual(json.loads((task / "tests" / "scenario.json").read_text()), case)
            self.assertEqual(json.loads((task / "solution" / "scenario.json").read_text()), case)


if __name__ == "__main__":
    unittest.main()
