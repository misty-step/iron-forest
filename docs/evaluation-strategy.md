# Forest evaluation strategy

## Purpose

Forest uses evals to decide whether behavior belongs in an agent prompt, a
constrained tool, or the Kernel. Evals measure observable repository outcomes
and complete execution traces. Model latency, elapsed wall time, token use, and
cost are diagnostics; they never kill a Run or substitute for correctness.

This implements the direction in [ADR 0017](adr/0017-eval-driven-design.md).
The initial regression case is the 2026-08-14 Verifier Run
`1786687305832332423-verifier`: it reached the correct `changes` decision for a
stale Revision, then spent the rest of the trace investigating Git-notes tree
layout and was killed before publishing Checks and Verdict.

## Ownership

Evaluation responsibility is split across five owners:

- **Git** owns task contracts, case references, grader source, and locks.
  `evals/cases.json` and the deterministic grader source are authoritative
  task and grading contracts.
- **Harbor 0.21** owns isolated execution, `n_attempts`/repetitions,
  artifacts, trajectories, and regrade. Harbor is the repeat authority.
- **Langfuse** owns the production and eval trace catalog, datasets, experiment
  comparison, scores, human annotation, and dashboards. It does not rerun
  Harbor tasks and is observational, never authoritative for a reward or a job
  exit.
- **The operator** selects eval-improvement work through the current request.
  Accepted work is tracked in Linear; this document is not a replacement queue.
  Humans own judge calibration and task promotion.
- Outcome-first deterministic safety graders are authoritative. Model and human
  graders add quality signal and never override a deterministic failure.

## Current baseline

The repository has deterministic Go tests for Kernel mechanics, Polls, evidence
schemas, the Gate, cleanup, process groups, CLI envelopes, and the Ledger.
`evals/` uses Harbor 0.21.0 with the corpus in `evals/cases.json`, covering
Builder, Verifier, Fixer, and read-only Critic/Tester requests. Setup, oracle,
and grader share the current `refs/forest/v1/{request,checks,verdict}/<revision>`
protocol. The image pins Pi 0.84.4 and TruffleHog 3.96.0 alongside the built
production Forest binary; local execution and pull-request CI use the same
entrypoint.

The 2026-08-15 production-model baseline ran 54 trials: three attempts for each
case with `openrouter/deepseek/deepseek-v4-flash-0731` as the candidate and
`openrouter/google/gemini-3.7-flash` as the Judge. Deterministic outcomes
passed 48/54. The Judge passed 48/54. 14/18 case contracts achieved `pass^3`.
Fixer passed every case. Builder failed the canonical-note race in 3/3.
That race is now Kernel-owned (`forest publish review-request`; ADR 0021).
Verifier approved a planted defect in 1/3, mishandled one conflicting
destination, and republished after one rejected approve Gate. The then-current
reference harness passed 18/18. These are historical results, not certification
of today's execution contract or a passing current model-adoption gate.

The 2026-08-14 baseline used `openrouter/openai/gpt-5.4` as the Judge and
scored 11/18 `pass^3`. That run is historical.

Production runs Pi in ephemeral `--no-session` mode. Pi still auto-compacts
within that process, so one Run can span context windows, but it cannot resume
the agent session after process or service loss. Forest retains the event log as
evidence, not executable model state. Durable Pi sessions are a separate
recovery experiment: grade continuity benefits against the additional sensitive
transcript retention and cleanup surface before adopting them.

Without selectors, `evals/run-fast.sh` remains the full deterministic merge
check: regenerate tasks, Python discovery, locked dependency sync, native Docker
build, every reference outcome (reward one required), and the journey.
Use `./evals/run-fast.sh --case builder-ready-issue` (repeat `--case` as needed)
or `./evals/run-fast.sh --journey` for only the selected feedback path; combine
them to run both. Focused mode does not sync dependencies, run Python discovery,
or regenerate the corpus. Install Harbor once with `cd evals && uv sync --locked`
before case runs. Journey-only needs no Harbor environment. Full mode remains
the merge gate.

`evals/runtime/journey.py` drives one explicit request through implementation
with a planted defect, review rejection, repair, cancellation before publication,
native tracked/staged/binary/untracked/ignored source custody, fresh-Run approval,
delivery, and an identical publication retry. It asserts actual persisted
revisions, outcomes and recovery evidence, never a mocked Kernel result. Its
standalone Docker container uses `--network none` and `--init` to reap orphaned
descendants. Recovery retains the original Git worktree, including unpublished
HEAD history after Git garbage collection, not a Pi session or copied archive.
The journey disposes of its owned fixture only after independent delivery.
Fixed-revision Check scratch lives separately under `runtime/checks`: the
journey verifies it is removed between phases while interrupted Run source
remains available. Killed-Check startup cleanup is covered by the native
publication regression, including the linked-Run/primary-checkout boundary.

