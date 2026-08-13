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

Declare each agent with two files, and optionally a profile layer:

```text
agents/<name>/agent.md
agents/<name>/task.md
agents/<name>/profile/          # optional; this declaration only
agents/_shared/profile/         # optional; every declaration
```

`agent.md` uses YAML frontmatter with optional `model`, `tools`, `thinking`, and
`env`, followed by the system prompt. `task.md` is the standing user prompt.
`model` and `thinking` resolve through the declaration, then
`forest.defaults.yaml` (or `$FOREST_DEFAULTS`), then — for `model` only — the
built-in `openrouter/deepseek/deepseek-v4-flash-0731`. An empty or
comment-only defaults file is the zero Defaults, not an error.
`forest declaration show` publishes the resolved model and its source. `env`
values are literals or `mint:<alias>` references rewritten to
`__mint.<alias>__`. The Kernel never prints an env value, including in JSON.

Each Run gets a private harness profile under `.forest/profiles/<run-id>`.
The operator's base profile (from defaults) copies first and may hold
credentials. The shared repository layer copies next. The declaration's own
layer copies last and wins on a name collision. A repository layer may not
contain `auth.json` or a symlink. The child sees the result through
`PI_CODING_AGENT_DIR`. Reserved startup GC removes leftover profiles.

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

Before `serve` or `once` loads trigger health, the Scheduler performs reserved
garbage collection under the Kernel lock. One 30-second deadline bounds the
total operation. It removes reserved `.forest/worktrees/<run-id>` paths through
Runner cleanup and prunes their registry entries. It removes reserved
`.forest/profiles/<run-id>` paths through the same trusted remover. One
`update-ref` transaction removes private Runner, Poll, and Audit refs. It removes only known stale
`audit.json`, `audit.log`, and `triggers.json` temps. The Ledger owns Ledger
temps. Run log retention owns Run logs. Any cleanup error blocks startup.
Reserved garbage collection never resumes a Run.

Start the Kernel:

```sh
./forest serve
```

Use exactly one Kernel checkout and process per repository. Its OS lock rejects
a second process in that checkout. It does not coordinate another checkout or
clone. Direct `forest poll` execution has a fixed 60-second deadline. The
Scheduler gives its configured Poll command a separate 65-second bound. The
supervisor preserves this full 5-second difference as Poll shutdown grace. It
lets the direct Poll stop Git/GitHub transport groups and remove private note
snapshot refs before the supervisor force-stops its command group. The
configured declaration timeout separately bounds worktree preparation and agent
execution.
Runner cleanup has a separate 10-second bound. A completed dispatch starts an
audit with a separate 60-second bound. The systemd unit has a separate
3900-second service drain bound. This bound covers the shipped declarations'
concurrent Runs, bounded Runner cleanup, and serialized post-dispatch audits.
The user service receives
`PATH=%h/.local/bin:%h/bin:/usr/local/bin:/usr/bin:/bin`. Before restart, the
installer runs selfcheck with the equivalent `$HOME`-expanded path.

Trusted transport captures keep at most 1 MiB while draining the complete
output. Output beyond the cap returns an explicit error after the process group
stops. Each Run log retains at most 2 MiB of output. When truncated, it contains
the exact first 1 MiB, an explicit marker, and the exact last 1 MiB. The marker
is the only file content outside the 2 MiB output cap. The Runner retains the 32
newest completed reserved `.log` files. It does not remove active logs or
foreign entries.

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

Verifier and Fixer Poll note enumeration is bounded at 500 entries per
canonical notes tree. A larger tree, or a note-enumeration transport-output
overflow, is a healthy exit-1 skip with an explicit log line. It never marks
the trigger unhealthy; the Auditor reports durable note growth as a bounded
policy violation.

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
Schema and actor checks cover each snapshotted `refs/notes/forest/*` entry
within a 500-entry-per-ref capacity bound. A ref that exceeds that bound, a
note enumeration or note-show transport-output overflow, a note payload
above 64 KiB, or malformed or unresolvable canonical note state (malformed
list or tree rows, a listed note without its tree entry, a mismatched,
unexpected, or duplicate tree entry, a non-SHA path, a non-blob entry, or a
note object missing from the object database) becomes a bounded persisted
policy violation and a non-pass Audit
result, never an AuditError. Current Audit results retain at most 999 concrete
violation entries, each at most 1 KiB, plus one exact omission summary. Remote
history
cannot reveal a tip that advanced again between audits; such intermediate tips
are not independently Gate-checked. The audit covers only observable final Git
state. It cannot prove check execution, atomic push ordering, or force absence.

