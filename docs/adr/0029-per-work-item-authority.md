# 0029 — Per-work-item authority

Status: accepted, 2026-09-12

Extends [0022](0022-kernel-verdict-publication.md). Destination store:
[README](../../README.md#merge-gate).

## Context

A profile's delivery mode cannot distinguish work agents may land from work
that must await operator review. A restriction attached only to a Builder Run
would disappear when a separate Verifier Run omitted it.

## Decision

`forest.request.v1` accepts optional `authority: land|review`. Absence retains
existing profile delivery semantics; explicit empty, null and other values are
invalid. Retain authority in Run requests, live evidence and Ledger rows, and
expose it in human/JSON Run surfaces.

Bind the same authority into create-only `forest.review-request.v3` candidate
evidence. Publication checks it against the originating live Run and retained
request. A Fixer cannot elevate a rejected review candidate.

Approval uses the more restrictive authority of candidate and approving Run;
absence on either side means the profile default. Either `review` means publish
exact Checks and approve Verdict under the existing atomic leased-ref protocol,
return exit 0 with `status: review-only`, and never update or push primary.
Identical retries retain this result. Both absent preserve current behavior;
`land` never overrides external delivery. Existing checks, independent review,
revision binding and fast-forward requirements remain mandatory.

Local Linear profiles map `Agent: Land` to land and `Agent: Review` to review.
Legacy `Agent: Ready` maps to review; review wins ambiguous labels. `Operator
Action` is always excluded. Review-only verification opens a PR after evidence
publication, never merges it, and never marks the tracker item done.

## Consequences

The Kernel enforces the primary-update boundary even when later Runs omit or
increase authority. PR creation remains profile policy, not Kernel orchestration.
This is an operational guard, not a security sandbox against processes sharing
the operator's filesystem and Git credentials. No scheduler, tracker or
coordination layer is added.
