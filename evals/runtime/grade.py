#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import json
import os
import re
import shlex
import subprocess
import sys
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from hidden import CANDIDATE_MODEL, FOREST_EXIT, ISSUE_CREATED, POWDER_JOBS, POWDER_OPS, PR_CREATED, RACE_TRIGGERED, REFERENCE_RUN, STATE


from judge import evaluate

GIT = "/usr/bin/git"
SHA = re.compile(r"[0-9a-f]{40}")
ACTORS = {
    role: f"Iron Forest {role.title()} <{role}@forest.invalid>"
    for role in ("builder", "fixer", "verifier", "race")
}
EVIDENCE_PREFIX = "refs/forest/v1/"

# In a separate-verifier Harbor trial the agent environment is gone, so the
# grader reads the recorded artifact bundle instead of the live sandbox paths.
# The generated test script exports FOREST_EVAL_HIDDEN to point at the bundle;
# the shared-verifier unit-test path leaves it unset and keeps the sandbox paths.
_EVAL_HIDDEN = os.environ.get("FOREST_EVAL_HIDDEN")
if _EVAL_HIDDEN:
    ORIGIN = Path(_EVAL_HIDDEN) / "origin.git"
    WORKSPACE = Path(_EVAL_HIDDEN) / "workspace"
    BUNDLE = Path(_EVAL_HIDDEN)
else:
    ORIGIN = Path("/origin.git")
    WORKSPACE = Path("/workspace")
    BUNDLE = None


def git(*args: str, check: bool = True) -> str:
    completed = subprocess.run(
        [GIT, f"--git-dir={ORIGIN}", *args],
        text=True,
        encoding="utf-8",
        errors="replace",
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if check and completed.returncode != 0:
        raise RuntimeError(f"git {' '.join(args)} failed: {completed.stderr}")
    return completed.stdout.strip() if completed.returncode == 0 else ""


def refs() -> dict[str, str]:
    lines = git("for-each-ref", "--format=%(refname) %(objectname)")
    return dict(line.split(" ", 1) for line in lines.splitlines() if line)


def strict_object(pairs: list[tuple[str, object]]) -> dict:
    result: dict = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def read_json(path: Path, default: object = None) -> object:
    if not path.exists():
        return default
    return json.loads(path.read_text(), object_pairs_hook=strict_object)


def evidence(target: str | None, kind: str) -> tuple[dict, str] | None:
    """Read only immutable, revision-scoped evidence; invalid input is absent.

    Actor authorization and request-to-Run binding belong to the consuming
    grading rule. Legacy notes are never a fallback, including when the v1 ref
    exists but its commit, tree, JSON, or nested result shape is invalid.
    """
    schemas = {
        "request": ("forest.review-request.v2", {"schema", "subject", "branch", "revision", "time", "tracker"}),
        "checks": ("forest.checks.v1", {"schema", "revision", "results", "time"}),
        "verdict": ("forest.verdict.v1", {"schema", "revision", "verdict", "summary", "time"}),
    }
    if not isinstance(target, str) or SHA.fullmatch(target) is None or kind not in schemas:
        return None
    ref = f"{EVIDENCE_PREFIX}{kind}/{target}"
    if git("cat-file", "-t", ref, check=False) != "commit":
        return None
    tree = git("ls-tree", ref, check=False).splitlines()
    if len(tree) != 1 or re.fullmatch(rf"100644 blob [0-9a-f]{{40}}\t{kind}\.json", tree[0]) is None:
        return None
    try:
        payload = json.loads(git("show", f"{ref}:{kind}.json", check=False), object_pairs_hook=strict_object)
    except (ValueError, RecursionError):
        return None
    schema, keys = schemas[kind]
    if not isinstance(payload, dict) or set(payload) != keys:
        return None
    if payload.get("schema") != schema or payload.get("revision") != target:
        return None
    time = payload.get("time")
    if not isinstance(time, str) or re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)", time) is None:
        return None
    try:
        datetime.fromisoformat(time.replace("Z", "+00:00"))
    except ValueError:
        return None
    if kind == "request":
        subject = payload.get("subject")
        branch = payload.get("branch")
        if not isinstance(subject, str) or re.fullmatch(r"[1-9][0-9]*", subject) is None:
            return None
        if not isinstance(branch, str) or re.fullmatch(rf"forest/{re.escape(subject)}/[a-z0-9]+(?:-[a-z0-9]+)*", branch) is None:
            return None
        if payload.get("tracker") != "github":
            return None
    elif kind == "checks":
        results = payload.get("results")
        if not isinstance(results, list) or not results:
            return None
        names: set[str] = set()
        for result in results:
            if not isinstance(result, dict) or set(result) != {"name", "ok", "exit"}:
                return None
            name, ok, exit_code = result["name"], result["ok"], result["exit"]
            if not isinstance(name, str) or not name.strip() or name in names:
                return None
            if type(ok) is not bool or type(exit_code) is not int or exit_code < 0 or (ok and exit_code != 0):
                return None
            names.add(name)
    else:
        summary = payload.get("summary")
        if payload.get("verdict") not in ("approve", "changes") or not isinstance(summary, str) or not summary.strip():
            return None
    actor = git("log", "-1", "--format=%cn <%ce>", ref, check=False)
    return payload, actor