The Auditor runs after a completed dispatch. It stores current violations in
`audit.json` and marks the last Audit as `violations` in `forest status`. It
appends violations to `audit.log` only when the current set differs from the
prior persisted set. A passing Audit clears current violations and adds no
history. Audit history retains exactly the latest 1,000 violation entries. The
Auditor never blocks a merge. Startup and idle Poll skips do not start an Audit.

## Commands

| Command | Purpose |
| --- | --- |
| `forest serve` | Poll and dispatch enabled declarations. |
| `forest once <agent>` | Poll once, then dispatch that declaration only when the Poll exits 0. |
| `forest poll <agent>` | Evaluate the built-in trigger for `builder`, `verifier`, or `fixer`. |
| `forest status` | Show Poll, Run, and Audit errors, live Runs, the last audit result, and recent Runs. |
| `forest selfcheck` | Validate `forest.yaml` and declarations locally. |
| `forest config show` | Print the loaded configuration. |
| `forest declaration list\|show <name>` | Print declaration names, or one declaration in full. |
| `forest trigger list\|show <agent>` | Print resolved trigger state. |
| `forest trigger reset <agent>` | Clear one agent's accumulated errors. Refuses while a Kernel runs. |
| `forest run list` | Page the Ledger, newest first. |
| `forest run show <run-id>` | Print one Ledger row. |
| `forest run logs [--follow] <run-id>` | Print a Run log, or stream it until the Run completes. |
| `forest audit show [--rescan]` | Print audit state, optionally re-running the Auditor first. |
| `forest audit log` | Print audit history. |

### Reading the factory

`serve`, `once`, and `poll` are the engine: they hold the Kernel lock and write.
Every other row is the read surface. Each read-surface command accepts
`--root <dir>` to read another checkout and `--json` to emit one envelope, and
each refuses a directory that holds no `forest.yaml` rather than reporting an
empty factory. Two of them write: `trigger reset` and `audit show --rescan` hold
the Kernel lock across the write and exit `5` while a Kernel holds it. Every
other read-surface command only reads, under a shared lock that never blocks the
Kernel.

`--json` emits exactly one envelope on stdout, including on failure:

```json
{"schema":"forest.cli.v1","command":"run show","args":["<run-id>"],"exit":0,"data":{},"error":null}
```

`command` names the verb only and selects the `data` shape; operands live in
`args`. `data` is `null` when a command fails, and `error` is the reason. Keys
are snake_case throughout, and an empty collection is `[]`, never `null`.
Adding a key is compatible; renaming or removing one requires `forest.cli.v2`.

Each payload publishes what the command resolved. Three keys guard the rest and
must be read first:

| Key | Meaning when false |
| --- | --- |
| `state_known` (per trigger) | No state is recorded for that agent, so its counters are zero because they are unknown, not because they are zero. |
| `state_present` (`trigger list`) | No trigger state file exists yet. |
| `complete` (`run logs`) | The Run has not finished, so `exit` is absent. |

`trigger list` publishes `state_error` and `status` publishes
`trigger_state_error`; both carry the same reason, scoped to their payload.
`status` also publishes `kernel.running` with `kernel.running_known`, which is
the only way to learn whether a Kernel holds the lock.

`run list` and `audit log` accept `--limit N`; `run list` defaults to 50 rows.
`run list` also accepts `--after <run-id>` and returns `next_after`, which is
empty on the last page. Paging with a cursor that no longer names a Run exits 4
rather than silently restarting, and a ledger whose identities are duplicated or
empty cannot carry a cursor, so paging fails instead of looping.

`run list` returns Runs newest first. `status` reports at most ten recent Runs in
Ledger order, oldest first, because it is a snapshot of the tail rather than a
pager; its human output labels the order.

Exit codes are stable: `0` success, `1` no work, `2` error, `4` not found,
`5` conflict, `6` invalid argument. One command leaves that space deliberately:
`run logs --follow` exits with the followed Run's own exit code, so its exit does
not carry the meanings above. It therefore also refuses `--json`, since one
envelope cannot describe a stream, and it names the Run's outcome on stderr so a
relayed code is distinguishable from a CLI verdict. Ticket #257 specifies that
relay; script against the stderr line or use `run show` for a structured answer.

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
