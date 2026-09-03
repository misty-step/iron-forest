#!/usr/bin/env python3
"""Assert and report on finished Iron Forest Harbor jobs.

The old assertion only checked that every reward equaled one. A saturated or
broken 20/20 suite looked identical to a healthy one, infra failures were
counted as model failures, and there was no promotion policy. This reporter
keeps the deterministic safety gate for regression cases while separating
regression from capability suites, classifying infra exceptions, recording the
environment and definition digests that Harbor resolved, and applying an
executable pass/fail policy per change class.

The reporter is observational over ``result.json`` and ``lock.json`` only. It
never uploads artifacts or exception messages, and it never reruns a trial.
"""

from __future__ import annotations

import argparse
import json
import math
import sys
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

REPORT_SCHEMA = "forest.eval.report.v1"
SUITE_REGRESSION = "regression"
SUITE_CAPABILITY = "capability"
SUITES = {SUITE_REGRESSION, SUITE_CAPABILITY}

DEFAULT_CASES = Path(__file__).resolve().parents[1] / "cases.json"
REPORT_JSON = "report.json"
REPORT_MD = "report.md"

# Change classes named in the promotion contract. ``prompt/skill``,
# ``model/thinking``, and ``tool/kernel`` are kept as separate canonical
# values that share one policy so callers can be specific without two spellings
# meaning different gates.
CHANGE_CLASSES = {
    "prompt",
    "skill",
    "model",
    "thinking",
    "tool",
    "kernel",
    "grader",
    "deployment",
}

_INFRA_MARKERS = (
    "docker",
    "connection",
    "timeout",
    "timed out",
    "cancelled",
    "cancelerror",
    "resource",
    "out of memory",
    "memoryerror",
    "disk",
    "storage",
    "image",
    "network",
    "provider",
    "rate limit",
    "429",
    "500",
    "502",
    "503",
    "504",
    "retry",
    "connection reset",
    "no space left",
)
_PROVIDER_MARKERS = ("provider", "openrouter", "rate limit", "429", "quota", "billing", "guardrail")
_INCOMPLETE_MARKERS = ("cancelled", "cancelerror", "interrupted", "workflow timeout")


@dataclass(frozen=True)
class GatePolicy:
    """Executable promotion requirements for one change class."""

    requires_judge: bool = False
    requires_regrade: bool = False
    requires_adr: bool = False


CHANGE_CLASS_POLICY: dict[str, GatePolicy] = {
    "prompt": GatePolicy(requires_judge=True),
    "skill": GatePolicy(requires_judge=True),
    "model": GatePolicy(requires_judge=True),
    "thinking": GatePolicy(requires_judge=True),
    "tool": GatePolicy(requires_judge=True, requires_adr=True),
    "kernel": GatePolicy(requires_judge=True, requires_adr=True),
    "grader": GatePolicy(requires_judge=True, requires_regrade=True),
    "deployment": GatePolicy(requires_judge=True),
}

MODEL_CHANGE_CLASSES = {"model", "thinking"}


def read_json(path: Path) -> dict[str, Any]:
    with path.open() as handle:
        value = json.load(handle)
    if not isinstance(value, dict):
        raise ValueError(f"expected a JSON object: {path}")
    return value


def parse_expected_cases(value: str) -> set[str]:
    """Parse ``--expected-cases`` as a JSON array of case id strings."""
    try:
        parsed = json.loads(value)
    except json.JSONDecodeError as error:
        raise argparse.ArgumentTypeError(f"invalid JSON: {error}") from error
    if not isinstance(parsed, list) or not all(isinstance(item, str) for item in parsed):
        raise argparse.ArgumentTypeError(
            "--expected-cases must be a JSON array of case id strings"
        )
    return set(parsed)


def case_coverage_failures(
    report: dict[str, Any], expected_cases: set[str] | None
) -> list[str]:
    """Fail closed when a produced report does not cover the planned cases."""
    if expected_cases is None:
        return []
    actual = set(report["cases"])
    missing = sorted(expected_cases - actual)
    unexpected = sorted(actual - expected_cases)
    if not missing and not unexpected:
        return []
    return [
        f"case coverage mismatch for {report['job']}: "
        f"missing {missing or 'none'}, unexpected {unexpected or 'none'}"
    ]


