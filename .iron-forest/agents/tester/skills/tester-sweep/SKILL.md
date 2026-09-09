---
name: tester-sweep
description: Inspect a requested code surface and report up to five observable test gaps with evidence and acceptance criteria. Read-only; do not create tickets or implement findings.
---

# Tester sweep

Run one sweep only for a current operator request or explicit delegation. Do
not infer an assignment from a timer or historical queue entry.

## 1. Orient

Read the accepted contract(s) affected by the request and repository
conventions (`README.md`, `.iron-forest/config.yaml`, and any repository-level `AGENTS.md`
when present). Identify user-visible surfaces (CLI commands, configuration, Gate
and evidence boundaries) named by the request before looking for gaps.

## 2. Sweep

Find under-tested OBSERVABLE behaviors only:

- boundaries: empty input, empty config, missing values, limits
- transitions: state changes a user can trigger (idle to running, open to
  closed, live to done)
- error paths: invalid CLI form, missing tools, conflicts, and failures users
  actually hit

Never propose implementation-unit tests for internal helpers, and never chase
raw coverage. Use `grep` and `read` to locate the exact surface and its current
tests. A finding must name a specific surface (`file:line` or a concrete
command path) and state both the observed untested behavior and the required
test. Discard anything without that concrete observation.

## Report

Check existing review evidence and active work for duplicate findings. Return
at most five findings in the session or requested report. Name the repository,
inspected revision, exact file and line or command path, observed state,
required state, and verification evidence. For a test gap, include a concrete
failing example and acceptance criteria. Label unknown runtime facts.

Report checked surfaces, skipped duplicates, and unsupported hypotheses that
were discarded. A clean sweep reports no findings and the evidence checked.
Do not create tickets, start implementation, edit code, publish Git evidence,
or promote a finding into work. The operator selects subsequent work.
