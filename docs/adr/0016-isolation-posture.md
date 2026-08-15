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

Stronger process and credential isolation belongs to the host the operator
chooses. The Kernel has no host-vendor API and no per-Run sandbox. Isolation
is exactly one live Kernel per repository, on an operator-chosen machine.
See [VISION.md](../../VISION.md).



## Consequences

The Kernel stays portable. Operators choose a host that supplies stronger
containment when agent trust or network risk requires it.

An untrusted agent must not be treated as contained by worktree isolation.

