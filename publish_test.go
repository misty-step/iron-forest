package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePassingChecks(t *testing.T, root string) {
	t.Helper()
	config := []byte(`repo: owner/name
primary: refs/heads/master
agents:
  builder: {poll: "true", interval: 1}
  fixer: {poll: "true", interval: 1}
checks:
  - {name: test, run: "true"}
`)
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "forest.yaml")
	runGitDir(t, root, "commit", "-m", "passing checks")

}

func writeReviewPayload(t *testing.T, root, revision, branch string) string {
	return writeReviewPayloadTracker(t, root, revision, branch, "github")
}

func writeReviewPayloadTracker(t *testing.T, root, revision, branch, tracker string) string {
	t.Helper()
	payload := `{"schema":"forest.review-request.v2","subject":"` + reviewSubjectForTest(branch) + `","branch":"` + branch + `","revision":"` + revision + `","time":"2026-08-15T00:00:00Z","tracker":"` + tracker + `"}`
	path := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(path, []byte(payload+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func pushRejectedRequest(t *testing.T, root, rejected, branch, tracker string) {
	t.Helper()
	payload := `{"schema":"forest.review-request.v2","subject":"` + reviewSubjectForTest(branch) + `","branch":"` + branch + `","revision":"` + rejected + `","time":"2026-08-15T00:00:00Z"`
	if tracker != "" {
		payload += `,"tracker":"` + tracker + `"`
	}
	payload += `}`
	pushEvidence(t, root, "request", rejected, payload+"\n", "Iron Forest Builder", "builder@forest.invalid")
}

func fetchEvidence(t *testing.T, root, kind, sha string) string {
	t.Helper()
	ref := evidenceKindRef(kind, sha)
	if ref == "" || !isSHA(sha) {
		t.Fatalf("invalid evidence identity kind=%s sha=%s", kind, sha)
	}
	local := "refs/forest/test/" + kind + "-" + sha
	runGitDir(t, root, "fetch", "origin", "+"+ref+":"+local)
	t.Cleanup(func() { _ = exec.Command("git", "-C", root, "update-ref", "-d", local).Run() })
	return local
}

func fetchEvidenceFile(t *testing.T, root, kind, sha, name string) []byte {
	t.Helper()
	local := fetchEvidence(t, root, kind, sha)
	return runGitDir(t, root, "show", local+":"+name)
}

func fetchEvidenceIdentity(t *testing.T, root, kind, sha string) []byte {
	t.Helper()
	local := fetchEvidence(t, root, kind, sha)
	return runGitDir(t, root, "log", "-1", "--format=%an <%ae>", local)
}

func TestPublishReviewRequestCreatesBranchAndRequestRef(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	t.Setenv("FOREST_RUN_ID", "1-builder")
	result, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"),
		RunID:       "1-builder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "published" || result.Revision != revision {
		t.Fatalf("result=%#v", result)
	}
	remote := strings.TrimSpace(string(runGitDir(t, root, "ls-remote", "origin", "refs/heads/forest/1/ready")))
	if !strings.HasPrefix(remote, revision) {
		t.Fatalf("remote branch=%q", remote)
	}
	if noteRef := strings.TrimSpace(string(runGitDir(t, root, "ls-remote", "origin", "refs/notes/forest/review-request"))); noteRef != "" {
		t.Fatalf("unexpected review-request note ref=%q", noteRef)
	}
	shown := string(fetchEvidenceFile(t, root, "request", revision, "request.json"))
	if !strings.Contains(shown, `"schema":"forest.review-request.v2"`) {
		t.Fatalf("request evidence=%q", shown)
	}
}

func TestPublishReviewRequestAcceptsV2PowderSubject(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/iron-forest-ready/work")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	payload := `{"schema":"forest.review-request.v2","subject":"iron-forest-ready","branch":"forest/iron-forest-ready/work","revision":"` + revision + `","time":"2026-08-15T00:00:00Z","tracker":"powder"}`
	path := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(path, []byte(payload+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOREST_RUN_ID", "1-builder")
	result, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/iron-forest-ready/work",
		PayloadPath: path,
		RunID:       "1-builder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "published" || result.Revision != revision {
		t.Fatalf("result=%#v", result)
	}
	shown := string(fetchEvidenceFile(t, root, "request", revision, "request.json"))
	if !strings.Contains(shown, `"schema":"forest.review-request.v2"`) || !strings.Contains(shown, `"subject":"iron-forest-ready"`) || !strings.Contains(shown, `"tracker":"powder"`) {
		t.Fatalf("request evidence=%q", shown)
	}
}

func TestPublishReviewRequestRequiresTracker(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	payload := `{"schema":"forest.review-request.v2","subject":"1","branch":"forest/1/ready","revision":"` + revision + `","time":"2026-08-15T00:00:00Z"}`
	path := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(path, []byte(payload+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOREST_RUN_ID", "1-builder")
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1/ready",
		PayloadPath: path,
		RunID:       "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "tracker must be github or powder") {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestConflictsMismatchedRequestRef(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	other := `{"schema":"forest.review-request.v2","subject":"9","branch":"forest/1/ready","revision":"` + revision + `","time":"2026-08-17T00:00:00Z"}` + "\n"
	pushEvidence(t, root, "request", revision, other, "Iron Forest Builder", "builder@forest.invalid")
	t.Setenv("FOREST_RUN_ID", "1-builder")
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"),
		RunID:       "1-builder",
	})
	if !publishConflict(err) {
		t.Fatalf("error=%v, want conflicting request evidence", err)
	}
}

func TestPublishReviewRequestIgnoresHostileGitIdentity(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	t.Setenv("GIT_AUTHOR_NAME", "Eve")
	t.Setenv("GIT_AUTHOR_EMAIL", "eve@invalid")
	t.Setenv("GIT_COMMITTER_NAME", "Eve")
	t.Setenv("GIT_COMMITTER_EMAIL", "eve@invalid")
	if _, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"), RunID: "1-builder",
	}); err != nil {
		t.Fatal(err)
	}
	identity := strings.TrimSpace(string(fetchEvidenceIdentity(t, root, "request", revision)))
	if identity != "Iron Forest Builder <builder@forest.invalid>" {
		t.Fatalf("request actor=%q", identity)
	}
}

func TestPublishReviewRequestAcceptsIdenticalRequest(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	payload := mustRead(t, writeReviewPayload(t, root, revision, "forest/1/ready"))
	pushEvidence(t, root, "request", revision, string(payload), "Iron Forest Builder", "builder@forest.invalid")
	result, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"), RunID: "1-builder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "identical" || result.Revision != revision {
		t.Fatalf("result=%#v", result)
	}
}

func TestPublishReviewRequestRefusesFailedCheck(t *testing.T) {
	root, _ := testClone(t)
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), []byte("repo: owner/name\nagents:\n  builder: {poll: \"true\", interval: 1}\nchecks:\n  - {name: test, run: \"false\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "failing check")

	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"),
		RunID:       "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), `check "test" failed`) {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestUsesTrustedSecretsScanner(t *testing.T) {
	root, _ := testClone(t)
	origLook, origRun := scanEnv.lookPath, scanEnv.runGeneric
	scanEnv.lookPath = func(string, string) (string, error) { return "stub", nil }
	scanEnv.runGeneric = func(context.Context, string, string, []string) ([]secretFinding, error) {
		return []secretFinding{{Path: "fixture.txt", Rule: "TestSecret"}}, nil
	}
	defer func() { scanEnv.lookPath, scanEnv.runGeneric = origLook, origRun }()

	// The candidate carries a neutered scanner and a planted fixture, and it
	// declares no `secrets` check. The Gate must still run its own scanner and
	// fail on the fixture rather than compiling candidate code or waiting for a
	// candidate-defined check name.
	writeTree(t, root, "scansecrets.go", "package main\nfunc scanSecretsTree(string) ([]secretFinding, error) { return nil, nil }\n")
	writeTree(t, root, "fixture.txt", "PLANTED-CREDENTIAL-FIXTURE\n")
	config := []byte(`repo: owner/name
primary: refs/heads/master
agents:
  builder: {poll: "true", interval: 1}
  fixer: {poll: "true", interval: 1}
checks:
  - {name: test, run: "true"}
`)
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "forest.yaml", "scansecrets.go", "fixture.txt")
	runGitDir(t, root, "commit", "-m", "candidate neutered scanner and planted fixture")
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	t.Setenv("FOREST_RUN_ID", "1-builder")
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"),
		RunID:       "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "secrets scan") || !strings.Contains(err.Error(), "fixture.txt") {
		t.Fatalf("error=%v, want the trusted secrets scan to fail on the planted fixture", err)
	}
	if strings.Contains(err.Error(), "PLANTED-CREDENTIAL-FIXTURE") {
		t.Fatalf("error leaked the planted credential value: %v", err)
	}
}

