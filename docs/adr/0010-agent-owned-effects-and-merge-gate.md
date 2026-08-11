# 0010 — Agent-owned Effects and the merge Gate

Status: accepted, 2026-08-10

## Context

The Kernel can schedule and observe work without owning the decisions that
agents make. Agents have the context to select Subjects, write evidence, push
branches, and merge reviewed work. Moving those Effects into the Kernel would
create bespoke policy and a second coordination authority.

## Decision

Agents own coordination Effects through prompts and native `git`; no bespoke
wrapper tool mediates note writes or merges. The Verifier runs configured
Checks, reviews the exact Revision, writes Checks and Verdict notes, and on
approval performs the merge.

The profile Gate contract is fixed:

1. Exactly one valid Builder-or-Fixer review-request binds to the reviewed SHA.
2. Passing Checks and the approve Verdict bind to that same SHA.
3. These three notes and a fast-forward of `master` to that SHA form the Gate.
4. The existing review-request remains durable and is not republished. Checks,
   Verdict, and the target advance use one atomic push:
   ```sh
   git push --atomic origin \
     "$checks_private:refs/notes/forest/checks" \
     "$verdict_private:refs/notes/forest/verdict" \
     "$revision:refs/heads/master"
   ```
5. No force flag is allowed. A non-fast-forward rejection is the compare-and-set
   failure.
6. Builder and Fixer publish their branch and review-request note through one
   normal `git push --atomic`. A canonical note race permits at most three total
   attempts.
7. For a `changes` Verdict, the Verifier publishes Checks and Verdict through
   one atomic push without advancing `master`. A canonical note race permits at
   most three total attempts.
8. For `approve`, the Verifier makes exactly one non-retryable atomic attempt
   with Checks, Verdict, and the exact `master` advance.

The Kernel does not enforce this profile contract. The first observed remote
`master` tip becomes a trusted baseline and is not Gate-checked. After a
completed dispatch, its read-only Auditor checks each snapshotted note entry's
schema and actor, including entries in the baseline snapshot. Ancestry and Gate
checks target only the final observed remote `master` tip. These checks cover
observable final Git state only. The Auditor does not prove check execution,
atomic push ordering, or force absence. It reports violations and does not
block or authorize a merge.

The actor assignment is provisional pending evals comparing prompted agents,
bespoke tools, and Kernel-owned Effects.

## Accepted risks

Neither client code nor the Auditor can prove remote push ordering or prove that
a force flag was absent after the remote accepts a push. Schema-valid passing
results do not prove that the Verifier ran the commands. A trusted declaration
has the operating-system user's configured credentials, filesystem access, and
network access, so worktree separation and time bounds are not a security
boundary. Stronger credential and process containment belongs to deployment.
Upgrade paths are a Kernel-owned final push, server hooks, forge rules, or
deployment isolation. Those paths are not implemented in this slice.

## Consequences

The profile owns workflow intent and durable Effects. The Kernel remains small
and deterministic. The merge decision is inspectable from notes and Git
history, with explicit residual risks instead of hidden assumptions.

A bad prompt can request a bad Effect. Independent Verifier review and the
read-only Auditor can detect an observable invalid final state. They cannot
detect a schema-valid lie about command execution or reconstruct publication
order. The pending actor-assignment eval decides whether to move any boundary.
