# Onboarding a managed repository

Start with one explicit request, one repository owner, and one delivery authority.
Do not begin by creating readiness labels, importing a backlog, or enabling
schedules. This guide replaces the retired queue-first onboarding walkthrough.

## Choose the boundary

Exactly one live Kernel serves one repository on an operator-owned host. The
factory source may be the managed checkout itself, or a sibling that supplies
only the installed `forest` executable. A second clone is not a second worker:
keep one operational owner and one live Kernel for the repository.

Choose `delivery` before granting credentials:

| Mode | Publication authority | Adoption requirement |
| --- | --- | --- |
| `external` | Repository-owned profile plus its external operator workflow | The profile defines candidate review, human merge and tracker reconciliation. Kernel native publication is refused; its native audit is not applicable. |
| `git-native` | Kernel publication commands and the native Git Gate | The operator explicitly accepts native primary publication after the Gate. This is not the R90 human-merge contract. |

R90 uses Habitat and its repository-owned external profile. Misty Step uses
current operator requests; optional tracker references are attribution, not
permission to start work. Canopy only observes. It does not admit Runs, merge,
change configuration, or deploy.

## Prepare the host and profile

Install `git`, `pi`, the managed project's check tools, and the profile's
explicit `required_tools`. The factory build uses its pinned `mise` Go toolchain.
Do not install an unrelated tracker CLI to satisfy a legacy example.

Version the complete profile together:

```text
.iron-forest/
  config.yaml
  defaults.yaml
  secrets.yaml
  agents/<name>/agent.md
  agents/<name>/task.md
  extensions/                 # only explicitly declared reviewed files
  bin/forest                  # installed executable, ignored by Git
  runtime/                    # generated evidence, ignored by Git
```

For manual-only onboarding, each configured declaration must have an explicit
non-dispatching Poll, for example `poll: "exit 1"` with `interval: 300`.
`once --request` bypasses that Poll. Replace it with an authorized selector only
when the owner deliberately adopts scheduling; an empty queue is not an
access-control mechanism.

