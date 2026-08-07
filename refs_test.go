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
