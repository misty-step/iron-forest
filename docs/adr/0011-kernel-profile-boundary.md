# 0011 — Kernel and profile boundary

Status: accepted, 2026-08-10

## Context

The former architecture mixed scheduling, agent policy, coordination, and
recovery machinery. A smaller appliance needs a stable mechanical Kernel and a
profile that can change agent behavior without changing Kernel code.

## Decision

The Kernel owns config load and validation, Poll scheduling, worktree
preparation, per-agent Git identity, OMP invocation, process-local Run
serialization, read-only auditing, Ledger writes, and the status CLI. The
configured timeout covers worktree preparation plus agent execution. Runner
cleanup has a separate 10-second bound. Post-dispatch audit has a separate
60-second bound. The systemd service uses a separate 3900-second drain bound.
This bound covers the shipped declarations' concurrent Runs, bounded Runner
cleanup, and serialized post-dispatch audits.

After each completed dispatch, the Auditor takes a bounded stable snapshot. It
does not run at startup or after an idle Poll skip. Schema and actor checks cover
every entry in every snapshotted `refs/notes/forest/*` ref. Ancestry and the
complete Gate check target the final observed remote `master` tip. The first
observed tip becomes the durable trusted baseline. Remote history cannot reveal
a tip that advanced again between audits; such intermediate tips are not
independently Gate-checked. The Auditor records only current violations in
`audit.json` and keeps history in `audit.log`.

The Auditor is read-only and checks observable final Git state only. It cannot
prove check execution, atomic push ordering, or force absence. It detects
violations after profile Effects. It does not block, authorize, reject, or
enforce a merge.

The profile owns `forest.yaml`, agent declarations, Poll commands, Subject
selection, Checks commands, workflow notes, branch pushes, and merges.
Declarations own `model`, `tools`, and `thinking`; the host owns OMP provider
routing. The Kernel never writes workflow notes and never selects a Subject.

Each Ledger row records Run identity, timing, exit, and token classes. The
Ledger never records or computes money.

Kernel non-goals are sandbox enforcement, leases, retirement or recovery
machinery, a Manager Flow, money accounting, MCP, webhooks, and a report Gate
beyond read-only Auditor validation. The Kernel does not self-update in place.
Deployment updates are a tracker issue, not Kernel behavior.

Day-one worktree separation and timeout are not a security sandbox. A trusted
declaration can access host credentials, filesystem, and network. Stronger
credential and process containment belongs to the deployment substrate.

The archived `overhaul/independent-flows` branch and its ADRs were not adopted.

## Consequences

A profile can evolve prompts and tools while the Kernel keeps deterministic
mechanics. The Kernel has no hidden policy path and cannot become a second
workflow writer.

Deployment must provide its own update mechanism. The missing deployment
workflow is visible as follow-up work instead of an in-process self-update
contract.
