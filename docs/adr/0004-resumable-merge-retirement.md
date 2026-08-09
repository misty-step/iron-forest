# 0004 — Merge retirement is a resumable transaction

Status: accepted, 2026-08-08

## Context

One merge crosses three systems. Git advances `master`, the Tracker closes the Item, and git deletes the source branch. A process can stop between any two effects.

The source branch cannot be the transaction record. A Host can delete it after merging. A squash merge also creates a new commit, so the reviewed Revision is not an ancestor of `master`.

Retrying the full merge is unsafe. The first merge can succeed before Tracker retirement fails. A restart must finish retirement without creating a second squash commit or repeating a Host merge.

## Decision

Store one remote retirement fact under `refs/forest/retirement/<encoded-branch>/<encoded-revision>`.

The fact records the branch, reviewed Revision, Item identity, transport, merge strategy, title, and state. Pending and landed facts also store Verifier attribution. Its content must match its ref key and branch-derived Item identity.

The native git path creates a `landed` fact in the same atomic compare-and-set push that advances `master`. The push also leases the target branch, source branch, and absent retirement ref.

The Host path creates a `pending` fact before publishing the durable approving Verdict. The fact is the ordering boundary: Verdict publication must not succeed until the exact retirement ref exists. When `AutoMerge=false`, Verifier preparation only observes the current Host state and never issues a merge command. When automatic merge is enabled, the fact remains pending until the Host reports that the pull request with the exact reviewed head merged. Recovery queries merged pull requests before open requests, so Host branch auto-deletion does not hide success. Pending recovery never creates a Projection; only initial publication or preparation may do so.

Every pending recovery revalidates the durable approving Verdict, passing Checks, and matching Verifier attribution before any Host effect. If preparation is incomplete or those facts do not match, recovery releases the pending fact and the subject becomes selectable again. A failed release retains the fact for retry; stale classification is terminal only after successful removal.

If the exact Host merge appears after branch loss but before approval is readable, the Verifier records an `observed` fact without attribution. This fact blocks Builder selection. Recovery refreshes durable notes and changes the fact directly to `landed` only after an approving Verdict and passing Checks exist. It takes attribution from that Verdict. Missing or non-approving evidence leaves the observation pending. A read failure retains it for repair.

The Verifier prioritizes retirement facts over ordinary branch work. Recovery deletes the exact reviewed branch when present, closes the Tracker Item, drops the attempt fact, and deletes the retirement fact. The fact excludes that Item from Builder selection until cleanup finishes. Each ref update uses compare-and-set.

## Consequences

A process restart can resume both merge paths from a fresh checkout without loading the current agent declaration. Pending and landed facts keep recovery tied to the Verifier that approved the merge. An observed fact has no attribution until that durable Verdict exists. A Tracker outage cannot cause a duplicate squash commit, Host merge, or Builder branch.

Remote retirement refs add one temporary durable object per in-flight merge. Invalid facts and duplicate branch or Item facts stop recovery instead of guessing.

Tracker closure is idempotent by contract. Source branch deletion tolerates absence because a Host can delete the branch. It still refuses to delete a branch that advanced past the reviewed Revision.
