# 0022 — Kernel-owned verdict publication

Status: accepted, 2026-08-17

> Amended 2026-09-01: approve now enforces the request, submitted Checks,
> configured Check-name set, request-branch tip, and request-ref lease before
> moving the primary branch.

Extends [0010](0010-agent-owned-effects-and-merge-gate.md),
[0017](0017-eval-driven-design.md), and
[0021](0021-kernel-review-request-publication.md). Destination store:
[VISION.md](../../VISION.md).

## Context

ADR 0021 moved Builder and Fixer publication into the Kernel after the
2026-08-15 eval failed the prompted canonical-note race in 3/3 trials. The
Verifier still ran prompted `git notes` and `git push --atomic`.

Issue #238 compared that prompted Verifier to the Kernel-shaped oracle
(`evals/run-fast.sh` 2026-08-17, 18/18). Prompted publication mishandled a
conflicting destination and republished after a rejected approve Gate. The
oracle passed both closed loops. Approving a planted defect in 1/3 is judgment,
not protocol.

## Decision

`forest publish verdict <checks-file> <verdict-file>` owns Checks and Verdict
publication.

- Payloads are `forest.checks.v1` and `forest.verdict.v1` for one SHA.
- Evidence is create-only:
  `refs/forest/v1/checks/<sha>` and `refs/forest/v1/verdict/<sha>`.
- Each ref is a commit. The tree is one JSON file. The committer is
  `Iron Forest Verifier <verifier@forest.invalid>`.
- `changes`: one atomic push of the two refs. `master` does not move.
- `approve`: fetch and validate the Builder or Fixer request, require its
  branch tip to equal the Revision, require every submitted Checks result to
  pass, and require the submitted names to equal the `forest.yaml` Check names
  at that Revision. Run those configured Checks, then make one atomic push of
  the Checks and Verdict refs plus `sha:refs/heads/master`. The validated
  request OID participates as a no-op refspec guarded by its exact
  `--force-with-lease`; request content cannot change. One attempt; the primary
  update remains a fast-forward.
- A byte-identical remote pair is success. Any other existing ref is conflict.
- A non-fast-forward `master` or moved request ref rejects the whole push.

The Verifier still decides approve versus changes and writes the files. It
does not push `master`.

Poll and Auditor read `refs/forest/v1/*` (#279). Leftover notes are unread.
The Verifier prompt calls `forest publish verdict`.

## Consequences

One Kernel publish family covers every Effect. Review taste stays in the
agent. Old factory notes no longer fail Poll or Audit.

