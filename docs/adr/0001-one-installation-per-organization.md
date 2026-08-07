# 0001 — One installation per organization

Status: accepted, 2026-08-07

## Context

Iron Forest was built for one repository. `VISION.md` listed "managing a fleet of
repositories from one installation" as a non-goal, and one systemd unit bound one
checkout.

Two forces made that shape wrong. An operator owns many repositories in one
organization and wants them all worked. An application can span several
repositories, so per-repository installations felt unwieldy.

Three shapes were measured against the shipped code.

- **Many units, one per checkout.** The shipped shape. Costs a hand-written unit,
  a log, and an update path per repository.
- **One process over many checkouts.** Costs roughly 80 lines: three `os.Getwd`
  call sites, a checkout list, a supervisor, worktree ownership, and a
  repository-qualified in-process subject set, whose key is currently the bare
  subject key and would collide across repositories. It loses per-repository
  failure and update isolation.
- **Many instances of one template unit.** Costs six lines of unit file and no Go
  code.

## Decision

One organization runs one installation. The installation is a systemd template
unit, `forest@.service`, instantiated once per managed checkout. Instance names
are sibling directories of the factory source, so the organization's checkout
directory is the root and no repository moves.

The factory's own source is separate from the repositories it manages, declared
by `serve --factory-dir`. Self-update is off unless that flag is present.

Only the instance whose managed checkout *is* the factory source may move that
source. Every other instance rebuilds from whatever the source currently is.
One process mutates the shared checkout; all of them converge on it.

A work item spans exactly one repository. Cross-repository changes are separate
items that a person sequences.

## Consequences

Per-repository isolation survives: separate process, config, refs, worktrees and
ledger. A stuck agent in one repository cannot stall another, which is why no
supervisor and no central policy file are needed.

Each repository keeps declaring its own factory in its own `forest.yaml`. That is
why Iron Forest needs no central authority reconciler, and it is the structural
reason it stays smaller than the system it replaces.

A managed repository may be in any language. Before this decision, self-update
ran `go build` in the managed checkout, so pointing Iron Forest at another Go
project would have built that project and executed it as the daemon.

Rejected and why: a coordinated multi-repository merge costs 180 or more lines,
adds group subjects and partial-failure recovery, and cannot be atomic across
separate remotes. Sibling-branch checks cost 45 to 70 lines, buy integration
confidence, and still cannot land a compatible pair. Both stay unbuilt until an
application demonstrably needs them.

Residual risk: an application change can land in one repository while its sibling
is still incompatible. A person carries that risk today, deliberately.
