#!/usr/bin/env python3
"""Fetch and summarize Iron Forest experiment history from Langfuse."""

from __future__ import annotations

import argparse
import json
import os
import sys
from collections import defaultdict
from statistics import median
from pathlib import Path
from typing import Any

from langfuse_config import normalized_base_url

DATASET_NAME = "iron-forest-evals"
HISTORY_SCHEMA = "forest.eval.experiment-history.v1"


def metadata_items(client: Any, limit: int) -> list[dict[str, Any]]:
    records: list[dict[str, Any]] = []
    page = 1
    while len(records) < limit:
        try:
            response = client.api.datasets.get_runs(
                DATASET_NAME,
                page=page,
                limit=min(100, limit - len(records)),
            )
        except Exception as error:
            if page == 1 and getattr(error, "status_code", None) == 404:
                client.create_dataset(
                    name=DATASET_NAME,
                    description="Paired Iron Forest incumbent and contender evaluations.",
                )
                return records
            raise
        runs = list(getattr(response, "data", []) or [])
        if not runs:
            break
        for run in runs:
            full = client.get_dataset_run(dataset_name=DATASET_NAME, run_name=run.name)
            for item in getattr(full, "dataset_run_items", []) or []:
                metadata = getattr(item, "metadata", None)
                if isinstance(metadata, dict) and metadata.get("experiment"):
                    records.append(metadata)
                    if len(records) >= limit:
                        break
            if len(records) >= limit:
                break
        meta = getattr(response, "meta", None)
        total_pages = getattr(meta, "total_pages", None) or getattr(meta, "totalPages", None)
        if len(runs) < 100 or (isinstance(total_pages, int) and page >= total_pages):
            break
        page += 1
    return records


def summarize(items: list[dict[str, Any]]) -> dict[str, Any]:
    grouped: dict[str, list[dict[str, Any]]] = defaultdict(list)
    for item in items:
        experiment = item.get("experiment") or {}
        fingerprint = experiment.get("experiment_fingerprint")
        if fingerprint:
            grouped[str(fingerprint)].append(item)

    records: list[dict[str, Any]] = []
    for fingerprint, entries in sorted(grouped.items()):
        experiment = entries[0]["experiment"]
        rewards = [entry.get("rewards") or {} for entry in entries]
        passed = sum(
            1
            for entry, reward in zip(entries, rewards, strict=True)
            if reward.get("deterministic") == 1
            and (reward.get("judge") in {None, 1})
            and not entry.get("exception_type")
        )
        costs = [
            (entry.get("token_cost") or {}).get("cost_usd")
            for entry in entries
            if (entry.get("token_cost") or {}).get("cost_usd") is not None
        ]
        durations = [
            float(entry["duration_seconds"])
            for entry in entries
            if entry.get("duration_seconds") is not None
        ]
        input_tokens = [
            int((entry.get("token_cost") or {})["input_tokens"])
            for entry in entries
            if (entry.get("token_cost") or {}).get("input_tokens") is not None
        ]
        output_tokens = [
            int((entry.get("token_cost") or {})["output_tokens"])
            for entry in entries
            if (entry.get("token_cost") or {}).get("output_tokens") is not None
        ]
        configuration_map: dict[str, dict[str, Any]] = {}
        for entry in entries:
            for configuration in (entry.get("experiment") or {}).get("configurations", []):
                if not isinstance(configuration, dict):
                    continue
                key = str(
                    configuration.get("configuration_fingerprint")
                    or json.dumps(configuration, sort_keys=True)
                )
                configuration_map[key] = configuration
        status_counts: dict[str, int] = defaultdict(int)
        for entry in entries:
            status_counts[str(entry.get("status") or "unclassified")] += 1
        records.append(
            {
                "experiment_fingerprint": fingerprint,
                "tier": experiment.get("tier"),
                "variant": (experiment.get("variant") or {}).get("id"),
                "source_revision": experiment.get("source_revision"),
                "planner": experiment.get("planner"),
                "cohorts": sorted({str((entry.get("experiment") or {}).get("cohort")) for entry in entries}),
                "cases": sorted({str(entry.get("case")) for entry in entries}),
                "trials": len(entries),
                "passed_trials": passed,
                "pass_rate": round(passed / len(entries), 4) if entries else None,
                "cost_usd": round(sum(float(cost) for cost in costs), 6) if costs else None,
                "mean_duration_seconds": (
                    round(sum(durations) / len(durations), 3) if durations else None
                ),
                "median_duration_seconds": round(median(durations), 3) if durations else None,
                "input_tokens": sum(input_tokens) if input_tokens else None,
                "output_tokens": sum(output_tokens) if output_tokens else None,
                "configurations": [configuration_map[key] for key in sorted(configuration_map)],
                "status_counts": dict(sorted(status_counts.items())),
                "configuration_fingerprints": sorted(
                    {
                        str(configuration.get("configuration_fingerprint"))
                        for entry in entries
                        for configuration in (entry.get("experiment") or {}).get("configurations", [])
                        if configuration.get("configuration_fingerprint")
                    }
                ),
            }
        )
    variant_counts: dict[str, int] = defaultdict(int)
    global_status_counts: dict[str, int] = defaultdict(int)
    for record in records:
        if record.get("variant"):
            variant_counts[str(record["variant"])] += 1
        for status, count in record["status_counts"].items():
            global_status_counts[status] += int(count)
    all_durations = [
        float(item["duration_seconds"])
        for item in items
        if item.get("duration_seconds") is not None
    ]
    return {
        "schema": HISTORY_SCHEMA,
        "records": records,
        "summary": {
            "experiments": len(records),
            "trials": sum(record["trials"] for record in records),
            "cost_usd": round(sum(float(record["cost_usd"] or 0) for record in records), 6),
            "input_tokens": sum(int(record["input_tokens"] or 0) for record in records),
            "output_tokens": sum(int(record["output_tokens"] or 0) for record in records),
            "mean_duration_seconds": (
                round(sum(all_durations) / len(all_durations), 3)
                if all_durations
                else None
            ),
            "status_counts": dict(sorted(global_status_counts.items())),
            "variant_experiments": dict(sorted(variant_counts.items())),
        },
    }


