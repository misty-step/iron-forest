# Iron Forest

Iron Forest is a self-hosted system that turns Tracker items into reviewed code changes.
Three independent Flows run in one process and coordinate through git state.

## Flows

Each Flow selects a Subject, takes a Lease, performs its Effect, and records a Run.
Each lane uses its own interval. No scheduler coordinates the lanes.

| Flow | Selector | Effects |
| --- | --- | --- |
| Builder | Eligible Tracker items without a forest branch and without configured exclude labels. | Creates an isolated worktree, runs the Builder, checks the Gate, and pushes a branch. It may create a Projection. |
| Verifier | Forest branches with no Verdict, or approved branches with passing Checks. | Runs the configured Checks, writes the Checks note, obtains an independent Verdict, and can merge an approved branch. |
| Fixer | Branches with a rejected Verdict or failed Checks below the attempt limit. | Runs the Builder on the branch, passes the Gate, pushes the repair, and records the attempt. An exhausted branch gets `forest:failed` for a human. |

## State in git

Git is the coordination surface. A Host can run any Flow because state stays in the repository.

- **Lease:** `refs/forest/lease/<subject>` stores ownership. Iron Forest creates it with a create-if-absent compare-and-set. Labels are not state.
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
| `forest serve [--flow <name>]...` | Run all enabled Flows, or only the named Flows. |
| `forest run <flow> <subject>` | Run one selected Subject by key, branch, or issue number in one Flow. |
| `forest show <sha>` | Print the Verdict and Checks notes for a commit. |
| `forest version` | Print the binary Revision. |
| `forest selfcheck` | Verify configuration and agent declarations offline. |
| `forest watch [--interval 2s]` | Show the operator board. |

## Configuration

`forest.yaml` uses these keys. The values below are the defaults from `config.go`.

```yaml
repo: owner/name                 # required; no default
protected: [.forest/, forest.yaml, agents/, .opencode/opencode.json]
lease:
  ttl_seconds: 7200
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

`repo` names the Tracker repository. `protected` lists paths the Gate rejects. `lease.ttl_seconds` permits recovery of an old Lease; zero disables expiry.

Iron Forest runs each command in `checks:` and writes one Checks note.
It never reads a Host check or review.
Labels such as `exclude_labels` are Tracker inputs, not factory state.

`flows.builder` selects items. `flows.verifier.merge` is `squash` or `ff`.
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
./forest serve
```

## Ledger and board

`forest stats` reads `.forest/runs.jsonl` and prints totals and breakdowns. `forest stats --json` emits machine-readable ledger data.

`forest watch` reads Runs, lease refs, git HEAD, and daemon state.
It shows each Flow and recent Effects.

## License

MIT. See [LICENSE](LICENSE).
