from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

import production_flywheel as flywheel  # noqa: E402


class FakeItem:
    def __init__(self, id: str, metadata: dict, source_trace_id: str | None = None):
        self.id = id
        self.metadata = metadata
        self.source_trace_id = source_trace_id


class FakeLangfuseClient(flywheel.LangfuseClient):
    def __init__(self, located_trace_ids: dict[str, list[str]] | None = None):
        self.items: dict[str, FakeItem] = {}
        self.located_trace_ids = located_trace_ids or {}
        self.ensure_calls: list[str] = []
        self.calls: list[str] = []

    def ensure_dataset(self, name: str) -> None:
        self.ensure_calls.append(name)
        self.calls.append("ensure_dataset")

    def get_dataset_item(self, dataset_name: str, id: str):
        return self.items.get(id)

    def create_dataset_item(self, *, dataset_name, id, input, expected_output, metadata, source_trace_id=None) -> None:
        self.calls.append("create_dataset_item")
        self.items[id] = FakeItem(id, metadata, source_trace_id)

    def list_dataset_items(self, dataset_name: str) -> list[FakeItem]:
        return list(self.items.values())

    def trace_ids_for_session(self, session_id: str) -> list[str]:
        return self.located_trace_ids.get(session_id, [])


def write_run_log(runs_dir: Path, run_id: str, role: str) -> None:
    runs_dir.mkdir(parents=True, exist_ok=True)
    (runs_dir / f"{run_id}.log").write_text(
        json.dumps({"type": "forest.run", "run_id": run_id, "agent": role, "model": "test/model"}) + "\n"
        + '{"type":"agent_end","message":"done"}\n'
    )


