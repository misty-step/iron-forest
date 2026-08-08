# Onboarding a managed repository

One organization runs one installation, and one process per checkout
(`docs/adr/0001`). Instance names are sibling directories of the factory source,
so attaching a repository means placing its checkout beside this one and enabling
one more unit.

This checklist was written while attaching `misty-step/cantrip`, a Rust
repository, to a machine already running `forest@iron-forest`. Every step below
was needed; none is decoration.

## Prerequisites

- `gh` authenticated for the target repository, with `repo` scope. The controller
  is the only caller of the Tracker; an agent run never receives this credential.
- An opencode provider route the agents can reach. On this machine that is Mint
  markers in the opencode configuration.
- The repository's own tools installed on the **host**. See step 5: a check child
  has a scrubbed `PATH`, so "the host has cargo" is not sufficient by itself.
- The checkout is a sibling of the factory source:
  `<org-dir>/iron-forest` and `<org-dir>/<name>`.

## 1. Create the Tracker labels

Selection is opt-in through labels, and they do not exist in a new repository.

```sh
gh label create forest:ready  -R owner/name --color 0e8a16 --description "promoted for the factory"
gh label create forest:failed -R owner/name --color b60205 --description "factory needs a human"
gh label create parked        -R owner/name --color cfd3d7 --description "not scheduled"
```

## 2. Declare the repository's factory

Write `forest.yaml` at the root of the managed checkout. It declares that
repository's own checks and lanes; there is no central policy.

```yaml
repo: owner/name
commit:
  name: forest
  email: forest@example.invalid

checks:                      # the repository's own commands, in its own language
  - name: fmt
    run: cargo fmt --check
  - name: clippy
    run: cargo clippy --all-targets -- -D warnings
  - name: test
    run: cargo test

flows:
  builder:
    enabled: true
    agent: builder
    interval_seconds: 45
    require_labels: ["forest:ready"]
    exclude_labels: ["forest:failed", "parked"]
  verifier:
    enabled: true
    agent: verifier
    interval_seconds: 30
    merge: squash
    auto_merge: false        # start here; see step 8
  fixer:
    enabled: true
    agent: builder
    interval_seconds: 45
    attempts: 3
  manager:
    enabled: true
    agent: manager
    interval_seconds: 60
    ready_depth: 1
    exclude_labels: ["forest:failed", "parked", "epic"]

projection:
  enabled: true
  merge_via_host: false
```

`checks:` is the whole stack declaration. Iron Forest never guesses a language.
If a command's tool cannot start, the check fails and the note names the command.

`exclude_labels` is repository policy. Add every label that marks a
non-dispatchable item. For example, exclude an `epic` that groups leaf items.

## 3. Declare the agents

The managed repository carries its own `agents/` tree, because an agent
declaration is data that belongs to the repository it works on.

```
agents/
  builder/
    agent.yaml            # harness, model, permissions, mcp
    instructions.md       # system prompt
    prompt.md             # user-prompt template
    report.schema.json    # the output contract the Gate enforces
    skills/               # optional, appended to the system prompt
  verifier/
    agent.yaml
    instructions.md
    prompt.md
    report.schema.json
  manager/
    agent.yaml
    instructions.md
    prompt.md
    report.schema.json
```

Copy this repository's `agents/` as a starting point and change the model,
permissions, and language-specific skill. Declare a positive
`deadline_seconds`. Do not declare `steps` or `budget_seconds`; both fixed
ceilings were deleted because they stop real work partway (`99b3b74`).

## 4. Keep the factory out of the repository's gates

**No ignore or exclude entries are needed.** Per-run factory artifacts are kept
out of the managed repository's working tree, so a managed repository needs no
`.gitignore`, no `.trufflehog-exclude`, and no hook change to be worked by the
factory.

