package main

import (
	"errors"
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
