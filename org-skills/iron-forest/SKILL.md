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

Resolve the managed repository and read `VISION.md`, `forest.yaml`, and the relevant declaration under `agents/<name>/`. Run the CLI read surfaces before inferring state:

```sh
forest version --json
forest config show --json
forest declaration list --json
forest status --json
forest audit show --json
```

Use `forest declaration show <name> --json`, `forest run list --json`, and `forest run show <id> --json` only for the declaration or Run that can change the decision. Treat Git evidence as authority and CLI output as the supported operational projection.

Done when the repository, Kernel revision, configured roster, active or failed Runs, and current Audit state are explicit.

## Configure

Keep the Kernel generic. Configure each role through:

- `forest.yaml`: arbitrary declaration name, executable Poll, interval, optional `max_duration`;
- `agents/<name>/agent.md`: system prompt plus model, thinking, and Pi tool allowlist;
- `agents/<name>/task.md`: standing task;
- `agents/_shared/skills/`: skills every declaration receives;
- `agents/<name>/skills/`: skills only that declaration receives;
- `checks:`: deterministic exact-revision gates.

Start with Pi's smallest useful tool set. Add a CLI through `bash` plus an explicit skill when that is sufficient. Add a Pi extension only through an accepted, inspectable declaration input; ambient Pi extensions are disabled. Keep credentials in the instance service environment, never in configuration, prompts, skills, or commits.

The shipped Builder, Verifier, and Fixer are opinionated defaults, not a
required roster. Critic and Tester remain experimental local canaries; keep
them out of external profiles until the rollout gate in `README.md` closes. A
managed repository may otherwise replace the roster, Polls, prompts, model,
thinking, tools, and skills without forking the Kernel.

The shipped end-to-end work sources remain GitHub and Powder. A custom Poll
does not replace the Kernel's current tracker validation and Powder terminal
reconciliation. Tracker-independent and Habitat profile lifecycles remain open
architecture work; follow `README.md`, `VISION.md`, and the accepted ADRs until
that cutover lands.

Done when `forest selfcheck`, `forest config show`, and every affected `forest declaration show` expose the intended configuration and no ambient resource supplies hidden behavior.

## Operate

Use `forest once <name>` for one supervised dispatch. Use `forest run logs`, `forest run cancel`, trigger controls, and the sanctioned deployment procedure shown by `forest --help` and the repository runbook. Diagnose a failed Run from its retained log, exact declaration digest, revision, and Audit state before changing configuration.

For several repositories, keep each Kernel independent. An external manager may collect their JSON read surfaces, groom each repository's work source, and propose profile changes. It does not create a cross-repository Kernel, shared coordination store, or hidden policy path.

Done when the requested Run or configuration action has one observable result, and any failure names its repository, declaration, Run id, and next owned action.

## Change the factory

Put stable role policy in declaration prompts or explicit skills. Put deterministic repository invariants in `checks:` or custom linters. Add Kernel code only for a closed mechanical loop with a known retry predicate and evidence that declarations or executable profile tools cannot own it reliably.

For a new agent, define its trigger, evidence surface, authority, output, and stop condition before adding it to the roster. For a new tool or extension, prove one real role scenario and record the capability and credential boundary.

Done when the change has one owner, one proof path, and no second representation of existing policy.
