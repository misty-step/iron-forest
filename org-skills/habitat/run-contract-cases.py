#!/usr/bin/env python3
"""Run the redacted Habitat CLI contract cases offline.

Each case replays a fixture through org-skills/habitat/bin/habitat and asserts the
documented exit code, output marker, and profile-owned repository/eligibility
rule from CONTRACT.md. When an installed CLI is available, binary wire cases
also exercise a loopback HTTP mock. No external network and no live ledger.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

HERE = Path(__file__).resolve().parent
BIN = HERE / "bin" / "habitat"
FIXTURES = HERE / "fixtures"

SELF_REPO = "misty-step/iron-forest"


def profile_eligible(fixture: dict) -> bool:
    profile = fixture.get("profile") or {}
    return profile.get("eligibility") == "eligible" and profile.get("repository") == SELF_REPO


CASES = [
    {
        "name": "eligible-item",
        "fixture": "eligible-item.json",
        "exit": 0,
        "stdout_contains": '"IRON-101"',
        "stderr_contains": None,
        "eligible": True,
    },
    {
        "name": "null-eligibility",
        "fixture": "null-eligibility.json",
        "exit": 0,
        "stdout_contains": '"IRON-NULL"',
        "stderr_contains": None,
        "eligible": False,
    },
    {
        "name": "foreign-repository",
        "fixture": "foreign-repository.json",
        "exit": 0,
        "stdout_contains": '"IRON-FOREIGN"',
        "stderr_contains": None,
        "eligible": False,
    },
    {
        "name": "missing-repository",
        "fixture": "missing-repository.json",
        "exit": 0,
        "stdout_contains": '"IRON-NOREPO"',
        "stderr_contains": None,
        "eligible": False,
    },
    {
        "name": "malformed",
        "fixture": "malformed.json",
        "exit": 1,
        "stdout_contains": None,
        "stderr_contains": "Invalid JSON response",
        "eligible": None,
    },
    {
        "name": "auth",
        "fixture": "auth.json",
        "exit": 1,
        "stdout_contains": None,
        "stderr_contains": "HTTP 401",
        "eligible": None,
    },
    {
        "name": "not-found",
        "fixture": "not-found.json",
        "exit": 1,
        "stdout_contains": None,
        "stderr_contains": "not found",
        "eligible": None,
    },
    {
        "name": "list-page-1",
        "fixture": "list-page-1.json",
        "exit": 0,
        "stdout_contains": '"IRON-101"',
        "stderr_contains": None,
        "eligible": None,
    },
    {
        "name": "list-page-2",
        "fixture": "list-page-2.json",
        "exit": 0,
        "stdout_contains": '"IRON-103"',
        "stderr_contains": None,
        "eligible": None,
    },
]


def find_binary() -> str | None:
    override = os.environ.get("HABITAT_BIN")
    if override is not None:
        binary = shutil.which(override)
        if binary is None:
            raise ValueError("HABITAT_BIN must name an executable")
        return binary
    installed = "/home/phaedrus/development/r90group/bin/habitat"
    return shutil.which(installed) or shutil.which("habitat")


def run_wire_cases(binary: str) -> tuple[int, int]:
    owner_id = "11111111-2222-4333-8444-555555555555"
    cases = [
        ("wire-owner-id", owner_id, 0),
        ("wire-owner-id-empty", "", 1),
        ("wire-owner-id-whitespace", " \t ", 1),
    ]
    paths: list[str] = []

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self) -> None:
            paths.append(self.path)
            body = b'{"count":0,"items":[]}'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, format: str, *args: object) -> None:
            pass

    failures = 0
    with HTTPServer(("127.0.0.1", 0), Handler) as server, tempfile.TemporaryDirectory() as home:
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            # Do not inherit real credentials, configuration, or proxy settings.
            env = {
                "PATH": os.environ.get("PATH", os.defpath),
                "HOME": home,
                "XDG_CONFIG_HOME": home,
                "HABITAT_API_URL": f"http://127.0.0.1:{server.server_port}",
                "HABITAT_API_TOKEN": "contract-test-dummy-token",
                "NO_PROXY": "127.0.0.1",
            }
            for name, value, expected_exit in cases:
                paths.clear()
                problems = []
                try:
                    proc = subprocess.run(
                        [binary, "list", "--owner-id", value, "--json"],
                        env=env,
                        capture_output=True,
                        text=True,
                        timeout=10,
                    )
                except (OSError, subprocess.TimeoutExpired) as error:
                    problems.append(f"binary invocation failed ({type(error).__name__})")
                else:
                    if proc.returncode != expected_exit:
                        problems.append(f"exit {proc.returncode} != {expected_exit}")
                    if expected_exit == 0:
                        if len(paths) != 1:
                            problems.append(f"expected one request, received {len(paths)}")
                        else:
                            url = urlsplit(paths[0])
                            query = parse_qs(url.query, keep_blank_values=True)
                            if url.path != "/api/work/items":
                                problems.append("wrong list endpoint")
                            if query.get("owner") != [owner_id]:
                                problems.append("owner UUID missing from wire query")
                            if "owner_id" in url.query:
                                problems.append("wire query contains owner_id")
                    elif paths:
                        problems.append("blank owner filter reached HTTP server")
                if problems:
                    failures += 1
                    print(f"FAIL {name}: {', '.join(problems)}")
                else:
                    print(f"PASS {name}")
        finally:
            server.shutdown()
            thread.join()
    return failures, len(cases)


def main() -> int:
    failures = 0
    for case in CASES:
        fixture_path = FIXTURES / case["fixture"]
        fixture = json.loads(fixture_path.read_text(encoding="utf-8"))

        proc = subprocess.run(
            [sys.executable, str(BIN)],
            env={"HABITAT_FIXTURE": str(fixture_path), "PATH": "/usr/bin:/bin"},
            capture_output=True,
            text=True,
            timeout=10,
        )

        problems = []
        if proc.returncode != case["exit"]:
            problems.append(f"exit {proc.returncode} != {case['exit']}")
        if case["stdout_contains"] is not None and case["stdout_contains"] not in proc.stdout:
            problems.append("stdout marker missing")
        if case["stderr_contains"] is not None and case["stderr_contains"] not in proc.stderr:
            problems.append("stderr marker missing")
        if case["eligible"] is not None and profile_eligible(fixture) is not case["eligible"]:
            problems.append(f"profile eligibility wrong ({profile_eligible(fixture)})")

        if problems:
            failures += 1
            print(f"FAIL {case['name']}: {', '.join(problems)}")
        else:
            print(f"PASS {case['name']}")

    wire_count = 0
    try:
        binary = find_binary()
        if binary is None:
            print("SKIP binary wire cases: habitat not found (set HABITAT_BIN)")
        else:
            wire_failures, wire_count = run_wire_cases(binary)
            failures += wire_failures
    except (OSError, ValueError) as error:
        failures += 1
        print(f"FAIL binary wire setup ({type(error).__name__})")

    if failures:
        print(f"{failures} case(s) failed", file=sys.stderr)
        return 1
    print(f"{len(CASES) + wire_count} cases passed ({len(CASES)} fixture, {wire_count} binary wire)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
