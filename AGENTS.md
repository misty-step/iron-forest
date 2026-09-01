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

- Adopt merged revisions with `deploy/install-service.sh update <instance>`,
  the one fenced adoption procedure. It checks a clean tree, stops the service
  (stops new dispatches and drains live Runs), confirms the instance is
  inactive, fast-forwards the checkout to the remote primary, rebuilds, runs
  `./forest selfcheck`, forces a fresh audit with
  `./forest audit show --rescan`, restarts the service, and verifies it is
  active. Never restart-only: the unit runs the checkout-local binary.
- One Kernel checkout per repository (ADR 0015). Do not start a second.
- The Iron Forest manager for this checkout owns only repo
  `misty-step/iron-forest`, root
  `/home/phaedrus/Development/misty-step/iron-forest`, and unit
  `forest@iron-forest`. Other repositories and `forest@*` units are
  consumer-owned. Accept field reports and inspect cited artifacts only;
  never run their binaries/status, operate systemd, cancel Runs, alter
  leases, deploy, or mutate their checkouts without an explicit request
  from that repository's owner.
- Fleet product design may aggregate read-only reported metadata; it does
  not transfer operational ownership.
- Judgment calls inside the operations mandate are executed and
  reported, not escalated. Escalate scope, spend, or risk changes.

## Commands

    mise exec -- go build ./... && mise exec -- go vet ./... && mise exec -- go test ./...
    ./evals/run-fast.sh   # before touching any declaration