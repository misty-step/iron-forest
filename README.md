# Iron Forest

Iron Forest is a headless software factory. Exactly one live Kernel serves
one repository, on a machine the operator chooses. The Kernel handles
mechanics. Declarations state what agents think and do. This README is the
current operating entrypoint. Accepted ADRs state technical contracts.

Misty Step work starts from a current operator request. Automatic backlog
intake is retired. Old tickets, labels, and timers do not authorize work.
Direct requests use the ordinary session or PR workflow; no ticket is required.
R90 deployments continue to use Habitat. The legacy protocol and migration
records below describe implementation history, not permission to restart old
queue consumers.
Linear owns current non-R90 work, prioritization, and selected unresolved
opportunities. It is not automatic intake, and a ticket is not permission to
start a Run. Repositories retain versioned contracts, accepted ADRs, and
curated eval fixtures; Git evidence, the Ledger, and Run logs retain their
native runtime authority. Store raw or sensitive eval output in approved
retained artifact storage and link summaries rather than copying it into work
records.

The review roster has Builder, Verifier, and Fixer for explicitly requested
work. A Verifier checks one exact Revision, and a Fixer repairs a rejected
Revision. Critic and Tester perform requested read-only sweeps and return
findings with evidence; they do not create tickets or start implementation.
Their automatic intake polls are disabled.

## Quick start

Create the repository-owned profile at `.iron-forest/config.yaml`:

```yaml
repo: misty-step/iron-forest
delivery: git-native
required_tools: [gh, trufflehog]
agents:
  builder:  { poll: "exit 1", interval: 300 }
  verifier: { poll: "exit 1", interval: 120 }
  fixer:    { poll: "exit 1", interval: 300 }
checks:
  - name: build
    run: mise exec -- go build ./...
  - name: vet
    run: mise exec -- go vet ./...
  - name: test
    run: mise exec -- go test ./...
```

These explicit exit-1 Polls disable scheduled dispatch during manual onboarding.
Use `once --request` for approved work; it bypasses Poll, not admission or the
Kernel lock. Adopt an actual selector only through a deliberate repository-owned
profile change. This quick start does not install or start a service.

### One profile, explicit requests, persistent admission

There is one supported layout. Config, defaults, secret-scan policy, declarations,
and reviewed extensions are versioned together; credentials are not:

```text
.iron-forest/
  config.yaml
  defaults.yaml
  secrets.yaml
  agents/<name>/{agent.md,task.md}
  extensions/                 # optional explicitly declared files
  bin/forest                  # installed executable; ignored by Git
  runtime/                    # Ledger, live Runs, logs, worktrees, admission
```

Neither root-level configuration/agents nor `.forest` are runtime inputs.
`FOREST_DEFAULTS` does not override `.iron-forest/defaults.yaml`.

For direct work, save a `forest.request.v1` JSON file:

```json
{
  "schema": "forest.request.v1",
  "id": "unique-request-id",
  "prompt": "Implement only the attached operator request and report its evidence.",
  "work": {
    "system": "https://tracker.example",
    "id": "immutable-item-id",
    "key": "EX-9",
    "url": "https://tracker.example/items/immutable-item-id"
  }
}
```

`work` is optional. Its `system` and `id` are required when present; `key` and
`url` are optional display values. Kernel never resolves a tracker or infers an
association from a title, branch, or current queue. One Run serves at most one
primary work reference.

```sh
./.iron-forest/bin/forest admission show --json
./.iron-forest/bin/forest once builder --request /path/to/request.json
```

An explicit `once` appends `prompt` to the standing task and bypasses both Poll
and a scheduled request command. It still requires open admission and the
exclusive Kernel lock: stop an existing scheduler before a foreground `once`.
The request ID and work object survive preparation failure, Pi failure,
cancellation, and interrupted-Run recovery in live evidence and the Ledger.
The full request is retained as `runtime/runs/<run-id>.request.json`.

