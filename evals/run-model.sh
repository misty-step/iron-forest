#!/usr/bin/env bash
set -euo pipefail

: "${OPENROUTER_API_KEY:?OPENROUTER_API_KEY is required}"
: "${FOREST_EVAL_JUDGE_MODEL:=openrouter/anthropic/claude-opus-4.8}"
export FOREST_EVAL_JUDGE_MODEL
if [[ -n "${FOREST_EVAL_CANDIDATE_MODEL:-}" && "$FOREST_EVAL_CANDIDATE_MODEL" == "$FOREST_EVAL_JUDGE_MODEL" ]]; then
  echo "candidate and Judge models must differ" >&2
  exit 2
fi

cd "$(dirname "$0")"
python3 scripts/sync_tasks.py
python3 -m unittest discover -s tests
uv sync --locked
docker build --file image/Dockerfile --tag iron-forest-eval:local ..
job_name="model-$(date -u +%Y%m%dT%H%M%SZ)"
model_args=()
if [[ -n "${FOREST_EVAL_CANDIDATE_MODEL:-}" ]]; then
  model_args=(--model "$FOREST_EVAL_CANDIDATE_MODEL")
fi
export FOREST_EVAL_REQUIRE_JUDGE=1
uv run harbor run \
  --path tasks \
  --agent iron_forest_eval.agent:IronForestAgent \
  "${model_args[@]}" \
  --n-attempts 3 \
  --job-name "$job_name" \
  --jobs-dir jobs/model \
  --n-concurrent "${FOREST_EVAL_CONCURRENCY:-3}" \
  --yes
python3 scripts/assert_results.py "jobs/model/$job_name"
