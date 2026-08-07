# 0002 — Write-once facts replace leases

Status: accepted, 2026-08-07

## Context

Iron Forest coordinated work with remote leases: a blob ref under
`refs/forest/lease/<key>`, created by compare-and-set, with a 7200 second time to
live and a takeover path that deleted an expired holder and recreated the ref.

A time to live is a guess about whether a holder is still alive. That guess forces
a second requirement: a run must not outlive its lease, so every agent phase and
every declared check needs a deadline. That in turn forces a third: a stale lease
needs recovery, alerting, and a forced-stop path. Three backlog cards existed only
to serve the guess.

The lease bought one property that nothing else provided: two hosts working one
repository would not duplicate work. `VISION.md` promised that capability. No
operator ran it. ADR 0001 makes one installation per organization, one process per
checkout, so it never will.

## Decision

Delete the remote lease layer. Coordinate with facts instead of locks.

- Exclusion inside one process is an in-process subject set. `.forest/daemon.lock`
  already guarantees one process per checkout.
- A verdict on an exact commit is written once. `git notes add` runs without `-f`,
  so a second writer is refused by git itself. The loser reads the winning note
  and continues from it, because a verdict about the identical commit is equally
  valid.
- Branch publication keeps `--force-with-lease` on the observed remote tip. That
  is git's own flag and still correct: it makes a lost race fail cleanly.
- Pull requests stay idempotent by listing before creating. Merges are already
  idempotent.
- The repeat-failure brake moved from the host-local ledger to
  `refs/forest/stalled/<flow>/<key>`, so selection no longer depends on one host's
  files.

Check timeouts survive as cost knobs. They carry no correctness weight,
because no lease can expire. `budget_seconds` was later deleted with the step
ceiling in `99b3b74`; nothing now bounds a run's length.

## Consequences

Measured erasure: 7 types, 13 functions, 2 config fields, and roughly 530 lines
including tests. Backlog cards for lease exclusivity under concurrency, bounding
every act below the lease time to live, and forced-stop lease recovery became
moot and were closed.

An immutability rule replaced a mutual-exclusion rule. It is smaller, and it holds
without any liveness estimate.

What is genuinely given up: duplicate agent work becomes possible if two processes
ever share a repository, costing about $0.0072 per item; and the human-facing
comments on issues and pull requests are not idempotent, so a duplicate process
could post a duplicate comment. Local issue/PR comment idempotency is card #148. Remote runner replay under cloud packaging remains #82.

`VISION.md` no longer promises two hosts on one repository. Any future need for it
must re-derive coordination from write-once facts, not reintroduce a timed lock.
