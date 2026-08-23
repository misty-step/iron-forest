# 0020 — Agent Runs have no wall-clock deadline

Status: accepted, 2026-08-13

Supersedes the agent-Run timeout and finite service-drain decisions in
[0011](0011-kernel-profile-boundary.md),
[0012](0012-poll-trigger-protocol.md),
[0016](0016-isolation-posture.md), and
[0018](0018-pi-harness.md). It does not remove bounded mechanical maintenance
operations.

## Context

A productive coding agent may work for hours or days. Wall-clock duration is
not evidence that it is stuck, and model-request latency is not agent failure.
A fixed declaration timeout destroyed useful work: a Verifier reached the
correct blocking decision, continued investigating, and was killed at 1,800
seconds before it could publish the verdict. The timeout then made the same
Revision eligible for another Run.

Increasing the limit would preserve the same failure at a later time. Progress,
correctness, and protocol adherence must be improved through harness design and
evals, not inferred from elapsed time.

## Decision

Agent Runs have no configured or implicit wall-clock deadline.

`forest.yaml` agents declare their Poll command and Poll interval. The
removed `timeout` key is rejected as unknown configuration rather than retained
as a compatibility alias. An optional per-declaration `max_duration` watchdog
bound may be set; it defaults off and is documented in the amendment below. The
Runner does not create a deadline around worktree preparation or Pi execution
and does not pass a duration to the harness.

A Run ends when Pi finishes or an operator explicitly cancels the foreground
`forest once` process. The Runner still responds to caller cancellation by
stopping the Run process group with TERM, a short shutdown grace, KILL when
needed, and a quiescence probe. Cancellation is a lifecycle command, not a
liveness judgment.

Scheduled Runs are detached from the polling context. On service shutdown the
Scheduler stops starting work and waits for every active Run to finish. The
systemd unit uses `TimeoutStopSec=infinity`, so systemd does not convert that
drain into a later forced kill.

Finite bounds remain on mechanical suboperations whose purpose is to prevent a
transport or cleanup primitive from wedging the Kernel: Poll commands, reserved
startup garbage collection, Runner cleanup after a completed or canceled Run,
post-dispatch Audit, and bounded output retention. Those bounds do not limit
agent reasoning or model execution.

## Consequences

- Long-running agents retain their worktree, Pi process, evidence stream, and
  provider session until they complete.
- A wedged agent requires explicit operator cancellation; elapsed time alone
  never authorizes the Kernel to kill it.
- A service restart or installer update waits for active Runs. Operators who
  choose to force-kill the service are making an explicit destructive decision.
- Harness improvements need progress observability, protocol-constrained tools,
  and regression evals. A larger timeout is not an accepted repair.
- Worktree separation and explicit runtime inputs remain operational isolation;
  they are not a security sandbox.

## Amendment: optional progress watchdog (2026-08-23)

The no-deadline decision remains the default. A repository may opt one
declaration back into a wall-clock bound for liveness by setting the optional
`max_duration` key (seconds) under that agent in `forest.yaml`. Zero or an
omitted key leaves the Run unbounded.

When set, the Kernel's progress watchdog cancels a Run that exceeds the bound.
It writes the durable cancellation marker and stops the Run process group
through the same supported cancel path as `forest run cancel`. The Runner then
records the cancellation in the Ledger with the same cause, and the Scheduler
returns the trigger to its not-running state so the Subject can be claimed
again. This is a liveness guard against wedged Runs, not a reasoning budget.
