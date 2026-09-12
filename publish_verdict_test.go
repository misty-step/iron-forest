package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEvidencePayloads(t *testing.T, revision, verdict string) (string, string) {
	t.Helper()
	results := `[{"name":"test","ok":true,"exit":0}]`
	if verdict == "changes" {
		results = `[{"name":"test","ok":false,"exit":1}]`
	}
	return writeEvidencePayloadsWithResults(t, revision, verdict, results)
}

func writeEvidencePayloadsWithResults(t *testing.T, revision, verdict, results string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	checks := `{"schema":"forest.checks.v1","revision":"` + revision + `","results":` + results + `,"time":"2026-08-17T00:00:00Z"}` + "\n"
	verdictPayload := `{"schema":"forest.verdict.v1","revision":"` + revision + `","verdict":"` + verdict + `","summary":"eval","time":"2026-08-17T00:00:00Z"}` + "\n"
	checksPath := filepath.Join(dir, "checks.json")
	verdictPath := filepath.Join(dir, "verdict.json")
	if err := os.WriteFile(checksPath, []byte(checks), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(verdictPath, []byte(verdictPayload), 0o644); err != nil {
		t.Fatal(err)
	}
	return checksPath, verdictPath
}

func requireMissingRemoteRef(t *testing.T, root, ref string) {
	t.Helper()
	if output := strings.TrimSpace(string(runGitDir(t, root, "ls-remote", "origin", ref))); output != "" {
		t.Fatalf("remote ref %s exists: %s", ref, output)
	}
}

func TestPublishVerdictRejectsWorkDriftBeforeIdentical(t *testing.T) {
	for _, decision := range []string{"approve", "changes"} {
		t.Run(decision, func(t *testing.T) {
			root, _ := testClone(t)
			writePassingChecks(t, root)
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			builder := liveRunRecord{RunID: "10-builder", Agent: "builder", StartedAt: "2026-09-09T00:00:00Z", RequestID: "build-request", Work: &WorkReference{System: "opaque", ID: "work-id", Key: "WORK-1"}}
			seedPublicationRun(t, root, builder)
			payload := writeReviewPayloadForRun(t, revision, "forest/work/implementation", builder)
			if _, err := publishReviewRequest(context.Background(), publishReviewRequestInput{Root: root, Role: "builder", Branch: "forest/work/implementation", PayloadPath: payload, RunID: builder.RunID}); err != nil {
				t.Fatal(err)
			}
			work := *builder.Work
			verifier := liveRunRecord{RunID: "11-verifier", Agent: "verifier", StartedAt: builder.StartedAt, RequestID: "review-request", Work: &work}
			checks, verdict := writeEvidencePayloads(t, revision, decision)
			input := publishVerdictInput{Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: verifier.RunID}
			for _, identical := range []bool{false, true} {
				work.Key = "DIFFERENT-1"
				seedPublicationRun(t, root, verifier)
				before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
				if _, err := publishVerdict(context.Background(), input); err == nil {
					t.Fatalf("work drift accepted (identical=%t)", identical)
				}
				if after := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); after != before {
					t.Fatal("unrelated work received verdict evidence")
				}
				if !identical {
					work.Key = builder.Work.Key
					seedPublicationRun(t, root, verifier)
					if _, err := publishVerdict(context.Background(), input); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestPublishVerdictRejectsCandidateMovedDuringChecks(t *testing.T) {
	root, origin := testClone(t)
	config := `repo: owner/name
primary: refs/heads/master
agents:
  builder: {poll: "true", interval: 1}
checks:
  - name: test
    run: git commit --allow-empty -m concurrent && git push origin HEAD:refs/heads/forest/work/implementation
`
	writeTree(t, root, profileName+"/config.yaml", config)
	runGitDir(t, root, "commit", "-am", "move candidate during checks")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	primary := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master")))
	pushEvidence(t, root, "request", revision, pollReviewNoteBranch(revision, "forest/work/implementation"), "Iron Forest Builder", "builder@forest.invalid")
	runGitDir(t, root, "push", "origin", revision+":refs/heads/forest/work/implementation")
	checks, verdict := writeEvidencePayloads(t, revision, "approve")
	seedVerdictRun(t, root, "1-verifier")
	_, err := publishVerdict(context.Background(), publishVerdictInput{Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier"})
	if err == nil || !publishConflict(err) {
		t.Fatalf("stale candidate error=%v", err)
	}
	if got := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/forest/work/implementation"))); got == revision {
		t.Fatal("configured check did not move candidate")
	}
	if got := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master"))); got != primary {
		t.Fatal("stale candidate advanced primary")
	}
	requireMissingRemoteRef(t, root, evidenceChecksRefPrefix+revision)
	requireMissingRemoteRef(t, root, evidenceVerdictRefPrefix+revision)
}

func seedVerdictRun(t *testing.T, root, runID string) {
	t.Helper()
	t.Setenv("FOREST_ROOT", root)
	if err := writeLiveRun(liveRunPath(root, "verifier"), liveRunRecord{RunID: runID, Agent: "verifier", StartedAt: "2026-08-17T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
}

func TestPublishVerdictChangesCreatesEvidenceRefs(t *testing.T) {
	root, origin := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushRequestForRevision(t, root, "if-changes", revision)
	before := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master")))
	checks, verdict := writeEvidencePayloads(t, revision, "changes")
	seedVerdictRun(t, root, "1-verifier")
	result, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "published" || result.Verdict != "changes" || result.Revision != revision {
		t.Fatalf("result=%#v", result)
	}
	if got := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master"))); got != before {
		t.Fatalf("master moved to %s", got)
	}
	for ref, path := range map[string]string{
		evidenceChecksRefPrefix + revision:  checks,
		evidenceVerdictRefPrefix + revision: verdict,
	} {
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got := runGit(t, "--git-dir="+origin, "show", ref+":"+filepath.Base(path))
		if string(got) != string(want) {
			t.Fatalf("published %s=%s, want %s", ref, got, want)
		}
	}
}

func TestPublishVerdictApproveFastForwardsMaster(t *testing.T) {
	root, origin := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushRequestForRevision(t, root, "if-approve", revision)
	requestBefore := string(runGitDir(t, root, "ls-remote", "origin", evidenceRequestRefPrefix+revision, "refs/heads/forest/if-approve/work"))
	checks, verdict := writeEvidencePayloads(t, revision, "approve")
	seedVerdictRun(t, root, "1-verifier")
	result, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "published" || result.Verdict != "approve" {
		t.Fatalf("result=%#v", result)
	}
	got := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master")))
	if got != revision {
		t.Fatalf("master=%s want %s", got, revision)
	}
	if got := string(runGitDir(t, root, "ls-remote", "origin", evidenceRequestRefPrefix+revision, "refs/heads/forest/if-approve/work")); got != requestBefore {
		t.Fatalf("request refs changed:\n%s\nwant:\n%s", got, requestBefore)
	}
	for ref, path := range map[string]string{
		evidenceChecksRefPrefix + revision:  checks,
		evidenceVerdictRefPrefix + revision: verdict,
	} {
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got := runGit(t, "--git-dir="+origin, "show", ref+":"+filepath.Base(path))
		if string(got) != string(want) {
			t.Fatalf("published %s=%s, want %s", ref, got, want)
		}
	}
}

func TestPublishVerdictApprovalAuthorityMatrix(t *testing.T) {
	for _, candidateAuthority := range []string{"", "land", "review"} {
		for _, verifierAuthority := range []string{"", "land", "review"} {
			t.Run("candidate="+candidateAuthority+"/verifier="+verifierAuthority, func(t *testing.T) {
				root, origin := testClone(t)
				branch := "forest/work/implementation"
				runGitDir(t, root, "checkout", "-b", branch)
				localPrimary := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "refs/heads/master")))
				remotePrimary := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master")))
				writePassingChecks(t, root)
				revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
				builder := liveRunRecord{RunID: "10-builder", Agent: "builder", StartedAt: "2026-09-12T00:00:00Z", RequestID: "build-request", Authority: candidateAuthority,
					Work: &WorkReference{System: "opaque", ID: "selected-work"}}
				seedPublicationRun(t, root, builder)
				payload := writeReviewPayloadForRun(t, revision, branch, builder)
				if _, err := publishReviewRequest(context.Background(), publishReviewRequestInput{Root: root, Role: "builder", Branch: branch, PayloadPath: payload, RunID: builder.RunID}); err != nil {
					t.Fatal(err)
				}
				requestBefore := string(runGitDir(t, root, "ls-remote", "origin", evidenceRequestRefPrefix+revision, "refs/heads/"+branch))
				if err := os.Remove(liveRunPath(root, "builder")); err != nil {
					t.Fatal(err)
				}
				verifier := liveRunRecord{RunID: "11-verifier", Agent: "verifier", StartedAt: "2026-09-12T01:00:00Z", RequestID: "review-request", Authority: verifierAuthority, Work: builder.Work}
				seedPublicationRun(t, root, verifier)
				checks, verdict := writeEvidencePayloads(t, revision, "approve")
				input := publishVerdictInput{Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: verifier.RunID}
				reviewOnly := candidateAuthority == "review" || verifierAuthority == "review"
				for _, status := range []string{"published", "identical"} {
					if reviewOnly {
						status = "review-only"
					}
					result, err := publishVerdict(context.Background(), input)
					if err != nil || result.Status != status || result.Revision != revision || result.Verdict != "approve" {
						t.Fatalf("approval=%#v error=%v want status %s", result, err, status)
					}
				}
				wantPrimary := revision
				if reviewOnly {
					wantPrimary = remotePrimary
				}
				if got := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master"))); got != wantPrimary {
					t.Fatalf("remote primary=%s want %s", got, wantPrimary)
				}
				if got := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "refs/heads/master"))); got != localPrimary {
					t.Fatalf("local primary moved to %s from %s", got, localPrimary)
				}
				if got := string(runGitDir(t, root, "ls-remote", "origin", evidenceRequestRefPrefix+revision, "refs/heads/"+branch)); got != requestBefore {
					t.Fatal("approval changed request revision or branch")
				}
				for ref, path := range map[string]string{
					evidenceRequestRefPrefix + revision: payload,
					evidenceChecksRefPrefix + revision:  checks,
					evidenceVerdictRefPrefix + revision: verdict,
				} {
					name := filepath.Base(path)
					if path == payload {
						name = "request.json"
					}
					got := runGit(t, "--git-dir="+origin, "show", ref+":"+name)
					if string(got) != string(mustRead(t, path)) {
						t.Fatalf("approval changed exact evidence for %s", ref)
					}
				}
			})
		}
	}
}

