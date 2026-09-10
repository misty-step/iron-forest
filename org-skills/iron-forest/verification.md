# Verify a factory change

Use this procedure for Kernel CLI, request, declaration, cancellation, and native
publication changes. The canonical entry point remains the `iron-forest` skill;
this is its on-demand verification reference, not another skill or runner.

## Select the proof

- Kernel mechanics: the Go build, vet, and test commands in
  [README Development](../../README.md#development) and CI.
- Declaration, prompt, skill, or publication protocol: existing
  `./evals/run-fast.sh`, including its real Forest/Pi delivery journey. See
  [evaluation strategy](../../docs/evaluation-strategy.md) for coverage.
- A narrow delivery/cancellation investigation: the standalone journey below.
  It does not replace the full fast tier when that tier is required.
- Model quality: only a separately authorized live-model experiment. Neither
  `selfcheck` nor the oracle establishes judgment quality or hosted integration
  readiness. Do not load evaluation keys or run `run-experiment.sh` by default.

`VERIFY.md` is historical evidence for a particular revision, not a current
setup procedure or permission to recreate its remote repository.

## Target and setup

Use a dedicated candidate worktree. Record `git rev-parse HEAD` and
`git status --short` with the evidence; an uncommitted candidate is not identified
by HEAD alone. Do not use the checked-in/profile-installed binary as evidence of
the candidate. Never copy `.env`, credentials, or `.iron-forest/runtime` from an
operating factory. Do not start its Kernel, dispatch against its origin, install
a service, or alter admission for local verification.

Prerequisites: Git, Python 3, Docker daemon access, uv, and the repository's Go
version via mise for host checks. The image pins Go, Node, Pi, and the scanner in
`evals/image/Dockerfile`; `evals/uv.lock` pins Harbor. Dependency/image downloads
need network access during setup. No model, GitHub, Linear, or production
credentials are needed. Fixture Git origins and identities belong to the
container. Existing evaluation adapters are controlled fixtures, not evidence
that hosted GitHub or another delivery backend works.

From the worktree root, the complete deterministic checks are:

```sh
mise exec -- go build ./...
mise exec -- go vet ./...
mise exec -- go test ./...
FOREST_EVAL_CONCURRENCY=2 ./evals/run-fast.sh
```

The fast tier generates its task corpus, syncs the locked Python environment,
builds `iron-forest-eval:local`, runs Harbor cases, checks rewards, and runs the
journey. Serialize fast-tier runs on a shared Docker daemon: that existing image
tag is shared. Do not overwrite another run's image. Retain the exact new job
path printed by Harbor, not an arbitrary latest historical result. Inspect its
`report.json`, `report.md`, and `journey.json` in `evals/jobs/fast/<job>/`.

## Bounded standalone real-target interaction

The existing journey is a consumer of the actual candidate `forest` executable,
real Pi process, `forest once`, publication CLI, and cancellation CLI. It supplies
deterministic actions through the existing oracle hook, not a live model. It
owns `/workspace` and `/origin.git` inside a fresh container; **never invoke
`evals/runtime/journey.py` directly on the host or mount a checkout into it**.

Run this shell sequence from the candidate worktree. The unique image and
container names belong only to this run. The 15-minute runtime limit does not
include dependency/image download time; a timeout is an incomplete exercise,
not a pass.

```sh
proof_dir=$(mktemp -d "${TMPDIR:-/tmp}/forest-proof.XXXXXXXX")
proof_id=$(basename "$proof_dir")
image="iron-forest-eval:$proof_id"
container="$proof_id"
sha=$(git rev-parse HEAD)
dirty=false
if test -n "$(git status --porcelain)"; then dirty=true; fi
git diff --binary > "$proof_dir/candidate.patch"
git status --short > "$proof_dir/candidate-status"
printf '%s\n' "$sha" > "$proof_dir/source-sha"
timeout --signal=TERM --kill-after=30s 20m docker build \
  --file evals/image/Dockerfile --tag "$image" \
  --build-arg "FOREST_BUILD_SHA=$sha" \
  --build-arg "FOREST_BUILD_DIRTY=$dirty" .
# No host mounts, credentials, or networking; --init supports cancellation.
timeout --signal=TERM --kill-after=30s 15m \
  docker run --name "$container" --init --network none --user root "$image" \
  python3 /opt/iron-forest-eval/journey.py > "$proof_dir/journey.json"
result=$?
cat "$proof_dir/journey.json"
printf 'journey exit: %s\n' "$result"
```

Do not proceed past a failed image build. Inspect the exit and report before
cleanup. If running from a shell with `set -e`, capture a nonzero runtime exit
explicitly so cleanup still runs. A successful report has `passed: true`,
`execution.kind: deterministic-oracle`, the expected `forest_version` build SHA
and dirty flag, distinct rejected/delivered revisions, and distinct interrupted
and recovered Run IDs. Inspect phases and final refs, not only process exit.

The journey's own assertions require:

1. Builder publishes `wrong` while primary remains unchanged.
2. Verifier publishes `changes` even though the weak file-existence Check passes;
   primary still cannot move. This planted semantic defect is the plausible
   failure exercise, not evidence that a model spotted it.
3. Fixer publishes `ready` in a fresh Revision and preserves historical evidence.
4. Cancellation before publication leaves delivery unperformed; a fresh Run,
   not a fictitious resumed session, reviews and delivers the repaired Revision.
5. Identical publication retry leaves all refs unchanged and only one human
   projection exists.

A missing rejection, early primary move, stale Revision approval, lost historical
ref, ineffective cancellation, or duplicate publication must fail the journey.
If it fails, retain its report and inspect `docker logs "$container"` and
`docker inspect "$container"`. Before removing the owned container, evidence
can be recovered with `docker cp "$container:/workspace/.iron-forest/runtime"
"$proof_dir/runtime"`. Inspect and sanitize logs before any sharing. Report
which phase failed and its Run/revision, rather than calling the whole factory
broken or claiming downstream phases passed.

## Ownership and cleanup

After collecting evidence, including on timeout:

```sh
docker rm --force "$container"
docker image rm "$image"
```

These names are from this run only. Never use Docker prune or remove another
run's containers/images. Retain `proof_dir` until the reviewer accepts the
receipt; then remove that exact directory, not a wildcard. Fast-tier job
artifacts stay in ignored `evals/jobs/`; inspect Harbor's job-specific resources
before deleting anything after interruption. Stopping a process is not evidence
of successful fixture cleanup. No systemd unit, shared backend, remote repo,
or scheduler belongs to this procedure.

## Discovery without ambient authority

The repository intentionally uses `org-skills`, not automatic `.agents` loading.
An operator-supervised Pi session composes this skill explicitly:

```sh
discovery_dir=$(mktemp -d "${TMPDIR:-/tmp}/forest-discovery.XXXXXXXX")
PI_CODING_AGENT_DIR="$discovery_dir" PI_TELEMETRY=0 \
  pi --offline --no-session --no-context-files --no-tools \
  --no-extensions --no-skills --no-prompt-templates --no-themes \
  --skill ./org-skills/iron-forest --verbose
```

Before sending any model prompt, inspect the loaded-resource display and confirm
`iron-forest` resolves to this candidate's `org-skills/iron-forest/SKILL.md`. Open
its verification reference and confirm the commands resolve from the candidate
root. Missing/duplicate/wrong-checkout skill loading is a discovery failure;
do not fix it by enabling ambient resources. Exit the supervised session when
done, then remove only the run-owned `discovery_dir`. An empty isolated config
may report no available models; that does not prevent resource discovery and
is not a reason to authenticate. A fresh operator can also reach this reference
through README Development and AGENTS without a global skill installation.

Factory Runs are a different composition: Runner disables ambient resources and
passes only declaration/shared skill paths. Do not add this operator skill to
all declarations merely to make it visible. For a deliberate declaration skill
change, inspect `forest declaration show <name> --json` and the resulting Run's
resolved resources/digests in the isolated authorized fixture. Production
selfcheck or existing installed-binary output cannot prove candidate discovery.
