# Iron Forest

Iron Forest is a self-hosted system that turns Tracker items into reviewed code changes.
Four independent Flows run in one process and coordinate through git facts and Tracker labels.

## Flows

Each Flow selects a Subject, performs its Effect, and records a Run.
Each lane uses its own interval. The process excludes duplicate work on one Subject.

| Flow | Selector | Effects |
| --- | --- | --- |
| Builder | Tracker items with every configured required label, without a forest branch, and without configured exclude labels. | Creates an isolated worktree, runs the Builder, checks the Gate, and pushes a branch. It may create a Projection. |
| Verifier | Pending retirements; forest branches without a Verdict and without a failing Checks note; approved branches with passing Checks when `auto_merge` is enabled and attempts remain, or one Host-preparation pass when Host merge is enabled and `auto_merge` is disabled. | Runs the configured Checks, writes the Checks note, obtains an independent Verdict, records pending Host retirement intent, recovers exact operator Host merges, and merges approved branches only when `auto_merge` is enabled. |
| Fixer | Branches with a rejected Verdict or failed Checks below the attempt limit. | Runs the Builder on the branch, passes the Gate, pushes the repair, and records the attempt. An exhausted branch gets `forest:failed` for a human. |
| Manager | Open Tracker items without a forest branch, pending retirement, ready label, configured exclude label, open blocker, or Builder stall. | Fills the configured ready depth one candidate per pass. It withdraws branchless ready items that become excluded, blocked, failed, or stalled. |

## State

Git stores durable decisions. One installation can run any Flow, and each
checkout has one daemon process.
- **Verdict:** `refs/notes/forest/verdict` stores a Verdict on the exact Revision reviewed.
- **Checks:** `refs/notes/forest/checks` stores the result of Iron Forest's own `checks:` commands on that exact Revision.
- **Retirement:** `refs/forest/retirement/` stores resumable merge effects until the Tracker item and source branch are retired.
- **Ledger:** `.forest/runs.jsonl` is host telemetry outside git. It records each Run's Flow, Subject, Revision, Status, Verdict, and measured `tokens_in`, `tokens_out`, `cache_read`, `cache_write`, and `reasoning` classes. It never records or computes money.

A new commit has no Verdict or Checks note, so Iron Forest needs no staleness comparison. Iron Forest never reads a Host's review or check state.

A pull request is an optional Projection for people. `projection.enabled` controls it. Set `projection.merge_via_host` for a protected target branch; this Host path supports only squash merge. With Host mode and `auto_merge: false`, the Verifier makes one preparation pass, records pending retirement, and never requests a merge. After an operator merges the exact reviewed revision, the next pass observes that merge for recovery. Iron Forest reads pull request identity only for idempotent publication and Host retirement recovery. It never treats Host review or check state as a Verdict or Gate.

## Commands

Run the binary from the repository root. The command surface is:

| Command | Purpose |
| --- | --- |
| `forest list` | Print eligible Tracker items. |
| `forest agents` | List declarations under `agents/` and their digests. |
| `forest stats [--json]` | Aggregate `.forest/runs.jsonl`; use `--json` for machine output. |
| `forest serve [--factory-dir <path>] [--flow <name>]...` | Run all enabled Flows, or only the named Flows. |
| `forest run <flow> <subject>` | Run one Subject by exact key, branch, or item ID. Ambiguous exact matches are refused. |
| `forest show <sha>` | Print the Verdict and Checks notes for a commit. |
| `forest version` | Print the binary Revision. |
| `forest selfcheck` | Verify configuration and agent declarations offline. |
| `forest watch [--interval 2s]` | Show the operator board. |

## Configuration

`forest.yaml` uses these keys. This is an example composition.

```yaml
repo: owner/name                 # required; no default
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
    require_labels: [forest:ready]
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
  manager:
    enabled: true
    agent: manager
    interval_seconds: 60
    ready_depth: 1
    exclude_labels: [parked, forest:failed]
projection:
  enabled: true
  merge_via_host: false
```

`repo` names the Tracker repository. There is no `protected` key: `docs/adr/0003`
removed it, so the Gate rejects nothing by path and independent review on the
exact commit is what decides whether a change lands.

Each `agents/<name>/agent.yaml` declares `commit.name` and `commit.email`.
Builder and Fixer commits use their acting agent's identity. Verifier rebases
preserve each original author and use the Verifier as committer. A native Git
squash commit uses the Verifier identity. A Host-projected merge retains the
Host platform's attribution. The authenticated Host account still pushes
branches and authors pull requests. A distinct Host actor needs its own
account or application credential.

Attaching a second repository to a running installation is
`docs/onboarding-managed-repo.md`.

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

`flows.builder` selects items. Declaring `require_labels` turns selection from opt-out into opt-in, so an open item needs every declared label. An enabled Manager requires exactly `require_labels: [forest:ready]`; that label is its assignment signal. `flows.verifier.merge` is `squash` or `ff`. `flows.verifier.auto_merge` makes an approved, passing branch eligible for Verifier merge when attempts remain; when false, a native merge remains disabled, while Host mode gets one preparation pass that records pending retirement and never requests a merge. The next pass observes the exact operator merge and completes retirement. `flows.fixer.attempts` bounds repairs. Projection keys control the optional human surface.

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

`forest serve` reads `forest.yaml` before each Flow pass. A committed configuration change takes effect without a process restart.

The first termination signal stops new actions and lets current actions finish. A second signal kills managed process groups and exits without waiting for repository I/O. The next startup reaps linked worktrees before any Flow starts.

Self-update waits until every Flow action is idle. It installs the tested binary and exits so the service supervisor can restart it. Deployed instances also serialize access to their shared factory source checkout.

## Ledger and board

`forest stats` reads `.forest/runs.jsonl` and prints totals and breakdowns. `forest stats --json` emits machine-readable ledger data.
`Status` is a Flow routing result. Summaries classify `built`, `reviewed`, `merged`, `fixed`, `done`, and `reaped` as progress. They classify any `*_failed` value as failed. Every other value is other.

`forest watch` reads Runs, tracked worktrees, git HEAD, and daemon state.
It shows each Flow and recent Effects.

## License

MIT. See [LICENSE](LICENSE).
