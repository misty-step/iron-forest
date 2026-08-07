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

projection:
  enabled: true
  merge_via_host: false
```

`checks:` is the whole stack declaration. Iron Forest never guesses a language.
If a command's tool cannot start, the check fails and the note names the command.

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
```

Copy this repository's `agents/` as a starting point and change the model,
the permissions, and any language-specific skill. Do not declare a `steps` or
`budget_seconds` key: both were deleted, and a fixed ceiling stops real work
partway (`99b3b74`).

## 4. Keep the factory out of the repository's gates

**Do this before the first run.** A worktree the factory works in carries two
things the repository never asked for:

- `.opencode/agents/<name>.md`, the rendered agent declaration, which carries
  provider configuration.
- `.opencode/node_modules/`, which opencode installs for its provider packages.
  On Cantrip this was 63 MB across 3,653 files.

Add to the managed repository's `.gitignore`:

```
.opencode/agents/
.opencode/node_modules/
```

If the repository runs a **working-tree** secret scanner or any other
filesystem-wide hook, exclude `.opencode/` from it as well. `.gitignore` does not
help there: a filesystem scanner reads the tree, not the git index.

Observed on Cantrip: its `pre-push` hook ran `trufflehog filesystem .`, found 11
findings in third-party `effect` and `zod` test fixtures under
`.opencode/node_modules`, and correctly refused every factory push. All agent
work on the repository was blocked until this step was done.

Needing this step at all is a factory defect, tracked as
[#174](https://github.com/misty-step/iron-forest/issues/174) and
[#146](https://github.com/misty-step/iron-forest/issues/146). Until those land,
a managed repository has to know about the factory.

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
  of CPU pulling its dependency tree. There is no step ceiling or deadline by
  design, so let it finish.
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
