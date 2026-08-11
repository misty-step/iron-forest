# Iron Forest

Iron Forest is a self-hosted system that turns Tracker items into reviewed code changes.
Four independent Flows run in one process and coordinate through git facts and Tracker labels.

## Flows

Each Flow selects a Subject, performs its Effect, and records a Run.
Each lane uses its own interval. Admission excludes duplicate work on one Subject across processes and checkouts.
After each pass, a lane resumes selection after the prior Subject. A retrying Subject cannot starve later work.

| Flow | Selector | Effects |
| --- | --- | --- |
| Builder | Open Tracker items with every required label, without a forest branch, retirement fact, exclude label, or Builder stall at the current Revision. | Creates an isolated worktree, runs the Builder, checks the Gate, and pushes a branch. It publishes the Revision-marked Tracker comment before Host mode records `preparing` recovery state. |
| Verifier | Recovery retirements; forest branches without a Verdict and without a failing Checks note; approved branches with passing Checks when `auto_merge` is enabled and Fixer attempts remain, or one Host-preparation pass when Host merge is enabled and `auto_merge` is disabled. | Runs the configured Checks, refreshes durable notes, obtains an independent Verdict, upgrades Host retirement intent, recovers exact operator Host merges, and merges approved branches only when `auto_merge` is enabled. |
| Fixer | Branches with a rejected Verdict or failed Checks below the attempt limit, plus exhausted branches whose human handoff is incomplete. | Runs the Builder on the branch and passes the Gate. One atomic push publishes both the repair and attempt count. An exhausted branch gets `forest:failed` for a human. |
| Manager | Open Tracker items without a forest branch, retirement fact, ready label, configured exclude label, open blocker, or Builder stall. | Fills the configured ready depth one candidate per pass. It withdraws branchless ready items that become excluded, blocked, failed, or stalled. |

## State

Git stores durable decisions. One installation can run any Flow, and each
checkout has one daemon process.
- **Admission:** `refs/forest/claim/` and a per-owner file lock serialize one canonical Subject across processes and checkouts.
- **Verdict:** `refs/notes/forest/verdict` stores a Verdict on the exact Revision reviewed.
- **Checks:** `refs/notes/forest/checks` stores the result of Iron Forest's own `checks:` commands on that exact Revision.
- **Retirement:** `refs/forest/retirement/` stores `preparing`, `pending`, `observed`, or `landed` merge recovery until the Tracker Item and branch retire.
- **Effect:** `refs/forest/attempt/effect-*` stores Revision-scoped write claims. Accepted Host merges and Tracker closes get separate acceptance claims.
Manager and Fixer tag updates use Subject admission as intent. Repeating their exact add/remove operation is safe.
- **Ledger:** `.forest/runs.jsonl` is host telemetry outside git. It records each Run's Flow, Subject, Revision, Status, and review verdict under `review`. It also records measured `tokens_in`, `tokens_out`, `cache_read`, `cache_write`, and `reasoning` classes. It never records or computes money.

A new commit has no Verdict or Checks note, so Iron Forest needs no staleness comparison. Iron Forest never reads a Host's review or check state.

A pull request is an optional Projection for people. `projection.enabled` controls it. Set `projection.merge_via_host` for a protected target branch; this Host path supports only squash merge. Host mode publishes the Builder comment before `preparing` can suppress branch selection. The retirement fact records that completed Effect and upgrades to `pending` after the durable winning Verdict.
With `auto_merge: false`, the Verifier never requests a merge. A Host merge advances `preparing` or `pending` to `observed` before approval-note read. A merge found without prior intent first records `observed`, then recovers any missing Builder comment. A Verifier-confirmed request advances `pending` directly to `landed`. A read failure retains `observed`. Recovery lands it only after approval and passing Checks.
Iron Forest reads pull request identity only for idempotent publication and Host retirement recovery. It never treats Host review or check state as a Verdict or Gate.
Projected Checks and Verdicts use a `COMMENT` review whose `commit_id` is the exact Revision.
Host merge acceptance is recorded separately from its write claim. Recovery observes an accepted request without issuing it again.
Completed retirement removes its fact, attempt record, Subject brakes, and Effect claims in one atomic compare-and-delete transaction.

If branch loss hides a Projection before approval is readable, `preparing`, `pending`, or `observed` retirement blocks duplicate Builder work until exact Host state and durable approval join.

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
| `forest selfcheck` | Verify configuration, agents, pinned OpenCode, Bubblewrap, and user namespaces offline. |
| `forest watch [--interval 2s]` | Show the operator board. |

## Configuration

`forest.yaml` uses these keys. This is an example composition.

