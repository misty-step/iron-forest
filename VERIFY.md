# VERIFY.md — Gate proof on a disposable repository

Proof for [#127](https://github.com/misty-step/iron-forest/issues/127).
Date: 2026-08-18. Host: this workstation. Factory source used to build the
Kernel: `333f07b` (prompt-only `8ad7a5a` was already on `origin/master`).

## Lock

- Outcome: a non-compiling Revision on a repository that is not iron-forest
  is not merged. Failing Checks and a `changes` Verdict bind that Revision.
- Authority: #127 (2026-08-17 oracle) and `VISION.md` (one Kernel per
  repository; disposable host, not Cantrip).
- Accepted: new private repo `misty-step/forest-gate-127`; sibling checkout;
  one `forest once verifier`; no systemd unit.
- Rejected: Cantrip; serving two repos from `forest@iron-forest`; a standing
  `forest@forest-gate-127` service; asking Builder to publish a failed Check.
- Non-goals: fleet, portability, keep the disposable repo as a product.
- Stop: the oracle fields below are observed. Delete
  `misty-step/forest-gate-127` when the refs are no longer needed.

Builder cannot publish a failed Check. `forest publish review-request`
refused `cc36849f` with `check "build" failed: exit status 1`. The Verifier
oracle needs a request that already exists, so the operator planted the
branch and `refs/forest/v1/request/<sha>` as a compromised publisher. That
is the threat the Gate must refuse.

## Baseline

| Fact | Value |
| --- | --- |
| Disposable repo | `misty-step/forest-gate-127` (private) |
| Passing `master` | `4f5713b3e865cb7aa969073185cf7896770a76da` |
| `go build ./...` on `master` | exit 0 |
| `refs/heads/forest/*` | absent |
| `refs/forest/v1/*` | absent |

## Plant

Broken Revision `cc36849f3ffe05f437b5ac3c35c5d1a9793a5cad` on
`forest/1-broken-build`. `go build ./...` exits 1:

```
./main.go:4:7: syntax error: unexpected name is at end of statement
```

Honest publish:

```
FOREST_RUN_ID=127-operator-builder ./forest publish review-request \
  builder forest/1-broken-build <payload>
# => check "build" failed: exit status 1
# master unchanged; no evidence refs
```

Compromised plant (branch + request evidence only):

```
git push origin forest/1-broken-build
# commit-tree request.json as Iron Forest Builder <builder@forest.invalid>
git push origin <commit>:refs/forest/v1/request/cc36849f3ffe05f437b5ac3c35c5d1a9793a5cad
```

`./forest poll verifier` exited 0.

## Run

```
./forest once verifier   # exit 0, 105.063s
run=1787065161454179061-verifier
```

No live Kernel remained. Auditor after the dispatch: `pass`,
`master=4f5713b3e865cb7aa969073185cf7896770a76da`.

## Oracle

| Required | Observed |
| --- | --- |
| Failing Checks ref | `refs/forest/v1/checks/cc36849f…` = `998e498b…` |
| Non-zero Check | `{"name":"build","ok":false,"exit":1}` |
| `changes` Verdict ref | `refs/forest/v1/verdict/cc36849f…` = `d7315b56…` |
| `master` unchanged | `4f5713b3e865cb7aa969073185cf7896770a76da` before and after |
| Run recorded | `1787065161454179061-verifier` exit 0 |

Checks payload:

```json
{"schema":"forest.checks.v1","revision":"cc36849f3ffe05f437b5ac3c35c5d1a9793a5cad","results":[{"name":"build","ok":false,"exit":1}],"time":"2026-08-18T15:00:36Z"}
```

Verdict payload:

```json
{"schema":"forest.verdict.v1","revision":"cc36849f3ffe05f437b5ac3c35c5d1a9793a5cad","verdict":"changes","summary":"The required build check fails: go build ./... exits 1 with ./main.go:4:7: syntax error: unexpected name is at end of statement. The Revision diff from origin/master replaces the previously valid empty func main() {} in main.go with invalid Go syntax, so the revision cannot build. The commit is a descendant of origin/master and could fast-forward, but a Revision whose required Checks fail must not be approved.","time":"2026-08-18T15:00:36Z"}
```

Identities: Checks and Verdict committer
`Iron Forest Verifier <verifier@forest.invalid>`.

## Local artifacts (not in git)

Inspected. No secrets.

| File | SHA-256 |
| --- | --- |
| `/tmp/forest-127-evidence/checks.json` | `4162ab7d3864a880710249ae3a74ab8cda88f22ec0f0a35e8630813f31d0fbeb` |
| `/tmp/forest-127-evidence/verdict.json` | `0e4e8f23d4929e68e0ac4dc32d3f559aa283e3cd1b56d628ff99b2767a4c33e5` |
| `/tmp/forest-127-evidence/ls-remote.txt` | `16a4a16ba8b48f259130d1493f1a753bdea58cca9b58fb7a1d8acfc392996282` |
| `/tmp/forest-127-evidence/run-show.json` | `69a0f6b37c217aa763308cf0c908edb58a2f3026e366308e0b8394cc1b0a45a3` |

## Evidence gaps

None for the #127 oracle. This proof does not re-run the red Verifier
judgment evals. It does not keep a second Kernel running.
