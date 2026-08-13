# 0019 — Harness profile composition

Status: accepted, 2026-08-13

Extends [0011](0011-kernel-profile-boundary.md) and [0018](0018-pi-harness.md).
The declaration format in [0013](0013-omp-harness-and-declarations.md) gains
optional `env` and an optional per-declaration profile directory. `model` is no
longer required on every declaration.

## Context

ADR 0018 made `pi` the harness and trusted project-local configuration so a
repository skill could load. That decision left two holes.

The first is identity. A single host profile is shared by every declaration.
A Builder skill is then visible to Verifier, and a credential in that profile
is visible to every Run.

The second is defaults. Every declaration restated the same model string. An
operator who wants one fleet default, with a repository override, had nowhere
to put it.

Credentials must not live in the repository. They stay in the operator's base
profile or host environment. The Kernel never prints a declared env value.

## Decision

The Kernel materializes one harness profile per Run, then points the child at
it with `PI_CODING_AGENT_DIR`. Layers copy in this order; a later file of the
same relative path replaces an earlier one:

1. The operator's base profile. The `profile` key in `forest.defaults.yaml`
   names it. `$FOREST_DEFAULTS` may name a different defaults file; it never
   names a profile directory. Otherwise the host Pi profile
   (`$PI_CODING_AGENT_DIR` or `~/.pi/agent`) is used. This is the only layer
   that may hold credentials.
2. `agents/_shared/profile/`, the repository layer every declaration shares.
3. `agents/<name>/profile/`, the declaration's own layer.

Repository layers are checked, not trusted: no `auth.json`, no symlink, and
nothing but regular files and directories. A declaration that ships a bad
layer fails at load, before any Run starts. The Kernel refreshes selected-model
OAuth in the shared base before copying it, and omits host session history.

The model resolves through three layers: the declaration, then instance
defaults, then the built-in `openrouter/deepseek/deepseek-v4-flash-0731`.
`thinking` resolves through the declaration, then instance defaults; there is
no built-in thinking level. An empty or comment-only defaults file is the
zero Defaults, not an error. Local models are available by declaring one;
they are never the default. `forest declaration show` and the Run log's
`forest.run` line publish the resolved model and the layer that supplied it.
JSON publication of a declaration omits env values.

Declared `env` is a map of opaque string scalars. Names the Kernel owns
(`PATH`, `HOME`, `FOREST_RUN_ID`, `PI_CODING_AGENT_DIR`, and the Git identity
variables) are rejected. The read surface and the Run evidence publish keys,
never values.

The profile directory is collected with the worktree through the bounded
trusted remover. Reserved startup GC sweeps leftovers by the same path.
Composition rejects any file over 16 MiB or any total above 4,096 files,
64 MiB, or a 512 KiB evidence manifest.

## Consequences

- A Builder skill lives in `agents/builder/profile/` and is invisible to
  Verifier. A shared skill lives in `agents/_shared/profile/`.
- An operator sets one fleet model in `forest.defaults.yaml` or
  `$FOREST_DEFAULTS`. A repository still overrides it.
- `forest selfcheck` publishes the defaults file it loaded.
- Replacing the harness still means changing one command shape. The profile
  directory is a filesystem the new harness must honor, not a Kernel secret.
