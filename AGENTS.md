# AGENTS

Iron Forest defines its agents as data so they are diffable, inspectable, and
reproducible. Each agent is a directory under `agents/<name>/`. The factory
never edits these directories: they are protected paths, reviewed like source.

## Agent anatomy

An agent directory contains:

- `agent.yaml` — the declaration: harness, model, permissions, MCP wiring,
  budget, and provider price table.
- `instructions.md` — the system prompt: identity and standing rules.
- `prompt.md` — the user-prompt template; the work item is injected per run.
- `report.schema.json` — the output contract the gate enforces.
- `skills/*.md` — optional extra context appended to the system prompt.

The gate requires that a run's report validate against the agent's schema
before the change can move forward.

## The beaver (build)

`agents/beaver/` implements one issue in the worktree and writes `report.json`.

- Model: `openrouter-mint/deepseek-v4-flash-0731` (DeepSeek through OpenRouter
  via the Mint proxy), temperature 0.2.
- Mode: `primary`, up to 50 steps, no deadline (`budget_seconds: 0`).
- Powers: read/edit/glob/grep/list/bash/lsp/todowrite allowed; the web, `task`,
  `skill`, and external-directory surfaces are denied — the builder runs
  offline against its own snapshot.
- The Exa MCP server is declared but disabled.

## The owl (review)

`agents/owl/` checks a worktree change against its issue and writes
`review.json`. Its approval is the only thing that can clear a change for
merge, so it must not share the builder's bias.

- Model: `openrouter/openai/gpt-5.6-luna` at `variant: max` — OpenAI, a
  different family than the builder, at maximum reasoning effort.
- Mode: `primary`, up to 30 steps, no deadline.
- Read-mostly: it inspects the diff and may write only `review.json`. Every
  network surface and every tool except read/edit/glob/grep/list/bash/lsp is
  denied.

## Adding an agent

1. Create `agents/<name>/` with the five files above, copying the shape from an
   existing agent.
2. Declare `workflow.build` or `workflow.review: <name>` in `forest.yaml`.
3. Wire the model and price table in `agent.yaml`.
4. Run `forest selfcheck` from the repository root to confirm the config loads
   and the agent resolves.
