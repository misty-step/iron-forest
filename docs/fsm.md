# Delivery state machine

Iron Forest delivers one Subject (a Tracker item, or the branch behind it) as a
**durable machine**, not a chat habit. Three Flows — Builder, Verifier, Fixer —
work the Subject on their own clocks and coordinate only through repository
facts. This document is the single source of truth for what a Flow may do. It
names the states, the legal transitions, the Flow that owns each transition,
and the invariants that must never break.

The machine is also encoded as a pure function (`fsm.go`) with table-driven
tests (`fsm_test.go`). When the words here and the code disagree, the tests are
the referee: `go test -run Transit ./...` must pass.

## How a state is derived

A state is derived **only from git-visible facts**. Iron Forest never reads a
Host's review, check, or merge decision as factory state. The facts are:

- a `forest/<id>-<slug>` branch on `origin`;
- a **Verdict** note on the exact Revision (`refs/notes/forest/verdict`);
- a **Checks** note on the exact Revision (`refs/notes/forest/checks`);
- the attempt ref `refs/forest/attempt/branch-<branch>`;
- the Tracker's `forest:failed` label.

The transient states *building* and *fixing* are the exception: they exist only
while a Run holds the Subject in-process (`inFlight` in `flow.go`), so a new
daemon cannot observe them and, importantly, **a new commit can never inherit a
Verdict or Checks note** — every publish lands a branch head with no notes.

`observe` (`fsm.go`) maps a snapshot of facts to the durable resting state.

## States

| State | Meaning | Derived when |
| --- | --- | --- |
| `eligible` | Tracker item not yet started | open item, no forest branch |
| `building` | Builder Run in flight (transient) | item claimed in-process |
| `pushed` | branch exists, no notes on head | forest branch, no Verdict/Checks |
| `checks_recorded` | Checks note on the exact Revision | Checks note on head |
| `verdict_approved` | independent Verdict `approve` on the exact Revision | Verdict note + green Checks on head |
| `verdict_rejected` | independent Verdict `changes` on the exact Revision | Verdict note + green Checks on head |
| `fixing` | Fixer Run in flight (transient) | branch claimed in-process |
| `merged` | landed on the protected target | item closed, branch retired |
| `failed` | halted for a human | attempt cap reached or `forest:failed` |

## Legal transitions

Each arrow is an **Effect** — one durable move a Flow makes. The Flow that owns
the arrow is shown in parentheses. `transit` (`fsm.go`) is the authority: an
arrow this table omits is illegal and is refused.

```
eligible --build(builder)--> building --publish(builder)--> pushed
pushed   --check(verifier)--> checks_recorded
checks_recorded --review(verifier)--> verdict_approved   (Checks == pass)
checks_recorded --review(verifier)--> verdict_rejected   (Checks == pass)
checks_recorded --fix(fixer)-----> fixing                (Checks == fail)
verdict_rejected --fix(fixer)----> fixing                (a rejected head has green Checks)
verdict_approved --merge(verifier)--> merged             (Checks == pass)
fixing  --publish(builder/fixer)--> pushed               (repair lands a bare head)
building --publish(builder)------> pushed
<any working state> --fail(fixer/human)--> failed         (attempt cap; operator halt)
```

`failed` and `merged` are terminal. No Effect leaves them. A new head after a
repair is a bare `pushed` head — nothing is inherited from the old one.

## Which Flow owns each transition

| Effect | Flow | Code path |
| --- | --- | --- |
| `build` | Builder | `flow_builder.go` `Act` (`createWorktree`, `runPhase`, `gate`) |
| `publish` | Builder (also Fixer) | `flow_builder.go` `commitAndPush`; `flow_fixer.go` `commitAndPush` |
| `check` | Verifier | `flow_verifier.go` `Act` (`runChecks`, `notes.go` `writeChecks`) |
| `review` | Verifier | `flow_verifier.go` `verifierReview` (`notes.go` `writeVerdict`) |
| `merge` | Verifier | `flow_verifier.go` `mergeVerified`/`finishMerge` |
| `fix` | Fixer | `flow_fixer.go` `Act` (`createWorktreeAtBranch`, `runPhase`, `gate`) |
| `fail` | Fixer / human | `flow_fixer.go` `markFixerFailed`; Tracker label `forest:failed` |

