package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNotesRoundTripImmutableAndCommitScope(t *testing.T) {
	remote, work, sha := notesTestRepository(t)

	if _, ok, err := readVerdict(work, sha); err != nil || ok {
		t.Fatalf("absent verdict = (%v, %v), want (false, nil)", ok, err)
	}
	secondClone := filepath.Join(t.TempDir(), "second")
	notesTestGit(t, "", "clone", remote, secondClone)
	first := verdictNote{
		Verdict:  "approve",
		Notes:    "first decision",
		Reviewer: "reviewer-a",
		Model:    "model-a",
		DefSHA:   "def-a",
		RunID:    "run-a",
	}
	if err := writeVerdict(work, sha, first); err != nil {
		t.Fatal(err)
	}
	got, ok, err := readVerdict(work, sha)
	if err != nil || !ok {
		t.Fatalf("written verdict = (%v, %v), want (true, nil)", ok, err)
	}
	if got.Verdict != first.Verdict || got.Notes != first.Notes || got.RunID != first.RunID {
		t.Fatalf("written verdict = %#v, want fields from %#v", got, first)
	}
	if _, err := time.Parse(time.RFC3339, got.Time); err != nil {
		t.Fatalf("verdict time %q is not RFC3339: %v", got.Time, err)
	}
	if !strings.HasSuffix(got.Time, "Z") {
		t.Fatalf("verdict time %q is not UTC", got.Time)
	}
	raw := notesTestGitOutput(t, work, "notes", "--ref=forest/verdict", "show", sha)
	if strings.Contains(strings.TrimSuffix(raw, "\n"), "\n") {
		t.Fatalf("verdict note has multiple lines: %q", raw)
	}

	replacement := first
	replacement.Verdict = "changes"
	replacement.Notes = "replacement decision"
	if err := writeVerdict(secondClone, sha, replacement); !errors.Is(err, errNoteExists) {
		t.Fatalf("second verdict write = %v, want errNoteExists", err)
	}
	fromSecond, ok, err := readVerdict(secondClone, sha)
	if err != nil || !ok {
		t.Fatalf("losing clone verdict = (%v, %v), want (true, nil)", ok, err)
	}
	if fromSecond.Verdict != first.Verdict || fromSecond.Time != got.Time {
		t.Fatalf("losing clone verdict = %#v, want %#v", fromSecond, got)
	}
	unchanged, ok, err := readVerdict(work, sha)
	if err != nil || !ok {
		t.Fatalf("verdict after refused rewrite = (%v, %v), want (true, nil)", ok, err)
	}
	if unchanged.Verdict != first.Verdict || unchanged.Notes != first.Notes || unchanged.RunID != first.RunID {
		t.Fatalf("verdict after refused rewrite = %#v, want first value %#v", unchanged, first)
	}
	if unchanged.Time != got.Time {
		t.Fatalf("verdict time after refused rewrite = %q, want %q", unchanged.Time, got.Time)
	}

	checks := checksNote{
		Status:  "pass",
		Results: []checkResult{{Name: "test", Code: 0, Seconds: 1.25, Output: "ok"}},
		RunID:   "run-checks",
	}
	if err := writeChecks(work, sha, checks); err != nil {
		t.Fatal(err)
	}
	if err := fetchNotes(secondClone); err != nil {
		t.Fatal(err)
	}
	if gotChecks, ok, err := readChecks(secondClone, sha); err != nil || !ok || gotChecks.Status != checks.Status {
		t.Fatalf("checks = (%#v, %v, %v), want pass note", gotChecks, ok, err)
	}

	if err := os.WriteFile(filepath.Join(work, "second.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "add", "second.txt")
	notesTestGit(t, work, "commit", "-m", "second")
	otherSHA := notesTestGitOutput(t, work, "rev-parse", "HEAD")
	if _, ok, err := readVerdict(work, otherSHA); err != nil || ok {
		t.Fatalf("verdict for another commit = (%v, %v), want (false, nil)", ok, err)
	}
	if _, ok, err := readChecks(work, otherSHA); err != nil || ok {
		t.Fatalf("checks for another commit = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestNotesAttemptsStartAtZeroAndIncrement(t *testing.T) {
	_, work, _ := notesTestRepository(t)
	key := "branch-forest/7-example"
	const head = "head-a"
	if got, err := readAttempts(work, key, head); err != nil || got != 0 {
		t.Fatalf("initial attempts = (%d, %v), want (0, nil)", got, err)
	}
	if got, err := bumpAttempts(work, key, head); err != nil || got != 1 {
		t.Fatalf("first bump = (%d, %v), want (1, nil)", got, err)
	}
	if got, err := bumpAttempts(work, key, head); err != nil || got != 2 {
		t.Fatalf("second bump = (%d, %v), want (2, nil)", got, err)
	}
	if got, err := readAttempts(work, key, head); err != nil || got != 2 {
		t.Fatalf("stored attempts = (%d, %v), want (2, nil)", got, err)
	}
	// The count keys to the head it was recorded against: an operator repairing a
	// branch by hand moves the head, and the new head reads a fresh budget.
	if got, err := readAttempts(work, key, "head-b"); err != nil || got != 0 {
		t.Fatalf("attempts for a new, operator-repaired head = (%d, %v), want (0, nil)", got, err)
	}
}

func TestNotesAttemptsCASRetriesAreBounded(t *testing.T) {
	remote, work, _ := notesTestRepository(t)
	key := "branch-forest/7-example"
	if _, err := bumpAttempts(work, key, "head-a"); err != nil {
		t.Fatal(err)
	}

	countPath := filepath.Join(t.TempDir(), "hook-count")
	hook := filepath.Join(remote, "hooks", "update")
	script := "#!/bin/sh\nprintf x >> " + countPath + "\n"
	script += "new=$(printf '{\"count\":999}' | git hash-object -w --stdin)\n"
	script += "git update-ref \"$1\" \"$new\"\n"
	script += "echo 'stale info' >&2\nexit 1\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := bumpAttempts(work, key, "head-a")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, errRefMoved) {
			t.Fatalf("bounded CAS result = %v, want ref-moved error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bumpAttempts exceeded its retry bound")
	}
	count, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(count); got != 5 {
		t.Fatalf("CAS push count = %d, want 5", got)
	}
}

func notesTestRepository(t *testing.T) (remote, work, sha string) {
	t.Helper()
	root := t.TempDir()
	remote = filepath.Join(root, "remote.git")
	work = filepath.Join(root, "work")
	notesTestGit(t, "", "init", "--bare", "--initial-branch=master", remote)
	notesTestGit(t, "", "clone", remote, work)
	notesTestGit(t, work, "config", "user.name", "notes-test")
	notesTestGit(t, work, "config", "user.email", "notes-test@example.com")
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "add", "file.txt")
	notesTestGit(t, work, "commit", "-m", "first")
	notesTestGit(t, work, "push", "-u", "origin", "HEAD:master")
	sha = notesTestGitOutput(t, work, "rev-parse", "HEAD")
	return remote, work, sha
}

func notesTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmdArgs := args
	if dir != "" {
		cmdArgs = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", cmdArgs...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(cmdArgs, " "), err, strings.TrimSpace(string(out)))
	}
}

func notesTestGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", cmdArgs...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(cmdArgs, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// TestNotesRejectedPushIsNotDurable pins the #189 fix: a note that never
// reached the remote is not a fact. The server rejects the notes push, so the
// write fails and the abandoned local note must not be read as the winner on a
// later pass.
func TestNotesRejectedPushIsNotDurable(t *testing.T) {
	remote, work, sha := notesTestRepository(t)
	hook := filepath.Join(remote, "hooks", "update")
	script := "#!/bin/sh\ncase \"$1\" in\n  refs/notes/forest/verdict)\n    echo 'verdict notes blocked' >&2\n    exit 1\n    ;;\nesac\nexit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeVerdict(work, sha, verdictNote{
		Verdict: "approve", Notes: "local only", Reviewer: "reviewer-a", RunID: "run-a",
	}); err == nil {
		t.Fatal("writeVerdict succeeded though the notes push is rejected")
	} else if errors.Is(err, errNoteExists) {
		t.Fatalf("writeVerdict = errNoteExists (%v); a rejected push is not a remote win", err)
	}
	// The next pass must not read the abandoned local note as a durable fact.
	if err := fetchNotes(work); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readVerdict(work, sha); err != nil || ok {
		t.Fatalf("verdict after rejected push = (present=%v, err=%v), want absent", ok, err)
	}
}

