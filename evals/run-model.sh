#!/usr/bin/env bash
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export FOREST_EVAL_TIER="${FOREST_EVAL_TIER:-monthly}"
exec "$here/run-experiment.sh"
