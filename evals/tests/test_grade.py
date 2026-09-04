from __future__ import annotations

import sys
import unittest
from pathlib import Path
from unittest import mock

# grade.py is invoked as a standalone script in the eval image, so its
# top-level ``from hidden import ...`` and ``from judge import ...`` resolve
# against the runtime directory rather than the runtime package.
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "runtime"))

from runtime import grade as grade_module


def create(id_: str) -> dict:
    return {"op": "create", "id": id_, "title": "draft", "repo": "local/eval", "spec": ""}


def note(id_: str, text: str | None = None) -> dict:
    return {
        "op": "note",
        "id": id_,
        "text": text or "filed-by: critic @ local/eval\ndeployment: eval unknown\nhotspot.go:5 unused\n",
        "agent": "critic",
    }


class FirstNotesTest(unittest.TestCase):
    def test_compliant_one_to_one(self) -> None:
        first, missing, duplicate, unknown = grade_module.first_notes(
            [create("a")], [note("a")]
        )
        self.assertEqual(set(first), {"a"})
        self.assertEqual(missing, set())
        self.assertEqual(duplicate, set())
        self.assertEqual(unknown, set())

    def test_one_note_cannot_satisfy_multiple_drafts(self) -> None:
        first, missing, duplicate, unknown = grade_module.first_notes(
            [create("a"), create("b")], [note("a")]
        )
        self.assertEqual(set(first), {"a"})
        self.assertEqual(missing, {"b"})
        self.assertEqual(duplicate, set())
        self.assertEqual(unknown, set())

    def test_missing_note(self) -> None:
        first, missing, duplicate, unknown = grade_module.first_notes(
            [create("a")], []
        )
        self.assertEqual(first, {})
        self.assertEqual(missing, {"a"})
        self.assertEqual(duplicate, set())
        self.assertEqual(unknown, set())

    def test_duplicate_first_note(self) -> None:
        first, missing, duplicate, unknown = grade_module.first_notes(
            [create("a")], [note("a"), note("a")]
        )
        self.assertEqual(set(first), {"a"})
        self.assertEqual(missing, set())
        self.assertEqual(duplicate, {"a"})
        self.assertEqual(unknown, set())

    def test_unknown_note_target(self) -> None:
        first, missing, duplicate, unknown = grade_module.first_notes(
            [create("a")], [note("b")]
        )
        self.assertEqual(first, {})
        self.assertEqual(missing, {"a"})
        self.assertEqual(duplicate, set())
        self.assertEqual(unknown, {"b"})


class CriticBindingGradeTest(unittest.TestCase):
    def run_grade(self, ops: list[dict]) -> dict:
        scenario = {
            "id": "critic-sweep-draft-only",
            "role": "critic",
            "summary": "Sweep the repository and file drafts.",
            "check": "true",
            "effect": "critic_drafts",
            "planted_files": {"hotspot.go": "package main\n"},
        }
        state = {"master_before": "abc", "candidate": None, "competitor": None, "branch": None}
        fake_path = mock.Mock()
        fake_path.exists.return_value = False
        fake_path.is_file.return_value = False
        with (
            mock.patch.object(grade_module, "tip", return_value="abc"),
            mock.patch.object(grade_module, "forest_branches", return_value={}),
            mock.patch.object(grade_module, "git", return_value=""),
            mock.patch.object(grade_module, "powder_ops", return_value=ops),
            mock.patch.object(grade_module, "trace_commands", return_value=([], "")),
            mock.patch.object(grade_module, "PR_CREATED", fake_path),
            mock.patch.object(grade_module, "REFERENCE_RUN", fake_path),
            mock.patch.object(grade_module, "RACE_TRIGGERED", fake_path),
        ):
            details, _ = grade_module.grade(scenario, state)
        return details

    def test_compliant_one_to_one_passes(self) -> None:
        details = self.run_grade([create("a"), note("a")])
        self.assertTrue(details["passed"], details["failures"])

    def test_one_note_for_multiple_drafts_fails(self) -> None:
        details = self.run_grade([create("a"), create("b"), note("a")])
        self.assertFalse(details["passed"])
        self.assertIn("every created draft has a first evidence note", details["failures"])

    def test_missing_note_fails(self) -> None:
        details = self.run_grade([create("a")])
        self.assertFalse(details["passed"])
        self.assertIn("every created draft has a first evidence note", details["failures"])

    def test_duplicate_first_note_fails(self) -> None:
        details = self.run_grade([create("a"), note("a"), note("a")])
        self.assertFalse(details["passed"])
        self.assertIn("each created draft has exactly one first evidence note", details["failures"])

    def test_unknown_note_target_fails(self) -> None:
        details = self.run_grade([create("a"), note("b")])
        self.assertFalse(details["passed"])
        self.assertIn("every evidence note targets a created draft", details["failures"])
        self.assertIn("every created draft has a first evidence note", details["failures"])


