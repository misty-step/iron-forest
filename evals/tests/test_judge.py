from __future__ import annotations

import os
import unittest
from unittest.mock import patch

from runtime.judge import assistant_errors, judge_environment, sanitize_trace


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


if __name__ == "__main__":
    unittest.main()
