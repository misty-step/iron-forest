# Iron Forest — vision

## Category

Iron Forest is a self-hosted software factory. One organization runs one
installation, which works an arbitrary number of that organization's
repositories. Each repository gets its own process, running three independent
Flows: Builder, Verifier, and Fixer.

## Value proposition

Iron Forest turns a Tracker item into a checked, independently reviewed change
that a person can explain afterwards from the repository alone.

## Audience

One engineer or a small team runs Iron Forest on repositories they control.
The operator reads evidence while the Flows work on their own clocks.

## Job to be done

Take one Tracker item and build it in an isolated worktree.
The Builder publishes an unreviewed branch.
The Verifier rebases the branch, runs the declared checks, obtains an
independent Verdict on the exact commit, and can merge.
The Fixer repairs rejected branches within the configured attempt limit.
The Ledger records each Run, Subject, Revision, Effect, and Verdict.
One item spans one repository. A person sequences work that crosses two.

## What ships today

Each repository declares its own factory in its own `forest.yaml`: the checks,
the agents, the paths no agent may touch. There is no central policy, which is
why there is no authority reconciler.

Coordination is facts, not locks. A Verdict or Checks note is keyed to the exact
reviewed commit and written once; git refuses the second writer. Attempts and the
repeat-failure brake are compare-and-set refs. Branch publication carries the
observed remote tip, so a lost race fails cleanly. Exclusion inside one process
is an in-process subject set, and one lock file keeps one process per checkout.

Run history lives in `.forest/runs.jsonl` on the host and is not in git. It is
telemetry. No selection decision depends on it.

The Tracker is the work source. Tracker and Projection both call `gh`, so this
repository requires GitHub today.

Iron Forest runs its own checks in the worktree and writes their results as
notes. It never reads a Host's checks, reviews, or merge decisions as factory
state. Projection is optional; when enabled it publishes a branch and mirrors
each decision as a comment, and it reads the open pull-request list so it does
not create a second one.

Agents run through opencode. Credentials are Mint markers in the opencode
configuration. No `.env` adapter ships. An agent run carries no credential of
its own: the controller is the only caller of the Tracker and the only writer.

The factory's own source is separate from the repositories it manages, so a
managed repository may be in any language. Only the instance that manages that
source may move it; the others rebuild from it.

Cost is bounded by the provider key. The Ledger records tokens, not currency.

## Standards

- **The Gate is deterministic.** Agents propose; the Gate decides from explicit evidence.
- **The Ledger is append-only and honest.** A Run that cannot be recorded must fail loudly.
- **Agent definitions are data.** Model, permissions, prompts, and output contracts stay inspectable.
- **Authority is bounded by the worktree.** A run must not change the host outside the worktree it was given, and must not hold a credential for anything else.
- **A fact about a commit is written once.** Coordination never guesses whether a holder is alive.

## Bets

- A deterministic Gate on explicit evidence is enough to make an unattended merge safe.
- Repository evidence stays sufficient to explain every Run without a database or a dashboard.
- An operator can replace the Tracker and the Host without moving factory state out of git.
- Facts and idempotent writes coordinate work as well as locks, for less.

## Excellent outcome — 2027-02-06

A new operator configures Iron Forest on repositories that are not this one and
runs it unattended for 30 days. Every merge is explainable from git refs, notes,
checks, and the Ledger. The Gate has demonstrably rejected a real regression.

## Direction

Iron Forest favours boring local operation, explicit repository evidence, and
small ports over hidden integrations. It grows by deleting premises, not by
matching features. Replace the direct `gh` calls with a declared Tracker port and
Projection port.

## Non-goals

- A hosted multi-tenant service that holds another organization's credentials.
- A web control plane or dashboard.
- Reading Host checks, reviews, or merge state as factory state.
- Central policy for repositories the installation does not own.
- A coordinated merge across repositories.
- Two processes working one repository at the same time.
