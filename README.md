# Iron Forest

Iron Forest is a self-hosted software factory. It is a small deterministic
Kernel plus agent-owned declarations, shipped as an appliance. The Kernel
handles mechanics. Declarations state what agents think and do.

The shipped roster has Builder, Verifier, and Fixer declarations. A Builder
turns a ready Tracker Issue into a branch. A Verifier checks and reviews the
exact Revision. A Fixer repairs a rejected Revision and sends it back for
review.

## Quick start

Create `forest.yaml` in the repository root:

```yaml
repo: misty-step/iron-forest
agents:
  builder:  { poll: "./forest poll builder",  interval: 300 }
  verifier: { poll: "./forest poll verifier", interval: 120 }
  fixer:    { poll: "./forest poll fixer",    interval: 300 }
checks:
  - name: build
    run: mise exec -- go build ./...
  - name: vet
    run: mise exec -- go vet ./...
  - name: test
    run: mise exec -- go test ./...
```

Declare each agent with two prompt files. Skills live only in the shared
directory and, when a role needs private skills, its own directory:

```text
agents/<name>/agent.md
agents/<name>/task.md
agents/_shared/skills/          # every declaration
agents/<name>/skills/           # optional; this declaration only
```

`agent.md` uses YAML frontmatter with optional `model`, `tools`, and `thinking`,
followed by the system prompt. `task.md` is the standing user prompt. `model`
and `thinking` resolve through the declaration, then `forest.defaults.yaml` (or
`$FOREST_DEFAULTS`), then — for `model` only — the built-in
`openrouter/deepseek/deepseek-v4-pro-0813`. An empty or comment-only defaults
file is the zero Defaults, not an error. `forest declaration show` publishes
the resolved model and its source.

Every Run gives Pi a new writable agent directory through
`PI_CODING_AGENT_DIR`; no operator Pi state is inherited. For an OpenRouter
model, the Runner writes only a credential-free `models.json` override that
enables Pi's OpenRouter session-affinity header. Pi extension, skill,
prompt-template, and theme discovery are disabled with `--no-extensions`,
`--no-skills`, `--no-prompt-templates`, and `--no-themes`. The Runner passes
each existing skill source directory with an explicit `--skill`. Those paths
are repository-relative and Pi resolves them from the Run worktree.
Declaration and Run evidence publish the directories as `skills`.

Pi's exact session ID is the Run ID. The generated OpenRouter model override
makes Pi send it as `x-session-id`, so Broadcast destinations can group every
model request for a Run and correlate the provider trace directly with the
Ledger and `.forest/runs/<run-id>.log`.

Credentials come only from the service environment inherited by the Run.
Declaration frontmatter has no `env` field; unknown metadata fails validation.
Credentials do not belong in prompts, skills, defaults, or commits. The shared
skills verify claims and debug failures. Verifier also receives the deep
correctness and code-quality review skills under `agents/verifier/skills/`;
Builder and Fixer receive only the shared skills. Each role's always-on
engineering rules live directly in `agent.md`, alongside the exact Git-note
protocol.

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
Runner cleanup and prunes their registry entries. One `update-ref` transaction
removes private Runner, Poll, and Audit refs. It removes only known stale
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
snapshot refs before the supervisor force-stops its command group. Agent Runs
have no wall-clock deadline. They finish when Pi finishes or an
operator explicitly cancels a foreground `forest once`; service shutdown stops
new dispatches and drains active Runs without a systemd deadline. Runner
cleanup has a separate 10-second bound. A completed dispatch starts an audit
with a separate 60-second bound. These mechanical bounds do not limit agent
reasoning or model execution.

The user service receives
`PATH=%h/.local/bin:%h/bin:%h/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin`,
loads operator-supplied credentials from
`%h/.config/iron-forest/%i.env`, and unsets `FOREST_DEFAULTS`. It does not set
`PI_CODING_AGENT_DIR`; the Runner owns that variable for each Run. Before
restart, the installer stops the instance, removes timestamped legacy
`.forest/profiles` residue, and runs selfcheck with the equivalent
`$HOME`-expanded environment.

Protect the environment file as mode `0600`. The current Runner accepts one
OpenRouter completion key for the instance:

```dotenv
OPENROUTER_API_KEY=<dedicated instance key>
```

Do not put an OpenRouter management key in this file. Do not reuse a personal
interactive key or an evaluation key. The intended production layout uses one
completion key per agent role for OpenRouter and Langfuse cost, latency, and
failure attribution. It is not a security boundary. The current Runner does not
select role-specific keys; use the Run ID and agent name in Forest evidence for
exact attribution until that data path is implemented.

Trusted transport captures keep at most 1 MiB while draining the complete
output. Output beyond the cap returns an explicit error after the process group
stops. Each Run log retains at most 2 MiB of output. When truncated, it contains
the exact first 1 MiB, an explicit marker, and the exact last 1 MiB. The marker
is the only file content outside the 2 MiB output cap. The Runner retains the 32
newest completed reserved `.log` files. It does not remove active logs or
foreign entries.
Pi's terminal `agent_end` event is authoritative: a terminal assistant error
fails the Run even when the Pi process exits zero.

