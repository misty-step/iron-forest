# Iron Forest — vision

A self-hosted daemon that turns tracker items into reviewed, merged changes, and
records what it spent and why it passed.

## Audience

One engineer, or a small team, running the factory on their own machine against
their own repositories. They already trust agents to write code. They do not
trust agents to merge code without a gate, a ledger, and a cost ceiling.

## Job to be done

Take one work item. Build it in isolation. Prove it against a deterministic
gate. Get an independent review. Publish a pull request. React to CI and to
human feedback. Merge when every condition holds. Record the run.

The operator's job is to shape work and read evidence, not to babysit a loop.

## Category

Self-hosted software factory. Not a hosted service, not an IDE assistant, not a
chat agent. The unit of value is a merged change with an audit trail.

## Standards

- **The gate is deterministic.** Agents propose; code disposes. A schema-valid
  report, an unchanged base SHA, and no protected-path writes are conditions,
  not suggestions.
- **Review is independent.** The reviewer never shares the builder's model
  family. A reviewer that shares the builder's blind spots is decoration.
- **The ledger is append-only and honest.** Cost comes from the provider, not
  from a local guess. History is never rewritten, including after a rename.
- **Agent definitions are data.** Model, budget, permissions, prompt, skills,
  and output contract live in files, are diffable, and are digested onto every
  run row.
- **Spend is bounded at the provider.** A daily cap on a dedicated key is the
  blast radius, because it holds even when the code is wrong.
- **Operations are boring.** One binary, one loop, one lock, one unit file. A
  cold operator can build, run, and debug it from the README.

## Non-goals

- A hosted, multi-tenant service that holds other people's tokens.
- A web control plane or dashboard. The terminal and the ledger are the surface.
- Multi-harness portability. One blessed harness until real demand appears.
- Replicating a release pipeline, admission freeze, or promotion machinery.

## Bets

1. **Declared agents beat embedded prompts.** A directory that names model,
   permissions, budget, and output contract is inspectable and reproducible.
2. **A deterministic gate plus an independent reviewer makes unattended merge
   defensible.** Autonomy is earned by the gate, not by confidence.
3. **A GitHub-issue queue is portable enough to start with, and a thin port
   keeps other boards reachable.** The controller should know work items,
   claims, and change references — never labels or issue numbers.
4. **One local binary outperforms a distributed control plane** at this scale.
   Cloud execution is a concurrency decision, not an architecture.
5. **Provider-side budget caps are the honest cost control.** Local price
   tables drift and lie.

## Where this is going, 6–12 months

A stranger clones the repository, supplies a provider key and a forge token,
runs one command, and watches their own backlog become reviewed pull requests.
The operator sees current state, spend, and blocked work in one terminal view.
Execution can move to ephemeral cloud sandboxes when concurrency demands it,
without the controller learning where it runs. The work source can change from
issues to another board by configuration and a conformance-tested adapter.

## Direction changes

- **2026-08-05.** Identity set to a product others self-host, not a private
  proving ground. Consequences: a provider key must work without the operator's
  credential broker, the forge token must be the user's own, and the harness
  dependency must be stated instead of assumed.
- **2026-08-05.** Work source stays GitHub issues, behind a seam. A second board
  may follow; no adapter is built until that board's semantics are settled.
- **2026-08-05.** Reviewer must not share the builder's model family, restoring
  a rule the shipped system had quietly broken.
- **2026-08-05.** Self-update stays, but auto-pull becomes opt-in and defaults
  off, because a default that deploys any merged change is a supply-chain path.