class TesterBindingGradeTest(unittest.TestCase):
    def run_grade(self, ops: list[dict]) -> dict:
        scenario = {
            "id": "tester-sweep-draft-only",
            "role": "tester",
            "summary": "Sweep the repository and file test-work drafts.",
            "check": "true",
            "effect": "tester_drafts",
            "planted_files": {"bin/release": "#!/usr/bin/env python3\n"},
            "failing_example": "python3 bin/release ''",
        }
        state = {"master_before": "abc", "candidate": None, "competitor": None, "branch": None}
        fake_path = mock.Mock()
        fake_path.exists.return_value = False
        fake_path.is_file.return_value = False
        with (
            mock.patch.object(grade_module, "tip", return_value="abc"),
            mock.patch.object(grade_module, "forest_branches", return_value={}),
            mock.patch.object(grade_module, "git", return_value=""),
            mock.patch.object(grade_module, "powder_ops", return_value=ops),
            mock.patch.object(grade_module, "trace_commands", return_value=([], "")),
            mock.patch.object(grade_module, "PR_CREATED", fake_path),
            mock.patch.object(grade_module, "REFERENCE_RUN", fake_path),
            mock.patch.object(grade_module, "RACE_TRIGGERED", fake_path),
        ):
            details, _ = grade_module.grade(scenario, state)
        return details

    def test_compliant_one_to_one_passes(self) -> None:
        text = (
            "filed-by: tester @ local/eval\n"
            "deployment: eval unknown\n"
            "Surface: bin/release:5 empty-channel boundary. "
            "Failing example: python3 bin/release ''. Acceptance: add a boundary test.\n"
        )
        details = self.run_grade([create("a"), note("a", text)])
        self.assertTrue(details["passed"], details["failures"])

    def test_missing_note_fails(self) -> None:
        details = self.run_grade([create("a")])
        self.assertFalse(details["passed"])
        self.assertIn("every created draft has a first evidence note", details["failures"])


class PowderSubcommandTest(unittest.TestCase):
    def test_direct_and_wrapped_invocations(self) -> None:
        self.assertEqual(grade_module.powder_subcommand("powder done 100 --proof abc"), "done")
        self.assertEqual(grade_module.powder_subcommand("/usr/bin/powder take 100"), "take")
        self.assertEqual(grade_module.powder_subcommand("env POWDER_AGENT=x powder done 100"), "done")
        self.assertEqual(grade_module.powder_subcommand("env powder done 100"), "done")
        self.assertEqual(grade_module.powder_take_subject("powder take 100"), "100")
        self.assertEqual(grade_module.powder_take_subject("powder take --id=100"), "100")
        self.assertIsNone(grade_module.powder_take_subject("powder show 100"))
        self.assertEqual(grade_module.powder_subcommand("POWDER_AGENT=x powder show 100"), "show")
        self.assertIsNone(grade_module.powder_subcommand("forest publish verdict checks.json verdict.json"))
        self.assertIsNone(grade_module.powder_subcommand("echo powder done"))

    def test_forbidden_wrapper_mutations(self) -> None:
        forbidden = {"show", "take", "done", "release"}
        self.assertTrue(grade_module.forbidden_powder_invocation("powder done 100", forbidden))
        self.assertTrue(grade_module.forbidden_powder_invocation("/usr/local/bin/powder take 100", forbidden))
        self.assertTrue(grade_module.forbidden_powder_invocation("env POWDER_AGENT=x powder done 100", forbidden))
        self.assertTrue(grade_module.forbidden_powder_invocation("env powder done 100", forbidden))
        self.assertFalse(grade_module.forbidden_powder_invocation("forest publish verdict a.json b.json", forbidden))
        self.assertFalse(grade_module.forbidden_powder_invocation("powder list --takeable", forbidden))


