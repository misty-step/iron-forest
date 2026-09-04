from __future__ import annotations

import json
import os
import runpy
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
POWDER = ROOT / "runtime" / "powder"
sys.path.insert(0, str(ROOT / "runtime"))
POWDER_FIXTURE = runpy.run_path(str(POWDER))


class PowderIntakeTest(unittest.TestCase):
    def run_powder(
        self,
        hidden: Path,
        *args: str,
        state: Path | None = None,
    ) -> subprocess.CompletedProcess[str]:
        env = os.environ.copy()
        env["FOREST_EVAL_HIDDEN"] = str(hidden)
        env["XDG_STATE_HOME"] = str(state or hidden / "client-state")
        return subprocess.run(
            [sys.executable, str(POWDER), *args],
            cwd=ROOT,
            env=env,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )

    def test_spec_less_external_filing_is_not_takeable(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            hidden = Path(root) / "hidden"
            hidden.mkdir()
            repo = "misty-step/iron-forest"

            external = self.run_powder(
                hidden, "create", "--id", "if-critic-external", "--title",
                "External finding", "--repo", repo,
            )
            self.assertEqual(external.returncode, 0, msg=external.stderr)
            promoted = self.run_powder(
                hidden, "create", "--id", "if-ready-work", "--title",
                "Ready work", "--repo", repo, "--spec", "## Problem\nConcrete need.\n",
            )
            self.assertEqual(promoted.returncode, 0, msg=promoted.stderr)

            takeable = self.run_powder(hidden, "list", "--takeable", "--repo", repo)
            self.assertEqual(takeable.returncode, 0, msg=takeable.stderr)
            takeable_ids = [job["id"] for job in json.loads(takeable.stdout)]
            self.assertIn("if-ready-work", takeable_ids)
            self.assertNotIn("if-critic-external", takeable_ids)

            all_ = self.run_powder(hidden, "list", "--repo", repo)
            self.assertEqual(all_.returncode, 0, msg=all_.stderr)
            all_ids = [job["id"] for job in json.loads(all_.stdout)]
            self.assertIn("if-critic-external", all_ids)

    def test_forbidden_commands_fail_closed(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            hidden = Path(root) / "hidden"
            hidden.mkdir()
            forbidden = (
                "renew", "answer", "abandon", "reopen",
                "set-title", "set-spec", "set-repo", "set-blockers",
                "version", "skill",
            )
            for command in forbidden:
                result = self.run_powder(hidden, command, "if-any")
                self.assertNotEqual(result.returncode, 0, msg=command)
                self.assertIn("forbidden command", result.stderr, msg=command)
            self.assertFalse((hidden / "powder-ops.jsonl").exists())
            self.assertFalse((hidden / "powder-jobs.json").exists())

    def test_allowed_commands_stay_deterministic_after_forbidden_attempt(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            hidden = Path(root) / "hidden"
            hidden.mkdir()
            repo = "misty-step/iron-forest"

            forbidden = self.run_powder(
                hidden, "set-spec", "if-draft", "--spec", "## Problem\nPromoted improperly.\n"
            )
            self.assertNotEqual(forbidden.returncode, 0, msg=forbidden.stderr)
            self.assertFalse((hidden / "powder-jobs.json").exists())

            created = self.run_powder(
                hidden, "create", "--id", "if-draft", "--title", "Draft", "--repo", repo
            )
            self.assertEqual(created.returncode, 0, msg=created.stderr)
            takeable = self.run_powder(hidden, "list", "--takeable", "--repo", repo)
            self.assertEqual(takeable.returncode, 0, msg=takeable.stderr)
            self.assertEqual(json.loads(takeable.stdout), [])
            all_ = self.run_powder(hidden, "list", "--repo", repo)
            self.assertEqual(all_.returncode, 0, msg=all_.stderr)
            self.assertEqual(
                [job["id"] for job in json.loads(all_.stdout)], ["if-draft"]
            )

    def test_invalid_job_id_cannot_escape_claim_state(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            hidden = Path(root) / "hidden"
            hidden.mkdir()
            escaped = Path(root) / "escaped"

            created = self.run_powder(
                hidden,
                "create",
                "--id",
                "../../escaped",
                "--title",
                "Invalid",
                "--repo",
                "local/eval",
                "--spec",
                "work",
            )

            self.assertNotEqual(created.returncode, 0, msg=created.stderr)
            self.assertEqual(json.loads(created.stderr)["code"], "invalid_id")
            self.assertFalse(escaped.exists())
            self.assertFalse((hidden / "powder-jobs.json").exists())

            (hidden / "powder-jobs.json").write_text(json.dumps([{
                "id": "../../escaped",
                "title": "Invalid",
                "repo": "local/eval",
                "spec": "work",
                "state": "open",
                "lease": None,
            }]))
            taken = self.run_powder(hidden, "take", "../../escaped")
            self.assertNotEqual(taken.returncode, 0, msg=taken.stderr)
            self.assertEqual(json.loads(taken.stderr)["code"], "invalid_id")
            self.assertFalse(escaped.exists())

    def test_failed_claim_persistence_does_not_lease_job(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            hidden = Path(root) / "hidden"
            hidden.mkdir()
            created = self.run_powder(
                hidden,
                "create",
                "--id",
                "if-safe",
                "--title",
                "Safe",
                "--repo",
                "local/eval",
                "--spec",
                "work",
            )
            self.assertEqual(created.returncode, 0, msg=created.stderr)
            blocked_state = Path(root) / "blocked-state"
            blocked_state.write_text("not a directory")

            taken = self.run_powder(hidden, "take", "if-safe", state=blocked_state)

            self.assertNotEqual(taken.returncode, 0)
            jobs = json.loads((hidden / "powder-jobs.json").read_text())
            self.assertIsNone(jobs[0]["lease"])
            self.assertNotIn("_claim_hash", jobs[0])

    def test_job_persistence_failure_deletes_new_claim(self) -> None:
        job = {"id": "if-safe", "lease": None}
        calls = mock.Mock()
        calls.save_jobs.side_effect = OSError("disk full")
        persist = POWDER_FIXTURE["persist_new_claim"]
        with mock.patch.dict(
            persist.__globals__,
            {
                "save_claim": calls.save_claim,
                "save_jobs": calls.save_jobs,
                "delete_claim": calls.delete_claim,
            },
        ):
            with self.assertRaisesRegex(OSError, "disk full"):
                persist([job], job, "if-safe", "audit", "claim")

        self.assertEqual(
            calls.method_calls,
            [
                mock.call.save_claim("if-safe", "claim"),
                mock.call.save_jobs([job]),
                mock.call.delete_claim("if-safe"),
            ],
        )
        self.assertIsNone(job["lease"])
        self.assertNotIn("_claim_hash", job)

    def test_failed_atomic_replace_preserves_existing_claim(self) -> None:
        save_claim = POWDER_FIXTURE["save_claim"]
        stored_claim = POWDER_FIXTURE["stored_claim"]
        with tempfile.TemporaryDirectory() as root:
            with mock.patch.dict(os.environ, {"XDG_STATE_HOME": root}):
                save_claim("if-safe", "existing")
                with mock.patch.object(
                    save_claim.__globals__["os"],
                    "replace",
                    side_effect=OSError("replace failed"),
                ):
                    with self.assertRaisesRegex(OSError, "replace failed"):
                        save_claim("if-safe", "replacement")

                self.assertEqual(stored_claim("if-safe"), "existing")
                claim_dir = Path(root) / "powder" / "claims" / "eval-origin"
                self.assertEqual([path.name for path in claim_dir.iterdir()], ["if-safe"])

    def test_claim_symlink_cannot_target_another_job(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            hidden = Path(root) / "hidden"
            hidden.mkdir()
            state = hidden / "client-state"
            for job_id in ("attacker", "victim"):
                created = self.run_powder(
                    hidden,
                    "create",
                    "--id",
                    job_id,
                    "--title",
                    job_id,
                    "--repo",
                    "local/eval",
                    "--spec",
                    "work",
                )
                self.assertEqual(created.returncode, 0, msg=created.stderr)
                taken = self.run_powder(hidden, "take", job_id)
                self.assertEqual(taken.returncode, 0, msg=taken.stderr)

            jobs_file = hidden / "powder-jobs.json"
            jobs = json.loads(jobs_file.read_text())
            hashes = {job["id"]: job["_claim_hash"] for job in jobs}
            next(job for job in jobs if job["id"] == "attacker")["_claim_hash"] = hashes["victim"]
            jobs_file.write_text(json.dumps(jobs))
            claims = state / "powder" / "claims" / "eval-origin"
            attacker_claim = claims / "attacker"
            victim_claim = claims / "victim"
            attacker_claim.unlink()
            attacker_claim.symlink_to(victim_claim.name)

            released = self.run_powder(hidden, "release", "attacker")

            self.assertNotEqual(released.returncode, 0)
            self.assertTrue(attacker_claim.is_symlink())
            self.assertTrue(victim_claim.is_file())


    def test_claim_commands_are_deterministic(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            hidden = Path(root) / "hidden"
            hidden.mkdir()
            repo = "misty-step/iron-forest"
            agent = "forest-iron-forest"
            client_state = hidden / "client-state"
            isolated_state = hidden / "isolated-state"

            for job_id, title in (("if-ready", "Ready"), ("if-held", "Held")):
                created = self.run_powder(
                    hidden,
                    "create",
                    "--id",
                    job_id,
                    "--title",
                    title,
                    "--repo",
                    repo,
                    "--spec",
                    "## Problem\nConcrete need.\n",
                )
                self.assertEqual(created.returncode, 0, msg=created.stderr)

            held_take = self.run_powder(hidden, "take", "if-held")
            self.assertEqual(held_take.returncode, 0, msg=held_take.stderr)
            held_job = json.loads(held_take.stdout)
            self.assertEqual(held_job["lease"]["agent"], agent)
            self.assertNotIn("claim_token", held_job)
            self.assertNotIn("_claim_hash", held_job)

            duplicate = self.run_powder(
                hidden,
                "create",
                "--id",
                "if-held",
                "--title",
                "Held",
                "--repo",
                repo,
                "--spec",
                "## Problem\nConcrete need.\n",
            )
            self.assertEqual(duplicate.returncode, 0, msg=duplicate.stderr)
            duplicate_job = json.loads(duplicate.stdout)
            self.assertNotIn("claim_token", duplicate_job)
            self.assertNotIn("_claim_hash", duplicate_job)

            ready_take = self.run_powder(hidden, "take", "if-ready")
            self.assertEqual(ready_take.returncode, 0, msg=ready_take.stderr)

            mine = self.run_powder(hidden, "list", "--mine", agent, "--repo", repo)
            self.assertEqual(
                [job["id"] for job in json.loads(mine.stdout)],
                ["if-ready", "if-held"],
            )
            takeable = self.run_powder(hidden, "list", "--takeable", "--repo", repo)
            self.assertEqual(json.loads(takeable.stdout), [])

            resumed = self.run_powder(
                hidden, "take", "if-held", "--agent", "forest-other"
            )
            self.assertEqual(resumed.returncode, 0, msg=resumed.stderr)

            isolated = self.run_powder(
                hidden, "take", "if-held", "--agent", agent, state=isolated_state
            )
            self.assertNotEqual(isolated.returncode, 0, msg=isolated.stderr)
            self.assertEqual(json.loads(isolated.stderr)["code"], "held")

            missing_claim = self.run_powder(
                hidden, "done", "if-held", "--proof", "wrong", state=isolated_state
            )
            self.assertNotEqual(missing_claim.returncode, 0, msg=missing_claim.stderr)
            self.assertEqual(json.loads(missing_claim.stderr)["code"], "claim_required")

            isolated_claim = (
                isolated_state / "powder" / "claims" / "eval-origin" / "if-held"
            )
            isolated_claim.parent.mkdir(parents=True)
            isolated_claim.write_text("wrong-claim")
            os.chmod(isolated_claim, 0o600)
            invalid_claim = self.run_powder(
                hidden, "done", "if-held", "--proof", "wrong", state=isolated_state
            )
            self.assertNotEqual(invalid_claim.returncode, 0, msg=invalid_claim.stderr)
            self.assertEqual(json.loads(invalid_claim.stderr)["code"], "invalid_claim")

            ready_claim = (
                client_state / "powder" / "claims" / "eval-origin" / "if-ready"
            )
            self.assertEqual(ready_claim.stat().st_mode & 0o777, 0o600)
            released_ready = self.run_powder(hidden, "release", "if-ready")
            self.assertEqual(released_ready.returncode, 0, msg=released_ready.stderr)
            self.assertFalse(ready_claim.exists())

            held_claim = (
                client_state / "powder" / "claims" / "eval-origin" / "if-held"
            )
            released_held = self.run_powder(hidden, "release", "if-held")
            self.assertEqual(released_held.returncode, 0, msg=released_held.stderr)
            self.assertFalse(held_claim.exists())

            retaken = self.run_powder(hidden, "take", "if-held")
            self.assertEqual(retaken.returncode, 0, msg=retaken.stderr)
            completed = self.run_powder(
                hidden, "done", "if-held", "--proof", "abc123"
            )
            self.assertEqual(completed.returncode, 0, msg=completed.stderr)
            completed_job = json.loads(completed.stdout)
            self.assertTrue(completed_job["derived"]["terminal"])
            self.assertEqual(completed_job["proof"], "abc123")
            self.assertIsNone(completed_job["lease"])
            self.assertFalse(held_claim.exists())

            jobs_before_terminal_take = (hidden / "powder-jobs.json").read_text()
            ops_before_terminal_take = (hidden / "powder-ops.jsonl").read_text()
            terminal_take = self.run_powder(hidden, "take", "if-held")
            self.assertNotEqual(terminal_take.returncode, 0, msg=terminal_take.stderr)
            self.assertEqual(json.loads(terminal_take.stderr)["code"], "terminal")
            self.assertEqual(
                (hidden / "powder-jobs.json").read_text(),
                jobs_before_terminal_take,
            )
            self.assertEqual(
                (hidden / "powder-ops.jsonl").read_text(),
                ops_before_terminal_take,
            )

            shown = self.run_powder(hidden, "show", "if-held")
            self.assertEqual(shown.returncode, 0, msg=shown.stderr)
            self.assertTrue(json.loads(shown.stdout)["derived"]["terminal"])

            ops = [
                json.loads(line)
                for line in (hidden / "powder-ops.jsonl").read_text().splitlines()
                if line.strip()
            ]
            self.assertEqual(
                [op["op"] for op in ops if op["id"] == "if-held"],
                ["create", "take", "take", "release", "take", "done", "show"],
            )


if __name__ == "__main__":
    unittest.main()
