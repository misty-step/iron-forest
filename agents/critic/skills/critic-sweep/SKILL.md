---
name: critic-sweep
description: >
  Run one whole-codebase critique sweep and file at most five SPEC-LESS draft
  Powder jobs with file:line evidence. Read-only: never edit code, never
  promote work, never call a Kernel Effect.
---

# Critic sweep

Run one sweep per dispatch. The poll cadence is set in `forest.yaml`; do not
loop or re-run on your own.

## 1. Orient

Read `VISION.md`, the accepted ADRs under `docs/adr/`, and the repository
conventions (`README.md`, `forest.yaml`, and any repository-level `AGENTS.md`
when present). Record the product lock and the shipped roster before judging
drift.

## 2. Sweep

Inspect the codebase for:

- architecture drift vs `VISION.md` and accepted ADRs
- dead weight: unused exported surface, orphaned paths, stale docs that
  contradict shipped behavior
- complecting: one component owning unrelated responsibilities
- convention violations without an ADR
- untested hotspots for observable behavior or failure paths

Use `grep` and `read` to locate the exact line. A finding must name a
`file:line` and state both the observed wrong state and the required state.
Discard anything without a concrete observation. Do not report style
preference as a defect.

## 3. Deduplicate

List current jobs once, using the top-level `repo:` value from `forest.yaml` as the repository:

```sh
powder list --repo <forest.yaml repo>
```

Skip a finding whose evidence or proposed direction is already covered by an
existing open or draft job.

## 4. File drafts

File at most five findings per sweep. For each, create a job with no spec so
it is never takeable, then attach the evidence note. The first note is an
external draft note: it must carry `filed-by` and `deployment` provenance
before the finding evidence.

```sh
powder create --id <slug> --title '<short title>' --repo <forest.yaml repo>
powder note <slug> --text 'filed-by: <agent-identity> @ <forest.yaml repo>
deployment: <instance> <observed-binary-revision>
Observed: <file:line> ... Required: ... Proposed spec direction: ...' --agent "${POWDER_AGENT:-critic}"
```

- `<forest.yaml repo>` is the top-level `repo:` value in `forest.yaml`; never hardcode the repository name.
- `<agent-identity>` is the filing agent (`${POWDER_AGENT:-critic}`).
- `<instance>` is the observed deployment identity and `<observed-binary-revision>` is the running revision reported by `forest version`; use `unknown` only when no binary is present.
- `<slug>` is a short unique id, for example `if-critic-<topic>`.
- The note must carry the concrete `file:line` evidence. No evidence, no file.
- Never pass `--spec` to `powder create`; a spec would make the job takeable.
- Never edit a file, run `forest publish`, `git commit`, or `git push`.

## 5. Report

Summarize the sweep: the surfaces checked, the findings filed (id and
`file:line`), the findings skipped as duplicates, and the findings discarded
for lack of evidence. If nothing was filed, say so and list the evidence you
checked.
