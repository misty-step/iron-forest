# 0004 — Merge retirement is a resumable transaction

Status: accepted, 2026-08-08

## Context

One merge crosses three systems. Git advances `master`, the Tracker closes the Item, and git deletes the source branch. A process can stop between any two effects.

The source branch cannot be the transaction record. A Host can delete it after merging. A squash merge also creates a new commit, so the reviewed Revision is not an ancestor of `master`.

Retrying the full merge is unsafe. The first merge can succeed before Tracker retirement fails. A restart must finish retirement without creating a second squash commit or repeating a Host merge.

## Decision

Store one remote retirement fact under `refs/forest/retirement/<encoded-branch>/<encoded-revision>`.

The fact records the branch, reviewed Revision, Item identity, transport, merge strategy, title, and state. States progress as `preparing`, `pending`, `observed`, and `landed`; `preparing` and `observed` carry no Verifier attribution, while `pending` and `landed` store it. Its content must match its ref key and branch-derived Item identity.

The native git path creates a `landed` fact in the same atomic compare-and-set push that advances `master`. The push also leases the target branch, source branch, and absent retirement ref.

The Host path creates a `preparing` fact before its Projection can exist. A live exact branch with this fact reaches the Verifier under its retirement Subject, which resumes normal Checks, review, or repair while the fact excludes duplicate Builder work. After the exact Checks and winning approving Verdict are durable, the Verifier changes the fact to `pending`. A crash between these steps leaves recoverable identity, not an unowned Host effect.

When `AutoMerge=false`, Verifier preparation first records `preparing`, then completes the approval path without issuing a merge command. When automatic merge is enabled, the `pending` fact remains until Host reports that the exact reviewed head merged. Recovery queries merged pull requests before open requests, so Host branch auto-deletion does not hide success. `preparing` recovery may create or reconcile its missing initial Projection; `pending`, `observed`, and `landed` recovery never creates a new Projection.

Active Host recovery covers `preparing`, `pending`, and `observed` facts. It refreshes the durable Verdict and Checks and inspects exact Host state before resetting an incomplete approval. A visible open request with non-approving evidence returns the fact to `preparing`, preserving Builder exclusion while the branch returns to normal Verifier or Fixer work. Missing Host visibility retains the current fact. A durable approval with different attribution replaces the preparation attribution with the winning Verdict. Native `landed` recovery uses its recorded attribution.

An exact Host merge first advances a `preparing` or `pending` fact to `observed` before approval-note read. A read failure then retains `observed`, not the prior state. Recovery advances `observed` to `landed` only after an approving Verdict and passing Checks exist. Missing or non-approving evidence leaves `observed` retryable.

The Verifier prioritizes `preparing`, `pending`, and `observed` facts over ordinary branch work. A `preparing` fact with a live exact branch runs under the retirement Subject through normal Checks, review, or repair. If the branch advances, the Verifier atomically moves the preparation fact before retrying on the next pass. Recovery deletes the exact reviewed branch when present, closes the Tracker Item, drops the attempt fact, and deletes the retirement fact. Each ref update uses compare-and-set.

## Consequences

A process restart can resume both merge paths from a fresh checkout without loading the current agent declaration. The `preparing` state preserves Host identity before approval; `pending` ties recovery to the durable winning Verdict; `observed` retains the exact Host merge until approval; `landed` records completion. A Tracker outage or temporary Host no-view response cannot cause a duplicate squash commit, Host merge, or Builder branch.

Remote retirement refs add one temporary durable object per in-flight Projection or merge. Invalid facts and duplicate branch or Item facts stop recovery instead of guessing.

Tracker closure is idempotent by contract. Source branch deletion tolerates absence because a Host can delete the branch. It still refuses to delete a branch that advanced past the reviewed Revision.
