# Iron Forest

Iron Forest is a headless software factory. Exactly one live Kernel serves
one repository, on a machine the operator chooses. The Kernel handles
mechanics. Declarations state what agents think and do. See
[VISION.md](VISION.md).


The shipped review roster has Builder, Verifier, and Fixer. A Builder turns a
ready GitHub Issue or takeable Powder job into a branch. A Verifier checks and
reviews the exact Revision. A Fixer repairs a rejected Revision and sends it back
for review. Critic sweeps the codebase and files Powder drafts; it never edits
code or joins the review loop. Tester maps under-tested observable behaviors
into test-work Powder drafts; it never edits code or joins the review loop.
Critic and Tester are EXPERIMENTAL and local-canary-only; see the rollout hold
below.


## Critic and Tester rollout hold

Critic and Tester are EXPERIMENTAL and local-canary-only. Iron Forest keeps
them enabled in this self-host checkout only for canary observation. External
operators must not copy or enable them until the rollout exit gate below
closes.

Rollout exit gate:

- the blocking repair jobs are merged:
  `if-investigator-provenance-contract`, `if-eval-powder-mutations`,
  `if-tester-eval-observable-surface`, `if-eval-draft-note-binding`, and
  `if-investigator-powder-availability`;
- the corrected deterministic evals pass; and
- one post-fix live sweep per role produces attributable spec-less drafts.

Their Polls skip cleanly (exit 1) when Powder is not configured: `POWDER_AGENT`
and one of `POWDER_URL` or `POWDER_API_BASE_URL` must both be set, otherwise a
daily sweep has no durable output and GitHub-only deployments wake no
investigator Run.


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

### Repository-owned composition

`forest.yaml` accepts arbitrary declaration names. The roster above is the
shipped opinionated profile, not a Kernel enum or a required workflow. Each
managed repository may supply its own Polls, prompts, model, thinking level,
Pi tool allowlists, shared skills, role skills, and Checks. One Kernel still
serves exactly one repository; an external manager coordinates several
instances through their CLI read surfaces.

Start each declaration with Pi's smallest useful tool set. A role can use an
installed CLI such as `gh`, `powder`, or a browser driver only when `bash` is
in that declaration's Pi tool allowlist and an explicit skill defines the CLI
contract. Pi extensions are different: the Runner disables extension discovery,
and declarations cannot yet select an extension path. Do not add an
extension-provided tool name until the Runner
has an explicit, inspectable extension input and the role proves one real
scenario with it.

The current shipped workflow still contains GitHub and Powder mechanics in the
Kernel. Tracker-independent profile executables and Habitat composition are
open architecture work; a custom Poll alone does not yet make the complete
review and terminal lifecycle tracker-agnostic.

Agent Runs have no wall-clock deadline by default. To bound a declaration that
has wedged before, set the optional `max_duration` key (seconds) under that
agent; `0` or an omitted key leaves the Run unbounded:

```yaml
agents:
  fixer: { poll: "./forest poll fixer", interval: 300, max_duration: 3600 }
```

When `max_duration` is set, the Kernel's progress watchdog cancels a Run that
exceeds the bound using the same supported cancel path as `forest run cancel`,
records the cancellation in the Ledger, and returns the trigger to a clean
not-running state.

Declare each agent with two prompt files. Skills live only in the shared
directory and, when a role needs private skills, its own directory:

```text
agents/<name>/agent.md
agents/<name>/task.md
agents/_shared/skills/          # every declaration
agents/<name>/skills/           # optional; this declaration only
```

Operator-supervised (org) skills live under `org-skills/`. They are not factory
skill sources; the Runner never auto-loads them, and an operator passes one to a
supervised `pi` session explicitly.

Iron Forest ships the operator-facing
[`iron-forest` skill](org-skills/iron-forest/SKILL.md). Pass that directory
explicitly to a supervised Pi or company-agent session that configures,
operates, or observes one or more repository instances.

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

### Second-party deployment checklist

To adopt an existing repository as a second-party deployment, follow the
[onboarding guide](docs/onboarding-managed-repo.md) and finish these checks
before handoff:

1. Add `forest.yaml`, `agents/`, and `checks:` to the new repository; mirror
   `checks:` in the new repository's CI.
2. Build and validate locally:
   `mise exec -- go build -o forest . && ./forest selfcheck`.
3. Install the service with `deploy/install-service.sh <sibling-directory-name>`
   (no argument in self-host mode).
4. Verify the installed service is active without starting a second Kernel:
   `systemctl --user is-active forest@<sibling-directory-name>` (expect
   `active`) and, from the managed checkout, `./forest status`.
