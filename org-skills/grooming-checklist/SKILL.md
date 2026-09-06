---
name: grooming-checklist
description: >
  Groom a current request into a bounded work brief during an
  operator-supervised planning session. Turns a raw idea into machine-checkable
  acceptance criteria and a verification path before the factory consumes it.
---

# Grooming checklist

Use this skill only in an operator-supervised session, not as a factory
declaration. The factory never auto-loads `org-skills/`; an operator passes
this directory to a supervised `pi` session explicitly.

## Goal

A groomed Subject is self-contained: a fresh Builder can select it, implement
one bounded change, and run a deterministic verification path with no additional
operator context.

Read the current request and current code first.

## Grooming loop

For each backlog item:

1. **Restate the problem.** Write it as an observable defect or concrete need.
   Remove solution language (`add a flag`, `refactor X`) unless the operator has
   already bound that solution and it is required.
2. **Collect or reject a repro.** For a defect, record exact steps plus the
   current observed result. For a feature, record the concrete scenario and its
   inputs. If the operator cannot provide one, send the item back as not ready.
3. **Bound the scope.** Write `In scope` and `Out of scope`. Split an EPIC into
   one bite-sized Subject per step; do not groom an EPIC straight into ready.
4. **Rewrite acceptance criteria.** One machine-checkable statement per line.
   Every criterion names a command, or one disqualifying observation, and its
   expected pass result.
5. **Add a verification path.** State the exact command(s) and expected exit or
   output. The Verifier will run the same path.
6. **Return the brief.** Report scope, acceptance criteria, and verification in the session or requested artifact; do not create a ticket.
7. **Self-check.** Apply the red flags and fix anything that trips one before
   asking the operator to approve.

## Red flags (rewrite or send back)

- No machine-checkable acceptance criterion.
- Verification path missing, or it cannot be run by an agent with git access.
- Scope has no `Out of scope` line.
- A criterion uses only unmeasurable words (`better`, `cleaner`, `robust`,
  `improved`) without a measurable bound and a command.
- EPIC-sized work presented as a single Subject.
- The operator must be consulted after selection to understand the work.

## Boundaries

Grooming is human-supervised
([ADR 0014](../../docs/adr/0014-agent-roster.md)). Work only on a current operator request; do not create speculative tickets. Do not implement
the Subject in this skill. Do not edit Kernel code or factory declarations here.

## Output

A current work brief with a problem, repro, scope bound,
machine-checkable acceptance criteria, and a verification path.