package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuditorBaselineAndLastGoodRevision(t *testing.T) {
	root, _ := testClone(t)
	baseline := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Violations) != 0 {
		t.Fatalf("baseline violations=%v", result.Violations)
	}
	state, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Baseline != baseline || state.LastMaster != baseline {
		t.Fatalf("baseline state=%#v", state)
	}
	if state.LastResult != "pass" || len(state.Violations) != 0 {
		t.Fatalf("baseline status=%#v", state)
	}
	if state.LastAt == "" {
		t.Fatal("baseline LastAt is empty")
	}
	if _, err := time.Parse(time.RFC3339Nano, state.LastAt); err != nil {
		t.Fatalf("baseline LastAt=%q: %v", state.LastAt, err)
	}
	passedAt := state.LastAt
	if err := os.WriteFile(filepath.Join(root, "later"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "later")
	runGitDir(t, root, "commit", "-m", "later")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/master")
	result, err = audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !containsViolation(result.Violations, "exactly one valid review-request note") {
		t.Fatalf("later violations=%v", result.Violations)
	}
	state, err = readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Baseline != baseline || state.LastMaster != baseline {
		t.Fatalf("unsafe revision became last-good: %#v", state)
	}
	if state.AuditedMaster != result.Master || state.AuditedMaster == state.LastMaster {
		t.Fatalf("audited tip=%q last-good=%q result=%q", state.AuditedMaster, state.LastMaster, result.Master)
	}
	if state.LastResult != "violations" || !containsViolation(state.Violations, "exactly one valid review-request note") {
		t.Fatalf("unsafe revision status=%#v", state)
	}
	if state.LastAt == "" {
		t.Fatal("violation LastAt is empty")
	}
	if _, err := time.Parse(time.RFC3339Nano, state.LastAt); err != nil {
		t.Fatalf("violation LastAt=%q: %v", state.LastAt, err)
	}
	if state.LastAt == passedAt {
		t.Fatalf("violation did not update LastAt from %q", passedAt)
	}
}

func TestAuditorFirstFailureDurablyAnchorsRetryAndDeduplicatesHistory(t *testing.T) {
	root, _ := testClone(t)
	baseline := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	preValidationErr := errors.New("injected pre-validation failure")
	deps := defaultAuditDependencies()
	runGit := deps.runGit
	deps.runGit = func(ctx context.Context, root string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "show" {
			return nil, preValidationErr
		}
		return runGit(ctx, root, args...)
	}
	if _, err := auditWithDependencies(context.Background(), root, deps); !errors.Is(err, preValidationErr) {
		t.Fatalf("first Audit error=%v", err)
	}
	state, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Baseline != baseline || state.LastMaster != "" {
		t.Fatalf("first failed Audit state=%#v", state)
	}
	if state.LastAt != "" || state.LastResult != "" || len(state.Violations) != 0 {
		t.Fatalf("first failed Audit recorded a result: %#v", state)
	}
	if _, err := os.Stat(auditLogPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first failed Audit created policy history: %v", err)
	}

	runGitDir(t, root, "checkout", "--orphan", "rewritten")
	runGitDir(t, root, "rm", "-rf", ".")
	config := []byte(`repo: owner/name
agents:
  builder: {poll: "forest poll builder", interval: 1}
checks:
  - {name: test, run: "go test ./..."}
`)
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unrelated"), []byte("unrelated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "forest.yaml", "unrelated")
	runGitDir(t, root, "commit", "-m", "unrelated tip")
	unrelated := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	addGateNotes(t, root, unrelated, `[{"name":"test","ok":true,"exit":0}]`)
	runGitDir(t, root, "push", "--force", "origin", "HEAD:refs/heads/master")

	want := "non-fast-forward from " + baseline + " to " + unrelated
	for attempt := range 2 {
		result, err := audit(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Advanced || len(result.Violations) != 1 || !containsViolation(result.Violations, want) {
			t.Fatalf("attempt %d result=%#v, want %s", attempt+1, result, want)
		}
		state, err = readAuditState(root)
		if err != nil {
			t.Fatal(err)
		}
		if state.Baseline != baseline || state.LastMaster != "" {
			t.Fatalf("attempt %d state=%#v", attempt+1, state)
		}
	}
	history, err := os.ReadFile(auditLogPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(history), want); count != 1 {
		t.Fatalf("unchanged violation history entries=%d want 1\n%s", count, history)
	}
}

