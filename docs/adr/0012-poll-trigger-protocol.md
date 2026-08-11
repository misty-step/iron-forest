# 0012 — Poll trigger protocol and error semantics

Status: accepted, 2026-08-10

## Context

The Kernel must wake declarations without passing hidden selection context or
embedding tracker policy. A small executable trigger gives each profile an
explicit interface and keeps Subject selection in the agent Run.

## Decision

A Poll trigger is an executable plus an interval. Direct `forest poll`
execution has a fixed 60-second deadline. The Scheduler gives its configured
Poll command a separate 65-second bound. This 5-second allowance lets the
direct Poll cancel and drain its Git/GitHub transport process groups before the
supervisor stops its command group. The declaration's configured timeout
separately bounds worktree preparation and agent execution. It cannot extend
either Poll bound. Exit 0 means work exists and dispatches the declaration.
Exit 1 means no work and is a healthy skip. Exit greater than 1, deadline
expiry, or malformed behavior is an error. The Kernel skips the tick, logs the
error, counts consecutive failures, and shows the unhealthy trigger in
`forest status`.

Verifier and Fixer Poll enumeration is bounded. Each parses at most 500 entries
of a canonical notes tree. A larger tree or a note-enumeration
transport-output overflow is no work: the Poll exits 1 with an explicit log
line and never reports an operational error. The Auditor reports durable note
growth or canonical note corruption as bounded persisted policy violations and
a non-pass Audit result, never an AuditError. For a Poll, malformed note tree
rows inside the bound and transport errors other than overflow stay operational
errors (exit 2).

A Poll answers yes or no and passes no context to the agent. The shipped
subcommands are `forest poll builder`, `forest poll verifier`, and
`forest poll fixer`. Builder checks a ready Issue without a matching branch;
Verifier checks a branch tip with a review request and no Verdict; Fixer checks
a `changes` Verdict on the branch tip.

## Consequences

Profiles can use any executable that obeys the exit contract. Poll failures do
not fabricate work and do not stop unrelated declarations. Trigger health
persists separate Poll, Run, and Audit errors. A healthy Poll clears only its
Poll error. A successful Run clears only its Run error. A successful Audit
clears only its Audit error. Operators see every current cause in status, even
when the durable audit state still shows an earlier success.

The Kernel does not parse trigger output or use it as agent context. A trigger
that needs richer data must let the agent select that data during its Run.