A declaration may instead set `request: <shell command>` in `agent.md`
frontmatter. After its configured Poll returns 0 and admission is reserved,
Kernel allocates the Run ID and executes that command in the repository root
with `FOREST_ROOT` and `FOREST_RUN_ID`. stdout must be one `forest.request.v1`
object; stderr goes to the Run log. The command has the Poll command's 65-second
bound. A clean exit 1 means selection raced to no work: no Pi process starts,
`once` exits 1, and the reserved selection receipt is retained with `no_work: true`
without becoming a successful or failed model Run in aggregate status. Other
command errors, malformed output, timeouts, and cleanup failures are failed Runs,
not hidden selection of another item. The profile, not Kernel, owns any external
claim/link command and its tracker API.

```sh
./.iron-forest/bin/forest admission pause
./.iron-forest/bin/forest admission drain
./.iron-forest/bin/forest admission resume
```

Pause is instance-scoped, durable, and blocks scheduled and manual dispatch.
It does not wait for active Runs, so a Run's own failure handler may safely pause
admission before exiting. Only drain waits for the Run lifetime leases.
Drain first pauses admission, then waits for already admitted Runs without
cancelling them. It fences a Poll completing concurrently with pause and a Run
reserved before its subprocess starts. Resume is refused while a drain command
is active. Interrupting drain leaves the instance paused. The JSON read surface
reports `paused`, `active_runs`, `active_count`, and `drained`; orphaned records
without live subprocesses are reported separately as `interrupted_runs`.
Drain does not promise to resume Pi sessions. A new Kernel fences and terminates
verified orphan Run groups, records interrupted attribution once, and then
cleans their worktrees.

Choose one delivery authority in `config.yaml`: `delivery: git-native` (the
default) retains native publication and Gate auditing; `delivery: external`
refuses native publication and reports audit `last_result: not_applicable`
with an explicit reason. External delivery is observed independently; it is
never reported as a fabricated native pass or violation. External profiles may
omit native `checks`. `required_tools` names additional profile dependencies;
selfcheck always requires only the Kernel's `git` and `pi`, plus those explicitly
declared tools.

### Execution, completion, and delivery

A Run records independent facts:

- `exit` remains the terminal Run status for existing CLI consumers.
- `process_exit`, when present, is the raw harness process exit; it does not
  change because usage parsing, cleanup, or completion observation failed.
- `outcome` records a known execution cause: `completed`, `no_work`,
  `setup_failed`, `execution_failed`, `provider_failed`, `cancelled`, `timed_out`,
  `interrupted`, or `internal_error`. Missing legacy fields mean unknown.
- `completion`, when configured, records the profile's observation of the
  requested external effect. It is not a delivery or correctness verdict.

An optional declaration frontmatter `completion: <shell command>` runs once
after a harness attempt, before worktree cleanup, in the owning repository root.
It receives `FOREST_ROOT`, `FOREST_RUN_ID`, and one JSON object on stdin:

```json
{"schema":"forest.completion-context.v1","run":{"run_id":"…"},"request":null}
```

`run` carries the Run facts available at that point; `request` is the retained
request object when supplied. The observer must return exactly one bounded
`forest.completion.v1` object. `status` is `completed`, `incomplete`, or
`unknown`; `completed` requires a nonempty `evidence` reference, and other
statuses require a `reason`. Execution uses the same bound as a request command.
Malformed output, failed observation, and unavailability remain unknown, never
an inferred pass. An unconfigured observer makes no completion claim.

The observer belongs to the profile: it reads the authoritative receipt, not an
agent's assertion of success. A Verifier's valid `changes` receipt is a completed
review; exit zero without the required receipt is not. The profile owns the
response to an incomplete effect, including admission pause when another paid
attempt would duplicate or obscure it. Kernel does not retry the observer,
merge a PR, or reconcile a tracker.

Keep completion observers and their dependencies versioned with the profile.
Their stdin and stderr can contain private request context; ordinary observers
should publish only bounded reasons and evidence references. Host credentials
and external permission boundaries still apply.

### Repository-owned composition

`.iron-forest/config.yaml` accepts arbitrary declaration names. The roster above is the
shipped opinionated profile, not a Kernel enum or a required workflow. Each
managed repository may supply its own Polls, prompts, model, thinking level,
Pi tool allowlists, shared skills, role skills, and Checks. One Kernel still
serves exactly one repository; an external manager coordinates several
instances through their CLI read surfaces.