func TestPublishReviewRequestCleansCanceledSecretsScanWorktree(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)

	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	t.Setenv("HEARTBEAT", heartbeat)
	binDir := t.TempDir()
	script := "#!/bin/sh\nwhile :; do printf x >> \"$HEARTBEAT\"; /bin/sleep 0.02; done\n"
	if err := os.WriteFile(filepath.Join(binDir, secretScanner), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := publishReviewRequest(ctx, publishReviewRequestInput{
			Root:        root,
			Role:        "builder",
			Branch:      "forest/1/ready",
			PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"),
			RunID:       "1-builder",
		})
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		listed := string(runGitDir(t, root, "worktree", "list", "--porcelain"))
		if strings.Contains(listed, "-checks") {
			cancel()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("secrets scan worktree did not appear")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation")
		}
	case <-time.After(cleanupTimeout + 2*time.Second):
		t.Fatal("publish did not return after cancel")
	}
	listed := string(runGitDir(t, root, "worktree", "list", "--porcelain"))
	if strings.Contains(listed, "-checks") {
		t.Fatalf("stale worktree remains:\n%s", listed)
	}
}

func TestPublishReviewRequestDetectsBranchRace(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "move")
	other := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "--force", "origin", "HEAD:refs/heads/forest/1/ready")
	runGitDir(t, root, "reset", "--hard", revision)
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"),
		RunID:       "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "branch race") {
		t.Fatalf("error=%v other=%s", err, other)
	}
}

