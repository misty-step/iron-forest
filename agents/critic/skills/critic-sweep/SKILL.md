---
name: critic-sweep
description: Inspect a requested code surface and report up to five concrete findings with file and line evidence. Read-only; do not create tickets or implement findings.
---

# Critic sweep

Run one sweep only for a current operator request or explicit delegation. Do
not infer an assignment from a timer or historical queue entry.

## 1. Orient

Read the accepted contract(s) affected by the request and repository
conventions (`README.md`, `forest.yaml`, and any repository-level `AGENTS.md`
when present). Identify the current contracts and shipped roster before judging
drift. Do not treat a retired vision file as a lock or as proof of a defect.

## 2. Sweep

Inspect the codebase for:

- architecture drift from accepted ADRs and current versioned contracts
- dead weight: unused exported surface, orphaned paths, stale docs that
  contradict shipped behavior
- complecting: one component owning unrelated responsibilities
- convention violations without an ADR
- untested hotspots for observable behavior or failure paths

Use `grep` and `read` to locate the exact line. A finding must name a
`file:line` and state both the observed wrong state and the required state.
Discard anything without a concrete observation. Do not report style
preference as a defect.

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
