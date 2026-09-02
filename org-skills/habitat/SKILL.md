---
name: habitat-work-source
description: >
  Consume the frozen Habitat CLI v0.5.4 work-source contract for a repository
  profile. Use only the documented list/detail/claim/status/terminal operations;
  Habitat has no comment operation.
---

# Habitat work source

Read the frozen contract first: `./CONTRACT.md` (same directory). This runbook
tells each role which operations it may perform and how to verify the redacted
fake-CLI contract cases without touching a live R90 ledger.

## Boundary

- Use only the five profile operations: list, detail/read, claim/brake,
  status/report, terminal close.
- Never read or mutate a live R90 Habitat ledger. Use redacted fixtures or a
  local fake server with a dummy token for verification.
- Habitat CLI v0.5.4 has no comment/note operation. Do not invent a command,
  flag, or fallback.
- Native external IDs are opaque and unchanged. Repository binding is
  profile-owned; explicit mapping wins over fallback; missing mapping is
  ineligible. Null eligibility is ineligible.

## Credential and executable lookup

Environment names: `HABITAT_API_TOKEN` (preferred complete bearer token),
`HABITAT_API_KEY` (`habitat:<key>`), `HABITAT_API_URL`, `APOLLO_API_KEY`,
`APOLLO_API_URL`.

Resolve `habitat` from a trusted `PATH`. Record the executable name and version
(`habitat-cli-v0.5.4`), never secret values and never an organization-specific
install path.

## Role capability matrix

| Operation | Command | Builder | Fixer | Verifier | Critic | Tester | Executive/operator |
| --- | --- | --- | --- | --- | --- | --- | --- |
| List eligible work | `habitat list [filters] [--limit N --offset N] --json` | yes | yes | yes | yes | yes | yes |
| Detail/read | `habitat get <id> --json` | yes | yes | yes | yes | yes | yes |
| Claim | `habitat transition <id> in_progress --expected-revision N --json` | yes | yes | no | no | no | yes |
| Brake | `habitat transition <id> blocked --expected-revision N --json` | yes | yes | no | no | no | yes |
| Status/report | `habitat get <id> --json` or `habitat list --status ... --json` | yes | yes | yes | yes | yes | yes |
| Terminal close | `habitat transition <id> done --expected-revision N --json` or `habitat delete <id> --expected-revision N --json` | no | no | no | no | no | yes |

No role has a comment/note capability. Review and QA roles are read-only.

## Work-source rules

1. **Bounded listing.** Always set `--limit` and a maximum page count. Follow
   pages by increasing `--offset` until `items.length < limit` or
   `offset >= count`.
2. **Null eligibility is ineligible.** Missing or null eligibility is never
   promoted to eligible.
3. **Explicit repository wins.** Resolve repository through an explicit
   source-specific map (module, epic, tag, or Poll identity). A configured
   fallback never overrides the explicit map.
4. **Native ids stay unchanged.** Pass the external ID through verbatim; do not
   rewrite, namespace, or co-query across sources.
5. **Terminal ownership.** Only an executive/operator terminal-closes. Builders
   and Fixers may claim/brake/status but not close.

## Verify the contract cases

The redacted fake-CLI fixtures live in `./fixtures/`. Run the offline case
runner:

```sh
./run-contract-cases.sh
```

This exercises eligible, null-eligibility, foreign-repository,
missing-repository, malformed, auth, not-found, and at-least-two-page cases
through the fake CLI without a network or live ledger.
