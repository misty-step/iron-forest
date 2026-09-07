# 0014 — Builder, Verifier, and Fixer roster

Status: accepted, 2026-08-10

Amended 2026-08-21: the shipped review roster remains Builder, Verifier, and
Fixer. Sentinel is not a shipped declaration. Critic is a shipped drafts-only,
read-only declaration that never edits code or joins the review loop. Tester
is a shipped drafts-only cartographer declaration that files test-work Powder
drafts and never edits code, promotes work, or joins the review loop.

Amended 2026-09-02: Critic and Tester are promoted into the default profile.
They are non-review, drafts-only roles that produce attributed spec-less
Powder drafts and never edit code, publish to Git, promote backlog jobs, or
add Kernel Effects. Builder, Verifier, and Fixer remain the review and Gate
roster. Promotion evidence: `evals/jobs/fast/fast-20260901T224519Z/report.md`
is 22/22, and settled Runs `1788301018846047029-critic` and
`1788301018844450077-tester` each produced one attributed spec-less draft.

## Context

The default profile needs a small roster with clear branch ownership and no
central policy agent. Humans groom the Tracker backlog. Any agent can file an
Issue when it finds a problem.

## Decision

The shipped review roster is Builder, Verifier, and Fixer. There is no
Manager agent and no Sentinel declaration. Critic and Tester are default-
profile, non-review, drafts-only declarations: Critic sweeps the codebase and
files Powder drafts; Tester maps under-tested observable behaviors and files
test-work Powder drafts. Neither edits code, publishes to Git, promotes
backlog jobs, adds Kernel Effects, or participates in review. Polls are
disjoint.

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
agent, and no shipped Sentinel). Critic and Tester are default-profile,
non-review, drafts-only roles: Critic files drafts-only findings and never
grooms, selects, or edits work; Tester files drafts-only test-work findings and
never grooms, selects, or tests work directly. Neither publishes to Git,
promotes backlog jobs, or adds Kernel Effects. Their Polls skip cleanly when
Powder is not configured.

A stalled or ambiguous branch needs operator attention. No central agent hides
that condition by changing labels or selecting work for another declaration.