func TestPublishReviewRequestKeepsPolicyRejectionDistinct(t *testing.T) {
	root, origin := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	hook := filepath.Join(origin, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'policy rejects this ref' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"),
		RunID:       "1-builder",
	})
	if err == nil {
		t.Fatal("expected remote policy rejection")
	}
	if publishConflict(err) {
		t.Fatalf("policy rejection must not be classified as a branch race: %v", err)
	}
	if strings.Contains(err.Error(), "branch race") {
		t.Fatalf("policy rejection must not emit a branch race: %v", err)
	}
	if !strings.Contains(err.Error(), "policy rejects this ref") {
		t.Fatalf("policy rejection diagnostic lost: %v", err)
	}
}

func TestClassifyReviewPushNarrowsToLeaseEvidence(t *testing.T) {
	tests := []struct {
		name         string
		output       string
		wantConflict bool
	}{
		{name: "stale lease", output: "! 0000000000000000000000000000000000000000:refs/heads/forest/1/ready\t[rejected] (stale info)", wantConflict: true},
		{name: "non fast forward", output: "! 0000000000000000000000000000000000000000:refs/heads/forest/1/ready\t[rejected] (non-fast-forward)", wantConflict: true},
		{name: "remote policy", output: "! 0000000000000000000000000000000000000000:refs/heads/forest/1/ready\t[remote rejected] (pre-receive hook declined)", wantConflict: false},
		{name: "transport failure without porcelain", output: "", wantConflict: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := errors.New("git push failed")
			got := classifyReviewPush([]byte(test.output), base)
			if publishConflict(got) != test.wantConflict {
				t.Fatalf("conflict=%v want %v (err=%v)", publishConflict(got), test.wantConflict, got)
			}
			if !test.wantConflict && !errors.Is(got, base) {
				t.Fatalf("original error lost: %v", got)
			}
		})
	}
}

func TestPublishReviewRequestRejectsAncestorBranchCreatedDuringPush(t *testing.T) {
	root, origin := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	parent := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", parent+":refs/heads/master")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("child\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "child")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	script := "#!/bin/sh\nset -e\n" +
		"if printf '%s' \"$*\" | grep -q -- '--atomic'; then\n" +
		"  \"" + realGit + "\" -C \"" + origin + "\" update-ref refs/heads/forest/1/ready " + parent + "\n" +
		"fi\n" +
		"exec \"" + realGit + "\" \"$@\"\n"

	if err := os.WriteFile(filepath.Join(wrapperDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err = publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"), RunID: "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "branch race") {
		t.Fatalf("error=%v", err)
	}
	remote := strings.TrimSpace(string(runGitDir(t, origin, "rev-parse", "refs/heads/forest/1/ready")))
	if remote != parent {
		t.Fatalf("origin branch=%s want parent=%s", remote, parent)
	}
}

