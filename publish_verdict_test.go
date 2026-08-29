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
	dir := t.TempDir()
	ok := "true"
	exit := "0"
	if verdict == "changes" {
		ok = "false"
		exit = "1"
	}
	checks := `{"schema":"forest.checks.v1","revision":"` + revision + `","results":[{"name":"test","ok":` + ok + `,"exit":` + exit + `}],"time":"2026-08-17T00:00:00Z"}` + "\n"
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

func TestPublishVerdictChangesCreatesEvidenceRefs(t *testing.T) {
	root, origin := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	before := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master")))
	checks, verdict := writeEvidencePayloads(t, revision, "changes")
	result, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict,
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
	if strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", evidenceChecksRefPrefix+revision))) == "" {
		t.Fatal("missing checks ref")
	}
	if strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", evidenceVerdictRefPrefix+revision))) == "" {
		t.Fatal("missing verdict ref")
	}
}

func TestPublishVerdictApproveFastForwardsMaster(t *testing.T) {
	root, origin := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	checks, verdict := writeEvidencePayloads(t, revision, "approve")
	result, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict,
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
}

func TestPublishVerdictSecondPublishConflicts(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	checks, verdict := writeEvidencePayloads(t, revision, "changes")
	if _, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict,
	}); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "verdict.json")
	if err := os.WriteFile(other, []byte(`{"schema":"forest.verdict.v1","revision":"`+revision+`","verdict":"changes","summary":"other","time":"2026-08-17T00:00:01Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: other,
	})
	if !publishConflict(err) {
		t.Fatalf("error=%v, want conflict", err)
	}
}

func TestPublishVerdictIdenticalIsSuccess(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	checks, verdict := writeEvidencePayloads(t, revision, "changes")
	input := publishVerdictInput{Root: root, ChecksPath: checks, VerdictPath: verdict}
	if _, err := publishVerdict(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	result, err := publishVerdict(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "identical" {
		t.Fatalf("result=%#v", result)
	}
}

func TestPublishVerdictApproveRejectsNonFastForward(t *testing.T) {
	root, origin := testClone(t)
	base := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "checkout", "--detach", base)
	if err := os.WriteFile(filepath.Join(root, "other"), []byte("side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "other")
	runGitDir(t, root, "commit", "-m", "side")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/side")
	side := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGit(t, "--git-dir="+origin, "update-ref", "refs/heads/master", side)
	checks, verdict := writeEvidencePayloads(t, revision, "approve")
	_, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict,
	})
	if err == nil || !publishConflict(err) {
		t.Fatalf("error=%v, want conflict", err)
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
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "forest.yaml")
	runGitDir(t, root, "commit", "-m", "failing checks")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	checks, verdict := writeEvidencePayloads(t, revision, "approve")
	_, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict,
	})
	if err == nil || !strings.Contains(err.Error(), `check "test" failed`) {
		t.Fatalf("error=%v", err)
	}
}

func pushRequestForRevision(t *testing.T, root, subject, revision string) {
	t.Helper()
	branch := "forest/" + subject + "/work"
	request := `{"schema":"forest.review-request.v2","subject":"` + subject + `","branch":"` + branch + `","revision":"` + revision + `","time":"2026-08-29T00:00:00Z"}`
	pushEvidence(t, root, "request", revision, request, "Iron Forest Builder", "builder@forest.invalid")
	runGitDir(t, root, "push", "origin", revision+":refs/heads/"+branch)
}

func TestPublishVerdictPreservesLandedApproveWhilePowderRetries(t *testing.T) {
	root, origin := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushRequestForRevision(t, root, "if-next", revision)
	checks, verdict := writeEvidencePayloads(t, revision, "approve")
	lifecycle := &fakePowderLifecycle{doneFailure: 1}
	poller := configuredPowderPoller(t, root, "if-next", lifecycle)

	result, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict, Powder: poller,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "published" || result.PowderStatus != "pending" || result.PowderSubject != "if-next" {
		t.Fatalf("result=%#v", result)
	}
	if got := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master"))); got != revision {
		t.Fatalf("master=%s want %s", got, revision)
	}

	result, err = publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict, Powder: poller,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "identical" || result.PowderStatus != "terminal" || lifecycle.proof != revision {
		t.Fatalf("retry result=%#v proof=%q", result, lifecycle.proof)
	}
}

func TestPublishVerdictBlocksLaterApproveOnPendingCurrentPowder(t *testing.T) {
	root, origin := testClone(t)
	current := seedApprovedCurrent(t, root, "if-current")
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushRequestForRevision(t, root, "if-next", revision)
	checks, verdict := writeEvidencePayloads(t, revision, "approve")
	lifecycle := &fakePowderLifecycle{doneFailure: 1}
	poller := configuredPowderPoller(t, root, "if-current", lifecycle)

	_, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict, Powder: poller,
	})
	if err == nil || !strings.Contains(err.Error(), "reconcile current Powder Subject before approve") {
		t.Fatalf("error=%v", err)
	}
	if got := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master"))); got != current {
		t.Fatalf("master=%s want %s", got, current)
	}
	if got, oidErr := remoteOID(context.Background(), root, evidenceVerdictRefPrefix+revision); oidErr != nil || got != "" {
		t.Fatalf("new verdict oid=%q error=%v", got, oidErr)
	}
}