Start each declaration with Pi's smallest useful tool set. A role can use an
installed CLI such as `gh` or a browser driver only when `bash` is
in that declaration's Pi tool allowlist and an explicit skill defines the CLI
contract. Pi extension discovery stays disabled. A declaration can opt into
reviewed, versioned files with frontmatter such as
`extensions: [.iron-forest/extensions/usage.ts]`. Paths must be normalized
repository-relative files under `.iron-forest`, outside `runtime` and `bin`;
symlinks are refused. Declaration and Run evidence expose paths and SHA-256
digests. The bytes in the fetched Run worktree must match before Pi starts.

Legacy tracker validation and reconciliation remain in Kernel code and old
evaluation fixtures. They are not active work-selection instructions. A profile
change alone does not remove those historical implementation paths.

Agent Runs have no wall-clock deadline by default. To bound a declaration that
has wedged before, set the optional `max_duration` key (seconds) under that
agent; `0` or an omitted key leaves the Run unbounded:

```yaml
agents:
  fixer: { poll: "./.iron-forest/bin/forest poll fixer", interval: 300, max_duration: 3600 }
```

When `max_duration` is set, the Kernel's elapsed-time watchdog cancels a Run that
exceeds the bound, records `timed_out` rather than operator cancellation, and
returns the trigger to a clean not-running state.

Declare each agent with two prompt files. Skills live only in an existing
shared directory and, when a role needs private skills, its own directory:

```text
.iron-forest/agents/<name>/agent.md
.iron-forest/agents/<name>/task.md
.iron-forest/agents/_shared/skills/          # optional; every declaration
.iron-forest/agents/<name>/skills/           # optional; this declaration only
```

Operator-supervised (org) skills live under `org-skills/`. They are not factory
skill sources; the Runner never auto-loads them, and an operator passes one to a
supervised `pi` session explicitly.

Iron Forest ships the operator-facing
[`iron-forest` skill](org-skills/iron-forest/SKILL.md). Pass that directory
explicitly to a supervised Pi or company-agent session that configures,
operates, or observes one or more repository instances.

`agent.md` uses YAML frontmatter with optional `model`, `tools`, `thinking`,
`request`, `completion`, and `extensions`, followed by the system prompt.
`task.md` is the standing user prompt. `model` and `thinking` resolve through the declaration,
then `.iron-forest/defaults.yaml`, then — for `model` only — the built-in
`openrouter/deepseek/deepseek-v4-pro-0813`. An empty or comment-only defaults
file is the zero Defaults, not an error. `forest declaration show` publishes
the resolved model and its source.

Every Run gives Pi a new writable agent directory under `.iron-forest/runtime`
through `PI_CODING_AGENT_DIR`; no operator Pi state is inherited. For an OpenRouter
model, the Runner writes only a credential-free `models.json` override that
enables Pi's OpenRouter session-affinity header. Pi extension, skill,
prompt-template, and theme discovery are disabled with `--no-extensions`,
`--no-skills`, `--no-prompt-templates`, and `--no-themes`. The Runner passes
each existing skill source directory with an explicit `--skill` and each
declared extension file with `--extension`. Paths resolve from the Run worktree.
Declaration and Run evidence publish resolved resources and request provenance.

Pi's exact session ID is the Run ID. The generated OpenRouter model override
makes Pi send it as `x-session-id`, so Broadcast destinations can group every
model request for a Run and correlate the provider trace directly with the
Ledger and `.iron-forest/runtime/runs/<run-id>.log`.

Credentials come only from the service environment inherited by the Run.
Declaration frontmatter has no `env` field; unknown metadata fails validation.
Credentials do not belong in prompts, skills, defaults, or commits. Role
engineering, selection, and publication rules live in each `agent.md`.
Factory Runs receive only existing shared or role skill directories; unused
generic packages are omitted rather than left as empty layers.

This quick start uses self-host mode: the factory source checkout is also the
managed repository. For a separate sibling managed checkout, use the
[onboarding guide](docs/onboarding-managed-repo.md); its installer builds the
Kernel from the factory source into that sibling.

### Second-party deployment checklist

Use the [onboarding guide](docs/onboarding-managed-repo.md). Before handing a
repository to another operator:

1. Name its operational owner, exact checkout, host and delivery authority.
   External human-merge profiles must not inherit native publication grants.
