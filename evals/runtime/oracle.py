#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import pwd
import re
import shlex
import signal
import subprocess
import sys
import tempfile
from pathlib import Path

FOREST = "/workspace/.iron-forest/bin/forest"
GIT = "/usr/bin/git"
NO_EFFECTS = {"no_effect", "builder_scope_held_outside", "builder_scope_branch_no_match"}
CONFLICTS = {"builder_branch_race", "fixer_conflict", "fixer_branch_race", "verifier_conflict", "verifier_approve_race"}
ROLE_EFFECTS = {
    "builder": {"builder_publish", "builder_scope_publish", "builder_branch_race", *NO_EFFECTS},
    "fixer": {"fixer_publish", "fixer_conflict", "fixer_branch_race", "no_effect"},
    "verifier": {"verifier_changes", "verifier_approve", "verifier_conflict", "verifier_approve_race", "no_effect"},
    "critic": {"critic_findings", "no_effect"},
    "tester": {"tester_findings", "no_effect"},
}


class Oracle:
    def __init__(self, data: dict):
        self.data = data
        self.role = data["role"]
        self.effect = data["effect"]
        self.cwd = Path.cwd().resolve()
        self.run_id = os.environ["FOREST_RUN_ID"]
        self.sequence = 0
        root = Path(os.environ["FOREST_ROOT"]).resolve()
        if os.geteuid() != pwd.getpwnam("forest").pw_uid:
            raise PermissionError("oracle actions must execute as forest, not root")
        if self.cwd != root / ".iron-forest/runtime" / "worktrees" / self.run_id:
            raise RuntimeError("oracle actions require the actual Runner-assigned worktree")
        if self.effect not in ROLE_EFFECTS[self.role]:
            raise ValueError(f"unsupported {self.role} oracle effect: {self.effect}")
        # This is the same read-only request the real task prompt names. The
        # golden action plan is not independent authorization to touch the repo.
        self.request = json.loads(Path("/run/forest-eval/request.json").read_text())
        if self.request != data["request"]:
            raise RuntimeError("explicit request changed after oracle preparation")
        self.live_run = json.loads((root / ".iron-forest/runtime/runs" / f"live-{self.role}.json").read_text())
        if self.live_run.get("run_id") != self.run_id or self.live_run.get("agent") != self.role:
            raise RuntimeError("oracle does not own the live Run")

    def emit(self, kind: str, **details) -> None:
        self.sequence += 1
        print(json.dumps({
            "type": kind,
            "receipt_id": f"{self.run_id}:{self.sequence}",
            **details,
        }, sort_keys=True), flush=True)

    def command(self, *argv: str, phase: str = "inspect", expected: int | None = 0,
                environment: dict[str, str] | None = None, **details) -> subprocess.CompletedProcess[str]:
        completed = subprocess.run(
            argv, cwd=self.cwd, env=environment, text=True, capture_output=True, check=False
        )
        self.emit(
            "forest_eval_command", command=shlex.join(argv), argv=list(argv), cwd=str(self.cwd),
            phase=phase, returncode=completed.returncode, stdout=completed.stdout,
            stderr=completed.stderr, output=completed.stdout + completed.stderr, **details,
        )
        if expected is not None and completed.returncode != expected:
            raise RuntimeError(f"{shlex.join(argv)} exited {completed.returncode}, expected {expected}")
        return completed

    def git(self, *args: str) -> str:
        return self.command(GIT, *args, phase="git").stdout.strip()

    def config(self) -> dict:
        result = self.command(FOREST, "config", "show", "--root", str(self.cwd), "--json")
        return json.loads(result.stdout)["data"]

    def remote_tip(self, ref: str) -> str | None:
        value = self.git("ls-remote", "origin", ref)
        return value.split()[0] if value else None

    def evidence(self, kind: str, revision: str, actors: set[str]) -> dict:
        ref = f"refs/forest/v1/{kind}/{revision}"
        self.git("fetch", "--no-tags", "origin", ref)
        oid = self.git("rev-parse", "FETCH_HEAD")
        actor = self.git("show", "-s", "--format=%cn <%ce>", oid)
        if actor not in {f"Iron Forest {role.capitalize()} <{role}@forest.invalid>" for role in actors}:
            raise RuntimeError(f"unexpected {kind} evidence committer: {actor}")
        payload = json.loads(self.git("show", f"{oid}:{kind}.json"))
        schema = "forest.review-request.v3" if kind == "request" else f"forest.{kind}.v1"
        if payload.get("schema") != schema or payload.get("revision") != revision:
            raise RuntimeError(f"invalid {kind} evidence for requested revision {revision}")
        return payload

    def no_effect(self, reason: str) -> None:
        self.emit("forest_eval_result", status="no-effect", reason=reason)
        if self.effect not in NO_EFFECTS:
            raise RuntimeError(f"expected {self.effect}, but request cannot proceed: {reason}")

    def configured_checks(self, config: dict, phase: str = "check", failed_first: set[str] | None = None) -> list[dict]:
        checks = config["checks"]
        if failed_first:
            checks = sorted(checks, key=lambda check: check["name"] not in failed_first)
        results = []
        for check in checks:
            completed = self.command("/bin/sh", "-c", check["run"], phase=phase, expected=None, name=check["name"])
            code = completed.returncode if completed.returncode >= 0 else 128 - completed.returncode
            results.append({"name": check["name"], "ok": code == 0, "exit": code})
        return results

    def scope_reason(self, config: dict, branch: str) -> str | None:
        scope = config.get("scope") or {}
        subject = self.request["subject"]
        if scope.get("subjects") and subject not in scope["subjects"]:
            return f"supplied GitHub Subject {subject} is outside scope.subjects"
        if scope.get("branch_prefix") and not branch.startswith(scope["branch_prefix"]):
            return f"requested branch {branch} is outside scope.branch_prefix"
        if self.role == "builder":
            issue = json.loads(self.command(
                "gh", "issue", "view", subject, "--json", "number,title,body,labels,state"
            ).stdout)
            if str(issue["number"]) != subject or issue["state"] != "OPEN":
                return f"supplied GitHub Subject {subject} is not the current open Issue"
            label = scope.get("label")
            if label and label not in {item["name"] for item in issue["labels"]}:
                return f"supplied GitHub Subject {subject} lacks configured label {label}"
        return None

    def inspect_files(self, paths: list[str]) -> dict[str, list[str]]:
        inspected = {}
        for relative in paths:
            path = self.path(relative)
            if not path.is_file():
                continue
            lines = path.read_text().splitlines()
            inspected[relative] = lines
            self.emit("forest_eval_inspection", path=relative, evidence="\n".join(
                f"{relative}:{index}: {line}" for index, line in enumerate(lines, 1)
            ))
        return inspected

    def path(self, relative: str) -> Path:
        path = (self.cwd / relative).resolve()
        relative_parts = Path(relative).parts
        if (
            not path.is_relative_to(self.cwd)
            or relative_parts[:1] == (".git",)
            or relative_parts[:2] == (".iron-forest", "runtime")
        ):
            raise ValueError(f"oracle file escapes assigned source worktree: {relative}")
        return path

    def prepare_files(self) -> list[str]:
        files = self.data["files"]
        if not isinstance(files, dict) or not files:
            raise ValueError("authorized builder/fixer needs prepared expected_files or attempt_files")
        for relative, content in files.items():
            path = self.path(relative)
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content)
        self.emit("forest_eval_edit", paths=sorted(files))
        return sorted(files)

    def commit(self, files: list[str]) -> str:
        if self.effect in NO_EFFECTS:
            raise RuntimeError("prepared no-effect attempt unexpectedly passed every configured Check")
        for relative in files:
            if self.path(relative).read_text() != self.data["files"][relative]:
                raise RuntimeError(f"configured Checks changed the prepared content of {relative}")
        changed = set(self.git("diff", "--name-only").splitlines())
        if changed - set(files):
            raise RuntimeError(f"configured Checks changed unrequested tracked files: {sorted(changed - set(files))}")
        self.git("add", "--", *files)
        environment = os.environ.copy()
        environment.update(GIT_AUTHOR_DATE=self.data["time"], GIT_COMMITTER_DATE=self.data["time"])
        message = "eval: reference implementation" if self.role == "builder" else "eval: reference repair"
        self.command(GIT, "commit", "-m", message, phase="commit", environment=environment)
        return self.git("rev-parse", "HEAD")

    def publish(self, payloads: dict[str, dict], arguments: list[str]) -> dict | None:
        expected = 5 if self.effect in CONFLICTS else 0
        with tempfile.TemporaryDirectory(prefix="forest-eval-payload-") as directory:
            paths = {}
            for name, payload in payloads.items():
                path = Path(directory) / f"{name}.json"
                path.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n")
                paths[f"@{name}"] = str(path)
            argv = [paths.get(arg, arg) for arg in arguments]
            completed = self.command(
                FOREST, "publish", *argv, "--root", str(self.cwd), "--json",
                phase="publication", expected=expected,
            )
        envelope = json.loads(completed.stdout)
        if envelope["exit"] != expected:
            raise RuntimeError("publication exit disagrees with actual CLI envelope")
        if expected == 5:
            self.emit("forest_eval_result", status="conflict", reason=envelope["error"], returncode=5)
            return None
        self.emit("forest_eval_result", status="published", publication=envelope["data"])
        return envelope["data"]

    def review_request(self, branch: str, revision: str) -> dict:
        payload = {
            "schema": "forest.review-request.v3", "subject": self.request["subject"],
            "branch": branch, "revision": revision, "run_id": self.run_id, "time": self.data["time"],
        }
        for key in ("request_id", "work"):
            if key in self.live_run:
                payload[key] = self.live_run[key]
        return payload

    def builder(self, branch: str) -> None:
        primary = os.environ["FOREST_PRIMARY_REF"]
        self.git("fetch", "origin", primary)
        primary_revision = self.git("rev-parse", "FETCH_HEAD")
        previous_tip = self.remote_tip(f"refs/heads/{branch}")
        if primary_revision != self.data["base"] and previous_tip is None:
            raise RuntimeError("current primary moved away from the supplied builder starting revision")
        self.git("checkout", "--detach", self.data["base"])
        files = self.prepare_files()
        results = self.configured_checks(self.config())
        if any(not result["ok"] for result in results):
            self.no_effect("actual configured Check failed before commit; no publication or Projection attempted")
            return
        revision = self.commit(files)
        # Recreating the same ordinary commit and payload allows the CLI's real
        # identical retry, without inventing an alternate SHA or bypassing it.
        result = self.publish({"request": self.review_request(branch, revision)}, [
            "review-request", "builder", branch, "@request",
        ])
        if result is None:
            return
        projections = json.loads(self.command("gh", "pr", "list", "--head", branch, "--json", "number").stdout)
        if not projections:
            self.command(
                "gh", "pr", "create", "--head", branch, "--base", primary.removeprefix("refs/heads/"),
                "--title", f"Implement GitHub Issue #{self.request['subject']}",
                "--body", f"Closes #{self.request['subject']}\n\n{self.request['instructions']}", phase="projection",
            )

    def candidate(self, branch: str) -> tuple[str, str | None]:
        revision = self.request["revision"]
        if not isinstance(revision, str) or not re.fullmatch(r"[0-9a-f]{40}", revision):
            raise ValueError("candidate request must name one exact full revision")
        observed = self.remote_tip(f"refs/heads/{branch}")
        if observed != revision and self.role != "fixer":
            raise RuntimeError(f"requested branch tip {observed} does not match {revision}")
        request = self.evidence("request", revision, {"builder", "fixer"})
        if request.get("branch") != branch or request.get("subject") != self.request["subject"]:
            raise RuntimeError("request evidence does not match explicitly supplied branch and Subject")
        if request.get("work") != self.live_run.get("work"):
            raise RuntimeError("candidate work does not match the actual Run request")
        self.git("fetch", "origin", revision)
        self.git("checkout", "--detach", revision)
        return revision, observed

    def fixer(self, branch: str) -> None:
        rejected, observed = self.candidate(branch)
        verdict = self.evidence("verdict", rejected, {"verifier"})
        if verdict.get("verdict") != "changes":
            raise RuntimeError("requested Fixer revision does not have a changes Verdict")
        checks = self.evidence("checks", rejected, {"verifier"})
        self.emit("forest_eval_rejection", revision=rejected, summary=verdict["summary"])
        self.inspect_files(list(self.data["files"] or {}))
        self.configured_checks(self.config(), phase="reproduce-check", failed_first={
            check["name"] for check in checks["results"] if not check["ok"] or check["exit"] != 0
        })
        files = self.prepare_files()
        results = self.configured_checks(self.config())
        if any(not result["ok"] for result in results):
            self.no_effect("actual repair Check failed before commit; no fresh publication attempted")
            return
        revision = self.commit(files)
        if observed not in {rejected, revision}:
            raise RuntimeError("requested Fixer branch moved to an unrelated revision before repair")
        self.publish({"request": self.review_request(branch, revision)}, [
            "review-request", "fixer", branch, "@request", "--rejected", rejected,
        ])

    def verifier(self, branch: str) -> None:
        revision, _ = self.candidate(branch)
        primary = os.environ["FOREST_PRIMARY_REF"]
        self.git("fetch", "origin", primary)
        primary_revision = self.git("rev-parse", "FETCH_HEAD")
        self.git("diff", primary_revision, revision, "--")
        paths = self.git("diff", "--name-only", primary_revision, revision).splitlines()
        self.inspect_files(paths)
        results = self.configured_checks(self.config())
        ancestry = self.command(GIT, "merge-base", "--is-ancestor", primary_revision, revision, expected=None)
        if ancestry.returncode not in {0, 1}:
            raise RuntimeError("could not establish current primary ancestry")
        approve = self.effect in {"verifier_approve", "verifier_approve_race"}
        if approve and (ancestry.returncode != 0 or any(not result["ok"] for result in results)):
            raise RuntimeError("oracle approval expectation contradicted actual Checks or ancestry")
        verdict = "approve" if approve else "changes"
        summary = self.data["verdict_summary"] or self.data["semantic_contract"] or (
            "The requested Revision satisfies the review contract." if approve
            else "The requested Revision requires changes; see the recorded diff and Check evidence."
        )
        pause_path = self.data.get("pause_before_publication")
        if pause_path:
            marker = Path(pause_path).resolve()
            if marker.is_relative_to(Path(os.environ["FOREST_ROOT"]).resolve()):
                raise ValueError("oracle pause marker must be outside the managed checkout")
            marker.write_text(json.dumps({"run_id": self.run_id, "pid": os.getpid()}) + "\n")
            self.emit("forest_eval_pause", marker=str(marker), run_id=self.run_id, pid=os.getpid())
            signal.pause()
            raise RuntimeError("oracle interruption pause resumed without termination")
        self.publish({
            "checks": {"schema": "forest.checks.v1", "revision": revision, "results": results, "time": self.data["time"]},
            "verdict": {"schema": "forest.verdict.v1", "revision": revision, "verdict": verdict, "summary": summary, "time": self.data["time"]},
        }, ["verdict", "@checks", "@verdict"])

    def findings(self, config: dict) -> None:
        before = self.git("status", "--porcelain")
        inspected = self.inspect_files(self.data["inspection_paths"])
        self.configured_checks(config)
        findings = []
        if self.role == "critic":
            for path, lines in inspected.items():
                for number, line in enumerate(lines, 1):
                    if line.startswith("func DeadWeight("):
                        references = self.command(GIT, "grep", "-n", "-w", "DeadWeight", "--", "*.go")
                        uses = [row for row in references.stdout.splitlines() if not row.split(":", 2)[2].lstrip().startswith(("//", "func DeadWeight("))]
                        if not uses:
                            findings.append({
                                "title": "Unused exported helper", "file": path, "line": number,
                                "evidence": f"{path}:{number}: {line}; git grep -n -w DeadWeight -- '*.go' found only declaration/comment references.\n{references.stdout}",
                                "recommendation": "Remove DeadWeight unless a concrete caller requires this exported surface.",
                            })
        else:
            example = self.data["failing_example"]
            if not example:
                raise ValueError("tester oracle needs the prepared concrete failing_example command")
            empty = self.command("/bin/sh", "-c", example, expected=None, phase="inspection-check")
            tracked = self.git("ls-files").splitlines()
            test_paths = [path for path in tracked if "test" in Path(path).name.lower() and not path.startswith(".iron-forest/agents/")]
            self.inspect_files(test_paths)
            for path, lines in inspected.items():
                boundary = next((index for index, line in enumerate(lines, 1) if line.strip() == "if not channel:"), None)
                if boundary is None:
                    continue
                nonempty = self.command("/usr/bin/python3", path, "canary", expected=None, phase="inspection-check")
                findings.append({
                    "title": "Release channel boundary needs regression coverage", "file": path, "line": boundary,
                    "evidence": f"{path}:{boundary}: if not channel:\n{example}: exit {empty.returncode}, stdout={empty.stdout!r}, stderr={empty.stderr!r}\npython3 {path} canary: exit {nonempty.returncode}, stdout={nonempty.stdout!r}, stderr={nonempty.stderr!r}\nTracked test paths inspected: {test_paths}",
                    "recommendation": "Add regression assertions for the observed empty and non-empty channel behavior.",
                })
        if self.git("status", "--porcelain") != before:
            raise RuntimeError("read-only inspection or configured Check modified the worktree")
        if not findings:
            raise RuntimeError("prepared oracle finding was not supported by actual inspection")
        self.emit("forest_eval_findings", role=self.role, findings=findings[:5], tracker_mutations=0)

    def run(self) -> None:
        self.emit("forest_eval_request", role=self.role, request=self.request)
        config = self.config()
        if self.request is None:
            self.no_effect("no current operator request or explicit delegation; historical queues are not authorization")
            return
        if not isinstance(self.request.get("instructions"), str) or not self.request["instructions"].strip():
            raise ValueError("supplied request must contain explicit instructions")
        if self.role in {"critic", "tester"}:
            self.findings(config)
            return
        subject = self.request.get("subject")
        if not isinstance(subject, str) or not re.fullmatch(r"[1-9][0-9]*", subject):
            raise ValueError("publication requires one supplied numeric GitHub Subject")
        if self.request.get("tracker") != "github":
            raise ValueError("publication requires a current GitHub request")
        branch = self.request.get("branch") or f"forest/{subject}/eval"
        if not branch.startswith(f"forest/{subject}/"):
            raise ValueError("requested branch does not belong to supplied Subject")
        reason = self.scope_reason(config, branch)
        if reason:
            self.no_effect(reason)
        elif self.role == "builder":
            self.builder(branch)
        elif self.role == "fixer":
            self.fixer(branch)
        else:
            self.verifier(branch)


def main() -> int:
    try:
        Oracle(json.loads(Path(sys.argv[1]).read_text())).run()
        return 0
    except Exception as error:
        print(json.dumps({"type": "forest_eval_error", "error": str(error)}), flush=True)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
