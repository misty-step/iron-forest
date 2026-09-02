# Langfuse eval dashboards

Langfuse is observational cataloging for humans, never a coordination store
(ADR 0025). The exporter in `evals/scripts/langfuse_export.py` publishes one
dataset (`iron-forest-evals`), one dataset item per Forest case, one dataset run
per Harbor attempt index, and deterministic scores keyed by
`<job>-<attempt>-<case>-<name>`.

Use these widget definitions when configuring the Langfuse `iron-forest`
project. Langfuse stores UI dashboards outside Git; this file is the source of
truth for what each panel means and the fields it reads.

## Dataset items

- Dataset: `iron-forest-evals`
- Item id: Forest case id (for example `builder-ready-issue`)
- `input.summary`: case contract summary
- `expected_output.role` / `expected_output.effect`: role and graded effect
- `metadata.case`, `metadata.role`, `metadata.effect`

## Scores

Every exported dataset run item carries numeric reward scores plus one
categorical `exception` score. Score names are the Harbor reward keys recorded
by the deterministic grader (`deterministic`, and `judge` when the model Judge
ran). The `exception` score is `none` or the Harbor `exception_type` class.

Run metadata also carries `experiment.experiment_fingerprint`,
`experiment.cohort`, `experiment.variant.id`, `experiment.planner`,
`experiment.selection_reason`, and the role-specific entries in
`experiment.configurations`. Configuration entries include model, thinking,
tools, prompt, skill, task, declaration, and evaluator digests. These fields
bind every score to the exact incumbent or contender configuration.

## Panels

### Pass rate by role

- Source: dataset runs
- X axis: `metadata.role`
- Y axis: mean of score `deterministic`
- Filter: `name = deterministic`

### Exception classes

- Source: dataset runs
- Breakdown: score `exception` (categorical)
- X axis: `metadata.role` or `metadata.case`
- Y axis: count

### Case outcomes

- Source: dataset runs
- X axis: `metadata.case`
- Y axis: mean of score `deterministic`
- Optional filter: `metadata.role`

### Model/provider

- Source: dataset run items
- Group by: `metadata.provider` and `metadata.model`
- Overlay: mean of score `deterministic`

### Incumbent versus contender

- Source: dataset run items
- Group by: `metadata.experiment.cohort`
- Breakdown: `metadata.experiment.variant.id`
- Overlay: mean `deterministic`, mean `judge`, trial duration, and recorded cost
- Filter: one `metadata.experiment.experiment_fingerprint`

### Longitudinal configuration quality

- Source: dataset run items
- X axis: run creation time
- Group by: role-specific
  `metadata.experiment.configurations.configuration_fingerprint`
- Overlay: deterministic pass rate, Judge pass rate, latency, and cost
- Filter out neither failures nor provider-unavailable attempts

### Token and cost

- Source: dataset run items
- Fields: `metadata.token_cost.input_tokens`,
  `metadata.token_cost.output_tokens`, `metadata.token_cost.cost_usd`
- Aggregation: sum by provider/model, or per case

### Latency

- Source: dataset run items
- Primary field: `metadata.duration_seconds`.
- Cross-check: `finished_at - started_at`, or trace latency from the linked
  OpenRouter Broadcast trace (`session_id = Forest Run id`).

## Trace linkage

Each dataset run item links a Harbor trial to its candidate Forest Run through
`trace_id`. The exporter locates the OpenRouter Broadcast trace by
`session_id = Forest Run id` before falling back to the Forest Run id itself,
and never recreates model spans or duplicates prompt/completion content.