2. Validate the repository-owned profile, actual packaged Pi/extensions and
   installed artifact identity without admitting paid work.
3. Provision scoped worker credentials separately from human completion and
   deployment authority. A prompt or Git author name is not a permission boundary.
4. Start exactly one Kernel through the owning deployment procedure, paused
   until the owner accepts its evidence. Do not enable historical queue or eval
   intake as an incidental setup step.
5. Exercise one approved request and inspect execution, required completion and
   delivery independently. Verify read-only observation and the recovery path.
6. Record the deployed identity in the owner's inventory and return the exact
   verification evidence. Source tests or a restart do not certify another binary.

`selfcheck` validates local config and declarations. It does not make a model
call or prove external completion. The native Auditor runs after completed
dispatches, not because a Kernel started or an idle Poll returned no work.

### Adopting merged revisions

Adopt merged revisions with the fenced update procedure:

    deploy/install-service.sh update <instance>                  # self-host factory checkout
    deploy/install-service.sh update <instance> <factory-sha>    # sibling managed checkout

The script snapshots the current binary and profile into a unique transaction
before touching the service. It never reads an unrelated `forest.prev`. It
pauses/drains a current installation (or drains the old service during explicit
adoption), stops it, acquires its Kernel lock, fast-forwards source, builds the
profile-local executable, verifies `build_sha`, and runs selfcheck and a fresh
audit. External delivery returns an explicit not-applicable audit. It refreshes
the service executable path, restarts paused, verifies the receipt, and only
then restores previously open admission. A fresh installation stays paused.
Failure restores this transaction's source, binary, profile and unit; historical
Run evidence is never replaced by a stale snapshot.
For a sibling managed checkout, pass the exact factory Revision already adopted
by the factory owner; the script verifies the factory checkout is clean and
exactly at that Revision before it stops the consumer unit, then builds that
Revision into the sibling. It never mutates the factory checkout. Never
restart-only: the unit runs the checkout-local binary.

For legacy adoption, the fetched revision must already move versioned config,
policy, agent paths and executable callers to `.iron-forest`. The installer
alone may move `.forest` to `.iron-forest/runtime`, preserving the Ledger and
Run artifacts, and adopt a root-level defaults file when there is no competing
profile defaults file. Competing old/new runtime or defaults directories fail
closed rather than merge evidence. Review/remove obsolete credential-bearing
legacy Pi profiles before adoption; they are not valid declaration inputs.

Before `serve` or `once` loads trigger health, the Scheduler performs reserved
garbage collection under the Kernel lock. One 30-second deadline bounds the
total operation. It removes reserved `.iron-forest/runtime/worktrees/<run-id>` paths through
Runner cleanup and prunes their registry entries. One `update-ref` transaction
removes private Runner, Poll, and Audit refs. It removes only known stale
`audit.json`, `audit.log`, and `triggers.json` temps. The Ledger owns Ledger
temps. Run log retention owns Run logs. Any cleanup error blocks startup.
Reserved garbage collection never resumes a Run.

Start the Kernel:

