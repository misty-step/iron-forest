# Vision

Iron Forest is a headless software factory. Exactly one live Kernel serves
one repository, on a machine the operator chooses. The Kernel is small and
deterministic. Agents judge.

Each managed repository owns its profile: roster, executable Polls, declaration
prompts, model and thinking choices, Pi tool allowlists, explicit skills, and
Checks. The shipped profile is an opinionated default, not a Kernel roster.
An external company or executive agent may observe and manage several
repository instances; it does not become a cross-repository Kernel.

This file is the product lock. Accepted ADRs state the contracts. GitHub
Issues and Powder jobs track work. If a later plan disagrees with this file,
change this file first.

## Aim

An operator marks one Issue `forest:ready` or files a takeable Powder job
for the repository. The factory implements it, reviews the exact Revision,
and either merges or returns a repair. The operator watches Git and the CLI.
The factory does not need a console or a host-vendor API.

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
| Kernel | The `forest` process. It polls, isolates a Run, starts the harness, and owns closed Git and Tracker protocols. |
| Declaration | One role: `agents/<name>/agent.md` and `task.md`. |
| Run | One dispatch of one declaration. At most one live Run per declaration. |
| Subject | One GitHub Issue or one Powder job that a Builder may select. |
| Revision | One commit SHA. Every evidence ref binds to it. |
| Tracker | GitHub Issues and Powder jobs. Humans and agents file work there. |
| Projection | A GitHub pull request. Humans read it. It is not authority. |
| Gate | One valid request, passing Checks, an approve Verdict, and a fast-forward of `master` to that Revision. |
| Auditor | `forest status` and `forest audit show` reading evidence refs. It does not merge. |
| Host | The Linux machine that runs the Kernel. The operator chooses it. |

Do not add Manager, dashboard, adapter, substrate, or spend ledger as product
terms.

## Shape

```text
forest:ready Issue or takeable Powder job
  → Builder implements and calls forest publish request
  → Verifier checks, judges, and calls forest publish verdict
  → Kernel reconciles a landed Powder Subject to terminal
  → Fixer repairs a rejected Revision

The shipped review roster is Builder, Verifier, and Fixer
([ADR 0014](docs/adr/0014-agent-roster.md)). Critic is an EXPERIMENTAL,
local-canary-only drafts-only declaration that sweeps the codebase and files
Powder drafts; it never edits code, promotes work, or joins the review loop.
Tester is an EXPERIMENTAL, local-canary-only drafts-only declaration that maps
under-tested observable behaviors into test-work Powder drafts; it never edits
code, promotes work, or joins the review loop. A Sentinel role for post-merge
live QA is not shipped.

Critic and Tester stay enabled only in the self-host Iron Forest checkout for
canary observation; external operators must not copy or enable them. Their
rollout exit gate is: the blocking repair jobs
`if-investigator-provenance-contract`, `if-eval-powder-mutations`,
`if-tester-eval-observable-surface`, `if-eval-draft-note-binding`, and
`if-investigator-powder-availability` are merged, the corrected deterministic
evals pass, and one post-fix live sweep per role produces attributable
spec-less drafts.

Each role is one event, one input, one isolated Run.

Git holds durable facts: branches, commits, and create-only evidence refs:

```text
refs/heads/master
refs/heads/forest/<subject>/<slug>
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
digest, publication, and deterministic Powder terminal reconciliation. Two
commands cover every agent-requested Git Effect:

```text
forest publish request <role> <branch> <file> [--rejected <sha>]
forest publish verdict <checks-file> <verdict-file>
```

On `changes`, `publish verdict` pushes the two evidence refs. On `approve`, it
runs `forest.yaml` Checks, then pushes those refs and `sha:refs/heads/master`
in one atomic update. Agents write the files and decide to call. They do not
push `master`.

For a Powder-backed Gate, current primary is the single pending reconciliation
slot. Before Builder dispatch or a later approve, the Kernel reads only that
Revision's exact request and approve refs. It probes Powder only when the
request records `tracker: powder`. GitHub and undiscriminated historical
requests never complete a job, even when a Powder id collides. If the bound
job is non-terminal, the same repository principal re-takes it and calls
`done` with the Revision as proof. Terminal observation requires that proof
to equal the landed Revision. Failure blocks later work but never reverses or
misreports a successful Gate. The Kernel keeps no outbox and performs no
historical evidence or board scan.

Agents own Subject selection, implementation, review judgment, the initial
Powder take, and the decision to publish
([ADR 0010](docs/adr/0010-agent-owned-effects-and-merge-gate.md)).

The operator owns the host, credentials, backlog labels, and Powder jobs.

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
The orchestrator may keep an explicit inventory of several independent
instances and consume their JSON read surfaces. Cross-repository priorities,
backlog grooming, profile changes, and compatibility choices remain external
management work, not a shared Kernel state machine.


`git ls-remote origin 'refs/forest/v1/*'` and `git show` of an evidence ref
are the escape. The Kernel does not wrap evidence inspection.

## Cutover

Cutover is complete. `forest publish verdict` and `forest publish
review-request` both ship only create-only evidence refs under
`refs/forest/v1/*`; prompted Verifier publication and the review-request note
write are retired (ADR 0028).

After cutover, unread `refs/notes/forest/*` is not a violation. Do not rewrite
note authors.

## Out of product

Mint, Habitat, Olympus, Fly Sprites, a Tracker adapter interface, a
Kernel substrate seam, and a central trace store are not Iron Forest.

Git remains the coordination authority. Powder is exclusive-work for jobs.
Agents select and initially take Powder work. The Kernel may list jobs and may
re-take and complete only the current Git-landed Subject through the bounded
reconciliation loop above; it does not rank, release, ask about, or otherwise
manage Powder work.

## Never finished

The factory is never finished. Each addition must still obey this file: closed
loops in the Kernel, judgment in the agent, observable Git outcomes, no
dashboard, no second coordination store, no host-vendor API in the Kernel.

A later workflow protocol moves into the Kernel only when evals show a closed
loop the model cannot execute. Operator CLI commands are not that gate.
Change this file first if an addition disagrees with the aim.
