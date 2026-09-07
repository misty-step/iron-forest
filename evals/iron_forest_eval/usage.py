"""Recover token and cost telemetry from retained Pi JSONL Run logs."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any


def usage_from_run_logs(runs_dir: Path) -> dict[str, int | float | None]:
    totals: dict[str, int | float | None] = {
        "n_input_tokens": None,
        "n_cache_tokens": None,
        "n_output_tokens": None,
        "cost_usd": None,
    }
    for path in sorted(runs_dir.glob("*.log")):
        for line in path.read_text(errors="replace").splitlines():
            try:
                event: dict[str, Any] = json.loads(line)
            except json.JSONDecodeError:
                continue
            if event.get("type") != "message_end":
                continue
            message = event.get("message") or {}
            if message.get("role") != "assistant":
                continue
            usage = message.get("usage") or {}
            input_tokens = usage.get("input")
            cache_read = usage.get("cacheRead")
            cache_write = usage.get("cacheWrite")
            output_tokens = usage.get("output")
            cost = (usage.get("cost") or {}).get("total")
            cache_tokens = sum(value for value in (cache_read, cache_write) if isinstance(value, int))
            if isinstance(input_tokens, int) or cache_tokens:
                totals["n_input_tokens"] = int(totals["n_input_tokens"] or 0) + int(input_tokens or 0) + cache_tokens
            if cache_tokens:
                totals["n_cache_tokens"] = int(totals["n_cache_tokens"] or 0) + cache_tokens
            if isinstance(output_tokens, int):
                totals["n_output_tokens"] = int(totals["n_output_tokens"] or 0) + output_tokens
            if isinstance(cost, (int, float)):
                totals["cost_usd"] = float(totals["cost_usd"] or 0) + float(cost)
    if totals["cost_usd"] is not None:
        totals["cost_usd"] = round(float(totals["cost_usd"]), 8)
    return totals