func TestPublishReviewRequestBuilderRejectsBranchWithoutRequestRef(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"), RunID: "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "branch race") {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestFixerRejectsBranchWithoutRequestRef(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	rejected := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushRejectedRequest(t, root, rejected, "forest/1/ready", "github")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "fix")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "fixer", Branch: "forest/1/ready", Rejected: rejected,
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"), RunID: "2-fixer",
	})
	if err == nil || !strings.Contains(err.Error(), "branch race") {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestFixerAdvancesRejectedBranch(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	rejected := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushRejectedRequest(t, root, rejected, "forest/1/ready", "github")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "fix")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	result, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "fixer",
		Branch:      "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"),
		Rejected:    rejected,
		RunID:       "2-fixer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "published" || result.Revision != revision {
		t.Fatalf("result=%#v", result)
	}
}

func TestPublishReviewRequestFixerRejectsTrackerFlip(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	rejected := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushRejectedRequest(t, root, rejected, "forest/1/ready", "github")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "fix")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "fixer", Branch: "forest/1/ready", Rejected: rejected,
		PayloadPath: writeReviewPayloadTracker(t, root, revision, "forest/1/ready", "powder"), RunID: "2-fixer",
	})
	if err == nil || !strings.Contains(err.Error(), `fixer tracker "powder" does not match rejected request "github"`) {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestFixerStampsGithubOnTrackerlessRejected(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	rejected := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushRejectedRequest(t, root, rejected, "forest/1/ready", "")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "fix")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	result, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "fixer", Branch: "forest/1/ready", Rejected: rejected,
		PayloadPath: writeReviewPayloadTracker(t, root, revision, "forest/1/ready", "github"), RunID: "2-fixer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "published" {
		t.Fatalf("result=%#v", result)
	}
}

