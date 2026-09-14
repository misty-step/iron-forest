from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

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
        self.datasets: set[str] = set()

    def ensure_dataset(self, name: str) -> None:
        self.datasets.add(name)

    def get_dataset_item(self, dataset_name: str, id: str):
        return self.items.get(id)

    def create_dataset_item(self, *, dataset_name, id, input, expected_output, metadata, source_trace_id=None) -> None:
        if dataset_name not in self.datasets:
            raise RuntimeError("dataset does not exist")
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


    def test_promote_validates_role_effects(self):
        with tempfile.TemporaryDirectory() as root:
            manifest = Path(root) / "production-cases.json"
            manifest.write_text(json.dumps({"schema": "forest.production-cases.v1", "cases": []}) + "\n")
            contract = {
                "schema": "forest.production-case.v1",
                "id": "prod-effect",
                "role": "builder",
                "summary": "Replay case.",
                "effect": "builder_publish",
                "source_trace_id": "trace-1",
                "source_run_id": "run-1",
                "expected_files": {"value.txt": "ready\n"},
            }
            before = manifest.read_bytes()
            for role, effect in (
                ("builder", "invalid_effect"),
                ("builder", "verifier_changes"),
                ("verifier", "verifier_approve_conflict"),
                ("fixer", "builder_publish"),
                ([], "builder_publish"),
            ):
                with self.subTest(role=role, effect=effect):
                    with self.assertRaises(ValueError):
                        flywheel.promote_contract(dict(contract, role=role, effect=effect), manifest)
                    self.assertEqual(manifest.read_bytes(), before)

            for role, effect in (
                ("builder", "builder_branch_race"),
                ("verifier", "verifier_approve_race"),
                ("fixer", "fixer_conflict"),
            ):
                flywheel.promote_contract(dict(contract, id=f"prod-{role}", role=role, effect=effect), manifest)
            self.assertEqual(
                {(case["role"], case["effect"]) for case in flywheel.load_production_manifest(manifest)["cases"]},
                {("builder", "builder_branch_race"), ("verifier", "verifier_approve_race"), ("fixer", "fixer_conflict")},
            )

    def test_promote_validates_scenario_field_shapes(self):
        with tempfile.TemporaryDirectory() as root:
            manifest = Path(root) / "production-cases.json"
            manifest.write_text(json.dumps({"schema": "forest.production-cases.v1", "cases": []}) + "\n")
            base = {
                "schema": "forest.production-case.v1",
                "id": "prod-builder-test",
                "role": "builder",
                "summary": "Replay case.",
                "effect": "builder_publish",
                "source_trace_id": "trace-1",
                "source_run_id": "run-1",
                "check": "true",
            }
            before = manifest.read_bytes()
            for fields in (
                {"expected_files": "not-a-dict"},
                {"expected_files": {"value.txt": 123}},
                {"expected_files": None},
                {"planted_files": []},
                {"planted_files": None},
                {"issue": "not-an-issue-dict"},
                {"issue": {"number": True, "title": "t", "body": "b"}},
                {"issue": {"number": 1, "title": "", "body": "b"}},
                {"issue": {"number": 1, "title": "t"}},
                {"issue": {"number": 1, "title": "t", "body": 123}},
                {"check": 123},
                {"check": None},
                {"check": "   "},
            ):
                with self.subTest(fields=fields):
                    with self.assertRaises(ValueError):
                        flywheel.promote_contract(dict(base, **fields), manifest)
                    self.assertEqual(manifest.read_bytes(), before)

            flywheel.promote_contract(dict(base, issue={"number": 1, "title": "t", "body": None}), manifest)
            flywheel.promote_contract(dict(base, id="prod-no-issue", issue=None), manifest)
            cases = flywheel.load_production_manifest(manifest)["cases"]
            self.assertEqual(cases[0]["issue"], {"number": 1, "title": "t", "body": None})
            self.assertIsNone(cases[1]["issue"])

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


    def test_report_links_promoted_cases_by_source_run_id_when_trace_corrected(self):
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
                        "source_trace_id": "trace-corrected-real",
                        "source_run_id": "run-1",
                        "expected_files": {"value.txt": "ready\n"},
                    }
                ],
            }) + "\n")
            client = FakeLangfuseClient()
            # Ingest used run-1 fallback as source_trace_id, but recorded run_id="run-1" in metadata
            client.items["prod-run-1"] = FakeItem(
                "prod-run-1",
                {"role": "builder", "status": "draft", "run_id": "run-1", "source_trace_id": "run-1"},
            )
            report = flywheel.report_markdown(client, manifest)
            self.assertIn("- New draft cases awaiting verification: 0", report)
            self.assertIn("- Saturation: 1/1", report)

            # Adding an unpromoted item counts as a new draft awaiting verification
            client.items["prod-run-2"] = FakeItem(
                "prod-run-2",
                {"role": "builder", "status": "draft", "run_id": "run-2", "source_trace_id": "trace-2"},
            )
            report2 = flywheel.report_markdown(client, manifest)
            self.assertIn("- New draft cases awaiting verification: 1", report2)

    def test_sdk_report_listing_exposes_transport_failure(self):
        class BrokenItems:
            def list(self, **kwargs):
                raise RuntimeError("transport down")

        class API:
            dataset_items = BrokenItems()

        class Client:
            api = API()

        client = flywheel.LangfuseSDKClient(Client())
        with self.assertRaises(RuntimeError):
            client.list_dataset_items(flywheel.PRODUCTION_DATASET)

    def test_sdk_list_dataset_items_pages_through_all_pages(self):
        expected = [f"item-{i}" for i in range(120)]

        def list_items(dataset_name, page, limit):
            return SimpleNamespace(
                data=expected[(page - 1) * limit:page * limit],
                meta=SimpleNamespace(total_pages=2),
            )

        client = flywheel.LangfuseSDKClient(SimpleNamespace(
            api=SimpleNamespace(dataset_items=SimpleNamespace(list=list_items)),
        ))
        self.assertEqual(client.list_dataset_items(flywheel.PRODUCTION_DATASET), expected)

    def test_sdk_listing_honors_metadata_on_short_pages(self):
        for meta in (
            {"total_pages": 2},
            {"totalPages": 2},
            SimpleNamespace(total_pages=2),
            SimpleNamespace(totalPages=2),
        ):
            with self.subTest(meta=meta):
                def list_items(dataset_name, page, limit):
                    # The server's page size need not equal the requested limit.
                    data = {1: ["first"], 2: ["last"]}[page]
                    return SimpleNamespace(data=data, meta=meta)

                client = flywheel.LangfuseSDKClient(SimpleNamespace(
                    api=SimpleNamespace(dataset_items=SimpleNamespace(list=list_items)),
                ))
                self.assertEqual(client.list_dataset_items(flywheel.PRODUCTION_DATASET), ["first", "last"])

    def test_sdk_listing_without_metadata_stops_on_short_or_empty_page(self):
        for count in (0, 2, 100, 120):
            with self.subTest(count=count):
                expected = [f"item-{i}" for i in range(count)]

                def list_items(dataset_name, page, limit):
                    return SimpleNamespace(data=expected[(page - 1) * limit:page * limit])

                client = flywheel.LangfuseSDKClient(SimpleNamespace(
                    api=SimpleNamespace(dataset_items=SimpleNamespace(list=list_items)),
                ))
                self.assertEqual(client.list_dataset_items(flywheel.PRODUCTION_DATASET), expected)

    def test_sdk_listing_exposes_failure_after_first_page(self):
        def list_items(dataset_name, page, limit):
            if page == 2:
                raise RuntimeError("second page unavailable")
            return SimpleNamespace(data=["first"], meta=SimpleNamespace(total_pages=2))

        client = flywheel.LangfuseSDKClient(SimpleNamespace(
            api=SimpleNamespace(dataset_items=SimpleNamespace(list=list_items)),
        ))
        with self.assertRaises(RuntimeError):
            client.list_dataset_items(flywheel.PRODUCTION_DATASET)


if __name__ == "__main__":
    unittest.main()
