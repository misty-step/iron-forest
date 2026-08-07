# Changelog

- 2026-08-07: Per-run factory artifacts no longer enter a managed repository's working tree. `renderMarkdown` and opencode's materialised `node_modules` move from the worktree's `.opencode/` into a per-run config root outside it, and opencode is pointed at that root with `XDG_CONFIG_HOME` set in the child environment. The provider configuration a real run actually uses (the factory project's own `.opencode/opencode.json`, falling back to the operator's global opencode config) is preserved there. Local project-config discovery is disabled for the managed worktree (`OPENCODE_DISABLE_PROJECT_CONFIG`), so a `.opencode/opencode.json` a repository ships is neither read nor written. A managed repository now needs no `.gitignore`, no filesystem-scanner exclusion, and no hook change to be worked by the factory, opencode installs its provider packages in the run root rather than the worktree, and `git add -A` can never stage the rendered declaration. The external-config harness is tested against opencode's real config/agent interface, including a worktree that already carries its own project config.
- 2026-08-07: Documented and enforced the delivery state machine as the single
  source of truth for what a Flow may do. `docs/fsm.md` lists the states derived
  from git-visible facts (eligible, building, pushed, checks_recorded,
  verdict_approved, verdict_rejected, fixing, merged, failed), the legal
  transitions, the Flow that owns each transition, and the invariants, mapped to
  the flow code. `fsm.go` encodes the machine as a pure `transit`/`observe`
  function and `fsm_test.go` pins both legal and illegal transitions in
  table-driven tests. The flows call the machine at their effect boundaries
  (the Verifier's `admitMerge` reads the exact Checks and Verdict notes before
  a merge, and `verifierReview` refuses to write a Verdict without green Checks),
  so a Flow that skips a required note or Gate fails its test. `transit` takes
  the effect's outcome (`review` names the Verdict), an exact Revision, and the
  acting Flow (`owns` refuses one lane another's Effect), and `observe` lets a
  failing-Checks head read as repair work. The Gate rejects a change to the
  factory's control plane (`.forest/`, `forest.yaml`, `agents/`,
  `.opencode/opencode.json`). No new agent roles were added.
- 2026-08-07: Item identity is now an opaque string end to end. `Subject.ID` and ledger rows carry the tracker's native id ("69" for GitHub, a Habitat id for another source); branches keep the `forest/<id>-<slug>` shape and the reverse lookup no longer assumes the id is an integer. Because the branch delimiter is the first `-`, an id that itself contains `-` is escaped (id `-` becomes `%2D`, `%` becomes `%25`) so any id round-trips; numeric and hyphen-free ids are unchanged. The ledger's `issue` field becomes a string: the loader tolerates the old integer shape so existing rows stay readable (clean cutover; old rows are accepted, new rows are written as opaque strings). Merge behavior, brake refs, and worktree naming are unchanged for numeric GitHub ids.
- 2026-08-06: Replaced the pull-request state machine, label-as-state ownership, and cost accounting. Three independent Flows coordinate through repository state, while Verdicts and checks are stored as git notes. The Builder publishes unreviewed branches; the Verifier reviews and merges them on its own clock.
