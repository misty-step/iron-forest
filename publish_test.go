package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writePassingChecks(t *testing.T, root string) {
	t.Helper()
	config := []byte(`repo: owner/name
agents:
  builder: {poll: "true", interval: 1}
  fixer: {poll: "true", interval: 1}
checks:
  - {name: test, run: "true"}
`)
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeReviewPayload(t *testing.T, root, revision, branch string) string {
	t.Helper()
	payload := `{"schema":"forest.review-request.v1","issue":1,"branch":"` + branch + `","revision":"` + revision + `","time":"2026-08-15T00:00:00Z"}`
	path := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(path, []byte(payload+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPublishReviewRequestCreatesBranchAndNote(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1-ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	t.Setenv("FOREST_RUN_ID", "1-builder")
	result, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1-ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1-ready"),
		RunID:       "1-builder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "published" || result.Revision != revision {
		t.Fatalf("result=%#v", result)
	}
	remote := strings.TrimSpace(string(runGitDir(t, root, "ls-remote", "origin", "refs/heads/forest/1-ready")))
	if !strings.HasPrefix(remote, revision) {
		t.Fatalf("remote branch=%q", remote)
	}
	runGitDir(t, root, "fetch", "origin", reviewRequestNoteRef+":"+reviewRequestNoteRef)
	shown := string(runGitDir(t, root, "notes", "--ref="+reviewRequestNoteRef, "show", revision))
	if !strings.Contains(shown, `"schema":"forest.review-request.v1"`) {
		t.Fatalf("note=%q", shown)
	}
}

func TestPublishReviewRequestRefusesFailedCheck(t *testing.T) {
	root, _ := testClone(t)
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), []byte("repo: owner/name\nagents:\n  builder: {poll: \"true\", interval: 1}\nchecks:\n  - {name: test, run: \"false\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1-ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1-ready"),
		RunID:       "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), `check "test" failed`) {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestDetectsBranchRace(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1-ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1-ready")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "move")
	other := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "--force", "origin", "HEAD:refs/heads/forest/1-ready")
	runGitDir(t, root, "reset", "--hard", revision)
	writePassingChecks(t, root)
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1-ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1-ready"),
		RunID:       "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "branch race") {
		t.Fatalf("error=%v other=%s", err, other)
	}
}

func TestPublishReviewRequestRetriesCanonicalNoteRace(t *testing.T) {
	root, origin := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1-ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	work := t.TempDir()
	runGit(t, "clone", origin, work)
	configGit(t, work, "Iron Forest Builder", "builder@forest.invalid")
	if err := os.WriteFile(filepath.Join(work, "other"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, work, "add", "other")
	runGitDir(t, work, "commit", "-m", "competitor")
	competitor := strings.TrimSpace(string(runGitDir(t, work, "rev-parse", "HEAD")))
	runGitDir(t, work, "push", "origin", "HEAD:refs/heads/forest/9-other")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if printf '%s' \"$*\" | grep -q 'push --atomic' && [ ! -f \"$RACE_ONCE\" ]; then\n" +
		"  touch \"$RACE_ONCE\"\n" +
		"  \"" + realGit + "\" -C \"$ORIGIN\" -c user.name='Iron Forest Builder' -c user.email='builder@forest.invalid' notes --ref=refs/notes/forest/review-request add -m '{\"schema\":\"forest.review-request.v1\",\"issue\":9,\"branch\":\"forest/9-other\",\"revision\":\"" + competitor + "\",\"time\":\"2026-08-15T00:00:00Z\"}' \"" + competitor + "\"\n" +
		"  exit 1\n" +
		"fi\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrapperDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ORIGIN", origin)
	t.Setenv("RACE_ONCE", filepath.Join(t.TempDir(), "once"))
	result, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1-ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1-ready"),
		RunID:       "1-builder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempts < 2 {
		t.Fatalf("expected a retry, got %#v", result)
	}
}

func TestPublishReviewRequestFixerAdvancesRejectedBranch(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1-ready")
	rejected := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1-ready")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "fix")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	result, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "fixer",
		Branch:      "forest/1-ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1-ready"),
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

func TestCLIPublishReviewRequestNeedsRunID(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	payload := writeReviewPayload(t, root, revision, "forest/1-ready")
	t.Setenv("FOREST_RUN_ID", "")
	code, _, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"publish", "review-request", "builder", "forest/1-ready", payload, "--root", root})
	})
	if code != exitError || !strings.Contains(stderr, "FOREST_RUN_ID") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}