```yaml
repo: owner/name                 # required; no default
checks:
  - name: build
    run: go build ./...
  - name: vet
    run: go vet ./...
  - name: test
    run: go test ./...
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

It never reads a Host check or review.
Labels such as `exclude_labels` are Tracker inputs, not factory state.

Every agent and check runs inside a required Bubblewrap namespace. The
namespace exposes the worktree, private state, selected system files, validated
toolchains, and private per-run build caches. It builds a read-only Git view
from known history and diff metadata. It hides unrecognized Git administration
files, configuration, hooks, and sibling worktrees. Bubblewrap's PID-1 reaper contains detached
descendants. The trace drain observes the declared deadline.

Agent declarations and provider configuration use rooted file reads. Symlinks
cannot escape the repository. Provider configuration accepts only the declared
Mint OpenRouter shape. The network remains shared because OpenCode must reach
Mint. Iron Forest has no unsandboxed fallback.

Each check runs in a child environment with a private `HOME` and a scrubbed
`PATH`. Its tools resolve one of two ways, both stack-agnostic:

- **mise-managed tools** use the validated active mise root. Only its `installs`
  and `shims` trees are mounted read-only.
- **host toolchain executables** outside that `PATH` are staged when the operator
  names their directories with `FOREST_CHECK_PATH`. Sibling files never enter
  the namespace. The variable is a platform path-list, and these executables
  precede mise shims.

A host executable can be a proxy that needs toolchain metadata. Name its root
with `FOREST_CHECK_ENV`, a newline-separated list of `KEY=VALUE` pairs. Rustup
receives only its `toolchains` tree. Iron Forest derives `RUSTUP_TOOLCHAIN` from
validated non-secret settings instead of mounting the settings file.

Both variables reach only the **check** child, never an agent run: the host
toolchain mechanism is applied where a managed repo's `checks:` needs to find
its declared tools, and is withheld from the opencode agent, so neither host
binaries on `PATH` nor toolchain metadata can bleed into agent reach.

Iron Forest resolves each declared path before launch. It rejects protected
roots and symlink escapes. It copies declared executable files into private
read-only staging directories.

`FOREST_CHECK_ENV` carries only a curated allowlist of metadata variables.
Today the single variable is `RUSTUP_HOME`. Iron Forest exposes only its
validated `toolchains` subtree. It never mounts `CARGO_HOME`, because
`~/.cargo/credentials.toml` can hold a registry token. The allowlist keeps the
private `HOME`, scrubbed `PATH`, and private caches authoritative.

Building the wrong thing is worse than not building: Iron Forest does not guess a
stack. If a `checks:` command's tool is missing, the check fails and the note
names the command that could not start.

`flows.builder` selects items. Declaring `require_labels` changes selection from opt-out to opt-in. An open Item then needs every declared label.

An enabled Manager requires `require_labels: [forest:ready]`. This label is its assignment signal.

`flows.verifier.merge` is `squash` or `ff`. `flows.verifier.auto_merge` lets the Verifier merge an approved, passing branch.

When automatic merge is off, a native merge stays disabled. Host mode records `preparing` after the Builder comment and requests no merge.

A Host merge found through inspection advances to `observed` before approval-note read. A Verifier-confirmed merge advances from `pending` to `landed`.

`flows.fixer.attempts` bounds repairs. The Verifier gates branch merges with the same branch attempt record.

Projection keys control the optional human surface.

## Requirements

- Go 1.26.5 and OpenCode 1.18.11 through mise.
- Bubblewrap (`bwrap`) with unprivileged user namespaces enabled.
- Git with access to the repository.
- `gh`, the Host CLI used for Tracker items and Projections.
- A tracked `.opencode/opencode.json` at the repository root. It must use the
  Mint OpenRouter route `http://mint.tail5f5eb4.ts.net:4949/proxy/https/openrouter.ai/api/v1`
  and the marker `__mint.openrouter.ironforest__` (optionally with `Bearer `).
  Ambient or global OpenCode configuration is not used.

Build and run with the pinned toolchain:

```sh
mise install
go build -o forest .
./forest serve --factory-dir /path/to/iron-forest
```

Omit `--factory-dir` to disable self-update.

`forest serve` reads `forest.yaml` before each Flow pass. A committed configuration change takes effect without a process restart.

The first termination signal stops new Effects and lets current Effects finish. A second signal kills managed process groups and exits without waiting for repository I/O. The next startup reaps linked worktrees before any Flow starts.

Self-update waits until every Flow Effect is idle. It installs the tested binary and exits so the service supervisor can restart it. Deployed instances also serialize access to their shared factory source checkout.

## Ledger and board

`forest stats` reads `.forest/runs.jsonl` and prints totals and breakdowns. `forest stats --json` emits machine-readable ledger data.
`Status` is a Flow routing result. Summaries classify `built`, `reviewed`, `merged`, `fixed`, `done`, and `reaped` as progress. They classify any `*_failed` value as failed. Every other value is other.

`forest watch` reads Runs, tracked worktrees, git HEAD, and daemon state.
It shows each Flow and recent Effects.

## License

MIT. See [LICENSE](LICENSE).
