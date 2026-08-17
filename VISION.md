# Vision

Iron Forest is a headless software factory. Exactly one live Kernel serves
one repository, on a machine the operator chooses. The Kernel is small and
deterministic. Agents judge.

This file is the product lock. Accepted ADRs state the contracts. GitHub
Issues track work. If a later plan disagrees with this file, change this file
first.

## Aim

An operator marks one Issue `forest:ready`. The factory implements it, reviews
the exact Revision, and either merges or returns a repair. The operator
watches Git and the CLI. The factory does not need a console, a second ledger,
or a vendor API.

## Philosophy

General methods that scale beat hand-built policy. Keep judgment in the model
and the prompt. Put only closed loops in the Kernel: known start, known stop,
known retry predicate, known atomic refs. Measure actor boundaries with evals
([ADR 0017](docs/adr/0017-eval-driven-design.md)). Do not encode review taste
in Go.

Delete a requirement that does not change an observable Git outcome.

## Domain

| Term | Meaning |
| --- | --- |
| Kernel | The `forest` process. It polls, isolates a Run, starts the harness, and owns closed Git protocols. |
| Declaration | One role: `agents/<name>/agent.md` and `task.md`. |
| Run | One dispatch of one declaration. At most one live Run per declaration. |
| Subject | One GitHub Issue that a Builder may select. |
| Revision | One commit SHA. Every evidence ref binds to it. |
| Tracker | GitHub Issues. Humans and agents file work there. |
| Projection | A GitHub pull request. Humans read it. It is not authority. |
| Gate | One valid request, passing Checks, an approve Verdict, and a fast-forward of `master` to that Revision. |
| Auditor | `forest status` and `forest audit show` reading evidence refs. It does not merge. |
| Host | The Linux machine that runs the Kernel. The operator chooses it. |

Do not add Manager, dashboard, adapter, substrate, or spend ledger as product
terms.

## Shape

```text
forest:ready Issue
  → Builder implements and calls forest publish request
  → Verifier checks, judges, and calls forest publish verdict
  → Fixer repairs a rejected Revision
```

The shipped roster is Builder, Verifier, and Fixer
([ADR 0014](docs/adr/0014-agent-roster.md)). There is no fourth declaration.
Each role is one event, one input, one isolated Run.

Git holds durable facts: branches, commits, and create-only evidence refs:

```text
refs/heads/master
refs/heads/forest/<issue>-<slug>
refs/forest/v1/request/<sha>
refs/forest/v1/checks/<sha>
refs/forest/v1/verdict/<sha>
```

Each evidence ref is a commit whose tree is one JSON file. The committer is
the role identity. A create conflict means the ref exists; stop. `v1` is the
schema prefix. A later schema is a new prefix. Old refs stay unread. Do not
rewrite them.

The Ledger and Run logs are local to the instance. They are not a second work
store.

## Boundaries

The Kernel owns mechanics: the lock, Poll, worktree, harness start, declaration
digest, and publication. Two commands cover every Effect:

```text
forest publish request <role> <branch> <file> [--rejected <sha>]
forest publish verdict <checks-file> <verdict-file>
```

On `changes`, `publish verdict` pushes the two evidence refs. On `approve`, it
runs `forest.yaml` Checks, then pushes those refs and `sha:refs/heads/master`
in one atomic update. Agents write the files and decide to call. They do not
push `master`.

Agents own Issue selection, implementation, review judgment, and the decision
to publish ([ADR 0010](docs/adr/0010-agent-owned-effects-and-merge-gate.md)).

The operator owns the host, credentials, and backlog labels.

The Kernel does not know the host vendor. Isolation is exactly one live
Kernel per repository, on an operator-chosen machine, not one sandbox per
Run ([ADR 0015](docs/adr/0015-one-kernel-per-repository.md),
[ADR 0016](docs/adr/0016-isolation-posture.md)).

`github.com` has no receive hook. The Gate is `forest publish verdict` on
approve. A forge ruleset that allows only the factory actor to update `master`
is deployment, not Kernel policy.

## Surfaces

The CLI is the operations surface. An orchestrator on another machine manages
a Kernel through SSH and `forest` commands. There is no HTML dashboard.

`git ls-remote origin 'refs/forest/v1/*'` and `git show` of an evidence ref
are the escape. The Kernel does not wrap evidence inspection.

## Cutover

This file is the destination. Kernel `publish verdict` ships only after
[issue 238](https://github.com/misty-step/iron-forest/issues/238) records the
Verifier-publication eval. Until [issue 278](https://github.com/misty-step/iron-forest/issues/278)
lands, the running binary still follows ADR 0021 for review-request notes and
prompted Verifier publication. Do not treat that binary as this lock.

After cutover, unread `refs/notes/forest/*` is not a violation. Do not rewrite
note authors.

## Out of product

Mint, Powder, Habitat, Olympus, Fly Sprites, a Tracker adapter interface, a
Kernel substrate seam, and a central trace store are not Iron Forest.

GitHub is the Tracker because that is where the work already lives. It is not
an adapter layer waiting for a second ledger.

## Never finished

The factory is never finished. Each addition must still obey this file: closed
loops in the Kernel, judgment in the agent, observable Git outcomes, no
dashboard, no second work store, no vendor API in the Kernel.

A later workflow protocol moves into the Kernel only when evals show a closed
loop the model cannot execute. Operator CLI commands are not that gate.
Change this file first if an addition disagrees with the aim.
