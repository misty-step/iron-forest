# Iron Forest

Headless software factory. One `forest` Kernel serves this checkout.
`README.md` is the current operator entrypoint and accepted ADRs state
technical contracts.

## Work selection

Work from the operator's current request. Check current code and existing work
before starting, state ownership when agents overlap, and report the result
with verification evidence. Old tickets and factory records are context, not
authorization to start work. Do not maintain a replacement backlog.
Linear owns current non-R90 work and selected unresolved opportunities; it is
not an automatic intake queue. Do not duplicate it in repo task lists. R90
continues to use Habitat.

## Evidence

Anchor defects to the evidence-ref payload, Ledger row, Run id and log line, or
command output actually read. Commit titles, timestamps, and recollection are
leads only.

For candidate verification, use the existing
[`iron-forest` skill](org-skills/iron-forest/SKILL.md) and its
[verification reference](org-skills/iron-forest/verification.md). It preserves
the repository's deterministic checks and real Forest/Pi journey; operator
skills require explicit composition, not ambient Runner discovery.

## Operations

- Adopt a merged revision with
  `deploy/install-service.sh update <instance>`. For a sibling checkout, the
  factory owner adopts the exact factory revision here first, then the sibling
  owner runs `deploy/install-service.sh update <instance> <factory-sha>`.
  The script requires a clean, exact factory revision before stopping the
  consumer unit, drains live Runs, fast-forwards and rebuilds the selected
  checkout, runs `./.iron-forest/bin/forest selfcheck`, verifies `build_sha`, forces
  `./.iron-forest/bin/forest audit show --rescan`, restarts, and verifies the unit is active.
  The factory checkout is never mutated; a restart alone is not an update.
- Keep exactly one Kernel checkout per repository (ADR 0015).
- This manager owns only repo `misty-step/iron-forest`, root
  `/home/phaedrus/Development/misty-step/iron-forest`, and unit
  `forest@iron-forest`. Other repositories and `forest@*` units belong to their
  owners. Accept their field reports and inspect cited artifacts; their
  binaries, status, systemd units, Runs, leases, deployments, and checkouts
  stay under their owners' control.
- Fleet design may aggregate read-only reported metadata without transferring
  operational ownership. Execute judgment calls inside this mandate and report
  them; escalate scope, spend, or risk changes.