def discover_trials(job_dir: Path) -> list[Path]:
    """Return every Harbor trial ``result.json`` under a job directory.

    Unreadable result files fail closed rather than silently narrowing the
    report. Only objects carrying a ``task_name`` are treated as trials so a
    future report file cannot be mistaken for one.
    """
    trials: list[Path] = []
    for path in sorted(job_dir.rglob("result.json")):
        try:
            value = read_json(path)
        except (OSError, json.JSONDecodeError, ValueError) as error:
            raise SystemExit(f"cannot read {path}: {error}")
        if "task_name" in value:
            trials.append(path)
    return trials


def task_case(result: dict[str, Any]) -> str:
    """Map a Harbor task name to the stable Forest eval case id."""
    return str(result.get("task_name", "")).rsplit("/", 1)[-1]


def rewards(result: dict[str, Any]) -> dict[str, Any]:
    verifier = result.get("verifier_result") or {}
    return verifier.get("rewards") or {}


def deterministic_reward(result: dict[str, Any]) -> Any:
    values = rewards(result)
    if "deterministic" in values:
        return values["deterministic"]
    if len(values) == 1:
        return next(iter(values.values()))
    return None


def judge_reward(result: dict[str, Any]) -> Any:
    return rewards(result).get("judge")


def classify_exception(exception: dict[str, Any] | None) -> str | None:
    """Classify an exception as infra vs agent without copying its contents.

    The classification is a best-effort triage aid, not an authority. The raw
    ``exception_type`` remains in the report; the message and traceback are
    never copied because they can carry credential-shaped content.
    """
    if exception is None:
        return None
    text = " ".join(
        str(exception.get(field) or "")
        for field in ("exception_type", "exception_message")
    ).lower()
    if any(marker in text for marker in _INFRA_MARKERS):
        return "infra"
    return "agent"

def exception_status(exception: dict[str, Any] | None) -> str | None:
    if exception is None:
        return None
    text = " ".join(
        str(exception.get(field) or "")
        for field in ("exception_type", "exception_message")
    ).lower()
    if any(marker in text for marker in _PROVIDER_MARKERS):
        return "provider-unavailable"
    if any(marker in text for marker in _INCOMPLETE_MARKERS):
        return "incomplete"
    if classify_exception(exception) == "infra":
        return "infra-error"
    return "agent-error"


def trial_outcome(result: dict[str, Any], require_judge: bool) -> dict[str, Any]:
    """Return a structured trial outcome.

    Deterministic safety is authoritative: an exception or a non-one
    deterministic reward is never promoted by a model judge. When
    ``require_judge`` is set, a missing or failing judge reward also fails.
    """
    exception = result.get("exception_info")
    deterministic = deterministic_reward(result)
    judge = judge_reward(result)

    if exception is not None:
        outcome = "exception"
        status = exception_status(exception)
    elif deterministic is None:
        outcome = "no-reward"
        status = "incomplete"
    elif deterministic != 1 and judge == 1:
        outcome = "fail"
        status = "judge-disagreement"
    elif deterministic != 1:
        outcome = "fail"
        status = "safety-failure"
    elif require_judge and judge is None:
        outcome = "judge-missing"
        status = "judge-error"
    elif require_judge and judge != 1:
        outcome = "judge-fail"
        status = "behavior-failure"
    else:
        outcome = "pass"
        status = "pass"

    return {
        "outcome": outcome,
        "status": status,
        "deterministic": deterministic,
        "judge": judge,
        "exception_type": (exception or {}).get("exception_type"),
        "exception_class": classify_exception(exception),
    }


