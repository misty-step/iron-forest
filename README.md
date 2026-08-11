# Iron Forest

Iron Forest is a self-hosted software factory. It is a small deterministic
Kernel plus an agent-owned profile, shipped as an appliance. The Kernel handles
mechanics. The profile declares what agents think and do.

The default profile has Builder, Verifier, and Fixer declarations. A Builder
turns a ready Tracker Issue into a branch. A Verifier checks and reviews the
exact Revision. A Fixer repairs a rejected Revision and sends it back for
review.

## Quick start

Create `forest.yaml` in the repository root:

```yaml
repo: misty-step/iron-forest
agents:
  builder:  { poll: "./forest poll builder",  interval: 300, timeout: 3600 }
  verifier: { poll: "./forest poll verifier", interval: 120, timeout: 1800 }
  fixer:    { poll: "./forest poll fixer",    interval: 300, timeout: 3600 }
checks:
  - name: build
    run: mise exec -- go build ./...
  - name: vet
    run: mise exec -- go vet ./...
  - name: test
    run: mise exec -- go test ./...
```

Declare each agent with OMP-compatible files:

```text
agents/<name>/agent.md
agents/<name>/task.md
```

`agent.md` uses YAML frontmatter with required `model` and optional `tools` and
`thinking`, followed by the system prompt. `task.md` is the standing user
prompt. `model`, `tools`, and `thinking` belong to the declaration. OMP provider
routing is host-managed, not repository configuration.

This quick start uses self-host mode: the factory source checkout is also the
managed repository. For a separate sibling managed checkout, use the
[onboarding guide](docs/onboarding-managed-repo.md); its installer builds the
Kernel from the factory source into that sibling.

Build with the pinned toolchain and validate local configuration:

```sh
mise exec -- go build -o forest .
./forest selfcheck
```

`forest selfcheck` validates `forest.yaml` and declaration frontmatter locally.
The read-only Auditor runs after each completed agent dispatch. Starting the
Kernel alone, or receiving only healthy Poll skips, does not audit the remote.

Start the Kernel:

```sh
./forest serve
```

Use exactly one Kernel checkout and process per repository. Its OS lock rejects
a second process in that checkout. It does not coordinate another checkout or
clone. Every Poll has a fixed 60-second deadline. The configured declaration timeout
separately bounds worktree preparation and agent execution. Runner cleanup has
a separate 10-second bound. A completed dispatch starts an audit with a
separate 60-second bound. The systemd unit has a separate 3900-second service
drain bound. This bound covers the shipped declarations' concurrent Runs,
bounded Runner cleanup, and serialized post-dispatch audits.
The user service receives
`PATH=%h/.local/bin:%h/bin:/usr/local/bin:/usr/bin:/bin`. Before restart, the
installer runs selfcheck with the equivalent `$HOME`-expanded path.

A trusted declaration runs with the operating-system user's configured
credentials and filesystem access. Worktree separation and time bounds are
operational boundaries, not a security sandbox. Stronger credential and process
containment belongs to the deployment substrate.

## Git coordination

Git is the coordination authority. Branches, commits, and notes under
`refs/notes/forest/*` hold workflow state. GitHub is the day-one forge adapter;
pull requests are disposable human Projections, never authority.

Every record binds to an exact Revision. The schema and writer sets are defined
in [ADR 0009](docs/adr/0009-git-coordination-authority.md): Builder and Fixer
write review requests, while Verifier writes Checks and Verdict notes.

Notes are write-once. Agents write each JSON payload to a temporary file, add it
with `git notes ... add -F`, and never use force. Builder and Fixer publish the
branch and review-request note through one normal `git push --atomic`. Their
canonical note race recovery permits at most three total atomic attempts; a
branch race stops. For a `changes` Verdict, the Verifier publishes Checks and
Verdict together and permits at most three total atomic attempts after a
canonical note race. For `approve`, the Verifier makes exactly one
non-retryable atomic attempt carrying Checks, Verdict, and the exact
fast-forward `master` advance. The existing review-request remains durable Gate
evidence; no standalone master push is valid. See [ADR
0009](docs/adr/0009-git-coordination-authority.md) for the absent-ref and
bounded retry protocol.

Agents own workflow Effects. The Kernel never writes workflow notes or merges a
branch. See [managed-repository onboarding](docs/onboarding-managed-repo.md)
for the operator procedure.

## Poll protocol

A Poll is a yes-or-no trigger. It passes no context; the agent selects its
Subject during the Run. Builder, Verifier, and Fixer each have a disjoint Poll
command. Exit 0 dispatches work, exit 1 is a healthy skip, and exit greater
than 1, timeout, or malformed behavior records an unhealthy trigger. See
[ADR 0012](docs/adr/0012-poll-trigger-protocol.md) and the
[onboarding guide](docs/onboarding-managed-repo.md) for selection rules.

## Merge Gate

The Gate requires exactly one valid Builder-or-Fixer review-request, passing
Checks, and an approving Verdict for the same exact Revision, plus a
fast-forward of `master` to that Revision. The approve publication is one atomic
push containing Checks, Verdict, and the exact `master` advance. The existing
review-request is not republished. The merge never uses force. The Gate is a
profile contract. Except for the trusted first `master` baseline, the Auditor
checks its observable final state after the Effect; it does not enforce it. See [ADR
0010](docs/adr/0010-agent-owned-effects-and-merge-gate.md) for the Gate and its
accepted client-side risks.

## Auditor and trust boundary

The Kernel Auditor is read-only. The first observed remote `master` tip becomes
a trusted baseline and is not Gate-checked. In each bounded stable snapshot,
ancestry and Gate checks target only the final observed remote `master` tip.
Schema and actor checks still cover every entry in every snapshotted
`refs/notes/forest/*` ref, including the baseline snapshot. Remote history
cannot reveal a tip that advanced again between audits; such intermediate tips
are not independently Gate-checked. The audit covers only observable final Git
state. It cannot prove check execution, atomic push ordering, or force absence.

The Auditor runs after a completed dispatch. It logs violations and marks the
last audit as `violations` in `forest status`; it never blocks a merge. Startup
and idle Poll skips do not start an audit.

Day-one worktree separation and time bounds do not hide operating-system
credentials, filesystem access, or network access from a trusted declaration.
Deployment supplies any stronger process or credential containment.

## Commands

| Command | Purpose |
| --- | --- |
| `forest serve` | Poll and dispatch enabled declarations. |
| `forest once <agent>` | Poll once, then dispatch that declaration only when the Poll exits 0. |
| `forest poll <agent>` | Evaluate one declaration's trigger. |
| `forest status` | Show trigger health, live runs, the last audit result, and recent runs. |
| `forest selfcheck` | Validate `forest.yaml` and declarations locally. |

## Development

Use `master` as the target branch. The repository's `checks:` commands must
match `.github/workflows/ci.yml`.

```sh
mise exec -- go build ./...
mise exec -- go vet ./...
mise exec -- go test ./...
```

The Ledger is `.forest/runs.jsonl`. Each row records Run identity (`run_id` and
`agent`), timing (`started` and `duration`), `exit`, and the token classes
`tokens_in`, `tokens_out`, `cache_read`, `cache_write`, and `reasoning`. It never
records or computes money.

## License

MIT. See [LICENSE](LICENSE).