def build_client() -> Any:
    from langfuse import Langfuse

    public_key = (os.environ.get("LANGFUSE_PUBLIC_KEY") or "").strip()
    secret_key = (os.environ.get("LANGFUSE_SECRET_KEY") or "").strip()
    base_url = normalized_base_url(os.environ.get("LANGFUSE_BASE_URL"))
    if not public_key or not secret_key:
        raise RuntimeError("Langfuse credentials are unavailable")
    return Langfuse(
        public_key=public_key,
        secret_key=secret_key,
        host=base_url,
    )


def render_markdown(history: dict[str, Any]) -> str:
    summary = history["summary"]
    lines = [
        "# Iron Forest experiment history",
        "",
        f"Experiments: {summary['experiments']}",
        f"Trials: {summary['trials']}",
        f"Recorded cost: ${summary['cost_usd']:.6f}",
        f"Recorded tokens: {summary['input_tokens']} input, {summary['output_tokens']} output",
        f"Mean trial duration: {summary['mean_duration_seconds'] if summary['mean_duration_seconds'] is not None else 'unknown'} seconds",
        "",
        "| Variant | Tier | Trials | Pass rate | Cost | Mean seconds | Fingerprint |",
        "| --- | --- | ---: | ---: | ---: | ---: | --- |",
    ]
    for record in history["records"]:
        cost = "unknown" if record["cost_usd"] is None else f"${record['cost_usd']:.6f}"
        lines.append(
            f"| {record['variant']} | {record['tier']} | {record['trials']} | {record['pass_rate']} | {cost} | {record['mean_duration_seconds']} | `{record['experiment_fingerprint'][:12]}` |"
        )
    return "\n".join(lines) + "\n"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--markdown", type=Path)
    parser.add_argument("--limit", type=int, default=500)
    parser.add_argument("--allow-unavailable", action="store_true")
    args = parser.parse_args(argv)
    try:
        history = summarize(metadata_items(build_client(), args.limit))
    except Exception as error:
        if not args.allow_unavailable:
            print(f"experiment history failed: {error}", file=sys.stderr)
            return 2
        history = {
            "schema": HISTORY_SCHEMA,
            "records": [],
            "summary": {
                "experiments": 0,
                "trials": 0,
                "cost_usd": 0,
                "input_tokens": 0,
                "output_tokens": 0,
                "mean_duration_seconds": None,
                "status_counts": {},
                "variant_experiments": {},
            },
            "warning": type(error).__name__,
        }
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(history, indent=2, sort_keys=True) + "\n")
    if args.markdown:
        args.markdown.write_text(render_markdown(history))
    print(json.dumps(history["summary"], sort_keys=True))
    return 0

if __name__ == "__main__":
    raise SystemExit(main())