def wilson_interval(successes: int, trials: int, z: float = 1.96) -> list[float]:
    """Wilson score interval for a binomial proportion.

    Small capability suites get honest uncertainty instead of a flattering
    point estimate; a 1/1 case is reported as a wide interval, not certainty.
    """
    if trials <= 0:
        return [0.0, 0.0]
    n = float(trials)
    p = successes / n
    z2 = z * z
    denom = 1.0 + z2 / n
    center = (p + z2 / (2.0 * n)) / denom
    margin = z * math.sqrt((p * (1.0 - p) / n) + (z2 / (4.0 * n * n))) / denom
    lower = max(0.0, center - margin)
    upper = min(1.0, center + margin)
    return [round(lower, 4), round(upper, 4)]


def token_totals(result: dict[str, Any]) -> dict[str, Any]:
    """Sum token and cost diagnostics across single- or multi-step trials."""
    agent_result = result.get("agent_result") or {}
    step_results = result.get("step_results") or []
    if agent_result:
        contexts = [agent_result]
    else:
        contexts = [
            step.get("agent_result") or {}
            for step in step_results
            if isinstance(step, dict) and (step.get("agent_result") or {})
        ]
    totals: dict[str, Any] = {
        "input_tokens": None,
        "cache_tokens": None,
        "output_tokens": None,
        "cost_usd": None,
    }
    for context in contexts:
        for key, field in (
            ("input_tokens", "n_input_tokens"),
            ("cache_tokens", "n_cache_tokens"),
            ("output_tokens", "n_output_tokens"),
            ("cost_usd", "cost_usd"),
        ):
            value = context.get(field)
            if value is not None:
                totals[key] = (totals[key] or 0) + value
    return totals


def model_info(result: dict[str, Any]) -> dict[str, Any]:
    agent_info = result.get("agent_info") or {}
    model = agent_info.get("model_info") or {}
    return {
        "agent": agent_info.get("name"),
        "model": model.get("name"),
        "provider": model.get("provider"),
    }


def duration_seconds(result: dict[str, Any]) -> float | None:
    started = result.get("started_at")
    finished = result.get("finished_at")
    if not started or not finished:
        return None
    try:
        start = datetime.fromisoformat(str(started).replace("Z", "+00:00"))
        finish = datetime.fromisoformat(str(finished).replace("Z", "+00:00"))
    except ValueError:
        return None
    return round((finish - start).total_seconds(), 3)


def task_digest(result: dict[str, Any], lock: dict[str, Any]) -> str | None:
    task = (lock or {}).get("task") or {}
    return task.get("digest") or result.get("task_checksum")


def skill_digests(lock: dict[str, Any]) -> list[dict[str, Any]]:
    skills = (lock or {}).get("skills") or []
    return [
        {
            "name": skill.get("name"),
            "digest": skill.get("digest"),
            "source": str(skill.get("source")) if skill.get("source") is not None else None,
        }
        for skill in skills
        if isinstance(skill, dict)
    ]


def resource_profile(lock: dict[str, Any]) -> dict[str, Any]:
    """Record the resolved resource ceilings Harbor placed on the trial.

    Harbor records enforcement ceilings (``override_*``), not floors, so the
    floor fields remain ``None``. This keeps the report honest about what was
    actually controlled rather than inventing a lower bound.
    """
    env = (lock or {}).get("environment") or {}
    return {
        "cpu": {
            "floor": None,
            "ceiling": env.get("override_cpus"),
            "enforcement": env.get("cpu_enforcement_policy"),
        },
        "memory_mb": {
            "floor": None,
            "ceiling": env.get("override_memory_mb"),
            "enforcement": env.get("memory_enforcement_policy"),
        },
        "storage_mb": {
            "floor": None,
            "ceiling": env.get("override_storage_mb"),
            "enforcement": None,
        },
    }


def job_concurrency(job_dir: Path) -> int | None:
    lock_path = job_dir / "lock.json"
    if not lock_path.is_file():
        return None
    try:
        lock = read_json(lock_path)
    except (OSError, json.JSONDecodeError, ValueError):
        return None
    value = lock.get("n_concurrent_trials")
    return value if isinstance(value, int) else None


