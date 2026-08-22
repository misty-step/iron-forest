# `forest:ready` contract

Status: convention for the Tracker backlog. Human-supervised grooming enforces it
during backlog sessions; the Kernel does not lint or reject a weak spec.

A Subject is **ready** when a Builder that has never seen the conversation can
read the backlog item, implement one bounded change, run a deterministic
verification path, and publish a Revision with no additional operator context.

`forest:ready` applies to both Tracker forms:

- a GitHub Issue labeled `forest:ready`; or
- a takeable Powder job whose `repo` is `misty-step/iron-forest`.

## Required sections

A ready Subject carries six sections. A missing or weak section is not ready.

1. **Problem** — the observable defect or concrete need. State what is wrong in
   workflow terms, not the chosen fix.
2. **Repro** — exact steps that reproduce the current behavior, or a concrete
   scenario the change must serve. For a defect, include inputs, commands, and
   the observed current result.
3. **Scope bound** — explicit `In scope` and `Out of scope`. A Subject with no
   out-of-scope line invites the branch to expand.
4. **Acceptance criteria** — machine-checkable statements, one per line. See the
   next section.
5. **Verification path** — the exact command(s) and expected result that prove
   the acceptance criteria. The Builder runs it before publication; the Verifier
   runs it during review.
6. **Evidence** — optional links, logs, and related Issues or jobs. It supports
   the sections above; it never replaces the repro or the verification path.

## Machine-checkable acceptance criteria

Each criterion names a command (or a single manual observation with a stated
disqualifying result) and the expected pass result. A Builder can turn it into
a true/false check: run `<command>`, observe `<expected result>`, pass or fail.

Good:

- `go test ./...` exits 0.
- `forest audit show` reports `violations: []` and `last_result: pass`.
- `git ls-remote origin 'refs/heads/forest/if-296/*'` returns no lines.

Reject:

- "The CLI is clearer."
- "Error handling is improved."
- "The factory runs faster."
- "Refactor the module."
- "Better UX."

A criterion may use words such as _faster_ or _clearer_ only when it also states
the measurable bound and the command that measures it.

## Verification path

Prefer one command, or a short sequence of commands, that a Verifier can run in
one go. List each command and its expected exit code or output. If the behavior
needs manual observation, name the exact condition that fails the Subject so the
pass/fail decision stays deterministic.

For a Powder job, put the verification path directly in the spec. For a GitHub
Issue, put it in the issue body.

## Scope bound

State what the Revision may touch and what it must not touch. An EPIC is not a
ready Subject; split it into one bite-sized Subject per step before grooming. A
selection-ready Subject is the smallest change that satisfies one acceptance set
and can be reviewed atomically.

## Zero additional operator context

A ready Subject is self-contained. After selection, the Builder does not need
comments, chat, telemetry dashboards, or host access to understand the work.
Every required command, file path, and acceptance bound is in the item.

## Enforcement

Grooming is human-supervised, not a fourth declaration
([ADR 0014](adr/0014-agent-roster.md)). The factory remains a dumb consumer:
Builder Poll checks only that a Powder spec is nonempty, that `repo` matches
`forest.yaml`, and that no `forest/<id>/*` branch exists. It does not judge spec
quality, and the Kernel does not lint it.

Operators apply this contract during supervised backlog sessions with the
grooming checklist skill and the Powder spec template:

- [`../org-skills/grooming-checklist/SKILL.md`](../org-skills/grooming-checklist/SKILL.md)
- [`templates/powder-job-spec.md`](templates/powder-job-spec.md)

A future read-only readiness lint is out of scope for this contract. It may only
aid the same supervised session; it must not become a factory gate.

## Poll scope is not selection permission

A scoped Poll wakes a declaration only for in-scope Subjects; it never widens
the declaration's own selection rules. When `forest.yaml` sets
`scope.subjects`, that list is the complete Builder allowlist, and a Run must
not claim or work any other Subject even if that Subject is otherwise eligible.
The declaration prompt is the enforcement point inside the Run, not the Poll.