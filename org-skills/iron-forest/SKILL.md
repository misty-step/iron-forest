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

Resolve the managed repository and use the README's [intent routes](../../README.md#start-here), not new-profile setup for an existing instance. Start with current health and admission:

```sh
./.iron-forest/bin/forest status --json
./.iron-forest/bin/forest admission show --json
```

Choose further reads only for the question: `forest version --json` identifies
the executing binary; `forest config show --json`, `forest declaration list
--json`, and `forest declaration show <name> --json` read the current on-disk
profile. These do not establish which revision or profile a live service adopted.
For a Run, use `forest run list --json`, `forest run show <id> --json`, and
`forest run logs <id> --json`; for native delivery, use `forest review show <sha>
--json` or `forest audit show --json`.
The README's [read/command boundary](../../README.md#reading-the-factory),
[Run facts](../../README.md#execution-completion-and-delivery), and
[Git authority](../../README.md#git-coordination) own their interpretation.
Read the relevant profile or declaration source when the action needs an edit.

Done when the repository and evidence needed for the requested decision are
explicit, with unknown state kept distinct from success.

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
metadata grants no queue or integration authority; see the
[composition boundary](../../README.md#repository-owned-composition).
R90 profiles continue to use Habitat.

Done when `forest selfcheck`, `forest config show`, and every affected `forest declaration show` expose the intended configuration and no ambient resource supplies hidden behavior.

## Operate

For first approved dispatch, follow [onboarding](../../docs/onboarding-managed-repo.md#execute-the-first-explicit-request).
For interrupted work, follow [pause, drain and recover](../../docs/onboarding-managed-repo.md#pause-drain-and-recover)
before authorizing another attempt. For merged revisions, use
[fenced adoption](../../README.md#adopting-merged-revisions), not a restart alone.

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