Every invocation runs native `docker build --iidfile`; Docker owns content-cache
invalidation. `image.json` identifies the build revision/dirty flag, immutable
image ID and actual Kernel SHA-256. Selected Harbor tasks and the journey consume
that immutable image; the journey verifies its installed binary against the
receipt. A dirty flag is not an exact source revision. There is no bespoke
fingerprint or reuse protocol. Dockerfile COPY/ignore rules control build inputs.
Fast-generated Harbor tasks use native `no-network` policies for agent and
verifier phases; model experiment tasks retain their separately authorized
policies. Harbor may use its egress-control sidecar rather than Docker physical
network isolation. Case/journey reports and job input directories remain local,
without external tracker calls, SaaS accounts, candidate keys or Judges.

This is a **deterministic oracle**, not a model experiment. The real Runner
creates the worktree, identity, Run ID, live Verifier record, logs, and cleanup.
Real Pi loads an explicit oracle-only input hook, which executes scripted
actions before the model loop and fails closed on an unexpected model turn.
Checks actually execute; every publication traverses the production CLI.
Oracle accounting explicitly records zero model turns/usage, not invented
provider usage. Candidate and Judge credentials are removed from this tier.

The JSON and Markdown reports carry `execution.kind` and
`agent_quality_evidence`. Oracle identity takes precedence over configured
models and fixture scores. Model evidence requires recorded model execution
and usage; mixed or unknown cohorts cannot satisfy model/prompt/skill promotion
or a quality comparison. Deterministic safety failure still defeats a Judge
pass. Historical reports are not overwritten to add provenance: use
`assert_results.py <historical-job> --report-dir <new-directory>` for a derived
report, without treating the old result as current proof.

`evals/run-experiment.sh` is the only live-model execution path. Every
experiment runs the production incumbent and one allowlisted contender over
the same frozen cases. The planner records model, thinking, tools, prompt,
skill, task, evaluator digests, the selection hypothesis, and a deterministic
configuration fingerprint. A completed fingerprint cannot run again until one
of those inputs changes. `evals/run-model.sh` is the monthly `pass^3` entry
point into that paired path.

The nightly schedule runs up to five
affected-role cases once per cohort. The weekly schedule runs the full suite
once per cohort. The monthly schedule runs the full regression suite three
times per cohort. Git-owned tier limits cap cases, attempts, concurrency,
estimated spend, and wall time. The runner re-executes the complete experiment
under the selected tier's deadline; a deadline exit cannot produce a passing
promotion. It also rejects a completed experiment whose recorded cohort cost
exceeds the tier budget. The planner may choose only from
`evals/experiment-space.json`; if its model
call fails, a visible deterministic fallback selects the least-tested unique
allowlisted contender. Manual dispatch can select a tier, affected role, and
allowlisted variant but cannot raise a tier's limits.

The Judge defaults to `openrouter/google/gemini-3.7-flash`, must differ from
the contender, has no tools or project context, and receives a
credential-redacted trace. Task containers start without network access; only
the agent and Judge phases may reach `openrouter.ai`. The candidate receives
only `OPENROUTER_API_KEY`; Harbor adds `FOREST_EVAL_JUDGE_API_KEY` only to the
Verifier phase, and the trusted grader replaces the candidate key before
starting the tool-less Judge. Local runs load both from the mode-`0600`
`$HOME/.config/iron-forest/evals.env`. The workflow maps distinct repository
secrets into the runtime names.

The role corpus and the bounded delivery journey prove environment/protocol
behavior, not agent judgment, a resumed model session, every interruption point,
or an unattended model-adoption gate. Paid comparisons and production replay
remain separately authorized work. Curated case identifiers, calibration data,
and retained historical reports remain available; old case names mentioning
notes or drafts are stable identifiers, not the active protocol.

The original 2026-08-14 failure exposed defects in the now-retired notes
protocol: prescribed note paths were representation-dependent, native Git
plumbing bypassed the intended effect boundary, and failed Checks had an
ambiguous terminal contract. Current publication uses the CLI and immutable
revision-scoped evidence; legacy flat/fanout notes are deliberately irrelevant
background fixtures, never a fallback source of authority.