func TestSameViolationSetIgnoresOrderAndDuplicates(t *testing.T) {
	if !sameViolationSet([]string{"first", "second", "first"}, []string{"second", "first"}) {
		t.Fatal("equal violation sets differed")
	}
	if sameViolationSet([]string{"first"}, []string{"second"}) {
		t.Fatal("different violation sets matched")
	}
}

func TestAuditorRevalidatesUnchangedNonBaselineAndClearsCurrentViolations(t *testing.T) {
	root, sha := newAdvancedAuditFixture(t, "")
	addGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)
	if result, err := audit(context.Background(), root); err != nil || len(result.Violations) != 0 {
		t.Fatalf("initial passing audit result=%#v err=%v", result, err)
	}
	runGitDir(t, root, "notes", "--ref="+checksNoteRef, "remove", sha)
	runGitDir(t, root, "push", "--force", "origin", checksNoteRef+":"+checksNoteRef)
	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Advanced || !containsViolation(result.Violations, "no passing checks") {
		t.Fatalf("unchanged evidence loss result=%#v", result)
	}
	state, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastMaster != sha || len(state.Violations) == 0 {
		t.Fatalf("violation state=%#v", state)
	}
	if state.LastResult != "violations" || !containsViolation(state.Violations, "no passing checks") {
		t.Fatalf("violation status=%#v", state)
	}
	if state.LastAt == "" {
		t.Fatal("violation LastAt is empty")
	}
	if _, err := time.Parse(time.RFC3339Nano, state.LastAt); err != nil {
		t.Fatalf("violation LastAt=%q: %v", state.LastAt, err)
	}
	violatingAt := state.LastAt
	checksPayload := `{"schema":"forest.checks.v1","revision":"` + sha + `","results":[{"name":"test","ok":true,"exit":0}],"time":"2026-08-10T00:00:00Z"}`
	addRemoteNote(t, root, checksNoteRef, sha, checksPayload, "Iron Forest Verifier", "verifier@forest.invalid")
	result, err = audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Violations) != 0 {
		t.Fatalf("cleared violations=%v", result.Violations)
	}
	persisted, err := os.ReadFile(auditStatePath(root))
	if err != nil {
		t.Fatal(err)
	}
	var current AuditState
	if err := json.Unmarshal(persisted, &current); err != nil {
		t.Fatal(err)
	}
	if len(current.Violations) != 0 {
		t.Fatalf("stale current violations=%v", current.Violations)
	}
	if current.LastResult != "pass" {
		t.Fatalf("cleared status=%#v", current)
	}
	if current.LastAt == "" {
		t.Fatal("cleared LastAt is empty")
	}
	if _, err := time.Parse(time.RFC3339Nano, current.LastAt); err != nil {
		t.Fatalf("cleared LastAt=%q: %v", current.LastAt, err)
	}
	if current.LastAt == violatingAt {
		t.Fatalf("passing audit did not update LastAt from %q", violatingAt)
	}
	logData, err := os.ReadFile(auditLogPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "no passing checks") {
		t.Fatal("audit history did not retain prior violation")
	}
}