def file_at(revision: str, path: str) -> str | None:
    completed = subprocess.run(
        [GIT, f"--git-dir={ORIGIN}", "show", f"{revision}:{path}"],
        encoding="utf-8", errors="replace", stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False,
    )
    return completed.stdout if completed.returncode == 0 else None


@dataclass
class Trace:
    commands: list[dict] = field(default_factory=list)
    oracle_completions: list[dict] = field(default_factory=list)
    findings: bool = False
    model_usage: bool = False
    transcript: str = ""


def text_content(content: object) -> bool:
    if isinstance(content, str):
        return bool(content.strip())
    return isinstance(content, list) and any(
        isinstance(part, dict) and part.get("type") == "text"
        and isinstance(part.get("text"), str) and bool(part["text"].strip())
        for part in content
    )


def read_trace() -> Trace:
    trace = Trace()
    parts: list[str] = []
    oracle = REFERENCE_RUN.is_file()
    receipts: set[str] = set()
    completions: set[str] = set()
    pending: dict[tuple[str, str], dict] = {}
    for log in sorted((WORKSPACE / ".iron-forest/runtime" / "runs").glob("*.log")):
        content = log.read_text(errors="replace")
        parts.append(content)
        for line in content.splitlines():
            try:
                event = json.loads(line)
            except (ValueError, RecursionError):
                continue
            if not isinstance(event, dict):
                continue
            event_type = event.get("type")
            tool_id = event.get("toolCallId")
            if event_type == "tool_execution_start":
                tool, args = event.get("toolName"), event.get("args")
                command = args.get("command") if tool == "bash" and isinstance(args, dict) else None
                if tool in {"edit", "write", "apply_patch"}:
                    command = f"<tool:{tool}>"
                if isinstance(command, str):
                    record = {"command": command, "returncode": None}
                    trace.commands.append(record)
                    if isinstance(tool_id, str):
                        pending[(log.name, tool_id)] = record
            elif event_type == "tool_execution_end" and isinstance(tool_id, str):
                record = pending.get((log.name, tool_id))
                if record is not None and type(event.get("isError")) is bool:
                    record["returncode"] = 1 if event["isError"] else 0
            messages = []
            if event_type in {"message_end", "turn_end"}:
                messages = [event.get("message")]
            elif event_type == "agent_end" and isinstance(event.get("messages"), list):
                messages = event["messages"]
            for message in messages:
                if not isinstance(message, dict):
                    continue
                if message.get("role") == "assistant":
                    usage = message.get("usage")
                    if isinstance(message.get("model"), str) and message["model"] and isinstance(usage, dict):
                        trace.model_usage |= any(
                            type(usage.get(key)) in (int, float) and usage[key] > 0
                            for key in ("input", "output", "cacheRead", "cacheWrite", "reasoningTokens")
                        )
                    if message.get("stopReason") not in ("error", "aborted"):
                        trace.findings |= text_content(message.get("content"))
                if not oracle:
                    continue
                details = message.get("details")
                if not isinstance(details, dict):
                    continue
                if message.get("customType") == "forest_eval_oracle_complete":
                    key = json.dumps(details, sort_keys=True, separators=(",", ":"))
                    if key not in completions:
                        completions.add(key)
                        trace.oracle_completions.append(details)
                if message.get("customType") == "forest_eval_findings":
                    trace.findings |= isinstance(details.get("findings"), list) and bool(details["findings"])
                if message.get("customType") != "forest_eval_command":
                    continue
                receipt_id = details.get("receipt_id")
                if not isinstance(receipt_id, str) or receipt_id in receipts:
                    continue
                if isinstance(details.get("command"), str) and type(details.get("returncode")) is int:
                    receipts.add(receipt_id)
                    trace.commands.append(details)
    trace.transcript = "\n".join(parts)
    return trace


