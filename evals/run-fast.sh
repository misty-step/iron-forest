#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"
# This tier never starts a model or Judge, even in an authenticated shell.
unset OPENROUTER_API_KEY FOREST_EVAL_JUDGE_API_KEY FOREST_EVAL_JUDGE_MODEL
unset FOREST_EVAL_FORENSIC_JUDGE_MODEL FOREST_EVAL_REQUIRE_JUDGE
cases=()
journey=false
focused=false
while (($#)); do
  case "$1" in
    --case)
      if (($# < 2)); then echo '--case requires an ID' >&2; exit 2; fi
      cases+=(--case "$2"); focused=true; shift 2 ;;
    --journey) journey=true; focused=true; shift ;;
    --help|-h)
      echo 'Usage: evals/run-fast.sh [--case ID ...] [--journey]'
      echo 'No selectors: full deterministic merge check. Selectors: only the named cases/journey.'
      echo 'Focused cases require existing dependencies: cd evals && uv sync --locked'
      exit 0 ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done
if ! $focused; then
  python3 scripts/sync_tasks.py
  python3 -m unittest discover -s tests
  uv sync --locked
  journey=true
fi
job_name="fast-$(date -u +%Y%m%dT%H%M%SZ)-$$"
job_dir="jobs/fast/$job_name"
# Harbor owns its job directory; sibling input/evidence paths avoid collisions.
input_dir="jobs/fast/$job_name-inputs"
mkdir -p "$input_dir"
build_sha=$(git rev-parse HEAD)
build_dirty=false
if [[ -n "$(git status --porcelain)" ]]; then build_dirty=true; fi
# Always ask Docker to build; its native content cache owns invalidation.
docker build --provenance=false --file image/Dockerfile --tag iron-forest-eval:local \
  --iidfile "$input_dir/image-id" \
  --build-arg "FOREST_BUILD_SHA=$build_sha" \
  --build-arg "FOREST_BUILD_DIRTY=$build_dirty" ..
image=$(cat "$input_dir/image-id")
kernel_sha256=$(docker run --rm --network none --entrypoint sha256sum "$image" /opt/iron-forest/.iron-forest/bin/forest)
python3 - "$build_sha" "$build_dirty" "$image" "${kernel_sha256%% *}" <<'PY' | tee "$input_dir/image.json"
import json, sys
sha, dirty, image, kernel = sys.argv[1:]
print(json.dumps({"build_sha": sha, "dirty": dirty == "true", "image": image, "kernel_sha256": kernel}, sort_keys=True))
PY
if ! $focused || ((${#cases[@]})); then
  python3 scripts/sync_tasks.py --offline --output-dir "$input_dir/tasks" --image "$image" "${cases[@]}"
  uv run --no-sync harbor run \
    --path "$input_dir/tasks" \
    --agent oracle \
    --job-name "$job_name" \
    --jobs-dir jobs/fast \
    --n-concurrent "${FOREST_EVAL_CONCURRENCY:-6}" \
    --yes
  expected_cases=$(python3 -c 'import json,sys; from pathlib import Path; print(json.dumps(sorted(p.name for p in Path(sys.argv[1]).iterdir() if p.is_dir())))' "$input_dir/tasks")
  python3 scripts/assert_results.py "$job_dir" --expected-cases "$expected_cases"
fi
mkdir -p "$job_dir"
cp "$input_dir/image.json" "$job_dir/image.json"
if $journey; then
  docker run --rm --init --network none --user root \
    --env "FOREST_EVAL_IMAGE_ID=$image" \
    --mount "type=bind,src=$PWD/$input_dir/image.json,dst=/run/forest-eval-image.json,readonly" \
    "$image" python3 /opt/iron-forest-eval/journey.py \
    | tee "$job_dir/journey.json"
fi
printf 'Deterministic evidence: %s\n' "$PWD/$job_dir"
