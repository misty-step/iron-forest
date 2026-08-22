# Iron Forest

Headless software factory. One `forest` Kernel serves this repository;
agents build, review, and merge tracked work. `VISION.md` is the product
lock; `README.md` is the operator manual; accepted ADRs are contracts.

## Tracker

Powder is this repository's tracker of record (jobs `if-*`, repo
`misty-step/iron-forest`). GitHub Issues remain supported by the product
but are not used here. File work with
`docs/templates/powder-job-spec.md`.

## Findings

Defect reports cite primary records: the evidence-ref payload, Ledger
row, Run id with its log line, or command output actually read. Commit
titles, timestamps, and recollection are leads, not findings.

## Operations

- Adopt merged revisions through the fence procedure: idle window ->
  stop service -> confirm inactive -> clean-tree check -> pull ->
  rebuild -> selfcheck -> start -> verify active -> observe an audit
  pass. Never restart-only: the unit runs the checkout-local binary.
- One Kernel checkout per repository (ADR 0015). Do not start a second.
- Judgment calls inside the operations mandate are executed and
  reported, not escalated. Escalate scope, spend, or risk changes.

## Commands

    mise exec -- go build ./... && mise exec -- go vet ./... && mise exec -- go test ./...
    ./evals/run-fast.sh   # before touching any declaration