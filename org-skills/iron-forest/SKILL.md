---
name: iron-forest
description: Operate or configure one Iron Forest repository factory; inspect its declarations, Runs, health, evidence, or managed-repository profile.
---

# Iron Forest

Iron Forest runs one small Kernel for one repository. The Kernel schedules and
isolates Pi Runs. Runs are unbounded by default; a declaration may set the
optional `max_duration`. The repository profile owns the roster, Polls,
prompts, skills, tool allowlists, and checks. Use an external manager to
coordinate multiple repository instances.

## Orient

Resolve the managed repository and read `README.md`, `.iron-forest/config.yaml`, and the relevant declaration under `.iron-forest/agents/<name>/`. Run the CLI read surfaces before inferring state:

```sh
./.iron-forest/bin/forest version --json
./.iron-forest/bin/forest config show --json
./.iron-forest/bin/forest declaration list --json
./.iron-forest/bin/forest status --json
./.iron-forest/bin/forest admission show --json
./.iron-forest/bin/forest audit show --json
```

Use `forest declaration show <name> --json`, `forest run list --json`, and `forest run show <id> --json` only for the declaration or Run that can change the decision. Treat Git evidence as authority and CLI output as the supported operational projection.

Done when the repository, Kernel revision, configured roster, active or failed Runs, and current Audit state are explicit.

## Configure

Keep the Kernel generic. Configure each role through:

- `.iron-forest/config.yaml`: declaration Polls/intervals, optional `max_duration`, profile `delivery` ceiling, and explicit `required_tools`;
- `.iron-forest/agents/<name>/agent.md`: system prompt, model, thinking, tool allowlist, optional scheduled `request` command and explicit `extensions`;
- `.iron-forest/agents/<name>/task.md`: standing task;
- `.iron-forest/agents/_shared/skills/`: skills every declaration receives when present;
- `.iron-forest/agents/<name>/skills/`: skills only that declaration receives when present;
- `checks:`: deterministic exact-revision gates.
- `.iron-forest/defaults.yaml` and `.iron-forest/secrets.yaml`: instance defaults and narrow scanner policy;
- `.iron-forest/bin/forest` and `.iron-forest/runtime`: the installed executable and persistent instance evidence.

Start with Pi's smallest useful tool set. Add a CLI through `bash` plus an explicit skill when that is sufficient. Add a Pi extension only through an accepted, inspectable declaration input; ambient Pi extensions are disabled. Keep credentials in the instance service environment, never in configuration, prompts, skills, or commits.

Builder, Verifier, and Fixer are optional defaults for explicitly requested
compatible work. Critic and Tester report requested read-only findings; they
do not create tickets, edit code, publish Git, or promote findings into work. A
managed repository may otherwise replace the roster, Polls, prompts, model,
thinking, tools, and skills without forking the Kernel.

Misty Step work starts from current operator requests. Historical tracker
adapters remain in code but must not be reconfigured or used as a queue. R90
profiles continue to use Habitat.

Done when `forest selfcheck`, `forest config show`, and every affected `forest declaration show` expose the intended configuration and no ambient resource supplies hidden behavior.

## Operate

Use `forest once <name> --request <file>` for one explicitly authorized supervised
dispatch from a `forest.request.v1` envelope; optional `authority: land|review`
can restrict delivery per work item (omission keeps the profile default).
`work.system` and `work.id` preserve an optional immutable association without
giving Kernel a tracker API. Review approval publishes evidence with
`status: review-only`, never advances primary, and remains restricted across
Verifier/Fixer Runs through the immutable candidate request. Stop an
existing scheduler before `once`; both respect persistent admission. Use
`forest admission pause|drain|resume` to control new dispatch, and `forest run
cancel` only to cancel a particular Run. Drain does not cancel or resume Pi
sessions. `delivery: external` refuses native publication and reports native
audit as not applicable, not as a pass. Diagnose failed or interrupted Runs from
their retained request, log, Ledger, and resolved resource digests.

For several repositories, keep each Kernel independent. An external manager may collect their JSON read surfaces, groom each repository's work source, and propose profile changes. It does not create a cross-repository Kernel, shared coordination store, or hidden policy path.

Done when the requested Run or configuration action has one observable result, and any failure names its repository, declaration, Run id, and next owned action.

## Change the factory

For candidate verification, load [verification.md](verification.md). It selects
the existing Go/fast-tier checks or the standalone real Forest/Pi journey, with
isolated fixture ownership, rejection/recovery oracles, evidence, cleanup, and
explicit skill-discovery checks. Do not use the historical `VERIFY.md` receipt
as current readiness or run local proof against the operating factory.

Put stable role policy in declaration prompts or explicit skills. Put deterministic repository invariants in `checks:` or custom linters. Add Kernel code only for a closed mechanical loop with a known retry predicate and evidence that declarations or executable profile tools cannot own it reliably.

For a new agent, define its trigger, evidence surface, authority, output, and stop condition before adding it to the roster. For a new tool or extension, prove one real role scenario and record the capability and credential boundary.

Done when the change has one owner, one proof path, and no second representation of existing policy.