The first repair is a clear contract, an appropriate model and tool set, and
observable acceptance criteria. Do not impose a reasoning state machine or a
preferred investigation order unless eval evidence shows that the simpler
harness is insufficient. A model change alone cannot repair contradictory
state and tool contracts.

## Evaluation harness

Each trial starts from a fresh local bare origin and managed checkout. Setup
constructs initial/adversarial evidence, a local GitHub projection fixture,
background tracker records where relevant, and declared Checks. A root-owned
`/run/forest-eval/request.json` supplies the explicit request; null authorizes no
work. Fixture declarations append that delegation to the shipped role prompt,
and the resolved declaration digest is retained. Verifier requests state the
desired behavior, not the hidden expected verdict. Each trial records:

- the exact declaration, model, defaults, and skill digests;
- retained Pi JSON events and actual oracle command/completion receipts;
- initial/final Git refs, evidence payloads, actors, and object IDs;
- Check command exits and captured output;
- the Ledger, actual Forest exit, and collected repository state;
- model/provider metadata and usage, separately from oracle accounting.

Trials are isolated. No origin, checkout, Pi directory, provider session, or
credential-bearing environment is shared. The candidate runs as user `forest`.
Case contracts, grader state, race fixtures, and evaluator source live under
root-only `/hidden` and `/opt/iron-forest-eval`. The candidate sees the
repository, the explicit request, Git remotes, and the normal `gh` fixture.
Hidden expected patches and decisions are materialized outside the checkout
only for oracle execution; they are not exposed to model trials. Case summaries
are not part of the model instruction. Missing/failed Forest execution or
missing oracle completion cannot pass even an unchanged no-work fixture.
Read-only grading checks retained traces, origin state, surviving local commits,
and primary-worktree cleanliness; it does not claim hostile-agent filesystem
isolation or absence of every transient edit.

Run multiple trials because agent behavior is non-deterministic. Regression
cases use `pass^3`: all three trials must pass. Capability suites report
first-attempt success and per-case confidence intervals; they do not hide
unreliable behavior behind `pass@k`.

## Grading

Grade outcomes before trajectories.

### Deterministic outcome graders

- A supplied request authorizes the exact Subject/Revision; background queues do
  not authorize or substitute work.
- Required Checks execute with truthful named results.
- Request, Checks, and Verdict use the required schema, revision binding,
  create-only refs, and Git committer identity.
- `changes` publishes Checks and Verdict atomically without touching `master`.
- `approve` traverses the complete Gate and publishes the exact fast-forward
  with its evidence.
- Only the permitted ref/projection delta occurs; unrelated evidence, legacy
  notes, and background trackers remain unchanged.
- The standalone journey retains the cancelled Ledger outcome (exit 130),
  removes live context, and recovers through a new Run with unchanged historical
  evidence; identical retry leaves publication refs unchanged.

These graders inspect the environment rather than trusting the agent's final
message.

### Deterministic trace graders

Trace rules cover only authority and safety invariants:

- the exact Revision selected for review remains fixed;
- required evidence and effects bind that exact Revision;
- publication uses the production effect CLI, never handwritten evidence or
  manual publication pushes;
- case-specific compare-and-set failures come from a real concurrent writer
  during the actual CLI attempt;
- oracle receipts are not exempt from publication-attempt or refusal checks.

The grader does not prescribe the agent's reasoning order, investigation depth,
or moment-to-moment phases. Turn count, token count, elapsed time, repeated
reads, and work after a provisional decision are diagnostics, not failures.

### Model and human graders

Model grading is split into three focused dimension judges —
`correctness`/defect detection, `evidence`/actionability, and
`scope`/overengineering — each of which returns a structured boolean score plus
`null` (Unknown) when the recorded evidence is insufficient. A trial passes the
Judge only when every dimension scores true; any false score fails it.
Deterministic graders remain authoritative and a model judge never overrides a
failed deterministic grader.

Ambiguous (Unknown), high-risk, and code-review cases escalate to a read-only
agentic forensic judge that inspects the recorded artifact bundle and the full
agent trajectory with `read`, `grep`, `find`, and `ls` only. It has no network
access, no mutation tool, and no hidden reference solution.

Human reviewers calibrate the dimension rubrics against a labeled calibration
bank (`evals/calibration.json`). `evals/scripts/calibrate_judge.py` records the
confusion matrix, per-dimension agreement, and judge version/model/prompt
fingerprint. A prompt or model change that does not match the bank's fingerprint
sets `regrade_required`, so stale human labels cannot silently certify a new
Judge. The bank is seeded from the deterministic reference outcomes and must
reach at least 40 trials, including human-labeled failing trials, before the
agreement gate is measured.

