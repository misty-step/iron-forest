from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

import assert_results as reporter  # noqa: E402


def sha(prefix: str) -> str:
    return f"sha256:{prefix}{'0' * (64 - len(prefix))}"


def result(
    case: str,
    attempt: int,
    *,
    deterministic: float = 1.0,
    judge: float | None = None,
    exception: dict | None = None,
    regrade: bool = False,
    agent: str | None = "iron-forest",
    usage: bool = True,
) -> dict:
    rewards = {"deterministic": deterministic}
    if judge is not None:
        rewards["judge"] = judge
    value = {
        "id": f"{case}-{attempt}",
        "task_name": f"iron-forest/{case}",
        "trial_name": f"{case}__{attempt}-abc",
        "started_at": f"2026-08-24T00:0{attempt}:00Z",
        "finished_at": f"2026-08-24T00:0{attempt}:10Z",
        "agent_info": {
            "name": agent,
            "version": "1",
            "model_info": {"name": "test/candidate", "provider": "openrouter"},
        } if agent is not None else None,
        "verifier_result": {"rewards": rewards},
        "exception_info": exception,
        "agent_result": (
            {"n_input_tokens": 100, "n_cache_tokens": 10, "n_output_tokens": 40, "cost_usd": 0.02}
            if usage else None
        ),
        "config": {"agent": {"model_name": "openrouter/test/candidate"}},
    }
    if regrade:
        value["config"]["source_trial"] = {"action": "regrade"}
    return value


def lock(regrade: bool = False, agent: str | None = "iron-forest") -> dict:
    value = {
        "agent": (
            {"name": "oracle", "model_name": "openrouter/test/candidate"}
            if agent == "oracle"
            else {"import_path": "iron_forest_eval.agent:IronForestAgent"}
            if agent == "iron-forest"
            else {}
        ),
        "task": {"name": "builder-ready-issue", "type": "local", "digest": sha("a")},
        "skills": [
            {"name": "builder", "source": "/skills/builder", "digest": sha("b")},
        ],
        "environment": {
            "cpu_enforcement_policy": "limit",
            "memory_enforcement_policy": "limit",
            "override_cpus": 2,
            "override_memory_mb": 4096,
            "override_storage_mb": 10240,
        },
    }
    if regrade:
        value["source_trial"] = {"action": "regrade"}
    return value


def write_trial(
    job_dir: Path,
    case: str,
    attempt: int,
    *,
    deterministic: float = 1.0,
    judge: float | None = None,
    exception: dict | None = None,
    regrade: bool = False,
    agent: str | None = "iron-forest",
    usage: bool = True,
) -> Path:
    trial_dir = job_dir / f"{case}__{attempt}"
    trial_dir.mkdir(parents=True)
    (trial_dir / "result.json").write_text(
        json.dumps(result(
            case, attempt, deterministic=deterministic, judge=judge,
            exception=exception, regrade=regrade, agent=agent, usage=usage,
        ))
    )
    (trial_dir / "lock.json").write_text(json.dumps(lock(regrade=regrade, agent=agent)))
    return trial_dir


def write_job(root: Path, name: str = "model-job") -> Path:
    job_dir = root / name
    job_dir.mkdir()
    (job_dir / "lock.json").write_text(json.dumps({"n_concurrent_trials": 3}))
    return job_dir


def minimal_report(
    pass_rate: float,
    wilson: list[float],
    concurrency: int = 3,
    *,
    cost_usd: float | None = None,
    mean_duration_seconds: float | None = None,
) -> dict:
    return {
        "job": "job",
        "execution": {"kind": "model", "agent_quality_evidence": True},
        "totals": {
            "pass_at_1_rate": pass_rate,
            "pass_at_1_wilson": wilson,
            "deterministic_failed_trials": 0,
            "cost_usd": cost_usd,
            "mean_duration_seconds": mean_duration_seconds,
            "environment": {
                "concurrency": concurrency,
                "resources": {
                    "cpu": {"ceiling": 2},
                    "memory_mb": {"ceiling": 4096},
                    "storage_mb": {"ceiling": 10240},
                },
            },
        },
    }


