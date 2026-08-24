from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

import langfuse_export as exporter  # noqa: E402


class FakeRunItem:
    def __init__(self, dataset_item_id: str, trace_id: str | None, dataset_run_id: str):
        self.dataset_item_id = dataset_item_id
        self.trace_id = trace_id
        self.dataset_run_id = dataset_run_id


class FakeRun:
    def __init__(self, run_id: str, name: str):
        self.id = run_id
        self.name = name
        self.dataset_run_items: list[FakeRunItem] = []


class FakeLangfuseClient(exporter.LangfuseClient):
    def __init__(self, located_trace_ids: dict[str, list[str]] | None = None):
        self.datasets: set[str] = set()
        self.items: dict[str, dict] = {}
        self.runs: dict[str, FakeRun] = {}
        self.scores: dict[str, dict] = {}
        self.created_run_items: list[FakeRunItem] = []
        self.located_trace_ids = located_trace_ids or {}

    def ensure_dataset(self, name: str) -> None:
        self.datasets.add(name)

    def create_dataset_item(self, *, dataset_name, id, input, expected_output, metadata, source_trace_id=None) -> None:
        self.items[id] = {
            "dataset_name": dataset_name,
            "input": input,
            "expected_output": expected_output,
            "metadata": metadata,
            "source_trace_id": source_trace_id,
        }

    def get_dataset_run(self, dataset_name: str, run_name: str):
        return self.runs.get(run_name)

    def create_dataset_run_item(self, *, run_name, dataset_item_id, trace_id, metadata, run_description):
        run = self.runs.get(run_name)
        if run is None:
            run = FakeRun(f"run-{run_name}", run_name)
            self.runs[run_name] = run
        item = FakeRunItem(dataset_item_id, trace_id, run.id)
        run.dataset_run_items.append(item)
        self.created_run_items.append(item)
        return item

    def get_score(self, score_id: str):
        return self.scores.get(score_id)

    def create_score(self, *, name, value, score_id, dataset_run_id, trace_id, data_type):
        self.scores[score_id] = {
            "name": name,
            "value": value,
            "dataset_run_id": dataset_run_id,
            "trace_id": trace_id,
            "data_type": data_type,
        }

    def trace_ids_for_session(self, session_id: str) -> list[str]:
        return self.located_trace_ids.get(session_id, [])


class LangfuseExportTest(unittest.TestCase):
    def setUp(self):
        self.manifest = {
            "builder-ready": {"id": "builder-ready", "role": "builder", "effect": "builder_publish", "summary": "Publish one ready issue."},
            "verifier-clean": {"id": "verifier-clean", "role": "verifier", "effect": "verifier_approve", "summary": "Approve a clean revision."},
        }

    def write_trial(self, job_dir: Path, case: str, attempt: int, started_at: str, forest_run_id: str | None, *, exception: str | None = None):
        trial_dir = job_dir / f"{case}__{attempt}"
        (trial_dir / "agent" / "runs").mkdir(parents=True)
        if forest_run_id:
            (trial_dir / "agent" / "runs" / f"{forest_run_id}.log").write_text(
                '{"type":"forest.run","run_id":"' + forest_run_id + '"}\n'
            )
        result = {
            "id": f"{case}-{attempt}",
            "task_name": f"iron-forest/{case}",
            "trial_name": f"{case}__{attempt}-abc",
            "started_at": started_at,
            "finished_at": started_at,
            "agent_info": {
                "name": "iron-forest",
                "version": "1",
                "model_info": {"name": "openrouter/deepseek/deepseek-v4-pro-0813", "provider": "openrouter"},
            },
            "verifier_result": {"rewards": {"deterministic": 1.0, "judge": 0.0}},
            "exception_info": {"exception_type": exception, "exception_message": "boom", "exception_traceback": "secret traceback"} if exception else None,
            "agent_result": {"n_input_tokens": 100, "n_output_tokens": 40, "cost_usd": 0.02},
        }
        (trial_dir / "result.json").write_text(json.dumps(result))
        (trial_dir / "lock.json").write_text('{"task": {"digest": "sha256:abc"}}\n')
        return trial_dir

    def build_job(self, root: Path, cases: list[str], attempts: int) -> Path:
        job_dir = root / "model-job"
        for case in cases:
            for attempt in range(attempts):
                self.write_trial(
                    job_dir,
                    case,
                    attempt,
                    f"2026-08-24T00:0{attempt}:00Z",
                    f"1787529620484390170-{case}-{attempt}",
                )
        return job_dir

    def test_one_run_per_attempt_is_idempotent(self):
        with tempfile.TemporaryDirectory() as root:
            job_dir = self.build_job(Path(root), ["builder-ready", "verifier-clean"], 3)
            client = FakeLangfuseClient()
            report = exporter.export_job(job_dir, client, self.manifest)

            self.assertEqual(report["trials"], 6)
            self.assertEqual(report["cases"], 2)
            self.assertEqual(report["dataset_runs"], 3)
            self.assertEqual(set(client.runs), {
                "model-job-attempt-0",
                "model-job-attempt-1",
                "model-job-attempt-2",
            })
            for run in client.runs.values():
                self.assertEqual(len(run.dataset_run_items), 2)

            run_item_count = len(client.created_run_items)
            score_count = len(client.scores)
            item_count = len(client.items)

            second_report = exporter.export_job(job_dir, client, self.manifest)
            self.assertEqual(second_report, report)
            self.assertEqual(len(client.created_run_items), run_item_count)
            self.assertEqual(len(client.scores), score_count)
            self.assertEqual(len(client.items), item_count)

    def test_export_never_uploads_artifacts_or_tracebacks(self):
        with tempfile.TemporaryDirectory() as root:
            job_dir = self.build_job(Path(root), ["builder-ready"], 1)
            trial_dir = next(job_dir.iterdir())
            secret = "sk-this-must-not-leave-the-repository"
            (trial_dir / "artifacts").mkdir(parents=True, exist_ok=True)
            (trial_dir / "artifacts" / "secrets.txt").write_text(secret + "\n")
            result = json.loads((trial_dir / "result.json").read_text())
            result["exception_info"] = {
                "exception_type": "ValueError",
                "exception_message": "boom",
                "exception_traceback": secret,
            }
            (trial_dir / "result.json").write_text(json.dumps(result))

            client = FakeLangfuseClient()
            exporter.export_job(job_dir, client, self.manifest)

            serialized = json.dumps(
                {
                    "items": list(client.items.values()),
                    "scores": list(client.scores.values()),
                    "run_items": [item.trace_id for item in client.created_run_items],
                }
            )
            self.assertNotIn(secret, serialized)

    def test_stable_identities_and_trace_linking(self):
        with tempfile.TemporaryDirectory() as root:
            job_dir = self.build_job(Path(root), ["builder-ready"], 2)
            client = FakeLangfuseClient(located_trace_ids={"1787529620484390170-builder-ready-0": ["trace-openrouter-1"]})
            exporter.export_job(job_dir, client, self.manifest)

            self.assertEqual(set(client.items), {"builder-ready"})
            self.assertIn("model-job-attempt-0", client.runs)
            first_run_item = client.runs["model-job-attempt-0"].dataset_run_items[0]
            self.assertEqual(first_run_item.dataset_item_id, "builder-ready")
            self.assertEqual(first_run_item.trace_id, "trace-openrouter-1")
            self.assertIn("deterministic", client.scores["model-job-0-builder-ready-deterministic"]["name"])


if __name__ == "__main__":
    unittest.main()
