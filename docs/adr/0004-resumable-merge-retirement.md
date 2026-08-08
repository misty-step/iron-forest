# 0004 — Merge retirement is a resumable transaction

Status: accepted, 2026-08-08

## Context

One merge crosses three systems. Git advances `master`, the Tracker closes the Item, and git deletes the source branch. A process can stop between any two effects.

The source branch cannot be the transaction record. A Host can delete it after merging. A squash merge also creates a new commit, so the reviewed Revision is not an ancestor of `master`.

Retrying the full merge is unsafe. The first merge can succeed before Tracker retirement fails. A restart must finish retirement without creating a second squash commit or repeating a Host merge.

## Decision

Store one remote retirement fact under `refs/forest/retirement/<encoded-branch>/<encoded-revision>`.

The fact records the branch, reviewed Revision, Item identity, transport, merge strategy, title, state, and Verifier attribution. Its content must match its ref key and branch-derived Item identity.

The native git path creates a `landed` fact in the same atomic compare-and-set push that advances `master`. The push also leases the target branch, source branch, and absent retirement ref.

The Host path creates a `pending` fact before requesting the merge. It promotes the fact to `landed` after the Host reports that the pull request with the exact reviewed head merged. Recovery queries merged pull requests before open requests, so Host branch auto-deletion does not hide success.

The Verifier prioritizes retirement facts over ordinary branch work. Recovery deletes the exact reviewed branch when present, closes the Tracker Item, drops the attempt fact, and deletes the retirement fact. The fact excludes that Item from Builder selection until cleanup finishes. Each ref update uses compare-and-set.

## Consequences

A process restart can resume both merge paths from a fresh checkout without loading the current agent declaration. The stored attribution keeps the recovery Run tied to the Verifier that initiated the merge. A Tracker outage cannot cause a duplicate squash commit, Host merge, or Builder branch.

Remote retirement refs add one temporary durable object per in-flight merge. Invalid facts and duplicate branch or Item facts stop recovery instead of guessing.

Tracker closure is idempotent by contract. Source branch deletion tolerates absence because a Host can delete the branch. It still refuses to delete a branch that advanced past the reviewed Revision.
