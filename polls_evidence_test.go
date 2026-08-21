package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pushEvidence(t *testing.T, root, kind, sha, payload, author, email string) {
	t.Helper()
	commit, err := commitEvidenceAs(context.Background(), root, evidenceFileName(kind), []byte(payload), "test "+kind+" "+sha, author, email)
	if err != nil {
		t.Fatal(err)
	}
	ref := evidenceKindRef(kind, sha)
	runGitDir(t, root, "push", "origin", commit+":"+ref)
}

func TestFixerPollIgnoresLegacyNotes(t *testing.T) {
	root, _ := testClone(t)
	runGitDir(t, root, "checkout", "-b", "forest/4/work")
	if err := os.WriteFile(filepath.Join(root, "work"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "work")
	runGitDir(t, root, "commit", "-m", "work")
	tip := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/4/work")
	addNote(t, root, verdictNoteRef, tip, pollVerdictNote(tip, "changes"), "phaedrus", "phraznikov@gmail.com")
	runGitDir(t, root, "push", "origin", verdictNoteRef+":"+verdictNoteRef)

	code, err := NewPoller(root, "owner/name", Scope{}).fixer(context.Background())
	if code != exitNoWork || err != nil {
		t.Fatalf("fixer poll code=%d err=%v, want no work despite leftover notes", code, err)
	}
}

func TestVerifierPollDispatchesOnRequestRef(t *testing.T) {
	root, _ := testClone(t)
	runGitDir(t, root, "checkout", "-b", "forest/4/work")
	if err := os.WriteFile(filepath.Join(root, "work"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "work")
	runGitDir(t, root, "commit", "-m", "work")
	tip := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/4/work")
	pushEvidence(t, root, "request", tip, pollReviewNote(tip), "Iron Forest Builder", "builder@forest.invalid")

	code, err := NewPoller(root, "owner/name", Scope{}).verifier(context.Background())
	if code != exitOK || err != nil {
		t.Fatalf("verifier poll code=%d err=%v", code, err)
	}
}

func TestFixerPollDispatchesOnChangesRef(t *testing.T) {
	root, _ := testClone(t)
	runGitDir(t, root, "checkout", "-b", "forest/4/work")
	if err := os.WriteFile(filepath.Join(root, "work"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "work")
	runGitDir(t, root, "commit", "-m", "work")
	tip := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/4/work")
	pushEvidence(t, root, "request", tip, pollReviewNote(tip), "Iron Forest Builder", "builder@forest.invalid")
	pushEvidence(t, root, "verdict", tip, pollVerdictNote(tip, "changes"), "Iron Forest Verifier", "verifier@forest.invalid")

	code, err := NewPoller(root, "owner/name", Scope{}).fixer(context.Background())
	if code != exitOK || err != nil {
		t.Fatalf("fixer poll code=%d err=%v", code, err)
	}
}