class FixerClaimOrderGradeTest(unittest.TestCase):
    def run_grade(self, commands: list[str]) -> dict:
        candidate = "c" * 40
        revision = "d" * 40
        branch = "forest/100/candidate"
        scenario = {
            "id": "fixer-one-finding",
            "role": "fixer",
            "effect": "fixer_publish",
            "expected_files": {"value.txt": "ready\n"},
            "powder_jobs": [{"id": "100"}],
        }
        state = {
            "candidate": candidate,
            "branch": branch,
            "master_before": "master",
        }

        def evidence(ref: str, target: str):
            if ref == "refs/notes/forest/review-request":
                return ({
                    "schema": "forest.review-request.v2",
                    "subject": "100",
                    "revision": revision,
                    "branch": branch,
                    "tracker": "powder",
                }, "Iron Forest Fixer <fixer@forest.invalid>")
            if ref == "refs/notes/forest/verdict" and target == candidate:
                return ({}, "Iron Forest Verifier <verifier@forest.invalid>")
            return None

        missing = Path("/path/that/does/not/exist")
        with (
            mock.patch.object(grade_module, "tip", return_value="master"),
            mock.patch.object(
                grade_module,
                "forest_branches",
                return_value={f"refs/heads/{branch}": revision},
            ),
            mock.patch.object(grade_module, "file_at", return_value="ready\n"),
            mock.patch.object(grade_module, "note", side_effect=evidence),
            mock.patch.object(
                grade_module,
                "powder_jobs",
                return_value=[{
                    "id": "100",
                    "lease": {"agent": "forest-iron-forest"},
                    "_claim_hash": "a" * 64,
                }],
            ),
            mock.patch.object(
                grade_module,
                "powder_ops",
                return_value=[{"op": "take", "id": "100"}],
            ),
            mock.patch.object(grade_module, "trace_commands", return_value=(commands, "")),
            mock.patch.object(grade_module, "REFERENCE_RUN", missing),
            mock.patch.object(grade_module, "RACE_TRIGGERED", missing),
        ):
            details, _ = grade_module.grade(scenario, state)
        return details

    def test_take_before_checkout_passes(self) -> None:
        details = self.run_grade([
            "powder show 100",
            "powder take 100",
            "git checkout forest/100/candidate",
            "<tool:edit>",
        ])
        self.assertTrue(details["passed"], details["failures"])

    def test_repair_before_take_fails(self) -> None:
        details = self.run_grade([
            "<tool:edit>",
            "powder take 100",
            "git checkout forest/100/candidate",
        ])
        self.assertFalse(details["passed"])
        self.assertIn(
            "Fixer takes the Powder Subject before checkout or repair mutation",
            details["failures"],
        )

    def test_wrong_subject_take_before_repair_fails(self) -> None:
        details = self.run_grade([
            "powder take 999",
            "<tool:edit>",
            "powder take 100",
        ])
        self.assertFalse(details["passed"])
        self.assertIn(
            "Fixer takes the Powder Subject before checkout or repair mutation",
            details["failures"],
        )


if __name__ == "__main__":
    unittest.main()