5. Record the deployment using the registry fields in the
   [ready contract](docs/forest-ready-contract.md#deployment-registry):
   `identity`, `host`, `repo`, and the running revision from `./forest version`.
6. File every external finding with the
   [draft-note provenance convention](docs/templates/powder-job-spec.md#external-draft-note):
   report `filed-by`, `deployment`, and evidence, and never pass `--spec` at
   filing.
7. Confirm observability before rollout with the read surface. After the first
   completed dispatch, run `./forest status` and `./forest audit show`. To force
   a rescan, confirm the service is inactive first:
   `systemctl --user stop forest@<sibling-directory-name>`, then
   `./forest audit show --rescan`, then
   `systemctl --user start forest@<sibling-directory-name>`.
8. When the planned `forest doctor` surface lands (if-293), run it and resolve
   every finding before declaring the deployment complete.

Build with the pinned toolchain and validate local configuration:

```sh
mise exec -- go build -o forest .
./forest selfcheck
```

`forest selfcheck` validates `forest.yaml` and declaration frontmatter locally.
The read-only Auditor runs after each completed agent dispatch. Starting the
Kernel alone, or receiving only healthy Poll skips, does not audit the remote.

### Adopting merged revisions

Adopt merged revisions with the fenced update procedure:

    deploy/install-service.sh update <instance>

It checks that the working tree is clean, stops the service (which stops new
dispatches and drains live Runs), confirms the instance is inactive,
fast-forwards the checkout to the remote primary, rebuilds, runs
`./forest selfcheck`, forces a fresh audit with `./forest audit show --rescan`,
restarts the service, and verifies it is active. Never restart-only: the unit
runs the checkout-local binary.

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
POWDER_URL=<origin>
POWDER_API_KEY=<key>
POWDER_AGENT=forest-<repo-slug>
```

`POWDER_AGENT` opts the Kernel into listing Powder jobs. Omit it for
GitHub-only selection. Use one agent identity per repository Kernel.

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

A trusted declaration runs with the inherited service credentials and
filesystem access. Worktree separation and time bounds are operational
boundaries, not a security sandbox. Stronger containment belongs to the host
the operator chooses.

## Git coordination

Git is the coordination authority. Live workflow state is create-only evidence
under `refs/forest/v1/{request,checks,verdict}/<sha>`, plus `forest/*` branches
and `master`. GitHub Issues and Powder jobs are the Tracker. Pull requests are disposable
human Projections, never authority.

Builder and Fixer call `forest publish review-request`. The Kernel publishes
the branch and a request evidence commit. Verifier calls `forest publish verdict`.
The Kernel writes Checks and Verdict evidence refs and, on approve, fast-forwards
`master` in the same atomic push. It then reconciles the landed Subject to
terminal Powder state without changing a successful Gate into failure. See
[ADR 0021](docs/adr/0021-kernel-review-request-publication.md),
[ADR 0022](docs/adr/0022-kernel-verdict-publication.md), and
[ADR 0023](docs/adr/0023-powder-jobs-and-review-request-v2.md).

Which identity may create or update which ref is in
[onboarding](docs/onboarding-managed-repo.md#forge-identities-and-references).
A read-only forge credential breaks every declaration. Branch protection cannot
see evidence refs. Restrict `master` with a forge ruleset.

Agents own Subject selection, implementation, review judgment, and the initial
Powder take. The Kernel owns publication and bounded terminal Powder
reconciliation. See [managed-repository
onboarding](docs/onboarding-managed-repo.md) for the operator procedure.

## Ready subjects

A `forest:ready` Issue or takeable Powder job is a self-contained spec: problem,
repro, scope bound, machine-checkable acceptance criteria, and a verification
path. Grooming stays human-supervised, and the factory remains a dumb consumer
of ready Subjects.

- [`docs/forest-ready-contract.md`](docs/forest-ready-contract.md) — what ready
  means and how it is enforced by convention.
- [`docs/templates/powder-job-spec.md`](docs/templates/powder-job-spec.md) — the
  template for a ready Powder job spec.
- [`org-skills/grooming-checklist/SKILL.md`](org-skills/grooming-checklist/SKILL.md) —
  the supervised grooming checklist skill (not a factory declaration).


## Poll protocol

A Poll is a yes-or-no trigger. It passes no context; the agent selects its
Subject during the Run. Builder, Verifier, and Fixer each have a disjoint Poll
command. Exit 0 dispatches work, exit 1 is a healthy skip, and exit greater
than 1, timeout, or malformed behavior records an unhealthy trigger. See
[ADR 0012](docs/adr/0012-poll-trigger-protocol.md) and the
[onboarding guide](docs/onboarding-managed-repo.md) for selection rules.

Builder Poll also wakes on a takeable or held Powder job for `forest.yaml`
`repo` when `POWDER_AGENT` is set. Before dispatch it reconciles the current
Git-landed Subject to terminal Powder state or fails closed.

Verifier and Fixer Poll `ls-remote` evidence refs for each `forest/*` tip.
Historical notes are unread. A missing evidence ref is no work.

## Merge Gate

The Gate requires one valid request evidence ref, passing Checks, and an
approve Verdict for the same Revision, plus a fast-forward of `master` to that
Revision. `forest publish verdict` performs that push. The existing request is
not republished. The merge never uses force. Except for the trusted first
`master` baseline, the Auditor checks the observable final state after the
Effect; it does not enforce it. See [ADR 0010](docs/adr/0010-agent-owned-effects-and-merge-gate.md).

## Auditor and trust boundary

The Kernel Auditor is read-only. The first observed remote `master` tip becomes
a trusted baseline and is not Gate-checked. Each snapshot fetches
`refs/forest/v1/*` and checks schema, committer identity, and the Gate on the
final remote `master`. Leftover notes are unread. The Auditor cannot prove
check execution, atomic push ordering, or force absence.

An operator direct push whose tip has no factory evidence and whose commit
author is not a shipped declaration identity is acknowledged and is not a Gate
violation. Any tip that carries factory evidence, or whose author is a
declaration identity, still requires the full Gate. See
[ADR 0026](docs/adr/0026-human-direct-push-audit-policy.md).

The Auditor runs after a completed dispatch. It stores current violations in
`audit.json` and marks the last Audit as `violations` in `forest status`. It
appends violations to `audit.log` only when the current set differs from the
prior persisted set. A passing Audit clears current violations and adds no
history. The Auditor never blocks a merge. Startup and idle Poll skips do not
start an Audit.


`audit show` and `status` publish these audit keys:

| Key | Meaning |
| --- | --- |
| `baseline` | First observed remote `master`. It is not Gate-checked. |
| `last_master` | Last Audit that passed. Ancestry checks start from this tip. |
| `audited_master` | Remote `master` the last completed Audit observed, pass or not. |
| `last_at` | When that Audit finished. |
| `last_result` | `pass` or `violations`. Empty means no Audit has completed. |
| `violations` | Current violation set. Empty on a pass. |

Human `master=` is `audited_master`. It is not `last_master`. A violating Audit
names the tip it scanned.

Human Run rows put `exit=` and `duration=` first so they stay inside 80
columns when the Run identity is long. `--json` still carries the full
`run_id` and `agent`.


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
| `forest publish verdict <checks> <verdict>` | Publish Checks and Verdict evidence refs; approve also fast-forwards `master`. |

### Reading the factory

`serve`, `once`, and `poll` are the engine: they hold the Kernel lock and write.
`publish review-request` and `publish verdict` write without taking that lock, so a
Run that already holds it can publish. `run cancel` also writes without taking
the Kernel lock, because the live Run's Runner already holds it. Every other
row is the read surface. Each read-surface command accepts `--json` and
`--root <dir>`. `--json` emits one
`forest.cli.v2` envelope on stdout; human text stays on stderr. `--root`
answers from another checkout. `trigger reset` and `audit show --rescan` take
the Kernel lock and refuse while a Kernel runs. `publish review-request` and
`publish verdict` do not.


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

`status` also publishes `live_runs`: for every in-flight Run it reports
`run_id`, `agent`, `started_at`, `elapsed`, and `cancel`. The `started_at`
value is the UTC/RFC3339 timestamp the Runner recorded when it dispatched the
Run; `elapsed` is derived from that recorded timestamp. Cancel a live Run with
the published `cancel` command (`forest run cancel <run-id>`), which targets the
Run's primary process state. Do not judge a live Run from log file size or
mtime: the Runner owns the log and bounds its retained tail. When the live-run
records cannot be read, `status` publishes `live_run_error` next to an empty
`live_runs` list, so a machine reader learns the list is unknown rather than
truly empty.

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
LANGFUSE_PUBLIC_KEY=<optional Langfuse public key>
LANGFUSE_SECRET_KEY=<optional Langfuse secret key>
LANGFUSE_BASE_URL=<optional Langfuse host>
```

Existing environment values take precedence. `FOREST_EVAL_ENV_FILE` selects a
different file. The OpenRouter management key stays outside production and
evaluation runtime environments. Langfuse keys are optional; when present,
`run-model.sh` exports completed Harbor trials post-run with
`evals/scripts/langfuse_export.py` (see
[`docs/langfuse-dashboards.md`](docs/langfuse-dashboards.md)). Export is
fail-open and never changes a Harbor reward or job exit.

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
