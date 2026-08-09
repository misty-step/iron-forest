# 0005 — Agent declarations own commit identities

Status: accepted, 2026-08-08

## Context

Iron Forest used one `forest.yaml` commit identity for every Flow. The Ledger named the acting agent, but `git log` did not. Work could not be attributed to the Builder, Fixer agent, or Verifier from repository history alone.

Git has two separate identity layers. Commit author name and email are commit data. The Host account that pushes a branch or opens a pull request is an authenticated actor. Changing one does not change the other.

## Decision

Require `commit.name` and `commit.email` in every `agents/<name>/agent.yaml` declaration. Remove the global commit identity from `forest.yaml`.

Builder and Fixer branch commits use the acting agent's declared identity. A Verifier rebase preserves each author and uses the Verifier as committer. A native squash commit uses the Verifier identity. Native fast-forward merges preserve existing commits. The Host path supports squash only because the Host API has no fast-forward-only operation.

`forest agents` shows the declared commit author. The definition digest covers `agent.yaml`. Each agent Run records that digest, and retirement facts retain the initiating Verifier attribution across restarts.

Do not imply that commit authorship creates a Host identity. Pull requests and pushes remain attributed to the credential that performs the Host Effect. A deployment that needs separate Host actors must provision accounts or application credentials separately.

## Consequences

`git log` and pull-request commit lists show which declared agent produced each commit without extra Host accounts.

Repositories must add commit identities to every agent declaration. A missing name or email is a configuration error.

Distinct Host attribution remains an optional deployment concern. It does not block per-agent commit traceability.