The factory still uses opencode to run an agent, and opencode still wants a
config root and per-provider packages. Where it gets them is what changed
(#174). The factory now points opencode at a per-run config root **outside** the
managed worktree through opencode's supported external-config mechanism
(`XDG_CONFIG_HOME` in the run's child environment). Under that root:

- the rendered agent declaration (`opencode/agents/<name>.md`) is written there,
  not under the worktree's `.opencode/`,
- the provider configuration a real run actually uses — the factory's own
  `.opencode/opencode.json`, falling back to the operator's global opencode
  config — is preserved there as `opencode/opencode.json`, so the run still
  reaches a provider route, and
- the `node_modules` opencode installs for its provider packages land under that
  root as well.

Nothing is placed in a working tree a hook or a filesystem scanner reads. The
per-run root is removed when the run completes.

A side effect of this placement: the rendered declaration cannot be staged by
`git add -A` no matter what the managed repository's ignore rules are, because it
does not live in the repository at all.

If a repository already carries its own `.opencode/` — its own provider
configuration, say — it is left untouched. The factory no longer writes there.
To keep it that way, the run disables opencode's local project-config discovery
for the managed worktree (`OPENCODE_DISABLE_PROJECT_CONFIG` in the child
environment), so a `.opencode/opencode.json` the repository ships is not read
and does not trigger an install into the managed tree.

This was not always true. Before #174, the factory rendered `.opencode/agents/`
and node_modules into the worktree. On Cantrip the `pre-push` hook ran
`trufflehog filesystem .`, read 63 MB of third-party `effect` and `zod` test
fixtures under `.opencode/node_modules`, found 11 findings, and refused every
factory push. Needing a `.gitignore` workaround for that was the defect #174
removed.

## 5. Give the check child its toolchain

Each check runs with a private `HOME` and a scrubbed `PATH`. Tools resolve two
ways:

- **mise-managed tools** with working shims are already reachable.
- **host toolchain directories** must be named by the operator.

For a rustup-based repository, add a per-instance systemd drop-in at
`~/.config/systemd/user/forest@<name>.service.d/toolchain.conf`:

```ini
[Service]
Environment=FOREST_CHECK_PATH=/home/you/.cargo/bin
Environment=FOREST_CHECK_ENV=RUSTUP_HOME=/home/you/.rustup
```

`FOREST_CHECK_PATH` prepends directories to the check child's `PATH`.
`FOREST_CHECK_ENV` adds allowlisted metadata; rustup's `cargo` proxy needs
`RUSTUP_HOME` or it reports "no default toolchain is configured" under an empty
`HOME`.

`CARGO_HOME` is deliberately **not** allowlisted: `~/.cargo` holds
`credentials.toml`. Both variables reach the check child only, never an agent
run.

Then `systemctl --user daemon-reload`.

## 6. Install the unit

```sh
cd <org-dir>/iron-forest
./deploy/install-service.sh <name>
```

This seeds the binary into the managed checkout from the factory source and
enables `forest@<name>`. The instance name is the checkout's directory name.

```sh
systemctl --user start forest@<name>
systemctl --user status forest@<name>
journalctl --user -u forest@<name> -f
```

## 7. Prove selection before promoting work

```sh
cd <org-dir>/<name>
./forest selfcheck      # config and agents, offline
./forest agents         # models and declaration digests
./forest list           # eligible items; empty until something is promoted
```

Open one small, well-shaped item, label it `forest:ready`, and confirm
`forest list` shows it.

## 8. Watch the first item end to end

With `auto_merge: false` the Verifier reviews and stops, and you merge. Read that
first diff yourself. When a full pass has landed and you trust the checks, set
`auto_merge: true`.

Expect these on a first run:

- **A cold build is slow.** Cantrip's first pass peaked at 8.2 GB and 18 minutes
  of CPU pulling its dependency tree. There is no step ceiling by design, so let
  it finish. Wall time is bounded by each agent's `deadline_seconds`; a run that
  outlives it is cancelled and recorded as `timeout_failed` (mechanical), and
  its lane reopens on the next pass.
- **Three failures on one revision park the item.** The repeat-failure brake is
  a ref under `refs/forest/stalled/`. Fix the cause, then move the item's
  revision — a comment is enough — and it becomes selectable again.
- **A daemon restart kills an in-flight agent** and records `agent_failed`
  against it. That counts toward the brake.

## 9. Fleet commands

```sh
systemctl --user list-units 'forest@*'          # every instance
journalctl --user -u 'forest@*' -f              # one log stream
systemctl --user disable --now forest@<name>    # detach one repository
```

Each instance has its own checkout, its own `forest.yaml`, its own lanes, and its
own lock. One repository's stuck agent cannot stall another.