// TestNotesPreExistingLocalNoteWinsOnRetry pins the #189 fix's retry path: a
// local note left behind by a failed push becomes the durable value once the
// retry lands. A caller proposing a different value must see errNoteExists and
// reread the older local note instead of trusting its in-memory verdict.
func TestNotesPreExistingLocalNoteWinsOnRetry(t *testing.T) {
	_, work, sha := notesTestRepository(t)

	// Install a local note directly, exactly as a failed push would leave it,
	// without letting it reach the remote.
	local := verdictNote{Verdict: "approve", Notes: "pre-existing local", Reviewer: "reviewer-a", RunID: "run-local"}
	body, err := json.Marshal(local)
	if err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "notes", "--ref=forest/verdict", "add", "-m", string(body), sha)

	// This caller proposes a different value. The write must report a win so the
	// caller rereads the durable (older local) note rather than its proposal.
	proposed := verdictNote{Verdict: "reject", Notes: "proposed", Reviewer: "reviewer-b", RunID: "run-b"}
	if err := writeVerdict(work, sha, proposed); !errors.Is(err, errNoteExists) {
		t.Fatalf("writeVerdict retry = %v, want errNoteExists so the caller rereads the durable note", err)
	}
	read, ok, err := readVerdict(work, sha)
	if err != nil || !ok {
		t.Fatalf("verdict after retry = (present=%v, err=%v), want the durable local note", ok, err)
	}
	if read.Verdict != local.Verdict || read.Notes != local.Notes {
		t.Fatalf("winning verdict = %#v, want the pre-existing local note %#v, not the proposed %#v", read, local, proposed)
	}
}

// TestDropAttemptsRetiresTheRecord pins the leak this factory has already paid
// for once: a retired subject must not leave a ref behind. The previous claim
// scheme left one on every failure and made those items unworkable forever.
func TestDropAttemptsRetiresTheRecord(t *testing.T) {
	_, work, _ := notesTestRepository(t)
	const key = "branch-forest/9-example"

	if err := dropAttempts(work, key); err != nil {
		t.Fatalf("dropAttempts on an absent record = %v, want nil", err)
	}
	if _, err := bumpAttempts(work, key, "head-a"); err != nil {
		t.Fatal(err)
	}
	if n, err := readAttempts(work, key, "head-a"); err != nil || n != 1 {
		t.Fatalf("readAttempts after bump = (%d, %v), want (1, nil)", n, err)
	}
	if err := dropAttempts(work, key); err != nil {
		t.Fatal(err)
	}
	if n, err := readAttempts(work, key, "head-a"); err != nil || n != 0 {
		t.Fatalf("readAttempts after drop = (%d, %v), want (0, nil)", n, err)
	}
	out, err := gitOut(work, "ls-remote", "origin", "refs/forest/attempt/"+key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("remote still holds the retired record: %q", out)
	}
}