class ProductionFlywheelTest(unittest.TestCase):
    def test_ingest_creates_draft_items_and_is_idempotent(self):
        with tempfile.TemporaryDirectory() as root:
            runs_dir = Path(root) / "runs"
            write_run_log(runs_dir, "1787529620484390170-builder", "builder")
            write_run_log(runs_dir, "1787529620484390171-verifier", "verifier")
            client = FakeLangfuseClient(located_trace_ids={"1787529620484390170-builder": ["trace-openrouter-1"]})

            first = flywheel.ingest_runs(runs_dir, client)
            self.assertEqual(first["created"], 2)
            self.assertEqual(first["skipped"], 0)
            self.assertEqual(set(client.items), {"prod-1787529620484390170-builder", "prod-1787529620484390171-verifier"})

            builder_item = client.items["prod-1787529620484390170-builder"]
            self.assertEqual(builder_item.metadata["role"], "builder")
            self.assertEqual(builder_item.metadata["status"], "draft")
            self.assertEqual(builder_item.source_trace_id, "trace-openrouter-1")

            verifier_item = client.items["prod-1787529620484390171-verifier"]
            self.assertEqual(verifier_item.source_trace_id, "1787529620484390171-verifier")

            second = flywheel.ingest_runs(runs_dir, client)
            self.assertEqual(second["created"], 0)
            self.assertEqual(second["skipped"], 2)

    def test_ingest_skips_files_without_forest_run_identity(self):
        with tempfile.TemporaryDirectory() as root:
            runs_dir = Path(root) / "runs"
            runs_dir.mkdir(parents=True)
            (runs_dir / "plain.log").write_text("not a forest run\n")
            client = FakeLangfuseClient()
            result = flywheel.ingest_runs(runs_dir, client)
            self.assertEqual(result["created"], 0)
            self.assertEqual(client.items, {})

    def test_ingest_ensures_dataset_before_creating_items(self):
        with tempfile.TemporaryDirectory() as root:
            runs_dir = Path(root) / "runs"
            write_run_log(runs_dir, "1787529620484390170-builder", "builder")
            client = FakeLangfuseClient()

            result = flywheel.ingest_runs(runs_dir, client)

            self.assertEqual(result["created"], 1)
            self.assertEqual(client.ensure_calls, [flywheel.PRODUCTION_DATASET])
            self.assertEqual(client.calls[0], "ensure_dataset")
            self.assertIn("create_dataset_item", client.calls)
            self.assertLess(
                client.calls.index("ensure_dataset"),
                client.calls.index("create_dataset_item"),
            )

    def test_promote_requires_provenance_and_scenario(self):
        with tempfile.TemporaryDirectory() as root:
            manifest = Path(root) / "production-cases.json"
            manifest.write_text(json.dumps({"schema": "forest.production-cases.v1", "cases": []}) + "\n")
            base = {
                "schema": "forest.production-case.v1",
                "id": "prod-builder-branch-race",
                "role": "builder",
                "summary": "Replay a branch race.",
                "effect": "builder_publish",
                "source_trace_id": "trace-openrouter-1",
                "source_run_id": "1787529620484390170-builder",
                "expected_files": {"value.txt": "ready\n"},
            }

            flywheel.promote_contract(dict(base), manifest)
            self.assertEqual(flywheel.load_production_manifest(manifest)["cases"][0]["id"], base["id"])

            with self.assertRaises(ValueError):
                flywheel.promote_contract(dict(base), manifest)

            missing_trace = dict(base)
            missing_trace["id"] = "prod-builder-other"
            missing_trace["source_trace_id"] = ""
            with self.assertRaises(ValueError):
                flywheel.promote_contract(missing_trace, manifest)

            missing_scenario = dict(base)
            missing_scenario["id"] = "prod-builder-no-scenario"
            missing_scenario.pop("expected_files")
            with self.assertRaises(ValueError):
                flywheel.promote_contract(missing_scenario, manifest)

    def test_promote_sorts_and_writes_versioned_manifest(self):
        with tempfile.TemporaryDirectory() as root:
            manifest = Path(root) / "production-cases.json"
            first = {
                "schema": "forest.production-case.v1",
                "id": "prod-verifier-defect",
                "role": "verifier",
                "summary": "Approve a planted defect.",
                "effect": "verifier_changes",
                "source_trace_id": "trace-v",
                "source_run_id": "run-v",
                "issue": {"number": 1, "title": "x", "body": "y"},
            }
            second = {
                "schema": "forest.production-case.v1",
                "id": "prod-builder-branch-race",
                "role": "builder",
                "summary": "Replay a branch race.",
                "effect": "builder_publish",
                "source_trace_id": "trace-b",
                "source_run_id": "run-b",
                "expected_files": {"value.txt": "ready\n"},
            }
            flywheel.promote_contract(first, manifest)
            flywheel.promote_contract(second, manifest)
            cases = flywheel.load_production_manifest(manifest)["cases"]
            self.assertEqual([case["id"] for case in cases], ["prod-builder-branch-race", "prod-verifier-defect"])
            self.assertTrue(all(case["suite"] == "production-replay" for case in cases))
            self.assertTrue(all("promoted_at" in case for case in cases))

    def test_report_names_new_cases_and_coverage(self):
        with tempfile.TemporaryDirectory() as root:
            manifest = Path(root) / "production-cases.json"
            manifest.write_text(json.dumps({"schema": "forest.production-cases.v1", "cases": []}) + "\n")
            client = FakeLangfuseClient()
            client.items["prod-a"] = FakeItem("prod-a", {"role": "builder", "outcome": "failed", "status": "draft", "source_trace_id": "trace-a"})
            client.items["prod-b"] = FakeItem("prod-b", {"role": "verifier", "outcome": "cancelled", "status": "broken", "source_trace_id": "trace-b"})

            report = flywheel.report_markdown(client, manifest)
            self.assertIn("- New draft cases awaiting verification: 2", report)
            self.assertIn("builder=1", report)
            self.assertIn("verifier=1", report)
            self.assertIn("failed=1", report)
            self.assertIn("cancelled=1", report)
            self.assertIn("- Production-distribution coverage: 2/2", report)
            self.assertIn("- Ambiguous or broken drafts: 1", report)
            self.assertIn("prod-a", report)
            self.assertIn("prod-b", report)

    def test_report_counts_grader_exploit_regressions(self):
        with tempfile.TemporaryDirectory() as root:
            manifest = Path(root) / "production-cases.json"
            manifest.write_text(json.dumps({
                "schema": "forest.production-cases.v1",
                "cases": [
                    {
                        "id": "prod-builder-exploit",
                        "role": "builder",
                        "summary": "x",
                        "effect": "builder_publish",
                        "source_trace_id": "trace-e",
                        "source_run_id": "run-e",
                        "source": "grader-exploit",
                        "expected_files": {"value.txt": "ready\n"},
                    }
                ],
            }) + "\n")
            client = FakeLangfuseClient()
            report = flywheel.report_markdown(client, manifest)
            self.assertIn("- Grader-exploit regressions promoted: 1", report)

    def test_report_links_promoted_cases_by_source_trace(self):
        with tempfile.TemporaryDirectory() as root:
            manifest = Path(root) / "production-cases.json"
            manifest.write_text(json.dumps({
                "schema": "forest.production-cases.v1",
                "cases": [
                    {
                        "id": "prod-builder-branch-race",
                        "role": "builder",
                        "summary": "x",
                        "effect": "builder_publish",
                        "source_trace_id": "trace-a",
                        "source_run_id": "run-a",
                        "expected_files": {"value.txt": "ready\n"},
                    }
                ],
            }) + "\n")
            client = FakeLangfuseClient()
            client.items["prod-a"] = FakeItem("prod-a", {"role": "builder", "status": "draft", "source_trace_id": "trace-a"})
            report = flywheel.report_markdown(client, manifest)
            self.assertIn("- New draft cases awaiting verification: 0", report)
            self.assertIn("- Saturation: 1/1", report)


if __name__ == "__main__":
    unittest.main()
