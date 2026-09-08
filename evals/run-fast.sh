#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"
# This tier never starts a model or Judge, even in an authenticated shell.
unset OPENROUTER_API_KEY FOREST_EVAL_JUDGE_API_KEY FOREST_EVAL_JUDGE_MODEL
unset FOREST_EVAL_FORENSIC_JUDGE_MODEL FOREST_EVAL_REQUIRE_JUDGE
python3 scripts/sync_tasks.py
python3 -m unittest discover -s tests
uv sync --locked
build_dirty=false
if [[ -n "$(git status --porcelain)" ]]; then build_dirty=true; fi
docker build --file image/Dockerfile --tag iron-forest-eval:local \
  --build-arg "FOREST_BUILD_SHA=$(git rev-parse HEAD)" \
  --build-arg "FOREST_BUILD_DIRTY=$build_dirty" ..
job_name="fast-$(date -u +%Y%m%dT%H%M%SZ)"
uv run harbor run \
  --path tasks \
  --agent oracle \
  --job-name "$job_name" \
  --jobs-dir jobs/fast \
  --n-concurrent "${FOREST_EVAL_CONCURRENCY:-6}" \
  --yes
python3 scripts/assert_results.py "jobs/fast/$job_name"
docker run --rm --init --network none --user root iron-forest-eval:local \
  python3 /opt/iron-forest-eval/journey.py \
  | tee "jobs/fast/$job_name/journey.json"
