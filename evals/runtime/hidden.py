#!/usr/bin/env python3
from __future__ import annotations

import os
from pathlib import Path

HIDDEN = Path(os.environ.get("FOREST_EVAL_HIDDEN", "/hidden"))
ROLE = HIDDEN / "role"
SCENARIO = HIDDEN / "scenario.json"
STATE = HIDDEN / "state.json"
RACE = HIDDEN / "race.json"
RACE_TRIGGERED = HIDDEN / "race-triggered"
PR_CREATED = HIDDEN / "pr-created.json"
ISSUE_CREATED = HIDDEN / "issue-created.json"
POWDER_OPS = HIDDEN / "powder-ops.jsonl"
POWDER_JOBS = HIDDEN / "powder-jobs.json"
CANDIDATE_MODEL = HIDDEN / "candidate-model"
REFERENCE_RUN = HIDDEN / "reference-run"
FOREST_EXIT = HIDDEN / "forest-exit"


def ensure() -> Path:
    HIDDEN.mkdir(parents=True, exist_ok=True)
    return HIDDEN
