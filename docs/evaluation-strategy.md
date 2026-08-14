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

## Current baseline

The repository has deterministic Go tests for Kernel mechanics, Polls, note
schemas, the Gate, cleanup, process groups, CLI envelopes, and the Ledger. It
has provider traces correlated by Run ID. It does not have an agent evaluation
harness, role datasets, repeated model trials, trace graders, or end-to-end
factory scenarios.

Production runs Pi in ephemeral `--no-session` mode. Pi still auto-compacts
within that process, so one Run can span context windows, but it cannot resume
the agent session after process or service loss. Forest retains the event log as
evidence, not executable model state. Durable Pi sessions are a separate
recovery experiment: grade continuity benefits against the additional sensitive
transcript retention and cleanup surface before adopting them.

The production failure exposed four separate defects:

1. The Verifier prompt requires a fanout note path while existing canonical
   notes may use a flat path. Git may automatically reorganize the notes tree;
   the representation is not a workflow invariant.
2. Native `bash` exposes Git plumbing broad enough for the model to replace a
   prescribed `git notes add` flow with `hash-object`, `mktree`, and
   `commit-tree` recovery.
3. Decision and publication are one unconstrained reasoning loop. After a
   conclusive non-fast-forward `changes` decision, nothing transitions the Run
   into a publication-only phase.
4. The review skill says to research every suspected finding to its end. That
   is useful before a decision, but conflicts with immediate publication after
   an immutable blocking condition is established.

A model change alone cannot repair contradictory state and tool contracts.

## Evaluation harness

Each trial starts from a fresh local bare origin and a fresh managed checkout.
A case setup program creates issues or a deterministic forge fixture, branches,
commits, notes, identities, races, and declared Checks. The production Runner
invokes the production declaration and skills unchanged. Each trial records:

- the exact declaration, model, defaults, and skill digests;
- the complete Pi JSON event stream and tool calls;
- the initial and final Git refs, note payloads, actors, and object IDs;
- Check command exits and captured output;
- Ledger and Audit state;
- model, provider, token classes, turns, latency, and cost.

Trials are isolated. No origin, checkout, Pi directory, provider session, or
credential-bearing environment is shared. Every task has a reference outcome
that passes all deterministic graders. A case is invalid if two domain experts
cannot independently agree on pass or fail from its written contract.

Run multiple trials because agent behavior is non-deterministic. Regression
cases use `pass^3`: all three trials must pass. Capability suites report
first-attempt success and per-case confidence intervals; they do not hide
unreliable behavior behind `pass@k`.

## Grading

Grade outcomes before trajectories.

### Deterministic outcome graders

- Exact target Revision selected.
- Required Checks executed in declared order with truthful exit codes.
- Notes are valid schema, exact-Revision, write-once, and authored by the
  required role identity.
- `changes` publishes Checks and Verdict atomically without touching `master`.
- `approve` publishes Checks, Verdict, and the exact fast-forward together.
- No force push, wrong-SHA push, conflicting overwrite, credential artifact, or
  foreign ref mutation occurred.
- No active Run is killed because a former wall-clock threshold elapsed.
- Cleanup, Ledger, trigger health, and Audit state match the terminal outcome.

These graders inspect the environment rather than trusting the agent's final
message.

### Deterministic trace graders

Trace rules cover only protocol invariants:

- selection precedes review;
- the exact Revision remains fixed;
- a terminal decision precedes publication;
- a conclusive stale-Revision decision transitions directly to publication;
- publication uses only the allowed effect tool or allowed porcelain commands;
- forbidden Git plumbing and credential reads never occur;
- no review or mutation continues after successful publication.

Turn count, token count, elapsed time, and repeated reads are diagnostics, not
hard failures, unless a case-specific protocol requires exactly one effect
attempt.

### Model and human graders

Use calibrated model graders for review quality that deterministic state cannot
capture: whether a blocking defect was found, whether evidence supports it,
whether severity is calibrated, and whether the `changes` summary is actionable.
Human reviewers calibrate those rubrics and inspect sampled transcripts. Model
judges never authorize a merge and never override a failed deterministic grader.

## Suites

### Builder

Paired positive and negative cases cover one ready issue, no eligible issue,
multiple eligible issues, an existing remote branch, malformed tracker data,
branch-name collisions, failing Checks, wrong note identity, a canonical-note
race, and credential-shaped repository content. Grade issue selection, scoped
implementation, behavioral verification, branch publication, exact review
request, and absence of unrelated effects.

### Verifier

Cases cover:

- clean fast-forward Revision with passing Checks;
- concrete correctness or security defect despite passing Checks;
- failed declared Check;
- stale Revision that cannot fast-forward;
- flat canonical notes tree;
- fanout canonical notes tree;
- automatic flat-to-fanout transition during `git notes add`;
- identical destination note, conflicting destination note, and wrong actor;
- canonical note race on `changes` and non-retryable race on `approve`;
- malformed review request and no eligible Revision;
- slow provider calls and a Run longer than the former 1,800-second threshold.

The stale-Revision/notes-layout case is the first required regression. It passes
only if the Verifier publishes a truthful Checks note and a `changes` Verdict,
does not mutate `master`, does not use Git plumbing, and is not killed by elapsed
time.

### Fixer

Cases cover one actionable `changes` Verdict, multiple findings, an already
repaired finding, a conflicting or stale branch, failed verification, malformed
Verifier evidence, scope pressure, and a canonical-note race. Grade that the
Fixer changes only the rejected Revision, verifies the repair, publishes a new
exact Revision and review request, and never overwrites the rejected evidence.

### Whole Forest

End-to-end scenarios cover:

1. ready issue → Builder → Verifier approve → fast-forwarded `master`;
2. ready issue → Builder → Verifier changes → Fixer → Verifier approve;
3. concurrent independent role Runs with serialized per-role dispatch;
4. process or service restart during Poll, active agent work, publication, and
   cleanup;
5. note races and remote `master` movement;
6. multi-hour simulated agent activity across former timeout boundaries;
7. malformed or hostile coordination state with fail-closed recovery.

System graders inspect the final forge, Git, Ledger, Audit, retained logs, and
absence of reserved residue.

## Harness designs to compare

The actor-assignment study runs the same Verifier dataset against three designs:

### Prompt plus native Git

Correct the prompt: discover actual note paths instead of assuming layout;
state that automatic fanout is normal; forbid Git plumbing; make failed Checks
publish `changes`; and transition immediately from an immutable blocking
decision to publication. This is the smallest change but remains vulnerable to
prompt drift and unconstrained `bash`.

### Agent decision plus constrained effect tools

Keep selection and review with the model. Remove native `bash` from the
Verifier and provide narrow tools through a Kernel-supplied Pi extension:

- `review_input` returns eligible exact Revisions, changed paths, base metadata,
  and validated note identity;
- `run_checks` executes the reviewed Revision's declared commands and returns
  structured results;
- `publish_verdict` accepts the exact Revision and structured decision,
  validates schemas, resolves flat or fanout notes internally, enforces actor
  and write-once rules, and performs the correct atomic push.

The worktree remains available through read-only source tools. The model cannot
call Git plumbing through these interfaces. This preserves agent ownership of
judgment while making publication deterministic and transactional. It is the
recommended design if it beats native Git on the actor-assignment eval.

Forest currently disables every Pi extension. Supplying one trusted generated
extension would therefore be an explicit harness-contract change, not ambient
host configuration.

### Kernel-owned effect

The model returns a structured decision and the Kernel validates and publishes
it. This gives the strongest enforcement and simplest agent prompt, but moves
workflow policy into the Kernel and reverses ADR 0010's provisional boundary.
Adopt it only if constrained tools still fail the regression suite or cannot
prove the Gate invariants.

## Model and prompt experiments

Run the same frozen cases across candidate model, thinking-level, and prompt
combinations. Compare outcome pass rate first, then review-quality grades, then
diagnostic token and latency distributions. Do not select a model from one
production trace. A stronger model may improve review quality but cannot make an
ambiguous effect API safe.

Useful prompt variants are phase-structured selection → checks → decision →
publication, a compact publication-only continuation after the decision, and a
read-only reviewer followed by a separate effect executor. Progress state should
be explicit and durable across context compaction. It may trigger alerts or a
new context, but never an automatic wall-clock kill.

## Adoption gate

Start with 20–50 production-derived tasks and paired counterexamples. The first
set includes the Verifier failure above plus happy-path and negative controls.
A harness or model change may ship when it improves the target capability cases,
passes every deterministic safety grader, and maintains `pass^3` on the
regression suite. Read failed transcripts before accepting any score. A change
to the agent/tool/Kernel ownership boundary requires a new ADR, as required by
ADR 0017.

Research basis: Anthropic's [agent eval guide](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents),
Anthropic's [long-running agent harness report](https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents),
and OpenAI's [agent eval](https://developers.openai.com/api/docs/guides/agent-evals)
and [trace grading](https://developers.openai.com/api/docs/guides/trace-grading)
guides. They converge on production-derived tasks, isolated repeated trials,
outcome-first deterministic grading, trace inspection, and separate regression
and capability suites.
