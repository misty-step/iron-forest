from __future__ import annotations

import json
import sys
import tempfile
import tomllib
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

import sync_tasks  # noqa: E402


class ProductionTaskGenerationTest(unittest.TestCase):
    def test_generate_tasks_uses_production_replay_keyword(self):
        with tempfile.TemporaryDirectory() as root:
            tasks_dir = Path(root) / "tasks-production"
            manifest = {
                "schema": "forest.production-cases.v1",
                "cases": [
                    {
                        "id": "prod-builder-branch-race",
                        "role": "builder",
                        "summary": "Replay a branch race.",
                        "effect": "builder_publish",
                        "source_trace_id": "trace-b",
                        "source_run_id": "run-b",
                        "expected_files": {"value.txt": "ready\n"},
                    }
                ],
            }
            sync_tasks.generate_tasks(manifest, tasks_dir, "production-replay")
            task = tasks_dir / "prod-builder-branch-race"
            config = tomllib.loads((task / "task.toml").read_text())
            self.assertEqual(config["task"]["name"], "iron-forest/prod-builder-branch-race")
            self.assertIn("production-replay", config["task"]["keywords"])
            self.assertEqual(json.loads((task / "tests" / "scenario.json").read_text()), manifest["cases"][0])
            self.assertTrue((task / "tests" / "test.sh").stat().st_mode & 0o111)

    def test_load_manifest_rejects_unknown_schema(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / "manifest.json"
            path.write_text(json.dumps({"schema": "forest.evals.v1", "cases": []}))
            with self.assertRaises(RuntimeError):
                sync_tasks.load_manifest(path, "forest.production-cases.v1")


if __name__ == "__main__":
    unittest.main()
