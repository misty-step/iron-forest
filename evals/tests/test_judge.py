from __future__ import annotations

import os
import unittest
from unittest.mock import patch

from runtime.judge import sanitize_trace


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


if __name__ == "__main__":
    unittest.main()
