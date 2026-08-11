# 0014 — Builder, Verifier, and Fixer roster

Status: accepted, 2026-08-10

## Context

The default profile needs a small roster with clear branch ownership and no
central policy agent. Humans groom the Tracker backlog. Any agent can file an
Issue when it finds a problem.

## Decision

The shipped roster is Builder, Verifier, and Fixer. There is no Manager agent.
Polls are disjoint.

The Builder creates `forest/<issue>-<slug>` and does not return to that branch
after its initial Revision. The Verifier reviews the exact Revision and owns
the approving merge. When the Verdict is `changes`, the Fixer owns the branch,
creates a repaired Revision, and writes a fresh
`refs/notes/forest/review-request` note. That fresh note is the reject handoff
that re-wakes the Verifier. Writer sets remain Builder and Fixer for review
requests and Verifier for Checks and Verdicts.

## Consequences

Each branch has one active repair owner at a time. The new Revision receives
fresh write-once evidence, so old Verdicts cannot authorize a changed branch.
Humans retain backlog grooming without a fourth declaration.

A stalled or ambiguous branch needs operator attention. No central agent hides
that condition by changing labels or selecting work for another declaration.
