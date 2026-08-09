# 0006 — The supervisor restarts after self-update

Status: accepted, 2026-08-08

## Context

A daemon can receive a termination signal while its updater prepares an executable handoff. An in-process `exec` can consume that signal in the old image, replace the signal handler, and lose the shutdown request.

The updater already takes the update gate only while every Flow Effect is idle. Deployed services use `Restart=always` and start the binary from each managed repository.

## Decision

Self-update builds and smoke-tests `forest.next` while the update gate is held. It atomically replaces the installed binary, records the update, and exits the current process.

The service supervisor starts the installed binary. Iron Forest does not replace its process image with `exec` during an update.

A termination signal can race with installation, but it cannot be lost through process replacement. Both paths stop the old process. A hard stop kills managed process groups and exits without waiting for blocked repository I/O. The next startup reaps linked worktrees before any Flow starts.

## Consequences

The deployed service restarts on the tested binary without an in-process signal handoff. Update and shutdown now converge on process exit.

A manually started daemon that uses `--factory-dir` exits after installation. Its operator must restart it, or run it under a supervisor.
