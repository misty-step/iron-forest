# 0013 — OMP harness and declaration format

Status: accepted, 2026-08-10

## Context

Agent behavior must remain inspectable and portable across profiles. The Kernel
must invoke the OMP harness directly without depending on OMP's agent discovery
or adding a custom prompt format.

## Decision

The Runner invokes OMP with this command shape:

```text
omp -p --mode json --no-session --auto-approve --cwd <worktree> \
  --max-time <timeout> --model <model> --system-prompt <instructions> \
  [--tools <comma-separated-tools>] [--thinking <level>] "<task>"
```

A declaration uses `agents/<name>/agent.md`. Its YAML frontmatter requires
`model` and may contain `tools` and `thinking`. The body is the system prompt.
The standing user prompt is `agents/<name>/task.md`. The Kernel parses the
frontmatter itself and passes the declaration data to OMP.

The Runner adds `--tools` only when `tools` is present. It adds `--thinking`
only when `thinking` is present.

## Consequences

Agent policy is data in the repository and can be reviewed with the profile.
The Kernel has one stable harness seam. OMP upgrades must preserve the command
contract or update this ADR and the Kernel together.

A declaration does not gain hidden behavior from OMP discovery. Missing or
invalid frontmatter is a configuration error reported by `forest selfcheck`.
