# 0011 — Kernel and profile boundary

Status: accepted, 2026-08-10

The agent-Run timeout and finite service-drain clauses are superseded by
[0020](0020-unbounded-agent-runs.md).

## Context

The former architecture mixed scheduling, agent policy, coordination, and
recovery machinery. A smaller appliance needs a stable mechanical Kernel and a
profile that can change agent behavior without changing Kernel code.

## Decision

The Kernel owns config load and validation, Poll scheduling, worktree
preparation, per-agent Git identity, OMP invocation, process-local Run
serialization, read-only auditing, Ledger writes, and the status CLI. Agent
Runs have no wall-clock deadline. Runner cleanup has a separate 10-second
bound. Post-dispatch audit has a separate 60-second bound. The systemd service
drains active Runs without a deadline.

After each completed dispatch, the Auditor takes a bounded stable snapshot. It
does not run at startup or after an idle Poll skip. Schema and actor checks cover
each snapshotted `refs/notes/forest/*` entry within a 500-entry-per-ref
capacity bound. Ancestry and the
complete Gate check target the final observed remote `master` tip. The first
observed tip becomes the durable trusted baseline. Remote history cannot reveal
a tip that advanced again between audits; such intermediate tips are not
independently Gate-checked. The Auditor stores only current violations in
`audit.json`. It appends violations to `audit.log` only when the current set
differs from the prior persisted set. A passing Audit clears current violations
and adds no history. Audit history retains exactly the latest 1,000 violation
entries. Each entry is at most 64 KiB. A ref with more than 500 listed or tree
entries, a note enumeration or note-show transport-output overflow, a note
payload above 64 KiB, or malformed or unresolvable canonical note state
(malformed list or tree rows, a listed note without its tree entry, a
mismatched, unexpected, or duplicate tree entry, a non-SHA path, a non-blob
entry, or a note object missing from the object database) becomes a bounded
persisted policy violation and a
non-pass Audit result, never an AuditError. Current Audit results retain at
most 999 concrete violation entries, each at most 1 KiB, plus one exact
omission summary. A bounded policy violation cannot permanently kill Auditor
health; a capacity violation clears when the ref shrinks again, and durable
corruption keeps its bounded violation until the state is repaired.

Each history rewrite removes prior reserved temps and streams old entries
through a bounded ring. It syncs a same-directory temp, renames it atomically,
and syncs the directory. An oversized history entry is rejected without
replacing the prior history.

The Auditor is read-only and checks observable final Git state only. It cannot
prove check execution, atomic push ordering, or force absence. It detects
violations after profile Effects. It does not block, authorize, reject, or
enforce a merge.

The profile owns `forest.yaml`, agent declarations, Poll commands, Subject
selection, Checks commands, and Verifier notes, branch pushes, and merges.
Declarations own `model`, `tools`, and `thinking`; the host owns OMP provider
routing. The Kernel writes review-request notes and their paired branch push
only through `forest publish review-request`. It never selects a Subject.

Each Ledger row records Run identity, timing, exit, and token classes. The
Ledger never records or computes money.

Kernel non-goals are sandbox enforcement, leases, retirement or recovery
machinery, a Manager Flow, money accounting, MCP, webhooks, and a report Gate
beyond read-only Auditor validation. The Kernel does not self-update in place.
Deployment updates are a tracker issue, not Kernel behavior.

Day-one worktree separation is not a security sandbox. A trusted
declaration can access host credentials, filesystem, and network. Stronger
credential and process containment belongs to the host the operator chooses.


The archived `overhaul/independent-flows` branch and its ADRs were not adopted.

## Consequences

A profile can evolve prompts and tools while the Kernel keeps deterministic
mechanics. The Kernel has no hidden policy path and cannot become a second
workflow writer.

Deployment must provide its own update mechanism. The missing deployment
workflow is visible as follow-up work instead of an in-process self-update
contract.