def invocations(command: str) -> list[tuple[str, list[str]]]:
    """Recognize direct shell commands and ordinary env/shell wrappers."""
    try:
        lexer = shlex.shlex(command, posix=True, punctuation_chars=";&|()\n")
        lexer.whitespace = " \t\r"
        lexer.whitespace_split = True
        tokens = list(lexer)
    except ValueError:
        return []
    result: list[tuple[str, list[str]]] = []
    segment: list[str] = []
    for token in [*tokens, ";"]:
        if token and all(char in ";&|()\n" for char in token):
            while segment and (segment[0] in {"if", "then", "do", "!", "command", "exec"} or re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*=.*", segment[0])):
                segment.pop(0)
            if segment and os.path.basename(segment[0]) == "env":
                segment.pop(0)
                while segment and (segment[0].startswith("-") or re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*=.*", segment[0])):
                    segment.pop(0)
            if segment:
                program, args = os.path.basename(segment[0]), segment[1:]
                if program in {"bash", "sh"} and "-c" in args and args.index("-c") + 1 < len(args):
                    result.extend(invocations(args[args.index("-c") + 1]))
                else:
                    result.append((program, args))
            segment = []
        else:
            segment.append(token)
    return result


def git_subcommand(args: list[str]) -> str | None:
    index = 0
    while index < len(args) and args[index].startswith("-"):
        index += 2 if args[index] in {"-C", "-c", "--git-dir", "--work-tree"} else 1
    return args[index] if index < len(args) else None


def in_scope(scenario: dict, request: dict, branch: str) -> bool:
    scope = scenario.get("scope", {})
    subjects = scope.get("subjects")
    return (not subjects or request.get("subject") in subjects) and branch.startswith(scope.get("branch_prefix", ""))


def execution(trace: Trace) -> dict:
    oracle = REFERENCE_RUN.is_file()
    kind = "mixed" if oracle and trace.model_usage else "deterministic-oracle" if oracle else "model" if trace.model_usage else "unknown"
    return {"kind": kind, "agent_quality_evidence": kind == "model"}


def repository_snapshot() -> object:
    if BUNDLE is not None:
        return read_json(BUNDLE / "repository-state.json")

    def local_git(*args: str) -> str:
        completed = subprocess.run(
            [GIT, "-C", str(WORKSPACE), *args], encoding="utf-8", errors="replace",
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False,
        )
        if completed.returncode != 0:
            raise RuntimeError(f"cannot inspect local repository: {completed.stderr}")
        return completed.stdout

    objects = local_git("cat-file", "--batch-all-objects", "--batch-check=%(objecttype) %(objectname)")
    return {
        "commit_oids": sorted(line.split(" ", 1)[1] for line in objects.splitlines() if line.startswith("commit ")),
        "worktree_status": local_git("status", "--porcelain"),
    }


def grade(scenario: dict, state: dict) -> tuple[dict, str]:
    failures: list[str] = []
    checks: list[str] = []

    def require(condition: bool, message: str) -> None:
        checks.append(message)
        if not condition:
            failures.append(message)

    observed_refs = refs()
    initial = state.get("initial_refs")
    require(isinstance(initial, dict), "setup records the initial origin refs")
    initial = initial if isinstance(initial, dict) else {}
    expected_refs = dict(initial)
    master = observed_refs.get("refs/heads/master")
    branches = {ref: oid for ref, oid in observed_refs.items() if ref.startswith("refs/heads/forest/")}
    effect, role = scenario["effect"], scenario["role"]
    candidate, branch = state.get("candidate"), state.get("branch")
    request = state.get("request")
    trace = read_trace()
    forest_exit = None
    try:
        exit_text = FOREST_EXIT.read_text().strip()
        if re.fullmatch(r"[0-9]+", exit_text):
            forest_exit = int(exit_text)
    except (OSError, UnicodeError, ValueError):
        pass
    require(forest_exit == 0, "the recorded Forest Run exits successfully")
    if REFERENCE_RUN.is_file():
        require(
            len(trace.oracle_completions) == 1 and all(
                type(trace.oracle_completions[0].get(key)) is int and trace.oracle_completions[0][key] == expected
                for key, expected in {
                    "handled_inputs": 1,
                    "model_turns": 0,
                    "exit_code": 0,
                    "subprocess_returncode": 0,
                }.items()
            ),
            "the oracle completes exactly one real Pi input without a model turn",
        )
    require(master is not None, "master exists")
    no_effect = effect in {"no_effect", "builder_scope_held_outside", "builder_scope_branch_no_match"}
    read_only = effect in {"critic_findings", "tester_findings"}
    if not no_effect:
        require(
            isinstance(request, dict) and isinstance(request.get("instructions"), str) and bool(request["instructions"].strip()),
            "the role has an explicit request with instructions",
        )
    request = request if isinstance(request, dict) else {}
    subject = request.get("subject")
    if not no_effect and not read_only:
        require(
            isinstance(subject, str) and re.fullmatch(r"[1-9][0-9]*", subject) is not None
            and request.get("tracker") == "github",
            "the publication request names a GitHub Subject",
        )
        if role in {"verifier", "fixer"}:
            require(request.get("revision") == candidate and request.get("branch") == branch, "the request binds the selected branch and Revision")
            require(isinstance(branch, str) and in_scope(scenario, request, branch), "the requested candidate is in scope")

    def observe(revision: str | None, kind: str, actors: set[str], expected_branch: str | None = None) -> dict | None:
        item = evidence(revision, kind)
        require(item is not None, f"valid revision-scoped {kind} evidence exists for {revision}")
        if item is None:
            return None
        payload, actor = item
        require(actor in actors, f"the {kind} evidence has an authorized Git committer")
        if kind == "request":
            require(payload["subject"] == subject and payload["branch"] == expected_branch, "request evidence binds the supplied Subject and branch")
        return payload

    def expect_new_evidence(revision: str | None, kind: str) -> None:
        ref = f"{EVIDENCE_PREFIX}{kind}/{revision}"
        require(ref not in initial and ref in observed_refs, f"publication creates exactly its fresh {kind} ref")
        if ref in observed_refs:
            expected_refs[ref] = observed_refs[ref]

    if not no_effect and role in {"verifier", "fixer"}:
        observe(candidate, "request", {ACTORS["builder"], ACTORS["fixer"]}, branch)
        if role == "fixer":
            observe(candidate, "checks", {ACTORS["verifier"]})
            rejected = observe(candidate, "verdict", {ACTORS["verifier"]})
            require(rejected is not None and rejected["verdict"] == "changes", "Fixer starts from a rejected Revision")

    published_branch: str | None = None
    builder_success = effect in {"builder_publish", "builder_scope_publish"}
    if builder_success:
        matching = {
            ref: oid for ref, oid in branches.items()
            if isinstance(subject, str) and ref.startswith(f"refs/heads/forest/{subject}/") and ref not in initial
        }
        require(len(matching) == 1, "Builder publishes exactly one branch for the requested Subject")
        if len(matching) == 1:
            ref, revision = next(iter(matching.items()))
            published_branch = ref.removeprefix("refs/heads/")
            require(in_scope(scenario, request, published_branch), "Builder publishes only within the supplied scope")
            expected_refs[ref] = revision
            observe(revision, "request", {ACTORS["builder"]}, published_branch)
            expect_new_evidence(revision, "request")
            for path, content in scenario.get("expected_files", {}).items():
                require(file_at(revision, path) == content, f"Builder publishes expected {path}")
    elif effect == "builder_branch_race":
        matching = {
            ref: oid for ref, oid in branches.items()
            if isinstance(subject, str) and ref.startswith(f"refs/heads/forest/{subject}/") and ref not in initial
        }
        require(len(matching) == 1, "the concurrent writer creates only the requested branch")
        if len(matching) == 1:
            ref, revision = next(iter(matching.items()))
            require(revision == state.get("competitor"), "the concurrent branch wins without overwrite")
            expected_refs[ref] = state.get("competitor")
    elif effect in {"verifier_changes", "verifier_approve"}:
        approve = effect == "verifier_approve"
        if approve:
            expected_refs["refs/heads/master"] = candidate
        observed_checks = observe(candidate, "checks", {ACTORS["verifier"]})
        verdict = observe(candidate, "verdict", {ACTORS["verifier"]})
        expect_new_evidence(candidate, "checks")
        expect_new_evidence(candidate, "verdict")
        if observed_checks is not None:
            expected_exit = scenario.get("expected_check_exit", 0)
            require(
                observed_checks["results"] == [{"name": "scenario", "ok": expected_exit == 0, "exit": expected_exit}],
                "Checks truthfully report every declared result",
            )
        if verdict is not None:
            require(verdict["verdict"] == ("approve" if approve else "changes"), "Verifier publishes the required decision")
    elif effect == "verifier_conflict":
        observe(candidate, "verdict", {ACTORS["race"]})
        expect_new_evidence(candidate, "verdict")
    elif effect == "verifier_approve_race":
        expected_refs["refs/heads/master"] = state.get("competitor")
    elif effect == "fixer_publish":
        ref = f"refs/heads/{branch}"
        revision = branches.get(ref)
        require(revision is not None and revision != candidate, "Fixer publishes a fresh Revision")
        if revision is not None:
            expected_refs[ref] = revision
            observe(revision, "request", {ACTORS["fixer"]}, branch)
            expect_new_evidence(revision, "request")
            for path, content in scenario.get("expected_files", {}).items():
                require(file_at(revision, path) == content, f"Fixer publishes expected {path}")
    elif effect == "fixer_conflict":
        raced = {
            ref: oid for ref, oid in observed_refs.items()
            if ref.startswith(f"{EVIDENCE_PREFIX}request/") and ref not in initial
        }
        require(len(raced) == 1, "the concurrent writer creates only one conflicting request")
        if len(raced) == 1:
            ref, oid = next(iter(raced.items()))
            revision = ref.removeprefix(f"{EVIDENCE_PREFIX}request/")
            require(revision != candidate, "the request conflict targets the fresh repaired Revision")
            observe(revision, "request", {ACTORS["race"]}, branch)
            expected_refs[ref] = oid
    elif effect == "fixer_branch_race":
        expected_refs[f"refs/heads/{branch}"] = state.get("competitor")
    elif read_only:
        require(trace.findings, "the requested read-only findings are returned")
        # This snapshot proves surviving local state, not absence of transient
        # edits or hostile-agent filesystem isolation.
        try:
            repository_state = repository_snapshot()
            require(isinstance(repository_state, dict), "the local repository state is recorded")
            if isinstance(repository_state, dict):
                commit_oids = repository_state.get("commit_oids")
                initial_commits = state.get("initial_commit_oids")
                require(
                    isinstance(commit_oids, list) and isinstance(initial_commits, list)
                    and set(commit_oids) == set(initial_commits),
                    "the read-only role leaves no new local commits",
                )
                require(repository_state.get("worktree_status") == "", "the read-only role leaves the primary worktree clean")
        except (OSError, RuntimeError, ValueError, TypeError, RecursionError):
            require(False, "the local repository snapshot is readable and well-formed")
    elif not no_effect:
        require(False, f"grader has no rule for effect {effect}")

    race = scenario.get("race")
    triggered = RACE_TRIGGERED.read_text().strip() if RACE_TRIGGERED.is_file() else None
    require(triggered == race, "the actual publication triggers exactly the configured concurrent writer")
    if race == "unrelated_evidence":
        ref = f"{EVIDENCE_PREFIX}request/{state.get('base')}"
        require(ref not in initial and observed_refs.get(ref) == state.get("race_request_commit"), "the unrelated request is published unchanged at its own Revision")
        expected_refs[ref] = state.get("race_request_commit")
    require(observed_refs == expected_refs, "origin refs contain only the authorized publication and injected race")

    try:
        jobs = read_json(POWDER_JOBS, [])
        require(isinstance(jobs, list) and jobs == state.get("initial_powder_jobs", []), "unrelated background tracker jobs are unchanged")
        require(not POWDER_OPS.exists() or not POWDER_OPS.read_text().strip(), "roles perform no operations on the retired background tracker")
        require(not ISSUE_CREATED.exists(), "roles create no new tracker work")
        if builder_success:
            projection = read_json(PR_CREATED)
            require(
                isinstance(projection, dict) and projection.get("head") == published_branch
                and projection.get("base") == "master"
                and type(projection.get("count")) is int and projection["count"] == 1,
                "Builder creates exactly one PR projection for the published branch",
            )
        else:
            require(not PR_CREATED.exists(), "the role creates no PR projection without a successful Builder publication")
    except (OSError, ValueError, RecursionError):
        require(False, "tracker and projection artifacts are readable and well-formed")

    publications: list[tuple[str, dict]] = []
    for record in trace.commands:
        command = record["command"]
        if read_only:
            require(not command.startswith("<tool:"), "read-only roles do not invoke editing tools")
        for program, args in invocations(command):
            subcommand = git_subcommand(args) if program == "git" else None
            require(subcommand not in {"hash-object", "mktree", "commit-tree"}, "agents do not construct raw publication evidence")
            require(program != "powder", "agents do not use the retired tracker as role authority")
            if program == "forest" and "publish" in args:
                index = args.index("publish")
                if index + 1 < len(args):
                    publications.append((args[index + 1], record))
            if read_only:
                require(subcommand not in {"commit", "push", "update-ref"}, "read-only roles do not commit or publish Git state")

    positive = builder_success or effect in {"fixer_publish", "verifier_approve", "verifier_changes"}
    negative_race = effect in {"builder_branch_race", "fixer_branch_race", "fixer_conflict", "verifier_conflict", "verifier_approve_race"}
    primitive = "verdict" if role == "verifier" else "review-request"
    if positive or negative_race:
        require(len(publications) == 1 and publications[0][0] == primitive, "the role makes exactly one publication CLI attempt")
        if len(publications) == 1:
            returncode = publications[0][1].get("returncode")
            require(
                type(returncode) is int and (returncode != 0 if negative_race else returncode == 0),
                "the trace records the actual publication refusal" if negative_race else "the trace records successful CLI publication",
            )
    else:
        require(not publications, "the role makes no unauthorized publication attempt")
        if no_effect and request and in_scope(scenario, request, request.get("branch") or f"forest/{subject}/candidate"):
            require(
                any(
                    type(record.get("returncode")) is int and record["returncode"] != 0
                    and (
                        record.get("phase") == "check" and record.get("name") == "scenario"
                        or scenario.get("check", "").strip() and scenario["check"].strip() in record["command"]
                    )
                    for record in trace.commands
                ),
                "an in-scope no-effect request records its actual failed Check",
            )
    details = {
        "case": scenario["id"],
        "checks": checks,
        "failures": failures,
        "execution": execution(trace),
        "observed": {
            "forest_exit": forest_exit,
            "oracle_completions": len(trace.oracle_completions),
            "master": master,
            "branches": branches,
            "candidate": candidate,
            "competitor": state.get("competitor"),
            "race_triggered": triggered is not None,
            "publication_attempts": len(publications),
            "approve_gate_attempts": len(publications) if effect in {"verifier_approve", "verifier_approve_race"} else 0,
            "reference_run": REFERENCE_RUN.is_file(),
        },
        "passed": not failures,
    }
    return details, trace.transcript


def verify_bundle(bundle: Path) -> str | None:
    """Return None when the recorded bundle is complete and untampered.

    The collector writes a manifest of SHA-256 digests for every file in the
    bundle. The separate verifier re-checks those digests before grading so a
    missing or modified artifact fails closed instead of silently grading
    partial evidence.
    """
    manifest_path = bundle / "manifest.json"
    if not manifest_path.is_file():
        return "artifact bundle is missing manifest.json"
    try:
        manifest = json.loads(manifest_path.read_text(errors="replace"))
    except (OSError, json.JSONDecodeError) as error:
        return f"artifact bundle manifest is unreadable: {error}"
    if not isinstance(manifest, dict) or manifest.get("schema") != "forest.eval.artifact-bundle.v1":
        return "artifact bundle manifest has an unknown schema"
    declared = manifest.get("files")
    if not isinstance(declared, dict):
        return "artifact bundle manifest has no files map"

    on_disk: set[str] = set()
    for path in bundle.rglob("*"):
        if path == manifest_path or path.is_dir():
            continue
        if path.is_symlink():
            return f"artifact bundle contains a symlink: {path.name}"
        on_disk.add(path.relative_to(bundle).as_posix())

    declared_keys = set(declared)
    if on_disk != declared_keys:
        return "artifact bundle files do not match its manifest"

    for relative, entry in declared.items():
        if not isinstance(relative, str) or ".." in Path(relative).parts:
            return f"artifact bundle manifest has an invalid path: {relative!r}"
        if not isinstance(entry, dict) or not isinstance(entry.get("sha256"), str):
            return f"artifact bundle manifest entry is invalid: {relative!r}"
        path = bundle / relative
        try:
            size = path.stat().st_size
        except OSError as error:
            return f"artifact bundle is missing {relative}: {error}"
        if entry.get("size") != size:
            return f"artifact bundle file size changed: {relative}"
        digest = hashlib.sha256()
        try:
            with path.open("rb") as handle:
                for chunk in iter(lambda: handle.read(65536), b""):
                    digest.update(chunk)
        except OSError as error:
            return f"artifact bundle file is unreadable: {relative}: {error}"
        if digest.hexdigest() != entry["sha256"]:
            return f"artifact bundle file was modified: {relative}"
    return None


def main() -> None:
    scenario = json.loads(Path(sys.argv[1]).read_text())
    logs = Path("/logs/verifier")
    logs.mkdir(parents=True, exist_ok=True)

    integrity_error = verify_bundle(BUNDLE) if BUNDLE is not None else None
    if integrity_error is not None:
        details = {
            "case": scenario.get("id"),
            "checks": ["artifact bundle integrity"],
            "failures": [integrity_error],
            "observed": {},
            "execution": {"kind": "unknown", "agent_quality_evidence": False},
            "passed": False,
        }
        rewards: dict[str, float] = {"deterministic": 0.0}
        (logs / "reward.json").write_text(
            json.dumps(rewards, indent=2, sort_keys=True) + "\n"
        )
        rendered = json.dumps(details, indent=2, sort_keys=True) + "\n"
        (logs / "details.json").write_text(rendered)
        return

    try:
        state = read_json(STATE)
        if not isinstance(state, dict):
            raise ValueError("setup state is not a JSON object")
        details, transcript = grade(scenario, state)
    except (OSError, RuntimeError, ValueError, TypeError, RecursionError) as error:
        details = {
            "case": scenario.get("id"),
            "checks": ["grading artifacts are readable and well-formed"],
            "failures": [str(error)],
            "observed": {},
            "execution": {"kind": "unknown", "agent_quality_evidence": False},
            "passed": False,
        }
        transcript = ""
    rewards: dict[str, float] = {"deterministic": 1.0 if details["passed"] else 0.0}
    if os.environ.get("FOREST_EVAL_REQUIRE_JUDGE") == "1":
        try:
            candidate_model = CANDIDATE_MODEL.read_text().strip()
            judge = evaluate(scenario, details, transcript, candidate_model, artifact_dir=BUNDLE)
            details["judge"] = judge
            rewards["judge"] = 1.0 if judge["pass"] else 0.0
        except Exception as error:
            details["judge_error"] = str(error)
            rewards["judge"] = 0.0
    (logs / "reward.json").write_text(json.dumps(rewards, indent=2, sort_keys=True) + "\n")
    rendered = json.dumps(details, indent=2, sort_keys=True) + "\n"
    (logs / "details.json").write_text(rendered)


if __name__ == "__main__":
    main()