func TestPublishLandAuthorityCannotElevateExternalDelivery(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	branch := "forest/work/implementation"
	builder := liveRunRecord{RunID: "10-builder", Agent: "builder", StartedAt: "2026-09-12T00:00:00Z", RequestID: "build-request", Authority: "land"}
	seedPublicationRun(t, root, builder)
	payload := writeReviewPayloadForRun(t, revision, branch, builder)
	build := publishReviewRequestInput{Root: root, Role: "builder", Branch: branch, PayloadPath: payload, RunID: builder.RunID}
	if _, err := publishReviewRequest(context.Background(), build); err != nil {
		t.Fatal(err)
	}
	verifier := liveRunRecord{RunID: "11-verifier", Agent: "verifier", StartedAt: builder.StartedAt, RequestID: "review-request", Authority: "land"}
	seedPublicationRun(t, root, verifier)
	checks, verdict := writeEvidencePayloads(t, revision, "approve")
	writeTree(t, root, profileName+"/config.yaml", "repo: owner/name\ndelivery: external\nagents:\n  builder: {poll: 'true', interval: 1}\n")
	before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
	if _, err := publishReviewRequest(context.Background(), build); err == nil || !publishConflict(err) {
		t.Fatalf("land builder bypassed external delivery: %v", err)
	}
	if _, err := publishVerdict(context.Background(), publishVerdictInput{Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: verifier.RunID}); err == nil || !publishConflict(err) {
		t.Fatalf("land verifier bypassed external delivery: %v", err)
	}
	if after := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); after != before {
		t.Fatal("land authority changed external delivery refs")
	}
}