A trusted declaration runs with the inherited service credentials and filesystem
access. Worktree separation and time bounds are
operational boundaries, not a security sandbox. Stronger credential and process
containment belongs to the deployment substrate.

## Git coordination

Git is the coordination authority. Branches, commits, and notes under
`refs/notes/forest/*` hold workflow state. GitHub is the day-one forge adapter;
pull requests are disposable human Projections, never authority.

Every record binds to an exact Revision. The schema and writer sets are defined
in [ADR 0009](docs/adr/0009-git-coordination-authority.md): Builder and Fixer
write review requests, while Verifier writes Checks and Verdict notes.

Notes are write-once. Builder and Fixer write the review-request payload, then
call `forest publish review-request`. The Kernel adds the note with
`git notes ... add -F`, stamps the role identity, and publishes the branch and
note through one normal `git push --atomic`. Canonical note race recovery
permits at most three total atomic attempts; a branch race stops. For a
`changes` Verdict, the Verifier still publishes Checks and Verdict together
and permits at most three total atomic attempts after a canonical note race.
For `approve`, the Verifier makes exactly one non-retryable atomic attempt
carrying Checks, Verdict, and the exact fast-forward `master` advance. The
existing review-request remains durable Gate evidence; no standalone master
push is valid. See [ADR 0021](docs/adr/0021-kernel-review-request-publication.md)
and [ADR 0009](docs/adr/0009-git-coordination-authority.md).

Agents own Issue selection, implementation, and the decision to publish.
The Kernel owns the review-request race loop. The Verifier still owns Checks,
Verdict, and merge. See [managed-repository
onboarding](docs/onboarding-managed-repo.md) for the operator procedure.

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
role contract. Except for the trusted first `master` baseline, the Auditor
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
| `forest run cancel <run-id>` | Stop a live Run's process group and record the cancellation in the Ledger. |
| `forest run logs [--follow] <run-id>` | Print a Run log, or stream it until the Run completes. |
| `forest audit show [--rescan]` | Print audit state, optionally re-running the Auditor first. |
| `forest audit log` | Print audit history. |
| `forest publish review-request <role> <branch> <payload> [--rejected <sha>]` | Publish a Builder or Fixer review-request note and branch. |

### Reading the factory

`serve`, `once`, and `poll` are the engine: they hold the Kernel lock and write.
`publish review-request` writes without taking that lock, so a Run that already
holds it can publish. `run cancel` also writes without taking the Kernel lock,
because the live Run's Runner already holds it. Every other row is the read
surface. Each read-surface command accepts `--json` and `--root <dir>`. `--json` emits one
`forest.cli.v2` envelope on stdout; human text stays on stderr. `--root`
answers from another checkout. `trigger reset` and `audit show --rescan` take
the Kernel lock and refuse while a Kernel runs. `publish review-request` does
not.

`--json` emits exactly one envelope on stdout, including on failure:

```json
{"schema":"forest.cli.v2","command":"run show","args":["<run-id>"],"exit":0,"data":{},"error":null}
```

`command` names the verb only and selects the `data` shape; operands live in
`args`. `data` is `null` when a command fails, and `error` is the reason. Keys
are snake_case throughout, and an empty collection is `[]`, never `null`.
Adding a key is compatible; renaming or removing one requires the next schema
version. Version 2 replaces declaration `profile_files` with `skills` and
removes declaration `env`.

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

Run the deterministic Harbor role regressions before changing an agent
declaration, prompt, skill, or publication contract:

```sh
./evals/run-fast.sh
```

The manual model tier runs every Builder, Verifier, and Fixer case three times
through the production `forest` binary. It uses each declaration's model unless
`FOREST_EVAL_CANDIDATE_MODEL` is set. The independent Judge defaults to
`openrouter/google/gemini-3.7-flash`.

Local runs load separate candidate and Judge completion keys from
`$HOME/.config/iron-forest/evals.env` by default. The file must be owned by the
current user, have mode `0600`, and contain only:

```dotenv
OPENROUTER_API_KEY=<evaluation candidate key>
FOREST_EVAL_JUDGE_API_KEY=<evaluation Judge key>
```

Existing environment values take precedence. `FOREST_EVAL_ENV_FILE` selects a
different file. The OpenRouter management key stays outside production and
evaluation runtime environments.

```sh
./evals/run-model.sh
```

The `model evals` GitHub workflow maps the distinct repository secrets
`IRON_FOREST_EVAL_CANDIDATE_API_KEY` and
`IRON_FOREST_EVAL_JUDGE_API_KEY` into those runtime names. Harbor outputs remain
under `evals/jobs/`, which is ignored by Git.

The Ledger is `.forest/runs.jsonl`. Each row records Run identity (`run_id` and
`agent`), timing (`started` and `duration`), `exit`, and the token classes
`tokens_in`, `tokens_out`, `cache_read`, `cache_write`, and `reasoning`. It never
records or computes money.

## License

MIT. See [LICENSE](LICENSE).
