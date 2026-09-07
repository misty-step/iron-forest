## [0.0.13](https://github.com/misty-step/iron-forest/compare/v0.0.12...v0.0.13) (2026-09-04)


### Bug Fixes

* **cancel:** remove cancellation marker on failed stop ([4ebab98](https://github.com/misty-step/iron-forest/commit/4ebab9850705207c295e465242e5bb674a51b27b))

## [0.0.12](https://github.com/misty-step/iron-forest/compare/v0.0.11...v0.0.12) (2026-09-04)


### Bug Fixes

* **config:** reject colliding agent slugs ([827e08e](https://github.com/misty-step/iron-forest/commit/827e08ea62be1b43f7e1bbc533eccc2a2a9779eb))

## [0.0.11](https://github.com/misty-step/iron-forest/compare/v0.0.10...v0.0.11) (2026-09-04)


### Bug Fixes

* **evals:** preserve committed Powder claims ([897241e](https://github.com/misty-step/iron-forest/commit/897241ec439772c8ccf9ee973824c34cf511d318))

## [0.0.10](https://github.com/misty-step/iron-forest/compare/v0.0.9...v0.0.10) (2026-09-04)


### Bug Fixes

* **evals:** preserve Powder jobs atomically ([747064c](https://github.com/misty-step/iron-forest/commit/747064cf0b1f6ce7afe4abea7b0f2341f69d989f))

## [0.0.9](https://github.com/misty-step/iron-forest/compare/v0.0.8...v0.0.9) (2026-09-04)


### Bug Fixes

* **evals:** persist Powder claims atomically ([1f3c41b](https://github.com/misty-step/iron-forest/commit/1f3c41b6c98c30cdd9f12033ab58b093a6fa84e0))

## [0.0.8](https://github.com/misty-step/iron-forest/compare/v0.0.7...v0.0.8) (2026-09-04)


### Bug Fixes

* **doctor:** accept role-scoped OpenRouter keys without instance fallback ([80e3111](https://github.com/misty-step/iron-forest/commit/80e31112019855b2304d7be09d24214d2ed572f6))
* **evals:** harden Powder claim state ([29b8e97](https://github.com/misty-step/iron-forest/commit/29b8e970bdaa61b184b7c5e1a6d23025fcc5baf5))

## [0.0.7](https://github.com/misty-step/iron-forest/compare/v0.0.6...v0.0.7) (2026-09-04)


### Bug Fixes

* **runner:** strip sibling role-scoped OpenRouter keys from child environment ([9c3d2b6](https://github.com/misty-step/iron-forest/commit/9c3d2b6df9adda04ab0dfd86c960e7e1e7f7bdee))

## [0.0.6](https://github.com/misty-step/iron-forest/compare/v0.0.5...v0.0.6) (2026-09-04)


### Bug Fixes

* **powder:** enforce per-job claims in factory ([640cd13](https://github.com/misty-step/iron-forest/commit/640cd1319fe26535cc3e92299f2fb40374fdedc6))

## [0.0.5](https://github.com/misty-step/iron-forest/compare/v0.0.4...v0.0.5) (2026-09-04)


### Bug Fixes

* **builder:** gate held and takeable Powder selection on GitHub-only scope ([2f0e610](https://github.com/misty-step/iron-forest/commit/2f0e610229a0d4b2fe7ff0684ffd52329eef13ed))
* doctor redacts powder failure; status recent is [] ([55f214e](https://github.com/misty-step/iron-forest/commit/55f214ed3fea88c5e7cb448343533fd4e1dd5f53))
* **evals:** allow model/thinking promotion quality wins ([0400e37](https://github.com/misty-step/iron-forest/commit/0400e370d473552c0373e123168cfedea173fd35))
* **evals:** bind experiment fingerprints to tier ([c36aa91](https://github.com/misty-step/iron-forest/commit/c36aa91e399ba84dfbe9287b67ad9a4839362bf8))
* **evals:** bind production source digest and correct workflow docs ([6854920](https://github.com/misty-step/iron-forest/commit/6854920618fd706539fbea0b8ebbb46f7011a997))
* **evals:** gate experiment reports on planned case coverage ([5cfd77f](https://github.com/misty-step/iron-forest/commit/5cfd77f39d0bda74b69195cbd513575816a284a2))
* **evals:** gate monthly model eval to first Monday ([aa2160b](https://github.com/misty-step/iron-forest/commit/aa2160bdde8d93ff4c84f6d0e23b5c0e03b260eb))
* **evals:** restrict live-model workflow to schedule and manual dispatch ([8de942b](https://github.com/misty-step/iron-forest/commit/8de942b3283023ea2a999c88784519b983306650))
* export explicit GitHub-only label scope signal ([97fa2f7](https://github.com/misty-step/iron-forest/commit/97fa2f73dcb7497642cdc7e6f7096c93fa8cc2cb))
* **powder:** adopt per-job claim protocol ([61ea233](https://github.com/misty-step/iron-forest/commit/61ea233bc65049ff4541c0afd7780672e22a00b2))
* **publish:** keep remote policy rejection distinct from branch race ([5111fcf](https://github.com/misty-step/iron-forest/commit/5111fcfc54c15a4f5ccdd9caccfc7f1911882632))
* **runner:** fail closed on OpenRouter 402 until trigger reset ([6470eb4](https://github.com/misty-step/iron-forest/commit/6470eb420ab53138d0df3c47dbce4ce33daa237d))
* **runner:** persist exact provider-budget classification and verify CLI reset ([af4b022](https://github.com/misty-step/iron-forest/commit/af4b02215fb0f7b9091a5eeb3a711b33af2011ae))

## [0.0.4](https://github.com/misty-step/iron-forest/compare/v0.0.3...v0.0.4) (2026-09-01)


### Bug Fixes

* **polls:** snapshot poll candidates in one ls-remote ([613e729](https://github.com/misty-step/iron-forest/commit/613e72956ab6d99b686cf154628a61a42463f4a9))

## [0.0.3](https://github.com/misty-step/iron-forest/compare/v0.0.2...v0.0.3) (2026-08-30)


### Bug Fixes

* **powder:** reconcile landed subjects in kernel ([#334](https://github.com/misty-step/iron-forest/issues/334)) ([26c0191](https://github.com/misty-step/iron-forest/commit/26c0191d175383f67208e8431507af66475ef6ec))

## [0.0.2](https://github.com/misty-step/iron-forest/compare/v0.0.1...v0.0.2) (2026-08-30)


### Bug Fixes

* clear all in-memory audit errors ([#336](https://github.com/misty-step/iron-forest/issues/336)) ([4eec748](https://github.com/misty-step/iron-forest/commit/4eec74870cb31fdd15af1977bbbc2ab57e207284))

## [0.0.1](https://github.com/misty-step/iron-forest/compare/v0.0.0...v0.0.1) (2026-08-29)


### Bug Fixes

* align flow policy and retirement evidence ([30c7d1d](https://github.com/misty-step/iron-forest/commit/30c7d1dee15c294bb38152c5ef306fded9728f95))
* bind publication to captured bytes and trusted tools ([a36a899](https://github.com/misty-step/iron-forest/commit/a36a89975c411c3095c492f805687fc793418e82))
* bound and fail closed on every Kernel boundary ([f8f87ea](https://github.com/misty-step/iron-forest/commit/f8f87ea05b0521c7dc6603e043807b065c607a0c))
* bound rejected host merge requests ([ac6f5bb](https://github.com/misty-step/iron-forest/commit/ac6f5bba5ad604320374c8e970835b0a0ec35574))
* close remaining publication protocol holes ([96c9886](https://github.com/misty-step/iron-forest/commit/96c98862290d0b65cd45cbef16b53199e440afb6))
* close the contract and locking defects a second review round found ([f099ed1](https://github.com/misty-step/iron-forest/commit/f099ed1303db69cbd96828df40ad62439735a056)), closes [#257](https://github.com/misty-step/iron-forest/issues/257)
* close the profile composition holes the first review found ([3207381](https://github.com/misty-step/iron-forest/commit/32073810a7145afdac6291d64f1068f3e8acc660))
* compare-and-swap publication refs with exact leases ([b33c152](https://github.com/misty-step/iron-forest/commit/b33c15215fa1e0e1354b78dc66dd24655f521095))
* do not record gh help as a created PR ([8a46a39](https://github.com/misty-step/iron-forest/commit/8a46a390d17b8edc0cee73c9e020a5043e9a8bed))
* drop leftover timeout from digest dispatch test ([d522d65](https://github.com/misty-step/iron-forest/commit/d522d6519dcf9a2da3bd2471cd29e2abacd9e272))
* emit OpenRouter Run session headers ([2d8ea23](https://github.com/misty-step/iron-forest/commit/2d8ea23e851b49f74dc52acab652bdbe2652fc08))
* fail closed and redact the secret scan ([ac87592](https://github.com/misty-step/iron-forest/commit/ac87592569496d82b64860d005dfc07cfe394892))
* fail closed on mismatched request evidence ([6c4e88a](https://github.com/misty-step/iron-forest/commit/6c4e88a0b2ff1e6fdb6ae8107329d4483d3bb14f))
* fail closed when an explicit operator profile is missing ([d05cb7c](https://github.com/misty-step/iron-forest/commit/d05cb7c165d20f92db4cfd78ed933c6fce9c1062))
* fence final publication and close recovery ([d949054](https://github.com/misty-step/iron-forest/commit/d949054073e4b1f73124ee448b366fb7aa16e47b))
* follow a symlink when copying the operator profile ([999979a](https://github.com/misty-step/iron-forest/commit/999979a33e6dd31bd33e84be324da57dc99acafa))
* forbid Builder probe writes on a no-work pass ([0b7d284](https://github.com/misty-step/iron-forest/commit/0b7d284a3706ff9e815678cd6b09cdc26daa1ba1))
* harden declaration profile composition ([08fa335](https://github.com/misty-step/iron-forest/commit/08fa3357943785daf2bb1cedda0d2ee585f7d148))
* harden host merge evidence and handoff ([3157c9b](https://github.com/misty-step/iron-forest/commit/3157c9b01ffe69b6818ed99e1ce6458845f1aad5))
* hide eval fixtures from the candidate ([9b5aab5](https://github.com/misty-step/iron-forest/commit/9b5aab5a55c28beb32f3be4ac28678eee39ed6e6))
* honor terminal Pi agent errors ([526d720](https://github.com/misty-step/iron-forest/commit/526d720bccd7e9f10e025de3ac486c792117095e))
* isolate Iron Forest evaluation credentials ([ca069e6](https://github.com/misty-step/iron-forest/commit/ca069e6dbbd071dbe0016f9e0c22ece4d5aaf54c))
* keep close claim reset retryable ([c290658](https://github.com/misty-step/iron-forest/commit/c290658b723e0768c7b4055724d3a21ea28f8259))
* let a Run use the repository's own skills and tools ([bb02e47](https://github.com/misty-step/iron-forest/commit/bb02e47d747f76467dd66269c1d49d3a4cf5f541))
* make CI and note identities hermetic ([37c28c1](https://github.com/misty-step/iron-forest/commit/37c28c1bda074f79a6d92ec2fe925fa4a3ad8917))
* make CI isolation portable ([e7bae63](https://github.com/misty-step/iron-forest/commit/e7bae63ef2977651a1906cce99a28af00ee46652))
* make flow recovery converge deterministically ([4ecd61a](https://github.com/misty-step/iron-forest/commit/4ecd61a05240de8a163672121cb84bc91ebdf358))
* make Flow recovery convergent ([156516a](https://github.com/misty-step/iron-forest/commit/156516a4fc698135346ac23da30f66c8cedcbf70))
* pass prompt to agent, close via gh issue close, honest PR file list ([de92a46](https://github.com/misty-step/iron-forest/commit/de92a465ca5eddb650f46fb3259a2e136615e961))
* preserve retry and malformed identity fences ([fd712ef](https://github.com/misty-step/iron-forest/commit/fd712ef201ab1cd2360138313661c4e491706fe1))
* put Check worktrees on the primary checkout ([783a1d6](https://github.com/misty-step/iron-forest/commit/783a1d67cd328ad988678546f5a63efdb371d0b6))
* reconcile [#144](https://github.com/misty-step/iron-forest/issues/144) with current master ([#270](https://github.com/misty-step/iron-forest/issues/270)/[#271](https://github.com/misty-step/iron-forest/issues/271)) ([d18726b](https://github.com/misty-step/iron-forest/commit/d18726b2e0bf7bb6b51c54fc3ebc41b663dce910))
* reconcile retirement deletion and watch flows ([15bec63](https://github.com/misty-step/iron-forest/commit/15bec632b7c368fb5e07efbb0b5236fd13d1f85c))
* recover from a canonical review-request race ([0266966](https://github.com/misty-step/iron-forest/commit/02669664b7ca0b149b1cf40716b5273420eca10f))
* reject malformed retirement branches ([29f29d4](https://github.com/misty-step/iron-forest/commit/29f29d4e54d9ddffa50dfc131014c5ab058e53c6))
* release advanced branches from stale retirement ([04805b4](https://github.com/misty-step/iron-forest/commit/04805b4dea31c3f358b09471918f4fede80e60cd))
* repair what independent QA of the rendered surface found ([08b3083](https://github.com/misty-step/iron-forest/commit/08b30836b0f48c8563796cced33d2f2531919af8))
* require Builder or Fixer identity on every destination note ([46e50e8](https://github.com/misty-step/iron-forest/commit/46e50e8c96da5a8635ef8a9ab5a911dea3209e06))
* require Verifier approve to fast-forward ([b1ece5f](https://github.com/misty-step/iron-forest/commit/b1ece5f4cfbc18da49bc35df121dca440e0d9bfe))
* resolve the harness through PATH and report one cause when it is missing ([47d1ced](https://github.com/misty-step/iron-forest/commit/47d1ced8c23e5d660e1e4192bb03e0d606ebfdb6))
* scope Run Git identity through config ([8ac9a13](https://github.com/misty-step/iron-forest/commit/8ac9a13b4010a4df4ab0478aba0f2d725c80f416))
* seed the host Pi profile and close the remaining composition holes ([f8daf2d](https://github.com/misty-step/iron-forest/commit/f8daf2d7cb30a6388dc63bf5b25014f13634e547))
* sweep killed Check worktrees with reserved GC ([c6a485d](https://github.com/misty-step/iron-forest/commit/c6a485dab49ec92d33cdf1922ac547d60e7ccaff))
* treat a just-recorded cancel as already finished ([93cd522](https://github.com/misty-step/iron-forest/commit/93cd522134be34ac94001cf47e1d1d51143d4a1f))
* treat wrong-author review-request notes as conflicts ([b473024](https://github.com/misty-step/iron-forest/commit/b47302473ea9af776a76449cbc57243161b73883))
* verify the declaration digest immediately before Pi starts ([21677bb](https://github.com/misty-step/iron-forest/commit/21677bbe80e3b4d78d2da36920f1086ba2c8fe0a))


### Features

* add Harbor agent regression harness ([f13da10](https://github.com/misty-step/iron-forest/commit/f13da10f62e5c1dd3dfb9de39cc5ac4bd3debad6))
* bind declarations to run-private refs and exact note writers ([391bd5f](https://github.com/misty-step/iron-forest/commit/391bd5fcb570ef49527f51fda73ea772547f5a61))
* compose a per-Run harness profile from layered sources ([cc10fd6](https://github.com/misty-step/iron-forest/commit/cc10fd6a56bd28b95d7361b384bb310159213373))
* correlate provider traces with Run IDs ([d975a38](https://github.com/misty-step/iron-forest/commit/d975a3828c415c3798d04494099373d4ed383399))
* default Forest to DeepSeek V4 Pro ([3532d8a](https://github.com/misty-step/iron-forest/commit/3532d8a4782a858bdc2f127f2abe9544eadfa190))
* define lean role harness profiles ([38fc8cf](https://github.com/misty-step/iron-forest/commit/38fc8cf9dc22d23924aeb1e4f31584212727713d))
* expose the full read surface on the CLI ([641bb78](https://github.com/misty-step/iron-forest/commit/641bb787cfff48a017adf8a0cd2aecf169efa77a)), closes [#258](https://github.com/misty-step/iron-forest/issues/258)
* forest backlog-to-PR factory first slice ([ad3b6ad](https://github.com/misty-step/iron-forest/commit/ad3b6ad233f405219eeaec6658b44fd15c3ab0bd))
* Kernel-publish Checks, Verdict, and approve ([db1962b](https://github.com/misty-step/iron-forest/commit/db1962b4174af39b97b789008899c6749f3cf935)), closes [#238](https://github.com/misty-step/iron-forest/issues/238) [#278](https://github.com/misty-step/iron-forest/issues/278)
* make pi the agent harness so a Run can complete ([db9330b](https://github.com/misty-step/iron-forest/commit/db9330ba21031ea8c434e658205aaa4b9a69ddb4))
* Poll and Auditor read evidence refs ([5bd1c2c](https://github.com/misty-step/iron-forest/commit/5bd1c2cafe96a5efe276a9313c8094ebce891778)), closes [#279](https://github.com/misty-step/iron-forest/issues/279)
* publish review-requests in the Kernel ([c00a0bd](https://github.com/misty-step/iron-forest/commit/c00a0bd4fff5d6bc570ae577a801013a243e1d11))
* remove agent run deadlines ([ce8685d](https://github.com/misty-step/iron-forest/commit/ce8685d6280b0a7654b8038acc1baaefe12c6436))

# Changelog

- 2026-09-02: Critic and Tester are promoted into the default profile as
  non-review, drafts-only roles. Promotion evidence:
  `evals/jobs/fast/fast-20260901T224519Z/report.md` is 22/22, and settled
  Runs `1788301018846047029-critic` and `1788301018844450077-tester` each
  produced one attributed spec-less draft.
- 2026-08-22: Critic and Tester are EXPERIMENTAL and local-canary-only.
  They stay enabled only in the self-host Iron Forest checkout for canary
  observation; external operators must not copy or enable them until the
  rollout exit gate closes (blockers merged, corrected deterministic evals
  pass, one post-fix live sweep per role produces attributable spec-less
  drafts).
- 2026-08-19: One Subject identity. Review-request is only v2
  (`subject` + `forest/<id>/<slug>`). Builder Poll lists GitHub Issues
  and Powder jobs. The Kernel does not take or complete them (ADR 0023).
  Leftover hyphen tips are unread by Poll.
- 2026-08-18: A second `forest run cancel` after the Runner records the
  Ledger row reports `already_finished` instead of not-found (#289).
- 2026-08-18: Poll and Auditor drop leftover notes-era read machinery.
  They still ignore `refs/notes/forest/*`. Publish still dual-writes
  the request note (#287).
- 2026-08-18: Gate proof on disposable repo `misty-step/forest-gate-127`.
  A non-compiling Revision produced failing Checks, a `changes` Verdict,
  unchanged `master`, and Run `1787065161454179061-verifier`. Transcript
  in `VERIFY.md` (#127).

- 2026-08-17: Onboarding states which identity may create or update which
  Git ref. A read-only forge credential is refused. Branch protection cannot
  see evidence refs (#255).

- 2026-08-17: Poll and Auditor read `refs/forest/v1/*`. Leftover notes are
  unread. Verifier calls `forest publish verdict`. Builder/Fixer dual-write
  a request evidence ref with the existing note (#279).

- 2026-08-17: `forest publish verdict` owns Checks and Verdict evidence refs
  (`refs/forest/v1/checks/<sha>`, `refs/forest/v1/verdict/<sha>`). Approve
  runs configured Checks and fast-forwards `master` in the same atomic push
  (#238, #278, ADR 0022).


- 2026-08-17: VISION names evidence refs `refs/forest/v1/*` and Kernel
  `publish verdict` as the destination (#277). The running binary still
  follows ADR 0021 until #238 and #278. Onboarding states one Kernel per
  repository per machine (#248). `audit show` prints `audited_master` as
  `master=` and keeps `last_master` as the last-good ancestry tip (#264).
  Human Run rows lead with `exit` and `duration` (#263).

- 2026-08-15: Recorded the product lock in `VISION.md`. One Kernel serves
  one repository on one machine. The CLI is the operations surface. The host
  vendor is an operator choice. Mint, Powder, Habitat, Fly Sprites, and a
  dashboard are out of product.

- 2026-08-15: Builder and Fixer publish review-requests through
  `forest publish review-request`. The Kernel owns the write-once note, role
  identity, configured Check gate, and bounded atomic retry. Shipped default
  model is `openrouter/deepseek/deepseek-v4-pro-0813`.

- 2026-08-13: Dispatch now verifies the agent bundle. The Kernel digests the
  ordered declaration pair (`agent.md` then `task.md`) at load and recomputes
  that digest immediately before starting Pi; a file changed after load aborts
  the Run with a nonzero-exit Ledger row and refuses to start Pi. The Ledger
  records the digest only after that verification succeeds (#144).

- 2026-08-13: Removed per-agent wall-clock deadlines. `forest.yaml` no longer
  accepts `timeout`; the Runner does not create a deadline around preparation
  or Pi execution; and the systemd service drains active Runs indefinitely.
  Explicit foreground cancellation and bounded mechanical cleanup remain.

- 2026-08-13: Replaced layered Pi profile composition with explicit per-Run
  inputs: an isolated temporary Pi directory, checked-in shared and role skill
  directories, and disabled ambient extension/resource discovery. For an
  OpenRouter model, the temporary directory contains only a generated,
  credential-free session-affinity override. The service
  now requires a protected per-instance credential environment file and uses a
  private temporary namespace; the installer removes credential-bearing legacy
  Run-profile residue during cutover. Declaration output and Run evidence
  publish `skills`; declaration `env` and the obsolete `profile_files` surface
  are removed. These breaking changes advance CLI envelopes to `forest.cli.v2`.
  Terminal Pi agent errors now fail the Run even when Pi exits zero. Per-Run
  Git identities use scoped Git configuration rather than author/committer
  overrides, so nested verification commands can set deterministic identities.
  Pi uses the exact Run ID as its provider session ID; the generated OpenRouter
  override sends it as `x-session-id` for trace correlation.

- 2026-08-10: Reforged Iron Forest as a Kernel plus declarations. Git is
  the coordination authority with schema-v1 write-once notes, agent-owned
  Effects, an evidence-first fast-forward Gate, and a read-only Auditor. The
  Builder, Verifier, and Fixer declarations use files under `agents/`,
  Polls use explicit exit semantics, and one Kernel serves each repository.
  Evals remain the instrument for actor-boundary changes.


Current behavior is defined by `VISION.md`, `README.md`, the shipped
declarations, and the accepted ADRs.


Historical pre-reforge entries remain in repository history before 2026-08-10.
