# Iron Forest — vision

Iron Forest is a self-hosted set of independent Flows: Builder, Verifier, and Fixer.
Each Flow selects work, takes a Lease, acts, and records an Effect.

## Audience

One engineer or a small team runs Iron Forest on a repository they control.
The operator reads evidence while the Flows work on their own clocks.

## System boundary

State lives in git refs and git notes, so any Host can run the same repository.
The Tracker is where work lives.
The Host is where code, branches, and checks live.
Tracker and Host are separate ports with no required vendor coupling.

Credentials are a port.
Iron Forest supports a Mint adapter and a `.env` adapter.
Neither adapter is privileged; each supplies values to the configured Runner.

Iron Forest runs its own checks and writes their results as notes.
Iron Forest does not read a Host's checks, reviews, or pull-request state.
A Host is an optional one-way Projection for people who prefer that surface.

## Job to be done

Take one Tracker item and build it in an isolated worktree.
The Builder publishes an unreviewed branch.
The Verifier runs checks, obtains an independent Verdict, and can merge.
The Fixer repairs rejected branches within the configured attempt limit.
The Ledger records each Run, Subject, Revision, Effect, and Verdict.

Cost is bounded by the provider key and is not accounted for here.

## Standards

- **The Gate is deterministic.** Agents propose; the Gate decides from explicit evidence.
- **The Ledger is append-only and honest.** History records what each Run observed and did.
- **Agent definitions are data.** Model, permissions, prompts, and output contracts stay inspectable.

## Model default

The reviewer should use a different model family from the Builder.
This separation is a default worth keeping because it reduces shared blind spots.
It is not a law when a deployment has a documented reason to differ.

## Non-goals

- A hosted multi-tenant service that holds other people's credentials.
- A web control plane or dashboard.
- Reading or mirroring Host decisions as factory state.

## Direction

Iron Forest favors boring local operation, explicit repository evidence, and small
ports over hidden integrations. Execution may move to another Host without moving
state out of git or teaching the controller vendor-specific rules.