func TestAuditorAnchorsAncestryAtLastGoodRevision(t *testing.T) {
	root, lastGood := newAdvancedAuditFixture(t, "")
	state, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	baseline := state.Baseline
	if baseline == "" || baseline == lastGood || state.LastMaster != baseline || len(state.Violations) != 0 {
		t.Fatalf("baseline fixture state=%#v", state)
	}

	addGateNotes(t, root, lastGood, `[{"name":"test","ok":true,"exit":0}]`)
	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Violations) != 0 {
		t.Fatalf("last-good audit violations=%v", result.Violations)
	}
	state, err = readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Baseline != baseline || state.LastMaster != lastGood {
		t.Fatalf("last-good fixture state=%#v", state)
	}

	runGitDir(t, root, "checkout", "-b", "replacement", baseline)
	if err := os.WriteFile(filepath.Join(root, "replacement"), []byte("replacement\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "replacement")
	runGitDir(t, root, "commit", "-m", "replacement")
	replaced := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "merge-base", "--is-ancestor", baseline, replaced)
	if cmd := exec.Command("git", "-C", root, "merge-base", "--is-ancestor", lastGood, replaced); cmd.Run() == nil {
		t.Fatalf("replacement %s unexpectedly descends from last-good %s", replaced, lastGood)
	}

	addGateNotes(t, root, replaced, `[{"name":"test","ok":true,"exit":0}]`)
	runGitDir(t, root, "push", "--force", "origin", "HEAD:refs/heads/master")
	result, err = audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	want := "non-fast-forward from " + lastGood + " to " + replaced
	if !result.Advanced || len(result.Violations) != 1 || !containsViolation(result.Violations, want) {
		t.Fatalf("replacement audit result=%#v, want %s", result, want)
	}
	state, err = readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Baseline != baseline || state.LastMaster != lastGood {
		t.Fatalf("replacement changed ancestry anchor: %#v", state)
	}
}

func TestAuditorRequiresDurableStateAndHistory(t *testing.T) {
	t.Run("unique state temp", func(t *testing.T) {
		root, _ := testClone(t)
		if err := os.MkdirAll(filepath.Dir(auditStatePath(root)), 0o755); err != nil {
			t.Fatal(err)
		}
		sentinel := auditStatePath(root) + ".tmp"
		if err := os.WriteFile(sentinel, []byte("sentinel\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := audit(context.Background(), root); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(sentinel)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "sentinel\n" {
			t.Fatalf("fixed audit temp was overwritten: %q", data)
		}
	})

	t.Run("state file sync", func(t *testing.T) {
		root, sha := newAdvancedAuditFixture(t, "")
		addGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)
		before, err := os.ReadFile(auditStatePath(root))
		if err != nil {
			t.Fatal(err)
		}
		deps := defaultAuditDependencies()
		deps.syncFile = func(file *os.File) error {
			if strings.HasPrefix(filepath.Base(file.Name()), ".audit.json-") {
				return errors.New("injected state sync failure")
			}
			return file.Sync()
		}

		if _, err := auditWithDependencies(context.Background(), root, deps); err == nil || !strings.Contains(err.Error(), "injected state sync failure") {
			t.Fatalf("state sync error=%v", err)
		}
		after, err := os.ReadFile(auditStatePath(root))
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatalf("failed state sync changed audit state\nbefore=%s\nafter=%s", before, after)
		}
	})

	t.Run("history sync", func(t *testing.T) {
		root, _ := newAdvancedAuditFixture(t, "")
		beforeState, err := os.ReadFile(auditStatePath(root))
		if err != nil {
			t.Fatal(err)
		}
		beforeHistory := []byte("prior history\n")
		if err := os.WriteFile(auditLogPath(root), beforeHistory, 0o644); err != nil {
			t.Fatal(err)
		}
		deps := defaultAuditDependencies()
		deps.syncFile = func(file *os.File) error {
			if strings.HasPrefix(filepath.Base(file.Name()), auditLogTempPrefix) {
				return errors.New("injected history sync failure")
			}
			return file.Sync()
		}

		if _, err := auditWithDependencies(context.Background(), root, deps); err == nil || !strings.Contains(err.Error(), "injected history sync failure") {
			t.Fatalf("history sync error=%v", err)
		}
		afterState, err := os.ReadFile(auditStatePath(root))
		if err != nil {
			t.Fatal(err)
		}
		if string(afterState) != string(beforeState) {
			t.Fatalf("failed history sync changed audit state\nbefore=%s\nafter=%s", beforeState, afterState)
		}
		afterHistory, err := os.ReadFile(auditLogPath(root))
		if err != nil {
			t.Fatal(err)
		}
		if string(afterHistory) != string(beforeHistory) {
			t.Fatalf("failed history sync changed history\nbefore=%s\nafter=%s", beforeHistory, afterHistory)
		}
		temps, err := filepath.Glob(filepath.Join(filepath.Dir(auditLogPath(root)), auditLogTempPrefix+"*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(temps) != 0 {
			t.Fatalf("failed history sync left temps: %v", temps)
		}
	})

	t.Run("audit directory sync", func(t *testing.T) {
		root, sha := newAdvancedAuditFixture(t, "")
		addGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)
		deps := defaultAuditDependencies()
		deps.syncFile = func(file *os.File) error {
			if filepath.Clean(file.Name()) == filepath.Clean(filepath.Dir(auditStatePath(root))) {
				return errors.New("injected directory sync failure")
			}
			return file.Sync()
		}

		if _, err := auditWithDependencies(context.Background(), root, deps); err == nil || !strings.Contains(err.Error(), "injected directory sync failure") {
			t.Fatalf("directory sync error=%v", err)
		}
	})
}