class AssertResultsTest(unittest.TestCase):
    def test_regression_all_pass_builds_report(self):
        with tempfile.TemporaryDirectory() as tmp:
            job_dir = write_job(Path(tmp))
            for attempt in range(3):
                write_trial(job_dir, "builder-ready-issue", attempt)

            report = reporter.build_report(job_dir, suite="regression")
            self.assertEqual(report["totals"]["cases"], 1)
            self.assertEqual(report["totals"]["trials"], 3)
            self.assertEqual(report["totals"]["passed_trials"], 3)
            self.assertEqual(report["totals"]["exception_trials"], 0)
            self.assertTrue(report["cases"]["builder-ready-issue"]["pass_cubed"])
            self.assertTrue(report["cases"]["builder-ready-issue"]["saturated"])
            self.assertEqual(reporter.evaluate_gate(report), [])

    def test_perfect_oracle_cannot_satisfy_model_promotion(self):
        with tempfile.TemporaryDirectory() as tmp:
            oracle_dir = write_job(Path(tmp), "oracle")
            model_dir = write_job(Path(tmp), "model")
            for attempt in range(3):
                write_trial(oracle_dir, "builder-ready-issue", attempt, agent="oracle", judge=1.0)
                write_trial(model_dir, "builder-ready-issue", attempt, judge=1.0)
            oracle = reporter.build_report(oracle_dir, require_judge=True)
            model = reporter.build_report(model_dir, require_judge=True)

            self.assertEqual(
                oracle["execution"],
                {"kind": "deterministic-oracle", "agent_quality_evidence": False},
            )
            self.assertTrue(oracle["cases"]["builder-ready-issue"]["pass_cubed"])
            self.assertEqual(reporter.evaluate_gate(oracle), [])
            self.assertEqual(
                reporter.evaluate_gate(oracle, change_class="kernel", adr="ADR 0022"), [],
            )
            self.assertNotEqual(reporter.evaluate_gate(oracle, change_class="model"), [])
            comparison = reporter.compare_reports(oracle, model)
            self.assertEqual(comparison["verdict"], "incomparable-execution")
            self.assertIsNone(comparison["delta_points"])
            self.assertEqual(
                reporter.compare_reports(model, oracle)["verdict"], "incomparable-execution",
            )

    def test_mixed_and_unknown_cohorts_fail_closed(self):
        with tempfile.TemporaryDirectory() as tmp:
            job_dir = write_job(Path(tmp), "mixed")
            write_trial(job_dir, "builder-ready-issue", 0, judge=1.0)
            model = reporter.build_report(job_dir, require_judge=True)
            self.assertEqual(model["execution"], {"kind": "model", "agent_quality_evidence": True})
            self.assertEqual(reporter.evaluate_gate(model, change_class="prompt"), [])

            write_trial(job_dir, "builder-ready-issue", 1, agent="oracle", judge=1.0)
            mixed = reporter.build_report(job_dir, require_judge=True)
            self.assertEqual(mixed["execution"], {"kind": "mixed", "agent_quality_evidence": False})
            self.assertEqual(mixed["cases"]["builder-ready-issue"]["execution"]["kind"], "mixed")
            self.assertNotEqual(reporter.evaluate_gate(mixed, change_class="prompt"), [])
            self.assertNotEqual(reporter.evaluate_gate(mixed, baseline_report=model), [])

            unknown_dir = write_job(Path(tmp), "unknown")
            write_trial(unknown_dir, "builder-ready-issue", 0, agent=None, judge=1.0)
            write_trial(unknown_dir, "builder-ready-issue", 1, usage=False, judge=1.0)
            unknown = reporter.build_report(unknown_dir, require_judge=True)
            self.assertEqual(
                unknown["execution"], {"kind": "unknown", "agent_quality_evidence": False},
            )
            self.assertNotEqual(reporter.evaluate_gate(unknown, change_class="skill"), [])
            self.assertEqual(
                reporter.compare_reports(model, unknown)["verdict"], "incomparable-execution",
            )

    def test_historical_report_is_not_reclassified_or_overwritten(self):
        with tempfile.TemporaryDirectory() as tmp:
            job_dir = write_job(Path(tmp))
            write_trial(job_dir, "builder-ready-issue", 0, judge=1.0)
            current = reporter.build_report(job_dir, require_judge=True)
            historical = dict(current)
            del historical["execution"]
            json_path, md_path = reporter.write_report(historical, job_dir)
            original_json = json_path.read_bytes()
            original_markdown = md_path.read_bytes()

            self.assertNotEqual(reporter.evaluate_gate(historical, change_class="model"), [])
            self.assertEqual(
                reporter.compare_reports(current, historical)["verdict"], "incomparable-execution",
            )
            with self.assertRaises(FileExistsError):
                reporter.write_report(current, job_dir)
            reporter.write_report(current, job_dir / "derived")
            self.assertEqual(json_path.read_bytes(), original_json)
            self.assertEqual(md_path.read_bytes(), original_markdown)

    def test_judge_cannot_replace_a_missing_deterministic_grade(self):
        trial = result("builder-ready-issue", 0, judge=1.0)
        del trial["verifier_result"]["rewards"]["deterministic"]
        outcome = reporter.trial_outcome(trial, require_judge=True)
        self.assertEqual(outcome["outcome"], "no-reward")
        self.assertIsNone(outcome["deterministic"])

    def test_regression_exception_is_classified_infra(self):
        with tempfile.TemporaryDirectory() as tmp:
            job_dir = write_job(Path(tmp))
            write_trial(
                job_dir,
                "builder-ready-issue",
                0,
                exception={"exception_type": "TimeoutError", "exception_message": "connection timed out"},
            )

            report = reporter.build_report(job_dir, suite="regression")
            attempt = report["cases"]["builder-ready-issue"]["attempts"][0]
            self.assertEqual(attempt["outcome"], "exception")
            self.assertEqual(attempt["exception_class"], "infra")
            self.assertEqual(attempt["status"], "infra-error")
            self.assertEqual(report["totals"]["infra_exceptions"], 1)
            failures = reporter.evaluate_gate(report)
            self.assertTrue(any("infra" in failure for failure in failures))

    def test_judge_required_fails_when_missing(self):
        with tempfile.TemporaryDirectory() as tmp:
            job_dir = write_job(Path(tmp))
            write_trial(job_dir, "builder-ready-issue", 0, deterministic=1.0)

            report = reporter.build_report(job_dir, suite="regression", require_judge=True)
            attempt = report["cases"]["builder-ready-issue"]["attempts"][0]
            self.assertEqual(attempt["outcome"], "judge-missing")
            self.assertEqual(attempt["status"], "judge-error")
            self.assertTrue(any("judge-missing" in failure for failure in reporter.evaluate_gate(report)))

    def test_provider_and_judge_disagreement_are_distinct_statuses(self):
        provider = reporter.trial_outcome(
            result(
                "builder-ready-issue",
                0,
                exception={"exception_type": "ProviderUnavailable", "exception_message": "OpenRouter guardrail"},
            ),
            require_judge=True,
        )
        disagreement = reporter.trial_outcome(
            result("builder-ready-issue", 0, deterministic=0.0, judge=1.0),
            require_judge=True,
        )
        self.assertEqual(provider["status"], "provider-unavailable")
        self.assertEqual(disagreement["status"], "judge-disagreement")

    def test_min_attempts_floor(self):
        with tempfile.TemporaryDirectory() as tmp:
            job_dir = write_job(Path(tmp))
            for attempt in range(2):
                write_trial(job_dir, "builder-ready-issue", attempt)

            report = reporter.build_report(job_dir, suite="regression")
            self.assertFalse(report["cases"]["builder-ready-issue"]["pass_cubed"])
            failures = reporter.evaluate_gate(report, min_attempts=3)
            self.assertTrue(any("fewer than required 3" in failure for failure in failures))

    def test_wilson_interval_never_pretends_certainty(self):
        lower, upper = reporter.wilson_interval(20, 20)
        self.assertLess(lower, 1.0)
        self.assertEqual(upper, 1.0)
        self.assertEqual(reporter.wilson_interval(0, 0), [0.0, 0.0])

    def test_under_three_point_model_delta_is_not_a_win(self):
        current = minimal_report(0.80, [0.71, 0.87])
        baseline = minimal_report(0.79, [0.70, 0.86])
        comparison = reporter.compare_reports(current, baseline)
        self.assertNotEqual(comparison["verdict"], "win")

    def test_overlapping_intervals_block_model_win(self):
        current = minimal_report(0.95, [0.85, 0.99])
        baseline = minimal_report(0.90, [0.80, 0.96])
        comparison = reporter.compare_reports(current, baseline)
        self.assertNotEqual(comparison["verdict"], "win")

    def test_model_win_requires_delta_evidence_and_matched_infra(self):
        current = minimal_report(0.95, [0.90, 0.98])
        baseline = minimal_report(0.80, [0.70, 0.88])
        comparison = reporter.compare_reports(current, baseline)
        self.assertEqual(comparison["verdict"], "win")

    def test_equal_quality_with_materially_lower_cost_is_efficiency_win(self):
        current = minimal_report(1.0, [0.9, 1.0], cost_usd=4.0, mean_duration_seconds=100)
        baseline = minimal_report(1.0, [0.9, 1.0], cost_usd=5.0, mean_duration_seconds=100)
        comparison = reporter.compare_reports(current, baseline)
        self.assertEqual(comparison["verdict"], "efficiency-win")

    def test_judge_quality_win_is_reachable_when_deterministic_regression_is_saturated(self):
        """Promotion compares Judge quality rather than forcing pass@1 to 1.0.

        The regression safety gate already enforces deterministic pass^3 on the
        same cases. With the Judge included in the pass@1 metric, a
        higher-quality contender can still earn a statistically supported win
        even though every deterministic first attempt passed for both cohorts.
        """
        with tempfile.TemporaryDirectory() as tmp:
            baseline_dir = write_job(Path(tmp), name="baseline")
            contender_dir = write_job(Path(tmp), name="contender")
            for index in range(23):
                case = f"case-{index}"
                for attempt in range(3):
                    baseline_judge = 1.0 if index < 14 else 0.0
                    contender_judge = 1.0 if index < 22 else 0.0
                    write_trial(
                        baseline_dir,
                        case,
                        attempt,
                        deterministic=1.0,
                        judge=baseline_judge if attempt == 0 else 1.0,
                    )
                    write_trial(
                        contender_dir,
                        case,
                        attempt,
                        deterministic=1.0,
                        judge=contender_judge if attempt == 0 else 1.0,
                    )

            baseline = reporter.build_report(
                baseline_dir, suite="capability", require_judge=True
            )
            contender = reporter.build_report(
                contender_dir, suite="capability", require_judge=True
            )
            comparison = reporter.compare_reports(contender, baseline)
            self.assertEqual(comparison["verdict"], "win")
            self.assertLess(
                baseline["totals"]["pass_at_1_rate"],
                contender["totals"]["pass_at_1_rate"],
            )

            # A later safety failure must not hide behind the same winning pass@1.
            failed_trial = contender_dir / "case-22__1" / "result.json"
            failed_trial.write_text(json.dumps(result("case-22", 1, deterministic=0.0, judge=1.0)))
            unsafe = reporter.build_report(contender_dir, suite="capability", require_judge=True)
            self.assertEqual(unsafe["execution"], {"kind": "model", "agent_quality_evidence": True})
            self.assertEqual(
                unsafe["totals"]["pass_at_1_rate"], contender["totals"]["pass_at_1_rate"],
            )
            unsafe["comparison"] = reporter.compare_reports(unsafe, baseline)
            self.assertEqual(unsafe["comparison"]["verdict"], "safety-failure")
            self.assertNotEqual(
                reporter.evaluate_gate(unsafe, change_class="model", baseline_report=baseline), [],
            )

    def test_grader_change_requires_regrade(self):
        with tempfile.TemporaryDirectory() as tmp:
            job_dir = write_job(Path(tmp))
            write_trial(job_dir, "builder-ready-issue", 0)
            report = reporter.build_report(job_dir, suite="regression", require_judge=True)
            failures = reporter.evaluate_gate(report, change_class="grader")
            self.assertTrue(any("regrade" in failure for failure in failures))

    def test_kernel_change_requires_adr(self):
        with tempfile.TemporaryDirectory() as tmp:
            job_dir = write_job(Path(tmp))
            write_trial(job_dir, "builder-ready-issue", 0)
            report = reporter.build_report(job_dir, suite="regression", require_judge=True)
            self.assertTrue(
                any("ADR" in failure for failure in reporter.evaluate_gate(report, change_class="kernel"))
            )
            self.assertFalse(
                any("ADR" in failure for failure in reporter.evaluate_gate(report, change_class="kernel", adr="ADR 0027"))
            )

    def test_report_never_copies_exception_messages(self):
        secret = "sk-this-must-not-leave-the-repository"
        with tempfile.TemporaryDirectory() as tmp:
            job_dir = write_job(Path(tmp))
            write_trial(
                job_dir,
                "builder-ready-issue",
                0,
                exception={"exception_type": "ValueError", "exception_message": secret, "exception_traceback": secret},
            )
            report = reporter.build_report(job_dir, suite="regression")
            serialized = json.dumps(report, sort_keys=True)
            self.assertNotIn(secret, serialized)

    def test_expected_cases_match_passes_gate(self):
        with tempfile.TemporaryDirectory() as tmp:
            job_dir = write_job(Path(tmp))
            write_trial(job_dir, "builder-ready-issue", 0)
            report = reporter.build_report(job_dir, suite="regression")
            self.assertEqual(
                reporter.evaluate_gate(report, expected_cases={"builder-ready-issue"}),
                [],
            )

    def test_missing_planned_case_fails_gate(self):
        with tempfile.TemporaryDirectory() as tmp:
            job_dir = write_job(Path(tmp))
            write_trial(job_dir, "builder-ready-issue", 0)
            report = reporter.build_report(job_dir, suite="regression")
            failures = reporter.evaluate_gate(
                report,
                expected_cases={"builder-ready-issue", "verifier-clean-approve"},
            )
            self.assertTrue(
                any(
                    "missing" in failure and "verifier-clean-approve" in failure
                    for failure in failures
                )
            )

    def test_unexpected_case_fails_gate(self):
        with tempfile.TemporaryDirectory() as tmp:
            job_dir = write_job(Path(tmp))
            write_trial(job_dir, "builder-ready-issue", 0)
            report = reporter.build_report(job_dir, suite="regression")
            failures = reporter.evaluate_gate(report, expected_cases={"other-case"})
            self.assertTrue(
                any(
                    "unexpected" in failure and "builder-ready-issue" in failure
                    for failure in failures
                )
            )

    def test_baseline_coverage_is_checked_too(self):
        with tempfile.TemporaryDirectory() as tmp:
            job_dir = write_job(Path(tmp), name="current")
            baseline_dir = write_job(Path(tmp), name="baseline")
            write_trial(job_dir, "builder-ready-issue", 0)
            write_trial(baseline_dir, "verifier-clean-approve", 0)
            report = reporter.build_report(job_dir, suite="regression")
            baseline = reporter.build_report(baseline_dir, suite="regression")
            failures = reporter.evaluate_gate(
                report,
                baseline_report=baseline,
                expected_cases={"builder-ready-issue"},
            )
            self.assertTrue(
                any(
                    "baseline" in failure and "missing" in failure
                    and "builder-ready-issue" in failure
                    for failure in failures
                )
            )


if __name__ == "__main__":
    unittest.main()
