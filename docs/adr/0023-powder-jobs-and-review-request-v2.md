# 0023 — One Subject identity

Status: accepted, 2026-08-19

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

Git stores one Subject. The review-request note is only:

```json
{"schema":"forest.review-request.v2","subject":"<id>","branch":"...","revision":"<sha>","time":"<rfc3339>"}
```

`<subject>` is a Powder slug: `[A-Za-z0-9][A-Za-z0-9._-]{0,127}`. A GitHub
Issue number is that decimal string. The branch is only
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

The Kernel does not `take`, `release`, `ask`, or `done`. Builder takes before
creating a branch. Verifier calls `powder done` after a successful approve
when `powder show <subject>` names a non-terminal job for this repository.
Fixer reuses the selected `subject` and branch.

`POWDER_AGENT` is one identity per Kernel.

## Consequences

GitHub and Powder differ only at the list/claim programs. Publish, Poll,
Auditor, and prompts share one note and one branch grammar. There is no
adapter type.
