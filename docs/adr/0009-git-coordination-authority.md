# 0009 — Git coordination authority and schema v1

Status: accepted, 2026-08-10

## Context

Workflow state needs one durable authority that agents and the Kernel can read
from separate processes. Forge review APIs cannot provide that authority. Git
branches, commits, and notes already provide immutable Revisions and compare-
and-set pushes.

GitHub returns `422 can not request changes on your own pull request` when an
actor attempts that operation. Review coordination therefore must not depend on
forge review APIs. A pull request is a human Projection only.

## Decision

Git is the coordination authority. Workflow state uses branch names and
`refs/notes/forest/*`. The schema is versioned and binds evidence to exact
Revisions:

- `refs/notes/forest/review-request` used
  `forest.review-request.v1` with `issue`, `branch`, `revision`, and `time`.
  [0023](0023-powder-jobs-and-review-request-v2.md) replaces that with
  `forest.review-request.v2`, `subject`, and write-required `tracker`.
- `refs/notes/forest/checks` uses `forest.checks.v1` with `revision`, `results`,
  and `time`.
- `refs/notes/forest/verdict` uses `forest.verdict.v1` with `revision`,
  `verdict`, `summary`, and `time`.

The writer sets are fixed: Builder and Fixer author review-request payloads;
Verifier writes Checks and Verdicts. Notes are write-once for each Revision.
Builder and Fixer call `forest publish review-request`, which stores the
complete JSON payload with `git notes --ref=<private-ref> add -F <payload-file>`,
never `-m` or `-f`. Each run-private notes ref starts from a remote canonical
snapshot fetched into a unique base ref. The command compares the exact
destination payload before every attempt. A byte-identical remote note is
success; a conflict fails closed.

The Kernel publishes the branch and review-request note with one normal
atomic push. A canonical notes-ref race may fetch a new stable snapshot, rebuild
the private ref, and retry at most three total attempts. A branch race, a
conflicting note, or an unchanged rejected ref stops; the branch and note are
never published through separate paths.

Verifier publishes Checks and a `changes` Verdict together with one atomic push.
A canonical note race permits at most three total atomic attempts for
`changes`. The approve Gate requires one valid Builder-or-Fixer review-request,
passing Checks, and an approving Verdict for the same exact Revision, plus its
fast-forward `master` advance. The Verifier makes exactly one non-retryable
approve attempt carrying Checks, Verdict, and that advance; the existing
review-request is not republished. A pull request remains a disposable
Projection.

The archived `overhaul/independent-flows` branch and its ADRs were reviewed and
were not adopted.

## Consequences

Git can explain coordination without a workflow database. Forge adapters can
publish or reconcile Projections without becoming authorities. Notes remain
append-only by Revision, and competing writers fail closed.

After a completed dispatch, the Kernel Auditor takes a bounded stable snapshot.
The first observed remote `master` tip becomes a trusted baseline and is not
Gate-checked. Every snapshot, including the baseline snapshot, checks each note
entry's payload and exact target-path actor within a 500-entry-per-ref
capacity bound. A ref beyond that bound, an enumeration or note-show transport
overflow, a payload above 64 KiB, or malformed or unresolvable canonical note
state (malformed list or tree rows, a listed note without its tree entry, a
mismatched, unexpected, or duplicate tree entry, a non-SHA path, a non-blob
entry, or a missing note object) is a bounded policy violation. Ancestry and
Gate checks target
only the final observed remote `master` tip. The Auditor reads observable final
Git state and cannot prove check execution, atomic push ordering, or force
absence. It reports violations but does not enforce the profile contract. A
forge outage delays adapter work but cannot change Git evidence.
