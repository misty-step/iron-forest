package main

import (
	"bytes"
	"context"
	"errors"
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
	runGitDir(t, root, "commit", "-am", "failing check")

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

func TestPublishReviewRequestConflictsOnWhitespaceOnlyNote(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1-ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	payload := writeReviewPayload(t, root, revision, "forest/1-ready")
	if _, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1-ready", PayloadPath: payload, RunID: "1-builder",
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
		Root: root, Role: "builder", Branch: "forest/1-ready", PayloadPath: padded, RunID: "2-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting review-request note") {
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
		Root: root, Role: "builder", Branch: "forest/1-ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1-ready"), RunID: "1-builder",
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
		Root: root, Role: "builder", Branch: "forest/1-ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1-ready"), RunID: "1-builder",
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
		Root: root, Role: "builder", Branch: "forest/1-ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1-ready"), RunID: "1-builder",
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
		Root: root, Role: "builder", Branch: "forest/1-ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1-ready"), RunID: "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), `check "test" failed`) {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestKeepsCapturedPayloadIfCheckRewritesFile(t *testing.T) {
	root, _ := testClone(t)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	payload := writeReviewPayload(t, root, revision, "forest/1-ready")
	config := "repo: owner/name\nagents:\n  builder: {poll: \"true\", interval: 1}\nchecks:\n  - {name: test, run: \"printf TAMPERED > " + payload + "\"}\n"
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "check rewrites payload")
	revision = strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	original := []byte(`{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-ready","revision":"` + revision + `","time":"2026-08-15T00:00:00Z"}` + "\n")
	if err := os.WriteFile(payload, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1-ready", PayloadPath: payload, RunID: "1-builder",
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(payload); err != nil || string(got) != "TAMPERED" {
		t.Fatalf("payload file after check=%q err=%v", got, err)
	}
	runGitDir(t, root, "fetch", "origin", reviewRequestNoteRef+":"+reviewRequestNoteRef)
	shown := runGitDir(t, root, "notes", "--ref="+reviewRequestNoteRef, "show", revision)
	if bytes.Contains(shown, []byte("TAMPERED")) {
		t.Fatalf("published tampered payload: %q", shown)
	}
	if !bytes.Contains(shown, []byte(`"revision":"`+revision+`"`)) {
		t.Fatalf("published note=%q", shown)
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
			Root: root, Role: "builder", Branch: "forest/1-ready",
			PayloadPath: writeReviewPayload(t, root, revision, "forest/1-ready"), RunID: "1-builder",
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

func TestPublishReviewRequestRejectsEmptyNoteAuthor(t *testing.T) {
	name, email := parseNoteIdentity(" <nobody@invalid>")
	if name != "" || email != "nobody@invalid" {
		t.Fatalf("parseNoteIdentity=%q %q", name, email)
	}
	if validIdentity(noteEntry{Author: name, Email: email}, "builder", "fixer") {
		t.Fatal("empty author accepted")
	}
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1-ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	payload := writeReviewPayload(t, root, revision, "forest/1-ready")
	addNote(t, root, reviewRequestNoteRef, revision, string(mustRead(t, payload)), "Eve", "eve@invalid")
	runGitDir(t, root, "push", "origin", reviewRequestNoteRef+":"+reviewRequestNoteRef)
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1-ready", PayloadPath: payload, RunID: "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "wrong author identity") {
		t.Fatalf("error=%v", err)
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

func TestPublishCheckWorktreeIsReservedAndSweptAfterKill(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runID := "1786820000000000001-builder"
	dir := forestPath(root, "worktrees", runID+"-checks")
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "worktree", "add", "--detach", dir, "HEAD")
	if !isReservedRunID(runID + "-checks") {
		t.Fatal("check worktree name is not reserved")
	}
	if err := cleanupReservedResidue(root, NewRunner(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reserved check worktree survived: %v", err)
	}
	listed := string(runGitDir(t, root, "worktree", "list", "--porcelain"))
	if strings.Contains(listed, "-checks") {
		t.Fatalf("registered check worktree survived:\n%s", listed)
	}
}

func TestPublishCheckWorktreeKilledProcessIsSwept(t *testing.T) {
	if os.Getenv("FOREST_PUBLISH_CHILD") == "1" {
		root := os.Getenv("FOREST_PUBLISH_ROOT")
		revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
		_, _ = publishReviewRequest(context.Background(), publishReviewRequestInput{
			Root: root, Role: "builder", Branch: "forest/1-ready",
			PayloadPath: writeReviewPayload(t, root, revision, "forest/1-ready"),
			RunID:       "1786820000000000002-builder",
		})
		os.Exit(0)
	}
	root, _ := testClone(t)
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), []byte("repo: owner/name\nagents:\n  builder: {poll: \"true\", interval: 1}\nchecks:\n  - {name: test, run: \"sleep 30\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "slow check")
	cmd := exec.Command(os.Args[0], "-test.run=^TestPublishCheckWorktreeKilledProcessIsSwept$", "-test.v=false")
	cmd.Env = append(os.Environ(), "FOREST_PUBLISH_CHILD=1", "FOREST_PUBLISH_ROOT="+root)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	dir := forestPath(root, "worktrees", "1786820000000000002-builder-checks")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("check worktree did not appear")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("killed process left no worktree to sweep: %v", err)
	}
	if err := cleanupReservedResidue(root, NewRunner(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("killed check worktree survived: %v", err)
	}
}
