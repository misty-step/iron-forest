# 0023 — One Subject identity

Status: accepted, 2026-08-19 (review-request note write retired by 0028, 2026-09-01)

> Retired in part by [0028](0028-review-request-notes-retired.md): the
> review-request note described below no longer carries request evidence.
> Request evidence is only the create-only `refs/forest/v1/request/<sha>`
> commit. Decision text retained as historical record.

Extends [0009](0009-git-coordination-authority.md),
[0010](0010-agent-owned-effects-and-merge-gate.md), and
[0012](0012-poll-trigger-protocol.md). Destination store:
[VISION.md](../../VISION.md).

## Context

Powder job ids are slugs. Schema v1 stored `"issue": <n>` and accepted only
`forest/<positive-integer>-<slug>`. That cannot name a Powder job.

A Tracker adapter interface remains out of product. Two Git dialects for the
same factory work are also out.

## Decision

Git stores one Subject. New review-request evidence is:

```json
{"schema":"forest.review-request.v2","subject":"<id>","branch":"...","revision":"<sha>","time":"<rfc3339>","tracker":"github|powder"}
```

`<subject>` is a Powder slug: `[A-Za-z0-9][A-Za-z0-9._-]{0,127}`. A GitHub
Issue number is that decimal string. `tracker` is the source actually selected,
never inferred from the id. Publication requires `github` or `powder`.
Historical v2 evidence without `tracker` stays Gate-readable and is never
Powder-completion-eligible. The branch is only
`forest/<subject>/<slug>`. `<slug>` keeps the old work-slug rule. The slash
is required because subjects contain hyphens.

Schema v1 and `forest/<n>-<slug>` are invalid. Leftover hyphen tips are unread
by Poll. They do not make Verifier or Fixer unhealthy.

Builder Poll lists two programs and asks one question: does this repository
have an unclaimed Subject?

- an open `forest:ready` Issue with no `forest/<n>/*` branch; or
- a Powder job for `forest.yaml` `repo` that is takeable or held by
  `POWDER_AGENT`, with no `forest/<id>/*` branch.

Powder listing runs only when `POWDER_AGENT` is set. Origin is `POWDER_URL`
or `POWDER_API_BASE_URL`. Agent set and origin missing is Poll exit 2.

Builder takes a Powder job before creating its branch. A Fixer confirms or
idempotently re-takes that same Subject before mutating a rejected branch.
Agents may release only a failed or unpublished Builder attempt.

After an approve Gate, the Kernel owns terminal completion for a request
with `tracker: powder`. It reads current primary and that Revision's exact
request and approve refs, then runs `powder show`, an idempotent
same-identity `take`, and `done --proof <revision>`. It retries at Builder
Poll and before a later approve. `POWDER_AGENT` is one identity per
repository Kernel.

## Consequences

GitHub and Powder still share one Subject grammar and one branch shape.
There is no adapter type and no second Git dialect. List and claim stay
tracker-specific. The review-request note records the source actually
selected as `tracker`. Kernel Powder completion reads that field; it does
not infer tracker from the Subject id.

Powder's `done` requires the completing identity to hold the live lease. The
Builder therefore keeps the lease after request publication. Fixer and Kernel
may re-take only that same Subject under the same repository identity.

Current primary is one bounded pending-completion slot. A later approve cannot
replace it, and Builder cannot dispatch, until its Powder job is terminal.
Failure after the atomic approve push is projection lag: the Gate remains
successful and the next Kernel boundary retries. If Powder `show` returns
`not_found` for the current Subject, Builder dispatch and later approves block
until the job exists again or primary carries a later request; a Gate that
already landed reports `powder_status: pending`. Recovery reads no historical
ref set or board list and stores no outbox. The Kernel probes Powder only when
the current request has `tracker: powder`. A GitHub or undiscriminated
historical request never calls `show`, `take`, or `done`, even if a Powder job
shares that id. A Fixer repairing a trackerless historical request does not
probe Powder and publishes `tracker: github` because that Run did not claim a
Powder job. Every accepted terminal observation requires `proof` equal to
the landed Revision.
