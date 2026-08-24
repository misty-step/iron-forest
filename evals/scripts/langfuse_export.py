#!/usr/bin/env python3
"""Export finished Harbor trials into Langfuse datasets, runs, and scores.

The export is observational and fail-open by contract (ADR 0025): Git task and
grader source plus Harbor lock, result, artifacts, and scores remain
authoritative, and a Langfuse outage, timeout, or retry never changes a Harbor
reward or a job exit. Any failure writes a retryable outbox entry and a warning
to stderr, then exits zero.

Identities are deterministic so a retry upserts the same dataset items, run
items, and scores instead of duplicating experiment data. One dataset run is
created per Harbor attempt index because Langfuse assumes one item occurrence
per experiment run.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

DATASET_NAME = "iron-forest-evals"
ENVIRONMENT = "production"
OUTBOX_DIR_NAME = "langfuse-outbox"
CASES_PATH = Path(__file__).resolve().parents[1] / "cases.json"


def slug(value: str) -> str:
    """Return a URL-safe deterministic identifier fragment."""
    value = re.sub(r"[^A-Za-z0-9_.-]+", "-", value).strip("-")
    return value or "unnamed"


def case_id(task_name: str) -> str:
    """Map a Harbor task name to the stable Forest eval case id."""
    return task_name.rsplit("/", 1)[-1]


def attempt_key(job_id: str, attempt_index: int) -> str:
    return f"{job_id}-attempt-{attempt_index}"


def score_key(job_id: str, attempt_index: int, case: str, name: str) -> str:
    return slug(f"{job_id}-{attempt_index}-{case}-{name}")


def read_json(path: Path) -> dict[str, Any]:
    with path.open() as handle:
        value = json.load(handle)
    if not isinstance(value, dict):
        raise ValueError(f"expected a JSON object: {path}")
    return value


def trial_dirs(job_dir: Path) -> list[Path]:
    """Return Harbor trial directories in a deterministic order."""
    return sorted(
        (path for path in job_dir.iterdir() if path.is_dir() and (path / "result.json").exists()),
        key=lambda path: path.name,
    )


def find_forest_run_id(trial_dir: Path) -> str | None:
    """Recover the candidate Forest Run id from the downloaded Run logs.

    The IronForestAgent copies the managed checkout's ``.forest/runs`` directory
    into the trial agent logs directory, and each retained Run log is named
    ``<run-id>.log``.
    """
    runs_dir = trial_dir / "agent" / "runs"
    if not runs_dir.is_dir():
        return None
    logs = sorted(runs_dir.glob("*.log"))
    if not logs:
        return None
    return logs[0].stem


def model_name(agent_info: dict[str, Any]) -> str | None:
    model = agent_info.get("model_info") or {}
    return model.get("name")


def provider_name(agent_info: dict[str, Any]) -> str | None:
    model = agent_info.get("model_info") or {}
    return model.get("provider")


def token_cost_totals(result: dict[str, Any]) -> dict[str, Any]:
    agent_result = result.get("agent_result") or {}
    step_results = result.get("step_results") or []
    if agent_result:
        contexts = [agent_result]
    elif step_results:
        contexts = [step.get("agent_result") for step in step_results if step.get("agent_result")]
    else:
        contexts = []
    totals: dict[str, Any] = {
        "input_tokens": None,
        "output_tokens": None,
        "cost_usd": None,
    }
    for context in contexts:
        if context.get("n_input_tokens") is not None:
            totals["input_tokens"] = (totals["input_tokens"] or 0) + context["n_input_tokens"]
        if context.get("n_output_tokens") is not None:
            totals["output_tokens"] = (totals["output_tokens"] or 0) + context["n_output_tokens"]
        if context.get("cost_usd") is not None:
            totals["cost_usd"] = (totals["cost_usd"] or 0) + context["cost_usd"]
    return totals


def sanitize_metadata(value: Any) -> Any:
    """Keep only scalar JSON-safe values, dropping nested artifacts and secrets."""
    if isinstance(value, dict):
        return {str(key): sanitize_metadata(item) for key, item in value.items() if str(key) not in _SECRET_FIELDS}
    if isinstance(value, list):
        return [sanitize_metadata(item) for item in value]
    if isinstance(value, (str, int, float, bool)) or value is None:
        return value
    return str(value)


_SECRET_FIELDS = {
    "api_key", "auth_token", "oauth_token", "secret", "password",
    "langfuse_public_key", "langfuse_secret_key", "openrouter_api_key",
    "forest_eval_judge_api_key",
}


def trial_summary(result: dict[str, Any], attempt_index: int, job_id: str, forest_run_id: str | None) -> dict[str, Any]:
    exception = result.get("exception_info") or {}
    totals = token_cost_totals(result)
    return {
        "job_id": job_id,
        "attempt_index": attempt_index,
        "case": case_id(result.get("task_name", "")),
        "trial_id": result.get("id"),
        "trial_name": result.get("trial_name"),
        "forest_run_id": forest_run_id,
        "model": model_name(result.get("agent_info") or {}),
        "provider": provider_name(result.get("agent_info") or {}),
        "exception_type": exception.get("exception_type"),
        "started_at": result.get("started_at"),
        "finished_at": result.get("finished_at"),
        "rewards": sanitize_metadata((result.get("verifier_result") or {}).get("rewards") or {}),
        "token_cost": sanitize_metadata(totals),
    }


def load_case_manifest() -> dict[str, dict[str, Any]]:
    """Map case ids to the authoritative Git task contract."""
    manifest = read_json(CASES_PATH)
    cases = manifest.get("cases")
    if not isinstance(cases, list):
        raise ValueError(f"case manifest has no cases list: {CASES_PATH}")
    return {case["id"]: case for case in cases if isinstance(case, dict) and case.get("id")}


def build_plan(job_dir: Path) -> list[dict[str, Any]]:
    """Return one export entry per trial, ordered deterministically.

    Harbor does not persist an attempt index on each trial result. The plan
    groups trials by task and sorts each group by start time (then trial name)
    so the same job directory always receives the same attempt indices.
    """
    trials: list[tuple[str, dict[str, Any], Path]] = []
    for trial_dir in trial_dirs(job_dir):
        result = read_json(trial_dir / "result.json")
        task_name = result.get("task_name", trial_dir.name)
        trials.append((task_name, result, trial_dir))

    plan: list[dict[str, Any]] = []
    groups: dict[str, list[tuple[str, dict[str, Any], Path]]] = {}
    for task_name, result, trial_dir in trials:
        groups.setdefault(task_name, []).append((task_name, result, trial_dir))

    for task_name, group in sorted(groups.items()):
        group.sort(key=lambda item: (item[1].get("started_at") or "", item[1].get("trial_name") or item[2].name))
        for attempt_index, (_, result, trial_dir) in enumerate(group):
            plan.append({
                "job_id": job_dir.name,
                "case": case_id(task_name),
                "attempt_index": attempt_index,
                "result": result,
                "trial_dir": trial_dir,
                "forest_run_id": find_forest_run_id(trial_dir),
            })
    return plan


class LangfuseClient:
    """Minimal client surface used by the exporter.

    Kept as a small protocol so the exporter can be tested with a deterministic
    fake and so the real implementation can wrap the Langfuse SDK lazily.
    """

    def ensure_dataset(self, name: str) -> None:
        raise NotImplementedError

    def create_dataset_item(
        self,
        *,
        dataset_name: str,
        id: str,
        input: Any,
        expected_output: Any,
        metadata: Any,
        source_trace_id: str | None = None,
    ) -> None:
        raise NotImplementedError

    def get_dataset_run(self, dataset_name: str, run_name: str) -> Any | None:
        raise NotImplementedError

    def create_dataset_run_item(
        self,
        *,
        run_name: str,
        dataset_item_id: str,
        trace_id: str,
        metadata: Any,
        run_description: str,
    ) -> Any:
        raise NotImplementedError

    def get_score(self, score_id: str) -> Any | None:
        raise NotImplementedError

    def create_score(
        self,
        *,
        name: str,
        value: Any,
        score_id: str,
        dataset_run_id: str | None,
        trace_id: str | None,
        data_type: str,
    ) -> None:
        raise NotImplementedError

    def trace_ids_for_session(self, session_id: str) -> list[str]:
        raise NotImplementedError


class LangfuseSDKClient(LangfuseClient):
    def __init__(self, client: Any):
        self._client = client

    def ensure_dataset(self, name: str) -> None:
        try:
            self._client.create_dataset(name=name)
        except Exception:
            # A dataset is immutable metadata in this exporter. If it already
            # exists the create call fails; treat that as success and let item
            # upsert surface any real transport failure.
            return

    def create_dataset_item(
        self,
        *,
        dataset_name: str,
        id: str,
        input: Any,
        expected_output: Any,
        metadata: Any,
        source_trace_id: str | None = None,
    ) -> None:
        self._client.create_dataset_item(
            dataset_name=dataset_name,
            id=id,
            input=input,
            expected_output=expected_output,
            metadata=metadata,
            source_trace_id=source_trace_id,
        )

    def get_dataset_run(self, dataset_name: str, run_name: str) -> Any | None:
        try:
            return self._client.get_dataset_run(dataset_name=dataset_name, run_name=run_name)
        except Exception:
            return None

    def create_dataset_run_item(
        self,
        *,
        run_name: str,
        dataset_item_id: str,
        trace_id: str,
        metadata: Any,
        run_description: str,
    ) -> Any:
        return self._client.api.dataset_run_items.create(
            run_name=run_name,
            dataset_item_id=dataset_item_id,
            trace_id=trace_id,
            metadata=metadata,
            run_description=run_description,
        )

    def get_score(self, score_id: str) -> Any | None:
        try:
            return self._client.api.scores.get_by_id(score_id)
        except Exception:
            return None

    def create_score(
        self,
        *,
        name: str,
        value: Any,
        score_id: str,
        dataset_run_id: str | None,
        trace_id: str | None,
        data_type: str,
    ) -> None:
        self._client.create_score(
            name=name,
            value=value,
            score_id=score_id,
            dataset_run_id=dataset_run_id,
            trace_id=trace_id,
            data_type=data_type,
            environment=ENVIRONMENT,
        )

    def trace_ids_for_session(self, session_id: str) -> list[str]:
        try:
            traces = self._client.api.trace.list(session_id=session_id)
            return [trace.id for trace in getattr(traces, "data", [])]
        except Exception:
            return []


def export_dataset_item(client: LangfuseClient, entry: dict[str, Any], case_info: dict[str, Any] | None) -> None:
    case = entry["case"]
    case_info = case_info or {}
    summary = sanitize_metadata(case_info.get("summary") or case)
    role = case_info.get("role")
    effect = case_info.get("effect")
    client.create_dataset_item(
        dataset_name=DATASET_NAME,
        id=case,
        input={"summary": summary, "case": case},
        expected_output={"role": role, "effect": effect},
        metadata={"case": case, "role": role, "effect": effect},
        source_trace_id=entry.get("forest_run_id"),
    )


def resolve_trace_id(client: LangfuseClient, forest_run_id: str | None) -> str | None:
    if not forest_run_id:
        return None
    located = client.trace_ids_for_session(forest_run_id)
    return located[0] if located else forest_run_id


def export_run_item(client: LangfuseClient, entry: dict[str, Any], run_name: str) -> str | None:
    trace_id = resolve_trace_id(client, entry.get("forest_run_id"))
    run = client.get_dataset_run(DATASET_NAME, run_name)
    existing = None
    if run is not None:
        dataset_run_id = getattr(run, "id", None)
        for item in getattr(run, "dataset_run_items", []) or []:
            if getattr(item, "dataset_item_id", None) == entry["case"]:
                existing = item
                break
    else:
        dataset_run_id = None

    if existing is None:
        created = client.create_dataset_run_item(
            run_name=run_name,
            dataset_item_id=entry["case"],
            trace_id=trace_id,
            metadata=sanitize_metadata(trial_summary(entry["result"], entry["attempt_index"], entry["job_id"], entry["forest_run_id"])),
            run_description=f"Forest {entry['case']} attempt {entry['attempt_index']}",
        )
        dataset_run_id = getattr(created, "dataset_run_id", None)
    return dataset_run_id


def export_scores(
    client: LangfuseClient,
    entry: dict[str, Any],
    run_name: str,
    dataset_run_id: str,
) -> None:
    result = entry["result"]
    trace_id = resolve_trace_id(client, entry.get("forest_run_id"))
    rewards = (result.get("verifier_result") or {}).get("rewards") or {}
    for name, value in sorted(rewards.items()):
        if not isinstance(value, (int, float)):
            continue
        score_id = score_key(entry["job_id"], entry["attempt_index"], entry["case"], name)
        if client.get_score(score_id) is None:
            client.create_score(
                name=name,
                value=value,
                score_id=score_id,
                dataset_run_id=dataset_run_id,
                trace_id=trace_id,
                data_type="NUMERIC",
            )
    exception = (result.get("exception_info") or {}).get("exception_type")
    exception_score_id = score_key(entry["job_id"], entry["attempt_index"], entry["case"], "exception")
    if client.get_score(exception_score_id) is None:
        client.create_score(
            name="exception",
            value=exception or "none",
            score_id=exception_score_id,
            dataset_run_id=dataset_run_id,
            trace_id=trace_id,
            data_type="CATEGORICAL",
        )


def export_job(job_dir: Path, client: LangfuseClient, manifest: dict[str, dict[str, Any]] | None = None) -> dict[str, Any]:
    if manifest is None:
        manifest = load_case_manifest()
    plan = build_plan(job_dir)
    client.ensure_dataset(DATASET_NAME)

    exported_cases: set[str] = set()
    exported_runs: set[str] = set()
    for entry in plan:
        if entry["case"] not in exported_cases:
            export_dataset_item(client, entry, manifest.get(entry["case"]))
            exported_cases.add(entry["case"])

        if entry["forest_run_id"] is None:
            continue

        run_name = attempt_key(entry["job_id"], entry["attempt_index"])
        dataset_run_id = export_run_item(client, entry, run_name)
        if dataset_run_id is None:
            continue
        exported_runs.add(run_name)
        export_scores(client, entry, run_name, dataset_run_id)

    return {
        "job": job_dir.name,
        "trials": len(plan),
        "cases": len(exported_cases),
        "dataset_runs": len(exported_runs),
    }


def build_client() -> LangfuseClient:
    try:
        from langfuse import Langfuse
    except Exception as exc:
        raise RuntimeError(f"langfuse is not installed: {exc}") from exc

    public_key = (sys.environ.get("LANGFUSE_PUBLIC_KEY") or "").strip()
    secret_key = (sys.environ.get("LANGFUSE_SECRET_KEY") or "").strip()
    base_url = (sys.environ.get("LANGFUSE_BASE_URL") or "").strip()
    if not public_key or not secret_key:
        raise RuntimeError("LANGFUSE_PUBLIC_KEY and LANGFUSE_SECRET_KEY are required for export")
    client = Langfuse(public_key=public_key, secret_key=secret_key, host=base_url or "https://cloud.langfuse.com")
    return LangfuseSDKClient(client)


def write_outbox(job_dir: Path, error: str) -> Path:
    outbox_dir = job_dir / OUTBOX_DIR_NAME
    outbox_dir.mkdir(parents=True, exist_ok=True)
    entry = {
        "job": job_dir.name,
        "time": datetime.now(timezone.utc).isoformat(),
        "error": error,
    }
    path = outbox_dir / f"{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')}-{slug(job_dir.name)}.json"
    path.write_text(json.dumps(entry, indent=2, sort_keys=True) + "\n")
    return path


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("job_dir", type=Path, help="Path to one completed Harbor job directory")
    args = parser.parse_args(argv)

    job_dir = args.job_dir
    if not job_dir.is_dir():
        print(f"langfuse export: not a directory: {job_dir}", file=sys.stderr)
        return 0

    try:
        client = build_client()
    except Exception as exc:
        outbox = write_outbox(job_dir, str(exc))
        print(f"langfuse export skipped; retryable outbox entry written to {outbox}: {exc}", file=sys.stderr)
        return 0

    try:
        report = export_job(job_dir, client)
    except Exception as exc:
        outbox = write_outbox(job_dir, str(exc))
        print(f"langfuse export failed; retryable outbox entry written to {outbox}: {exc}", file=sys.stderr)
        return 0

    print(f"langfuse export complete: {json.dumps(report, sort_keys=True)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
