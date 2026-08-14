# 0016 — Isolation posture

Status: accepted, 2026-08-10

The agent-Run timeout and finite service-drain clauses are superseded by
[0020](0020-unbounded-agent-runs.md).

## Context

Agents need separate workspaces. Strong process containment requires host and
deployment support. A large mandatory containment layer in the Kernel would
couple the appliance to one substrate.

## Decision

Day-one isolation is worktree separation. Agent Runs have no wall-clock
deadline. Runner cleanup uses a separate 10-second bound, post-dispatch audit
uses a separate 60-second bound, and the systemd service drains active Runs
without a deadline.

This posture is not a security sandbox. A trusted declaration still runs with
the operating-system user's configured credentials and can access the
filesystem and network. The Kernel does not claim process, filesystem,
credential, or network containment beyond worktree separation.

Stronger process and credential isolation belongs to the deployment substrate. A
Fly.io instance per agent is the direction for that substrate, not a Kernel
responsibility.

## Consequences

The Kernel stays portable and deterministic across supported hosts. Operators
must choose a substrate that supplies stronger containment when agent trust or
network risk requires it.

An untrusted agent must not be treated as contained by the day-one posture.
Deployment requirements and substrate evaluation are explicit follow-up work.
