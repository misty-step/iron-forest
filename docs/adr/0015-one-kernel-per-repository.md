# 0015 — One Kernel per repository

Status: accepted, 2026-08-10

## Context

Two Kernel processes for one repository would race on Polls, worktrees, and
local Run state. Day-one coordination does not provide a distributed claim
protocol for multiple instances or organizations.

## Decision

Run exactly one live Kernel checkout and process per repository. An OS lock
rejects a second process in the same checkout. This lock does not coordinate a
different clone or checkout, so the deployment contract forbids those duplicate
instances. The process may dispatch several declarations, but it serializes at
most one live Run per declaration. Multi-instance and multi-organization Git
claims are deferred to a follow-up Issue.

This decision supersedes the earlier organization-wide installation scope.

## Consequences

A repository has one scheduler and one local status view. Worktree preparation
and local Ledger writes cannot race between Kernel processes. Multiple
repositories use separate Kernel processes and remain isolated.

A second process cannot provide day-one horizontal scaling for one repository.
Operators must use the repository's configured Kernel process until a Git-claim
protocol is evaluated and accepted.
