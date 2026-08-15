# Changelog

- 2026-08-15: Builder and Fixer publish review-requests through
  `forest publish review-request`. The Kernel owns the write-once note, role
  identity, configured Check gate, and bounded atomic retry. Shipped default
  model is `openrouter/deepseek/deepseek-v4-pro-0813`.

- 2026-08-13: Dispatch now verifies the agent bundle. The Kernel digests the
  ordered declaration pair (`agent.md` then `task.md`) at load and recomputes
  that digest immediately before starting Pi; a file changed after load aborts
  the Run with a nonzero-exit Ledger row and refuses to start Pi. The Ledger
  records the digest only after that verification succeeds (#144).

- 2026-08-13: Removed per-agent wall-clock deadlines. `forest.yaml` no longer
  accepts `timeout`; the Runner does not create a deadline around preparation
  or Pi execution; and the systemd service drains active Runs indefinitely.
  Explicit foreground cancellation and bounded mechanical cleanup remain.

- 2026-08-13: Replaced layered Pi profile composition with explicit per-Run
  inputs: an isolated temporary Pi directory, checked-in shared and role skill
  directories, and disabled ambient extension/resource discovery. For an
  OpenRouter model, the temporary directory contains only a generated,
  credential-free session-affinity override. The service
  now requires a protected per-instance credential environment file and uses a
  private temporary namespace; the installer removes credential-bearing legacy
  Run-profile residue during cutover. Declaration output and Run evidence
  publish `skills`; declaration `env` and the obsolete `profile_files` surface
  are removed. These breaking changes advance CLI envelopes to `forest.cli.v2`.
  Terminal Pi agent errors now fail the Run even when Pi exits zero. Per-Run
  Git identities use scoped Git configuration rather than author/committer
  overrides, so nested verification commands can set deterministic identities.
  Pi uses the exact Run ID as its provider session ID; the generated OpenRouter
  override sends it as `x-session-id` for trace correlation.

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
