# 0016 — Isolation posture

Status: accepted, 2026-08-10

## Context

Agents need separate workspaces and a bounded Run. Strong process containment
requires host and deployment support. A large mandatory containment layer in the
Kernel would couple the appliance to one substrate.

## Decision

Day-one isolation is worktree separation plus a configured timeout for
worktree preparation and agent execution. Runner cleanup uses a separate
10-second bound. Post-dispatch audit uses a separate 60-second bound. The
systemd service uses a separate 3900-second drain bound. This bound covers the
shipped declarations' concurrent Runs, bounded Runner cleanup, and serialized
post-dispatch audits.

This posture is not a security sandbox. A trusted declaration still runs with
the operating-system user's configured credentials and can access the
filesystem and network. The Kernel does not claim process, filesystem,
credential, or network containment beyond worktree separation and these time
bounds.

Stronger process and credential isolation belongs to the deployment substrate. A
Fly.io instance per agent is the direction for that substrate, not a Kernel
responsibility.

## Consequences

The Kernel stays portable and deterministic across supported hosts. Operators
must choose a substrate that supplies stronger containment when agent trust or
network risk requires it.

An untrusted agent must not be treated as contained by the day-one posture.
Deployment requirements and substrate evaluation are explicit follow-up work.
