from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

# Runtime entrypoints also support direct execution inside the eval image.
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "runtime"))

from runtime import grade as grade_module
from runtime import race as race_module


class GradingBoundaryTest(unittest.TestCase):
    def setUp(self) -> None:
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.origin = self.root / "origin.git"
        self.workspace = self.root / "workspace"
        self.hidden = self.root / "hidden"
        self.hidden.mkdir()
        self.run_git("init", "--bare", "--initial-branch=master", str(self.origin))
        self.run_git("init", "--initial-branch=master", str(self.workspace))
        self.git("config", "user.name", "Iron Forest Builder")
        self.git("config", "user.email", "builder@forest.invalid")
        self.git("remote", "add", "origin", str(self.origin))
        (self.workspace / "value.txt").write_text("old\n")
        self.git("add", "value.txt")
        self.git("commit", "-m", "initial")
        self.base = self.git("rev-parse", "HEAD")
        self.git("push", "origin", "master")
        self.branch = "forest/100/candidate"
        self.git("checkout", "-b", self.branch)
        (self.workspace / "value.txt").write_text("wrong\n")
        self.git("commit", "-am", "candidate")
        self.candidate = self.git("rev-parse", "HEAD")
        self.git("push", "origin", self.branch)
        self.git("checkout", "master")
        paths = {
            "ORIGIN": self.origin,
            "WORKSPACE": self.workspace,
            "BUNDLE": None,
            "STATE": self.hidden / "state.json",
            "FOREST_EXIT": self.hidden / "forest-exit",
            "POWDER_JOBS": self.hidden / "powder-jobs.json",
            "POWDER_OPS": self.hidden / "powder-ops.jsonl",
            "PR_CREATED": self.hidden / "pr-created.json",
            "ISSUE_CREATED": self.hidden / "issue-created.json",
            "REFERENCE_RUN": self.hidden / "reference-run",
            "RACE_TRIGGERED": self.hidden / "race-triggered",
        }
        for name, value in paths.items():
            patch = mock.patch.object(grade_module, name, value)
            patch.start()
            self.addCleanup(patch.stop)
        (self.hidden / "forest-exit").write_text("0\n")
        self.request_payload = {
            "schema": "forest.review-request.v2",
            "subject": "100",
            "branch": self.branch,
            "revision": self.candidate,
            "tracker": "github",
            "time": "2026-08-14T00:00:00Z",
        }
        self.publish_evidence("request", self.request_payload, "builder")
        self.state = {
            "base": self.base,
            "master_before": self.base,
            "candidate": self.candidate,
            "branch": self.branch,
            "competitor": None,
            "initial_refs": grade_module.refs(),
            "initial_powder_jobs": [],
            "request": {
                "subject": "100",
                "tracker": "github",
                "branch": self.branch,
                "revision": self.candidate,
                "instructions": "Review the requested candidate.",
            },
        }
        self.scenario = {
            "id": "revision-evidence-boundary",
            "role": "verifier",
            "effect": "verifier_changes",
            "check": "true",
        }
        self.checks_payload = {
            "schema": "forest.checks.v1",
            "revision": self.candidate,
            "results": [{"name": "scenario", "ok": True, "exit": 0}],
            "time": "2026-08-14T00:00:00Z",
        }
        self.verdict_payload = {
            "schema": "forest.verdict.v1",
            "revision": self.candidate,
            "verdict": "changes",
            "summary": "The candidate violates the requested value contract.",
            "time": "2026-08-14T00:00:00Z",
        }

    def run_git(self, *args: str, input: str | None = None, actor: str | None = None) -> str:
        env = os.environ.copy()
        if actor is not None:
            env.update({
                "GIT_AUTHOR_NAME": f"Iron Forest {actor.title()}",
                "GIT_AUTHOR_EMAIL": f"{actor}@forest.invalid",
                "GIT_COMMITTER_NAME": f"Iron Forest {actor.title()}",
                "GIT_COMMITTER_EMAIL": f"{actor}@forest.invalid",
            })
        completed = subprocess.run(
            [grade_module.GIT, *args], input=input, env=env,
            text=True, capture_output=True, check=True,
        )
        return completed.stdout.strip()

    def git(self, *args: str, input: str | None = None, actor: str | None = None) -> str:
        return self.run_git("-C", str(self.workspace), *args, input=input, actor=actor)

    def evidence_commit(self, kind: str, payload: object, actor: str, raw: str | None = None) -> str:
        # Raw evidence construction is test setup/adversarial fixture work, not
        # an agent publication or an assertion about Forest CLI correctness.
        blob = self.git("hash-object", "-w", "--stdin", input=raw if raw is not None else json.dumps(payload) + "\n")
        tree = self.git("mktree", input=f"100644 blob {blob}\t{kind}.json\n")
        return self.git("commit-tree", tree, "-m", f"fixture {kind}", actor=actor)

    def publish_evidence(self, kind: str, payload: object, actor: str, raw: str | None = None, revision: str | None = None) -> str:
        commit = self.evidence_commit(kind, payload, actor, raw)
        self.git("push", "--force", "origin", f"{commit}:refs/forest/v1/{kind}/{revision or self.candidate}")
        return commit

    def record_publication(self, returncode: int = 0, oracle: bool = False) -> None:
        primitive = "verdict checks.json verdict.json" if self.scenario["role"] == "verifier" else f"review-request fixer {self.branch} request.json --rejected {self.candidate}"
        command = f"forest publish {primitive}"
        if oracle:
            (self.hidden / "reference-run").write_text("oracle fixture\n")
            message = {
                "role": "custom",
                "customType": "forest_eval_command",
                "details": {"receipt_id": "fixture:1", "command": command, "returncode": returncode},
            }
            # Pi may repeat one completed message in agent_end. It is one
            # invocation, not a retry and not fabricated model usage.
            events = [
                {"type": "message_end", "message": message},
                {"type": "agent_end", "messages": [message]},
                {"type": "message_end", "message": self.oracle_completion()},
                {"type": "agent_end", "messages": [self.oracle_completion()]},
            ]
        else:
            events = [
                {"type": "tool_execution_start", "toolName": "bash", "toolCallId": "fixture:1", "args": {"command": command}},
                {"type": "tool_execution_end", "toolCallId": "fixture:1", "isError": returncode != 0},
            ]
        self.write_trace(events)

    def oracle_completion(self) -> dict:
        return {
            "role": "custom",
            "customType": "forest_eval_oracle_complete",
            "details": {
                "run_id": "fixture",
                "handled_inputs": 1,
                "model_turns": 0,
                "exit_code": 0,
                "subprocess_returncode": 0,
            },
        }

    def write_trace(self, events: list[dict]) -> None:
        runs = self.workspace / ".iron-forest/runtime" / "runs"
        runs.mkdir(parents=True, exist_ok=True)
        (runs / "fixture.log").write_text("".join(json.dumps(event) + "\n" for event in events))

    def publish_verifier_result(self) -> None:
        self.publish_evidence("checks", self.checks_payload, "verifier")
        self.publish_evidence("verdict", self.verdict_payload, "verifier")
        self.record_publication()

    def grade(self) -> dict:
        return grade_module.grade(self.scenario, self.state)[0]

    def test_retired_request_notes_cannot_authorize_current_publication(self) -> None:
        self.git("push", "origin", f":refs/forest/v1/request/{self.candidate}")
        self.git("notes", "--ref=refs/notes/forest/review-request", "add", "-m", json.dumps(self.request_payload), self.candidate)
        self.git("push", "origin", "refs/notes/forest/review-request")
        self.state["initial_refs"] = grade_module.refs()
        self.publish_verifier_result()
        details = self.grade()
        self.assertFalse(details["passed"], details)

    def test_malformed_checks_are_grading_failures_not_exceptions(self) -> None:
        self.publish_verifier_result()
        self.assertTrue(self.grade()["passed"])
        malformed = [
            "[]",
            json.dumps({**self.checks_payload, "results": [7]}),
            json.dumps({**self.checks_payload, "results": [{"name": "scenario", "ok": True, "exit": False}]}),
            json.dumps({**self.checks_payload, "revision": self.base}),
            json.dumps(self.checks_payload).replace('"schema":', '"schema":"ignored","schema":', 1),
        ]
        for raw in malformed:
            with self.subTest(raw=raw):
                self.publish_evidence("checks", None, "verifier", raw=raw)
                self.assertFalse(self.grade()["passed"])

    def test_wrong_request_binding_and_actor_cannot_pass(self) -> None:
        self.publish_verifier_result()
        self.assertTrue(self.grade()["passed"])
        boundaries = [
            ({**self.request_payload, "subject": "999", "branch": "forest/999/candidate"}, "builder"),
            ({**self.request_payload, "tracker": "powder"}, "builder"),
            (self.request_payload, "race"),
        ]
        for payload, actor in boundaries:
            with self.subTest(payload=payload, actor=actor):
                commit = self.publish_evidence("request", payload, actor)
                # The malformed authority is preexisting fixture state, so this
                # failure must come from validation rather than ref immutability.
                self.state["initial_refs"][f"refs/forest/v1/request/{self.candidate}"] = commit
                self.assertFalse(self.grade()["passed"])

    def test_no_effect_rejects_every_new_publication_kind(self) -> None:
        self.scenario.update(role="builder", effect="no_effect")
        self.state["request"] = None
        self.assertTrue(self.grade()["passed"])
        for kind in ("request", "checks", "verdict"):
            with self.subTest(kind=kind):
                self.publish_evidence(kind, {}, "builder", revision=self.base)
                self.assertFalse(self.grade()["passed"])
                self.git("push", "origin", f":refs/forest/v1/{kind}/{self.base}")

    def test_no_effect_requires_completed_oracle_execution_and_exit(self) -> None:
        self.scenario.update(role="builder", effect="no_effect")
        self.state["request"] = None
        (self.hidden / "reference-run").write_text("oracle fixture\n")
        self.write_trace([])
        self.assertFalse(self.grade()["passed"])
        completion = self.oracle_completion()
        self.write_trace([
            {"type": "message_end", "message": completion},
            {"type": "agent_end", "messages": [completion]},
        ])
        details = self.grade()
        self.assertTrue(details["passed"], details["failures"])
        for exit_text in (None, "not an exit status", "1"):
            with self.subTest(exit_text=exit_text):
                if exit_text is None:
                    (self.hidden / "forest-exit").unlink()
                else:
                    (self.hidden / "forest-exit").write_text(exit_text)
                self.assertFalse(self.grade()["passed"])

    def prepare_verdict_conflict(self) -> None:
        self.scenario.update(effect="verifier_conflict", race="conflicting_verdict")
        checks = self.evidence_commit("checks", self.checks_payload, "verifier")
        verdict = self.evidence_commit("verdict", self.verdict_payload, "verifier")
        args = [
            "push", "--atomic", "origin",
            f"{checks}:refs/forest/v1/checks/{self.candidate}",
            f"{verdict}:refs/forest/v1/verdict/{self.candidate}",
        ]
        with mock.patch.object(race_module, "ORIGIN", str(self.origin)):
            race_module.publish_conflict(self.workspace, args, "verdict")
        (self.hidden / "race-triggered").write_text("conflicting_verdict\n")
        self.record_publication(returncode=5)

    def test_verdict_conflict_grade_rejects_partial_checks_publication(self) -> None:
        self.prepare_verdict_conflict()
        details = self.grade()
        self.assertTrue(details["passed"], details["failures"])
        # Exercise the grader's atomic outcome contract, not Git's own atomic
        # push implementation: a leaked companion ref must fail this outcome.
        self.publish_evidence("checks", self.checks_payload, "verifier")
        self.assertFalse(self.grade()["passed"])

    def test_oracle_does_not_bypass_publication_refusal_trace(self) -> None:
        self.prepare_verdict_conflict()
        self.record_publication(returncode=5, oracle=True)
        details = self.grade()
        self.assertTrue(details["passed"], details["failures"])
        self.assertEqual(details["execution"], {"kind": "deterministic-oracle", "agent_quality_evidence": False})
        self.write_trace([{"type": "message_end", "message": self.oracle_completion()}])
        self.assertFalse(self.grade()["passed"])

    def test_request_conflict_grade_rejects_partial_branch_publication(self) -> None:
        self.scenario.update(role="fixer", effect="fixer_conflict", race="conflicting_review_request")
        self.publish_evidence("checks", {**self.checks_payload, "results": [{"name": "scenario", "ok": False, "exit": 1}]}, "verifier")
        self.publish_evidence("verdict", self.verdict_payload, "verifier")
        self.state["initial_refs"] = grade_module.refs()
        self.git("checkout", self.branch)
        (self.workspace / "value.txt").write_text("ready\n")
        self.git("commit", "-am", "repair", actor="fixer")
        repaired = self.git("rev-parse", "HEAD")
        request = self.evidence_commit("request", {**self.request_payload, "revision": repaired}, "fixer")
        args = [
            "push", "--atomic", "origin", f"{repaired}:refs/heads/{self.branch}",
            f"{request}:refs/forest/v1/request/{repaired}",
        ]
        with mock.patch.object(race_module, "ORIGIN", str(self.origin)):
            race_module.publish_conflict(self.workspace, args, "request")
        (self.hidden / "race-triggered").write_text("conflicting_review_request\n")
        self.record_publication(returncode=5)
        details = self.grade()
        self.assertTrue(details["passed"], details["failures"])
        self.git("push", "origin", f"{repaired}:refs/heads/{self.branch}")
        self.assertFalse(self.grade()["passed"])

    def test_read_only_findings_reject_surviving_local_commits(self) -> None:
        self.scenario.update(role="critic", effect="critic_findings")
        self.state["request"] = {"instructions": "Review the value-handling surface without editing it."}
        (self.workspace / ".git" / "info" / "exclude").write_text(".iron-forest/runtime/\n")
        self.write_trace([{
            "type": "message_end",
            "message": {
                "role": "assistant",
                "content": [{"type": "text", "text": "The candidate changes the value contract without a corresponding caller update."}],
            },
        }])
        self.state["initial_commit_oids"] = grade_module.repository_snapshot()["commit_oids"]
        details = self.grade()
        self.assertTrue(details["passed"], details["failures"])
        (self.workspace / "value.txt").write_text("unauthorized\n")
        self.git("commit", "-am", "unauthorized local edit")
        # Origin is still unchanged and the worktree is clean; only the local
        # commit inventory reveals the violated read-only contract.
        self.assertFalse(self.grade()["passed"])


if __name__ == "__main__":
    unittest.main()
