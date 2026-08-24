from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

from calibrate_judge import calibrate, confusion_matrix, dimension_agreement, judge_fingerprint_matches  # noqa: E402


def bank(trials: list[dict]) -> dict:
    return {
        "schema": "forest.eval.calibration.v1",
        "judge": {
            "version": "judge-v2",
            "model": "judge/model",
            "prompt_sha256": "a" * 64,
        },
        "trials": trials,
    }


def predictions(trials: list[dict]) -> dict:
    return {
        "schema": "forest.eval.judge-predictions.v1",
        "judge": {
            "version": "judge-v2",
            "model": "judge/model",
            "prompt_sha256": "a" * 64,
        },
        "trials": trials,
    }


def trial(case: str, label: str, correctness=True, evidence=True, scope=True) -> dict:
    return {
        "case": case,
        "role": "builder",
        "label": label,
        "dimensions": {"correctness": correctness, "evidence": evidence, "scope": scope},
    }


class ConfusionMatrixTest(unittest.TestCase):
    def test_counts(self):
        labels = {
            "a": {"label": "fail"},
            "b": {"label": "fail"},
            "c": {"label": "pass"},
            "d": {"label": "pass"},
        }
        predicted = {
            "a": {"pass": False},
            "b": {"pass": True},
            "c": {"pass": False},
            "d": {"pass": True},
        }
        matrix = confusion_matrix(labels, predicted)
        self.assertEqual(matrix["true_positive"], 1)
        self.assertEqual(matrix["false_negative"], 1)
        self.assertEqual(matrix["false_positive"], 1)
        self.assertEqual(matrix["true_negative"], 1)
        self.assertEqual(matrix["total"], 4)


class DimensionAgreementTest(unittest.TestCase):
    def test_agreement_and_unknown(self):
        labels = {
            "a": {"label": "pass", "dimensions": {"correctness": True, "evidence": True, "scope": True}},
            "b": {"label": "fail", "dimensions": {"correctness": False, "evidence": False, "scope": False}},
        }
        predicted = {
            "a": {"pass": True, "dimensions": {"correctness": True, "evidence": False, "scope": None}},
            "b": {"pass": False, "dimensions": {"correctness": False, "evidence": False, "scope": False}},
        }
        agreement = dimension_agreement(labels, predicted)
        self.assertEqual(agreement["correctness"]["matched"], 2)
        self.assertEqual(agreement["evidence"]["matched"], 1)
        self.assertEqual(agreement["evidence"]["known"], 2)
        self.assertEqual(agreement["scope"]["known"], 1)
        self.assertEqual(agreement["scope"]["unknown"], 1)


class CalibrateTest(unittest.TestCase):
    def test_report_zero_false_negatives_and_full_agreement(self):
        result = calibrate(
            bank(
                [
                    trial("a", "pass"),
                    trial("b", "fail", correctness=False, evidence=False, scope=False),
                ]
            ),
            predictions(
                [
                    {
                        "case": "a",
                        "pass": True,
                        "dimensions": {"correctness": True, "evidence": True, "scope": True},
                    },
                    {
                        "case": "b",
                        "pass": False,
                        "dimensions": {"correctness": False, "evidence": False, "scope": False},
                    },
                ]
            ),
        )
        self.assertEqual(result["confusion_matrix"]["false_negative"], 0)
        self.assertEqual(result["false_negative_rate"], 0.0)
        self.assertEqual(result["overall_accuracy"], 1.0)
        self.assertFalse(result["regrade_required"])
        self.assertTrue(result["judge_fingerprint_matched"])

    def test_fingerprint_mismatch_requires_regrade(self):
        labels = bank([trial("a", "pass")])
        observed = predictions([{"case": "a", "pass": True, "dimensions": {}}])
        observed["judge"]["prompt_sha256"] = "b" * 64
        result = calibrate(labels, observed)
        self.assertTrue(result["regrade_required"])
        self.assertFalse(result["judge_fingerprint_matched"])

    def test_missing_fingerprint_is_unknown(self):
        labels = bank([trial("a", "pass")])
        labels["judge"] = {}
        observed = predictions([{"case": "a", "pass": True, "dimensions": {}}])
        observed["judge"] = {}
        result = calibrate(labels, observed)
        self.assertIsNone(result["judge_fingerprint_matched"])
        self.assertFalse(result["regrade_required"])


if __name__ == "__main__":
    unittest.main()
