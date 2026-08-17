package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAuditorStableSnapshotRetriesOnRemoteChange(t *testing.T) {
	t.Skip("retired with notes-era Auditor/Poll; see #279")
	root, origin := testClone(t)
	if _, err := audit(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "first-change"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "first-change")
	runGitDir(t, root, "commit", "-m", "first change")
	first := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/staged-first")
	if err := os.WriteFile(filepath.Join(root, "second-change"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "second-change")
	runGitDir(t, root, "commit", "-m", "second change")
	second := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/staged-second")
	addGateNotes(t, root, second, `[{"name":"test","ok":true,"exit":0}]`)

	callCount := filepath.Join(t.TempDir(), "calls")
	wrapper := `#!/bin/sh
set -eu
root=""
if [ "$1" = "-C" ]; then
  root=$2
  shift 2
fi
if [ "$1" != "ls-remote" ]; then
  exec "$AUDIT_REAL_GIT" -C "$root" "$@"
fi
count=0
if [ -e "$AUDIT_WRAP_COUNT" ]; then
  count=$(cat "$AUDIT_WRAP_COUNT")
fi
count=$((count + 1))
printf '%s\n' "$count" > "$AUDIT_WRAP_COUNT"
if [ "$count" -eq 2 ]; then
  "$AUDIT_REAL_GIT" --git-dir "$AUDIT_ORIGIN" update-ref refs/heads/master "$AUDIT_FIRST_MASTER"
elif [ "$count" -eq 4 ]; then
  "$AUDIT_REAL_GIT" --git-dir "$AUDIT_ORIGIN" update-ref refs/heads/master "$AUDIT_SECOND_MASTER"
fi
exec "$AUDIT_REAL_GIT" -C "$root" "$@"
`
	installAuditGitWrapper(t, wrapper)
	t.Setenv("AUDIT_ORIGIN", origin)
	t.Setenv("AUDIT_FIRST_MASTER", first)
	t.Setenv("AUDIT_SECOND_MASTER", second)
	t.Setenv("AUDIT_WRAP_COUNT", callCount)
	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Master != second || len(result.Violations) != 0 {
		t.Fatalf("third-attempt snapshot result=%#v", result)
	}
	countData, err := os.ReadFile(callCount)
	if err != nil {
		t.Fatal(err)
	}
	if advertisements := strings.TrimSpace(string(countData)); advertisements != "6" {
		t.Fatalf("snapshot advertisements=%s, want 6 across three total attempts", advertisements)
	}
}

func TestAuditorStableSnapshotFailsAfterThreeTotalAttempts(t *testing.T) {
	t.Skip("retired with notes-era Auditor/Poll; see #279")
	root, origin := testClone(t)
	if _, err := audit(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	baseline := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	if err := os.WriteFile(filepath.Join(root, "race-change"), []byte("race\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "race-change")
	runGitDir(t, root, "commit", "-m", "race change")
	raced := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/race-staged")

	callCount := filepath.Join(t.TempDir(), "calls")
	wrapper := `#!/bin/sh
set -eu
root=""
if [ "$1" = "-C" ]; then
  root=$2
  shift 2
fi
if [ "$1" != "ls-remote" ]; then
  exec "$AUDIT_REAL_GIT" -C "$root" "$@"
fi
count=0
if [ -e "$AUDIT_WRAP_COUNT" ]; then
  count=$(cat "$AUDIT_WRAP_COUNT")
fi
count=$((count + 1))
printf '%s\n' "$count" > "$AUDIT_WRAP_COUNT"
if [ $((count % 2)) -eq 0 ]; then
  current=$("$AUDIT_REAL_GIT" --git-dir "$AUDIT_ORIGIN" rev-parse refs/heads/master)
  next="$AUDIT_BASELINE"
  if [ "$current" = "$AUDIT_BASELINE" ]; then
    next="$AUDIT_RACED_MASTER"
  fi
  "$AUDIT_REAL_GIT" --git-dir "$AUDIT_ORIGIN" update-ref refs/heads/master "$next"
fi
exec "$AUDIT_REAL_GIT" -C "$root" "$@"
`
	installAuditGitWrapper(t, wrapper)
	t.Setenv("AUDIT_ORIGIN", origin)
	t.Setenv("AUDIT_BASELINE", baseline)
	t.Setenv("AUDIT_RACED_MASTER", raced)
	t.Setenv("AUDIT_WRAP_COUNT", callCount)
	if _, err := audit(context.Background(), root); err == nil || !strings.Contains(err.Error(), "remote snapshot changed during audit") {
		t.Fatalf("perpetual snapshot race error=%v", err)
	}
	countData, err := os.ReadFile(callCount)
	if err != nil {
		t.Fatal(err)
	}
	if advertisements := strings.TrimSpace(string(countData)); advertisements != "6" {
		t.Fatalf("snapshot advertisements=%s, want 6 across three total attempts", advertisements)
	}
}

func TestAuditorFlagsUnaccountedNoteTreeEntries(t *testing.T) {
	t.Skip("retired with notes-era Auditor/Poll; see #279")
	cases := []struct {
		name string
		row  func(string, string) string
		want string
	}{
		{name: "malformed row", row: func(_, _ string) string { return "malformed" }, want: "malformed note tree row"},
		{name: "non-SHA path", row: func(_, blob string) string { return "100644 blob " + blob + "\tnot/a-note" }, want: "non-SHA note tree path"},
		{name: "non-blob mode", row: func(target, blob string) string { return "100755 blob " + blob + "\t" + target }, want: "non-blob note tree entry"},
		{name: "non-blob type", row: func(target, _ string) string { return "160000 commit " + target + "\t" + target }, want: "non-blob note tree entry"},
		{name: "unexpected target", row: func(target, blob string) string {
			unexpected := strings.Repeat("f", 40)
			if unexpected == target {
				unexpected = strings.Repeat("e", 40)
			}
			return "100644 blob " + blob + "\t" + unexpected
		}, want: "unexpected note tree entry"},
	}
	const wrapper = `#!/bin/sh
set -eu
root=""
if [ "$1" = "-C" ]; then
  root=$2
  shift 2
fi
if [ "$1" = "ls-tree" ] && [ "$2" = "-r" ] && [ "$3" = "--full-tree" ]; then
  "$AUDIT_REAL_GIT" -C "$root" "$@"
  printf '%s\n' "$AUDIT_EXTRA_TREE_ROW"
  exit 0
fi
exec "$AUDIT_REAL_GIT" -C "$root" "$@"
`
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root, _ := testClone(t)
			target := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			ref := reviewRequestNoteRef
			addRemoteNote(t, root, ref, target, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+target+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
			fields := strings.Fields(string(runGitDir(t, root, "notes", "--ref="+ref, "list")))
			if len(fields) != 2 {
				t.Fatalf("note list=%q", fields)
			}
			installAuditGitWrapper(t, wrapper)
			t.Setenv("AUDIT_EXTRA_TREE_ROW", testCase.row(target, fields[0]))

			assertPersistedViolationAudit(t, root, []string{testCase.want})
		})
	}
}

func TestAuditRejectsFetchedPrivateRefABA(t *testing.T) {
	t.Skip("retired with notes-era Auditor/Poll; see #279")
	root, _ := testClone(t)
	if _, err := audit(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	ref := reviewRequestNoteRef
	addRemoteNote(t, root, ref, revision, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+revision+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
	advertised := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", ref)))
	other := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD^{tree}")))
	addNote(t, root, ref, other, `{"schema":"forest.review-request.v1","issue":2,"branch":"forest/2-gate","revision":"`+other+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
	wrong := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", ref)))
	if wrong == advertised {
		t.Fatal("crafted private note ref did not change")
	}
	before, err := os.ReadFile(auditStatePath(root))
	if err != nil {
		t.Fatal(err)
	}

	callCount := filepath.Join(t.TempDir(), "calls")
	wrapper := `#!/bin/sh
set -eu
root=""
if [ "$1" = "-C" ]; then
  root=$2
  shift 2
fi
if [ "$1" = "fetch" ]; then
  for arg in "$@"; do
    case "$arg" in
      "$AUDIT_SOURCE_REF":*)
        private_ref=${arg#*:}
        "$AUDIT_REAL_GIT" -C "$root" "$@"
        "$AUDIT_REAL_GIT" -C "$root" update-ref "$private_ref" "$AUDIT_WRONG_OID"
        count=0
        if [ -e "$AUDIT_WRAP_COUNT" ]; then
          count=$(cat "$AUDIT_WRAP_COUNT")
        fi
        printf '%s\n' $((count + 1)) > "$AUDIT_WRAP_COUNT"
        exit 0
        ;;
    esac
  done
fi
exec "$AUDIT_REAL_GIT" -C "$root" "$@"
`
	installAuditGitWrapper(t, wrapper)
	t.Setenv("AUDIT_SOURCE_REF", ref)
	t.Setenv("AUDIT_WRONG_OID", wrong)
	t.Setenv("AUDIT_WRAP_COUNT", callCount)

	if _, err := audit(context.Background(), root); err == nil || !strings.Contains(err.Error(), "does not match advertised object") {
		t.Fatalf("fetched private ref ABA error=%v", err)
	}
	countData, err := os.ReadFile(callCount)
	if err != nil {
		t.Fatal(err)
	}
	if attempts := strings.TrimSpace(string(countData)); attempts != "3" {
		t.Fatalf("wrong private ref fetch attempts=%s, want 3", attempts)
	}
	after, err := os.ReadFile(auditStatePath(root))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("failed fetched-OID audit changed state\nbefore=%s\nafter=%s", before, after)
	}
}

func TestAuditorRetriesWhenChecksNoteChangesDuringSnapshot(t *testing.T) {
	t.Skip("retired with notes-era Auditor/Poll; see #279")
	root, sha := newAdvancedAuditFixture(t, "")
	origin := strings.TrimSpace(string(runGitDir(t, root, "remote", "get-url", "origin")))
	addGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)

	mutator := filepath.Join(t.TempDir(), "mutator")
	runGit(t, "clone", origin, mutator)
	configGit(t, mutator, "Iron Forest Verifier", "verifier@forest.invalid")
	runGitDir(t, mutator, "fetch", "origin", checksNoteRef+":"+checksNoteRef)
	runGitDir(t, mutator, "notes", "--ref="+checksNoteRef, "remove", sha)
	addNote(t, mutator, checksNoteRef, sha, `{"schema":"forest.checks.v1","revision":"`+sha+`","results":[{"name":"test","ok":false,"exit":1}],"time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")

	callCount := filepath.Join(t.TempDir(), "calls")
	wrapper := `#!/bin/sh
set -eu
root=""
if [ "$1" = "-C" ]; then
  root=$2
  shift 2
fi
if [ "$1" != "ls-remote" ]; then
  exec "$AUDIT_REAL_GIT" -C "$root" "$@"
fi
count=0
if [ -e "$AUDIT_WRAP_COUNT" ]; then
  count=$(cat "$AUDIT_WRAP_COUNT")
fi
count=$((count + 1))
printf '%s\n' "$count" > "$AUDIT_WRAP_COUNT"
if [ "$count" -eq 2 ]; then
  "$AUDIT_REAL_GIT" -C "$AUDIT_MUTATOR" push --force origin refs/notes/forest/checks:refs/notes/forest/checks
fi
exec "$AUDIT_REAL_GIT" -C "$root" "$@"
`
	installAuditGitWrapper(t, wrapper)
	t.Setenv("AUDIT_MUTATOR", mutator)
	t.Setenv("AUDIT_WRAP_COUNT", callCount)
	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !containsViolation(result.Violations, "no passing checks") {
		t.Fatalf("changed checks note was not observed: %#v", result)
	}
	countData, err := os.ReadFile(callCount)
	if err != nil {
		t.Fatal(err)
	}
	advertisements, err := strconv.Atoi(strings.TrimSpace(string(countData)))
	if err != nil || advertisements < 4 {
		t.Fatalf("snapshot advertisements=%q, want retry after note change", strings.TrimSpace(string(countData)))
	}
}

func TestConcurrentAuditsUseIsolatedLinkedWorktreeSnapshots(t *testing.T) {
	t.Skip("retired with notes-era Auditor/Poll; see #279")
	root, _ := testClone(t)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	addRemoteNote(t, root, reviewRequestNoteRef, revision, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+revision+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")

	var linked [2]string
	for index := range linked {
		linked[index] = filepath.Join(t.TempDir(), "linked")
		runGitDir(t, root, "worktree", "add", "--detach", linked[index], revision)
		if _, err := audit(context.Background(), linked[index]); err != nil {
			t.Fatal(err)
		}
	}

	readyA := filepath.Join(t.TempDir(), "ready-a")
	readyB := filepath.Join(t.TempDir(), "ready-b")
	releaseA := filepath.Join(t.TempDir(), "release-a")
	releaseB := filepath.Join(t.TempDir(), "release-b")
	defer func() {
		_ = os.WriteFile(releaseA, nil, 0o644)
		_ = os.WriteFile(releaseB, nil, 0o644)
	}()
	installAuditGitWrapper(t, `#!/bin/sh
set -eu
root=""
if [ "$1" = "-C" ]; then
  root=$2
  shift 2
fi
ready=""
release=""
if [ "$root" = "$AUDIT_ROOT_A" ]; then
  ready=$AUDIT_READY_A
  release=$AUDIT_RELEASE_A
elif [ "$root" = "$AUDIT_ROOT_B" ]; then
  ready=$AUDIT_READY_B
  release=$AUDIT_RELEASE_B
fi
if [ -n "$ready" ] && [ "$1" = "show" ]; then
  : > "$ready"
  while [ ! -e "$release" ]; do
    /bin/sleep 0.01
  done
fi
exec "$AUDIT_REAL_GIT" -C "$root" "$@"
`)
	t.Setenv("AUDIT_ROOT_A", linked[0])
	t.Setenv("AUDIT_ROOT_B", linked[1])
	t.Setenv("AUDIT_READY_A", readyA)
	t.Setenv("AUDIT_READY_B", readyB)
	t.Setenv("AUDIT_RELEASE_A", releaseA)
	t.Setenv("AUDIT_RELEASE_B", releaseB)

	type outcome struct {
		result AuditResult
		err    error
	}
	firstDone := make(chan outcome, 1)
	go func() {
		result, err := audit(context.Background(), linked[0])
		firstDone <- outcome{result: result, err: err}
	}()
	waitForAuditSignal(t, readyA)

	other := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD^{tree}")))
	addRemoteNote(t, root, reviewRequestNoteRef, other, "garbage", "Iron Forest Builder", "builder@forest.invalid")

	secondDone := make(chan outcome, 1)
	go func() {
		result, err := audit(context.Background(), linked[1])
		secondDone <- outcome{result: result, err: err}
	}()
	waitForAuditSignal(t, readyB)

	live := auditPrivateRefs(t, root)
	if len(live) != 4 {
		t.Fatalf("overlapping private refs=%v want four", live)
	}
	owners := make(map[string]int)
	for _, ref := range live {
		owner := auditPrivateOwner(t, ref)
		owners[owner]++
	}
	if len(owners) != 2 {
		t.Fatalf("overlapping Audits share private namespace: refs=%v owners=%v", live, owners)
	}
	for owner, count := range owners {
		if count != 2 {
			t.Fatalf("private namespace %s has %d refs, want master and notes", owner, count)
		}
	}

	if err := os.WriteFile(releaseB, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var second outcome
	select {
	case second = <-secondDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for second Audit")
	}
	if second.err != nil {
		t.Fatal(second.err)
	}
	if !containsViolation(second.result.Violations, "malformed JSON note") {
		t.Fatalf("second Audit did not read final note snapshot: %#v", second.result)
	}
	remaining := auditPrivateRefs(t, root)
	if len(remaining) != 2 {
		t.Fatalf("second Audit cleared first Audit refs: %v", remaining)
	}
	if auditPrivateOwner(t, remaining[0]) != auditPrivateOwner(t, remaining[1]) {
		t.Fatalf("remaining refs cross private namespaces: %v", remaining)
	}

	if err := os.WriteFile(releaseA, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var first outcome
	select {
	case first = <-firstDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for first Audit")
	}
	if first.err != nil {
		t.Fatal(first.err)
	}
	if len(first.result.Violations) != 0 {
		t.Fatalf("first Audit read second Audit snapshot: %#v", first.result)
	}
	if refs := auditPrivateRefs(t, root); len(refs) != 0 {
		t.Fatalf("private Audit refs remain: %v", refs)
	}
}

func TestAuditCleanupUsesFreshBoundedContextAndJoinsErrors(t *testing.T) {
	t.Skip("retired with notes-era Auditor/Poll; see #279")
	root, _ := testClone(t)
	if _, err := audit(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	addRemoteNote(t, root, reviewRequestNoteRef, revision, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+revision+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
	before, err := os.ReadFile(auditStatePath(root))
	if err != nil {
		t.Fatal(err)
	}

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := defaultAuditDependencies()
	runGit := deps.runGit
	cleanupErr := errors.New("injected cleanup failure")
	cleanupCalls := 0
	deps.runGit = func(ctx context.Context, root string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "show" {
			cancel()
			return nil, context.Canceled
		}
		if len(args) >= 3 && args[0] == "update-ref" && args[1] == "-d" && parent.Err() == context.Canceled {
			if err := ctx.Err(); err != nil {
				t.Fatalf("cleanup context is already done: %v", err)
			}
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("cleanup context has no deadline")
			}
			if remaining := time.Until(deadline); remaining <= 0 || remaining > time.Second {
				t.Fatalf("cleanup deadline remaining=%v want within one second", remaining)
			}
			cleanupCalls++
			output, err := runGit(ctx, root, args...)
			if err == nil && cleanupCalls == 1 {
				return output, cleanupErr
			}
			return output, err
		}
		return runGit(ctx, root, args...)
	}

	if _, err := auditWithDependencies(parent, root, deps); !errors.Is(err, context.Canceled) || !errors.Is(err, cleanupErr) {
		t.Fatalf("canceled Audit error=%v", err)
	}
	if cleanupCalls != 4 {
		t.Fatalf("cleanup calls=%d want 4", cleanupCalls)
	}
	after, err := os.ReadFile(auditStatePath(root))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("canceled Audit changed durable state\nbefore=%s\nafter=%s", before, after)
	}
	if refs := auditPrivateRefs(t, root); len(refs) != 0 {
		t.Fatalf("canceled Audit left private refs: %v", refs)
	}
}

func TestAuditNoteAuthorFailureDoesNotMutateDurableState(t *testing.T) {
	t.Skip("retired with notes-era Auditor/Poll; see #279")
	root, _ := testClone(t)
	if _, err := audit(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	addRemoteNote(t, root, reviewRequestNoteRef, revision, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+revision+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
	before, err := os.ReadFile(auditStatePath(root))
	if err != nil {
		t.Fatal(err)
	}

	deps := defaultAuditDependencies()
	runGit := deps.runGit
	authorErr := errors.New("injected note author failure")
	deps.runGit = func(ctx context.Context, root string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "log" {
			return nil, authorErr
		}
		return runGit(ctx, root, args...)
	}

	if _, err := auditWithDependencies(context.Background(), root, deps); !errors.Is(err, authorErr) || !strings.Contains(err.Error(), "read note author") {
		t.Fatalf("note author error=%v", err)
	}
	after, err := os.ReadFile(auditStatePath(root))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("note author failure changed audit state\nbefore=%s\nafter=%s", before, after)
	}
	if _, err := os.Stat(auditLogPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("note author failure created policy history: %v", err)
	}
	if refs := auditPrivateRefs(t, root); len(refs) != 0 {
		t.Fatalf("note author failure left private refs: %v", refs)
	}
}

func TestAuditTransportFailuresPreserveStateAndCleanup(t *testing.T) {
	t.Skip("retired with notes-era Auditor/Poll; see #279")
	ancestryCleanupErr := errors.New("injected ancestry cleanup failure")
	tests := []struct {
		name               string
		failFetch          bool
		ancestryErr        error
		wantErr            error
		wantAdvertisements int
		wantNoteFetches    int
	}{
		{name: "canonical note fetch", failFetch: true, wantAdvertisements: 3, wantNoteFetches: 3},
		{name: "second final advertisement", wantAdvertisements: 2, wantNoteFetches: 1},
		{name: "joined deadline ancestry", ancestryErr: errors.Join(gitExitError(t, 1), context.DeadlineExceeded), wantErr: context.DeadlineExceeded, wantAdvertisements: 2, wantNoteFetches: 1},
		{name: "joined cleanup ancestry", ancestryErr: errors.Join(gitExitError(t, 1), ancestryCleanupErr), wantErr: ancestryCleanupErr, wantAdvertisements: 2, wantNoteFetches: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, revision := newAdvancedAuditFixture(t, "")
			noteRef := reviewRequestNoteRef
			addRemoteNote(t, root, noteRef, revision, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+revision+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
			before, err := os.ReadFile(auditStatePath(root))
			if err != nil {
				t.Fatal(err)
			}

			deps := defaultAuditDependencies()
			runGit := deps.runGit
			sentinel := errors.New("injected " + test.name + " failure")
			wantErr := test.wantErr
			if wantErr == nil {
				wantErr = sentinel
			}
			advertisements := 0
			noteFetches := 0
			deps.runGit = func(ctx context.Context, root string, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "ls-remote" {
					advertisements++
					if !test.failFetch && test.ancestryErr == nil && advertisements == 2 {
						return nil, sentinel
					}
				}
				if len(args) > 0 && args[0] == "fetch" && slices.ContainsFunc(args, func(arg string) bool {
					return strings.HasPrefix(arg, noteRef+":")
				}) {
					noteFetches++
					if test.failFetch {
						return nil, sentinel
					}
				}
				if len(args) > 0 && args[0] == "merge-base" && test.ancestryErr != nil {
					return nil, test.ancestryErr
				}
				return runGit(ctx, root, args...)
			}

			if _, err := auditWithDependencies(context.Background(), root, deps); !errors.Is(err, wantErr) {
				t.Fatalf("Audit error=%v, want %v identity", err, wantErr)
			}
			if advertisements != test.wantAdvertisements || noteFetches != test.wantNoteFetches {
				t.Fatalf("advertisements=%d note fetches=%d, want %d and %d", advertisements, noteFetches, test.wantAdvertisements, test.wantNoteFetches)
			}
			after, err := os.ReadFile(auditStatePath(root))
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("transport failure changed audit state\nbefore=%s\nafter=%s", before, after)
			}
			if _, err := os.Stat(auditLogPath(root)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("transport failure created policy history: %v", err)
			}
			if refs := auditPrivateRefs(t, root); len(refs) != 0 {
				t.Fatalf("transport failure left private refs: %v", refs)
			}
		})
	}
}

func TestAuditorRetriesAbsentPresentNoteRefRacesToFinalTuple(t *testing.T) {
	t.Skip("retired with notes-era Auditor/Poll; see #279")
	tests := []struct {
		name           string
		startPresent   bool
		finalPresent   bool
		wantViolations bool
	}{
		{name: "absent to present", finalPresent: true, wantViolations: true},
		{name: "present to absent", startPresent: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, origin := testClone(t)
			if _, err := audit(context.Background(), root); err != nil {
				t.Fatal(err)
			}
			ref := reviewRequestNoteRef
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD^{tree}")))
			addNote(t, root, ref, revision, "garbage", "Iron Forest Builder", "builder@forest.invalid")
			oid := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", ref)))
			runGitDir(t, root, "push", "origin", ref+":refs/notes/forest-test/race")
			if test.startPresent {
				runGitDir(t, root, "push", "origin", ref+":"+ref)
			}

			callCount := filepath.Join(t.TempDir(), "calls")
			installAuditGitWrapper(t, `#!/bin/sh
set -eu
root=""
if [ "$1" = "-C" ]; then
  root=$2
  shift 2
fi
if [ "$1" != "ls-remote" ]; then
  exec "$AUDIT_REAL_GIT" -C "$root" "$@"
fi
count=0
if [ -e "$AUDIT_WRAP_COUNT" ]; then
  count=$(cat "$AUDIT_WRAP_COUNT")
fi
count=$((count + 1))
printf '%s\n' "$count" > "$AUDIT_WRAP_COUNT"
if [ "$count" -eq 2 ]; then
  if [ "$AUDIT_FINAL_PRESENT" = "true" ]; then
    "$AUDIT_REAL_GIT" --git-dir "$AUDIT_ORIGIN" update-ref "$AUDIT_NOTE_REF" "$AUDIT_NOTE_OID"
  else
    "$AUDIT_REAL_GIT" --git-dir "$AUDIT_ORIGIN" update-ref -d "$AUDIT_NOTE_REF"
  fi
fi
exec "$AUDIT_REAL_GIT" -C "$root" "$@"
`)
			t.Setenv("AUDIT_WRAP_COUNT", callCount)
			t.Setenv("AUDIT_ORIGIN", origin)
			t.Setenv("AUDIT_NOTE_REF", ref)
			t.Setenv("AUDIT_NOTE_OID", oid)
			t.Setenv("AUDIT_FINAL_PRESENT", strconv.FormatBool(test.finalPresent))

			result, err := audit(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			hasMalformed := containsViolation(result.Violations, "malformed JSON note")
			if hasMalformed != test.wantViolations {
				t.Fatalf("final tuple result=%#v want malformed=%t", result, test.wantViolations)
			}
			countData, err := os.ReadFile(callCount)
			if err != nil {
				t.Fatal(err)
			}
			if advertisements := strings.TrimSpace(string(countData)); advertisements != "4" {
				t.Fatalf("snapshot advertisements=%s want four across retry", advertisements)
			}
			if refs := auditPrivateRefs(t, root); len(refs) != 0 {
				t.Fatalf("note-ref race left private refs: %v", refs)
			}
		})
	}
}

func TestAuditorGitRunStopsDescendants(t *testing.T) {
	testGitTransportStopsDescendants(t, "Audit", "audit-output", func(ctx context.Context, root string) ([]byte, error) {
		return runAuditGit(ctx, root, "--version")
	})
}

func TestAuditorSurfacesRemoteFailure(t *testing.T) {
	root, _ := testClone(t)
	runGitDir(t, root, "remote", "remove", "origin")
	if _, err := audit(context.Background(), root); err == nil {
		t.Fatal("expected remote audit failure")
	}
	if _, err := os.Stat(auditStatePath(root)); !os.IsNotExist(err) {
		t.Fatalf("audit state should not be written after remote failure: %v", err)
	}
}
