#!/usr/bin/env bash
set -euo pipefail

script="$(realpath "$0")"
cd "$(dirname "$script")"

tier="${FOREST_EVAL_TIER:-manual}"
variant="${FOREST_EVAL_VARIANT:-}"
role="${FOREST_EVAL_ROLE:-}"
if ! timeout_minutes="$(jq -er --arg tier "$tier" '.tiers[$tier].timeout_minutes | select(type == "number" and . > 0)' experiment-space.json)"; then
  echo "unknown experiment tier or invalid timeout: $tier" >&2
  exit 2
fi
if [[ "${FOREST_EVAL_DEADLINE_ACTIVE:-0}" != "1" ]]; then
  export FOREST_EVAL_DEADLINE_ACTIVE=1
  exec timeout --signal=TERM --kill-after=5m "${timeout_minutes}m" "$script" "$@"
fi
if [[ "${FOREST_EVAL_PRINT_TIMEOUT:-0}" == "1" ]]; then
  printf '%s\n' "$timeout_minutes"
  exit 0
fi
credential_file="${FOREST_EVAL_ENV_FILE:-$HOME/.config/iron-forest/evals.env}"
load_credentials() {
  [[ -f "$credential_file" ]] || return
  [[ -O "$credential_file" ]] || { echo "evaluation credential file is not owned by the current user: $credential_file" >&2; exit 2; }
  [[ "$(stat -c '%a' "$credential_file")" == "600" ]] || { echo "evaluation credential file must have mode 0600: $credential_file" >&2; exit 2; }
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "$line" || "$line" == \#* ]] && continue
    [[ "$line" == *=* ]] || { echo "invalid evaluation credential entry: $credential_file" >&2; exit 2; }
    name="${line%%=*}"
    value="${line#*=}"
    if (( ${#value} >= 2 )); then
      first="${value:0:1}"
      last="${value: -1}"
      if [[ ( "$first" == '"' && "$last" == '"' ) || ( "$first" == "'" && "$last" == "'" ) ]]; then
        value="${value:1:${#value}-2}"
      fi
    fi
    case "$name" in
      OPENROUTER_API_KEY|FOREST_EVAL_JUDGE_API_KEY|LANGFUSE_PUBLIC_KEY|LANGFUSE_SECRET_KEY|LANGFUSE_BASE_URL) ;;
      *) echo "unsupported evaluation credential name $name: $credential_file" >&2; exit 2 ;;
    esac
    if [[ -z "${!name:-}" ]]; then printf -v "$name" '%s' "$value"; export "$name"; fi
  done < "$credential_file"
}
load_credentials
: "${OPENROUTER_API_KEY:?OPENROUTER_API_KEY is required}"
: "${FOREST_EVAL_JUDGE_API_KEY:?FOREST_EVAL_JUDGE_API_KEY is required}"
: "${FOREST_EVAL_JUDGE_MODEL:=openrouter/google/gemini-3.7-flash}"
export FOREST_EVAL_JUDGE_MODEL FOREST_EVAL_REQUIRE_JUDGE=1

python3 scripts/sync_tasks.py
python3 -m unittest discover -s tests
uv sync --locked --extra langfuse
docker build --file image/Dockerfile --tag iron-forest-eval:local ..

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
experiment_dir="jobs/experiments/$stamp"
mkdir -p "$experiment_dir"
history_args=(--output "$experiment_dir/history.json" --markdown "$experiment_dir/history.md")
if [[ "${FOREST_EVAL_ALLOW_HISTORY_UNAVAILABLE:-0}" == "1" ]]; then history_args+=(--allow-unavailable); fi
uv run --extra langfuse python scripts/experiment_history.py "${history_args[@]}"
plan_args=(--tier "$tier" --history "$experiment_dir/history.json" --output "$experiment_dir/plan.json")
if [[ -n "$variant" ]]; then plan_args+=(--variant "$variant"); fi
if [[ -n "$role" ]]; then plan_args+=(--role "$role"); fi
if [[ "${FOREST_EVAL_DISABLE_AGENTIC_PLANNER:-0}" == "1" ]]; then plan_args+=(--no-agentic-planner); fi
python3 scripts/plan_experiment.py "${plan_args[@]}"

attempts="$(jq -r '.attempts' "$experiment_dir/plan.json")"
concurrency="$(jq -r '.budgets.concurrency' "$experiment_dir/plan.json")"
suite="capability"
if [[ "$tier" == "monthly" || "$tier" == "promotion" ]]; then suite="regression"; fi
contender_model="$(jq -r '.variant.model' "$experiment_dir/plan.json")"
incumbent_model="$(jq -r '.incumbent_configurations[0].model' "$experiment_dir/plan.json")"
thinking="$(jq -r '.variant.thinking // empty' "$experiment_dir/plan.json")"
tools="$(jq -r '.variant.tools // empty' "$experiment_dir/plan.json")"
prompt_append="$(jq -r '.variant.prompt_append // empty' "$experiment_dir/plan.json")"
variant_id="$(jq -r '.variant.id' "$experiment_dir/plan.json")"
variant_roles="$(jq -r '(.variant.apply_roles // .variant.roles) | join(",")' "$experiment_dir/plan.json")"
change_class="$(jq -r '.variant.change_class' "$experiment_dir/plan.json")"
# Model and thinking promotions use deterministic pass^3 as the safety gate
# and the Judge as the quality signal. Other change classes keep the stricter
# all-pass (Judge included) regression gate.
promotion_quality_win=false
if [[ "$tier" == "promotion" && ( "$change_class" == "model" || "$change_class" == "thinking" ) ]]; then
  promotion_quality_win=true
fi
comparison_suite="$suite"
if [[ "$promotion_quality_win" == "true" ]]; then comparison_suite="capability"; fi
cohort_judge_args=(--require-judge)
if [[ "$promotion_quality_win" == "true" ]]; then cohort_judge_args=(); fi

if [[ "$contender_model" == "$FOREST_EVAL_JUDGE_MODEL" ]]; then
  echo "contender and Judge models must differ" >&2
  exit 2
fi
case_args=()
while IFS= read -r case_id; do case_args+=(--include-task-name "$case_id"); done < <(jq -r '.cases[]' "$experiment_dir/plan.json")
expected_cases_json="$(jq -c '.cases' "$experiment_dir/plan.json")"

run_cohort() {
  local cohort="$1"
  local model="$2"
  local job_name="experiment-${stamp}-${cohort}"
  local job_dir="jobs/model/$job_name"
  local -a variant_args=()
  if [[ "$cohort" == "contender" ]]; then
    [[ -n "$thinking" ]] && variant_args+=(--agent-kwarg "thinking=$thinking")
    [[ -n "$variant_roles" ]] && variant_args+=(--agent-kwarg "variant_roles=$variant_roles")
    [[ -n "$tools" ]] && variant_args+=(--agent-kwarg "tools=$tools")
    [[ -n "$prompt_append" ]] && variant_args+=(--agent-kwarg "prompt_append=$prompt_append")
  fi
  set +e
  uv run harbor run \
    --path tasks \
    --agent iron_forest_eval.agent:IronForestAgent \
    --model "$model" \
    "${variant_args[@]}" \
    "${case_args[@]}" \
    --n-attempts "$attempts" \
    --job-name "$job_name" \
    --jobs-dir jobs/model \
    --n-concurrent "$concurrency" \
    --yes
  local harbor_exit=$?
  set -e
  if [[ ! -d "$job_dir" ]]; then
    echo "$cohort Harbor job produced no result directory" >&2
    return 1
  fi
  if [[ "$cohort" == "incumbent" ]]; then
    jq '. + {cohort: "incumbent", configurations: .incumbent_configurations}' "$experiment_dir/plan.json" > "$job_dir/experiment.json"
  else
    jq '. + {cohort: "contender", configurations: .contender_configurations}' "$experiment_dir/plan.json" > "$job_dir/experiment.json"
  fi
  set +e
  python3 scripts/assert_results.py "$job_dir" --suite "$suite" "${cohort_judge_args[@]}" --min-attempts "$attempts" --expected-cases "$expected_cases_json"
  local report_exit=$?
  set -e
  [[ $harbor_exit -eq 0 && $report_exit -eq 0 ]]
}

status=0
run_cohort incumbent "$incumbent_model" || status=1
run_cohort contender "$contender_model" || status=1
incumbent_job_dir="jobs/model/experiment-${stamp}-incumbent"
contender_job_dir="jobs/model/experiment-${stamp}-contender"
if [[ -d "$incumbent_job_dir" && -d "$contender_job_dir" ]]; then
  comparison_args=(
    "$contender_job_dir"
    --suite "$comparison_suite"
    --require-judge
    --min-attempts "$attempts"
    --expected-cases "$expected_cases_json"
    --baseline "$incumbent_job_dir"
  )
  if [[ "$tier" == "promotion" ]]; then
    comparison_args+=(--change-class "$change_class")
  fi
  python3 scripts/assert_results.py "${comparison_args[@]}" || status=1

  max_cost="$(jq -r '.budgets.max_estimated_cost_usd' "$experiment_dir/plan.json")"
  if ! jq -e '.totals.cost_usd != null' "$incumbent_job_dir/report.json" >/dev/null ||
    ! jq -e '.totals.cost_usd != null' "$contender_job_dir/report.json" >/dev/null; then
    echo "experiment cost telemetry is incomplete" >&2
    status=1
  else
    incumbent_cost="$(jq -r '.totals.cost_usd' "$incumbent_job_dir/report.json")"
    contender_cost="$(jq -r '.totals.cost_usd' "$contender_job_dir/report.json")"
    actual_cost="$(jq -n --argjson incumbent "$incumbent_cost" --argjson contender "$contender_cost" '$incumbent + $contender')"
    if ! jq -en --argjson actual "$actual_cost" --argjson maximum "$max_cost" '$actual <= $maximum' >/dev/null; then
      echo "experiment cost $actual_cost USD exceeds tier budget $max_cost USD" >&2
      status=1
    fi
  fi
fi
for job_dir in "$incumbent_job_dir" "$contender_job_dir"; do
  if [[ -d "$job_dir" ]]; then
    uv run --extra langfuse python scripts/langfuse_export.py "$job_dir" || status=1
  fi
done
jq -n \
  --arg schema forest.eval.experiment-result.v1 \
  --arg variant "$variant_id" \
  --arg fingerprint "$(jq -r '.experiment_fingerprint' "$experiment_dir/plan.json")" \
  --arg verdict "$(jq -r '.comparison.verdict // "unavailable"' "$contender_job_dir/report.json" 2>/dev/null || echo unavailable)" \
  --argjson passed "$([[ $status -eq 0 ]] && echo true || echo false)" \
  '{schema: $schema, variant: $variant, experiment_fingerprint: $fingerprint, comparison_verdict: $verdict, passed: $passed}' \
  > "$experiment_dir/result.json"
exit "$status"
