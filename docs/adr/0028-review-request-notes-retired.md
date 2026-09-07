# 0028 — Review-request notes retired

Status: accepted, 2026-09-01

Extends [0021](0021-kernel-review-request-publication.md),
[0022](0022-kernel-verdict-publication.md), and
[0023](0023-powder-jobs-and-review-request-v2.md). Destination store:
[VISION.md](../../VISION.md).

## Context

ADR 0021 moved Builder and Fixer publication into the Kernel but kept a dual
write: a Git note on `refs/notes/forest/review-request` beside the create-only
request evidence ref `refs/forest/v1/request/<sha>`. The note was the
cutover-era surface. No Kernel reader consumes it: Poll and Auditor read
`refs/forest/v1/*`, and Verifier publication (ADR 0022) already writes only
evidence refs. The note write is the only reason request publication carries a
canonical-note race loop, run-private note refs, and note-tree rebuild helpers.

## Decision

`forest publish review-request` writes only create-only request evidence. It
builds the `request.json` evidence commit exactly like Verdict publication,
then pushes the branch and the request ref in one atomic
`--force-with-lease` push.

- Builder publication expects the branch absent, then CAS-creates both
  `refs/heads/forest/<subject>/<slug>` and `refs/forest/v1/request/<sha>`.
- Fixer publication expects the branch to remain at the rejected SHA, then
  advances it and creates the fresh request ref in the same push.
- A branch already at the Revision with byte-identical request evidence is
  success. A differing request evidence ref is conflict.
- The canonical-note race loop, run-private note refs, and note helpers are
  deleted.

Existing `refs/notes/forest/*` refs stay unread and unrewritten.

## Consequences

- Request and Verdict publication now share one primitive: commit evidence,
  one atomic force-with-lease push, no retries.
- ADR 0021 and ADR 0023 decision text remains as historical record; only the
  review-request note write they described is retired.
- Operators inspect request evidence with
  `git fetch origin refs/forest/v1/request/<sha>` then
  `git show FETCH_HEAD:request.json`.
