# 0018 — pi is the agent harness

Status: accepted, 2026-08-12

Supersedes the command shape in [0013](0013-omp-harness-and-declarations.md). The
declaration format that ADR defines is unchanged.

## Context

ADR 0013 chose OMP as the harness the Runner invokes. On the host that runs this
factory, no Run completes: the Runner probes the installed OMP for usage and
rejects what it receives.

```text
$ forest once builder
parse OMP usage: OMP output has no usage
exit 2
```

Every behavior behind a dispatch was therefore unreachable: the post-dispatch
Audit, log retention, the truncation marker, worktree collection, and following a
live Run. A factory that cannot complete a Run is not a factory.

`pi` is present on the host, is the engine the review lanes already run, and its
`--mode json` output is JSON Lines whose `turn_end` messages carry a `usage`
object with `input`, `output`, `cacheRead`, `cacheWrite`, and `reasoning`. That
is the shape the Ledger already records, so the token classes need no
translation. `pi` also routes to local `ollama` models, which lets the factory
run with no money at all.

## Decision

The Runner invokes `pi` with this command shape:

```text
pi -p --mode json --no-session --no-approve --model <model> \
  --system-prompt <instructions> [--tools <comma-separated-tools>] \
  [--thinking <level>] "<task>"
```

Three differences from the OMP shape, each deliberate:

- No `--cwd`. The Runner already sets the child's working directory to the Run's
  private worktree, so a second statement of the same fact could disagree with
  the first.
- No `--max-time`. The Runner owns the timeout: it cancels the context and stops
  the process group with TERM, a grace period, KILL, and a quiescence probe.
  Delegating the deadline would leave the Runner unable to prove the child died.
- `--no-approve` instead of `--auto-approve`. A Run executes against repository
  content the factory did not author, so project-local harness configuration is
  ignored rather than trusted. This is a deliberate reduction in what the audited
  repository can influence, and it is not a sandbox: worktree separation and the
  timeout remain the only isolation, as [0016](0016-isolation-posture.md) states.

`AGENTS.md` discovery stays enabled. Repository instructions are the task
context an agent is supposed to read, and they are visible in the Run log.

The trusted tool set becomes `git`, `gh`, and `pi`. `forest selfcheck` resolves
and publishes all three, so a host that cannot dispatch says so before a Run.

## Consequences

- The service unit's `PATH` must reach `pi`. The installer resolves it through
  the operator's mise shim directory, and `selfcheck` fails when it cannot.
- A declaration's `model` is a pi model pattern, so `provider/id` selects a
  provider. `ollama/<model>` runs locally and records zero cost.
- Token classes land in the Ledger unchanged, because pi's usage keys are the
  aliases the Ledger reader already accepts.
- Replacing the harness again means changing one command shape and one executable
  resolver. The declaration format does not move.
