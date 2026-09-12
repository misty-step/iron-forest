# Local Linear factory

Operate one explicitly selected Linear delivery through a repository-owned
profile. This is an opt-in local workflow, not permission to restart retired
backlog consumers. A current operator request, committed profile, and deliberate
admission resume are required; an old label or ticket alone authorizes nothing.
The shipped Iron Forest profile keeps automatic intake disabled.

Use [managed-repository onboarding](onboarding-managed-repo.md) for host and
profile adoption, and the [Iron Forest skill](../org-skills/iron-forest/SKILL.md)
for the standard read surfaces. This runbook adds the local Linear adapter and
the human-owned completion step. It does not change external delivery profiles.

## Bootstrap one repository

1. Create the forge repository and local Git checkout, with `origin`, a committed
   primary baseline, and the correct remote default branch. Keep the managed
   checkout beside the Iron Forest source checkout for the local installer.
   Name one operator and one service instance; run exactly one Kernel.
2. Commit the product's build/check commands and complete profile before starting
   a service. An empty product must first acquire real required checks; missing
   implementation is a publication blocker, not a reason to install no-op checks.
3. Start from the [committed Poppycock reference profile](https://github.com/misty-step/poppycock/tree/1bd8293e10c950e0c1644016ec043f20668e09b5/.iron-forest),
   not an operator's uncommitted files. Adapt its `linear.py`, Builder, Verifier,
   and Fixer declarations and standing tasks together. Set the new `repo` and
   `primary`, add its exact repository-to-Linear-project entry to `PROJECTS` in
   `linear.py`, and set `LINEAR_SYSTEM` to the owning workspace. The reference
   adapter recognizes only its declared projects; a new repository is not
   discovered automatically. Preserve its review-only publication and PR steps.
4. Review defaults, model availability, tool access, skills, extensions and
   scanner policy for this product. Do not inherit another product's check
   command or silently grant its delivery authority. Commit the adopted profile
   and ignore only its generated `bin/` and `runtime/` directories.

```text
.iron-forest/
  config.yaml                 # repo, primary, delivery, Polls, checks, required_tools
  defaults.yaml               # credential-free instance model defaults
  secrets.yaml                # narrow credential-scanner exclusions, not credentials
  linear.py                   # reviewed, repository-owned adapter
  agents/<name>/agent.md      # role policy and request command
  agents/<name>/task.md       # standing task
  agents/_shared/skills/      # optional shared skills
  agents/<name>/skills/       # optional role skills
  extensions/                # only files explicitly declared by the roles
  bin/forest                 # installed executable; ignored
  runtime/                   # persistent instance evidence; ignored
```

For the native-evidence, human-merge path below, select `delivery: git-native`
and dispatch with `authority: review`. `delivery: external` refuses the native
publication commands; it is not an interchangeable way to obtain this workflow.
Put the actual project commands in nonempty `checks:`. Declare `python3`, `gh`,
`trufflehog`, and the product's check tools in `required_tools`; Kernel also
requires `git` and `pi`. Installer dependencies include `mise`, `jq`, `flock`,
`tar` and a user systemd service. Follow the
[profile preparation guide](onboarding-managed-repo.md#prepare-the-host-and-profile)
for credential and isolation boundaries.

Keep each Poll at `"exit 1"` during initial manual onboarding. When the owner
explicitly adopts this local workflow, version this wiring in `config.yaml`:

```yaml
agents:
  builder:  { poll: "python3 .iron-forest/linear.py poll builder", interval: 300 }
  verifier: { poll: "python3 .iron-forest/linear.py poll verifier", interval: 120 }
  fixer:    { poll: "python3 .iron-forest/linear.py poll fixer", interval: 300 }
```

Each corresponding `agents/<name>/agent.md` frontmatter must also declare
`request: python3 .iron-forest/linear.py request <name>`, using the actual role
name, alongside its reviewed tools, model/extension choices and policy. Keep
other roles disabled unless separately authorized. Copying only the Poll does
not supply a work request or the required publication policy.

## Linear adapter contract

The [reference adapter](https://github.com/misty-step/poppycock/blob/1bd8293e10c950e0c1644016ec043f20668e09b5/.iron-forest/linear.py)
is profile code, not a Kernel tracker integration. Its Builder selects an
unarchived issue in the configured project, in an open state (`triage`,
`backlog`, `unstarted`, or `started`), with an authority label and without
`Operator Action`.

| Label | Request authority |
| --- | --- |
| `Agent: Land` | `land` |
| `Agent: Review` | `review` |
| `Agent: Ready` (legacy) | `review` |
| `Operator Action` | Excluded, even with an authority label |

Review wins ambiguous authority labels. Use `Agent: Review` for a human merge;
`Agent: Land` instead permits Kernel landing where the profile allows it. Neither
a label nor `land` can override the delivery ceiling. The binding and precedence
rules live in [ADR 0029](adr/0029-per-work-item-authority.md).

`poll <role>` is a trigger: exit 0 means work, 1 means no work, and errors are
unhealthy rather than permission to choose a different item. After admission,
Kernel invokes `request <role>` with `FOREST_ROOT` and its allocated
`FOREST_RUN_ID`. stdout is one `forest.request.v1` object; diagnostics belong on
stderr. The Builder request retains the ticket title/body/acceptance as task
data, authority, and immutable `work` identity with display key and URL.
Verifier and Fixer requests retain the candidate's work snapshot and authority,
but receive their own Run/request IDs. See the
[request lifecycle](../README.md#one-profile-explicit-requests-persistent-admission)
for no-work races and command failures; do not invoke a live request command by
inventing a Run ID.

**Allow only one delivery in flight.** Builder Poll and request independently
read the eligible list and select `issues[0]`. The adapter does not claim the
ticket, deduplicate admitted Builder work, promise priority ordering, or reject
multiple eligible tickets. Its query reads at most 50 results, not a paginated
backlog. Label exactly one current ticket; never use that order as scheduling
policy. Once the Builder's retained request identifies the intended work, remove
its intake labels so a later Builder tick cannot select it again. That does not
alter the already-retained authority or candidate evidence.

Verifier and Fixer do not select fresh tickets. They refresh published candidate
refs and require exactly one pending candidate (no verdict for Verifier, a
`changes` verdict for Fixer), then consult the native evidence Poll. Zero or
multiple matches skip dispatch. Old unresolved evidence can therefore block the
next delivery even with only one labelled ticket. Inspect it and resolve its
ownership before resuming; do not delete immutable evidence to unblock a queue.

## Credentials and receipts

The adapter looks in the service environment first, then `~/.secrets`, for these
property names in order: `LINEAR_API_KEY`, `LINEAR_API_TOKEN`, `LINEAR_KEY`,
`LINEAR_PERSONAL_API_KEY`. It accepts simple `NAME=value` properties with optional
matching quotes; it does not source shell code. Do not print, commit or paste
credential values into a ticket or receipt.

Provider completion credentials such as `OPENROUTER_API_KEY` belong in the
protected service environment, `~/.config/iron-forest/<instance>.env`, owned by
the operator and mode `0600`. The installer does not source `~/.secrets` for Pi.
Provide scoped forge authentication for branch/evidence publication and PR
creation, separately from the human's merge authority. Do not give workers
provider management credentials. Follow onboarding's
[credential boundary](onboarding-managed-repo.md#prepare-the-host-and-profile);
worktrees are not a security sandbox.

Local operator captures and adoption/verification receipts live under
`~/.config/misty-step/`; choose descriptive instance/date filenames and retain
only credential-free summaries and source references. These files are operator
captures, not an automatically maintained Kernel ledger. The instance's native
requests, Run logs and Ledger remain under `.iron-forest/runtime/`. Published
candidate/Checks/Verdict refs are authoritative, as specified by
[ADR 0028](adr/0028-review-request-notes-retired.md). A PR comment, local receipt
or Canopy card cannot replace missing published evidence.

## Install and adopt a committed revision

Commit and merge the profile first. Require clean factory and managed working
trees and an identified factory commit before installation or update; do not
build an operational binary from edits awaiting commit. The installer enforces
clean target trees on update and a clean, exact factory revision for sibling
updates. Initial installation can build a dirty source tree, so the operator
must enforce the clean-tree discipline there too.

From the factory source checkout, install a new sibling instance:

```sh
deploy/install-service.sh <instance>
```

Omit the instance only for a new self-host installation. There is no `install`
subcommand. This builds the binary, validates the profile and audit, and starts
the user service paused; it does not merely write configuration. Do not run it
against a live instance as a substitute for the fenced update procedure.

For an existing installation, from the factory source checkout:

```sh
deploy/install-service.sh update <instance> <factory-sha>  # sibling
# Self-host instead: deploy/install-service.sh update <instance>
```

The factory owner must already have adopted that exact merged revision before
a sibling update. Updates drain, rebuild and verify before restoring previously
open admission. Pause explicitly first when the instance must stay paused. See
[adopt updates coherently](onboarding-managed-repo.md#adopt-updates-coherently)
for the transaction and rollback boundary. A restart alone never adopts source.

From the managed checkout, before any resume:

```sh
./.iron-forest/bin/forest version --json
./.iron-forest/bin/forest config show --json
./.iron-forest/bin/forest declaration list --json
./.iron-forest/bin/forest declaration show builder --json
./.iron-forest/bin/forest declaration show verifier --json
./.iron-forest/bin/forest declaration show fixer --json
./.iron-forest/bin/forest selfcheck
./.iron-forest/bin/forest admission show --json
```

Require `data.build_sha` to equal the chosen factory commit and `data.dirty` to
be `false`; an unknown SHA or dirty build is not accepted deployment identity.
Check the actual loaded Poll/request wiring, authority ceiling, Checks and
paused state. Record the factory SHA, managed profile revision, version output
and validation result in the operator receipt. Selfcheck does not prove model
access or an end-to-end delivery; the supervised attempt below supplies that
separate evidence.

## Deliver one ticket and read the result

Use a concrete, current ticket with scope, acceptance criteria, forbidden effects
and spending authority agreed by its operator. This example chooses review-only
delivery; all CLI commands below run in the managed checkout.

1. **Label one ticket.** While admission is paused, inspect `forest review list`
   and resolve previous pending ownership. Ensure no other ticket in this
   project's intake is eligible. Apply only `Agent: Review` to the selected open
   ticket, without `Operator Action`. Do not bulk-label a backlog.
2. **Resume and watch the Run.** The installed scheduler owns the Kernel; do not
   start `once` beside it.

   ```sh
   ./.iron-forest/bin/forest admission resume
   ./.iron-forest/bin/forest run list --json
   ./.iron-forest/bin/forest run show <builder-run-id> --json
   ./.iron-forest/bin/forest run logs <builder-run-id> --json
   ```

   Confirm its retained request/work ID and `review` authority, then remove the
   ticket's intake labels before the next Builder dispatch. Leave the scheduler
   available for this candidate's Verifier and, if needed, Fixer. Follow their
   own Run IDs, not a guessed time-window association. If selection or execution
   is wrong, pause admission immediately and inspect evidence before retrying.
3. **Read the exact candidate.** After the Builder publishes, inspect:

   ```sh
   ./.iron-forest/bin/forest review list --json
   ./.iron-forest/bin/forest review show <candidate-sha> --json
   ./.iron-forest/bin/forest run show <verifier-run-id> --json
   ```

   Follow request, Checks and Verdict for that exact revision. A successful
   process exit alone is not approval. A `changes` verdict is a completed review,
   not a merge authorization; inspect the Fixer's new revision and its new
   verification. The approval must have a bound `verifier_run_id` as defined in
   [ADR 0022](adr/0022-kernel-verdict-publication.md). Historical unbound evidence
   is readable, not identity proof.
4. **Open the PR and perform the human step.** Follow
   [Read the result](../README.md#read-the-result): require passing exact-revision
   Checks, an approve Verdict with the bound Verifier Run, and an open PR whose
   head is that same candidate SHA. Inspect the diff and required GitHub checks,
   then merge on GitHub as the operator. A changed head needs fresh evidence;
   never merge a new revision on an old approval. Agents never merge review-only
   work, enable auto-merge, or mark tracker work done.
5. **Drain and reconcile.** After terminal delivery, pause/drain before admitting
   another ticket. It is also safe to drain as soon as approval and PR creation
   are terminal, before taking time for human review; do not leave intake open
   while away.

   ```sh
   ./.iron-forest/bin/forest admission drain
   ./.iron-forest/bin/forest admission show --json
   ```

   Require paused admission with no active Runs. Drain waits without cancelling;
   `admission pause` is the immediate non-waiting alternative. Confirm the actual
   GitHub merge and record the PR URL, candidate SHA, bound Verifier Run and merge
   SHA in the local receipt. Reconcile the Linear ticket manually: attach the
   evidence, mark it done only after the merge, and keep intake labels removed.
   A merged-but-unreconciled ticket is operator work, not another paid Run.

Canopy is a read-only way to watch the repository, Runs and review/PR joins.
Use its source links to reach the CLI/Git evidence and GitHub PR; preserve
missing, stale or unknown states instead of inferring success from a green card.
Keep full prompts/logs behind the owning access boundary. Neither a merge nor
tracker reconciliation proves deployment.
