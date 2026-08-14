# 0019 — Explicit Pi runtime inputs

Status: accepted, 2026-08-13

Extends [0011](0011-kernel-profile-boundary.md) and
[0018](0018-pi-harness.md). The declaration read surface gains the resolved
repository skill paths. `model` is no longer required on every declaration.

## Context

Pi normally discovers state from its agent directory and the current project.
Project `AGENTS.md` and other context files are trusted Run-worktree inputs and
remain discoverable. An installed extension, operator skill, prompt template,
or theme is different: it is ambient state outside the declaration. Sharing
the operator's Pi directory can also expose mutable session state or one role's
resources to another.

The factory needs one inspectable process contract without disabling trusted
project context. Skills must be selected by the declaration, and unrelated
ambient resources must be excluded. Credentials must not live in the
repository; the service supplies them as inherited process environment rather
than Pi filesystem state.

The separate defaults problem remains: an operator needs one fleet model and
thinking level while a declaration can still override either.

## Decision

The Runner invokes Pi from the Run worktree with non-context discovery disabled:

```text
pi -p --mode json --no-session --session-id <run-id> --approve \
  --no-extensions --no-skills --no-prompt-templates --no-themes \
  --model <model> --system-prompt <instructions> \
  [--tools <comma-separated-tools>] [--thinking <level>] \
  [--skill <repository-relative-path>]... "<task>"
```

`--no-skills` disables skill discovery; it does not replace the repeated,
explicit `--skill` arguments. The only skill sources are:

1. `agents/_shared/skills`, for every declaration;
2. `agents/<name>/skills`, when that role-specific directory exists.

Skill source directory paths are repository-relative and Pi resolves them from
the Run worktree. The declaration read surface and the Run's `forest.run`
evidence publish the selected directories as `skills`. The obsolete
`profile_files` and declaration `env` fields are removed rather than aliased;
these breaking read-surface changes advance the envelope to `forest.cli.v2`.

No extension, ambient skill, prompt template, or theme is available to the
child, including one installed in the host's Pi directory. Project context-file
discovery remains enabled for the Run worktree.

The ephemeral Pi session ID is the exact Run ID. For an OpenRouter model, the
Runner writes a minimal `models.json` override that enables
`sendSessionAffinityHeaders` with the `openrouter` format. Pi therefore sends
the Run ID as `x-session-id`, giving Broadcast destinations a stable join key
for provider traces, Ledger records, and retained Run logs without adding
credentials or prompt content to Kernel evidence.

For every Run, the Runner creates a new writable scratch directory and sets
the child's `PI_CODING_AGENT_DIR` to it. It starts without operator Pi
filesystem state; only the credential-free OpenRouter model override may be
materialized there. Declarations cannot supply environment entries.
Provider and forge credentials reach Pi only through the inherited service
environment. The installed service reads that environment from
`%h/.config/iron-forest/%i.env`; the Runner does not write credential values to
the Pi scratch directory or Run evidence.

The model resolves through the declaration, then instance defaults, then the
built-in `openrouter/deepseek/deepseek-v4-flash-0731`. `thinking` resolves
through the declaration and then instance defaults; there is no built-in
thinking level. Defaults contain only `model` and `thinking`. An empty or
comment-only defaults file is the zero Defaults, not an error. `forest
declaration show` and the Run's `forest.run` line publish the resolved model and
its source.

The explicit inputs do not create a sandbox. A trusted declaration still runs
with the service user's inherited credentials, filesystem access, and network
access. Worktree separation and time bounds remain operational boundaries;
stronger credential and process containment belongs to the deployment
substrate.

## Consequences

- Builder and Fixer receive the shared skills. Verifier receives those plus the
  skills under `agents/verifier/skills`.
- Installing an MCP extension or other Pi resource on the host cannot alter a
  Run unless the explicit process contract changes.
- An operator sets one fleet model or thinking level in
  `forest.defaults.yaml` or `$FOREST_DEFAULTS`; declaration frontmatter still
  wins.
- `forest selfcheck` publishes the defaults file it loaded.
- Replacing the harness still means changing one explicit command shape.
