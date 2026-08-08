package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	runGitTest(t, "", "clone", remote, secondClone)
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
	raw := runGitTest(t, work, "notes", "--ref=forest/verdict", "show", sha)
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
	runGitTest(t, work, "add", "second.txt")
	runGitTest(t, work, "commit", "-m", "second")
	otherSHA := runGitTest(t, work, "rev-parse", "HEAD")
	if _, ok, err := readVerdict(work, otherSHA); err != nil || ok {
		t.Fatalf("verdict for another commit = (%v, %v), want (false, nil)", ok, err)
	}
	if _, ok, err := readChecks(work, otherSHA); err != nil || ok {
		t.Fatalf("checks for another commit = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestFetchNotesLockHelper(t *testing.T) {
	repo := os.Getenv("FOREST_NOTES_LOCK_HELPER")
	if repo == "" {
		return
	}
	if err := fetchNotes(repo); err != nil {
		t.Fatal(err)
	}
}

func TestFetchNotesUnlocksBeforeCallerContinues(t *testing.T) {
	_, work, _ := notesTestRepository(t)
	if err := fetchNotes(work); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- fetchNotes(work) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second fetch: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notes lock remained held after fetchNotes returned")
	}
}

func TestFetchNotesConcurrentReconcile(t *testing.T) {
	remote, work, sha := notesTestRepository(t)
	source := filepath.Join(t.TempDir(), "source")
	runGitTest(t, "", "clone", remote, source)
	runGitTest(t, source, "config", "user.name", "notes-source")
	runGitTest(t, source, "config", "user.email", "notes-source@example.com")

	if err := writeVerdict(source, sha, verdictNote{
		Verdict: "approve", Notes: "remote verdict", Reviewer: "reviewer-a", RunID: "run-verdict",
	}); err != nil {
		t.Fatal(err)
	}
	wantVerdict, ok, err := readVerdict(source, sha)
	if err != nil || !ok {
		t.Fatalf("remote verdict = (%#v, %v, %v), want a note", wantVerdict, ok, err)
	}
	if err := writeChecks(source, sha, checksNote{
		Status: "pass", Results: []checkResult{{Name: "test", Code: 0, Seconds: 1.5, Output: "remote checks"}}, RunID: "run-checks",
	}); err != nil {
		t.Fatal(err)
	}
	wantChecks, ok, err := readChecks(source, sha)
	if err != nil || !ok {
		t.Fatalf("remote checks = (%#v, %v, %v), want a note", wantChecks, ok, err)
	}

	linked := filepath.Join(t.TempDir(), "linked")
	runGitTest(t, work, "worktree", "add", "--detach", linked, sha)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	wrapper := filepath.Join(wrapperDir, "git")
	if err := os.WriteFile(wrapper, []byte(`#!/bin/sh
case " $* " in
  *" fetch "*)
    printf x >&3
    IFS= read -r release <&4
    ;;
esac
exec "$FOREST_REAL_GIT" "$@"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	ready, childReady, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	defer childReady.Close()
	childRelease, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer release.Close()
	defer childRelease.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestFetchNotesLockHelper$")
	cmd.Env = append(os.Environ(),
		"FOREST_NOTES_LOCK_HELPER="+work,
		"FOREST_REAL_GIT="+realGit,
		"PATH="+wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	cmd.ExtraFiles = []*os.File{childReady, childRelease}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	helperDone, markWaited := startTestProcess(t, cmd)
	_ = childReady.Close()
	_ = childRelease.Close()

	var one [1]byte
	if _, err := io.ReadFull(ready, one[:]); err != nil {
		t.Fatalf("notes helper did not acquire the lock: %v: %s", err, stderr.String())
	}
	contenderStarted := make(chan struct{})
	contender := make(chan error, 1)
	go func() {
		close(contenderStarted)
		contender <- fetchNotes(linked)
	}()
	writerStarted := make(chan struct{})
	writer := make(chan error, 1)
	go func() {
		close(writerStarted)
		writer <- writeNote(linked, verdictNotesRef, sha, wantVerdict)
	}()
	<-contenderStarted
	<-writerStarted
	select {
	case err := <-contender:
		t.Fatalf("concurrent fetch completed while another process held the lock: %v", err)
	case err := <-writer:
		t.Fatalf("concurrent write completed while another process held the lock: %v", err)
	case <-time.After(time.Second):
	}
	if _, err := release.Write([]byte{'\n'}); err != nil {
		t.Fatal(err)
	}
	_ = release.Close()
	select {
	case err := <-helperDone:
		markWaited()
		if err != nil {
			t.Fatalf("notes helper: %v: %s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("notes helper did not finish: %s", stderr.String())
	}
	select {
	case err := <-contender:
		if err != nil {
			t.Fatalf("concurrent fetchNotes: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent fetch did not resume after lock release")
	}
	select {
	case err := <-writer:
		if !errors.Is(err, errNoteExists) {
			t.Fatalf("concurrent writeNote: %v, want existing durable note", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent write did not resume after lock release")
	}
	reacquired := make(chan error, 1)
	go func() { reacquired <- fetchNotes(work) }()
	select {
	case err := <-reacquired:
		if err != nil {
			t.Fatalf("fetch after unlock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notes lock remained held after fetch returned")
	}

	gotVerdict, ok, err := readVerdict(linked, sha)
	if err != nil || !ok || !reflect.DeepEqual(gotVerdict, wantVerdict) {
		t.Fatalf("fetched verdict = (%#v, %v, %v), want remote %#v", gotVerdict, ok, err, wantVerdict)
	}
	gotChecks, ok, err := readChecks(linked, sha)
	if err != nil || !ok || !reflect.DeepEqual(gotChecks, wantChecks) {
		t.Fatalf("fetched checks = (%#v, %v, %v), want remote %#v", gotChecks, ok, err, wantChecks)
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
	runGitTest(t, "", "init", "--bare", "--initial-branch=master", remote)
	runGitTest(t, "", "clone", remote, work)
	runGitTest(t, work, "config", "user.name", "notes-test")
	runGitTest(t, work, "config", "user.email", "notes-test@example.com")
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, work, "add", "file.txt")
	runGitTest(t, work, "commit", "-m", "first")
	runGitTest(t, work, "push", "-u", "origin", "HEAD:master")
	sha = runGitTest(t, work, "rev-parse", "HEAD")
	return remote, work, sha
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
	// A caller must not read the rejected local note, even before the next
	// remote reconciliation.
	if _, ok, err := readVerdict(work, sha); err != nil || ok {
		t.Fatalf("verdict immediately after rejected push = (present=%v, err=%v), want absent", ok, err)
	}
}

func TestNotesReaderNeverSeesUnpublishedLocalNote(t *testing.T) {
	remote, work, sha := notesTestRepository(t)
	state := t.TempDir()
	started := filepath.Join(state, "started")
	release := filepath.Join(state, "release")
	hook := filepath.Join(remote, "hooks", "update")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n  refs/notes/forest/verdict)\n    touch '%s'\n    while test ! -f '%s'; do sleep 0.01; done\n    exit 1\n    ;;\nesac\nexit 0\n", started, release)
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- writeVerdict(work, sha, verdictNote{
			Verdict: "approve", Notes: "never durable", Reviewer: "reviewer-a", RunID: "run-a",
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("notes push did not reach the rejecting hook")
		}
		time.Sleep(10 * time.Millisecond)
	}
	type readResult struct {
		ok  bool
		err error
	}
	readDone := make(chan readResult, 1)
	go func() {
		_, ok, err := readVerdict(work, sha)
		readDone <- readResult{ok: ok, err: err}
	}()
	select {
	case result := <-readDone:
		t.Fatalf("reader crossed an unpublished write: %#v", result)
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.WriteFile(release, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err == nil {
		t.Fatal("writeVerdict succeeded though every notes push was rejected")
	}
	result := <-readDone
	if result.err != nil || result.ok {
		t.Fatalf("verdict after rejected write = (present=%v, err=%v), want absent", result.ok, result.err)
	}
}

func TestNotesReaderDiscardsInterruptedLocalWriterResidue(t *testing.T) {
	_, work, sha := notesTestRepository(t)
	body := `{"verdict":"approve","notes":"local residue","reviewer":"dead","model":"m","def_sha":"d","run_id":"r"}`
	runGitTest(t, work, "notes", "--ref="+verdictNotesRef, "add", "-m", body, sha)
	if _, ok, err := readVerdict(work, sha); err != nil || ok {
		t.Fatalf("verdict from local-only writer residue = (present=%v, err=%v), want absent", ok, err)
	}
}

func TestConcurrentCloneNoteWritersConvergeOnRemoteWinner(t *testing.T) {
	remote, first, sha := notesTestRepository(t)
	second := filepath.Join(t.TempDir(), "second")
	runGitTest(t, "", "clone", remote, second)
	notes := []verdictNote{
		{Verdict: "approve", Notes: "first", Reviewer: "reviewer-a", RunID: "run-a"},
		{Verdict: "reject", Notes: "second", Reviewer: "reviewer-b", RunID: "run-b"},
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i, repo := range []string{first, second} {
		go func(repo string, note verdictNote) {
			<-start
			results <- writeVerdict(repo, sha, note)
		}(repo, notes[i])
	}
	close(start)
	var successes, existing int
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, errNoteExists):
			existing++
		default:
			t.Fatalf("concurrent notes write = %v, want success or errNoteExists", err)
		}
	}
	if successes != 1 || existing != 1 {
		t.Fatalf("concurrent notes results = %d success, %d existing; want one each", successes, existing)
	}
	reader := filepath.Join(t.TempDir(), "reader")
	runGitTest(t, "", "clone", remote, reader)
	if err := fetchNotes(reader); err != nil {
		t.Fatal(err)
	}
	got, ok, err := readVerdict(reader, sha)
	if err != nil || !ok {
		t.Fatalf("remote winner = (present=%v, err=%v), want one Verdict", ok, err)
	}
	if got.Notes != notes[0].Notes && got.Notes != notes[1].Notes {
		t.Fatalf("remote winner = %#v, want one submitted Verdict", got)
	}
}

// TestNotesPreExistingLocalSecretIsDiscarded proves a local-only note cannot
// bypass the outbound redaction boundary when a later writer finds it.
func TestNotesPreExistingLocalSecretIsDiscarded(t *testing.T) {
	remote, work, sha := notesTestRepository(t)
	local := verdictNote{
		Verdict: "approve", Notes: "token sk-live-local-only-secret", Reviewer: "reviewer-a", RunID: "run-local",
	}
	body, err := json.Marshal(local)
	if err != nil {
		t.Fatal(err)
	}
	runGitTest(t, work, "notes", "--ref=forest/verdict", "add", "-m", string(body), sha)
	if err := os.WriteFile(filepath.Join(work, "second.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, work, "add", "second.txt")
	runGitTest(t, work, "commit", "-m", "second")
	runGitTest(t, work, "push", "origin", "master")
	otherSHA := runGitTest(t, work, "rev-parse", "HEAD")
	runGitTest(t, work, "notes", "--ref=forest/verdict", "add", "-m", string(body), otherSHA)

	proposed := verdictNote{Verdict: "reject", Notes: "proposed", Reviewer: "reviewer-b", RunID: "run-b"}
	if err := writeVerdict(work, sha, proposed); err != nil {
		t.Fatalf("writeVerdict after local-only secret: %v", err)
	}
	secondClone := filepath.Join(t.TempDir(), "second")
	runGitTest(t, "", "clone", remote, secondClone)
	if err := fetchNotes(secondClone); err != nil {
		t.Fatal(err)
	}
	got, ok, err := readVerdict(secondClone, sha)
	if err != nil || !ok {
		t.Fatalf("remote verdict = (present=%v, err=%v), want proposed durable note", ok, err)
	}
	if got.Verdict != proposed.Verdict || got.Notes != proposed.Notes {
		t.Fatalf("remote verdict = %#v, want proposed %#v", got, proposed)
	}
	if strings.Contains(got.Notes, "sk-live-local-only-secret") {
		t.Fatal("remote verdict contains the discarded local-only secret")
	}
	if _, ok, err := readVerdict(secondClone, otherSHA); err != nil || ok {
		t.Fatalf("unrelated local-only verdict = (present=%v, err=%v), want absent", ok, err)
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
	if _, err := bumpAttempts(work, key); err != nil {
		t.Fatal(err)
	}
	if n, err := readAttempts(work, key); err != nil || n != 1 {
		t.Fatalf("readAttempts after bump = (%d, %v), want (1, nil)", n, err)
	}
	if err := dropAttempts(work, key); err != nil {
		t.Fatal(err)
	}
	if n, err := readAttempts(work, key); err != nil || n != 0 {
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