def trial_is_regrade(result: dict[str, Any], lock: dict[str, Any]) -> bool:
    config = result.get("config") or {}
    return bool(config.get("source_trial") or (lock or {}).get("source_trial"))


def load_cases(cases_path: Path) -> dict[str, dict[str, Any]]:
    try:
        manifest = read_json(cases_path)
    except (OSError, json.JSONDecodeError, ValueError) as error:
        raise SystemExit(f"cannot read case manifest {cases_path}: {error}")
    if manifest.get("schema") != "forest.evals.v1":
        raise SystemExit(f"unsupported case manifest schema in {cases_path}")
    cases = manifest.get("cases")
    if not isinstance(cases, list):
        raise SystemExit(f"case manifest has no cases list: {cases_path}")
    return {
        case["id"]: case
        for case in cases
        if isinstance(case, dict) and isinstance(case.get("id"), str)
    }


def _sorted_attempts(entries: list[tuple[Path, dict[str, Any]]]) -> list[tuple[Path, dict[str, Any]]]:
    return sorted(
        entries,
        key=lambda item: (
            str(item[1].get("started_at") or ""),
            str(item[1].get("trial_name") or ""),
            str(item[0]),
        ),
    )


def _unique(values: list[Any]) -> list[Any]:
    return sorted({value for value in values if value is not None}, key=str)


def _ceiling(values: list[Any]) -> Any:
    present = [value for value in values if value is not None]
    return max(present) if present else None


