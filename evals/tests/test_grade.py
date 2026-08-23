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


if __name__ == "__main__":
    unittest.main()
