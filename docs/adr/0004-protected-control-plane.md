# 0004 — The Gate rejects the factory's control plane

Status: accepted, 2026-08-07

Supersedes: 0003

## Context

Item #171 makes the delivery process a **durable machine**: states and illegal
transitions are explicit, and a Flow may only do what the state machine allows.
A machine's control plane is where its rules, prompts, permissions, and
composition live. In Iron Forest that is `.forest/`, `forest.yaml`, `agents/`,
and `.opencode/opencode.json`.

ADR 0003 deleted the Gate's protected-path check, arguing it was not a security
boundary (the enforcement code was itself writable) and that it blocked the
factory from maintaining its own declarations. Both facts were true, but they
answered the wrong question. The delivery machine does not rely on protected
paths as a *security* boundary — the independent reviewer on the exact commit
is the audit surface. The machine relies on protected paths as a **safety**
invariant: a Build run must not, as an incidental consequence of unrelated work,
rewrite the very rules and prompts the daemon runs under, because that is how a
machine's own levers get pulled. That invariant is worth keeping regardless of
whether it also defends against a malicious run.

## Decision

Restore the Gate rejection and make it an invariant of the delivery machine:

- `gate.go` refuses any Build run whose changed files touch `.forest/`,
  `forest.yaml`, `agents/`, or `.opencode/opencode.json`, before trusting
  anything the report claims.
- The rule is documented in `docs/fsm.md` (invariant 4) and enforced by the
  deterministic Gate, not by a permissive list an agent reads and answers to.

The factory still works on its own control plane: a card that renames an agent
or edits a prompt is real work, and it is done the same way — a Builder run that
touches `agents/` is rejected by the Gate, so the operator or a dedicated change
makes it deliberately, and the independent reviewer on the exact commit audits
it. The rejected-path list is not a bypass of review; it forces a change to the
control plane to happen on purpose, and always through review.

## Consequences

- A careless Build run can no longer silently rewrite `forest.yaml`, its agent
  declaration, or the harness wiring as a side effect of unrelated work; the
  Gate refuses the run.
- The factory can still maintain its own declarations: the operator makes a
  deliberate, reviewed change, matching the delivery machine's `fail`/`merged`
  halt-and-human conventions rather than an invisible auto-edit.
- Agent worktrees never touch the control plane, so the Gate, the prompts that
  sit behind it, and the review that approves a change all stay consistent
  across a run.

## Result

The protected-path list is a machine invariant, not an afterthought: `transit`
and the Gate both encode what a Flow may not do, and both are under table-driven
tests that fail when a Flow skips the boundary.
