# 0003 — No protected paths

Status: accepted, 2026-08-07

## Context

`forest.yaml` declared a `protected` list — `.forest/`, `forest.yaml`,
`agents/`, `.opencode/opencode.json` — and the gate rejected any run whose
change touched one. The intent was to stop an agent from rewriting its own
instructions, permissions, or budget during unrelated work.

Four facts undermined it.

**It was not a security boundary.** `gate.go`, which implements the check, was
itself unprotected. So was `main.go`, `daemon.go`, and every other source file.
An agent motivated to escape the list could delete the list, or delete the code
reading it. A protection an agent can edit protects nothing.

**One entry was structurally dead.** `.gitignore` excludes `/.forest/`, so it
can never appear in a commit. The gate also skipped it by prefix before the
protection check ran. Two mechanisms guarded a path a third made impossible.

**It could be dodged by accident.** `gitOut` trims its output, and
`parseChanged` indexed the porcelain by column. Porcelain leaves the first
column blank for an unstaged change, so the first modified file arrived as
`" M path"`, lost its leading space to the trim, and then lost the first
character of its path to the column offset. `agents/x` became `gents/x`, which
matched no protected prefix. The first modified file in every run was exempt.
That bug is fixed in the same change as this decision.

**It blocked the factory from working on itself.** Iron Forest builds Iron
Forest. Cards that rename an agent, change a prompt template, or adjust a check
are ordinary work, and all of them land in protected paths. The cost was
recurring and rising:

- Two agent renames required the operator to temporarily relax the list, then
  restore it.
- Item #149 spent three Fixer attempts discovering that `agents/` was not
  writable, after which the operator made the change by hand.
- The list could not be deleted by the factory, because deleting it requires
  writing `forest.yaml` and `gate.go` in one change — and `forest.yaml` was
  protected. The rule prevented its own removal.

## Decision

Delete protected paths. There is no `protected` key in `forest.yaml`, no
`Protected` field in `Config`, and no path check in the gate. An agent run may
change any file in the repository it is working, including the factory's own
declarations.

The boundaries that remain are the ones that were doing the work:

- **Independent review on the exact commit.** A different agent, on a different
  model family, reviews the precise SHA and writes a write-once verdict note.
- **The deterministic gate.** The agent must not commit, must produce a real
  change, and must write a `report.json` that satisfies its declared schema.
- **Declared checks.** Build, vet, and test run in the worktree, and their
  result is a note on the commit.
- **The merge gate.** Auto-merge requires an approving verdict and green checks.
- **Authority bounded by the worktree.** A run holds no credential of its own
  and has a private home outside the worktree.

## Consequences

**A careless run can now propose a change to its own declarations.** It will be
reviewed like any other change. The operator reads the diff before it merges, or
auto-merge requires an approving independent verdict first.

**A malicious run is no worse off than before.** It could already edit the
enforcement code. This decision removes the appearance of a boundary rather than
a boundary.

**The factory can maintain itself.** Renames, prompt changes, check changes, and
agent declaration changes are now ordinary cards with no operator relaxation
step and no manual follow-up commit.

**Erasure.** 19 non-test lines, 12 test lines, one config field, one YAML block,
and one function (`isProtected`). `parseChanged` lost its second return value,
which existed only to feed the rename side of this check.

**Open risk, accepted.** An approving verdict is currently produced by one
reviewer. If that reviewer is wrong about a change to `agents/`, the next run
inherits it. The mitigation is that the reviewer runs a different model family
from the builder, and that agent declarations are data the operator can read in
one diff. If this becomes a real failure, the answer is a stronger review
requirement for declaration changes — not a list of forbidden paths.
