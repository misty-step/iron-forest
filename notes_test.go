package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNotesRoundTripRewriteAndCommitScope(t *testing.T) {
	remote, work, sha := notesTestRepository(t)

	if _, ok, err := readVerdict(work, sha); err != nil || ok {
		t.Fatalf("absent verdict = (%v, %v), want (false, nil)", ok, err)
	}
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

	secondClone := filepath.Join(t.TempDir(), "second")
	notesTestGit(t, "", "clone", remote, secondClone)
	if err := fetchNotes(secondClone); err != nil {
		t.Fatalf("fetch notes with one missing namespace: %v", err)
	}
	fromSecond, ok, err := readVerdict(secondClone, sha)
	if err != nil || !ok {
		t.Fatalf("fresh clone verdict = (%v, %v), want (true, nil)", ok, err)
	}
	if fromSecond.Verdict != first.Verdict || fromSecond.Time != got.Time {
		t.Fatalf("fresh clone verdict = %#v, want %#v", fromSecond, got)
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

	replacement := first
	replacement.Verdict = "changes"
	replacement.Notes = "replacement decision"
	if err := writeVerdict(work, sha, replacement); err != nil {
		t.Fatal(err)
	}
	if err := fetchNotes(secondClone); err != nil {
		t.Fatal(err)
	}
	updated, ok, err := readVerdict(secondClone, sha)
	if err != nil || !ok {
		t.Fatalf("rewritten verdict = (%v, %v), want (true, nil)", ok, err)
	}
	if updated.Verdict != replacement.Verdict || updated.Notes != replacement.Notes {
		t.Fatalf("rewritten verdict = %#v, want %#v", updated, replacement)
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
	if got, err := readAttempts(work, key); err != nil || got != 0 {
		t.Fatalf("initial attempts = (%d, %v), want (0, nil)", got, err)
	}
	if got, err := bumpAttempts(work, key); err != nil || got != 1 {
		t.Fatalf("first bump = (%d, %v), want (1, nil)", got, err)
	}
	if got, err := bumpAttempts(work, key); err != nil || got != 2 {
		t.Fatalf("second bump = (%d, %v), want (2, nil)", got, err)
	}
	if got, err := readAttempts(work, key); err != nil || got != 2 {
		t.Fatalf("stored attempts = (%d, %v), want (2, nil)", got, err)
	}
}

func TestNotesAttemptsCASRetriesAreBounded(t *testing.T) {
	remote, work, _ := notesTestRepository(t)
	key := "branch-forest/7-example"
	if _, err := bumpAttempts(work, key); err != nil {
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
		_, err := bumpAttempts(work, key)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, errLeaseHeld) {
			t.Fatalf("bounded CAS result = %v, want lease-held error", err)
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
