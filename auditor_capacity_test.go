package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// capacitySnapshot returns a snapshot with only the review-request note ref
// populated so readNotes exercises exactly one canonical ref.
func capacitySnapshot() auditSnapshot {
	return auditSnapshot{
		Master: strings.Repeat("a", 40),
		Notes: map[string]string{
			reviewRequestNoteRef: strings.Repeat("b", 40),
		},
		id: "test-snapshot",
	}
}

func TestReadNotesRejectsCapacityBeforePerNoteWork(t *testing.T) {
	snapshot := capacitySnapshot()
	blob := strings.Repeat("b", 40)
	var list strings.Builder
	var tree strings.Builder
	for index := range auditorCapacityEntries + 1 {
		revision := fmt.Sprintf("%040x", index)
		fmt.Fprintf(&list, "%s %s\n", blob, revision)
		fmt.Fprintf(&tree, "100644 blob %s\t%s\n", blob, revision)
	}
	showCalls := 0
	authorCalls := 0
	deps := auditDependencies{runGit: func(ctx context.Context, root string, args ...string) ([]byte, error) {
		if len(args) >= 3 && args[0] == "notes" && args[2] == "list" {
			return []byte(list.String()), nil
		}
		if len(args) >= 2 && args[0] == "ls-tree" {
			return []byte(tree.String()), nil
		}
		if len(args) >= 3 && args[0] == "notes" && args[2] == "show" {
			showCalls++
			return []byte("{}"), nil
		}
		if len(args) >= 1 && args[0] == "log" {
			authorCalls++
			return []byte("Builder\x00builder@forest.invalid\n"), nil
		}
		t.Fatalf("unexpected git call %v", args)
		return nil, nil
	}}

	entries, violations, err := readNotes(context.Background(), "", snapshot, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries=%d want 0", len(entries))
	}
	want := "exceeds " + strconv.Itoa(auditorCapacityEntries) + "-entry"
	if len(violations) != 1 || !containsViolation(violations, want) {
		t.Fatalf("violations=%v, want %q", violations, want)
	}
	if showCalls != 0 || authorCalls != 0 {
		t.Fatalf("per-note work after capacity rejection: show=%d author=%d", showCalls, authorCalls)
	}
}

func TestReadNotesTreatsEnumerationOverflowAsCapacity(t *testing.T) {
	snapshot := capacitySnapshot()
	deps := auditDependencies{runGit: func(ctx context.Context, root string, args ...string) ([]byte, error) {
		return nil, errTrustedTransportOutputOverflow
	}}
	entries, violations, err := readNotes(context.Background(), "", snapshot, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries=%d want 0", len(entries))
	}
	want := "exceeds " + strconv.Itoa(auditorCapacityEntries) + "-entry"
	if len(violations) != 1 || !containsViolation(violations, want) {
		t.Fatalf("enumeration overflow violations=%v, want %q", violations, want)
	}
}

