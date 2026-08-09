# 0007 — Flow recovery is Subject-local and paced

Status: accepted, 2026-08-08

## Context

A Fixer can publish a repair before it records the attempt. A stop in that window can exceed the limit or lose the human handoff.

One malformed Tracker Item, branch identity, retirement fact, or durable brake previously aborted a complete Selector. Unrelated valid Subjects then stopped behind one damaged record.

Two unchanged pending Subjects can alternate successfully. Cursor rotation preserves fairness, but immediate reselection still creates an unbounded remote-read loop.

## Decision

A Fixer publishes its branch Revision and attempt record in one atomic compare-and-set push. Both refs advance, or neither ref advances.

An exhausted attempt record selects its branch even when the new Revision has no Verdict or Checks. The Fixer completes the `forest:failed` handoff before it stops that Subject.

Selectors split valid Tracker, remote branch, and durable evidence from invalid evidence with known identity. Each invalid entry becomes one failing Subject with its own brake.

Transport failure for a complete source read still fails that Flow pass. Unknown identity never authorizes an Effect against a guessed Subject.

Every Flow pass waits for its configured interval. The cursor still resumes after the prior Subject on the next pass.

## Consequences

A process stop cannot separate a published Fixer Revision from its attempt count. Restart can finish the exhausted handoff without another agent run.

Malformed evidence stops only its known Subject. Builder, Verifier, Fixer, and Manager continue with unrelated valid Subjects.

Pending outcomes keep bounded poll pressure. The configured interval now applies after both changed and unchanged pass results.
