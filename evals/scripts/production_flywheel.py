#!/usr/bin/env python3
"""Production flywheel intake, promotion, and weekly report.

This is the executable core of the production replay pipeline named in
ADR 0025 and the evaluation strategy. It moves a production failure from an
observed trace into a maintainable eval case without making Langfuse a
coordination store:

  trace -> annotated draft dataset item -> versioned Git task source ->
  Harbor task -> fixed agent trial -> regression pass

The script is observational and fail-open by contract. Git task source plus
Harbor lock, result, artifacts, and scores remain authoritative; a Langfuse
outage, timeout, or retry never changes a reward, a promotion result, or a job
exit. Any Langfuse failure writes a retryable outbox entry and exits zero.

Identities are deterministic so a retry upserts the same draft items instead of
duplicating them.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

PRODUCTION_DATASET = "iron-forest-production"
PRODUCTION_MANIFEST = Path(__file__).resolve().parents[1] / "production-cases.json"
MANIFEST_SCHEMA = "forest.production-cases.v1"
CONTRACT_SCHEMA = "forest.production-case.v1"
OUTBOX_DIR_NAME = "production-flywheel-outbox"
SHIPPED_ROLES = {"builder", "verifier", "fixer"}
SCENARIO_FIELDS = ("issue", "powder_jobs", "check", "expected_files", "planted_files")
SLUG_PATTERN = re.compile(r"^[a-z0-9][a-z0-9_.-]*$")


def slug(value: str) -> str:
    """Return a URL-safe deterministic identifier fragment."""
    value = re.sub(r"[^A-Za-z0-9_.-]+", "-", value).strip("-")
    return value or "unnamed"


def read_json(path: Path) -> dict[str, Any]:
    with path.open() as handle:
        value = json.load(handle)
    if not isinstance(value, dict):
        raise ValueError(f"expected a JSON object: {path}")
    return value


def parse_run_log(path: Path) -> dict[str, Any] | None:
    """Return the first ``forest.run`` evidence line from a Run log, if any.

    The production Runner writes one typed JSON line per Run. This parser reads
    only that identity line and never copies unbounded Pi event content.
    """
    with path.open(errors="replace") as handle:
        for line in handle:
            line = line.strip()
            if not line or line.startswith("---"):
                continue
            try:
                value = json.loads(line)
            except json.JSONDecodeError:
                continue
            if isinstance(value, dict) and value.get("type") == "forest.run":
                return value
    return None


def run_logs(runs_dir: Path) -> list[Path]:
    if not runs_dir.is_dir():
        return []
    return sorted(runs_dir.glob("*.log"))


class LangfuseClient:
    """Minimal client surface used by the production flywheel.

    Kept as a small protocol so each subcommand can be tested with a
    deterministic fake and so the real implementation wraps the Langfuse SDK
    lazily.
    """

    def get_dataset_item(self, dataset_name: str, id: str) -> Any | None:
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

    def list_dataset_items(self, dataset_name: str) -> list[Any]:
        raise NotImplementedError

    def trace_ids_for_session(self, session_id: str) -> list[str]:
        raise NotImplementedError


class LangfuseSDKClient(LangfuseClient):
    def __init__(self, client: Any):
        self._client = client

    def get_dataset_item(self, dataset_name: str, id: str) -> Any | None:
        try:
            return self._client.api.dataset_items.get(id=id)
        except Exception:
            return None

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

    def list_dataset_items(self, dataset_name: str) -> list[Any]:
        try:
            items = self._client.api.dataset_items.list(dataset_name=dataset_name)
            return list(getattr(items, "data", []) or [])
        except Exception:
            return []

    def trace_ids_for_session(self, session_id: str) -> list[str]:
        try:
            traces = self._client.api.trace.list(session_id=session_id)
            return [trace.id for trace in getattr(traces, "data", [])]
        except Exception:
            return []


def build_client() -> LangfuseClient:
    try:
        from langfuse import Langfuse
    except Exception as exc:
        raise RuntimeError(f"langfuse is not installed: {exc}") from exc

    public_key = (sys.environ.get("LANGFUSE_PUBLIC_KEY") or "").strip()
    secret_key = (sys.environ.get("LANGFUSE_SECRET_KEY") or "").strip()
    base_url = (sys.environ.get("LANGFUSE_BASE_URL") or "").strip()
    if not public_key or not secret_key:
        raise RuntimeError("LANGFUSE_PUBLIC_KEY and LANGFUSE_SECRET_KEY are required")
    client = Langfuse(public_key=public_key, secret_key=secret_key, host=base_url or "https://cloud.langfuse.com")
    return LangfuseSDKClient(client)


def write_outbox(error: str) -> Path:
    outbox_dir = Path.cwd() / OUTBOX_DIR_NAME
    outbox_dir.mkdir(parents=True, exist_ok=True)
    entry = {
        "time": datetime.now(timezone.utc).isoformat(),
        "error": error,
    }
    path = outbox_dir / f"{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')}-flywheel.json"
    path.write_text(json.dumps(entry, indent=2, sort_keys=True) + "\n")
    return path


def item_id(run_id: str) -> str:
    return f"prod-{slug(run_id)}"


def ingest_runs(runs_dir: Path, client: LangfuseClient, dataset: str = PRODUCTION_DATASET) -> dict[str, Any]:
    created: list[str] = []
    skipped: list[str] = []
    for path in run_logs(runs_dir):
        evidence = parse_run_log(path)
        if evidence is None:
            continue
        run_id = evidence.get("run_id")
        agent = evidence.get("agent") or evidence.get("role")
        if not isinstance(run_id, str) or not run_id:
            continue
        if not isinstance(agent, str) or not agent:
            continue
        identifier = item_id(run_id)
        if client.get_dataset_item(dataset, identifier) is not None:
            skipped.append(identifier)
            continue
        located = client.trace_ids_for_session(run_id)
        source_trace_id = located[0] if located else run_id
        client.create_dataset_item(
            dataset_name=dataset,
            id=identifier,
            input={"run_id": run_id, "role": agent, "summary": None},
            expected_output=None,
            metadata={
                "status": "draft",
                "run_id": run_id,
                "role": agent,
                "outcome": "unknown",
                "source_trace_id": source_trace_id,
                "created_at": datetime.now(timezone.utc).isoformat(),
            },
            source_trace_id=source_trace_id,
        )
        created.append(identifier)
    return {"created": len(created), "skipped": len(skipped), "items": created}


def load_production_manifest(path: Path) -> dict[str, Any]:
    if path.exists():
        manifest = read_json(path)
        if manifest.get("schema") != MANIFEST_SCHEMA:
            raise ValueError(f"unsupported production manifest schema: {path}")
        if not isinstance(manifest.get("cases"), list):
            raise ValueError(f"production manifest has no cases list: {path}")
        return manifest
    return {"schema": MANIFEST_SCHEMA, "cases": []}


def validate_contract(contract: dict[str, Any], existing_ids: set[str]) -> None:
    if contract.get("schema") != CONTRACT_SCHEMA:
        raise ValueError(f"contract schema must be {CONTRACT_SCHEMA}")
    case_id = contract.get("id")
    if not isinstance(case_id, str) or not SLUG_PATTERN.match(case_id):
        raise ValueError("contract id must be a nonempty slug")
    if case_id in existing_ids:
        raise ValueError(f"duplicate production case id: {case_id}")
    if contract.get("role") not in SHIPPED_ROLES:
        raise ValueError("contract role must be builder, verifier, or fixer")
    for field in ("summary", "effect", "source_trace_id", "source_run_id"):
        if not isinstance(contract.get(field), str) or not contract[field]:
            raise ValueError(f"contract {field} must be a nonempty string")
    if not any(contract.get(field) for field in SCENARIO_FIELDS):
        raise ValueError("contract must include at least one scenario field: " + ", ".join(SCENARIO_FIELDS))


def promote_contract(contract: dict[str, Any], manifest_path: Path = PRODUCTION_MANIFEST) -> dict[str, Any]:
    manifest = load_production_manifest(manifest_path)
    existing_ids = {case.get("id") for case in manifest["cases"]}
    validate_contract(contract, existing_ids)
    case = dict(contract)
    case.pop("schema", None)
    case.setdefault("suite", "production-replay")
    case["promoted_at"] = datetime.now(timezone.utc).isoformat()
    manifest["cases"].append(case)
    manifest["cases"].sort(key=lambda entry: entry.get("id", ""))
    manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
    return {"promoted": case["id"], "cases": len(manifest["cases"])}


def report_markdown(client: LangfuseClient, manifest_path: Path = PRODUCTION_MANIFEST, dataset: str = PRODUCTION_DATASET) -> str:
    manifest = load_production_manifest(manifest_path)
    promoted = {case.get("id"): case for case in manifest["cases"]}
    promoted_traces = {case.get("source_trace_id") for case in promoted.values() if case.get("source_trace_id")}
    items = [item for item in client.list_dataset_items(dataset)]
    lines: list[str] = [
        "# Production flywheel weekly report",
        "",
        f"Generated: {datetime.now(timezone.utc).isoformat()}",
        "",
    ]

    def item_trace(item: Any) -> str | None:
        metadata = getattr(item, "metadata", None) or {}
        if isinstance(metadata, dict) and metadata.get("source_trace_id"):
            return metadata["source_trace_id"]
        return getattr(item, "source_trace_id", None)

    drafts = [item for item in items if item_trace(item) not in promoted_traces]
    new_cases = len(drafts)
    total = len(items)
    promoted_count = len(promoted)
    lines.append(f"- New draft cases awaiting verification: {new_cases}")
    lines.append(f"- Promoted production cases: {promoted_count}")

    role_counts: dict[str, int] = {}
    outcome_counts: dict[str, int] = {}
    trace_linked = 0
    ambiguous_or_broken = 0
    for item in items:
        metadata = getattr(item, "metadata", None) or {}
        if isinstance(metadata, dict):
            role = metadata.get("role") or "unknown"
            outcome = metadata.get("outcome") or "unknown"
            role_counts[role] = role_counts.get(role, 0) + 1
            outcome_counts[outcome] = outcome_counts.get(outcome, 0) + 1
            if metadata.get("source_trace_id"):
                trace_linked += 1
            if metadata.get("status") in {"ambiguous", "broken"}:
                ambiguous_or_broken += 1
    lines.append("- Coverage by role: " + (", ".join(f"{role}={count}" for role, count in sorted(role_counts.items())) or "none"))
    lines.append("- Coverage by outcome: " + (", ".join(f"{outcome}={count}" for outcome, count in sorted(outcome_counts.items())) or "none"))
    lines.append(f"- Production-distribution coverage: {trace_linked}/{total} dataset items carry a source trace id")
    lines.append(f"- Saturation: {promoted_count}/{promoted_count + new_cases} draft cases promoted")
    lines.append(f"- Ambiguous or broken drafts: {ambiguous_or_broken}")

    grader_exploit = sum(
        1 for case in promoted.values() if (case.get("metadata") or {}).get("source") == "grader-exploit" or case.get("source") == "grader-exploit"
    )
    lines.append(f"- Grader-exploit regressions promoted: {grader_exploit}")

    if new_cases:
        lines.append("")
        lines.append("## New draft cases")
        for item in drafts:
            lines.append(f"- {getattr(item, 'id', 'unknown')}")
    return "\n".join(lines) + "\n"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    ingest_parser = subparsers.add_parser("ingest", help="Turn production Run logs into draft Langfuse dataset items")
    ingest_parser.add_argument("--runs-dir", type=Path, default=Path(".forest/runs"))
    ingest_parser.add_argument("--dataset", default=PRODUCTION_DATASET)

    promote_parser = subparsers.add_parser("promote", help="Validate and record a human-verified production case")
    promote_parser.add_argument("--contract", type=Path, required=True)
    promote_parser.add_argument("--manifest", type=Path, default=PRODUCTION_MANIFEST)

    report_parser = subparsers.add_parser("report", help="Print a weekly production flywheel report")
    report_parser.add_argument("--dataset", default=PRODUCTION_DATASET)
    report_parser.add_argument("--manifest", type=Path, default=PRODUCTION_MANIFEST)

    args = parser.parse_args(argv)

    if args.command == "promote":
        try:
            contract = read_json(args.contract)
            result = promote_contract(contract, args.manifest)
        except Exception as exc:
            print(f"promotion rejected: {exc}", file=sys.stderr)
            return 1
        print(json.dumps(result, sort_keys=True))
        return 0

    try:
        client = build_client()
    except Exception as exc:
        outbox = write_outbox(str(exc))
        print(f"production flywheel skipped; retryable outbox entry written to {outbox}: {exc}", file=sys.stderr)
        return 0

    try:
        if args.command == "ingest":
            report = ingest_runs(args.runs_dir, client, args.dataset)
        else:
            report = {"report": report_markdown(client, args.manifest, args.dataset)}
    except Exception as exc:
        outbox = write_outbox(str(exc))
        print(f"production flywheel failed; retryable outbox entry written to {outbox}: {exc}", file=sys.stderr)
        return 0

    if args.command == "report":
        sys.stdout.write(report["report"])
    else:
        print(f"production flywheel complete: {json.dumps(report, sort_keys=True)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
