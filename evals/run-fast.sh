#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"
python3 scripts/sync_tasks.py
python3 -m unittest discover -s tests
uv sync --locked
docker build --file image/Dockerfile --tag iron-forest-eval:local ..
job_name="fast-$(date -u +%Y%m%dT%H%M%SZ)"
uv run harbor run \
  --path tasks \
  --agent oracle \
  --job-name "$job_name" \
  --jobs-dir jobs/fast \
  --n-concurrent "${FOREST_EVAL_CONCURRENCY:-6}" \
  --yes
python3 scripts/assert_results.py "jobs/fast/$job_name"
