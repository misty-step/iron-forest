# Powder job spec template

Use this template when filing a Powder job so the spec carries everything a
Builder and Verifier need. Replace each placeholder and delete optional sections
rather than leaving placeholders in the filed job. Read the
[`forest:ready` contract](../forest-ready-contract.md) for the meaning of each
section.

## Blank template

```text
## Problem

<Observable defect or concrete need. Not the chosen fix.>

## Repro

<Exact steps that reproduce the current behavior, or the scenario to serve.>
1.
2.
Expected: <what should happen>
Observed:  <what actually happens today>

## Scope

In scope:
- <what this Subject may change>

Out of scope:
- <what this Subject must not change>

## Acceptance criteria

<One machine-checkable criterion per line.>
- <criterion> — run `<command>`, expect `<pass result>`

## Verification path

<The exact command(s) a Builder and Verifier run to prove the criteria.>

    <command>
    # exit: <expected code> / output: <expected result>

## Evidence

<Required when Problem describes a defect: cite the primary record you
observed — the evidence-ref payload, Ledger row, Run id with its log
line, or command output. Commit titles, timestamps, and recollection are
leads, not evidence. Optional for non-defect work: related jobs or links.>
```

## Condensed example

```text
## Problem

`forest audit show` reports one violation on the current master tip for
`refs/forest/v1/request/8ad7a5a...`: `unknown JSON object key`.

## Repro

1. Fetch a repository tip whose evidence sweep includes that ref.
2. Run `forest audit show`.
Observed: `violations: ["invalid evidence refs/forest/v1/request/8ad7a5a...:
unknown JSON object key"]`.
Expected: zero violations.

## Scope

In scope:
- Tolerate the known legacy v1 review-request payload in the Auditor decoder.

Out of scope:
- Deleting or rewriting existing evidence refs.
- Changing the v2 review-request schema.

## Acceptance criteria

- `forest audit show` reports pass with zero violations on the current tip —
  run `forest audit show --rescan`, expect `last_result: pass` and
  `violations: []`.
- A v2 payload with a genuinely unknown key still records a violation — run the
  Auditor against a fixture with an unknown key, expect one violation.

## Verification path

mise exec -- go test ./...
# exit: 0

forest audit show --rescan
# expected: last_result=pass, violations=[]
```