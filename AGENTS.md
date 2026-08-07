# AGENTS

This file is the contributor contract for Iron Forest.

## Vocabulary

Use these names for system concepts: Flow, Run, Phase, Subject, Revision,
Selector, Effect, Verdict, Gate, Ledger, Tracker, Host, Runner, Projection,
Builder, Verifier, and Fixer.

Forbidden vocabulary includes the retired animal labels, the former action and state words, and the obsolete configuration noun.
Do not add accounting for monetary amounts.
Use the system names above in source, tests, issues, commits, and documentation.

## Agent declarations

Iron Forest has two declarations:

- `agents/builder/` contains the Builder declaration.
- `agents/verifier/` contains the Verifier declaration.

Each declaration uses these files:

- `agent.yaml` declares the harness, model, permissions, MCP wiring, and run limits.
- `instructions.md` contains the system prompt and standing rules.
- `prompt.md` contains the user-prompt template for one Subject.
- `report.schema.json` defines the output contract enforced by the Gate.
- `skills/*.md` contains optional context added to the system prompt.

The Builder writes `report.json` after it implements a Subject.
The Verifier writes `review.json` after it produces a Verdict.
The Builder declaration currently includes `skills/go-style.md`.
The Verifier declaration has no skills directory.

## Gate

The Gate validates the agent output against its `report.schema.json`.

There is a protected-path check. The Gate refuses any Build run whose change
touches the factory's control plane — `.forest/`, `forest.yaml`, `agents/`, and
`.opencode/opencode.json` — before trusting anything the report claims. `docs/adr/
0003` once removed that check; `docs/adr/0004` superseded it and restored it as
an FSM invariant of the delivery machine. Independent review on the exact commit
remains the audit surface. An agent may not change a protected path; a change to
`forest.yaml` or `agents/` is deliberate, reviewed work, done through a card
whose run the Gate accepts. See `docs/fsm.md` (invariant 4), `docs/adr/0004`,
and `README.md`.

Iron Forest runs its own commands from `checks:` and writes their results as git
notes. It never reads a Host's review or check state.

The Ledger records tokens only.

## Toolchain and branch

Use the pinned Go toolchain through mise:

```sh
mise exec -- go build ./...
mise exec -- go vet ./...
mise exec -- go test ./...
```

Use `master` as the target branch. Keep agent changes small, update the relevant
tests and docs, and run `./forest selfcheck` after configuration changes.
