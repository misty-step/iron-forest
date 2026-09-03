from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))
sys.path.insert(0, str(ROOT))

import experiment_history  # noqa: E402
from langfuse_config import normalized_base_url  # noqa: E402
import plan_experiment  # noqa: E402
from iron_forest_eval.usage import usage_from_run_logs  # noqa: E402
import affected_role  # noqa: E402


class AffectedRoleTest(unittest.TestCase):
    def test_single_agent_surface_targets_its_role(self):
        self.assertEqual(
            affected_role.classify(["agents/verifier/agent.md", "agents/verifier/skills/review/SKILL.md"]),
            "verifier",
        )

    def test_kernel_or_cross_role_change_uses_shared_sentinels(self):
        self.assertEqual(affected_role.classify(["runner.go"]), "shared")
        self.assertEqual(
            affected_role.classify(["agents/builder/agent.md", "agents/verifier/agent.md"]),
            "shared",
        )


class LangfuseConfigurationTest(unittest.TestCase):
    def test_base_url_accepts_host_names_and_rejects_non_http_schemes(self):
        self.assertEqual(normalized_base_url("langfuse.example.test/"), "https://langfuse.example.test")
        self.assertEqual(
            normalized_base_url('"https://langfuse.example.test/"'),
            "https://langfuse.example.test",
        )
        self.assertEqual(normalized_base_url("http://localhost:3000/"), "http://localhost:3000")
        with self.assertRaisesRegex(ValueError, "HTTP"):
            normalized_base_url("ftp://langfuse.example.test")


class ExperimentDeadlineTest(unittest.TestCase):
    def test_runner_binds_every_tier_to_its_git_owned_wall_ceiling(self):
        space = json.loads((ROOT / "experiment-space.json").read_text())
        for tier, policy in space["tiers"].items():
            completed = subprocess.run(
                ["./evals/run-experiment.sh"],
                check=True,
                capture_output=True,
                cwd=ROOT.parent,
                text=True,
                env={
                    **os.environ,
                    "FOREST_EVAL_TIER": tier,
                    "FOREST_EVAL_PRINT_TIMEOUT": "1",
                },
            )
            self.assertEqual(int(completed.stdout.strip()), policy["timeout_minutes"])


class ExperimentPlannerTest(unittest.TestCase):
    def test_plan_pairs_identical_cases_within_budget(self):
        plan = plan_experiment.build_plan(
            "nightly",
            {"records": []},
            "qwen-3.7-high",
            False,
        )
        self.assertEqual(plan["variant"]["id"], "qwen-3.7-high")
        self.assertEqual(plan["total_trials"], len(plan["cases"]) * plan["attempts"] * 2)
        self.assertLessEqual(plan["total_trials"], plan["budgets"]["max_trials"])
        self.assertLessEqual(plan["estimated_max_cost_usd"], plan["budgets"]["max_estimated_cost_usd"])
        self.assertEqual(
            {item["role"] for item in plan["incumbent_configurations"]},
            {item["role"] for item in plan["contender_configurations"]},
        )

    def test_experiment_fingerprint_binds_production_source_digest(self):
        with mock.patch.object(
            plan_experiment, "production_source_digest", return_value="production-a"
        ):
            first = plan_experiment.build_plan(
                "nightly",
                {"records": []},
                "qwen-3.7-high",
                False,
            )
        with mock.patch.object(
            plan_experiment, "production_source_digest", return_value="production-b"
        ):
            second = plan_experiment.build_plan(
                "nightly",
                {"records": []},
                "qwen-3.7-high",
                False,
            )
        self.assertNotEqual(first["experiment_fingerprint"], second["experiment_fingerprint"])

    def test_production_source_digest_tracks_go_module_and_source(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "go.mod").write_text("module example\n\ngo 1.26.5\n")
            (root / "go.sum").write_text("")
            (root / ".mise.toml").write_text("[tools]\ngo = \"1.26.5\"\n")
            (root / "main.go").write_text("package main\n")
            first = plan_experiment.production_source_digest(root)
            (root / "main.go").write_text("package main\n\nfunc main() {}\n")
            second = plan_experiment.production_source_digest(root)
            self.assertNotEqual(first, second)

    def test_budget_rejects_trial_and_cost_overruns(self):
        tier = {"max_trials": 10, "max_estimated_cost_usd": 5.0}
        with self.assertRaisesRegex(ValueError, "trial budget"):
            plan_experiment.validate_budget(
                {"cases": ["case"], "total_trials": 11, "estimated_max_cost_usd": 1.0},
                tier,
            )
        with self.assertRaisesRegex(ValueError, "cost budget"):
            plan_experiment.validate_budget(
                {"cases": ["case"], "total_trials": 10, "estimated_max_cost_usd": 5.01},
                tier,
            )

    def test_duplicate_experiment_fingerprint_is_rejected(self):
        plan = plan_experiment.build_plan(
            "nightly",
            {"records": []},
            "qwen-3.7-high",
            False,
        )
        with self.assertRaisesRegex(ValueError, "no unique contender"):
            plan_experiment.build_plan(
                "nightly",
                {"records": [{"experiment_fingerprint": plan["experiment_fingerprint"], "cohorts": ["incumbent", "contender"]}]},
                "qwen-3.7-high",
                False,
            )

    def test_variant_fingerprint_covers_prompt_tools_and_thinking(self):
        base = {"id": "x", "model": "openrouter/example", "thinking": "low", "roles": ["verifier"]}
        fingerprints = {
            plan_experiment.effective_configuration("verifier", base)["configuration_fingerprint"],
            plan_experiment.effective_configuration("verifier", {**base, "thinking": "high"})["configuration_fingerprint"],
            plan_experiment.effective_configuration("verifier", {**base, "tools": "read,bash"})["configuration_fingerprint"],
            plan_experiment.effective_configuration("verifier", {**base, "prompt_append": "Be concise."})["configuration_fingerprint"],
        }
        self.assertEqual(len(fingerprints), 4)

    def test_tree_digest_ignores_python_bytecode(self):
        with tempfile.TemporaryDirectory() as root:
            tree = Path(root)
            (tree / "source.py").write_text("value = 1\n")
            expected = plan_experiment.tree_digest(tree)
            cache = tree / "__pycache__"
            cache.mkdir()
            (cache / "source.cpython-313.pyc").write_bytes(b"unstable")
            self.assertEqual(plan_experiment.tree_digest(tree), expected)

    def test_role_scoped_variant_rejects_unrelated_target_and_preserves_sentinels(self):
        with self.assertRaisesRegex(ValueError, "no change for role builder"):
            plan_experiment.build_plan(
                "nightly",
                {"records": []},
                "verifier-readonly-lean",
                False,
                "builder",
            )

        plan = plan_experiment.build_plan(
            "promotion",
            {"records": []},
            "verifier-readonly-lean",
            False,
            "verifier",
        )
        configurations = {
            item["role"]: item["variant"]
            for item in plan["contender_configurations"]
        }
        self.assertEqual(configurations["verifier"], "verifier-readonly-lean")
        self.assertEqual(configurations["builder"], "production")
        self.assertEqual(configurations["fixer"], "production")
        self.assertEqual(set(configurations), {"builder", "verifier", "fixer"})

    def test_shared_promotion_runs_the_full_regression_suite(self):
        plan = plan_experiment.build_plan(
            "promotion",
            {"records": []},
            "qwen-3.7-high",
            False,
            "shared",
        )
        manifest = json.loads((ROOT / "cases.json").read_text())["cases"]
        regression_cases = [
            case for case in manifest if case["role"] in plan_experiment.REGRESSION_ROLES
        ]
        self.assertEqual(len(plan["cases"]), len(regression_cases))
        self.assertEqual(len(plan["cases"]), 23)
        self.assertEqual(plan["attempts"], 3)

    def test_promotion_rejects_roles_without_regression_cases(self):
        with self.assertRaisesRegex(ValueError, "no regression cases for role critic"):
            plan_experiment.build_plan(
                "promotion",
                {"records": []},
                "qwen-3.7-high",
                False,
                "critic",
            )


