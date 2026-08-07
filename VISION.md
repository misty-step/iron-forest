# Iron Forest — vision

## Category

Iron Forest is a self-hosted software factory for one repository.
Three independent Flows run in one Go process: Builder, Verifier, and Fixer.

## Value proposition

Iron Forest turns a Tracker item into a checked, independently reviewed change
that a person can explain afterwards from the repository alone.

## Audience

One engineer or a small team runs Iron Forest on a repository they control.
The operator reads evidence while the Flows work on their own clocks.

## Job to be done

Take one Tracker item and build it in an isolated worktree.
The Builder publishes an unreviewed branch.
The Verifier rebases the branch, runs the declared checks, obtains an
independent Verdict on the exact commit, and can merge.
The Fixer repairs rejected branches within the configured attempt limit.
The Ledger records each Run, Subject, Revision, Effect, and Verdict.

## What ships today

Coordination state lives in git refs and git notes: leases under
`refs/forest/lease/`, attempts under `refs/forest/attempt/`, and Verdict and
Checks notes keyed to the exact reviewed commit.
Run history lives in `.forest/runs.jsonl` on the host and is not in git.

The Tracker is the work source. Tracker and Projection both call `gh`, so this
repository requires GitHub today.

Iron Forest runs its own checks in the worktree and writes their results as
notes. It never reads a Host's checks, reviews, or merge decisions as factory
state. Projection is optional; when enabled it publishes a branch and mirrors
each decision as a comment, and it reads the open pull-request list so it does
not create a second one.

Agents run through opencode. Credentials are Mint markers in the opencode
configuration. No `.env` adapter ships.

Cost is bounded by the provider key. The Ledger records tokens, not currency.

## Standards

- **The Gate is deterministic.** Agents propose; the Gate decides from explicit evidence.
- **The Ledger is append-only and honest.** A Run that cannot be recorded must fail loudly.
- **Agent definitions are data.** Model, permissions, prompts, and output contracts stay inspectable.
- **Authority is bounded by the worktree.** A run must not change the host outside the worktree it was given.

## Bets

- A deterministic Gate on explicit evidence is enough to make an unattended merge safe.
- Repository evidence stays sufficient to explain every Run without a database or a dashboard.
- An operator can replace the Tracker and the Host without moving factory state out of git.

## Excellent outcome — 2027-02-06

A new operator configures Iron Forest on a repository that is not this one and
runs it unattended for 30 days. Every merge is explainable from git refs, notes,
checks, and the Ledger. The Gate has demonstrably rejected a real regression.

## Direction

Iron Forest favours boring local operation, explicit repository evidence, and
small ports over hidden integrations. Replace the direct `gh` calls with a
declared Tracker port and Projection port. Move execution to another Host only
when the same repository evidence stays sufficient.

## Non-goals

- A hosted multi-tenant service that holds other people's credentials.
- A web control plane or dashboard.
- Reading Host checks, reviews, or merge state as factory state.
- Managing a fleet of repositories from one installation.