Use repository-owned declarations for the chosen delivery mode. Do not copy a
Git-native Verifier into an external profile: its publication authority differs.
A declaration may specify `model`, `tools`, `thinking`, `request`, `completion`
and explicit `extensions`. See the [declaration contract](../README.md#repository-owned-composition).

Keep credentials outside Git, prompts, defaults and declarations. For a local
user service the owner supplies `~/.config/iron-forest/<instance>.env`, owned by
that user and mode `0600`. Use provider completion credentials, not provider
management, evaluation or personal interactive credentials. Provider controls
own spending limits; the Ledger does not calculate USD.

Worktrees and fresh Pi directories are not a sandbox. A declaration runs with
the service user's filesystem, network and credential authority. Use separate
host/forge principals and provider-side restrictions when those capabilities
must be separate. See [isolation posture](adr/0016-isolation-posture.md).

## Validate without admitting work

Build the chosen revision into the target's `.iron-forest/bin/forest`. From the
factory checkout, for a sibling named `product`:

```sh
mise exec -- go build -o ../product/.iron-forest/bin/forest .
```

From the managed checkout:

```sh
./.iron-forest/bin/forest admission pause
./.iron-forest/bin/forest version --json
./.iron-forest/bin/forest config show --json
./.iron-forest/bin/forest declaration list --json
./.iron-forest/bin/forest declaration show builder --json
./.iron-forest/bin/forest selfcheck
```

These reads and selfcheck do not prove a model can complete a task. Before a
paid attempt, the host owner must also check the actual packaged Pi and declared
extension combination, available project tools and external permissions. A
successful local source check does not certify another image or installed binary.
An unversioned development build may report an unknown build SHA; it is not a
release identity.

For an explicitly approved local service installation, run
`deploy/install-service.sh <sibling-directory-name>` from the factory checkout
(or omit the name for self-host mode). The installer builds the target binary,
checks the profile and audit mode, and starts a new installation paused. It is
not a harmless setup command: it owns a service transition. R90 Fly installations
use their owning infrastructure procedures instead.

## Execute the first explicit request

Agree on the useful outcome, scope, forbidden effects, evidence and spending
authority before dispatch. Save a `forest.request.v1` file outside source with a
unique request ID, a self-contained prompt, and an optional immutable `work`
reference. Use the [request schema](../README.md#one-profile-explicit-requests-persistent-admission).
No ticket is required by Kernel. A tracker title or branch name is not an
immutable work association.

A foreground `once` requires exclusive Kernel ownership. Do not run it alongside
a scheduler. The owning operator stops an existing scheduler before choosing
this manual path. Then, from the managed checkout:

```sh
./.iron-forest/bin/forest admission resume
./.iron-forest/bin/forest once builder --request /path/to/approved-request.json
./.iron-forest/bin/forest admission pause
./.iron-forest/bin/forest run list --json
```

An explicit request bypasses Poll and scheduled selection, not admission or the
Kernel lock. It appends to the standing task; it does not replace role policy.
A Run may create branches and external effects according to the profile. Pausing
later does not undo an admitted Run or cancel it.

For native delivery, use the [v3 publication contract](../README.md#git-coordination).
Builder/Fixer evidence includes its actual live Run ID and optional retained
request/work association. Verifier and Fixer requests must preserve the complete
opaque work snapshot; they use their own Run/request identities. Never relabel
Habitat as GitHub/Powder. Both Verdict kinds require exact candidate evidence,
and native approval runs the scanner and candidate-configured Checks before
atomic publication. Greenfield profiles declare real required commands; absent
product/check implementation blocks publication rather than becoming a no-op.
External delivery profiles remain under their own completion/merge authority.

## Read the result, not just the exit code

Use the recorded Run ID with:

```sh
./.iron-forest/bin/forest run show <run-id> --json
./.iron-forest/bin/forest run logs <run-id> --json
./.iron-forest/bin/forest status --json
./.iron-forest/bin/forest admission show --json
```

Inspect three independent facts:

1. **Execution:** the recorded cause and raw process exit when known. A harness
   setup failure, cancellation or interruption is not a product-quality score.
2. **Completion:** when the profile declares a completion observer, its required
   external effect was observed, incomplete, or unavailable. A valid review
   requesting changes is a completed review. Missing completion evidence is not
   success; an unconfigured observer makes no completion claim.
3. **Delivery:** the chosen authority's exact-revision evidence. For R90 this
   includes review of the candidate, an operator merge, observed merge facts and
   tracker reconciliation. A merge is not a deployment.

Keep full logs and prompts behind the owning access boundary. Canopy displays
bounded read-only projections, preserves unknown/partial/stale states, and links
back to source evidence. It is not a second business ledger or permission system.

## Pause, drain and recover

`admission pause` durably blocks new dispatches and returns without waiting.
`admission drain` pauses and waits for admitted Runs without cancelling them.
`run cancel <run-id>` is an explicit cancellation. Resume is refused while a
drain is active; interrupted drain leaves admission paused.

After loss of a Kernel, startup records interrupted attribution and terminates
verified orphan Run groups. It does not resume model context or reconcile an
uncertain remote effect. Inspect the retained request, Run and external evidence
before authorizing another attempt. Never substitute a fresh paid Run for
checking whether a PR, receipt or tracker transition already happened.

An R90 merged-but-unreconciled item belongs to its operator completion command,
not another Verifier. Use the infrastructure owner's current README; do not
operate its worker through this checkout's local service.

## Adopt updates coherently

The owning factory checkout first adopts a clean merged revision. Its operator
uses `deploy/install-service.sh update <instance>` for self-host mode. A sibling
owner uses `deploy/install-service.sh update <instance> <factory-sha>` after the
factory owner has adopted that exact revision. The installer drains, rebuilds,
checks `build_sha`, rescans the appropriate audit and verifies the restarted unit.
A restart alone is not an update. Preserve the transaction's rollback evidence
when an update fails; do not copy an arbitrary older binary over new source.

## Second-engineer acceptance

The second engineer should be able to identify the owner and delivery mode,
validate the installed artifact, dispatch only the agreed request, follow its
exact revision and effect evidence, complete the human-owned step, and explain
how to pause or recover without an undocumented command from the original author.

Before expanding a pilot, compare a bounded sample with the existing supervised
agent/PR workflow. Record accepted outcomes, scoping/review/repair/operations
minutes, interventions, waiting versus execution time, provider receipt coverage,
infrastructure cost and escaped defects. Set scope and spend before running it.
A small sample is directional evidence, not a reliability or ROI guarantee.
