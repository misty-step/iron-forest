package main

import (
	"errors"
	"os"
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

func newLeaseGitRepo(t *testing.T) string {
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
	repo := newLeaseGitRepo(t)
	ref := "refs/forest/lease/item-11"
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
	if err := putBlobRef(repo, ref, "second", ""); !errors.Is(err, errLeaseHeld) {
		t.Fatalf("second create = %v, want lease held", err)
	}
	if err := putBlobRef(repo, ref, "second", sha); err != nil {
		t.Fatalf("compare-and-set update: %v", err)
	}
	_, content, err = getBlobRef(repo, ref)
	if err != nil || content != "second" {
		t.Fatalf("updated ref = %q %v, want second", content, err)
	}
}

func TestBlobRefMissingAndReleaseCompareAndSet(t *testing.T) {
	repo := newLeaseGitRepo(t)
	ref := "refs/forest/lease/missing"
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
	if err := pushLeaseDelete(repo, ref, blobSHA("other")); !errors.Is(err, errLeaseHeld) {
		t.Fatalf("wrong release = %v, want lease held", err)
	}
	if err := pushLeaseDelete(repo, ref, sha); err != nil {
		t.Fatalf("release: %v", err)
	}
	sha, content, err = getBlobRef(repo, ref)
	if err != nil || sha != "" || content != "" {
		t.Fatalf("released ref = %q %q %v, want empty", sha, content, err)
	}
}

func TestLeaseHolderDocumentUsesCurrentHostAndProcess(t *testing.T) {
	store := newMemoryLeaseStore()
	h, err := acquireLeaseFrom(store, Config{}, "item-12", "Verifier", "run-12")
	if err != nil {
		t.Fatal(err)
	}
	_, content, err := store.read(h.Ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"flow":"Verifier"`, `"run_id":"run-12"`, `"host":`, `"pid":`, `"time":`} {
		if !strings.Contains(content, required) {
			t.Errorf("holder missing %s: %s", required, content)
		}
	}
	if _, err := os.Hostname(); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyRefMigration(t *testing.T) {
	repo := newLeaseGitRepo(t)
	ref := "refs/heads/forest/claim/item-13"
	if err := os.WriteFile(filepath.Join(repo, "legacy.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "legacy.txt")
	runGitTest(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "legacy")
	runGitTest(t, repo, "push", "origin", "HEAD:"+ref)
	migrateLegacyClaims(repo, Config{})
	sha, content, err := getBlobRef(repo, ref)
	if err != nil || sha != "" || content != "" {
		t.Fatalf("retired ref = %q %q %v, want empty", sha, content, err)
	}
}
