#!/usr/bin/env python3
"""Calibrate Iron Forest judge predictions against a labeled bank.

The bank is `evals/calibration.json` (schema ``forest.eval.calibration.v1``).
Predictions are emitted as ``forest.eval.judge-predictions.v1``; operators
collect them from the `judge` object written to each trial's `details.json`.
The report records a confusion matrix, per-dimension agreement, and whether the
judge fingerprint (version/model/prompt) matches the bank. A fingerprint
mismatch sets `regrade_required` so a changed prompt or model cannot quietly
reuse stale human labels.
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path

CALIBRATION_SCHEMA = "forest.eval.calibration.v1"
PREDICTIONS_SCHEMA = "forest.eval.judge-predictions.v1"
REPORT_SCHEMA = "forest.eval.calibration-report.v1"

DIMENSIONS = ("correctness", "evidence", "scope")


def load_json(path: Path, schema: str) -> dict:
    try:
        value = json.loads(path.read_text(errors="replace"))
    except (OSError, json.JSONDecodeError) as error:
        raise SystemExit(f"cannot read {path}: {error}")
    if not isinstance(value, dict):
        raise SystemExit(f"{path} is not a JSON object")
    if value.get("schema") != schema:
        raise SystemExit(f"{path} has unknown schema (expected {schema})")
    return value


def confusion_matrix(labels: dict[str, dict], predictions: dict[str, dict]) -> dict:
    """Count pass/fail predictions against labels.

    A positive is a labeled failure (defect). ``true_positive`` is a correctly
    predicted failure and ``false_negative`` is a missed failure, so a zero
    false-negative rate is the safety bar for known blocking regressions.
    """
    matrix = {"true_positive": 0, "false_positive": 0, "true_negative": 0, "false_negative": 0}
    for case, label in labels.items():
        prediction = predictions.get(case)
        if prediction is None:
            continue
        label_pass = label.get("label") == "pass"
        prediction_pass = bool(prediction.get("pass"))
        if label_pass and prediction_pass:
            matrix["true_negative"] += 1
        elif label_pass and not prediction_pass:
            matrix["false_positive"] += 1
        elif not label_pass and prediction_pass:
            matrix["false_negative"] += 1
        else:
            matrix["true_positive"] += 1
    matrix["total"] = sum(matrix.values())
    return matrix


def false_negative_rate(matrix: dict) -> float | None:
    denominator = matrix["true_positive"] + matrix["false_negative"]
    if denominator == 0:
        return None
    return matrix["false_negative"] / denominator


def dimension_agreement(labels: dict[str, dict], predictions: dict[str, dict]) -> dict:
    agreement: dict[str, dict] = {}
    for dimension in DIMENSIONS:
        matched = 0
        known = 0
        unknown = 0
        for case, label in labels.items():
            prediction = predictions.get(case)
            if prediction is None:
                continue
            expected = label.get("dimensions", {}).get(dimension)
            observed = prediction.get("dimensions", {}).get(dimension)
            if expected is None or observed is None:
                unknown += 1
                continue
            known += 1
            if bool(expected) == bool(observed):
                matched += 1
        agreement[dimension] = {
            "matched": matched,
            "known": known,
            "unknown": unknown,
            "agreement": matched / known if known else None,
        }
    return agreement


def judge_fingerprint_matches(bank: dict, predictions: dict) -> bool | None:
    expected = bank.get("judge")
    observed = predictions.get("judge")
    if not isinstance(expected, dict) or not isinstance(observed, dict):
        return None
    if any(expected.get(key) is None or observed.get(key) is None for key in ("version", "model", "prompt_sha256")):
        return None
    return all(expected.get(key) == observed.get(key) for key in ("version", "model", "prompt_sha256"))


def calibrate(bank: dict, predictions: dict) -> dict:
    labels = {trial["case"]: trial for trial in bank.get("trials", [])}
    predicted = {trial["case"]: trial for trial in predictions.get("trials", [])}
    matrix = confusion_matrix(labels, predicted)
    matched = judge_fingerprint_matches(bank, predictions)
    accuracy = (matrix["true_positive"] + matrix["true_negative"]) / matrix["total"] if matrix["total"] else None
    return {
        "schema": REPORT_SCHEMA,
        "confusion_matrix": matrix,
        "false_negative_rate": false_negative_rate(matrix),
        "overall_accuracy": accuracy,
        "dimension_agreement": dimension_agreement(labels, predicted),
        "judge_fingerprint_matched": matched,
        "regrade_required": matched is False,
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--labels", required=True, type=Path, help="calibration bank JSON")
    parser.add_argument("--predictions", required=True, type=Path, help="judge predictions JSON")
    parser.add_argument("--output", "-o", type=Path, default=None, help="write report to this file")
    args = parser.parse_args(argv)

    bank = load_json(args.labels, CALIBRATION_SCHEMA)
    predictions = load_json(args.predictions, PREDICTIONS_SCHEMA)
    report = calibrate(bank, predictions)
    rendered = json.dumps(report, indent=2, sort_keys=True) + "\n"
    if args.output is not None:
        args.output.write_text(rendered)
    else:
        print(rendered, end="")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
