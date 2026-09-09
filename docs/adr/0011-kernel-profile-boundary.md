# 0011 — Kernel and profile boundary

Status: accepted, 2026-08-10

The agent-Run timeout and finite service-drain clauses are superseded by
[0020](0020-unbounded-agent-runs.md).

The current profile layout and request/admission contract are documented in
[README](../../README.md#one-profile-explicit-requests-persistent-admission).
Config, defaults, policy, declarations, explicit extension inputs, the installed
binary and runtime live under `.iron-forest`. Root-level loaders are retired.
`delivery: external` delegates delivery authority outside Kernel: native
publication is refused and native audit is explicitly not applicable. The
Git-native contracts below apply only to `delivery: git-native`.

## Context

The former architecture mixed scheduling, agent policy, coordination, and
recovery machinery. A smaller appliance needs a stable mechanical Kernel and a
profile that can change agent behavior without changing Kernel code.

## Decision

The Kernel owns config validation, Poll scheduling, explicit request admission,
worktree preparation, Pi invocation, cooperative Run serialization and recovery,
native publication mechanics, read-only auditing, Ledger writes, and the CLI.
There is no default Run deadline; a declaration may set an elapsed-time
`max_duration`. Cleanup and external observation commands have their own bounds.
Persistent pause and drain fence new dispatch while allowing admitted work to
finish. Startup records interrupted Runs; it does not resume model context.

The profile owns selection, prompts, tools, checks and the interpretation of
external effects. `request` returns an opaque, attributed request.
Optional `completion` observes its required effect and returns a bounded
`forest.completion.v1` result. Kernel retains that result separately from raw
process exit and known execution cause. Neither a successful process nor a
completed review is a claim of delivery.

For `git-native`, publication uses the current write-once
`refs/forest/v1/{request,checks,verdict}/<sha>` contracts. Retired notes are not a
parallel authority; see [ADR 0028](0028-review-request-notes-retired.md).
For `external`, profile and operator own publication and reconciliation; Kernel
native publication is refused. A profile's tracker or forge semantics do not
become new Kernel branches.

The Auditor checks observable final Git state. It cannot prove every historical
check execution, intermediate tip or atomic push ordering. It does not authorize
a merge. The current bounds and persisted evidence are specified by the
[operator contract](../../README.md#auditor-and-trust-boundary), not by a second
description of the retired notes protocol.

Ledger rows retain execution identity, request/work attribution, declared
resource digests, timing, outcome, optional completion evidence and five token
classes (`tokens_in`, `tokens_out`, `cache_read`, `cache_write`, `reasoning`).
The Ledger is operational evidence, not monetary accounting; it does not
calculate cost, price, spend or currency.

Kernel non-goals are sandbox enforcement, external workflow orchestration,
automatic retries, a fleet manager, provider budget authority and self-update.
The host owns grants, containment and deployment. The repository profile owns
whether and how an uncertain effect needs operator reconciliation.
Deployment updates are explicit owner operations, not Kernel behavior.

Day-one worktree separation is not a security sandbox. A trusted
declaration can access host credentials, filesystem, and network. Stronger
credential and process containment belongs to the host the operator chooses.


The archived `overhaul/independent-flows` branch and its ADRs were not adopted.

## Consequences

A profile can evolve prompts and tools while the Kernel keeps deterministic
mechanics. The Kernel has no hidden policy path and cannot become a second
workflow writer.

Deployment supplies its own revision-coherent update and recovery procedure.
The Kernel does not update itself or transfer operational ownership.