func TestPublishVerdictApproveRejectsMissingRequest(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
	checks, verdict := writeEvidencePayloads(t, revision, "approve")
	seedVerdictRun(t, root, "1-verifier")
	_, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier",
	})
	if err == nil {
		t.Fatal("approved without request evidence")
	}
	if got := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); got != before {
		t.Fatalf("remote refs changed after rejection:\n%s\nwant:\n%s", got, before)
	}
}

func TestPublishVerdictApproveRejectsInvalidRequestEvidence(t *testing.T) {
	for _, name := range []string{"wrong identity", "different revision"} {
		t.Run(name, func(t *testing.T) {
			root, _ := testClone(t)
			base := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			writePassingChecks(t, root)
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			requestRevision := revision
			author, email := "Iron Forest Builder", "builder@forest.invalid"
			if name == "wrong identity" {
				author, email = "Iron Forest Verifier", "verifier@forest.invalid"
			} else {
				requestRevision = base
			}
			request := pollReviewNoteBranch(requestRevision, "forest/if-invalid-request/work")
			pushEvidence(t, root, "request", revision, request, author, email)
			runGitDir(t, root, "push", "origin", revision+":refs/heads/forest/if-invalid-request/work")
			before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
			checks, verdict := writeEvidencePayloads(t, revision, "approve")
			seedVerdictRun(t, root, "1-verifier")

			_, err := publishVerdict(context.Background(), publishVerdictInput{
				Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier",
			})
			if err == nil {
				t.Fatal("approved invalid request evidence")
			}
			if got := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); got != before {
				t.Fatalf("remote refs changed after rejection:\n%s\nwant:\n%s", got, before)
			}
		})
	}
}

