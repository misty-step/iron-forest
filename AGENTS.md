# AGENTS

This file is the contributor contract for Iron Forest.

## Product shape

Iron Forest is a self-hosted software factory shipped as a Kernel plus profile
appliance. The Kernel provides deterministic mechanics. A profile contains
`forest.yaml` and the agent declarations that define agent behavior.

## Vocabulary

Use these names for system concepts:

- **Flow:** one declaration's trigger-run-effect lane, such as the Builder Flow.
- **Run:** one dispatched agent execution.
- **Phase:** a bounded stage of a Run: preparation, agent execution, cleanup, or audit.
- **Subject:** the unit of work selected by an agent, an Issue or branch Revision.
- **Revision:** an exact commit SHA. Evidence binds to Revisions, never branches.
- **Selector:** the agent's in-Run work selection. A Poll only wakes an agent.
- **Effect:** a durable write, such as a note, branch push, or merge.
- **Verdict:** the Verifier's decision note, `approve` or `changes`.
- **Gate:** the merge preconditions for one exact Revision: one valid review-request, passing Checks, an approving Verdict, and fast-forward.
- **Ledger:** the Run record in `.forest/runs.jsonl`.
- **Tracker:** the issue backlog, GitHub on day one.
- **Runner:** the Kernel component that prepares worktrees and invokes OMP.
- **Projection:** a human-facing forge artifact, such as a pull request or comment.
  A Projection is never authoritative.
- **Builder:** the declaration that creates a branch and requests review.
- **Verifier:** the declaration that runs Checks, reviews a Revision, and merges an approved Revision.
- **Fixer:** the declaration that repairs a rejected Revision and requests review again.

Use one name for one concept in source, tests, issues, commits, and documents.
Do not add accounting for monetary amounts.

The following concepts are deleted from the current architecture: Manager agent,
lease, landed fact, retirement, sandbox, and Bubblewrap. Do not describe any of
them as current behavior. Historical rationale belongs in the changelog and
accepted ADRs.

## Agent declarations

Each declaration has this layout:

```text
agents/<name>/agent.md
agents/<name>/task.md
```

`agent.md` starts with YAML frontmatter. `model` is required. `tools` and
`thinking` are optional. The body is the system prompt. `task.md` is the
standing user prompt. The Kernel parses this frontmatter and does not depend on
OMP agent discovery.

The shipped profile has these declarations:

```text
agents/builder/agent.md
agents/builder/task.md
agents/verifier/agent.md
agents/verifier/task.md
agents/fixer/agent.md
agents/fixer/task.md
```

Keep `model`, `tools`, `thinking`, and prompt policy in declarations. OMP
provider routing is host-managed; do not add a repository provider adapter.
Do not add a bespoke wrapper tool for Git coordination. Agents use native
`git` and configured CLI adapters from their prompts.

## Kernel responsibilities

The Kernel owns the complete mechanical surface:

- load and validate `forest.yaml`;
- schedule Poll triggers;
- prepare worktrees;
- invoke OMP through the configured Runner with per-declaration
  `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`, and
  `GIT_COMMITTER_EMAIL`;
- enforce the configured preparation and agent-execution timeout;
- serialize at most one live Run per declaration in one process;
- audit Git state after each completed dispatch without writing workflow notes;
- write Run identity, timing, exit, and token classes to the Ledger;
- provide `forest serve`, `forest once`, `forest poll`, `forest status`, and
  `forest selfcheck`.

The Kernel never chooses a Subject, writes workflow notes, performs workflow
coordination Effects, or merges a branch. Profiles and agents own those actions.
The first observed remote `master` tip becomes a trusted baseline and is not
Gate-checked. In each bounded stable snapshot, the Kernel Auditor applies
ancestry and Gate checks only to the final observed remote `master` tip.
Separately, it applies schema and actor checks to every entry in every
snapshotted `refs/notes/forest/*` ref, including the baseline snapshot.
Remote history cannot reveal a tip that advanced again between audits; such
intermediate tips are not independently Gate-checked. The Auditor checks only
observable final Git state. It cannot prove check execution, atomic push
ordering, or force absence. It detects violations and does not block, authorize,
or enforce a merge. An audit runs after a completed dispatch, not at startup or
after an idle Poll skip.

The Kernel has no sandbox, lease, retirement, recovery, Manager flow, money
accounting, MCP, webhook, or report-gate machinery.

The deployment contract permits exactly one Kernel checkout and process per
repository. The OS lock rejects a second process only in that same checkout.
Git claims for multiple checkouts or clones are deferred to a follow-up Issue.
Every Poll has a fixed 60-second deadline. The configured declaration timeout
separately covers worktree preparation and agent execution. Runner cleanup has
a 10-second bound. Post-dispatch audit has a separate 60-second bound. The
systemd service uses a separate 3900-second drain bound. This bound covers the
shipped declarations' concurrent Runs, bounded Runner cleanup, and serialized
post-dispatch audits. The service PATH is
`%h/.local/bin:%h/bin:/usr/local/bin:/usr/bin:/bin`. Before restart, the
installer runs selfcheck with the equivalent `$HOME`-expanded path.
Day-one worktree separation and these time bounds do not contain a trusted
declaration: it can access operating-system credentials, filesystem, and
network. Stronger containment belongs to deployment.

## Coordination and merge rules

Git is the authority. Branches, commits, and notes under
`refs/notes/forest/*` hold workflow state. A Pull Request is a disposable
Projection for people.

Notes are write-once. Fetch the notes ref before writing. Use `git notes add`
without `-f`. If the object already has an identical note, treat the Effect as
complete. A conflict is an error. Builder and Fixer publish the branch and review-request
note through one normal `git push --atomic`. Their canonical note race recovery
permits at most three total atomic attempts; a branch race stops. For a
`changes` Verdict, Verifier publishes Checks and Verdict together and permits at
most three total atomic attempts after a canonical note race. For `approve`,
Verifier makes exactly one non-retryable atomic attempt that also carries the
exact fast-forward `master` advance. No publication path uses force. No branch
and note publication is split.

The note writer sets are fixed:

- `review-request`: Builder and Fixer;
- `checks`: Verifier;
- `verdict`: Verifier.

The Verifier merges only when one valid Builder-or-Fixer review-request, passing
Checks, and an approving Verdict bind to the same exact Revision. The one
approve push carries Checks, Verdict, and the exact reviewed tip with a
fast-forward-only `master` advance; the existing review-request is not
republished. The read-only Auditor checks note writer identities after each
completed dispatch. It checks the Gate after the trusted first `master`
baseline. These checks are profile contracts, not Kernel security enforcement.

## Ledger and checks

Each Ledger row records `run_id`, `agent`, `started`, `duration`, `exit`,
`tokens_in`, `tokens_out`, `cache_read`, `cache_write`, and `reasoning`. The
Ledger never records or computes money.

The `checks:` list in `forest.yaml` is the source for Checks commands. The
repository's `.github/workflows/ci.yml` must mirror those commands and their
required order. Keep local checks and CI checks consistent:

```sh
mise exec -- go build ./...
mise exec -- go vet ./...
mise exec -- go test ./...
```

Use `master` as the target branch. Run `forest selfcheck` locally after
configuration or declaration validation. A remote audit occurs only after a
completed agent dispatch; starting the Kernel or receiving only idle Poll skips
does not audit. Keep changes small and update the relevant proof.
