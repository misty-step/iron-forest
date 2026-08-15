# 0021 — Kernel-owned review-request publication

Status: accepted, 2026-08-15

Extends [0010](0010-agent-owned-effects-and-merge-gate.md) and
[0017](0017-eval-driven-design.md).

## Context

ADR 0010 left Builder and Fixer publication to prompted `git`. The 2026-08-15
eval baseline failed the Builder canonical-note race in 3/3 trials. The prompt
already stated the retry contract. The model did not execute it.

The race loop is a closed protocol: known start, known stop, known retry
predicate, known atomic refs. That is Kernel work. Agent judgment stays on
Issue selection, implementation, and whether a Check result is ready to
publish.

## Decision

`forest publish review-request` owns the write-once review-request note and
the paired branch push. It implements the ADR 0010 profile contract exactly:

- one payload file, one exact Revision, one `forest/<issue>-<slug>` branch
- private refs under `FOREST_RUN_ID`
- `git notes add -F` only
- flat and fanout note paths by enumeration, never by derived SHA
- Builder: branch must be absent, or already at the Revision with an
  identical note
- Fixer: branch must remain at the rejected SHA, or already at the Revision
  with an identical note
- at most three attempts, and only when the canonical note ref changed
- never `--force`; never push the branch and note separately
- the atomic push compare-and-swaps both destination refs with
  `--force-with-lease=<ref>:<expected>`: notes at the snapshotted OID
  (empty when absent); Builder branch empty; Fixer branch the rejected SHA


The command also runs every configured `forest.yaml` Check at `HEAD`. A
nonzero exit refuses publication. That is the declared Check, not a
heuristic.

The command does not take the Kernel lock. A Run already holds that lock.

Agents still own the decision to call the command. They do not own the race
loop.

## Consequences

- Builder and Fixer prompts name this command instead of restating Git notes
  protocol.
- A later Verifier publication command can follow the same cut. It is not in
  this slice.
- Eval evidence that a remaining hole is judgment, not protocol, still
  belongs to the model or to a later ADR.
