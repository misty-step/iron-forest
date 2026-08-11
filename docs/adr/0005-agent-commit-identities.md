# 0005 — Runner injects per-declaration commit identities

Status: accepted, 2026-08-10

## Context

Repository history must show which declaration authored a commit. Commit
metadata and the authenticated actor that pushes a branch are separate Git
identity layers.

## Decision

When the Runner invokes OMP, it injects `GIT_AUTHOR_NAME`,
`GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`, and `GIT_COMMITTER_EMAIL` for the
active declaration. It does not mutate Git configuration during worktree
preparation. For example, Builder receives `Iron Forest Builder` and
`builder@forest.invalid`. Git commands run by OMP inherit that identity, so
coordination note commits use it. The Auditor checks each note identity at the
exact target note-tree path. It does not validate the reviewed Revision's
commit author.

The commit author identifies the declaration. The account that pushes a branch
or opens a Projection remains the authenticated forge actor. Do not treat one
identity as the other.

## Consequences

`git log` attributes commits to Builder, Verifier, or Fixer without extra forge
accounts. The Auditor detects an unexpected coordination note identity, not an
unexpected Revision commit author. Deployment may use a separate forge account,
but that is outside this ADR.
