from __future__ import annotations

import json
import os
import sys
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "runtime"))

from runtime import judge  # noqa: E402
from runtime.judge import (  # noqa: E402
    aggregate_dimensions,
    assistant_errors,
    judge_environment,
    parse_dimension,
    parse_forensic,
    prompt_fingerprint,
    sanitize_trace,
)


class JudgeTraceTest(unittest.TestCase):
    def test_sanitize_trace_redacts_provider_credentials(self):
        value = "sensitive-value-for-test"
        with patch.dict(os.environ, {"OPENROUTER_API_KEY": value}, clear=False):
            sanitized = sanitize_trace(f"before {value} after")
        self.assertEqual(sanitized, "before <redacted:OPENROUTER_API_KEY> after")

    def test_sanitize_trace_preserves_non_secret_environment_values(self):
        with patch.dict(os.environ, {"FOREST_EVAL_JUDGE_MODEL": "provider/model"}, clear=False):
            sanitized = sanitize_trace("provider/model")
        self.assertEqual(sanitized, "provider/model")

    def test_judge_environment_replaces_candidate_credential(self):
        with patch.dict(
            os.environ,
            {
                "OPENROUTER_API_KEY": "candidate-sensitive-value",
                "FOREST_EVAL_JUDGE_API_KEY": "judge-sensitive-value",
            },
            clear=True,
        ):
            environment = judge_environment()
        self.assertEqual(environment["OPENROUTER_API_KEY"], "judge-sensitive-value")
        self.assertNotIn("FOREST_EVAL_JUDGE_API_KEY", environment)

    def test_assistant_errors_finds_terminal_provider_error(self):
        event = {
            "type": "agent_end",
            "messages": [{"role": "assistant", "errorMessage": "provider unavailable"}],
        }
        self.assertEqual(list(assistant_errors(event)), ["provider unavailable"])


class JudgeParsingTest(unittest.TestCase):
    def test_parse_correctness_dimension(self):
        text = json.dumps(
            {"score": True, "reason": "caught the defect", "false_findings": [], "missed_findings": []}
        )
        result = parse_dimension("correctness", text)
        self.assertTrue(result["score"])
        self.assertEqual(result["false_findings"], [])

    def test_parse_dimension_rejects_missing_finding_fields(self):
        text = json.dumps({"score": True, "reason": "ok"})
        with self.assertRaises(RuntimeError):
            parse_dimension("correctness", text)

    def test_parse_dimension_accepts_unknown(self):
        text = json.dumps({"score": None, "reason": "insufficient evidence"})
        result = parse_dimension("evidence", text)
        self.assertIsNone(result["score"])

    def test_parse_forensic(self):
        text = json.dumps(
            {"pass": True, "correctness": True, "evidence": True, "scope": True, "reason": "ok"}
        )
        result = parse_forensic(text)
        self.assertTrue(result["pass"])

    def test_parse_forensic_rejects_non_boolean_pass(self):
        text = json.dumps(
            {"pass": "yes", "correctness": True, "evidence": True, "scope": True, "reason": "ok"}
        )
        with self.assertRaises(RuntimeError):
            parse_forensic(text)

    def test_prompt_fingerprint_is_stable(self):
        self.assertEqual(prompt_fingerprint(), prompt_fingerprint())
        self.assertRegex(prompt_fingerprint(), r"^[0-9a-f]{64}$")


class JudgeAggregationTest(unittest.TestCase):
    def test_all_true_passes(self):
        dimensions = {
            "correctness": {"score": True},
            "evidence": {"score": True},
            "scope": {"score": True},
        }
        self.assertEqual(aggregate_dimensions(dimensions), {"pass": True, "state": "pass"})

    def test_any_false_fails(self):
        dimensions = {
            "correctness": {"score": False},
            "evidence": {"score": True},
            "scope": {"score": True},
        }
        self.assertEqual(aggregate_dimensions(dimensions), {"pass": False, "state": "fail"})

    def test_unknown_without_false_is_unknown(self):
        dimensions = {
            "correctness": {"score": None},
            "evidence": {"score": True},
            "scope": {"score": True},
        }
        self.assertEqual(aggregate_dimensions(dimensions), {"pass": None, "state": "unknown"})


def correctness_json(score):
    return json.dumps(
        {"score": score, "reason": "ok", "false_findings": [], "missed_findings": []}
    )


def dimension_json(score):
    return json.dumps({"score": score, "reason": "ok"})


def forensic_json(passed):
    return json.dumps(
        {"pass": passed, "correctness": passed, "evidence": passed, "scope": passed, "reason": "ok"}
    )


class JudgeEvaluateTest(unittest.TestCase):
    def run_evaluate(self, scenario, deterministic, side_effect):
        with (
            patch.dict(
                os.environ,
                {
                    "FOREST_EVAL_JUDGE_MODEL": "judge/model",
                    "FOREST_EVAL_JUDGE_API_KEY": "judge-key-value",
                },
                clear=False,
            ),
            patch.object(judge, "run_pi", side_effect=side_effect) as run,
        ):
            result = judge.evaluate(scenario, deterministic, "trace", "candidate/model")
        return result, run

    def test_deterministic_failure_is_never_overridden(self):
        result, run = self.run_evaluate({}, {"passed": False}, [])
        self.assertFalse(result["pass"])
        self.assertTrue(result["deterministic_override"])
        run.assert_not_called()

    def test_dimension_judges_pass_without_forensic(self):
        result, run = self.run_evaluate(
            {},
            {"passed": True},
            [correctness_json(True), dimension_json(True), dimension_json(True)],
        )
        self.assertTrue(result["pass"])
        self.assertNotIn("forensic", result)
        self.assertEqual(run.call_count, 3)

    def test_dimension_false_fails(self):
        result, run = self.run_evaluate(
            {},
            {"passed": True},
            [correctness_json(False), dimension_json(True), dimension_json(True)],
        )
        self.assertFalse(result["pass"])
        self.assertNotIn("forensic", result)
        self.assertEqual(run.call_count, 3)

    def test_unknown_escalates_to_forensic(self):
        result, run = self.run_evaluate(
            {},
            {"passed": True},
            [
                correctness_json(None),
                dimension_json(True),
                dimension_json(True),
                forensic_json(True),
            ],
        )
        self.assertTrue(result["pass"])
        self.assertIn("forensic", result)
        self.assertEqual(run.call_count, 4)

    def test_high_risk_always_escalates_to_forensic(self):
        result, run = self.run_evaluate(
            {"judge_high_risk": True},
            {"passed": True},
            [
                correctness_json(True),
                dimension_json(True),
                dimension_json(True),
                forensic_json(False),
            ],
        )
        self.assertFalse(result["pass"])
        self.assertIn("forensic", result)
        self.assertEqual(run.call_count, 4)


if __name__ == "__main__":
    unittest.main()
