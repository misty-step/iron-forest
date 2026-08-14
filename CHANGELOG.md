# Changelog

- 2026-08-13: Replaced layered Pi profile composition with explicit per-Run
  inputs: an empty temporary Pi directory, checked-in shared and role skill
  directories, and disabled ambient extension/resource discovery. The service
  now requires a protected per-instance credential environment file and uses a
  private temporary namespace; the installer removes credential-bearing legacy
  Run-profile residue during cutover. Declaration output and Run evidence
  publish `skills`; declaration `env` and the obsolete `profile_files` surface
  are removed. These breaking changes advance CLI envelopes to `forest.cli.v2`.
  Terminal Pi agent errors now fail the Run even when Pi exits zero. Per-Run
  Git identities use scoped Git configuration rather than author/committer
  overrides, so nested verification commands can set deterministic identities.

- 2026-08-10: Reforged Iron Forest as a Kernel plus profile appliance. Git is
  the coordination authority with schema-v1 write-once notes, agent-owned
  Effects, an evidence-first fast-forward Gate, and a read-only Auditor. The
  Builder, Verifier, and Fixer declarations use OMP files under `agents/`,
  Polls use explicit exit semantics, and one Kernel serves each repository.
  Forge adapters start with GitHub. Stronger isolation targets the deployment
  substrate, and evals remain the instrument for future actor-boundary changes.

Current behavior is defined by `README.md`, the shipped declarations, and the
accepted ADRs.

Historical pre-reforge entries remain in repository history before 2026-08-10.
