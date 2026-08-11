# 0012 — Poll trigger protocol and error semantics

Status: accepted, 2026-08-10

## Context

The Kernel must wake declarations without passing hidden selection context or
embedding tracker policy. A small executable trigger gives each profile an
explicit interface and keeps Subject selection in the agent Run.

## Decision

A Poll trigger is an executable plus an interval. The Kernel gives every Poll
a fixed 60-second deadline and reads only its exit result. The declaration's
configured timeout separately bounds worktree preparation and agent execution;
it cannot extend the Poll deadline. Exit 0 means work exists and dispatches the
declaration. Exit 1 means no work and is a healthy skip. Exit greater than 1,
deadline expiry, or malformed behavior is an error: skip the tick, log loudly,
count consecutive failures, and show the unhealthy trigger in `forest status`.

A Poll answers yes or no and passes no context to the agent. The shipped
subcommands are `forest poll builder`, `forest poll verifier`, and
`forest poll fixer`. Builder checks a ready Issue without a matching branch;
Verifier checks a branch tip with a review request and no Verdict; Fixer checks
a `changes` Verdict on the branch tip.

## Consequences

Profiles can use any executable that obeys the exit contract. Poll failures do
not fabricate work and do not stop unrelated declarations. Operators can see
persistent trigger failures in status.

The Kernel does not parse trigger output or use it as agent context. A trigger
that needs richer data must let the agent select that data during its Run.
