# 0002 — Write-once facts replace leases

Status: accepted, 2026-08-07; amended, 2026-08-08

## Context

Iron Forest coordinated work with remote leases: a blob ref under
`refs/forest/lease/<key>`, created by compare-and-set, with a 7200 second time to
live and a takeover path that deleted an expired holder and recreated the ref.

A time to live is a guess about whether a holder is still alive. That guess forces
a second requirement: a run must not outlive its lease, so every agent phase and
every declared check needs a deadline. That in turn forces a third: a stale lease
needs recovery, alerting, and a forced-stop path. Three backlog cards existed only
to serve the guess.

The original decision assumed one process per checkout. Manual runs and separate
checkouts later proved that one repository can have concurrent participants.
Coordination is required, but a clock-based takeover is still unsafe.

## Decision

Delete the timed remote lease layer. Use untimed admission for active Effects and
durable facts for completed decisions.

- Effect admission takes a non-blocking, per-owner file lock and creates
  `refs/forest/claim/<key>` with compare-and-set. One canonical Item key joins
  Builder, Verifier, Fixer, and retirement Subjects across processes and
  checkouts. A same-Host stale claim is replaced only while holding the lock.
  A foreign-Host claim fails closed.
- A verdict on an exact commit is written once. `git notes add` runs without `-f`,
  so a second writer is refused by git itself. The loser reads the winning note
  and continues from it, because a verdict about the identical commit is equally
  valid.
- Branch publication keeps `--force-with-lease` on the observed remote tip. That
  is git's own flag and still correct: it makes a lost race fail cleanly.
- Pull-request publication validates one exact Projection before creation.
  Ambiguous or foreign Projection identity is refused.
- The repeat-failure brake moved from the host-local ledger to
  `refs/forest/stalled/<flow>/<key>`, so selection no longer depends on one host's
  files.

Agent deadlines and check timeouts are resource and shutdown bounds. They do not
expire admission because an admission has no time to live.

## Consequences

The initial change erased 7 types, 13 functions, 2 configuration fields, and
roughly 530 lines including tests. It removed every clock-based ownership guess.

Later admission added one smaller boundary when concurrent processes and
checkouts became real. A process crash releases its operating-system lock. The
next same-Host participant can replace the stale claim. A foreign-Host claim
requires operator repair, which prefers stopped work to split ownership.

Decision facts remain immutable. Active ownership is explicit and has no expiry
or takeover clock.