func TestPublishVerdictApproveRejectsFailingChecks(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushRequestForRevision(t, root, "if-failing-checks", revision)
	before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
	checks, verdict := writeEvidencePayloadsWithResults(t, revision, "approve", `[{"name":"test","ok":false,"exit":1}]`)
	seedVerdictRun(t, root, "1-verifier")
	_, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier",
	})
	if err == nil {
		t.Fatal("approved failing submitted Checks")
	}
	if got := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); got != before {
		t.Fatalf("remote refs changed after rejection:\n%s\nwant:\n%s", got, before)
	}
}

func TestPublishVerdictApproveRequiresCandidateCheckNames(t *testing.T) {
	tests := []struct {
		name    string
		results string
		approve bool
	}{
		{name: "different name", results: `[{"name":"test","ok":true,"exit":0},{"name":"other","ok":true,"exit":0}]`},
		{name: "reordered", results: `[{"name":"vet","ok":true,"exit":0},{"name":"test","ok":true,"exit":0}]`},
		{name: "missing configured check", results: `[{"name":"test","ok":true,"exit":0}]`},
		{name: "extra submitted check", results: `[{"name":"test","ok":true,"exit":0},{"name":"vet","ok":true,"exit":0},{"name":"other","ok":true,"exit":0}]`},
		{name: "candidate order", results: `[{"name":"test","ok":true,"exit":0},{"name":"vet","ok":true,"exit":0}]`, approve: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, origin := testClone(t)
			config := `repo: owner/name
primary: refs/heads/master
agents:
  builder: {poll: "true", interval: 1}
checks:
  - {name: test, run: "true"}
  - {name: vet, run: "true"}
`
			writeTree(t, root, profileName+"/config.yaml", config)
			runGitDir(t, root, "add", ".iron-forest/config.yaml")
			runGitDir(t, root, "commit", "-m", "candidate checks")
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			pushRequestForRevision(t, root, "if-candidate-checks", revision)
			before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
			// The invoking checkout advertises only test; the candidate also requires vet.
			workingConfig := strings.Replace(config, "  - {name: vet, run: \"true\"}\n", "", 1)
			writeTree(t, root, profileName+"/config.yaml", workingConfig)
			checks, verdict := writeEvidencePayloadsWithResults(t, revision, "approve", test.results)
			seedVerdictRun(t, root, "1-verifier")

			result, err := publishVerdict(context.Background(), publishVerdictInput{
				Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier",
			})
			if !test.approve {
				if err == nil {
					t.Fatal("approved Checks that do not match the candidate configuration")
				}
				if got := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); got != before {
					t.Fatalf("remote refs changed after rejection:\n%s\nwant:\n%s", got, before)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "published" {
				t.Fatalf("result=%#v", result)
			}
			if got := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master"))); got != revision {
				t.Fatalf("master=%s want %s", got, revision)
			}
			for ref, path := range map[string]string{
				evidenceChecksRefPrefix + revision:  checks,
				evidenceVerdictRefPrefix + revision: verdict,
			} {
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				got := runGit(t, "--git-dir="+origin, "show", ref+":"+filepath.Base(path))
				if string(got) != string(want) {
					t.Fatalf("published %s=%s, want %s", ref, got, want)
				}
			}
		})
	}
}