def build_report(
    job_dir: Path,
    cases: dict[str, dict[str, Any]] | None = None,
    *,
    suite: str = SUITE_REGRESSION,
    require_judge: bool = False,
) -> dict[str, Any]:
    """Build the machine-readable promotion report for one Harbor job."""
    cases = cases or {}
    trial_paths = discover_trials(job_dir)
    if not trial_paths:
        raise SystemExit("no Harbor trial results found")

    parsed: list[tuple[Path, dict[str, Any], dict[str, Any]]] = []
    for path in trial_paths:
        result = read_json(path)
        trial_dir = path.parent
        lock_path = trial_dir / "lock.json"
        lock = read_json(lock_path) if lock_path.is_file() else {}
        parsed.append((path, result, lock))

    groups: dict[str, list[tuple[Path, dict[str, Any], dict[str, Any]]]] = {}
    for path, result, lock in parsed:
        groups.setdefault(task_case(result), []).append((path, result, lock))

    case_reports: dict[str, dict[str, Any]] = {}
    all_attempts: list[dict[str, Any]] = []
    models: list[str] = []
    providers: list[str] = []
    task_digests: list[str] = []
    skill_map: dict[str, str] = {}
    cpu_ceilings: list[Any] = []
    memory_ceilings: list[Any] = []
    storage_ceilings: list[Any] = []

    for case_id in sorted(groups):
        entries = _sorted_attempts(groups[case_id])
        attempts: list[dict[str, Any]] = []
        for index, (path, result, lock) in enumerate(entries):
            outcome = trial_outcome(result, require_judge)
            info = model_info(result)
            digest = task_digest(result, lock)
            resources = resource_profile(lock)
            if info.get("model") is not None:
                models.append(info["model"])
            if info.get("provider") is not None:
                providers.append(info["provider"])
            if digest is not None:
                task_digests.append(digest)
            for skill in skill_digests(lock):
                if skill.get("name") and skill.get("digest"):
                    skill_map[str(skill["name"])] = str(skill["digest"])
            cpu_ceilings.append(resources["cpu"]["ceiling"])
            memory_ceilings.append(resources["memory_mb"]["ceiling"])
            storage_ceilings.append(resources["storage_mb"]["ceiling"])

            attempt = {
                "attempt": index,
                "trial": result.get("id") or result.get("trial_name") or path.parent.name,
                "outcome": outcome["outcome"],
                "status": outcome["status"],
                "deterministic": outcome["deterministic"],
                "judge": outcome["judge"],
                "exception_type": outcome["exception_type"],
                "exception_class": outcome["exception_class"],
                "model": info,
                "task_digest": digest,
                "skill_digests": skill_digests(lock),
                "resources": resources,
                "timing": {
                    "started_at": result.get("started_at"),
                    "finished_at": result.get("finished_at"),
                    "duration_seconds": duration_seconds(result),
                },
                "tokens": token_totals(result),
                "is_regrade": trial_is_regrade(result, lock),
            }
            attempts.append(attempt)
            all_attempts.append(attempt)

        passed = sum(1 for attempt in attempts if attempt["outcome"] == "pass")
        total = len(attempts)
        infra_exceptions = sum(
            1 for attempt in attempts if attempt["outcome"] == "exception" and attempt["exception_class"] == "infra"
        )
        agent_exceptions = sum(
            1 for attempt in attempts if attempt["outcome"] == "exception" and attempt["exception_class"] == "agent"
        )
        case_info = cases.get(case_id) or {}
        case_reports[case_id] = {
            "suite": suite,
            "role": case_info.get("role"),
            "summary": case_info.get("summary"),
            "attempts": attempts,
            "total_attempts": total,
            "passed_attempts": passed,
            "pass_rate": round(passed / total, 4) if total else None,
            "wilson": wilson_interval(passed, total),
            "pass_at_1": bool(attempts) and attempts[0]["outcome"] == "pass",
            "pass_cubed": total >= 3 and all(attempt["outcome"] == "pass" for attempt in attempts[:3]),
            "saturated": total > 0 and passed == total,
            "infra_exceptions": infra_exceptions,
            "agent_exceptions": agent_exceptions,
        }

    total_trials = len(all_attempts)
    exception_trials = sum(1 for attempt in all_attempts if attempt["outcome"] == "exception")
    failed_trials = sum(1 for attempt in all_attempts if attempt["outcome"] not in {"pass", "exception"})
    passed_trials = sum(1 for attempt in all_attempts if attempt["outcome"] == "pass")
    infra_exceptions = sum(1 for attempt in all_attempts if attempt["exception_class"] == "infra")
    agent_exceptions = sum(1 for attempt in all_attempts if attempt["exception_class"] == "agent")
    case_count = len(case_reports)
    pass_at_1_cases = sum(1 for case in case_reports.values() if case["pass_at_1"])
    pass_cubed_cases = sum(1 for case in case_reports.values() if case["pass_cubed"])
    saturated_cases = [case_id for case_id, case in case_reports.items() if case["saturated"]]
    status_counts: dict[str, int] = {}
    for attempt in all_attempts:
        status = str(attempt["status"])
        status_counts[status] = status_counts.get(status, 0) + 1
    known_costs = [
        float(attempt["tokens"]["cost_usd"])
        for attempt in all_attempts
        if attempt["tokens"]["cost_usd"] is not None
    ]
    known_durations = [
        float(attempt["timing"]["duration_seconds"])
        for attempt in all_attempts
        if attempt["timing"]["duration_seconds"] is not None
    ]

    report = {
        "schema": REPORT_SCHEMA,
        "job": job_dir.name,
        "suite": suite,
        "require_judge": require_judge,
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "is_regrade": total_trials > 0 and all(attempt["is_regrade"] for attempt in all_attempts),
        "totals": {
            "cases": case_count,
            "trials": total_trials,
            "passed_trials": passed_trials,
            "failed_trials": failed_trials,
            "exception_trials": exception_trials,
            "infra_exceptions": infra_exceptions,
            "agent_exceptions": agent_exceptions,
            "status_counts": dict(sorted(status_counts.items())),
            "pass_at_1_cases": pass_at_1_cases,
            "pass_at_1_rate": round(pass_at_1_cases / case_count, 4) if case_count else None,
            "pass_at_1_wilson": wilson_interval(pass_at_1_cases, case_count),
            "pass_cubed_cases": pass_cubed_cases,
            "saturated_cases": saturated_cases,
            "cost_usd": round(sum(known_costs), 8) if known_costs else None,
            "mean_duration_seconds": (
                round(sum(known_durations) / len(known_durations), 3)
                if known_durations
                else None
            ),
            "environment": {
                "concurrency": job_concurrency(job_dir),
                "models": _unique(models),
                "providers": _unique(providers),
                "task_digests": _unique(task_digests),
                "skill_digests": skill_map,
                "resources": {
                    "cpu": {"floor": None, "ceiling": _ceiling(cpu_ceilings)},
                    "memory_mb": {"floor": None, "ceiling": _ceiling(memory_ceilings)},
                    "storage_mb": {"floor": None, "ceiling": _ceiling(storage_ceilings)},
                },
            },
        },
        "cases": case_reports,
    }
    return report


