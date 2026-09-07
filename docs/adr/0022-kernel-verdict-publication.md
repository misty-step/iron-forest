# 0022 — Kernel-owned verdict publication

Status: accepted, 2026-08-17

> Amended 2026-09-07: approve enforces the request, passing submitted Checks,
> exact configured Check order, request-branch tip, and request-ref lease.
> Both Verdict kinds require live Verifier context before retry handling and
> immediately before publication.

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

- The Runner's `FOREST_RUN_ID` must match a valid live Verifier record in the
  Git worktree's owning primary checkout, not an owner supplied by
  `FOREST_ROOT`. Resolve linked worktrees to that owner and recheck ownership
  after configured Checks, immediately before pushing. Refuse missing,
  malformed, ended, replaced, or misattributed context before any remote write.
- Payloads are `forest.checks.v1` and `forest.verdict.v1` for one SHA.
- Evidence is create-only:
  `refs/forest/v1/checks/<sha>` and `refs/forest/v1/verdict/<sha>`.
- Each ref is a commit. The tree is one JSON file. The committer is
  `Iron Forest Verifier <verifier@forest.invalid>`.
- `changes`: one atomic push of the two refs. `master` does not move.
- `approve`: fetch and validate the Builder or Fixer request, require its
  branch tip to equal the Revision, require every submitted Checks result to
  pass, and require the submitted names to equal the `forest.yaml` Check names
  at that Revision in the same order. Run the Kernel-owned credential scan and
  those configured Checks, then make one atomic push of the Checks and Verdict
  refs plus `sha:refs/heads/master`. The validated request OID participates as
  a no-op refspec with its exact `--force-with-lease`; the publisher does not
  replace the request commit. One attempt; the primary update is a fast-forward.
- A byte-identical remote pair is success only with valid live context, without
  another push. Any other existing ref, including an incomplete pair, is conflict.
- A non-fast-forward `master` or request lease conflict rejects the whole push.

The Verifier still decides approve versus changes and writes the files. It
does not push `master`.
The live record is operational validation, not proof of isolation from another
process with the same user's filesystem and Git authority.

Poll and Auditor read `refs/forest/v1/*` (#279). Leftover notes are unread.
The Verifier prompt calls `forest publish verdict`.

## Consequences

One Kernel publish family covers every Effect. Review taste stays in the
agent. Old factory notes no longer fail Poll or Audit.

