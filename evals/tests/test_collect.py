from __future__ import annotations

import sys
import unittest
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "runtime"))

from runtime import collect as collect_module  # noqa: E402


class CollectorSanitizationTest(unittest.TestCase):
    def test_sensitive_environment_names_are_flagged(self):
        for name in (
            "OPENROUTER_API_KEY",
            "LANGFUSE_SECRET_KEY",
            "LANGFUSE_PUBLIC_KEY",
            "AUTH_TOKEN",
            "PASSWORD",
            "DB_PASSWORD",
        ):
            with self.subTest(name=name):
                self.assertTrue(collect_module.is_sensitive_env_name(name))

    def test_benign_environment_names_are_kept(self):
        for name in ("HOME", "PATH", "FOREST_EVAL_REQUIRE_JUDGE", "LANG"):
            with self.subTest(name=name):
                self.assertFalse(collect_module.is_sensitive_env_name(name))

    def test_sanitize_environment_redacts_sensitive_values(self):
        with patch.dict(
            "os.environ",
            {
                "OPENROUTER_API_KEY": "sk-openrouter-secret",
                "LANGFUSE_SECRET_KEY": "sk-lf-secret",
                "FOREST_EVAL_JUDGE_MODEL": "openrouter/model",
            },
            clear=True,
        ):
            sanitized = collect_module.sanitize_environment()
        self.assertEqual(sanitized["OPENROUTER_API_KEY"], "<redacted:OPENROUTER_API_KEY>")
        self.assertEqual(sanitized["LANGFUSE_SECRET_KEY"], "<redacted:LANGFUSE_SECRET_KEY>")
        self.assertEqual(sanitized["FOREST_EVAL_JUDGE_MODEL"], "openrouter/model")


if __name__ == "__main__":
    unittest.main()
