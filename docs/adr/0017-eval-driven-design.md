# 0017 — Evals decide actor boundaries

Status: accepted, 2026-08-10

## Context

Some responsibilities can belong to agents, the Kernel, or a tool boundary.
Design intuition cannot establish which assignment is reliable. The reforge has
no eval harness in this slice.

## Decision

Use evals as the decision instrument for agent-versus-Kernel questions. File a
follow-up issue for remaining actor-assignment studies. The 2026-08-15 Builder
canonical-note race (3/3 failures under the current prompt and model) moved
review-request publication into the Kernel; see
[0021](0021-kernel-review-request-publication.md). Keep remaining agent-owned
Effects provisional until later evals produce evidence.

Evaluate observable outcomes: note-schema compliance, exact-Revision evidence,
merge Gate adherence, rejected-work handoff, recovery after process failure,
and cost in token classes. Do not use a provider or monetary score as the
architecture authority.

The executable suite and actor-assignment study follow the repository's
[evaluation strategy](../evaluation-strategy.md).

## Consequences

The current appliance can ship a small boundary without pretending that the
assignment is final. Future changes can compare evidence against a stable
contract instead of adding machinery by preference.

Until the evals exist, accepted risks remain those recorded in ADR 0010. An eval
result that changes ownership requires a new ADR and a coordinated contract
update.
