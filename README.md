# Iron Forest

A self-hosted software factory. `forest` is a daemon that takes work items from
a GitHub issue backlog, builds each one in an isolated git worktree with an LLM
agent, checks the result against a deterministic gate, gets an independent
review, and publishes a pull request — recording every run and its cost in an
append-only ledger.

The operator shapes work and reads evidence; the factory does the chewing.

## Requirements

- Go 1.24 or newer (see `.mise.toml` for the pinned toolchain).
- The `forest` daemon talks to GitHub through `gh` and to providers through
  OpenCode and a provider key. Those are only needed to run the factory, not to
  build or test the code.
- Building, testing, and formatting are offline *once the Go modules are
  present in the local module cache*. A fresh clone with an empty cache will
  download the single dependency (`gopkg.in/yaml.v3`) on first build, because
  it is not vendored.

## Build, test, format

The repository offers one obvious command per task via Make:

```sh
make build   # compile the forest binary at the repository root
make test    # run the Go test suite (offline)
make fmt     # gofmt every Go file in place
make vet     # go vet across the module
make clean   # remove build artifacts
```

## Install and run

```sh
make build
./forest
```

With no arguments, `forest` prints its usage and exits with status 2. Point it
at a repository with a `forest.yaml` by running it from that directory.

### Systemd daemon

Deploying as a user service is one command:

```sh
deploy/install-service.sh
```

The unit (`deploy/forest-chew.service`) runs `forest chew` from the repo
directory, survives reboots, and gives a draining pass unlimited time to finish
an in-flight agent.

## Commands

`forest` reads a `forest.yaml` for its configuration and resolves the working
directory as the repository root. Every subcommand that touches the network
requires `gh` to be authenticated.

| Command | What it does |
| --- | --- |
| `forest list` | Print the current backlog (open tracked issues). |
| `forest agents` | List declared agents under `agents/` and their composition digest. |
| `forest stats [--json]` | Aggregate the run ledger from `.forest/`. Pass `--json` for machine-readable output. |
| `forest once <issue>` | Chew a single issue end to end: claim, worktree, build, gate, review, publish, record. |
| `forest chew` | Poll: chew the backlog AND watch open factory PRs until they merge or stall. |
| `forest version` | Print the git SHA this binary was built from. |
| `forest selfcheck` | Offline smoke gate: config loads and agents resolve. |
| `forest watch [--interval 2s] [--live-gh]` | Live operator board over `.forest/` and the daemon. |

Run `forest` with no arguments to see the same list printed by `usage()`.

## Configuration

`forest.yaml` at the repository root declares the backlog source, poll cadence,
factory-wide protected paths, and the workflow:

```yaml
repo: misty-step/iron-forest
poll_interval_seconds: 30
protected:
  - .forest/
  - forest.yaml
  - agents/
  - .opencode/opencode.json
workflow:
  build: beaver
  review: owl
  max_fix_iterations: 1
  auto_merge: true
  max_reaction_fixes: 2
```

- `repo`: the `owner/name` GitHub repository that is the backlog source.
- `poll_interval_seconds`: seconds between passes over the backlog.
- `protected`: paths no agent may modify; the gate enforces it.
- `workflow.build`: the agent that implements changes.
- `workflow.review`: the agent that reviews them; `""` disables review.
- `workflow.max_fix_iterations`: corrective build passes before a final verdict.
- `workflow.auto_merge`: merge PRs automatically when the reviewer approved, CI
  is green, and no human `CHANGES_REQUESTED` review is open (default off).
- `workflow.max_reaction_fixes`: how many re-entry passes the loop makes on one
  PR before parking it for a human.

## How the factory works

1. **Claim.** The loop takes one item from the backlog and labels it.
2. **Worktree.** It creates an isolated git worktree from the current master.
3. **Build.** The build agent (`beaver`) implements the change and writes a
   `report.json` matching its schema.
4. **Gate.** A deterministic gate checks the report, confirms the base SHA is
   unchanged, and rejects any protected-path write.
5. **Review.** The review agent (`owl`) independently checks the diff and writes
   a verdict. Its model family is separate from the builder's.
6. **Publish.** An approved change is committed and pushed; a pull request is
   opened and linked to the issue.
7. **React & record.** The loop watches open PRs (CI and human feedback),
   re-enters the builder on change requests, and appends a costed run row to
   the ledger.

## Agents

Agent definitions are data, one directory per agent under `agents/`. Each
directory names the model, permissions, budget, MCP wiring, system prompt, and
output contract. See [AGENTS.md](AGENTS.md) for how to read and add an agent.

## License

MIT — see [LICENSE](LICENSE).