def resource_signature(report: dict[str, Any]) -> dict[str, Any]:
    env = report["totals"]["environment"]
    resources = env["resources"]
    return {
        "concurrency": env.get("concurrency"),
        "cpu_ceiling": resources["cpu"]["ceiling"],
        "memory_ceiling": resources["memory_mb"]["ceiling"],
        "storage_ceiling": resources["storage_mb"]["ceiling"],
    }


def compare_reports(current: dict[str, Any], baseline: dict[str, Any]) -> dict[str, Any]:
    """Compare paired quality and efficiency under identical infra ceilings."""
    current_totals = current["totals"]
    baseline_totals = baseline["totals"]
    current_rate = current_totals["pass_at_1_rate"]
    baseline_rate = baseline_totals["pass_at_1_rate"]
    current_cost = current_totals.get("cost_usd")
    baseline_cost = baseline_totals.get("cost_usd")
    current_latency = current_totals.get("mean_duration_seconds")
    baseline_latency = baseline_totals.get("mean_duration_seconds")
    infra_matched = resource_signature(current) == resource_signature(baseline)
    delta_points = (
        round((current_rate - baseline_rate) * 100.0, 2)
        if current_rate is not None and baseline_rate is not None
        else None
    )
    cost_ratio = (
        round(current_cost / baseline_cost, 4)
        if current_cost is not None and baseline_cost not in {None, 0}
        else None
    )
    latency_ratio = (
        round(current_latency / baseline_latency, 4)
        if current_latency is not None and baseline_latency not in {None, 0}
        else None
    )
    efficiency_gain = (
        (cost_ratio is not None and cost_ratio <= 0.9)
        or (latency_ratio is not None and latency_ratio <= 0.9)
    )
    if delta_points is None or not infra_matched:
        verdict = "inconclusive"
    elif delta_points < 0:
        verdict = "regression"
    elif delta_points == 0 and efficiency_gain:
        verdict = "efficiency-win"
    elif delta_points <= 0:
        verdict = "no-improvement"
    else:
        current_ci = current_totals["pass_at_1_wilson"]
        baseline_ci = baseline_totals["pass_at_1_wilson"]
        overlap = current_ci[0] <= baseline_ci[1] and baseline_ci[0] <= current_ci[1]
        verdict = "win" if delta_points >= 3.0 and not overlap else "inconclusive"
    return {
        "current_job": current["job"],
        "baseline_job": baseline["job"],
        "current_pass_at_1_rate": current_rate,
        "baseline_pass_at_1_rate": baseline_rate,
        "delta_points": delta_points,
        "cost_ratio": cost_ratio,
        "latency_ratio": latency_ratio,
        "infra_matched": infra_matched,
        "verdict": verdict,
    }


