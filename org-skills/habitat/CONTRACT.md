# Habitat CLI v0.5.4 profile contract

This document freezes the source-verified Habitat CLI contract that a
repository-owned work-source profile may consume. It covers the five profile
operations — list, detail/read, claim/brake, status/report, and terminal close —
and records the exact command surface, JSON fields, exit behavior, pagination,
error mapping, native-id grammar, repository binding, and null-eligibility rule.

It is a contract, not a live integration. Production Go code, a `tracker:
habitat` Kernel enum, a generic Tracker adapter, live R90-ledger reads, and R90
deployment are out of scope.

## Executable and version

| Fact | Value | Source |
| --- | --- | --- |
| Executable name | `habitat` | `habitat --help` |
| Version | `habitat-cli-v0.5.4` | `habitat --version` |
| Trusted lookup | `habitat` resolved from a trusted `PATH` | operator runbook |

The contract records the executable name and version only. It does not record an
organization-specific install path; a deployment supplies its own trusted
`PATH` entry or explicit executable path.

## Credential and server environment

Names only, never values:

| Name | Meaning |
| --- | --- |
| `HABITAT_API_TOKEN` | Complete bearer token (preferred) |
| `HABITAT_API_KEY` | Habitat API key, normalized to `habitat:<key>` |
| `HABITAT_API_URL` | Server URL (help text default `http://localhost:3000`) |
| `APOLLO_API_KEY` | Compatibility fallback key |
| `APOLLO_API_URL` | Compatibility fallback URL |

Observed transport form: `Authorization: Bearer <token>`, JSON content type.

## Global surface

- `habitat --version` — print `habitat-cli-v0.5.4`, exit 0.
- `habitat --help` — print the command list, revision flags, and environment names.
- `--json` — machine output on stdout for the commands that support it.
- `--expected-revision N` — required for `update`, `transition`, and `delete`.
  The value is an integer revision.

## Frozen profile operations

### 1. List — bounded eligible-item listing

Command:

```text
habitat list [filters] [--limit N] [--offset N] [--json]
```

Transport: `GET /api/work/items` with the provided filters as query parameters.

Observed query parameters and their flag forms:

| Flag | Query parameter |
| --- | --- |
| `--status <v>` | `status` |
| `--type <v>` | `type` |
| `--priority <v>` | `priority` |
| `--owner <v>` | `owner` |
| `--owner-id <uuid>` | `owner_id` |
| `--team <slug>` | `team` |
| `--sprint <slug>` | `sprint` |
| `--sprint-id <uuid>` | `sprint_id` |
| `--epic <external_id>` | `epic` |
| `--epic-id <uuid>` | `epic_id` |
| `--module <name>` | `module` |
| `--module-id <uuid>` | `module_id` |
| `--search <v>` | `search` |
| `--tags <a,b>` | `tags` |
| `--tag-match <any\|all>` | `tag_match` |
| `--complexity <v>` | `complexity` |
| `--risk <v>` | `risk` |
| `--size <v>` | `size` |
| `--limit <n>` | `limit` |
| `--offset <n>` | `offset` |

JSON envelope:

```json
{ "count": 0, "items": [] }
```

`count` is the total count. `items` is the current bounded page. Each item has
these fields: `external_id`, `revision`, `title`, `status`, `priority`, `size`,
`type`, `owner`, `sprint`, `epic`, `module`, `tags`, `pr_url`, `branch`,
`head_sha`, and normalized `blocked_by` external IDs.

Success exit is 0. HTTP failures exit non-zero with
`Error (HTTP <code>): <message>`.

### 2. Get — detail/read

Command:

```text
habitat get <EXTERNAL_ID> [--json]
```

Transport: `GET /api/work/items/<EXTERNAL_ID>`.

Returns the complete item object, including `pr_url`, `branch`, `head_sha`, and
normalized `blocked_by` external IDs, plus the item's current `revision` for the
next mutation.

A `404` maps to:

```text
Error: Work item '<EXTERNAL_ID>' not found.
```

exit 1. HTTP failures map to `Error (HTTP <code>): <message>`, exit 1.

### 3. Transition — claim, brake, and terminal status

Command:

```text
habitat transition <EXTERNAL_ID> <STATUS> [--expected-revision N] [--idempotency-key KEY] [--json]
```

Transport: `PATCH /api/work/items/<EXTERNAL_ID>` with body:

```json
{ "status": "<STATUS>", "expected_revision": <int> }
```

`--expected-revision` is required; omitting it exits 1 with the usage error.

Supported statuses:

```text
unscheduled, refining, ready_for_dev, in_progress, blocked, verification, done
```

Profile mapping:

- claim = `transition <id> in_progress`
- brake = `transition <id> blocked`
- terminal close = `transition <id> done`

### 4. Delete — soft-delete terminal close

Command:

```text
habitat delete <EXTERNAL_ID> [--expected-revision N] [--idempotency-key KEY] [--json]
```

Transport: `DELETE /api/work/items/<EXTERNAL_ID>` with body:

```json
{ "external_id": "<EXTERNAL_ID>", "expected_revision": <int> }
```

`--expected-revision` is required; omitting it exits 1 with the usage error.

### 5. Status/report

There is no dedicated status command. A profile reads status through:

- `habitat get <EXTERNAL_ID> --json` (single item), or
- `habitat list --status <status> [--limit N --offset N] --json` (bounded report).

## Pagination

`list` is bounded by `--limit`/`--offset`. The profile follows pages by
increasing `offset` by `limit` until the returned `items.length` is less than
`limit` or `offset >= count`. A bounded maximum page count is required; do not
loop without a limit.

`run-links` (out of scope for the five profile operations) uses its own bounded
`limit`/`offset` and returns `count`, `has_more`, and `next_offset`.

## Error behavior

| Condition | Observable | Exit |
| --- | --- | --- |
| CLI usage/validation error | `Error: <message>` on stderr | 1 |
| HTTP 401 | `Error (HTTP 401): <message>` plus `Run: habitat configure to update the API key.` | 1 |
| HTTP 403 | `Error (HTTP 403): <message>` | 1 |
| HTTP 404 (`get`) | `Error: Work item '<id>' not found.` | 1 |
| HTTP other | `Error (HTTP <code>): <message>` | 1 |
| Malformed JSON body with HTTP 200 | `Error (HTTP 200): Invalid JSON response from server.` plus a `Request ID:` line | 1 |

## Native-id grammar

`<EXTERNAL_ID>` is a positional, nonempty, opaque string. It is passed verbatim
in the URL path and mutation body. The CLI performs no client-side validation
beyond nonempty. A profile must preserve native ids unchanged and never
rewrite, namespace, or co-query them across sources.

## Repository binding

Habitat v0.5.4 work items have no `repository` field, and `list` rejects both
`--repo` and `--repository` as unknown flags. Repository binding is therefore
profile-owned: a Poll or profile map resolves an item to a repository through an
explicit, source-specific mapping (for example by module, epic, tag, or the
source Poll identity). An explicit mapping wins over any configured fallback. An
item with no resolvable mapping is ineligible.

## Null eligibility

Habitat v0.5.4 has no `eligibility` field in the work-item schema. A profile
derives eligibility. Null or missing eligibility is ineligible and must never be
treated as eligible.

## Comment/note operation

v0.5.4 exposes no comment or note operation.

- `habitat comment <id> <text>` → `Error: Unknown command 'comment'.` exit 1.
- `habitat update <id> --comment x` → `Error: Unknown flag '--comment'.` exit 1.

This is a Habitat CLI capability gap, not a missing Iron Forest Kernel feature.
The contract documents the absence and invents no command, flag, or fallback.

## Out-of-profile commands present in v0.5.4

`create`, `update`, `history`, `link_run`, `run-links`, `filters`, `people`,
`debt`, `gov`, `scope`, `prd`, `credentials`, `cycle`, `sprint`, `capacity`,
`pods`, `verify`, and `configure` exist in v0.5.4 but are not part of this
five-operation profile contract.

## Source evidence

- `habitat --version` output: `habitat-cli-v0.5.4`.
- `habitat --help` output: command list, environment names, and revision flags.
- Subcommand usage strings for `get`, `create`, `update`, `transition`,
  `delete`, `link_run`, and `run-links`.
- Local throwaway HTTP mock probes with a dummy token (no live ledger): request
  methods, paths, query parameters, request bodies, and exit/message mapping for
  200/401/403/404/500 and malformed JSON.
- v0.5.4 embedded tool description text for `list`, `get`, `history`, and
  `run_links`, which names the list envelope and item fields and the
  `run_links` pagination fields.
