# Changelog

- 2026-08-22: Critic and Tester are EXPERIMENTAL and local-canary-only.
  They stay enabled only in the self-host Iron Forest checkout for canary
  observation; external operators must not copy or enable them until the
  rollout exit gate closes (blockers merged, corrected deterministic evals
  pass, one post-fix live sweep per role produces attributable spec-less
  drafts).
- 2026-08-19: One Subject identity. Review-request is only v2
  (`subject` + `forest/<id>/<slug>`). Builder Poll lists GitHub Issues
  and Powder jobs. The Kernel does not take or complete them (ADR 0023).
  Leftover hyphen tips are unread by Poll.
- 2026-08-18: A second `forest run cancel` after the Runner records the
  Ledger row reports `already_finished` instead of not-found (#289).
- 2026-08-18: Poll and Auditor drop leftover notes-era read machinery.
  They still ignore `refs/notes/forest/*`. Publish still dual-writes
  the request note (#287).
- 2026-08-18: Gate proof on disposable repo `misty-step/forest-gate-127`.
  A non-compiling Revision produced failing Checks, a `changes` Verdict,
  unchanged `master`, and Run `1787065161454179061-verifier`. Transcript
  in `VERIFY.md` (#127).

- 2026-08-17: Onboarding states which identity may create or update which
  Git ref. A read-only forge credential is refused. Branch protection cannot
  see evidence refs (#255).

- 2026-08-17: Poll and Auditor read `refs/forest/v1/*`. Leftover notes are
  unread. Verifier calls `forest publish verdict`. Builder/Fixer dual-write
  a request evidence ref with the existing note (#279).

- 2026-08-17: `forest publish verdict` owns Checks and Verdict evidence refs
  (`refs/forest/v1/checks/<sha>`, `refs/forest/v1/verdict/<sha>`). Approve
  runs configured Checks and fast-forwards `master` in the same atomic push
  (#238, #278, ADR 0022).


- 2026-08-17: VISION names evidence refs `refs/forest/v1/*` and Kernel
  `publish verdict` as the destination (#277). The running binary still
  follows ADR 0021 until #238 and #278. Onboarding states one Kernel per
  repository per machine (#248). `audit show` prints `audited_master` as
  `master=` and keeps `last_master` as the last-good ancestry tip (#264).
  Human Run rows lead with `exit` and `duration` (#263).

- 2026-08-15: Recorded the product lock in `VISION.md`. One Kernel serves
  one repository on one machine. The CLI is the operations surface. The host
  vendor is an operator choice. Mint, Powder, Habitat, Fly Sprites, and a
  dashboard are out of product.

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

- 2026-08-10: Reforged Iron Forest as a Kernel plus declarations. Git is
  the coordination authority with schema-v1 write-once notes, agent-owned
  Effects, an evidence-first fast-forward Gate, and a read-only Auditor. The
  Builder, Verifier, and Fixer declarations use files under `agents/`,
  Polls use explicit exit semantics, and one Kernel serves each repository.
  Evals remain the instrument for actor-boundary changes.


Current behavior is defined by `VISION.md`, `README.md`, the shipped
declarations, and the accepted ADRs.


Historical pre-reforge entries remain in repository history before 2026-08-10.