func TestPublishVerdictApproveRejectsMovedRequestBranch(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushRequestForRevision(t, root, "if-moved-branch", revision)
	if err := os.WriteFile(filepath.Join(root, "later"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "later")
	runGitDir(t, root, "commit", "-m", "move request branch")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/if-moved-branch/work")
	before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
	checks, verdict := writeEvidencePayloads(t, revision, "approve")
	seedVerdictRun(t, root, "1-verifier")
	_, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier",
	})
	if err == nil {
		t.Fatal("approved a revision no longer at the request branch tip")
	}
	if got := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); got != before {
		t.Fatalf("remote refs changed after rejection:\n%s\nwant:\n%s", got, before)
	}
}

func TestPublishVerdictApproveLeasesRequest(t *testing.T) {
	root, origin := testClone(t)
	config := []byte(`repo: owner/name
primary: refs/heads/master
agents:
  builder: {poll: "true", interval: 1}
checks:
  - {name: test, run: 'sha=$(git rev-parse HEAD); git push --force origin HEAD:refs/forest/v1/request/$sha'}
`)
	writeTree(t, root, profileName+"/config.yaml", string(config))
	runGitDir(t, root, "add", ".iron-forest/config.yaml")
	runGitDir(t, root, "commit", "-m", "move request during checks")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	before := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master")))
	pushRequestForRevision(t, root, "if-request-lease", revision)
	checks, verdict := writeEvidencePayloads(t, revision, "approve")
	seedVerdictRun(t, root, "1-verifier")
	_, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier",
	})
	if err == nil || !publishConflict(err) {
		t.Fatalf("error=%v, want request lease conflict", err)
	}
	if got := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", evidenceRequestRefPrefix+revision))); got != revision {
		t.Fatalf("concurrent request update=%s want %s", got, revision)
	}
	if got := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master"))); got != before {
		t.Fatalf("master moved to %s", got)
	}
	requireMissingRemoteRef(t, root, evidenceChecksRefPrefix+revision)
	requireMissingRemoteRef(t, root, evidenceVerdictRefPrefix+revision)
}

