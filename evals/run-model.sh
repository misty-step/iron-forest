#!/usr/bin/env bash
set -euo pipefail

credential_file="${FOREST_EVAL_ENV_FILE:-$HOME/.config/iron-forest/evals.env}"
load_credentials() {
  [[ -f "$credential_file" ]] || return
  [[ -O "$credential_file" ]] || {
    echo "evaluation credential file is not owned by the current user: $credential_file" >&2
    exit 2
  }
  [[ "$(stat -c '%a' "$credential_file")" == "600" ]] || {
    echo "evaluation credential file must have mode 0600: $credential_file" >&2
    exit 2
  }
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "$line" || "$line" == \#* ]] && continue
    [[ "$line" == *=* ]] || {
      echo "invalid evaluation credential entry: $credential_file" >&2
      exit 2
    }
    name="${line%%=*}"
    value="${line#*=}"
    case "$name" in
      OPENROUTER_API_KEY|FOREST_EVAL_JUDGE_API_KEY|LANGFUSE_PUBLIC_KEY|LANGFUSE_SECRET_KEY|LANGFUSE_BASE_URL) ;;
      *)
        echo "unsupported evaluation credential name $name: $credential_file" >&2
        exit 2
        ;;
    esac
    if [[ -z "${!name:-}" ]]; then
      printf -v "$name" '%s' "$value"
      export "$name"
    fi
  done < "$credential_file"
}
if [[ -z "${OPENROUTER_API_KEY:-}" || -z "${FOREST_EVAL_JUDGE_API_KEY:-}" || -z "${LANGFUSE_PUBLIC_KEY:-}" || -z "${LANGFUSE_SECRET_KEY:-}" ]]; then
  load_credentials
fi
: "${OPENROUTER_API_KEY:?OPENROUTER_API_KEY is required}"
: "${FOREST_EVAL_JUDGE_API_KEY:?FOREST_EVAL_JUDGE_API_KEY is required}"
: "${FOREST_EVAL_JUDGE_MODEL:=openrouter/google/gemini-3.7-flash}"
export FOREST_EVAL_JUDGE_MODEL
: "${FOREST_EVAL_FORENSIC_JUDGE_MODEL:=}"
export FOREST_EVAL_FORENSIC_JUDGE_MODEL
if [[ -n "${FOREST_EVAL_CANDIDATE_MODEL:-}" && "$FOREST_EVAL_CANDIDATE_MODEL" == "$FOREST_EVAL_JUDGE_MODEL" ]]; then
  echo "candidate and Judge models must differ" >&2
  exit 2
fi
if [[ -n "${FOREST_EVAL_FORENSIC_JUDGE_MODEL:-}" && -n "${FOREST_EVAL_CANDIDATE_MODEL:-}" && "$FOREST_EVAL_CANDIDATE_MODEL" == "$FOREST_EVAL_FORENSIC_JUDGE_MODEL" ]]; then
  echo "candidate and forensic Judge models must differ" >&2
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
uv run --extra langfuse python scripts/langfuse_export.py "jobs/model/$job_name"