func TestReadNotesFlagsOversizedPayloadAndContinues(t *testing.T) {
	snapshot := capacitySnapshot()
	blob := strings.Repeat("b", 40)
	oversized := strings.Repeat("d", 40)
	valid := strings.Repeat("e", 40)
	payload := `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"` + valid + `","time":"2026-08-10T00:00:00Z"}`
	big := strings.Repeat("x", auditHistoryEntryBytes+1)
	var authorCalls int
	deps := auditDependencies{runGit: func(ctx context.Context, root string, args ...string) ([]byte, error) {
		if len(args) >= 3 && args[0] == "notes" && args[2] == "list" {
			return []byte(blob + " " + oversized + "\n" + blob + " " + valid + "\n"), nil
		}
		if len(args) >= 2 && args[0] == "ls-tree" {
			return []byte("100644 blob " + blob + "\t" + oversized + "\n" + "100644 blob " + blob + "\t" + valid + "\n"), nil
		}
		if len(args) >= 4 && args[0] == "notes" && args[2] == "show" && args[3] == oversized {
			return []byte(big), nil
		}
		if len(args) >= 4 && args[0] == "notes" && args[2] == "show" && args[3] == valid {
			return []byte(payload), nil
		}
		if len(args) >= 1 && args[0] == "log" {
			authorCalls++
			return []byte("Builder\x00builder@forest.invalid\n"), nil
		}
		t.Fatalf("unexpected git call %v", args)
		return nil, nil
	}}

	entries, violations, err := readNotes(context.Background(), "", snapshot, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Revision != valid {
		t.Fatalf("entries=%v want only the valid revision %s", entries, valid)
	}
	if len(violations) != 1 || !containsViolation(violations, "note payload on "+reviewRequestNoteRef) {
		t.Fatalf("payload violations=%v", violations)
	}
	if authorCalls != 1 {
		t.Fatalf("author reads=%d want 1 (only the valid note)", authorCalls)
	}
}

func TestReadNotesFlagsShowOverflowAndContinues(t *testing.T) {
	snapshot := capacitySnapshot()
	blob := strings.Repeat("b", 40)
	valid := strings.Repeat("e", 40)
	deps := auditDependencies{runGit: func(ctx context.Context, root string, args ...string) ([]byte, error) {
		if len(args) >= 3 && args[0] == "notes" && args[2] == "list" {
			return []byte(blob + " " + valid + "\n"), nil
		}
		if len(args) >= 2 && args[0] == "ls-tree" {
			return []byte("100644 blob " + blob + "\t" + valid + "\n"), nil
		}
		if len(args) >= 3 && args[0] == "notes" && args[2] == "show" {
			return nil, errTrustedTransportOutputOverflow
		}
		t.Fatalf("author read after show overflow: %v", args)
		return nil, nil
	}}

	entries, violations, err := readNotes(context.Background(), "", snapshot, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries=%d want 0", len(entries))
	}
	if len(violations) != 1 || !containsViolation(violations, "note transport output overflow on "+reviewRequestNoteRef) {
		t.Fatalf("show overflow violations=%v", violations)
	}
}

func TestNoteDecoderOmitsHugeKeyFromError(t *testing.T) {
	sha := strings.Repeat("e", 40)
	hugeKey := strings.Repeat("z", 8*1024)
	payload := `{"schema":"forest.review-request.v1","` + hugeKey + `":1,"issue":1,"branch":"forest/1-gate","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`
	if _, err := decodeReview([]byte(payload), sha); err == nil {
		t.Fatal("huge unknown key accepted")
	} else if len(err.Error()) > auditorViolationEntryBytes {
		t.Fatalf("strict-decoder error unbounded: %d bytes", len(err.Error()))
	} else if !strings.Contains(err.Error(), "unknown JSON object key") {
		t.Fatalf("key classification lost: %v", err)
	}
}

func TestViolationCollectorBoundsAt999WithOmissionSummary(t *testing.T) {
	var collector violationCollector
	const total = 1002
	for range total {
		collector.add("violation")
	}
	got := collector.finalize()
	wantLen := auditorConcreteViolations + 1
	if len(got) != wantLen {
		t.Fatalf("finalized entries=%d want %d", len(got), wantLen)
	}
	summary := fmt.Sprintf("%d additional violations omitted", total-auditorConcreteViolations)
	if got[len(got)-1] != summary {
		t.Fatalf("summary=%q want %q", got[len(got)-1], summary)
	}
}

func TestViolationCollectorTruncatesOversizedEntry(t *testing.T) {
	var collector violationCollector
	collector.add(strings.Repeat("x", auditorViolationEntryBytes*2))
	got := collector.finalize()
	if len(got) != 1 || len(got[0]) > auditorViolationEntryBytes {
		t.Fatalf("truncated entries=%q", got)
	}
}

func TestAuditorFailsClosedOnRealOverCapacityRef(t *testing.T) {
	t.Skip("retired with notes-era Auditor/Poll; see #279")
	root, _ := newAdvancedAuditFixture(t, "")
	ref := reviewRequestNoteRef
	index := filepath.Join(t.TempDir(), "index")
	runIndexGit := func(args ...string) []byte {
		commandArgs := append([]string{"-C", root}, args...)
		cmd := exec.Command("git", commandArgs...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return output
	}
	runIndexGit("read-tree", "--empty")
	for index := range auditorCapacityEntries + 1 {
		revision := fmt.Sprintf("%040x", index)
		payload := "payload-" + strconv.Itoa(index) + "\n"
		blobPath := filepath.Join(t.TempDir(), "p")
		if err := os.WriteFile(blobPath, []byte(payload), 0o644); err != nil {
			t.Fatal(err)
		}
		blob := strings.TrimSpace(string(runGitDir(t, root, "hash-object", "-w", blobPath)))
		runIndexGit("update-index", "--add", "--cacheinfo", "100644,"+blob+","+revision)
	}
	tree := strings.TrimSpace(string(runIndexGit("write-tree")))
	commit := strings.TrimSpace(string(runGitDir(t, root, "commit-tree", tree, "-m", "over-capacity notes")))
	runGitDir(t, root, "update-ref", ref, commit)
	runGitDir(t, root, "push", "origin", ref+":"+ref)

	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	fragment := "exceeds " + strconv.Itoa(auditorCapacityEntries) + "-entry"
	if !containsViolation(result.Violations, fragment) {
		t.Fatalf("over-capacity ref not flagged: %v", result.Violations)
	}
	if !containsViolation(result.Violations, "does not have exactly one valid review-request note") {
		t.Fatalf("real over-capacity lost Gate non-pass: %v", result.Violations)
	}
	if private := auditPrivateRefs(t, root); len(private) != 0 {
		t.Fatalf("private snapshot refs not cleaned: %v", private)
	}
}

func TestAuditorBoundsOversizedPayloadAndDeduplicatesHistory(t *testing.T) {
	t.Skip("retired with notes-era Auditor/Poll; see #279")
	root, sha := newAdvancedAuditFixture(t, "")
	big := strings.Repeat("x", auditHistoryEntryBytes+1)
	addRemoteNote(t, root, reviewRequestNoteRef, sha, big, "Iron Forest Builder", "builder@forest.invalid")
	addVerifierGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)

	for attempt := range 2 {
		result, err := audit(context.Background(), root)
		if err != nil {
			t.Fatalf("attempt %d error=%v", attempt+1, err)
		}
		fragment := "note payload on " + reviewRequestNoteRef
		if !containsViolation(result.Violations, fragment) {
			t.Fatalf("attempt %d violations=%v, want %q", attempt+1, result.Violations, fragment)
		}
		if !containsViolation(result.Violations, "exactly one valid review-request note") {
			t.Fatalf("attempt %d lost Gate non-pass: %v", attempt+1, result.Violations)
		}
	}
	if refs := auditPrivateRefs(t, root); len(refs) != 0 {
		t.Fatalf("private snapshot refs not cleaned: %v", refs)
	}
	history, err := os.ReadFile(auditLogPath(root))
	if err != nil {
		t.Fatal(err)
	}
	fragment := "note payload on " + reviewRequestNoteRef
	if count := strings.Count(string(history), fragment); count != 1 {
		t.Fatalf("payload history entries=%d want 1\n%s", count, history)
	}
}

// gitExitError returns a real git semantic failure with exactly the wanted
// exit code, matching the error shape production sees from git notes show.
func gitExitError(t *testing.T, code int) error {
	t.Helper()
	var command *exec.Cmd
	switch code {
	case 1:
		command = exec.Command("git", "cat-file", "-e", strings.Repeat("0", 40))
	case 128:
		command = exec.Command("git", "cat-file", "-p", strings.Repeat("0", 40))
	default:
		t.Fatalf("unsupported git semantic exit %d", code)
	}
	err := command.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ProcessState.ExitCode() != code {
		t.Fatalf("git semantic failure exit=%v, want %d", err, code)
	}
	return err
}

func TestReadNotesFlagsMalformedListRowAndContinues(t *testing.T) {
	snapshot := capacitySnapshot()
	blob := strings.Repeat("b", 40)
	valid := strings.Repeat("e", 40)
	authorCalls := 0
	deps := auditDependencies{runGit: func(ctx context.Context, root string, args ...string) ([]byte, error) {
		if len(args) >= 3 && args[0] == "notes" && args[2] == "list" {
			return []byte("not a note row\n" + blob + " " + valid + "\n"), nil
		}
		if len(args) >= 2 && args[0] == "ls-tree" {
			return []byte("100644 blob " + blob + "\t" + valid + "\n"), nil
		}
		if len(args) >= 3 && args[0] == "notes" && args[2] == "show" {
			return []byte("{}"), nil
		}
		if len(args) >= 1 && args[0] == "log" {
			authorCalls++
			return []byte("Builder\x00builder@forest.invalid\n"), nil
		}
		t.Fatalf("unexpected git call %v", args)
		return nil, nil
	}}

	entries, violations, err := readNotes(context.Background(), "", snapshot, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Revision != valid {
		t.Fatalf("entries=%v want only the valid revision %s", entries, valid)
	}
	if len(violations) != 1 || !containsViolation(violations, "malformed note list on "+reviewRequestNoteRef) {
		t.Fatalf("malformed list violations=%v", violations)
	}
	if authorCalls != 1 {
		t.Fatalf("author reads=%d want 1 (only the valid note)", authorCalls)
	}
}

func TestReadNotesFlagsMissingTreeEntryAndContinues(t *testing.T) {
	snapshot := capacitySnapshot()
	blob := strings.Repeat("b", 40)
	absent := strings.Repeat("d", 40)
	valid := strings.Repeat("e", 40)
	authorCalls := 0
	deps := auditDependencies{runGit: func(ctx context.Context, root string, args ...string) ([]byte, error) {
		if len(args) >= 3 && args[0] == "notes" && args[2] == "list" {
			return []byte(blob + " " + absent + "\n" + blob + " " + valid + "\n"), nil
		}
		if len(args) >= 2 && args[0] == "ls-tree" {
			return []byte("100644 blob " + blob + "\t" + valid + "\n"), nil
		}
		if len(args) >= 3 && args[0] == "notes" && args[2] == "show" {
			return []byte("{}"), nil
		}
		if len(args) >= 1 && args[0] == "log" {
			authorCalls++
			return []byte("Builder\x00builder@forest.invalid\n"), nil
		}
		t.Fatalf("unexpected git call %v", args)
		return nil, nil
	}}

	entries, violations, err := readNotes(context.Background(), "", snapshot, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Revision != valid {
		t.Fatalf("entries=%v want only the valid revision %s", entries, valid)
	}
	if len(violations) != 1 || !containsViolation(violations, "missing note tree entry for "+absent) {
		t.Fatalf("missing tree entry violations=%v", violations)
	}
	if authorCalls != 1 {
		t.Fatalf("author reads=%d want 1 (only the valid note)", authorCalls)
	}
}

func TestReadNotesFlagsBlobMismatchAndContinues(t *testing.T) {
	snapshot := capacitySnapshot()
	blob := strings.Repeat("b", 40)
	wrong := strings.Repeat("c", 40)
	mismatched := strings.Repeat("d", 40)
	valid := strings.Repeat("e", 40)
	authorCalls := 0
	deps := auditDependencies{runGit: func(ctx context.Context, root string, args ...string) ([]byte, error) {
		if len(args) >= 3 && args[0] == "notes" && args[2] == "list" {
			return []byte(wrong + " " + mismatched + "\n" + blob + " " + valid + "\n"), nil
		}
		if len(args) >= 2 && args[0] == "ls-tree" {
			return []byte("100644 blob " + blob + "\t" + mismatched + "\n" + "100644 blob " + blob + "\t" + valid + "\n"), nil
		}
		if len(args) >= 3 && args[0] == "notes" && args[2] == "show" {
			return []byte("{}"), nil
		}
		if len(args) >= 1 && args[0] == "log" {
			authorCalls++
			return []byte("Builder\x00builder@forest.invalid\n"), nil
		}
		t.Fatalf("unexpected git call %v", args)
		return nil, nil
	}}

	entries, violations, err := readNotes(context.Background(), "", snapshot, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Revision != valid {
		t.Fatalf("entries=%v want only the valid revision %s", entries, valid)
	}
	if len(violations) != 1 || !containsViolation(violations, "note blob mismatch for "+mismatched) {
		t.Fatalf("blob mismatch violations=%v", violations)
	}
	if containsViolation(violations, "unexpected note tree entry") {
		t.Fatalf("blob mismatch left an unaccounted tree entry: %v", violations)
	}
	if authorCalls != 1 {
		t.Fatalf("author reads=%d want 1 (only the valid note)", authorCalls)
	}
}

func TestReadNotesFlagsDuplicateNotePathsAndContinues(t *testing.T) {
	snapshot := capacitySnapshot()
	blob := strings.Repeat("b", 40)
	duplicated := strings.Repeat("d", 40)
	valid := strings.Repeat("e", 40)
	authorCalls := 0
	deps := auditDependencies{runGit: func(ctx context.Context, root string, args ...string) ([]byte, error) {
		if len(args) >= 3 && args[0] == "notes" && args[2] == "list" {
			return []byte(blob + " " + duplicated + "\n" + blob + " " + valid + "\n"), nil
		}
		if len(args) >= 2 && args[0] == "ls-tree" {
			return []byte("100644 blob " + blob + "\t" + duplicated + "\n" +
				"100644 blob " + blob + "\t" + duplicated[:2] + "/" + duplicated[2:] + "\n" +
				"100644 blob " + blob + "\t" + valid + "\n"), nil
		}
		if len(args) >= 3 && args[0] == "notes" && args[2] == "show" {
			return []byte("{}"), nil
		}
		if len(args) >= 1 && args[0] == "log" {
			authorCalls++
			return []byte("Builder\x00builder@forest.invalid\n"), nil
		}
		t.Fatalf("unexpected git call %v", args)
		return nil, nil
	}}

	entries, violations, err := readNotes(context.Background(), "", snapshot, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Revision != duplicated || entries[1].Revision != valid {
		t.Fatalf("entries=%v want duplicated then valid", entries)
	}
	if len(violations) != 1 || !containsViolation(violations, "duplicate note paths for "+duplicated) {
		t.Fatalf("duplicate paths violations=%v", violations)
	}
	if authorCalls != 2 {
		t.Fatalf("author reads=%d want 2", authorCalls)
	}
}

func TestReadNotesFlagsMissingNoteObjectAndContinues(t *testing.T) {
	snapshot := capacitySnapshot()
	blob := strings.Repeat("b", 40)
	absent := strings.Repeat("d", 40)
	valid := strings.Repeat("e", 40)
	missingObjectErr := gitExitError(t, 128)
	authorCalls := 0
	deps := auditDependencies{runGit: func(ctx context.Context, root string, args ...string) ([]byte, error) {
		if len(args) >= 3 && args[0] == "notes" && args[2] == "list" {
			return []byte(blob + " " + absent + "\n" + blob + " " + valid + "\n"), nil
		}
		if len(args) >= 2 && args[0] == "ls-tree" {
			return []byte("100644 blob " + blob + "\t" + absent + "\n" + "100644 blob " + blob + "\t" + valid + "\n"), nil
		}
		if len(args) >= 4 && args[0] == "notes" && args[2] == "show" && args[3] == absent {
			return nil, missingObjectErr
		}
		if len(args) >= 4 && args[0] == "notes" && args[2] == "show" && args[3] == valid {
			return []byte("{}"), nil
		}
		if len(args) >= 1 && args[0] == "log" {
			authorCalls++
			return []byte("Builder\x00builder@forest.invalid\n"), nil
		}
		t.Fatalf("unexpected git call %v", args)
		return nil, nil
	}}

	entries, violations, err := readNotes(context.Background(), "", snapshot, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Revision != valid {
		t.Fatalf("entries=%v want only the valid revision %s", entries, valid)
	}
	if len(violations) != 1 || !containsViolation(violations, "missing note object on "+reviewRequestNoteRef+" for "+absent) {
		t.Fatalf("missing note object violations=%v", violations)
	}
	if authorCalls != 1 {
		t.Fatalf("author reads=%d want 1 (only the valid note)", authorCalls)
	}
}

func TestReadNotesFlagsShowMissingNoteAndContinues(t *testing.T) {
	snapshot := capacitySnapshot()
	blob := strings.Repeat("b", 40)
	absent := strings.Repeat("d", 40)
	valid := strings.Repeat("e", 40)
	missingNoteErr := gitExitError(t, 1)
	authorCalls := 0
	deps := auditDependencies{runGit: func(ctx context.Context, root string, args ...string) ([]byte, error) {
		if len(args) >= 3 && args[0] == "notes" && args[2] == "list" {
			return []byte(blob + " " + absent + "\n" + blob + " " + valid + "\n"), nil
		}
		if len(args) >= 2 && args[0] == "ls-tree" {
			return []byte("100644 blob " + blob + "\t" + absent + "\n" + "100644 blob " + blob + "\t" + valid + "\n"), nil
		}
		if len(args) >= 4 && args[0] == "notes" && args[2] == "show" && args[3] == absent {
			return nil, missingNoteErr
		}
		if len(args) >= 4 && args[0] == "notes" && args[2] == "show" && args[3] == valid {
			return []byte("{}"), nil
		}
		if len(args) >= 1 && args[0] == "log" {
			authorCalls++
			return []byte("Builder\x00builder@forest.invalid\n"), nil
		}
		t.Fatalf("unexpected git call %v", args)
		return nil, nil
	}}

	entries, violations, err := readNotes(context.Background(), "", snapshot, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Revision != valid {
		t.Fatalf("entries=%v want only the valid revision %s", entries, valid)
	}
	if len(violations) != 1 || !containsViolation(violations, "missing note on "+reviewRequestNoteRef+" for "+absent) {
		t.Fatalf("missing note violations=%v", violations)
	}
	if authorCalls != 1 {
		t.Fatalf("author reads=%d want 1 (only the valid note)", authorCalls)
	}
}

func TestAuditorFlagsCorruptNoteListState(t *testing.T) {
	t.Skip("retired with notes-era Auditor/Poll; see #279")
	cases := []struct {
		name     string
		mode     string
		dropTree bool
		want     []string
	}{
		{name: "malformed list row", mode: "append-junk", want: []string{"malformed note list on " + reviewRequestNoteRef}},
		{name: "listed note without tree entry", dropTree: true, want: []string{"missing note tree entry for "}},
		{name: "blob mismatch", mode: "reblob", want: []string{"note blob mismatch for "}},
	}
	const wrapper = `#!/bin/sh
set -eu
root=""
if [ "$1" = "-C" ]; then
  root=$2
  shift 2
fi
if [ "$1" = "notes" ] && [ "$3" = "list" ] && [ -n "${AUDIT_LIST_MODE:-}" ]; then
  out=$("$AUDIT_REAL_GIT" -C "$root" "$@")
  case "$AUDIT_LIST_MODE" in
    append-junk)
      printf '%s\n' "$out" "not a note row"
      ;;
    reblob)
      rest=${out#* }
      printf 'ffffffffffffffffffffffffffffffffffffffff %s\n' "$rest"
      ;;
  esac
  exit 0
fi
if [ "$1" = "ls-tree" ] && [ "$3" = "--full-tree" ] && [ "${AUDIT_DROP_TREE:-}" = "true" ]; then
  exit 0
fi
exec "$AUDIT_REAL_GIT" -C "$root" "$@"
`
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root, _ := testClone(t)
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			addRemoteNote(t, root, reviewRequestNoteRef, revision, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+revision+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
			installAuditGitWrapper(t, wrapper)
			t.Setenv("AUDIT_LIST_MODE", testCase.mode)
			t.Setenv("AUDIT_DROP_TREE", strconv.FormatBool(testCase.dropTree))

			assertPersistedViolationAudit(t, root, testCase.want)
		})
	}
}

func TestAuditorFlagsMissingNoteObject(t *testing.T) {
	t.Skip("retired with notes-era Auditor/Poll; see #279")
	root, _ := testClone(t)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	ref := reviewRequestNoteRef
	addRemoteNote(t, root, ref, revision, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+revision+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
	addRemoteNote(t, root, verdictNoteRef, revision, `{"schema":"forest.verdict.v1","revision":"`+revision+`","verdict":"approve","summary":"ok","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")
	fields := strings.Fields(string(runGitDir(t, root, "notes", "--ref="+ref, "list")))
	if len(fields) != 2 {
		t.Fatalf("note list=%q", fields)
	}
	blob := fields[0]
	loose := filepath.Join(root, ".git", "objects", blob[:2], blob[2:])
	if err := os.Remove(loose); err != nil {
		t.Fatal(err)
	}

	assertPersistedViolationAudit(t, root, []string{"missing note object on " + ref + " for " + revision})
}

func TestAuditorContinuesValidNotesAfterCorruptTreeRow(t *testing.T) {
	t.Skip("retired with notes-era Auditor/Poll; see #279")
	root, sha := newAdvancedAuditFixture(t, "")
	addGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)
	fields := strings.Fields(string(runGitDir(t, root, "notes", "--ref="+reviewRequestNoteRef, "list")))
	if len(fields) != 2 {
		t.Fatalf("note list=%q", fields)
	}
	installAuditGitWrapper(t, `#!/bin/sh
set -eu
root=""
if [ "$1" = "-C" ]; then
  root=$2
  shift 2
fi
if [ "$1" = "ls-tree" ] && [ "$2" = "-r" ] && [ "$3" = "--full-tree" ]; then
  case "$4" in
    *review-request)
      "$AUDIT_REAL_GIT" -C "$root" "$@"
      printf '%s\n' "$AUDIT_EXTRA_TREE_ROW"
      exit 0
      ;;
  esac
fi
exec "$AUDIT_REAL_GIT" -C "$root" "$@"
`)
	t.Setenv("AUDIT_EXTRA_TREE_ROW", "100644 blob "+fields[0]+"\tnot/a-note")

	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Violations) != 1 || !containsViolation(result.Violations, "non-SHA note tree path on "+reviewRequestNoteRef) {
		t.Fatalf("continuing Audit result=%#v", result)
	}
	state, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastResult != "violations" || !sameViolationSet(state.Violations, result.Violations) {
		t.Fatalf("persisted continuing audit state=%#v", state)
	}
	if refs := auditPrivateRefs(t, root); len(refs) != 0 {
		t.Fatalf("continuing Audit left private refs: %v", refs)
	}
	if summary := auditSummary(context.Background(), root); summary != "" {
		t.Fatalf("continuing violation Audit left AuditError=%q", summary)
	}
	state, err = readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastResult != "violations" || len(state.Violations) != 1 {
		t.Fatalf("AuditError clear cleared the violations: %#v", state)
	}
}