func TestPublishVerdictSecondPublishConflicts(t *testing.T) {
	for _, kind := range []string{"checks", "verdict"} {
		t.Run(kind, func(t *testing.T) {
			root, _ := testClone(t)
			writePassingChecks(t, root)
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			pushRequestForRevision(t, root, "if-conflict", revision)
			checks, verdict := writeEvidencePayloads(t, revision, "changes")
			seedVerdictRun(t, root, "1-verifier")
			input := publishVerdictInput{Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier"}
			if _, err := publishVerdict(context.Background(), input); err != nil {
				t.Fatal(err)
			}
			before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
			if kind == "checks" {
				input.ChecksPath, _ = writeEvidencePayloadsWithResults(t, revision, "changes", `[{"name":"test","ok":false,"exit":2}]`)
			} else {
				input.VerdictPath = filepath.Join(t.TempDir(), "verdict.json")
				if err := os.WriteFile(input.VerdictPath, []byte(`{"schema":"forest.verdict.v1","revision":"`+revision+`","verdict":"changes","summary":"other","time":"2026-08-17T00:00:01Z"}`+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			_, err := publishVerdict(context.Background(), input)
			if !publishConflict(err) {
				t.Fatalf("error=%v, want conflict", err)
			}
			if got := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); got != before {
				t.Fatalf("remote refs changed after conflict:\n%s\nwant:\n%s", got, before)
			}
		})
	}
}

func TestPublishVerdictRejectsPartialEvidence(t *testing.T) {
	for _, kind := range []string{"checks", "verdict"} {
		t.Run(kind, func(t *testing.T) {
			root, _ := testClone(t)
			writePassingChecks(t, root)
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			pushRequestForRevision(t, root, "if-partial", revision)
			checks, verdict := writeEvidencePayloads(t, revision, "changes")
			path := checks
			if kind == "verdict" {
				path = verdict
			}
			payload, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			pushEvidence(t, root, kind, revision, string(payload), "Iron Forest Verifier", "verifier@forest.invalid")
			before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
			seedVerdictRun(t, root, "1-verifier")

			_, err = publishVerdict(context.Background(), publishVerdictInput{
				Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier",
			})
			if !publishConflict(err) {
				t.Fatalf("error=%v, want conflict", err)
			}
			if got := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); got != before {
				t.Fatalf("remote refs changed after partial-evidence conflict:\n%s\nwant:\n%s", got, before)
			}
		})
	}
}

func TestPublishVerdictIdenticalIsSuccess(t *testing.T) {
	for _, decision := range []string{"approve", "changes"} {
		t.Run(decision, func(t *testing.T) {
			root, _ := testClone(t)
			writePassingChecks(t, root)
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			pushRequestForRevision(t, root, "if-identical", revision)
			checks, verdict := writeEvidencePayloads(t, revision, decision)
			seedVerdictRun(t, root, "1-verifier")
			input := publishVerdictInput{Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier"}
			if _, err := publishVerdict(context.Background(), input); err != nil {
				t.Fatal(err)
			}
			before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
			dir := t.TempDir()
			pushAttempt := filepath.Join(dir, "push-attempt")
			receivePack := filepath.Join(dir, "reject-push")
			if err := os.WriteFile(receivePack, []byte("#!/bin/sh\n: > '"+pushAttempt+"'\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			runGitDir(t, root, "config", "remote.origin.receivepack", receivePack)

			result, err := publishVerdict(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "identical" {
				t.Fatalf("result=%#v", result)
			}
			if _, err := os.Stat(pushAttempt); !os.IsNotExist(err) {
				t.Fatalf("identical retry attempted another push: %v", err)
			}
			if got := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); got != before {
				t.Fatalf("remote refs changed on identical retry:\n%s\nwant:\n%s", got, before)
			}
		})
	}
}

func TestPublishVerdictApproveRejectsNonFastForward(t *testing.T) {
	root, origin := testClone(t)
	base := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushRequestForRevision(t, root, "if-non-fast-forward", revision)
	runGitDir(t, root, "checkout", "--detach", base)
	if err := os.WriteFile(filepath.Join(root, "other"), []byte("side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "other")
	runGitDir(t, root, "commit", "-m", "side")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/side")
	side := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGit(t, "--git-dir="+origin, "update-ref", "refs/heads/master", side)
	before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
	checks, verdict := writeEvidencePayloads(t, revision, "approve")
	seedVerdictRun(t, root, "1-verifier")
	_, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier",
	})
	if err == nil || !publishConflict(err) {
		t.Fatalf("error=%v, want conflict", err)
	}
	if got := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); got != before {
		t.Fatalf("remote refs changed after non-fast-forward refusal:\n%s\nwant:\n%s", got, before)
	}
}

func TestPublishVerdictApproveRunsChecks(t *testing.T) {
	root, _ := testClone(t)
	config := []byte(`repo: owner/name
primary: refs/heads/master
agents:
  builder: {poll: "true", interval: 1}
checks:
  - {name: test, run: "false"}
`)
	writeTree(t, root, profileName+"/config.yaml", string(config))
	runGitDir(t, root, "add", ".iron-forest/config.yaml")
	runGitDir(t, root, "commit", "-m", "failing checks")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushRequestForRevision(t, root, "if-runs-checks", revision)
	before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
	workingConfig := strings.Replace(string(config), `run: "false"`, `run: "true"`, 1)
	writeTree(t, root, profileName+"/config.yaml", workingConfig)
	checks, verdict := writeEvidencePayloads(t, revision, "approve")
	seedVerdictRun(t, root, "1-verifier")
	_, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier",
	})
	if err == nil {
		t.Fatal("approved a candidate whose configured check fails")
	}
	if got := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); got != before {
		t.Fatalf("remote refs changed after configured-check failure:\n%s\nwant:\n%s", got, before)
	}
}

func reviewRequestJSON(subject, revision, tracker string) string {
	payload := `{"schema":"forest.review-request.v2","subject":"` + subject + `","branch":"forest/` + subject + `/work","revision":"` + revision + `","time":"2026-08-29T00:00:00Z"`
	if tracker != "" {
		payload += `,"tracker":"` + tracker + `"`
	}
	return payload + `}`
}

func pushRequestForRevision(t *testing.T, root, subject, revision string) {
	t.Helper()
	branch := "forest/" + subject + "/work"
	request := pollReviewNoteBranch(revision, branch)
	pushEvidence(t, root, "request", revision, request, "Iron Forest Builder", "builder@forest.invalid")
	runGitDir(t, root, "push", "origin", revision+":refs/heads/"+branch)
}

func TestPublishVerdictApproveAndRetryIgnoreAmbientPowderCredentials(t *testing.T) {
	root, origin := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushEvidence(t, root, "request", revision, reviewRequestJSON("if-next", revision, "powder"), "Iron Forest Builder", "builder@forest.invalid")
	runGitDir(t, root, "push", "origin", revision+":refs/heads/forest/if-next/work")
	checks, verdict := writeEvidencePayloads(t, revision, "approve")
	seedVerdictRun(t, root, "1-verifier")
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "tracker-invoked")
	if err := os.WriteFile(filepath.Join(bin, "powder"), []byte("#!/bin/sh\nprintf invoked > \"$TRACKER_MARKER\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("TRACKER_MARKER", marker)
	t.Setenv("POWDER_AGENT", "forest-owner-name")
	t.Setenv("POWDER_URL", "https://powder.invalid")
	t.Setenv("POWDER_API_BASE_URL", "https://powder.invalid")
	t.Setenv("POWDER_API_KEY", "unused-test-key")

	result, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "published" || result.Revision != revision || result.Verdict != "approve" {
		t.Fatalf("result=%#v", result)
	}
	if got := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master"))); got != revision {
		t.Fatalf("master=%s want %s", got, revision)
	}
	before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))

	result, err = publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "identical" || result.Revision != revision || result.Verdict != "approve" {
		t.Fatalf("retry result=%#v", result)
	}
	if got := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); got != before {
		t.Fatalf("remote refs changed on identical retry:\n%s\nwant:\n%s", got, before)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("obsolete tracker invoked or marker unreadable: %v", err)
	}
}
