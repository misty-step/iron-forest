# Production flywheel

The production flywheel turns observed production failures and field reports
into maintainable eval cases. It implements the production replay pipeline
named in [ADR 0025](adr/0025-harbor-langfuse-outcome-first-evals.md) and the
[evaluation strategy](evaluation-strategy.md).

The flywheel is observational and fail-open. Git task source plus Harbor lock,
result, artifacts, and scores remain authoritative. Langfuse is cataloging and
human annotation only; it never decides a reward, a promotion result, or a job
exit.

## Loop

```text
production trace (session id = Forest Run id)
  -> draft dataset item in Langfuse (sourceTraceId preserved)
  -> human-verified contract
  -> versioned Git task source (evals/production-cases.json)
  -> Harbor task (evals/tasks-production/<id>)
  -> fixed agent trial and regression pass
  -> exporter attaches scores back to the dataset item
```

The bidirectional provenance key is `source_trace_id`. It is recorded on the
draft dataset item at intake and copied into the promoted case contract, so the
original production trace stays linked to the eval case and every later replay
score.

## Intake

`evals/scripts/production_flywheel.py ingest` reads the retained Run logs in
`.forest/runs`, extracts each Run's `forest.run` identity line, locates the
OpenRouter Broadcast trace by `session_id = Forest Run id`, and creates a draft
dataset item in `iron-forest-production`. Before writing draft items, ingest
ensures the `iron-forest-production` dataset exists, mirroring the exporter's
`ensure_dataset` create-on-write behavior; a fresh Langfuse project does not
need manual dataset creation.

```sh
cd evals
uv run --extra langfuse python scripts/production_flywheel.py ingest --runs-dir ../.forest/runs
```

Draft items are created only once. The item id is `prod-<run-id>`, so a retry
upserts the same item instead of duplicating it. If no trace is located the
Run id is used as the source trace id, matching the exporter's fallback.

## Human verification

A draft item stays a draft until a human confirms the contract, privacy,
reference outcome, and a paired counterexample. The Langfuse annotation queue
is the review surface for those labels; it does not promote anything.

## Promotion

A human-authored contract is validated and recorded as versioned Git task
source:

```sh
cd evals
uv run --extra langfuse python scripts/production_flywheel.py promote \
  --contract ../path/to/production-case.json
```

The contract uses `forest.production-case.v1`:

```json
{
  "schema": "forest.production-case.v1",
  "id": "prod-builder-branch-race",
  "role": "builder",
  "summary": "Replay the observed branch race.",
  "effect": "builder_publish",
  "source_trace_id": "trace-openrouter-1",
  "source_run_id": "1787529620484390170-builder",
  "expected_files": {"value.txt": "ready\n"}
}
```

Promotion requires a unique slug id, a shipped role, a summary, an effect, a
`source_trace_id`, a `source_run_id`, and at least one scenario field
(`issue`, `powder_jobs`, `check`, `expected_files`, or `planted_files`). It
appends the case to `evals/production-cases.json`, sorted by id, and records
`suite: production-replay` plus `promoted_at`.

`evals/scripts/sync_tasks.py` generates production Harbor tasks from that
manifest into the separate `evals/tasks-production/` directory, leaving the
frozen regression suite in `evals/tasks/` untouched. Harbor remains the
execution authority; the script never runs a trial.

## Automated intake and report

Self-host installation enables `forest-eval-flywheel@iron-forest.timer`. The
timer runs daily with a bounded randomized delay and is persistent across host
downtime. It first retries queued paired-eval exports, then reads the manager
checkout's retained `.forest/runs`, performs idempotent production intake, and
emits the current coverage report to the unit journal. Successful retries
remove their outbox; failed jobs remain queued. The service has no write path
to the Git production-case manifest.

The unit starts only when the protected
`~/.config/iron-forest/evals.env` exists. Langfuse failures remain fail-open and
write retryable outbox entries. Inspect one run with:

```sh
systemctl --user status forest-eval-flywheel@iron-forest.timer
journalctl --user -u forest-eval-flywheel@iron-forest.service
```

The report names new draft cases, coverage by role and outcome,
production-distribution coverage, saturation, ambiguous or broken drafts, and
promoted grader-exploit regressions. Judge drift remains in the calibration
report and Langfuse score panels.

## Experiments

Prompt, tool, and model experiments remain separate from the production loop.
Each experiment records its hypothesis, frozen suite, baseline, result, and
decision in its own Powder job and Langfuse experiment. Failed experiments stay
visible and are never folded into a promoted production case silently.
