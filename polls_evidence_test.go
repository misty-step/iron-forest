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
	addNote(t, root, "refs/notes/forest/verdict", tip, pollVerdictNote(tip, "changes"), "phaedrus", "phraznikov@gmail.com")
	runGitDir(t, root, "push", "origin", "refs/notes/forest/verdict"+":"+"refs/notes/forest/verdict")

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

func TestFixerPollSkipsLandedChangesVerdictDespiteStaleLocalMaster(t *testing.T) {
	root, _ := testClone(t)
	initial := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
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

	// Land the rejected work with a fixer revision on top of the same branch.
	// The origin branch keeps the rejected tip, while primary advances to the
	// descendant that contains it.
	if err := os.WriteFile(filepath.Join(root, "work"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "work")
	runGitDir(t, root, "commit", "-m", "fix")
	landed := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/master")

	// The local remote-tracking primary is deliberately stale: it points at the
	// original master tip, which does not contain the rejected tip. Only the
	// fetched advertised primary OID may decide ancestry.
	if landed == initial {
		t.Fatalf("fixer revision did not advance master")
	}
	runGitDir(t, root, "update-ref", "refs/remotes/origin/master", initial)

	code, err := NewPoller(root, "owner/name", Scope{}).fixer(context.Background())
	if code != exitNoWork || err != nil {
		t.Fatalf("fixer poll code=%d err=%v, want no work for landed rejected tip", code, err)
	}
}
