# Iron Forest

Iron Forest is a self-hosted system that turns Tracker items into reviewed code changes.
Three independent Flows run in one process and coordinate through git state.

## Flows

Each Flow selects a Subject, performs its Effect, and records a Run.
Each lane uses its own interval. The process excludes duplicate work on one Subject.

| Flow | Selector | Effects |
| --- | --- | --- |
| Builder | Eligible Tracker items without a forest branch and without configured exclude labels. | Creates an isolated worktree, runs the Builder, checks the Gate, and pushes a branch. It may create a Projection. |
| Verifier | Forest branches with no Verdict, or approved branches with passing Checks. | Runs the configured Checks, writes the Checks note, obtains an independent Verdict, and can merge an approved branch. |
| Fixer | Branches with a rejected Verdict or failed Checks below the attempt limit. | Runs the Builder on the branch, passes the Gate, pushes the repair, and records the attempt. An exhausted branch gets `forest:failed` for a human. |

## State in git

Git stores durable decisions. One installation can run any Flow, and each
checkout has one daemon process.
- **Verdict:** `refs/notes/forest/verdict` stores a Verdict on the exact Revision reviewed.
- **Checks:** `refs/notes/forest/checks` stores the result of Iron Forest's own `checks:` commands on that exact Revision.
- **Ledger:** `.forest/runs.jsonl` is an append-only record of each Run, Subject, Revision, Status, Verdict, and token count.

A new commit has no Verdict or Checks note, so Iron Forest needs no staleness comparison. Iron Forest never reads a Host's review or check state.

A pull request is an optional one-way Projection for people. `projection.enabled` controls it. Set `projection.merge_via_host` for a protected target branch. Iron Forest never reads the Projection back.

## Commands

Run the binary from the repository root. The command surface is:

| Command | Purpose |
| --- | --- |
| `forest list` | Print eligible Tracker items. |
| `forest agents` | List declarations under `agents/` and their digests. |
| `forest stats [--json]` | Aggregate `.forest/runs.jsonl`; use `--json` for machine output. |
| `forest serve [--factory-dir <path>] [--flow <name>]...` | Run all enabled Flows, or only the named Flows. |
| `forest run <flow> <subject>` | Run one selected Subject by key, branch, or issue number in one Flow. |
| `forest show <sha>` | Print the Verdict and Checks notes for a commit. |
| `forest version` | Print the binary Revision. |
| `forest selfcheck` | Verify configuration and agent declarations offline. |
| `forest watch [--interval 2s]` | Show the operator board. |

## Configuration

`forest.yaml` uses these keys. This is an example composition.

```yaml
repo: owner/name                 # required; no default
protected: [.forest/, forest.yaml, agents/, .opencode/opencode.json]
checks:
  - name: build
    run: mise exec -- go build ./...
  - name: vet
    run: mise exec -- go vet ./...
  - name: test
    run: mise exec -- go test ./...
flows:
  builder:
    enabled: true
    agent: builder
    interval_seconds: 30
    exclude_labels: [parked, forest:failed]
  verifier:
    enabled: true
    agent: verifier
    interval_seconds: 20
    merge: squash
    auto_merge: false
  fixer:
    enabled: true
    agent: builder
    interval_seconds: 40
    attempts: 2
projection:
  enabled: true
  merge_via_host: false
```

`repo` names the Tracker repository. `protected` lists paths the Gate rejects.

Iron Forest runs each command in `checks:` and writes one Checks note.
It never reads a Host check or review.
Labels such as `exclude_labels` are Tracker inputs, not factory state.

Each check runs in a child environment with a private `HOME` and a scrubbed
`PATH`. Its tools resolve one of two ways, both stack-agnostic:

- **mise-managed tools** with working shims are already reachable on the child
  `PATH`.
- **host toolchain directories** outside that `PATH` (for example rustup's
  `~/.cargo/bin`) are reachable when the operator names them with the
  `FOREST_CHECK_PATH` environment variable on the host running the Factory, a
  platform path-list of directories to prepend to the child `PATH`. They sit
  before the mise shims, so a working host binary wins over a dead shim.

A host binary is often only a proxy that must read toolchain metadata to find
its real driver (rustup's `cargo` needs `RUSTUP_HOME` to locate the default
toolchain; with a private empty `HOME` and only the proxy on `PATH` it reports
"no default is configured"). Name that metadata with `FOREST_CHECK_ENV` on the
host running the Factory, a newline-separated list of `KEY=VALUE` pairs, one per
line, added to the check child environment.

Both variables reach only the **check** child, never an agent run: the host
toolchain mechanism is applied where a managed repo's `checks:` needs to find
its declared tools, and is withheld from the opencode agent, so neither host
binaries on `PATH` nor toolchain metadata can bleed into agent reach.

`FOREST_CHECK_ENV` carries only a curated allowlist of metadata variables, and
drops everything else. Today the single allowlisted variable is `RUSTUP_HOME`,
which points at the rustup install root (settings and toolchains) and holds no
credentials. A substring denylist would be unsound — `CI_JOB_JWT`,
`AWS_ACCESS_KEY_ID`, `KUBECONFIG`, or `GIT_CONFIG_GLOBAL` could slip through,
and `CARGO_HOME` deliberately is *not* allowlisted because `~/.cargo` holds
`credentials.toml`, so pointing a check at it would expose the operator's
registry token. An explicit allowlist is the only defensible boundary: it can
never be fooled by an unlisted credential name or path, and it keeps the private
`HOME`, the scrubbed `PATH`, and the managed caches authoritative.

Building the wrong thing is worse than not building: Iron Forest does not guess a
stack. If a `checks:` command's tool is missing, the check fails and the note
names the command that could not start.

`flows.builder` selects items; declaring `require_labels` (for example
`require_labels: [forest:ready]`) turns selection from opt-out into opt-in, so
an open item is eligible only when it carries every declared label. `flows.verifier.merge` is `squash` or `ff`.
`flows.verifier.auto_merge` controls the merge Effect. `flows.fixer.attempts` bounds repairs. Projection keys control the optional human surface.

## Requirements

- Go 1.26.5 through mise.
- Git with access to the repository.
- `gh`, the Host CLI used for Tracker items and Projections.
- A provider key for the configured Runner.
Provider usage is bounded by the provider key outside Iron Forest.

Build and run with the pinned toolchain:

```sh
mise exec -- go build -o forest .
./forest serve --factory-dir /path/to/iron-forest
```

Omit `--factory-dir` to disable self-update.

## Ledger and board

`forest stats` reads `.forest/runs.jsonl` and prints totals and breakdowns. `forest stats --json` emits machine-readable ledger data.

`forest watch` reads Runs, tracked worktrees, git HEAD, and daemon state.
It shows each Flow and recent Effects.

## License

MIT. See [LICENSE](LICENSE).
