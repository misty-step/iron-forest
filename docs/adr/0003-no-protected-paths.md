# 0003 — No protected paths

Status: accepted, 2026-08-07

## Context

An earlier configuration used a path list to reject changes to declarations and
factory configuration. That list provided no security containment because an agent
could edit the code that enforced it. It also blocked ordinary self-hosting
work, such as changing a prompt or check.

## Decision

Iron Forest has no protected-path list. `forest.yaml`, `agents/`, and every
other repository path may change in a Subject. The merge Gate uses independent
review of the exact Revision, declared Checks, and repository Git rules instead
of path names.

The actual boundaries are workflow separation and attribution controls:
worktree separation, per-agent identity, and profile-declared permissions. They
are not security containment. Stronger containment is deferred to ADR0016.

The Auditor validates durable note shape and exact-Revision evidence. It does
not reject a path. A declaration change is reviewed like any other change.

## Consequences

The factory can maintain its own profile and configuration without an operator
exception. A careless agent can propose a declaration change, so the operator
or the independent Verifier must inspect that Revision before merge.