func TestCLIPublishReviewRequestNeedsRunID(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	payload := writeReviewPayload(t, root, revision, "forest/1/ready")
	t.Setenv("FOREST_RUN_ID", "")
	code, _, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"publish", "review-request", "builder", "forest/1/ready", payload, "--root", root})
	})
	if code != exitError || !strings.Contains(stderr, "FOREST_RUN_ID") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestCLIPublishReviewRequestFixerBranchRaceIsConflict(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	rejected := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushRejectedRequest(t, root, rejected, "forest/1/ready", "github")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "fix")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	payload := writeReviewPayload(t, root, revision, "forest/1/ready")
	t.Setenv("FOREST_RUN_ID", "2-fixer")
	code, stdout, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"publish", "review-request", "fixer", "forest/1/ready", payload, "--rejected", rejected, "--json", "--root", root})
	})
	if code != exitConflict || !strings.Contains(stdout+stderr, "branch race") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestPublishReviewRequestConflictsOnWhitespaceOnlyRequestRef(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	payload := writeReviewPayload(t, root, revision, "forest/1/ready")
	if _, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready", PayloadPath: payload, RunID: "1-builder",
	}); err != nil {
		t.Fatal(err)
	}
	padded := filepath.Join(t.TempDir(), "padded.json")
	raw, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(padded, append(raw, '\n', '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready", PayloadPath: padded, RunID: "2-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting request evidence") {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestRefusesRepositoryGit(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	if err := os.WriteFile(filepath.Join(root, "git"), []byte("#!/bin/sh\necho PLANTED >&2\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"), RunID: "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "refuse repository executable") {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestRefusesRepositorySh(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	if err := os.WriteFile(filepath.Join(root, "sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"), RunID: "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "refuse repository executable") {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestIgnoresRepositoryGo(t *testing.T) {
	root, _ := testClone(t)
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), []byte("repo: owner/name\nagents:\n  builder: {poll: \"true\", interval: 1}\nchecks:\n  - {name: test, run: \"go planted\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "check uses go")
	if err := os.WriteFile(filepath.Join(root, "go"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"), RunID: "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), `check "test" failed`) {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestIgnoresUncommittedCheckPass(t *testing.T) {
	root, _ := testClone(t)
	config := []byte(`repo: owner/name
agents:
  builder: {poll: "true", interval: 1}
checks:
  - {name: test, run: "grep -qx dirty file"}
`)
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "check requires dirty file")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"), RunID: "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), `check "test" failed`) {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestKeepsCapturedPayloadIfCheckRewritesFile(t *testing.T) {
	root, _ := testClone(t)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	payload := writeReviewPayload(t, root, revision, "forest/1/ready")
	config := "repo: owner/name\nagents:\n  builder: {poll: \"true\", interval: 1}\nchecks:\n  - {name: test, run: \"printf TAMPERED > " + payload + "\"}\n"
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "check rewrites payload")
	revision = strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	original := []byte(`{"schema":"forest.review-request.v2","subject":"1","branch":"forest/1/ready","revision":"` + revision + `","time":"2026-08-15T00:00:00Z","tracker":"github"}` + "\n")
	if err := os.WriteFile(payload, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready", PayloadPath: payload, RunID: "1-builder",
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(payload); err != nil || string(got) != "TAMPERED" {
		t.Fatalf("payload file after check=%q err=%v", got, err)
	}
	shown := fetchEvidenceFile(t, root, "request", revision, "request.json")
	if bytes.Contains(shown, []byte("TAMPERED")) {
		t.Fatalf("published tampered payload: %q", shown)
	}
	if !bytes.Contains(shown, []byte(`"revision":"`+revision+`"`)) {
		t.Fatalf("published request evidence=%q", shown)
	}
}
func TestPublishReviewRequestCleansCanceledCheckWorktree(t *testing.T) {
	root, _ := testClone(t)
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), []byte("repo: owner/name\nagents:\n  builder: {poll: \"true\", interval: 1}\nchecks:\n  - {name: test, run: \"sleep 30\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "slow check")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := publishReviewRequest(ctx, publishReviewRequestInput{
			Root: root, Role: "builder", Branch: "forest/1/ready",
			PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"), RunID: "1-builder",
		})
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		listed := string(runGitDir(t, root, "worktree", "list", "--porcelain"))
		if strings.Contains(listed, "-checks") {
			cancel()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("check worktree did not appear")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation")
		}
	case <-time.After(cleanupTimeout + 2*time.Second):
		t.Fatal("publish did not return after cancel")
	}
	listed := string(runGitDir(t, root, "worktree", "list", "--porcelain"))
	if strings.Contains(listed, "-checks") {
		t.Fatalf("stale worktree remains:\n%s", listed)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func reservedCheckDirs(root string) []string {
	entries, err := os.ReadDir(forestPath(root, "worktrees"))
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && isReservedRunID(entry.Name()) && strings.HasSuffix(entry.Name(), "-checks") {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestPublishCheckWorktreeIsReservedAndSweptAfterKill(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	dir := forestPath(root, "worktrees", newRunID("checks", time.Unix(1, 0)))
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "worktree", "add", "--detach", dir, "HEAD")
	if !isReservedRunID(filepath.Base(dir)) {
		t.Fatal("check worktree name is not reserved")
	}
	if err := cleanupReservedResidue(root, NewRunner(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reserved check worktree survived: %v", err)
	}
}

func TestPublishCheckWorktreeFromLinkedRunIsSweptOnPrimary(t *testing.T) {
	if os.Getenv("FOREST_PUBLISH_CHILD") == "1" {
		root := os.Getenv("FOREST_PUBLISH_ROOT")
		revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
		_, _ = publishReviewRequest(context.Background(), publishReviewRequestInput{
			Root: root, Role: "builder", Branch: "forest/1/ready",
			PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready"),
			RunID:       "manual",
		})
		os.Exit(0)
	}
	primary, _ := testClone(t)
	if err := os.WriteFile(filepath.Join(primary, "forest.yaml"), []byte("repo: owner/name\nagents:\n  builder: {poll: \"true\", interval: 1}\nchecks:\n  - {name: test, run: \"sleep 30\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, primary, "commit", "-am", "slow check")
	linked := filepath.Join(t.TempDir(), "1786820000000000002-builder")
	runGitDir(t, primary, "worktree", "add", "--detach", linked, "HEAD")
	cmd := exec.Command(os.Args[0], "-test.run=^TestPublishCheckWorktreeFromLinkedRunIsSweptOnPrimary$", "-test.v=false")
	cmd.Env = append(os.Environ(), "FOREST_PUBLISH_CHILD=1", "FOREST_PUBLISH_ROOT="+linked)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var found []string
	for {
		found = reservedCheckDirs(primary)
		if len(found) > 0 {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("primary check worktree did not appear")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(forestPath(linked, "worktrees")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("check worktree was nested under the linked Run worktree")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
	if err := cleanupReservedResidue(primary, NewRunner(primary)); err != nil {
		t.Fatal(err)
	}
	if leftover := reservedCheckDirs(primary); len(leftover) != 0 {
		t.Fatalf("primary check worktrees survived: %v", leftover)
	}
}

func TestPublishReviewRequestCLI(t *testing.T) {
	t.Run("human publish and identical republish", func(t *testing.T) {
		t.Setenv("FOREST_RUN_ID", "1-builder")
		root, _ := testClone(t)
		writePassingChecks(t, root)
		runGitDir(t, root, "checkout", "-b", "forest/1/ready")
		revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
		payload := writeReviewPayload(t, root, revision, "forest/1/ready")

		code, stdout, stderr := captureCLIOutput(t, func() int {
			return runSurfaceCommand([]string{"publish", "review-request", "builder", "forest/1/ready", payload, "--root", root})
		})
		if code != exitOK {
			t.Fatalf("code=%d, want %d (stderr=%q)", code, exitOK, stderr)
		}
		if want := fmt.Sprintf("published review-request %s on %s\n", revision, "forest/1/ready"); stdout != want {
			t.Fatalf("stdout=%q, want %q", stdout, want)
		}
		if stderr != "" {
			t.Fatalf("stderr=%q, want empty", stderr)
		}

		code, stdout, stderr = captureCLIOutput(t, func() int {
			return runSurfaceCommand([]string{"publish", "review-request", "builder", "forest/1/ready", payload, "--root", root})
		})
		if code != exitOK {
			t.Fatalf("republish code=%d, want %d (stderr=%q)", code, exitOK, stderr)
		}
		if want := fmt.Sprintf("accepted identical review-request %s on %s\n", revision, "forest/1/ready"); stdout != want {
			t.Fatalf("republish stdout=%q, want %q", stdout, want)
		}
		if stderr != "" {
			t.Fatalf("republish stderr=%q, want empty", stderr)
		}
	})

	t.Run("json envelope publish and identical republish", func(t *testing.T) {
		t.Setenv("FOREST_RUN_ID", "1-builder")
		root, _ := testClone(t)
		writePassingChecks(t, root)
		runGitDir(t, root, "checkout", "-b", "forest/1/ready")
		revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
		payload := writeReviewPayload(t, root, revision, "forest/1/ready")

		code, envelope, stderr := decodeEnvelope(t, "publish", "review-request", "builder", "forest/1/ready", payload, "--json", "--root", root)
		if code != exitOK {
			t.Fatalf("code=%d, want %d (stderr=%q)", code, exitOK, stderr)
		}
		if stderr != "" {
			t.Fatalf("stderr=%q, want empty", stderr)
		}
		keys := payloadKeys(t, envelope)
		if keys["status"] != "published" || keys["revision"] != revision || keys["branch"] != "forest/1/ready" {
			t.Fatalf("payload=%v, want status=published revision=%s branch=forest/1/ready", keys, revision)
		}

		code, envelope, stderr = decodeEnvelope(t, "publish", "review-request", "builder", "forest/1/ready", payload, "--json", "--root", root)
		if code != exitOK {
			t.Fatalf("republish code=%d, want %d (stderr=%q)", code, exitOK, stderr)
		}
		if stderr != "" {
			t.Fatalf("republish stderr=%q, want empty", stderr)
		}
		keys = payloadKeys(t, envelope)
		if keys["status"] != "identical" || keys["revision"] != revision || keys["branch"] != "forest/1/ready" {
			t.Fatalf("republish payload=%v, want status=identical revision=%s branch=forest/1/ready", keys, revision)
		}
	})
}
