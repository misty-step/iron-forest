# Iron Forest — vision

## Category

Iron Forest is a self-hosted software factory. One organization runs one
installation, which works an arbitrary number of that organization's
repositories. Each repository gets its own process, running four independent
Flows: Builder, Verifier, Fixer, and Manager.

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
the agents, the merge rules. There is no central policy, which is why there is
no authority reconciler. No path is withheld from an agent; the factory may
work on its own declarations, and independent review on the exact commit is
what decides whether that work lands.

Coordination is facts, not locks. A Verdict or Checks note is keyed to the exact
reviewed commit and written once; git refuses the second writer. Attempts and the
repeat-failure brake are compare-and-set refs. Branch publication carries the
observed remote tip, so a lost race fails cleanly. Exclusion inside one process
is an in-process subject set, and one lock file keeps one daemon per checkout.
That lock covers `serve` only: `forest run` dispatches outside it, and nothing
today excludes two checkouts of the same repository.

Run history lives in `.forest/runs.jsonl` on the host and is not in git. It is
telemetry. No selection decision depends on it.

The Tracker is the work source. Tracker and Projection both call `gh`, so this
repository requires GitHub today.

Iron Forest runs its own checks in the worktree and writes their results as
notes. It never reads a Host's checks or reviews as factory state. Projection
is optional. It publishes branches, mirrors decisions, and reads pull-request
identity for idempotent publication. When the Host owns merge, it reads the
exact merged head only to recover a pending retirement.

Agents run through opencode. Credentials are Mint markers in the opencode
configuration. No `.env` adapter ships. An agent run carries no credential of
its own: the controller is the only caller of the Tracker and the only writer.

The factory's own source is separate from the repositories it manages, so a
managed repository may be in any language. Only the instance that manages that
source may move it; the others rebuild from it.

Cost is bounded by the provider key. The Ledger records tokens, not currency.

## Surfaces

The core is a headless program. It runs with no terminal, no operator present,
and no network listener, and it is complete on its own. Everything it knows is
in the repository, the refs, the notes, and the Ledger.

Every operation the core performs should be reachable through one internal API,
with each surface a client of that API and no privileged path around it. That is
the target, not today's state. `core/core.go` currently exposes durable reads
only; `cmdList`, `cmdAgents`, `cmdShow`, `cmdStats`, and `watch` still reach past
it. #176 migrates the read callers, #177 moves the implementations, and #162 adds
the writes an operator needs.

- **CLI** — the reference surface and the contract, and today the only one that
  ships. Scriptable, pipeable, and usable by an agent that is not part of Iron
  Forest.
- **TUI** — the same operations with a live view, for an operator watching work.
  Planned; see #62.
- **UI** — the same operations again, for reading traces and configuration
  comfortably. Planned; see #180.

Surfaces will have parity: anything readable is readable from any of them, and
anything writable is writable from any of them. Once two surfaces ship, a
capability that exists in only one is a defect rather than a roadmap item.

One exception shapes the design rather than breaking the rule. Which subject is
in flight, and cancelling a live run, exist only inside the running process.
Those operations need the daemon to serve them over a local transport, which is
not built yet; #163 is that work, and until it lands `watch` infers liveness from
git, systemd, and the Ledger. Everything else is durable state that any surface
may read directly.

## Standards

- **The Gate is deterministic.** Agents propose; the Gate decides from explicit evidence.
- **The Ledger is append-only and honest.** A Run that cannot be recorded must fail loudly.
- **Agent definitions are data.** Model, permissions, prompts, and output contracts stay inspectable.
- **Surfaces have parity.** Every read and every write is reachable from the CLI, and no surface holds a capability the others lack.
- **Authority is bounded by the worktree.** A run must not change the host outside the worktree it was given, and must not hold a credential for anything else.
- **A fact about a commit is written once.** Coordination never guesses whether a holder is alive.

## Bets

- A deterministic Gate on explicit evidence is enough to make an unattended merge safe.
- Repository evidence stays sufficient to explain every Run without a database. A surface presents that evidence; it never becomes the source of it.
- An operator can replace the Tracker and the Host without moving factory state out of git.
- Facts and idempotent writes coordinate work as well as locks, for less.

## Excellent outcome — 2027-02-06

A fresh operator runs a recorded factory Revision on `misty-step/cantrip` and at
least one further repository that is not Iron Forest, for 30 consecutive UTC
days, with no manual Flow intervention. Record the start and end instants, the
factory Revision, and each `forest.yaml` Revision.

For every merge, the Ledger retains one row linking Subject, reviewed Revision,
Checks note, Verdict note, and the resulting master Revision. Branch deletion at
merge must not break that lineage.

Seed one known regression into code or checks. Require a failing Checks note, no
merge, and a retained evidence bundle naming the exact Revision that was refused.

## Direction

Iron Forest favours boring local operation, explicit repository evidence, and
small ports over hidden integrations. It grows by deleting premises, not by
matching features. Replace the direct `gh` calls with a declared Tracker port and
Projection port. Put one API in front of the core so the CLI, the TUI, and a UI
are the same program seen from three places.

## Non-goals

- A hosted multi-tenant service that holds another organization's credentials.
- A surface that becomes the source of truth. A UI and a TUI are in scope; state that lives only in one of them is not.
- Reading Host checks, reviews, or merge state as factory state.
- Central policy for repositories the installation does not own.
- A coordinated merge across repositories.
- Two processes working one repository at the same time.