def evaluate_gate(
    report: dict[str, Any],
    *,
    change_class: str | None = None,
    min_attempts: int | None = None,
    baseline_report: dict[str, Any] | None = None,
    adr: str = "",
    expected_cases: set[str] | None = None,
) -> list[str]:
    """Return every reason the promotion gate does not pass."""
    failures: list[str] = []
    policy = CHANGE_CLASS_POLICY.get(change_class) if change_class else None
    if change_class is not None and policy is None:
        failures.append(f"unknown change class: {change_class}")

    if expected_cases is not None:
        failures.extend(case_coverage_failures(report, expected_cases))
        if baseline_report is not None:
            failures.extend(case_coverage_failures(baseline_report, expected_cases))

    if report["suite"] == SUITE_REGRESSION:
        for case_id, case in report["cases"].items():
            for attempt in case["attempts"]:
                if attempt["outcome"] == "exception":
                    failures.append(
                        f"{case_id}: attempt {attempt['attempt']} raised "
                        f"{attempt['exception_type']} ({attempt['exception_class']})"
                    )
                elif attempt["outcome"] != "pass":
                    failures.append(
                        f"{case_id}: attempt {attempt['attempt']} outcome {attempt['outcome']}"
                    )
            if min_attempts is not None and case["total_attempts"] < min_attempts:
                failures.append(
                    f"{case_id}: has {case['total_attempts']} attempts, "
                    f"fewer than required {min_attempts}"
                )

    if change_class is not None:
        if policy.requires_judge and not report["require_judge"]:
            failures.append(f"change class {change_class} requires --require-judge")
        if policy.requires_regrade and not report["is_regrade"]:
            failures.append(
                f"change class {change_class} requires a regrade run "
                "(every trial must carry source_trial)"
            )
        if policy.requires_adr and not adr.strip():
            failures.append(f"change class {change_class} requires an ADR (--adr)")

    if baseline_report is not None and change_class in MODEL_CHANGE_CLASSES:
        comparison = report.get("comparison")
        if comparison is None:
            failures.append("model change has a baseline but no comparison was produced")
        elif comparison["verdict"] not in {"win", "efficiency-win"}:
            failures.append(
                f"model change is not a supported win: {comparison['verdict']} "
                f"(delta {comparison['delta_points']} points, "
                f"infra_matched={comparison['infra_matched']})"
            )

    return failures


def render_markdown(report: dict[str, Any]) -> str:
    """Render the report as a compact human-readable Markdown document."""
    totals = report["totals"]
    lines = [
        f"# Forest eval report: {report['job']}",
        "",
        f"- suite: `{report['suite']}`",
        f"- judge required: `{report['require_judge']}`",
        f"- regrade run: `{report['is_regrade']}`",
        f"- generated: `{report['generated_at']}`",
        "",
        "## Totals",
        "",
        f"- cases: {totals['cases']}",
        f"- trials: {totals['trials']}",
        f"- passed: {totals['passed_trials']}",
        f"- failed: {totals['failed_trials']}",
        f"- exceptions: {totals['exception_trials']} "
        f"(infra {totals['infra_exceptions']}, agent {totals['agent_exceptions']})",
        f"- pass@1 cases: {totals['pass_at_1_cases']}/{totals['cases']} "
        f"({totals['pass_at_1_rate']}) Wilson {totals['pass_at_1_wilson']}",
        f"- pass^3 cases: {totals['pass_cubed_cases']}",
        f"- saturated cases: {', '.join(totals['saturated_cases']) or 'none'}",
        "",
        "## Environment",
        "",
        f"- concurrency: {totals['environment']['concurrency']}",
        f"- models: {', '.join(str(m) for m in totals['environment']['models']) or 'unknown'}",
        f"- providers: {', '.join(str(p) for p in totals['environment']['providers']) or 'unknown'}",
        f"- task digests: {', '.join(totals['environment']['task_digests']) or 'none'}",
        "- resources: "
        f"cpu ceiling {totals['environment']['resources']['cpu']['ceiling']}, "
        f"memory ceiling {totals['environment']['resources']['memory_mb']['ceiling']} MB, "
        f"storage ceiling {totals['environment']['resources']['storage_mb']['ceiling']} MB",
        "",
        "## Cases",
        "",
        "| case | role | attempts | pass | pass@1 | pass^3 | rate | Wilson | infra | agent | saturated |",
        "| --- | --- | ---: | ---: | --- | --- | ---: | --- | ---: | ---: | --- |",
    ]
    for case_id, case in report["cases"].items():
        lines.append(
            f"| {case_id} | {case['role'] or '-'} | {case['total_attempts']} "
            f"| {case['passed_attempts']} | {'yes' if case['pass_at_1'] else 'no'} "
            f"| {'yes' if case['pass_cubed'] else 'no'} | {case['pass_rate']} "
            f"| {case['wilson']} | {case['infra_exceptions']} | {case['agent_exceptions']} "
            f"| {'yes' if case['saturated'] else 'no'} |"
        )
    comparison = report.get("comparison")
    if comparison:
        lines.extend(
            [
                "",
                "## Comparison",
                "",
                f"- baseline: `{comparison['baseline_job']}`",
                f"- pass@1 delta: {comparison['delta_points']} points",
                f"- infra matched: `{comparison['infra_matched']}`",
                f"- verdict: `{comparison['verdict']}`",
            ]
        )
    return "\n".join(lines) + "\n"


