# Verify a factory change

Use this procedure for Kernel CLI, request, declaration, cancellation, and native
publication changes. The canonical entry point remains the `iron-forest` skill;
this is its on-demand verification reference, not another skill or runner.

## Select the proof

- Kernel mechanics: the Go build, vet, and test commands in
  [README Development](../../README.md#development) and CI.
- Prose-only operator documentation: review meaning and consistency against the
  owning implementation and resolve changed links. If the operator skill entry
  changed, also use [explicit offline discovery](#discovery-without-ambient-authority).
  This does not require a factory journey or model run by default.
- Factory declaration, shared/role skill, prompt, or publication protocol:
  existing `./evals/run-fast.sh`, including its real Forest/Pi delivery journey.
  See [evaluation strategy](../../docs/evaluation-strategy.md) for coverage.
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

Prerequisites depend on the selected proof: Git, Python 3 and Docker daemon
access for the journey; uv for Harbor cases; the repository's Go version via
mise for host checks. The standalone sequence also uses GNU `timeout`. The image
pins Go, Node, Pi, and the scanner in `evals/image/Dockerfile`; `evals/uv.lock`
pins Harbor. Dependency/image downloads need network access during setup.
No model, GitHub, Linear, or production
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

Use [README Development](../../README.md#development) for full versus focused
fast-tier commands and dependency setup. No selectors means the full merge
check; focused success covers only the selected cases/journey, not the full tier.
Every invocation builds through Docker's content cache and records an immutable
image ID plus the actual Kernel SHA-256 in `image.json`. Tasks and the journey
execute that ID, not the shared `iron-forest-eval:local` tag; the journey verifies
the receipt against its installed binary. Do not remove or retag shared images
during cleanup.

Retain this invocation's `evals/jobs/fast/<job>/` and sibling `<job>-inputs/`,
not an arbitrary latest historical result. The inputs retain `image-id`,
`image.json`, and any selected generated tasks. On completion the job contains
`image.json`, Harbor's `report.json` and `report.md` when cases ran, and
`journey.json` when the journey ran. A failure can stop before later artifacts
are written; missing downstream evidence is not a pass. HEAD plus a dirty flag
does not identify exact uncommitted source: retain the candidate diff/status
and any untracked inputs needed to reconstruct it.

## Bounded standalone real-target interaction

The existing journey is a consumer of the actual candidate `forest` executable,
real Pi process, `forest once`, publication CLI, and cancellation CLI. It supplies
deterministic actions through the existing oracle hook, not a live model. It
owns `/workspace` and `/origin.git` inside a fresh container; **never invoke
`evals/runtime/journey.py` directly on the host or mount a checkout into it**.

Run this shell sequence from the candidate worktree. The unique image tag,
container name, and receipt directory belong only to this run. The only host
mount is the generated, read-only image receipt, never checkout or runtime data.
The 15-minute runtime limit does not include dependency/image download time;
a timeout is an incomplete exercise, not a pass.

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
  --provenance=false --file evals/image/Dockerfile --tag "$image" \
  --iidfile "$proof_dir/image-id" \
  --build-arg "FOREST_BUILD_SHA=$sha" \
  --build-arg "FOREST_BUILD_DIRTY=$dirty" . || exit 1
image_id=$(cat "$proof_dir/image-id") || exit 1
kernel_sha256=$(docker run --rm --network none --entrypoint sha256sum \
  "$image_id" /opt/iron-forest/.iron-forest/bin/forest) || exit 1
python3 - "$sha" "$dirty" "$image_id" "${kernel_sha256%% *}" \
  > "$proof_dir/image.json" <<'PY' || exit 1
import json, sys
sha, dirty, image, kernel = sys.argv[1:]
print(json.dumps({"build_sha": sha, "dirty": dirty == "true",
                  "image": image, "kernel_sha256": kernel}, sort_keys=True))
PY
# Receipt-only mount, no credentials or networking; --init supports cancellation.
if timeout --signal=TERM --kill-after=30s 15m \
  docker run --name "$container" --init --network none --user root \
  --env "FOREST_EVAL_IMAGE_ID=$image_id" \
  --mount "type=bind,src=$proof_dir/image.json,dst=/run/forest-eval-image.json,readonly" \
  "$image_id" python3 /opt/iron-forest-eval/journey.py > "$proof_dir/journey.json"
then result=0; else result=$?; fi
cat "$proof_dir/journey.json"
printf 'journey exit: %s\n' "$result"
```

Do not proceed past a failed image build or receipt-generation step. Inspect
the exit and report before cleanup. A successful report has `passed: true`,
`execution.kind: deterministic-oracle`, copied `image_inputs` fields matching
the `image.json` receipt, and additionally `image_inputs.actual_kernel_sha256`
equal to `image.json.kernel_sha256`, the expected `forest_version` build SHA and
dirty flag, distinct rejected/delivered revisions, and distinct interrupted and
recovered Run IDs. Inspect phases and final refs, not only process exit.

The journey's own assertions require:

1. Builder publishes `wrong` while primary remains unchanged.
2. Verifier publishes `changes` even though the weak file-existence Check passes;
   primary still cannot move. This planted semantic defect is the plausible
   failure exercise, not evidence that a model spotted it.
3. Fixer publishes `ready` in a fresh Revision and preserves historical evidence.
4. Cancellation before publication leaves delivery unperformed and preserves
   exact tracked, staged, untracked, ignored, and unpublished committed source
   bytes through Git GC. A fresh Run, not a fictitious resumed session, reviews
   and delivers the repaired Revision without deleting interrupted source.
5. Identical publication retry leaves all refs unchanged and only one human
   projection exists.
6. After delivery and retry, the fixture explicitly disposes of the retained
   interrupted worktree and records that disposal in `source_recovery`.

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

These standalone names are from this run only. Remove its unique tag, not the
image ID by force: Docker may share identical image content with another run.
Never use Docker prune or remove another run's containers/images. Retain
`proof_dir` until the reviewer accepts the receipt; then remove that exact
directory, not a wildcard.

Fast-tier artifacts and generated inputs stay in ignored `evals/jobs/`. Its
direct hash-probe and journey containers use `--rm`; Harbor owns its job-specific
resources. The runner does not perform blanket image or job-directory cleanup.
After interruption, inspect resources for that exact job before deleting
anything; do not remove the shared `iron-forest-eval:local` tag. Stopping a
process is not evidence of successful fixture cleanup. No systemd unit, shared
backend, remote repo, or scheduler belongs to this procedure.

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
