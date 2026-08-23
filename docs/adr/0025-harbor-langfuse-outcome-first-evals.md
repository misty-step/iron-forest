# 0025 — Harbor executes, Langfuse catalogs, outcome-first evals

Status: accepted, 2026-08-23

Extends [0017](0017-eval-driven-design.md). Destination store:
[evaluation strategy](../evaluation-strategy.md).

## Context

Iron Forest has a strong aspirational evaluation strategy but one 20-case
executable role suite, a monolithic uncalibrated Judge, no whole-Forest suite,
no production-trace flywheel, and no Langfuse datasets despite live OpenRouter
Broadcast traces. Recent review proved current graders can certify forbidden
behavior.

The original harness conflated three responsibilities: executing tasks,
cataloging results, and deciding what passes. Those need explicit,
non-overlapping owners before the suite inventory grows.

## Options considered

- **Harbor as the only system.** Rejected: Harbor has no production-trace
  catalog or experiment-comparison surface, and a single-system boundary
  conflates execution with cataloging.
- **Langfuse as the execution/repeat authority.** Rejected: Langfuse has no
  repetition inside one experiment and must not rerun Harbor tasks. It catalogs
  and compares; it does not execute.
- **A monolithic model Judge as the merge-quality gate.** Rejected: the current
  uncalibrated Judge can certify forbidden behavior. Deterministic safety
  graders must be authoritative, with model and human graders as additive
  signal only.

## Decision

### Ownership

- **Git** owns task contracts, case references, grader source, and locks.
  `evals/cases.json` and the deterministic grader source are authoritative task
  and grading contracts.
- **Harbor 0.21** owns isolated execution, `n_attempts`/repetitions, artifacts,
  trajectories, and regrade. Harbor is the repeat authority.
- **Langfuse** owns the production and eval trace catalog, datasets, experiment
  comparison, scores, human annotation, and dashboards. It does not rerun
  Harbor tasks and is observational, never authoritative for a reward or a job
  exit.
- **Powder** owns eval-improvement work. Humans own judge calibration and task
  promotion.
- Outcome-first deterministic safety graders are authoritative. Model and human
  graders add quality signal and never override a deterministic failure.

### Suites and metrics

Six suites partition evaluation work:

1. **eval-integrity** — task contracts, grader source, and grading authority
   are themselves tested; deterministic safety graders must catch forbidden
   behavior.
2. **regression** — the frozen shipped-role publication and race cases;
   `pass^3`.
3. **capability** — broader role capabilities; `pass@1` plus per-case
   confidence intervals, never hidden behind `pass@k`.
4. **whole-Forest** — end-to-end multi-role scenarios.
5. **adversarial/security** — planted defects, forbidden behavior,
   credential-shaped content, and prompt-injection counterexamples.
6. **production replay** — production-derived traces replayed through Harbor
   and cataloged in Langfuse.

### Langfuse export invariant

Langfuse export is post-run, idempotent, and fail-open. Git task/grader source
plus Harbor lock, results, artifacts, and scores remain authoritative. A
Langfuse outage, timeout, or retry never changes a Harbor reward, a promotion
result, or a job exit. Export failure records a retryable outbox entry and an
explicit warning.

Export identities are stable, derived from Harbor job id + trial id + candidate
Forest Run id, with the attempt index appended for dataset-run grouping. Retries
upsert the same dataset item, run item, and scores; they never duplicate
experiment data. The exporter locates existing OpenRouter Broadcast trace(s)
through `sessionId = Forest Run ID` and attaches dataset links, scores, and
metadata to them. It does not recreate model spans or duplicate
prompt/completion content.

Langfuse currently has no repetition inside one experiment, so Harbor remains
the repeat authority and each attempt index becomes one Langfuse run on export.

## Consequences

- Git remains the coordination authority. Langfuse is observational
  cataloging, not a second authority and not a coordination store.
- The CLI remains the operations surface. Langfuse dashboards are eval
  instrumentation for humans, not a Kernel product surface.
- The executable role suite is 20 cases across Builder, Verifier, and Fixer.
  The two experimental draft-only Critic and Tester sweeps are tracked
  separately and do not join the review loop.
- Deterministic safety failure cannot be overridden by a model or human grader.
- Follow-up work is filed as Powder jobs and lands through the normal Gate; the
  strategy document names the child jobs.