def write_report(report: dict[str, Any], report_dir: Path) -> tuple[Path, Path]:
    report_dir.mkdir(parents=True, exist_ok=True)
    json_path = report_dir / REPORT_JSON
    md_path = report_dir / REPORT_MD
    json_path.write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    md_path.write_text(render_markdown(report))
    return json_path, md_path


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("job_dir", type=Path, help="Path to one completed Harbor job directory")
    parser.add_argument(
        "--cases",
        type=Path,
        default=DEFAULT_CASES,
        help="Path to the cases.json task manifest",
    )
    parser.add_argument(
        "--suite",
        choices=sorted(SUITES),
        default=SUITE_REGRESSION,
        help="Suite policy: regression is all-pass/pass^3; capability reports pass@1",
    )
    parser.add_argument(
        "--change-class",
        choices=sorted(CHANGE_CLASSES),
        default=None,
        help="Promotion change class whose gate must pass",
    )
    parser.add_argument(
        "--require-judge",
        action="store_true",
        help="Require a judge reward of 1 on every trial",
    )
    parser.add_argument(
        "--min-attempts",
        type=int,
        default=None,
        help="Require at least this many attempts per regression case",
    )
    parser.add_argument(
        "--baseline",
        type=Path,
        default=None,
        help="Path to a baseline Harbor job directory to compare against",
    )
    parser.add_argument(
        "--adr",
        default="",
        help="ADR reference required for tool/kernel boundary changes",
    )
    parser.add_argument(
        "--expected-cases",
        type=parse_expected_cases,
        default=None,
        help="JSON array of planned case ids the produced report must cover exactly",
    )
    parser.add_argument(
        "--report-dir",
        type=Path,
        default=None,
        help="Directory for report.json and report.md (defaults to the job dir)",
    )
    args = parser.parse_args(argv)

    if not args.job_dir.is_dir():
        print(f"assert_results: not a directory: {args.job_dir}", file=sys.stderr)
        return 2

    cases = load_cases(args.cases)
    report = build_report(
        args.job_dir,
        cases,
        suite=args.suite,
        require_judge=args.require_judge,
    )

    baseline_report = None
    if args.baseline is not None:
        if not args.baseline.is_dir():
            print(f"assert_results: not a directory: {args.baseline}", file=sys.stderr)
            return 2
        baseline_report = build_report(
            args.baseline,
            cases,
            suite=args.suite,
            require_judge=args.require_judge,
        )
        report["comparison"] = compare_reports(report, baseline_report)

    failures = evaluate_gate(
        report,
        change_class=args.change_class,
        min_attempts=args.min_attempts,
        baseline_report=baseline_report,
        adr=args.adr,
        expected_cases=args.expected_cases,
    )

    report_dir = args.report_dir or args.job_dir
    json_path, md_path = write_report(report, report_dir)

    if failures:
        print(
            "Harbor report failed:\n" + "\n".join(f"- {failure}" for failure in failures),
            file=sys.stderr,
        )
        print(f"report: {json_path}", file=sys.stderr)
        print(f"report: {md_path}", file=sys.stderr)
        return 2

    print(
        f"validated {report['totals']['trials']} Harbor trials across "
        f"{report['totals']['cases']} cases; pass@1 {report['totals']['pass_at_1_rate']}; "
        f"pass^3 {report['totals']['pass_cubed_cases']}"
    )
    print(f"report: {json_path}")
    print(f"report: {md_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
