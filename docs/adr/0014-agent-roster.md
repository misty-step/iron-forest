# 0014 — Builder, Verifier, and Fixer roster

Status: accepted, 2026-08-10

Amended 2026-08-21: the shipped review roster remains Builder, Verifier, and
Fixer. Sentinel is not a shipped declaration. Critic is a shipped drafts-only,
read-only declaration that never edits code or joins the review loop.

## Context

The default profile needs a small roster with clear branch ownership and no
central policy agent. Humans groom the Tracker backlog. Any agent can file an
Issue when it finds a problem.

## Decision

The shipped review roster is Builder, Verifier, and Fixer. There is no
Manager agent and no Sentinel declaration. Critic is a separate drafts-only
sweep declaration; it files Powder drafts and never edits code or participates
in review. Polls are disjoint.

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
Humans retain backlog grooming without a policy declaration (no Manager
agent, and no shipped Sentinel). Critic files drafts-only findings and never
grooms or selects work.

A stalled or ambiguous branch needs operator attention. No central agent hides
that condition by changing labels or selecting work for another declaration.
