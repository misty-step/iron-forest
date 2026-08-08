package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, strings.TrimSpace(string(out)))
	}
	return string(out)
}

func newRefGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	runGitTest(t, root, "init", "--bare", remote)
	runGitTest(t, root, "init", repo)
	runGitTest(t, repo, "remote", "add", "origin", remote)
	return repo
}

func TestBlobRefCreateReadAndCompareAndSet(t *testing.T) {
	repo := newRefGitRepo(t)
	ref := "refs/forest/attempt/item-11"
	if err := putBlobRef(repo, ref, "first", ""); err != nil {
		t.Fatalf("create ref: %v", err)
	}
	sha, content, err := getBlobRef(repo, ref)
	if err != nil {
		t.Fatalf("read ref: %v", err)
	}
	if sha != blobSHA("first") || content != "first" {
		t.Fatalf("read ref = %q %q, want %q %q", sha, content, blobSHA("first"), "first")
	}
	if err := putBlobRef(repo, ref, "second", ""); !errors.Is(err, errRefMoved) {
		t.Fatalf("second create = %v, want ref moved", err)
	}
	if err := putBlobRef(repo, ref, "second", sha); err != nil {
		t.Fatalf("compare-and-set update: %v", err)
	}
	_, content, err = getBlobRef(repo, ref)
	if err != nil || content != "second" {
		t.Fatalf("updated ref = %q %v, want second", content, err)
	}
}

func TestBlobRefMissingAndDeleteCompareAndSet(t *testing.T) {
	repo := newRefGitRepo(t)
	ref := "refs/forest/attempt/missing"
	sha, content, err := getBlobRef(repo, ref)
	if err != nil || sha != "" || content != "" {
		t.Fatalf("missing ref = %q %q %v, want empty", sha, content, err)
	}
	if err := putBlobRef(repo, ref, "held", ""); err != nil {
		t.Fatal(err)
	}
	sha, _, err = getBlobRef(repo, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteRef(repo, ref, blobSHA("other")); !errors.Is(err, errRefMoved) {
		t.Fatalf("wrong delete = %v, want ref moved", err)
	}
	if err := deleteRef(repo, ref, sha); err != nil {
		t.Fatalf("delete: %v", err)
	}
	sha, content, err = getBlobRef(repo, ref)
	if err != nil || sha != "" || content != "" {
		t.Fatalf("deleted ref = %q %q %v, want empty", sha, content, err)
	}
}

func TestListRefsEnumeratesPrefix(t *testing.T) {
	repo := newRefGitRepo(t)
	for _, ref := range []string{"refs/forest/attempt/a", "refs/forest/attempt/b", "refs/forest/stalled/a"} {
		if err := putBlobRef(repo, ref, ref, ""); err != nil {
			t.Fatalf("put %s: %v", ref, err)
		}
	}
	refs, err := listRefs(repo, "refs/forest/attempt/")
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("listed %d refs, want 2", len(refs))
	}
}

// newBranchRemoteRepo returns a repo whose origin has a branch named "forest/9-change"
// pointing at the given commit target ("" pushes an empty — tests pass a real sha).
func pushTestBranch(t *testing.T, repo, branch, target string) {
	t.Helper()
	runGitTest(t, repo, "push", "origin", target+":refs/heads/"+branch)
}

// TestDeleteBranchIfPresentIsIdempotentUnderRetry pins the cleanup idempotency of
// item #190: deleting a branch that is already gone (a retry after a partial
// finalisation) is a no-op, never a stale-ref error, so a concurrent pass cannot
// make reconciliation fail forever.
func TestDeleteBranchIfPresentIsIdempotentUnderRetry(t *testing.T) {
	repo := newRefGitRepo(t)
	// Materialise a commit so a branch can exist.
	runGitTest(t, repo, "config", "user.email", "t@example.com")
	runGitTest(t, repo, "config", "user.name", "test")
	runGitTest(t, repo, "commit", "--allow-empty", "-m", "root")
	head := strings.TrimSpace(runGitTest(t, repo, "rev-parse", "HEAD"))
	pushTestBranch(t, repo, "forest/9-change", head)
	// First deletion succeeds.
	if err := deleteBranchIfPresent(repo, "forest/9-change", head); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// A retry is a safe no-op instead of a stale-ref error.
	if err := deleteBranchIfPresent(repo, "forest/9-change", head); err != nil {
		t.Fatalf("retry delete of an already-removed branch: %v", err)
	}
}

// TestDropAttemptsIsIdempotentUnderRetry pins the same boundary for the attempt
// record: dropping an already-dropped record is a no-op, so a retry after a
// concurrent pass never fails forever on a stale compare-and-set.
func TestDropAttemptsIsIdempotentUnderRetry(t *testing.T) {
	repo := newRefGitRepo(t)
	ref := "refs/forest/attempt/item-11"
	if err := putBlobRef(repo, ref, "attempt", ""); err != nil {
		t.Fatalf("put attempt: %v", err)
	}
	if err := dropAttempts(repo, "item-11"); err != nil {
		t.Fatalf("first drop: %v", err)
	}
	if err := dropAttempts(repo, "item-11"); err != nil {
		t.Fatalf("retry drop of an already-dropped record: %v", err)
	}
}