```sh
./.iron-forest/bin/forest serve
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
`%h/.config/iron-forest/%i.env`. It does not set `PI_CODING_AGENT_DIR`; the
Runner owns that variable for each Run. The installer runs selfcheck with the
equivalent `$HOME`-expanded trusted PATH. Defaults come only from the profile.

Protect the environment file as mode `0600`. The Runner selects the OpenRouter
completion key for each Run from the instance environment:

```dotenv
OPENROUTER_API_KEY=<instance fallback key>
OPENROUTER_API_KEY_BUILDER=<builder key>
OPENROUTER_API_KEY_VERIFIER=<verifier key>
OPENROUTER_API_KEY_FIXER=<fixer key>
```

`OPENROUTER_API_KEY_<ROLE>` wins for the declaration whose name matches
`<ROLE>` (uppercased); the instance-wide `OPENROUTER_API_KEY` is the fallback
for any role without a dedicated key.

Do not configure retired queue credentials or enable automatic backlog
selection. Supply current work through an explicit operator handoff.

Do not put an OpenRouter management key in this file. Do not reuse a personal
interactive key or an evaluation key. The intended production layout uses one
completion key per agent role for OpenRouter and Langfuse cost, latency, and
failure attribution. It is not a security boundary: trusted Runs still share
the service user and can read the per-instance environment file.

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
and `master`. This protocol is retained for compatible requests explicitly
supplied by the operator. A historical queue item does not authorize a Run.

Builder and Fixer call `forest publish review-request`. The Kernel publishes
the branch and a request evidence commit. Verifier calls `forest publish verdict`.
The Kernel writes Checks and Verdict evidence refs and, on approve, fast-forwards
the configured primary in the same atomic push. New requests use
`forest.review-request.v3`; historical v1/v2 evidence remains readable and is
never rewritten. Only pending v2 Powder requests retain their legacy
reconciliation behavior. Generic v3 requests never invoke GitHub/Powder work
mutation; their profile completion observer owns work-system effects. See
[ADR 0021](docs/adr/0021-kernel-review-request-publication.md),
[ADR 0022](docs/adr/0022-kernel-verdict-publication.md), and
[ADR 0023](docs/adr/0023-powder-jobs-and-review-request-v2.md).

The current review-request payload is:

```json
{
  "schema": "forest.review-request.v3",
  "subject": "selected-work",
  "branch": "forest/selected-work/implementation",
  "revision": "<full candidate SHA>",
  "time": "<RFC3339 timestamp>",
  "run_id": "<actual builder or fixer FOREST_RUN_ID>",
  "request_id": "<actual Run request id>",
  "work": {
    "system": "<opaque system identifier>",
    "id": "<immutable work identifier>",
    "key": "<optional display key>",
    "url": "<optional work URL>"
  }
}
```

`subject` is a branch-routing identity, not a tracker enum. `run_id` must name
the actual live Builder/Fixer owner. `request_id` must match that Run's retained
request and must be omitted when absent. `work` must exactly equal the complete
persisted WorkReference snapshot, including optional display fields, or be
omitted when absent. An explicit request without work is valid. No `tracker`
member is accepted. Unknown or duplicate fields are rejected.

The Verifier has its own Run/request IDs but must review the same complete work
snapshot. Fixer preserves the rejected Subject, branch and work, requires its
exact `changes` verdict, and stamps its own actual Run/request IDs onto the
fresh revision. Old request and verdict refs remain unchanged.

Which identity may create or update which ref is in
[onboarding](docs/onboarding-managed-repo.md#forge-identities-and-references).
A read-only forge credential breaks every declaration. Branch protection cannot
see evidence refs. Restrict `master` with a forge ruleset.

The operator selects current work. Agents own implementation and review
judgment; the Kernel owns the retained publication protocol. The
[managed-repository guide](docs/onboarding-managed-repo.md) is a historical
setup reference, not an instruction to restart retired intake.

## Current requests

Clarify the selected request with a problem or scenario, scope, observable
acceptance criteria, and a verification path. Use
[`org-skills/grooming-checklist/SKILL.md`](org-skills/grooming-checklist/SKILL.md)
when that brief is useful. No queue entry or readiness label is required.

If the request needs durable work tracking, use Linear for non-R90 work.
Capture only selected opportunities with source pointers and proposal status;
do not import historical queues or create issues automatically.

The former readiness contract and job template remain historical records. They
do not govern new Misty Step work.

## Poll protocol

A Poll is a yes-or-no trigger. It passes no context; the agent selects its
Subject during the Run. Builder, Verifier, and Fixer each have a disjoint Poll
command. Exit 0 dispatches work, exit 1 is a healthy skip, and exit greater
than 1, timeout, or malformed behavior records an unhealthy trigger. See
[ADR 0012](docs/adr/0012-poll-trigger-protocol.md) and the
[onboarding guide](docs/onboarding-managed-repo.md) for selection rules.

The old Builder tracker path remains in the binary for historical compatibility.
Do not configure or restart it as a source of new work.

Verifier and Fixer Poll `ls-remote` evidence refs for each `forest/*` tip.
Historical notes are unread. A missing evidence ref is no work.

## Merge Gate

All publication roles require the Runner's `FOREST_RUN_ID` to match a valid live
role owner in the primary checkout and, when present, its retained request.
A linked worktree resolves to that owner; `FOREST_ROOT` cannot redirect the
check. Missing, ended, finalizing or cancelled context is refused even for an
identical retry. Ownership and complete request/work association are rechecked
after candidate Checks and before publication. This is an operational guard,
not security containment between processes running as the same user.

The Gate requires one valid request evidence ref, passing Checks, and an
approve Verdict for the same Revision, plus a fast-forward of `master` to that
Revision. Before `forest publish verdict` runs the configured Checks, it
validates the Builder or Fixer request, confirms the request branch still
points to the Revision, requires every submitted result to pass, and requires
the submitted names to equal the `.iron-forest/config.yaml` Check names at that Revision,
in the same order.
The credential scan is a Kernel-owned preflight (`forest scan-secrets` against
the detached candidate worktree, resolved from the running Kernel binary and
the external `trufflehog` outside the managed checkout). It runs unconditionally
before configured Checks and never compiles or executes candidate code, so a
candidate cannot supply the Gate's credential scanner. The atomic push
publishes Checks and Verdict, fast-forwards primary, and includes the validated
request OID and candidate branch as no-op leased refspecs. Both Verdict kinds
require the exact request and work; moved candidates are refused after Checks.
The request content is not replaced, and the primary branch is never forced.
Greenfield checks fail closed: the profile declares required commands, and
publication remains blocked until the candidate actually implements them.
Candidate configuration must retain native delivery and nonempty Checks.
Except for the trusted first
`master` baseline, the Auditor checks the observable final state after the
Effect; it remains the observer rather than the Gate owner. See
[ADR 0010](docs/adr/0010-agent-owned-effects-and-merge-gate.md).

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
| `forest status` | Show Poll, Run, and Audit errors, live Runs, the last audit result, recent Runs, and Ledger aggregates. |
| `forest selfcheck` | Validate `.iron-forest/config.yaml` and declarations locally. |
| `forest config show` | Print the loaded configuration. |
| `forest declaration list\|show <name>` | Print declaration names, or one declaration in full. |
| `forest trigger list\|show <agent>` | Print resolved trigger state. |
| `forest trigger reset <agent>` | Clear one agent's accumulated errors, including provider-budget fail-closed (`run_error=provider budget exhausted`). Refuses while a Kernel runs; resume is stop Kernel, reset, start. |
| `forest doctor` | Run non-mutating machine-operability checks and report one explicit result per check. |
| `forest run list` | Page the Ledger, newest first, optionally filtered by agent, exit code, or start time. |
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

`run list` filters the Ledger before paging with `--agent <agent>`,
`--exit <code>`, and `--since <rfc3339>`. `--since` matches Runs whose recorded
start time is at or after the timestamp. A filter and `--after` compose: the
cursor stays inside the filtered sequence, so a client can walk only the rows it
asked for.

`run list` returns Runs newest first. `status` reports at most ten recent Runs in
Ledger order, oldest first, because it is a snapshot of the tail rather than a
pager; its human output labels the order.

`status` also publishes `ledger`, a roll-up of the whole Ledger for one-command
instance health: overall `runs` and `pass_rate`; one entry per agent with
`runs`, `pass_rate`, `duration_p50`, `duration_p95`, and the five retained token
classes (`tokens_in`, `tokens_out`, `cache_read`, `cache_write`, `reasoning`);
and `recent_failures`, the newest nonzero rows with `run_id`, `agent`, `exit`,
and any recorded `error`. The retained `pass_rate` key is the fraction of
exit-zero Runs, not review quality, observed completion or delivery. Human output
names this execution exit-zero rate. Token classes are observability, not
accounting: no cost, price, spend, or currency value is ever computed.

`doctor` checks one checkout without mutating it. Each check reports a result
verb — `observed` for a local presence or mode read, `evidenced` for a
read-only external probe, or `unknown` when no answer was obtained — plus
`ok`, and either `evidence` or a `reason`. It checks `mise`, `go`, and `pi` on
PATH; `gh auth status`; the instance credential file mode (`0600`); read-only
forge push capability through `gh api`; the OpenRouter key with a read-only key
probe. Legacy tracker reachability checks remain in code. The forge and key
probes never write remotely, and evidence/reason never contain credential
values. Exit is `0` when every check is healthy and `2` otherwise.

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

Run the deterministic production-protocol checks before changing an agent
declaration, prompt, skill, or publication contract:

```sh
./evals/run-fast.sh
```
This command runs the Python checks, builds the pinned evaluation image, runs
every Harbor oracle case, and exercises an explicit-request delivery journey.
The oracle uses the real Pi process, `forest once`, and publication CLI, with a
trusted input hook supplying deterministic actions instead of a model. It
removes candidate/Judge credentials; a pass is **not agent-quality evidence**.

The journey covers implementation, rejection, repair, `forest run cancel`,
fresh-Run review, delivery, and an identical publication retry. Its standalone
container uses Docker `--init` so cancellation can reap orphaned descendants.
Inspect `report.json`, `report.md`, and `journey.json` under the new
`evals/jobs/fast/<job>/` directory. Reports identify oracle/model/unknown/mixed
execution; model, prompt, and skill promotion cannot use oracle or unknown
provenance. See [evaluation strategy](docs/evaluation-strategy.md) for the
coverage boundary and preserved historical evidence.

Separately authorized live-model experiments run the production incumbent and one allowlisted
contender over identical frozen cases. `evals/run-experiment.sh` accepts
`FOREST_EVAL_TIER=nightly|weekly|monthly|manual` and an optional
`FOREST_EVAL_VARIANT` from `evals/experiment-space.json`. With no variant, a
bounded planner selects a unique contender from historical results. The
independent Judge defaults to `openrouter/google/gemini-3.7-flash`.

Live-model runs load separate candidate and Judge completion keys from
`$HOME/.config/iron-forest/evals.env` by default. The file must be owned by the
current user, have mode `0600`, and contain only:

```dotenv
OPENROUTER_API_KEY=<evaluation candidate key>
FOREST_EVAL_JUDGE_API_KEY=<evaluation Judge key>
LANGFUSE_PUBLIC_KEY=<Langfuse public key>
LANGFUSE_SECRET_KEY=<Langfuse secret key>
LANGFUSE_BASE_URL=<Langfuse host>
```

Existing environment values take precedence. `FOREST_EVAL_ENV_FILE` selects a
different file. The OpenRouter management key stays outside production and
evaluation runtime environments. Scheduled and paired runs require Langfuse
history so they can reject duplicate contender fingerprints. Export remains
fail-open after execution and never changes a Harbor reward or job exit.
`evals/scripts/experiment_history.py` writes longitudinal quality, latency,
token, and cost summaries from the Langfuse catalog.

```sh
FOREST_EVAL_TIER=nightly FOREST_EVAL_VARIANT=qwen-3.7-high ./evals/run-experiment.sh
./evals/run-model.sh # paired monthly pass^3 certification
```

The `ci` workflow runs the same deterministic production-protocol command
(`./evals/run-fast.sh`) on every pull request. The `model evals` workflow runs
a rotating nightly tier Tuesday through Saturday, a weekly full `pass@1` tier,
and a monthly full `pass^3` tier. Manual dispatch preserves tier and
allowlisted contender controls. It maps separate candidate, Judge, and
Langfuse credentials into the runtime. Harbor and history artifacts remain
under `evals/jobs/`, which is ignored by Git.

The Ledger is `.iron-forest/runtime/runs.jsonl`. Each row records Run identity (`run_id` and
`agent`), timing (`started` and `duration`), `exit`, and exactly five retained
token classes — `tokens_in`, `tokens_out`, `cache_read`, `cache_write`, and
`reasoning` — as operational observability, not accounting. The Ledger never
records a cost, price, spend, or currency field and never computes money.

## Historical promotion evidence

Historical promotion evidence remains in
`evals/jobs/fast/fast-20260901T224519Z/report.md` and settled Runs
`1788301018846047029-critic` and `1788301018844450077-tester`. Those results
predate the retirement of automatic draft intake.

The local ignored report path is an original locator, not proof of shared or
durable artifact retention. Preserve the receipt and Run identities; when
citing them outside this checkout, use an approved retained artifact locator
with the relevant revision or digest. Do not treat the old promotion result as
current readiness or permission to restart draft intake.

## License

MIT. See [LICENSE](LICENSE).
