# Onboarding a managed repository

Iron Forest runs one Kernel process per repository. The Kernel uses that
repository's `forest.yaml`, agent declarations, Git refs, and local Ledger.
Self-host mode uses the factory source checkout as the managed repository.
Sibling mode keeps a separate managed checkout beside the factory source.

## Prerequisites

Install these tools on the host:

- Git with push access to the managed repository;
- `gh` for the day-one GitHub adapter;
- `mise` and the managed repository's declared check tools;
- `pi`; the shipped contract uses its built-in providers. It is the agent harness; see [ADR 0018](adr/0018-pi-harness.md).

The user service resolves these tools through
`%h/.local/bin:%h/bin:%h/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin`.

Configure forge access and Pi provider credentials for the service. Put its
credential variables in `%h/.config/iron-forest/%i.env`; do not put
credentials or adapter configuration in `forest.yaml`, defaults, declarations,
prompts, skills, or commits. Runs inherit credentials only from the service
environment. A trusted declaration has those credentials plus filesystem and
network access. Worktree separation and timeout are not a security sandbox;
stronger containment belongs to deployment.

## 1. Create the ready label

Selection starts only when an open Issue has the `forest:ready` label:

```sh
gh label create forest:ready \
  -R owner/name \
  --color 0e8a16 \
  --description "ready for Iron Forest"
```

Do not use a second scheduling label. The Builder Poll checks this label and
checks that no matching remote `forest/<issue>-*` branch exists.

## 2. Declare the repository

Write `forest.yaml` at the root of the managed checkout:

```yaml
repo: owner/name
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

`repo` is the forge identity. `agents` maps each declaration to a Poll command
and interval. Agent Runs have no wall-clock deadline. They finish when Pi
finishes or an operator explicitly cancels a foreground `forest once`; service
shutdown stops new dispatches and drains active Runs without a systemd
deadline. Direct `forest poll` execution has a fixed 60-second deadline. The
Scheduler gives its configured Poll command a separate 65-second bound. The
supervisor preserves this full 5-second difference as Poll shutdown grace. It
lets the direct Poll stop Git/GitHub transport groups and remove private note
snapshot refs before the supervisor force-stops its command group. Runner
cleanup has a separate 10-second bound. A completed dispatch starts an audit
with a separate 60-second bound. These mechanical bounds do not limit agent
reasoning or model execution. The model is
in declaration frontmatter, not `forest.yaml`. `checks:` is the complete check
list for this repository. Mirror these commands in `.github/workflows/ci.yml`
in the same order.

## 3. Add declarations

Create one pair of prompt files per shipped declaration, one shared skill
directory, and optional role-specific skill directories:

```text
agents/
  _shared/
    skills/
  builder/
    agent.md
    task.md
  verifier/
    agent.md
    task.md
    skills/
  fixer/
    agent.md
    task.md
