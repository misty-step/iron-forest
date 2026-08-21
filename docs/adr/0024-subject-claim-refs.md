# 0024 — Subject claim refs

Status: accepted, 2026-08-21

Extends [0009](0009-git-coordination-authority.md),
[0010](0010-agent-owned-effects-and-merge-gate.md),
[0015](0015-one-kernel-per-repository.md),
[0021](0021-kernel-review-request-publication.md), and
[0023](0023-powder-jobs-and-review-request-v2.md).

## Context

ADR 0015 runs exactly one Kernel per checkout and trusts the deployment
contract to forbid a second Kernel on another clone. When that contract is
violated, or when the factory is deliberately scaled to parallel Builders on
separate clones, two Kernels can select the same ready GitHub Issue and both
dispatch a Builder. The race resolves only at publication, after both Builders
have burned a full Run, when the create-only request ref and the branch-name
collision reject one of them.

Powder jobs already have an atomic cross-Kernel claim: `powder take`. A GitHub
Issue has no equivalent. Its `forest:ready` list is read-only, so nothing
arbitrates two Builders before either creates a branch.

## Decision

Add one create-only evidence ref for lease-less Subjects:

```text
refs/forest/v1/claim/<subject>
```

The ref is a commit. Its tree is one JSON file, `claim.json`:

```json
{"schema":"forest.claim.v1","subject":"<id>","revision":"<sha>","time":"<rfc3339>"}
```

`<subject>` is the Subject id defined by
[0023](0023-powder-jobs-and-review-request-v2.md): a GitHub Issue number as its
decimal string. `revision` is the primary tip the Builder resolved immediately
before claiming — the exact base it will branch from. The committer is
`Iron Forest Builder <builder@forest.invalid>`.

The Builder claims only a GitHub Issue Subject. A Powder Subject keeps the
`powder take` lease as its claim; no Git claim ref is written for it. The two
claim programs stay single-purpose: the Kernel effect guards lease-less GitHub
selection, and Powder keeps owning exclusive work for jobs.

The Builder claims immediately after selection and after resolving the base
revision, before creating the branch:

1. `git fetch origin`
2. resolve `base_sha`
3. write the claim payload
4. `forest publish claim <subject> <payload>`
5. create `forest/<subject>/<slug>` from `base_sha`

`forest publish claim` CAS-creates `refs/forest/v1/claim/<subject>` with
`--force-with-lease=<ref>:` expecting the ref to be absent. A byte-identical
existing claim by the Builder identity is success. Any other existing ref is a
conflict: the Builder stops cleanly. The forge arbitrates; there is no lock
service and no second coordination store.

The Builder Poll skips a GitHub Issue Subject when either `forest/<subject>/*`
or `refs/forest/v1/claim/<subject>` exists. Powder Subjects keep the existing
take/mine and branch checks.

## Stale and abandoned claims

A claim is durable and is not released on a normal verdict. After the branch
and request evidence exist, Poll already skips the Subject by branch, and the
claim ref remains read-only selection evidence exactly like the request, Checks,
and Verdict refs.

A claim becomes stale only when the Builder stops before publishing a branch
(crash, failed Check, or a stop before `forest publish review-request`). The
Subject then stays claimed and Poll skips it. The recovery path is an operator
CAS delete, never `--force`:

```sh
forest publish claim-release <subject>
```

`forest publish claim-release` reads the current claim OID and pushes a delete
of `refs/forest/v1/claim/<subject>` guarded by
`--force-with-lease=<ref>:<oid>`. If the ref moved, the lease fails and the
command stops. Before releasing, the operator confirms no in-flight Run owns the
Subject: no `forest/<subject>/*` branch and no live Builder Run for it. The same
command is the documented path for reopening a Subject whose durable claim would
otherwise keep Poll from selecting it again.

## Consequences

- Two Kernels on separate clones cannot both hold a claim for one GitHub
  Subject: the CAS create has one winner, and the loser gets a conflict and
  stops cleanly.
- A stale or abandoned claim has a documented, force-free recovery path.
- The claim does not authorize `master` movement. The Gate is unchanged:
  request, Checks, Verdict, and a fast-forward of `master`.
- The implementation behind this decision adds `forest publish claim` and
  `forest publish claim-release` to the Kernel publish family, adds
  `refs/forest/v1/claim/*` to Poll and Auditor enumeration with a claim decoder,
  and updates the Builder prompt. Eval tests cover the CAS-create race and the
  stale-release path.
