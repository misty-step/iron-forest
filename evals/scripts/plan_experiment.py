#!/usr/bin/env python3
"""Plan one bounded incumbent-versus-contender Forest experiment."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request
from datetime import date
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[2]
EVALS = ROOT / "evals"
SPACE_PATH = EVALS / "experiment-space.json"
CASES_PATH = EVALS / "cases.json"
PLANNER_URL = "https://openrouter.ai/api/v1/chat/completions"
REGRESSION_ROLES = {"builder", "verifier", "fixer"}


def read_json(path: Path) -> dict[str, Any]:
    value = json.loads(path.read_text())
    if not isinstance(value, dict):
        raise ValueError(f"expected object: {path}")
    return value


def canonical_digest(value: Any) -> str:
    payload = json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(payload).hexdigest()


def file_digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def tree_digest(path: Path) -> str:
    files = sorted(
        file
        for file in path.rglob("*")
        if file.is_file()
        and "__pycache__" not in file.parts
        and file.suffix not in {".pyc", ".pyo"}
    )
    return canonical_digest(
        {str(file.relative_to(path)): file_digest(file) for file in files}
    )


def parse_declaration(role: str) -> dict[str, Any]:
    path = ROOT / "agents" / role / "agent.md"
    text = path.read_text()
    lines = text.splitlines()
    if not lines or lines[0] != "---":
        raise ValueError(f"agent declaration has no frontmatter: {path}")
    values: dict[str, str] = {}
    closing = None
    for index, line in enumerate(lines[1:], 1):
        if line == "---":
            closing = index
            break
        name, separator, value = line.partition(":")
        if separator:
            values[name.strip()] = value.strip()
    if closing is None:
        raise ValueError(f"agent declaration has unclosed frontmatter: {path}")
    return {
        "role": role,
        "model": values.get("model"),
        "thinking": values.get("thinking"),
        "tools": values.get("tools"),
        "prompt_digest": canonical_digest("\n".join(lines[closing + 1 :])),
        "declaration_digest": file_digest(path),
    }


def effective_configuration(role: str, variant: dict[str, Any] | None) -> dict[str, Any]:
    configuration = parse_declaration(role)
    if variant is None:
        configuration["variant"] = "production"
        configuration["prompt_append_digest"] = None
    else:
        configuration["variant"] = variant["id"]
        for field in ("model", "thinking", "tools"):
            if variant.get(field) is not None:
                configuration[field] = variant[field]
        appended = variant.get("prompt_append") or ""
        configuration["prompt_append_digest"] = canonical_digest(appended) if appended else None
    role_skills = ROOT / "agents" / role / "skills"
    shared_skills = ROOT / "agents" / "_shared" / "skills"
    configuration["skill_digest"] = canonical_digest(
        {
            "role": tree_digest(role_skills) if role_skills.is_dir() else None,
            "shared": tree_digest(shared_skills) if shared_skills.is_dir() else None,
        }
    )
    configuration["configuration_fingerprint"] = canonical_digest(configuration)
    return configuration


def evaluator_digest() -> str:
    files = [
        CASES_PATH,
        EVALS / "experiment-space.json",
        EVALS / "run-experiment.sh",
        EVALS / "scripts" / "assert_results.py",
        EVALS / "image" / "Dockerfile",
    ]
    trees = [
        EVALS / "runtime",
        EVALS / "iron_forest_eval",
    ]
    payload = {str(path.relative_to(ROOT)): file_digest(path) for path in files}
    payload.update({str(path.relative_to(ROOT)): tree_digest(path) for path in trees})
    return canonical_digest(payload)

def load_history(path: Path | None) -> dict[str, Any]:
    if path is None or not path.exists():
        return {"records": []}
    history = read_json(path)
    records = history.get("records")
    if not isinstance(records, list):
        raise ValueError(f"history has no records list: {path}")
    return history


def completed_fingerprints(history: dict[str, Any]) -> set[str]:
    return {
        str(record["experiment_fingerprint"])
        for record in history.get("records", [])
        if isinstance(record, dict)
        and record.get("experiment_fingerprint")
        and set(record.get("cohorts") or []) == {"incumbent", "contender"}
    }


def candidate_cases(
    cases: list[dict[str, Any]],
    variant: dict[str, Any],
    tier: str,
    limit: int,
    requested_role: str | None,
    sentinels: dict[str, str],
) -> list[dict[str, Any]]:
    by_id = {str(case["id"]): case for case in cases}
    supported_roles = set(variant["roles"])
    if requested_role == "shared":
        required_roles = REGRESSION_ROLES if tier == "promotion" else set(sentinels)
        if not required_roles.issubset(supported_roles):
            raise ValueError(f"variant {variant['id']} does not support required sentinel roles")
        if tier == "promotion":
            return [
                case
                for case in cases
                if case.get("role") in supported_roles & REGRESSION_ROLES
            ][:limit]
        return [
            by_id[case_id]
            for role, case_id in sentinels.items()
            if role in supported_roles and case_id in by_id
        ][:limit]
    if tier == "promotion" and requested_role is not None and requested_role not in REGRESSION_ROLES:
        raise ValueError(f"promotion has no regression cases for role {requested_role}")
    if requested_role is not None and requested_role not in supported_roles:
        raise ValueError(f"variant {variant['id']} does not support role {requested_role}")
    if requested_role:
        selected = [case for case in cases if case.get("role") == requested_role]
        if tier == "promotion":
            selected_ids = {str(case["id"]) for case in selected}
            selected.extend(
                by_id[case_id]
                for role, case_id in sentinels.items()
                if role in REGRESSION_ROLES
                and role != requested_role
                and case_id in by_id
                and case_id not in selected_ids
            )
        return selected[:limit]
    eligible_roles = (
        set(variant.get("apply_roles") or variant["roles"])
        if tier == "nightly"
        else supported_roles
    )
    if tier in {"monthly", "promotion"}:
        eligible_roles &= REGRESSION_ROLES
    eligible = [case for case in cases if case.get("role") in eligible_roles]
    if tier != "nightly" or len(eligible) <= limit:
        return eligible[:limit]
    roles = sorted({str(case["role"]) for case in eligible})
    role = roles[date.today().toordinal() % len(roles)]
    return [case for case in eligible if case.get("role") == role][:limit]


def plan_candidate(
    variant: dict[str, Any],
    cases: list[dict[str, Any]],
    tier_name: str,
    tier: dict[str, Any],
    evaluator: str,
    requested_role: str | None,
    sentinels: dict[str, str],
) -> dict[str, Any]:
    selected_cases = candidate_cases(
        cases,
        variant,
        tier_name,
        int(tier["case_limit"]),
        requested_role,
        sentinels,
    )
    roles = sorted({str(case["role"]) for case in selected_cases})
    apply_roles = set(variant.get("apply_roles") or variant["roles"])
    if requested_role not in {None, "shared"} and requested_role not in apply_roles:
        raise ValueError(f"variant {variant['id']} has no change for role {requested_role}")
    if not set(roles).intersection(apply_roles):
        raise ValueError(f"variant {variant['id']} does not change any selected role")
    contender_configs = [
        effective_configuration(role, variant if role in apply_roles else None)
        for role in roles
    ]
    incumbent_configs = [effective_configuration(role, None) for role in roles]
    for configuration in contender_configs + incumbent_configs:
        role_cases = [case for case in selected_cases if case.get("role") == configuration["role"]]
        configuration["task_digest"] = canonical_digest(role_cases)
        configuration["evaluator_digest"] = evaluator
        configuration["configuration_fingerprint"] = canonical_digest(
            {key: value for key, value in configuration.items() if key != "configuration_fingerprint"}
        )
    incumbent_models = {configuration["model"] for configuration in incumbent_configs}
    if len(incumbent_models) != 1:
        raise ValueError("one paired Harbor job requires one incumbent model across selected roles")
    if apply_roles != set(variant["roles"]):
        production_model = next(iter(incumbent_models))
        if variant["model"] != production_model:
            raise ValueError("role-scoped variants must keep the production model")
    attempts = int(tier["attempts"])
    total_trials = len(selected_cases) * attempts * 2
    estimated_cost = len(selected_cases) * attempts * (
        float(variant["estimated_max_usd_per_trial"])
        + float(read_json(SPACE_PATH)["incumbent"]["estimated_max_usd_per_trial"])
    )
    fingerprint_payload = {
        "variant": variant["id"],
        "cases": [case["id"] for case in selected_cases],
        "attempts": attempts,
        "contender_configurations": [item["configuration_fingerprint"] for item in contender_configs],
        "evaluator_digest": evaluator,
    }
    return {
        "variant": variant,
        "cases": [case["id"] for case in selected_cases],
        "roles": roles,
        "requested_role": requested_role,
        "attempts": attempts,
        "total_trials": total_trials,
        "estimated_max_cost_usd": round(estimated_cost, 4),
        "incumbent_configurations": incumbent_configs,
        "contender_configurations": contender_configs,
        "experiment_fingerprint": canonical_digest(fingerprint_payload),
    }


def validate_budget(candidate: dict[str, Any], tier: dict[str, Any]) -> None:
    if not candidate["cases"]:
        raise ValueError("contender has no eligible cases")
    if candidate["total_trials"] > int(tier["max_trials"]):
        raise ValueError("experiment exceeds trial budget")
    if candidate["estimated_max_cost_usd"] > float(tier["max_estimated_cost_usd"]):
        raise ValueError("experiment exceeds estimated cost budget")


def agentic_choice(candidates: list[dict[str, Any]], history: dict[str, Any], model: str) -> tuple[str, str]:
    api_key = (os.environ.get("OPENROUTER_API_KEY") or "").strip()
    if not api_key:
        raise RuntimeError("OPENROUTER_API_KEY is unavailable")
    compact = [
        {
            "id": candidate["variant"]["id"],
            "hypothesis": candidate["variant"]["hypothesis"],
            "roles": candidate["roles"],
            "cases": candidate["cases"],
            "estimated_max_cost_usd": candidate["estimated_max_cost_usd"],
        }
        for candidate in candidates
    ]
    prior = [
        {
            "variant": record.get("variant"),
            "tier": record.get("tier"),
            "trials": record.get("trials"),
            "pass_rate": record.get("pass_rate"),
            "cost_usd": record.get("cost_usd"),
            "mean_duration_seconds": record.get("mean_duration_seconds"),
        }
        for record in history.get("records", [])[-20:]
    ]
    payload = {
        "model": model.removeprefix("openrouter/"),
        "messages": [
            {
                "role": "system",
                "content": "Select exactly one allowed Iron Forest contender. Optimize expected quality gain per dollar and information gain. Return only JSON with variant_id and reason. Never invent an id.",
            },
            {
                "role": "user",
                "content": json.dumps({"candidates": compact, "history": prior}, sort_keys=True),
            },
        ],
        "response_format": {"type": "json_object"},
        "temperature": 0.2,
        "max_tokens": 300,
    }
    request = urllib.request.Request(
        PLANNER_URL,
        data=json.dumps(payload).encode(),
        headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            result = json.load(response)
    except (OSError, urllib.error.HTTPError, json.JSONDecodeError) as error:
        raise RuntimeError(f"planner request failed: {type(error).__name__}") from error
    try:
        content = result["choices"][0]["message"]["content"]
        choice = json.loads(content)
        variant_id = str(choice["variant_id"])
        reason = str(choice["reason"])
    except (KeyError, IndexError, TypeError, json.JSONDecodeError) as error:
        raise RuntimeError("planner returned invalid JSON") from error
    if variant_id not in {candidate["variant"]["id"] for candidate in candidates}:
        raise RuntimeError("planner selected a non-allowlisted variant")
    return variant_id, reason


def git_revision() -> str:
    supplied = (os.environ.get("GITHUB_SHA") or "").strip()
    if supplied:
        return supplied
    return subprocess.run(
        ["git", "rev-parse", "HEAD"], cwd=ROOT, check=True, capture_output=True, text=True
    ).stdout.strip()


def build_plan(
    tier_name: str,
    history: dict[str, Any],
    requested_variant: str | None,
    use_agentic_planner: bool,
    requested_role: str | None = None,
) -> dict[str, Any]:
    space = read_json(SPACE_PATH)
    if space.get("schema") != "forest.eval.experiment-space.v1":
        raise ValueError("unsupported experiment-space schema")
    tier = dict(space["tiers"][tier_name])
    cases = read_json(CASES_PATH)["cases"]
    evaluator = evaluator_digest()
    variants = [variant for variant in space["variants"] if requested_variant in {None, variant["id"]}]
    if requested_variant and not variants:
        raise ValueError(f"unknown contender variant: {requested_variant}")

    seen = completed_fingerprints(history)
    candidates: list[dict[str, Any]] = []
    variant_errors: list[str] = []
    for variant in variants:
        try:
            candidate = plan_candidate(
                variant,
                cases,
                tier_name,
                tier,
                evaluator,
                requested_role,
                space["sentinels"],
            )
            validate_budget(candidate, tier)
        except ValueError as error:
            if requested_variant:
                raise
            variant_errors.append(f"{variant['id']}: {error}")
            continue
        if candidate["experiment_fingerprint"] not in seen:
            candidates.append(candidate)
    if not candidates:
        detail = "; ".join(variant_errors)
        suffix = f": {detail}" if detail else ""
        raise ValueError(f"no unique contender remains for this tier and evaluator revision{suffix}")

    planner = "manual" if requested_variant else "deterministic-fallback"
    reason = "Selected the least-tested allowlisted contender with a unique fingerprint."
    variant_counts = (history.get("summary") or {}).get("variant_experiments") or {}
    selected = min(
        candidates,
        key=lambda candidate: (
            int(variant_counts.get(candidate["variant"]["id"], 0)),
            candidate["variant"]["id"],
        ),
    )
    if requested_variant is None and use_agentic_planner:
        try:
            variant_id, reason = agentic_choice(candidates, history, space["planner_model"])
            selected = next(candidate for candidate in candidates if candidate["variant"]["id"] == variant_id)
            planner = "agentic"
        except RuntimeError as error:
            reason = f"{reason} Agentic planner fallback: {error}."

    return {
        "schema": "forest.eval.experiment-plan.v1",
        "tier": tier_name,
        "source_revision": git_revision(),
        "planner": planner,
        "selection_reason": reason,
        "budgets": tier,
        "evaluator_digest": evaluator,
        **selected,
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tier", choices=("nightly", "weekly", "monthly", "promotion", "manual"), required=True)
    parser.add_argument("--history", type=Path)
    parser.add_argument("--variant")
    parser.add_argument("--role", choices=("builder", "verifier", "fixer", "critic", "tester", "shared"))
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--no-agentic-planner", action="store_true")
    args = parser.parse_args(argv)
    try:
        plan = build_plan(
            args.tier,
            load_history(args.history),
            args.variant,
            not args.no_agentic_planner,
            args.role,
        )
    except (KeyError, OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"experiment plan rejected: {error}", file=sys.stderr)
        return 2
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(plan, indent=2, sort_keys=True) + "\n")
    print(json.dumps({"fingerprint": plan["experiment_fingerprint"], "planner": plan["planner"], "variant": plan["variant"]["id"]}, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