The Selectors are the read side; they only ever offer a Subject whose state
makes the Flow's Effect legal:

- `flow_builder.go` `Select` → `eligibleItems` offers `eligible` Subjects.
- `flow_verifier.go` `Select` reads Verdict/Checks notes:
  - no Verdict + no failing Checks → `pushed`/`checks_recorded` (check/review path);
  - Verdict `approve` + Checks `pass` → merge path (subject to `mergeBlocked`).
- `flow_fixer.go` `Select` reads Verdict/Checks notes, offering only broken
  heads (`verdict == changes` or `checks == fail`) below the attempt ceiling.

## Halt and human-only states

- `failed` is reached by the Fixer when `flows.fixer.attempts` is exhausted, or
  when an operator applies the `forest:failed` label. The machine records the
  halt but never resumes a `failed` Subject on its own; a human clears the state.
- `mergeBlocked` (`flow_verifier.go`) can hold an approved, green branch at
  `verdict_approved`: when `auto_merge` is off, or merge attempts are
  exhausted, the branch waits for an operator and is never hot-looped.

## Invariants

1. **Never merge without Checks and an approved Verdict on the exact Revision.**
   `transit` refuses `merge` from every state but `verdict_approved`, and refuses
   it there too unless the Checks on that head are `pass`. The Verifier Act calls
   `admitMerge` (`flow_verifier.go`) at the merge boundary, which reads both
   notes on the exact head and asks the machine, so a flow that ever skips a
   required note is refused there.
2. **Never double-claim one Subject across concurrent Flows.** `transit`
   refuses a second `build`/`fix` once the Subject is `building`/`fixing`, and
   `flow.go` `inFlight.claim` excludes the Subject within one process. `transit`
   also enforces ownership: each Effect names the Flow that owns it (`owns`),
   and a lane that attempts another lane's Effect is refused.
3. **Fix attempts respect the configured cap.** `flow_fixer.go` reads
   `flows.fixer.attempts`; on exhaustion `markFixerFailed` labels the item for a
   human (`failed`). `transit` only reaches `failed` through `fail`.
4. **Gate rejects a change to a protected path.** `gate.go` `gate` refuses any
   run whose change touches `.forest/`, `forest.yaml`, `agents/`, or
   `.opencode/opencode.json` — the factory's control plane — before trusting
   anything the report claims. It also requires no commit, a real change, and a
   `report.json` that satisfies the agent's declared schema; `gateReview`
   requires a valid Verdict. (See ADR 0004, which supersedes the earlier
   ADR 0003 rejection of protected paths.)
5. **A new commit has no inherited Verdict or Checks.** Every `publish` lands a
   bare `pushed` head, so no staleness comparison is needed and none is made.

## FSM semantics

`transit(from, effect, facts, outcome, actor)` is the pure authority a Flow
calls at each Effect boundary.

- `outcome` is the Effect's own recorded decision. A `review` writes the Verdict,
  so its legality depends only on what must already be true — green Checks on the
  exact Revision — and `outcome` (`approve`/`changes`) names the new state. It
  never requires the Verdict to pre-exist before the Effect that writes it.
- `facts.revision` carries the exact Revision every note keys to. A `review` or
  `merge` without a revision cannot satisfy the exact-note invariant and is
  refused.
- `actor` is the Flow performing the Effect. `owns` refuses a lane that attempts
  another lane's Effect, except the Fixer's `publish` (it runs the Builder
  declaration) and the Fixer's `fail`.

`observe(facts)` derives the durable resting state. A failing head is always
repair work, so the Checks fact dominates the Verdict fact: a head whose Checks
fail is observed `checks_recorded` (the Fixer's work) and is never
`verdict_approved` or `verdict_rejected`.

## Keeping the vocabulary

The states and effects above use the accepted terms — Flow, Run, Phase, Subject,
Revision, Selector, Effect, Verdict, Checks, Gate, Ledger, Tracker, Host,
Runner, Projection, Builder, Verifier, Fixer. No new agent roles are added by
this work; extra quality is expressed as `checks:` and optional config stages.