func TestAuditHistoryRetainsLatestEntriesAndCleansStaleTemps(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Dir(auditLogPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	var old strings.Builder
	oldCount := auditHistoryEntries + 5
	for index := range oldCount {
		fmt.Fprintf(&old, "old-%04d\n", index)
	}
	if err := os.WriteFile(auditLogPath(root), []byte(old.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(filepath.Dir(auditLogPath(root)), auditLogTempPrefix+"stale")
	if err := os.WriteFile(stale, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := appendAuditLog(context.Background(), root, []string{"new"}, defaultAuditDependencies()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale audit temp remains: %v", err)
	}
	data, err := os.ReadFile(auditLogPath(root))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != auditHistoryEntries {
		t.Fatalf("history entries=%d want %d", len(lines), auditHistoryEntries)
	}
	firstOld := oldCount + 1 - auditHistoryEntries
	for index := range auditHistoryEntries - 1 {
		if want := fmt.Sprintf("old-%04d", firstOld+index); lines[index] != want {
			t.Fatalf("history entry %d=%q want %q", index, lines[index], want)
		}
	}
	if !strings.HasSuffix(lines[len(lines)-1], " new") {
		t.Fatalf("latest history entry=%q", lines[len(lines)-1])
	}
}

func TestAuditHistoryRejectsImpossibleEntriesWithoutChangingHistory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Dir(auditLogPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	before := []byte("prior history\n")
	if err := os.WriteFile(auditLogPath(root), before, 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		violation string
		want      string
	}{
		{name: "oversized", violation: strings.Repeat("x", auditHistoryEntryBytes+1), want: "exceeds"},
		{name: "line break", violation: "first\nsecond", want: "line break"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := appendAuditLog(context.Background(), root, []string{test.violation}, defaultAuditDependencies())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("append error=%v want %q", err, test.want)
			}
			after, err := os.ReadFile(auditLogPath(root))
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("rejected entry changed history\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
}

func TestAuditorChangesVerdictDoesNotApproveMaster(t *testing.T) {
	root, sha := newAdvancedAuditFixture(t, "")
	addRemoteNote(t, root, reviewRequestNoteRef, sha, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+sha+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
	addRemoteNote(t, root, checksNoteRef, sha, `{"schema":"forest.checks.v1","revision":"`+sha+`","results":[{"name":"test","ok":true,"exit":0}],"time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")
	addRemoteNote(t, root, verdictNoteRef, sha, `{"schema":"forest.verdict.v1","revision":"`+sha+`","verdict":"changes","summary":"repair required","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")

	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !containsViolation(result.Violations, "has no approve verdict note") {
		t.Fatalf("changes verdict result=%#v", result)
	}
	state, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastMaster == sha {
		t.Fatalf("changes verdict advanced last good master to %s", sha)
	}
}

func TestAuditSyncsRootWhenItCreatesForest(t *testing.T) {
	root, _ := testClone(t)
	deps := defaultAuditDependencies()
	rootSyncs := 0
	deps.syncFile = func(file *os.File) error {
		if filepath.Clean(file.Name()) == filepath.Clean(root) {
			rootSyncs++
		}
		return file.Sync()
	}

	if _, err := auditWithDependencies(context.Background(), root, deps); err != nil {
		t.Fatal(err)
	}
	if _, err := auditWithDependencies(context.Background(), root, deps); err != nil {
		t.Fatal(err)
	}
	if rootSyncs != 1 {
		t.Fatalf("repository root syncs=%d want one first-creation sync", rootSyncs)
	}
}
