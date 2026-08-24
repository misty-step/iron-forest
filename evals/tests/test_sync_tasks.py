from __future__ import annotations

import json
import sys
import tomllib
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))


class GeneratedTaskContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tasks = ROOT / "tasks"
        cls.manifest = json.loads((ROOT / "cases.json").read_text())

    def task_paths(self):
        for case in self.manifest["cases"]:
            yield case, self.tasks / case["id"]

    def test_every_generated_task_uses_a_separate_verifier(self):
        for case, task in self.task_paths():
            with self.subTest(case=case["id"]):
                config = tomllib.loads((task / "task.toml").read_text())
                self.assertEqual(config["verifier"]["environment_mode"], "separate")
                self.assertEqual(config["verifier"]["environment"]["workdir"], "/tests")

    def test_every_generated_task_declares_the_artifact_bundle(self):
        for case, task in self.task_paths():
            with self.subTest(case=case["id"]):
                config = tomllib.loads((task / "task.toml").read_text())
                sources = [entry.get("source") for entry in config.get("artifacts", [])]
                self.assertIn("/var/lib/forest-eval/bundle", sources)

    def test_every_generated_task_runs_the_root_collector_after_the_agent(self):
        for case, task in self.task_paths():
            with self.subTest(case=case["id"]):
                config = tomllib.loads((task / "task.toml").read_text())
                hooks = config["verifier"].get("collect", [])
                self.assertTrue(hooks)
                self.assertIn(
                    "python3 /opt/iron-forest-eval/collect.py",
                    [hook.get("command") for hook in hooks],
                )
                self.assertTrue(all(hook.get("user") == "root" for hook in hooks))

    def test_generated_test_points_the_verifier_at_the_bundle(self):
        for case, task in self.task_paths():
            with self.subTest(case=case["id"]):
                test_script = (task / "tests" / "test.sh").read_text()
                self.assertIn(
                    "export FOREST_EVAL_HIDDEN=/var/lib/forest-eval/bundle",
                    test_script,
                )
                self.assertIn(
                    "python3 /opt/iron-forest-eval/grade.py /tests/scenario.json",
                    test_script,
                )


if __name__ == "__main__":
    unittest.main()