```

`agent.md` starts with YAML frontmatter containing optional `model`, `tools`,
and `thinking`, then the system prompt. `task.md` is the standing user prompt.
`model` and `thinking` fall through declaration frontmatter to instance
defaults; `model` alone has a built-in final value. Defaults contain only
`model` and `thinking`. Keep Git note, merge, and always-on engineering
instructions in the system prompts. Agents use native `git`; no wrapper is
required.

The only skill sources are `agents/_shared/skills` and, when present,
`agents/<name>/skills`. Their published paths are repository-relative and Pi
resolves them from the Run worktree. The Runner gives each Run a new writable
`PI_CODING_AGENT_DIR` without operator Pi state. For an OpenRouter model, it
contains only a generated, credential-free model override that enables the
Run ID's session-affinity header. The Runner invokes Pi with `--no-extensions`,
`--no-skills`, `--no-prompt-templates`, and `--no-themes`, plus one explicit
`--skill` per existing skill source directory. Declaration and Run evidence
publish those directory paths as `skills`.

## 4. Build and validate

Choose one deployment mode.

Both modes require a protected environment file named after the managed
checkout directory. Before running the installer, create
`~/.config/iron-forest/<checkout-directory-name>.env`, put the required
provider credential variables in it, and set its mode to `0600`. The installer
does not create or rewrite this file.

### Self-host mode

The factory source checkout is also the managed repository. From that checkout,
build with the pinned toolchain and validate its declarations:

```sh
mise exec -- go build -o forest .
./forest selfcheck
deploy/install-service.sh
```

The no-argument installer builds the factory source into the same checkout and
enables its service instance.

### Sibling mode

The managed repository is a sibling of the factory source checkout and does not
need the Iron Forest Go source. From the factory source checkout, build the
Kernel into the sibling, validate there, and install that sibling instance:

```sh
mise exec -- go build -o ../<sibling-directory-name>/forest .
(cd ../<sibling-directory-name> && ./forest selfcheck)
deploy/install-service.sh <sibling-directory-name>
cd ../<sibling-directory-name>
```

The one-argument installer always builds the Kernel from the factory source
checkout into the named sibling. Before restarting either mode, the installer
stops that instance and removes only timestamped legacy `.forest/profiles`
entries, which may contain credentials copied by an older Kernel. It then runs
selfcheck with the service's trusted `PATH` and without `FOREST_DEFAULTS`.
The installed unit reads operator-supplied credentials from
`%h/.config/iron-forest/%i.env`; protect it as mode `0600` and set
`OPENROUTER_API_KEY` to a dedicated completion key for this Forest instance.
Never put a management, personal interactive, or evaluation key there. Use this
per-instance file rather than setting `PI_CODING_AGENT_DIR` or naming an
operator profile. Separate role keys are not an isolation boundary while Runs
share the service user and can read this file. Use a systemd drop-in only when
one instance needs a different defaults file. The installer stops on any
selfcheck error. The Auditor needs a completed agent dispatch before it can
validate remote Git evidence.

The final `cd` keeps all later Kernel and observation commands in the managed
repository.

## 5. Start the Kernel

The installer steps above enable and restart one user service for this
repository. Do not start a second foreground Kernel. For a manual foreground
deployment, skip the installer and start exactly one process from the managed
checkout:

```sh
./forest serve
```

Exit 0 dispatches work.
Exit 1 is a healthy skip. Exit greater than 1, deadline expiry, or malformed
behavior skips the tick and logs an error. `forest status` reports the trigger's
consecutive Poll error count, last Poll exit code, and separate persisted Poll,
Run, and Audit errors. A healthy Poll clears only its Poll error. A successful
Run clears only its Run error. A successful Audit clears only its Audit error.
The Auditor runs after a completed dispatch, not at startup or after an idle
Poll skip.

Verifier and Fixer Poll enumeration is bounded at 500 entries per canonical
notes tree. A larger tree or a note-enumeration transport-output overflow is a
healthy exit-1 skip with an explicit log line. It does not mark the trigger
unhealthy; the Auditor reports durable note growth as a bounded policy
violation.

Poll once and conditionally dispatch one declaration:

```sh
./forest once builder
./forest poll builder
./forest status
```

`once` evaluates the configured Builder Poll first. It dispatches only when that
Poll exits 0. A healthy Poll skip exits 1 without an agent Run.

## 6. Observe the first Subject

After an Issue receives `forest:ready`, the Builder selects it and creates a
`forest/<issue>-<slug>` branch. It writes a review-request note on the exact
Revision and publishes the branch and note with one normal atomic push. A
canonical note race permits at most three total atomic attempts; a branch race
stops. The Builder may open a pull request as a human Projection.

The Verifier selects that branch and runs every configured Check. For `changes`,
it publishes Checks and Verdict together. A canonical note race permits at most
three total atomic attempts. For `approve`, the Verifier makes exactly one
non-retryable atomic attempt carrying Checks, Verdict, and the exact reviewed
Revision's fast-forward `master` update. The Gate also requires the existing
valid Builder-or-Fixer review-request for that Revision; the approve push does
not republish it. No standalone master push is valid. If the Verdict is
`changes`, the Fixer owns the branch, creates a new Revision, and publishes a
fresh review request atomically. That note is the reject handoff back to the
Verifier.

From the managed checkout, use status and Git notes as the evidence surface:

```sh
./forest status
git log --oneline --decorate --all
git fetch origin \
  refs/notes/forest/review-request:refs/notes/forest/review-request \
  refs/notes/forest/checks:refs/notes/forest/checks \
  refs/notes/forest/verdict:refs/notes/forest/verdict
git notes --ref=refs/notes/forest/review-request show <revision>
git notes --ref=refs/notes/forest/checks show <revision>
git notes --ref=refs/notes/forest/verdict show <revision>
```

Pull requests and other forge artifacts are Projections. Git branches, commits,
and notes remain authoritative. The first observed remote `master` tip becomes
a trusted baseline and is not Gate-checked. In each bounded stable snapshot,
Auditor ancestry and Gate checks target only the final observed remote
`master` tip. Schema and actor checks cover each snapshotted
`refs/notes/forest/*` entry within a 500-entry-per-ref capacity bound. Remote
history cannot reveal a tip that advanced again between audits; such
intermediate tips are not independently Gate-checked. The Auditor checks
observable final Git state only. It cannot prove check execution, atomic push
ordering, or force absence. It detects violations after a completed dispatch
and does not block or enforce them.

## 7. Change configuration safely

Commit changes to `forest.yaml` or `agents/` through the same review Gate as
code. Run `./forest selfcheck` after local configuration or declaration
validation. A remote audit occurs only after a completed agent dispatch; Kernel
startup and idle Poll skips do not audit. Keep `checks:` and
`.github/workflows/ci.yml` aligned.

Each `.forest/runs.jsonl` Ledger row records `run_id`, `agent`, `started`,
`duration`, `exit`, `tokens_in`, `tokens_out`, `cache_read`, `cache_write`, and
`reasoning`. It never records or computes money.
