# 0014 — Builder, Verifier, Fixer, and Sentinel roster

Status: accepted, 2026-08-10

Amended 2026-08-21: the roster gains Sentinel. "No fourth declaration"
referred to a policy Manager, not to post-merge live QA.

## Context

The default profile needs a small roster with clear branch ownership and no
central policy agent. Humans groom the Tracker backlog. Any agent can file an
Issue when it finds a problem.

## Decision

The shipped roster is Builder, Verifier, Fixer, and Sentinel. There is no
Manager agent. Sentinel is post-merge live QA, distinct from Verifier's
pre-merge review: different inputs, surfaces, skills, and failure semantics.
Polls are disjoint.

The Builder creates `forest/<subject>/<slug>` and does not return to that branch
after its initial Revision. The Verifier reviews the exact Revision and owns
the approving merge. When the Verdict is `changes`, the Fixer owns the branch,
creates a repaired Revision, and writes a fresh
`refs/notes/forest/review-request` note. That fresh note is the reject handoff
that re-wakes the Verifier. Writer sets remain Builder and Fixer for review
requests and Verifier for Checks and Verdicts.

## Consequences

Each branch has one active repair owner at a time. The new Revision receives
fresh write-once evidence, so old Verdicts cannot authorize a changed branch.
Humans retain backlog grooming without a policy Manager declaration.

A stalled or ambiguous branch needs operator attention. No central agent hides
that condition by changing labels or selecting work for another declaration.