class UsageTelemetryTest(unittest.TestCase):
    def test_pi_message_usage_populates_harbor_fields_without_double_counting(self):
        with tempfile.TemporaryDirectory() as root:
            runs = Path(root)
            message = {
                "type": "message_end",
                "message": {
                    "role": "assistant",
                    "usage": {
                        "input": 100,
                        "output": 25,
                        "cacheRead": 50,
                        "cacheWrite": 5,
                        "cost": {"total": 0.0123},
                    },
                },
            }
            duplicate = {**message, "type": "turn_end"}
            (runs / "run.log").write_text(json.dumps(message) + "\n" + json.dumps(duplicate) + "\n")
            self.assertEqual(
                usage_from_run_logs(runs),
                {"n_input_tokens": 155, "n_cache_tokens": 55, "n_output_tokens": 25, "cost_usd": 0.0123},
            )


class ExperimentHistoryTest(unittest.TestCase):
    def test_history_aggregates_quality_cost_and_configuration(self):
        experiment = {
            "experiment_fingerprint": "abc",
            "tier": "nightly",
            "variant": {"id": "qwen-3.7-high"},
            "source_revision": "deadbeef",
            "planner": "agentic",
            "cohort": "contender",
            "configurations": [{"configuration_fingerprint": "config-1"}],
        }
        history = experiment_history.summarize(
            [
                {
                    "case": "builder-ready-issue",
                    "experiment": experiment,
                    "rewards": {"deterministic": 1, "judge": 1},
                    "exception_type": None,
                    "token_cost": {"cost_usd": 0.25, "input_tokens": 100, "output_tokens": 25},
                    "duration_seconds": 12.5,
                    "status": "pass",
                }
            ]
        )
        self.assertEqual(history["summary"]["experiments"], 1)
        self.assertEqual(history["summary"]["cost_usd"], 0.25)
        self.assertEqual(history["summary"]["mean_duration_seconds"], 12.5)
        self.assertEqual(history["summary"]["input_tokens"], 100)
        self.assertEqual(history["summary"]["output_tokens"], 25)
        self.assertEqual(
            history["records"][0]["configurations"],
            [{"configuration_fingerprint": "config-1"}],
        )
        self.assertEqual(history["records"][0]["median_duration_seconds"], 12.5)
        self.assertEqual(history["summary"]["status_counts"], {"pass": 1})
        self.assertEqual(history["records"][0]["pass_rate"], 1.0)
        self.assertEqual(history["records"][0]["configuration_fingerprints"], ["config-1"])

    def test_missing_history_dataset_is_initialized_once(self):
        class MissingDataset(Exception):
            status_code = 404

        class Datasets:
            def get_runs(self, *_args, **_kwargs):
                raise MissingDataset()

        class Client:
            api = type("Api", (), {"datasets": Datasets()})()

            def __init__(self):
                self.created: list[tuple[str, str]] = []

            def create_dataset(self, *, name, description):
                self.created.append((name, description))

        client = Client()
        self.assertEqual(experiment_history.metadata_items(client, 10), [])
        self.assertEqual(client.created[0][0], experiment_history.DATASET_NAME)


if __name__ == "__main__":
    unittest.main()
