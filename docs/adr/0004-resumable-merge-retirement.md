# 0004 — Merge retirement is a resumable transaction

Status: accepted, 2026-08-08

## Context

One merge crosses three systems. Git advances `master`, the Tracker closes the Item, and git deletes the source branch. A process can stop between any two effects.

The source branch cannot be the transaction record. A Host can delete it after merging. A squash merge also creates a new commit, so the reviewed Revision is not an ancestor of `master`.

Retrying the full merge is unsafe. The first merge can succeed before Tracker retirement fails. A restart must finish retirement without creating a second squash commit or repeating a Host merge.

## Decision

Store one remote retirement fact under `refs/forest/retirement/<encoded-branch>/<encoded-revision>`.

The fact records the branch, reviewed Revision, Item identity, transport, merge strategy, title, state, and Builder-comment completion. Valid states are `preparing`, `pending`, `observed`, and `landed`. `preparing` and `observed` carry no Verifier attribution. `pending` and `landed` store it. The content must match its ref key and branch-derived Item identity.

The native git path creates a `landed` fact in the same atomic compare-and-set push that advances `master`. The push also leases the target branch, source branch, and absent retirement ref.

The Host path publishes or reconciles the Revision-marked Builder comment before it creates a `preparing` fact. A live exact branch with this fact reaches the Verifier under its retirement Subject, which resumes normal Checks, review, or repair while the fact excludes duplicate Builder work. Legacy facts without the completion field recover the comment before they advance or complete cleanup. After the exact Checks and winning approving Verdict are durable, the Verifier changes the fact to `pending`.

When `AutoMerge=false`, Verifier preparation records `preparing` and completes approval without a merge request. With automatic merge, one durable claim precedes the Host request. A successful command records acceptance separately. Recovery observes that request without issuing it again. An uncertain request outcome becomes an operator handoff. `pending`, `observed`, and `landed` recovery never creates a new Projection.

Active Host recovery covers `preparing`, `pending`, and `observed` facts. It refreshes the durable Verdict and Checks and inspects exact Host state before resetting an incomplete approval. A visible open request with non-approving evidence returns the fact to `preparing`, preserving Builder exclusion while the branch returns to normal Verifier or Fixer work. Missing Host visibility retains the current fact. A durable approval with different attribution replaces the preparation attribution with the winning Verdict. Native `landed` recovery uses its recorded attribution.

A Host merge discovered by inspection first records or advances an `observed` fact. This durable fact precedes missing Builder-comment recovery and approval-note read. Recovery records comment completion before Tracker cleanup. A read failure retains `observed`, not the prior state. Recovery advances `observed` to `landed` only after an approving Verdict and passing Checks exist. A Verifier-confirmed merge request advances `pending` directly to `landed`.

The Verifier schedules mergeable and fresh branches before active retirement recovery. Each lane also rotates selection after its prior Subject. Retirement facts still exclude duplicate Builder work. A `preparing` fact with a live exact branch resumes Checks, review, or repair. If that branch advances, the Verifier atomically moves the preparation fact. Recovery deletes the exact reviewed branch and closes the Tracker Item. It then deletes the retirement fact, attempt record, Subject brakes, and Effect claims in one atomic compare-and-delete transaction.

## Consequences

A process restart can resume both merge paths from a fresh checkout without loading the current agent declaration. The `preparing` state preserves Host identity before approval; `pending` ties recovery to the durable winning Verdict; `observed` retains the exact Host merge until approval; `landed` records completion. A Tracker outage or temporary Host no-view response cannot cause a duplicate squash commit, Host merge, or Builder branch.

Retirement refs are temporary. Effect claims remain replay fences while an external outcome is uncertain. Completed retirement removes its claims atomically. Invalid facts and duplicate branch or Item identities quarantine their known Subjects without suppressing unrelated work.

Tracker closure uses separate request and acceptance claims. A failed close retries only after exact `open` evidence. Malformed or uncertain evidence stops recovery. Source branch deletion tolerates absence because a Host can delete the branch. It still refuses an advanced branch. A terminal brake keeps that head out of the Verifier because a second fact for the same branch or Item would invalidate both facts. An operator must reconcile the old fact first.
