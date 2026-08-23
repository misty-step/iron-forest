# 0026 — Human direct push audit policy

Status: accepted, 2026-08-23

Extends [0005](0005-agent-commit-identities.md),
[0010](0010-agent-owned-effects-and-merge-gate.md), and
[0011](0011-kernel-profile-boundary.md).

## Context

The operator owns the host, credentials, backlog labels, and Powder jobs.
Operators can therefore push directly to `master` without a Builder Revision or
Gate evidence. The Auditor's Gate check treats every non-baseline `master` tip
as a factory Revision, so a direct human push produced the same violation as a
broken agent merge:

> `master <sha> does not have exactly one valid review-request note`

That violation re-appeared at each new human tip and cleared on the next
factory merge, which made it noise rather than a signal about the factory
invariant.

## Decision

The Auditor distinguishes an operator direct push from a factory-authored
Revision using only observable Git state:

1. If any evidence ref in `refs/forest/v1/{request,checks,verdict}/<sha>`
   targets the observed `master` tip, the tip is factory-involved and receives
   the full Gate check.
2. Otherwise the Auditor reads the tip commit's author identity. If the author
   is not one of the shipped declaration identities
   (`Iron Forest Builder <builder@forest.invalid>`,
   `Iron Forest Fixer <fixer@forest.invalid>`,
   `Iron Forest Verifier <verifier@forest.invalid>`), the tip is an operator
   direct push and is not a Gate violation.
3. A tip whose author is an agent identity still receives the full Gate check
   even when no evidence targets it. Factory-authored content cannot land on
   `master` without review evidence.

The classification is fail-closed: an operator commit that carries any factory
evidence, and any agent-authored commit, keep the Gate in force.

## Consequences

- A documented human direct push produces no Auditor violation. The Auditor
  does not flag operator-owned `master` movement as a broken factory merge.
- The factory invariant is unchanged for factory work: Builder- and
  Fixer-authored Revisions must still carry request, Checks, and approve
  Verdict evidence and a fast-forward of `master`.
- The rule is testable from repo state alone: evidence refs plus the tip
  commit author are enough to classify a tip without any workflow database or
  forge API.
- The Auditor remains read-only and does not enforce the policy. An operator
  who pushes factory-authored content directly still gets a violation.