## Trace catalog and export

Langfuse export is post-run, idempotent, and fail-open. Git task/grader source
plus Harbor lock, results, artifacts, and scores remain authoritative. A
Langfuse outage, timeout, or retry never changes a Harbor reward or job exit.
Scheduled planning fails closed when history is unavailable because duplicate
rejection would otherwise be false; manual diagnostics can explicitly permit
an empty-history fallback.

Export identities are stable and derived from the Harbor job id plus trial id
plus candidate Forest Run id, with the attempt index appended for dataset-run
grouping. Retries upsert the same dataset item, run item, and scores. Provider
failures before a Forest trace still create a no-trace run item, so unavailable
contenders stay in the history.

Each run item records its experiment and configuration fingerprints, cohort,
selection reason, model, provider, thinking, tools, prompt/skill/task/evaluator
digests, outcome class, latency, token counts, and cost. The Harbor adapter
derives token and cost fields from retained Pi `message_end` usage records.
`evals/scripts/experiment_history.py` reads the Langfuse dataset-run catalog and
produces machine-readable JSON plus a quality/cost/latency summary artifact.
Harbor remains the repetition and result authority; Langfuse owns longitudinal
cataloging and dashboards.

The post-run exporter is `evals/scripts/langfuse_export.py`; the paired runner
invokes it for both cohorts without changing either Harbor job exit. The
optional `langfuse` dependency reads `LANGFUSE_PUBLIC_KEY`,
`LANGFUSE_SECRET_KEY`, and `LANGFUSE_BASE_URL`. Panel definitions live in
[`langfuse-dashboards.md`](langfuse-dashboards.md). Production-trace intake,
promotion, and reporting remain separate, equally fail-open steps in
`evals/scripts/production_flywheel.py`; see
[`production-flywheel.md`](production-flywheel.md).

## Suites

Six design categories partition evaluation work; not all are executable suites:

- **eval-integrity** — task contracts, grader source, and grading authority are
  themselves tested; deterministic safety graders must catch forbidden
  behavior.
- **regression** — the frozen shipped-role publication and race cases below;
  `pass^3`.
- **capability** — broader role capabilities; `pass@1` plus per-case confidence
  intervals, never hidden behind `pass@k`.
- **whole-Forest** — end-to-end multi-role scenarios.
- **adversarial/security** — planted defects, forbidden behavior,
  credential-shaped content, and prompt-injection counterexamples.
- **production replay** — production-derived traces replayed through Harbor and
  cataloged in Langfuse.

The executable regression corpus contains 25 stable case identifiers:

- Builder: explicit requested work, absent authorization despite an available
  issue, ignored historical notes, unrelated evidence and branch races, failed
  Checks, and subject/label/branch-prefix scope.
- Verifier: stale revision, planted defect despite passing Checks, clean
  approval, failed Check, conflicting Verdict, and concurrent primary advance.
- Fixer: one or multiple findings, ignored historical notes, conflicting
  request or branch, and no requested rejected Revision.
- Critic/Tester: requested read-only findings with no code publication or
  tracker creation. Their old `draft-only` identifiers do not authorize drafts.

### Verifier boundaries

The role corpus exercises successful approval and truthful `changes`, including
failed Checks and stale ancestry. Concurrent-writer fixtures must preserve the
winning evidence/primary ref and reject partial publication. Grader unit
regressions also reject missing, malformed, wrongly bound, or wrongly authored
evidence and prove retired notes cannot satisfy current authority.

### Fixer

Fixer cases repair only the explicitly requested rejected Revision, execute the
declared verification, publish a fresh request through the CLI, and preserve
the old request/Checks/Verdict. Unrelated tracker leases remain unchanged.

### Whole Forest

The executable bounded journey is `evals/runtime/journey.py`. Its six real Runs
cover Builder publication, Verifier rejection, Fixer repair, cancellation of a
live Verifier before publication, fresh Verifier approval, and identical retry.
It records the built Forest/Pi/scanner versions, immutable image/Kernel identity,
build revision and dirty state, Ledger outcomes, retained logs, ref snapshots,
cancellation response, native source hashes and sizes, post-GC survival,
explicit fixture disposal, and delivered revision.

This is not an implementation of the entire historical whole-Forest scenario
outline. Concurrent multi-role scheduling, service loss at every Poll/effect/
cleanup boundary, long-running model continuity, and hostile process isolation
need their own evidence before being claimed.

## Historical design outline

The following names came from the earlier eval work plan. They are design
context, not an active backlog or authorization to create tracker work:

- **eval-integrity** — add a deterministic forbidden-behavior safety grader and
  split the monolithic Judge into calibrated per-rubric model graders.
- **capability** — build the capability case inventory and report `pass@1` with
  per-case confidence intervals. The machine-readable inventory lives at
  `evals/capability-inventory.json`; each entry pairs a passing reference with
  a counterexample and records task class, source, and source incident.
- **whole-Forest** — implement the whole-Forest scenario set and system
  graders.
- **adversarial/security** — add planted-defect, credential-shaped, and
  prompt-injection counterexamples.
- **production replay** — Langfuse export, production trace intake, daily
  self-host scheduling, and coverage reporting are implemented. Human
  annotation still controls promotion into `evals/production-cases.json`; the
  remaining work is adding each verified production-derived case and its
  paired counterexample.

## Historical harness alternatives

The earlier actor-assignment study considered native Git, constrained tools,
and Kernel-owned effects. The current contract is agent judgment plus the
production publication CLI (ADRs 0021/0022). Alternatives are not implemented
variants or an instruction to run a paid comparison.

### Retired prompt plus native Git

Notes-aware prompts did not enforce atomic publication or prevent raw Git
plumbing. This is not a valid oracle path for the current protocol.

### Agent decision plus constrained effect tools

Keep selection and review with the model. Remove native `bash` from the
Verifier and provide narrow tools through a Kernel-supplied Pi extension:

- `review_input` returns the explicitly supplied exact Revision, changed paths,
  base metadata, and validated evidence identity;
- `run_checks` executes the reviewed Revision's declared commands and returns
  structured results;
- `publish_verdict` accepts the exact Revision and structured decision and
  delegates schema, actor, write-once, and atomic publication enforcement to the
  current Kernel CLI.

The worktree remains available through read-only source tools. The model cannot
call Git plumbing through these interfaces. This preserves agent ownership of
judgment while making publication deterministic and transactional. It is the
recommended design if it beats native Git on the actor-assignment eval.

Production declarations do not supply these proposed extensions. The no-model
oracle wrapper explicitly loads one trusted input hook only for evaluation;
it is neither ambient discovery nor a production per-agent capability API.

### Kernel-owned effect

The model returns a structured decision and the Kernel validates and publishes
it. This gives the strongest enforcement and simplest agent prompt, but moves
workflow policy into the Kernel and reverses ADR 0010's provisional boundary.
Adopt it only if constrained tools still fail the regression suite or cannot
prove the Gate invariants.

## Model and prompt experiments

Run the same frozen cases across candidate model, thinking-level, tool-set, and
prompt combinations. Compare outcome pass rate first, then review-quality
grades, then diagnostic token and latency distributions. Do not select a model
from one production trace. A stronger model, clearer acceptance criteria, and
the right tools may remove the failure without adding orchestration.

Start with the smallest prompt that states authority, inputs, acceptance
criteria, publication invariants, and the definition of done. Treat
phase-structured prompts, a separate effect executor, and durable progress state
as experimental variants, not defaults. Adopt one only when repeated frozen
trials show that the less restrictive harness is unreliable. Context compaction
may trigger an alert or a new context, but never an automatic wall-clock kill.

## Adoption gate

Promotion scope follows the changed boundary instead of charging every change
the same 69-trial tax:

- prompt, skill, model, or thinking changes run the affected role at `pass^3`
  plus cross-role sentinel cases;
- shared Kernel, tool, grader, task-harness, or protocol changes run the full
  regression suite at `pass^3`;
- every promotion requires all deterministic safety grades, no incomplete or
  infrastructure outcome, and no regression against the paired incumbent;
- model and thinking changes require a statistically supported quality win or
  equal quality with at least 10% lower recorded cost or mean latency;
- production replay and whole-Forest cases join the gate when the changed
  boundary reaches them.

The nightly and weekly tiers discover regressions and contenders. Only the
promotion tier can change a production declaration. Langfuse scores never
promote a configuration. Read failed transcripts before accepting a result. A
change to an agent/tool/Kernel ownership boundary still requires an ADR under
ADR 0017.

Research basis: Anthropic's [agent eval guide](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents),
Anthropic's [long-running agent harness report](https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents),
and OpenAI's [agent eval](https://developers.openai.com/api/docs/guides/agent-evals)
and [trace grading](https://developers.openai.com/api/docs/guides/trace-grading)
guides. They converge on production-derived tasks, isolated repeated trials,
outcome-first deterministic grading, trace inspection, and separate regression
and capability suites.
