# 0005 — Runner injects per-declaration commit identities

Status: accepted, 2026-08-10

## Context

Repository history must show which declaration authored a commit. Commit
metadata and the authenticated actor that pushes a branch are separate Git
identity layers.

## Decision

When the Runner invokes Pi, it injects the active declaration's `user.name` and
`user.email` through Git's command-scope `GIT_CONFIG_*` environment protocol.
It does not mutate shared Git configuration during worktree preparation and
does not set the stronger `GIT_AUTHOR_*` or `GIT_COMMITTER_*` overrides. For
example, Builder receives `Iron Forest Builder` and `builder@forest.invalid`.
Git commands run by Pi inherit that identity, while a nested command can still
override a fixture identity with `git -c`. Coordination note commits therefore
use the declaration identity. The Auditor checks each note identity at the
exact target note-tree path. It does not validate the reviewed Revision's
commit author.

The commit author identifies the declaration. The account that pushes a branch
or opens a Projection remains the authenticated forge actor. Do not treat one
identity as the other.

## Consequences

`git log` attributes commits to Builder, Verifier, or Fixer without extra forge
accounts. Test and tool subprocesses can use explicit command-scoped identities
without being overridden by the parent Run. The Auditor detects an unexpected
coordination note identity, not an unexpected Revision commit author.
Deployment may use a separate forge account,
but that is outside this ADR.
