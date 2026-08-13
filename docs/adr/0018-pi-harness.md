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
translation.

## Decision

The Runner invokes `pi` with this command shape, further constrained by
[0019](0019-explicit-pi-runtime-inputs.md):

```text
pi -p --mode json --no-session --approve \
  --no-extensions --no-skills --no-prompt-templates --no-themes \
  --model <model> --system-prompt <instructions> \
  [--tools <comma-separated-tools>] [--thinking <level>] \
  [--skill <repository-relative-path>]... "<task>"
```

Two omissions from the OMP shape, each deliberate:

- No `--cwd`. The Runner already sets the child's working directory to the Run's
  private worktree, so a second statement of the same fact could disagree with
  the first.
- No `--max-time`. The Runner owns the timeout: it cancels the context and stops
  the process group with TERM, a grace period, KILL, and a quiescence probe.
  Delegating the deadline would leave the Runner unable to prove the child died.

The declaration system prompt and project context files discovered from the Run
worktree are trusted repository instructions. Selected repository skills reach
Pi through explicit arguments. Extension, skill, prompt-template, and theme
discovery are disabled, while normal project context-file discovery remains
enabled. The Runner passes only the shared and role-specific repository skill
directories with repeated `--skill` arguments and gives Pi an empty per-Run
agent directory, so host Pi resources do not become Run inputs.

The first draft of this ADR chose `--no-approve` on the reasoning that a Run
executes against content the factory did not author. That was incoherent:
repository-authored project context, the declaration prompt, and selected
skills cross the same trusted boundary, while refusing their tools did not
close a security boundary. [0016](0016-isolation-posture.md) states that
worktree separation and the timeout are operational boundaries, not a sandbox.

The factory runs agents on a repository at its operator's instruction. That
repository is the workspace, not hostile input. A declaration states which
tools an agent gets, and ADR 0019 excludes undeclared Pi resources without
excluding trusted project context.

The trusted tool set becomes `git`, `gh`, and `pi`. `forest selfcheck` resolves
and publishes all three, so a host that cannot dispatch says so before a Run.

## Consequences

- The service unit's `PATH` must reach `pi`. The installer resolves it through
  the operator's mise shim directory, and `selfcheck` fails when it cannot.
- A declaration's `model` is a Pi model pattern. The shipped contract supports
  providers Pi can resolve without operator agent-directory state.
- Token classes land in the Ledger unchanged, because pi's usage keys are the
  aliases the Ledger reader already accepts.
- Replacing the harness again means changing one command shape and one executable
  resolver. The declaration format does not move.
- Pi's non-context configuration inputs are explicit per Run; see
  [0019](0019-explicit-pi-runtime-inputs.md).
