package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestBlobRefReadsDoNotCreateSharedLocalRef(t *testing.T) {
	repo := newRefGitRepo(t)
	ref := "refs/forest/retirement/branch/revision"
	if err := putBlobRef(repo, ref, "fact", ""); err != nil {
		t.Fatal(err)
	}
	type result struct {
		sha, body string
		err       error
	}
	results := make(chan result, 16)
	for range 16 {
		go func() {
			sha, body, err := getBlobRef(repo, ref)
			results <- result{sha: sha, body: body, err: err}
		}()
	}
	for range 16 {
		got := <-results
		if got.err != nil || got.sha != blobSHA("fact") || got.body != "fact" {
			t.Fatalf("concurrent ref read = %#v", got)
		}
	}
	if _, err := gitOut(repo, "show-ref", "--verify", ref); err == nil {
		t.Fatalf("ref read created shared local ref %s", ref)
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

func TestInternalEvidenceRefsBypassPrePushButSourcePushDoesNot(t *testing.T) {
	_, repo, sha := notesTestRepository(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	const key = "stalled-hook"
	if count, err := bumpAttempts(repo, key); err != nil || count != 1 {
		t.Fatalf("bumpAttempts = (%d, %v), want (1, nil)", count, err)
	}
	attemptRef := "refs/forest/attempt/" + key
	if out, err := gitOut(repo, "ls-remote", "origin", attemptRef); err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("attempt ref after bump = (%q, %v), want present", out, err)
	}
	if err := dropAttempts(repo, key); err != nil {
		t.Fatalf("dropAttempts = %v, want internal deletion to bypass pre-push", err)
	}
	if out, err := gitOut(repo, "ls-remote", "origin", attemptRef); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("attempt ref after drop = (%q, %v), want absent", out, err)
	}

	if err := writeVerdict(repo, sha, verdictNote{Verdict: "approve", Reviewer: "hook-test"}); err != nil {
		t.Fatalf("writeVerdict = %v, want notes publication to bypass pre-push", err)
	}
	noteRef := "refs/notes/forest/verdict"
	if out, err := gitOut(repo, "ls-remote", "origin", noteRef); err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("notes ref after write = (%q, %v), want present", out, err)
	}

	if err := git(repo, "push", "origin", "HEAD:refs/heads/source"); err == nil {
		t.Fatal("source push succeeded despite the failing pre-push hook")
	}
	if out, err := gitOut(repo, "ls-remote", "origin", "refs/heads/source"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("source ref after blocked push = (%q, %v), want absent", out, err)
	}
}
