# 0014 — Builder, Verifier, and Fixer roster

Status: accepted, 2026-08-10

Amended 2026-08-21: the shipped review roster remains Builder, Verifier, and
Fixer. Sentinel is not a shipped declaration. Critic is a shipped drafts-only,
read-only declaration that never edits code or joins the review loop. Tester
is a shipped drafts-only cartographer declaration that files test-work Powder
drafts and never edits code, promotes work, or joins the review loop.

Amended 2026-08-22: Critic and Tester are EXPERIMENTAL and
local-canary-only. They remain enabled only in the self-host Iron Forest
checkout for canary observation; external operators must not copy or enable
them. Rollout exit gate: the blocking repair jobs
`if-investigator-provenance-contract`, `if-eval-powder-mutations`,
`if-tester-eval-observable-surface`, `if-eval-draft-note-binding`, and
`if-investigator-powder-availability` are merged; the corrected deterministic
evals pass; and one post-fix live sweep per role produces attributable
spec-less drafts.

## Context

The default profile needs a small roster with clear branch ownership and no
central policy agent. Humans groom the Tracker backlog. Any agent can file an
Issue when it finds a problem.

## Decision

The shipped review roster is Builder, Verifier, and Fixer. There is no
Manager agent and no Sentinel declaration. Critic and Tester are EXPERIMENTAL
and local-canary-only, enabled only in the self-host Iron Forest checkout for
canary observation. Critic is a drafts-only sweep declaration; it files Powder
drafts and never edits code or participates in review. Tester is a
drafts-only cartography declaration; it files test-work Powder drafts and
never edits code or participates in review. Polls are disjoint.

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
agent, and no shipped Sentinel). Critic and Tester are local-canary-only:
external operators do not copy or enable them until the rollout exit gate
closes (the five blocking repair jobs are merged, the corrected deterministic
evals pass, and one post-fix live sweep per role produces attributable
spec-less drafts). Critic files drafts-only findings and never grooms or
selects work. Tester files drafts-only test-work findings and never grooms,
selects, or tests work directly.

A stalled or ambiguous branch needs operator attention. No central agent hides
that condition by changing labels or selecting work for another declaration.
