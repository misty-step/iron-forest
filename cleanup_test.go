package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReservedGarbageCollectionRemovesRealResidue(t *testing.T) {
	root, _ := testClone(t)
	runID := newRunID("builder", time.Unix(1, 0))
	worktree := forestPath(root, "worktrees", runID)
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "worktree", "add", "--detach", worktree, "HEAD")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	reservedRefs := []string{
		"refs/notes/forest/private/" + runID + "/builder/review-request/residue",
		"refs/notes/forest-poll/snapshot/verdict",
		"refs/notes/forest-audit/snapshot/checks",
		"refs/heads/forest-audit/snapshot",
	}
	for _, ref := range reservedRefs {
		runGitDir(t, root, "update-ref", ref, revision)
	}

	staleTemps := []string{".audit.json-dead", ".audit.log-dead", "triggers.json.dead.tmp"}
	for _, name := range staleTemps {
		if err := os.WriteFile(forestPath(root, name), []byte("stale\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runLog := forestPath(root, "runs", runID+".log")
	if err := os.MkdirAll(filepath.Dir(runLog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runLog, []byte("preserve run evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	transactionLog := filepath.Join(t.TempDir(), "transactions")
	transactionCount := filepath.Join(t.TempDir(), "transaction-count")
	t.Setenv("CLEANUP_REAL_GIT", realGit)
	t.Setenv("CLEANUP_TRANSACTION_LOG", transactionLog)
	t.Setenv("CLEANUP_TRANSACTION_COUNT", transactionCount)
	gitWrapper := filepath.Join(t.TempDir(), "git")
	wrapper := `#!/bin/sh
set -eu
if [ "${1-}" = update-ref ] && [ "${2-}" = --no-deref ] && [ "${3-}" = --stdin ]; then
  input="$CLEANUP_TRANSACTION_LOG.input"
  cat > "$input"
  cat "$input" >> "$CLEANUP_TRANSACTION_LOG"
  printf '1\n' >> "$CLEANUP_TRANSACTION_COUNT"
  exec "$CLEANUP_REAL_GIT" "$@" < "$input"
fi
exec "$CLEANUP_REAL_GIT" "$@"
`
	if err := os.WriteFile(gitWrapper, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.GitPath = gitWrapper
	if err := cleanupReservedResidue(root, runner); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(worktree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reserved worktree survived: %v", err)
	}
	registry := string(runGitDir(t, root, "worktree", "list", "--porcelain"))
	if strings.Contains(registry, worktree) {
		t.Fatalf("reserved worktree registry survived:\n%s", registry)
	}
	namespaces := reservedRefNamespaces()
	refs := strings.Fields(string(runGitDir(t, root, append([]string{"for-each-ref", "--format=%(refname)"}, namespaces[:]...)...)))
	if len(refs) != 0 {
		t.Fatalf("reserved refs survived: %v", refs)
	}
	for _, name := range staleTemps {
		if _, err := os.Stat(forestPath(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reserved temp %s survived: %v", name, err)
		}
	}
	if data, err := os.ReadFile(runLog); err != nil || string(data) != "preserve run evidence\n" {
		t.Fatalf("Run log changed: data=%q err=%v", data, err)
	}
	count, err := os.ReadFile(transactionCount)
	if err != nil || string(count) != "1\n" {
		t.Fatalf("update-ref transaction count=%q err=%v, want one", count, err)
	}
	transaction, err := os.ReadFile(transactionLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(transaction), "start\n") || !strings.HasSuffix(string(transaction), "prepare\ncommit\n") {
		t.Fatalf("update-ref input is not one explicit transaction:\n%s", transaction)
	}
	for _, ref := range reservedRefs {
		if !strings.Contains(string(transaction), "delete "+ref+"\n") {
			t.Fatalf("update-ref transaction omitted %s:\n%s", ref, transaction)
		}
	}
}

func TestReservedGarbageCollectionPreservesForeignState(t *testing.T) {
	root, _ := testClone(t)
	manualWorktree := forestPath(root, "worktrees", "manual-checkout")
	if err := os.MkdirAll(filepath.Dir(manualWorktree), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "worktree", "add", "--detach", manualWorktree, "HEAD")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	foreignRefs := []string{
		reviewRequestNoteRef,
		checksNoteRef,
		verdictNoteRef,
		"refs/notes/forest/privateish/keep",
		"refs/notes/forest-pollish/keep",
		"refs/notes/forest-auditish/keep",
		"refs/heads/forest-auditish/keep",
		"refs/heads/operator/keep",
	}
	for _, ref := range foreignRefs {
		runGitDir(t, root, "update-ref", ref, revision)
	}

	foreignFiles := map[string]string{
		"audit.json":             "canonical audit state\n",
		"audit.log":              "canonical audit history\n",
		"triggers.json":          "canonical trigger health\n",
		"runs.jsonl":             "canonical Ledger history\n",
		".audit.json.keep":       "foreign\n",
		".audit.log.keep":        "foreign\n",
		"triggers.json.keep":     "foreign\n",
		".runs.jsonl-stale-temp": "Ledger-owned\n",
	}
	for name, content := range foreignFiles {
		if err := os.WriteFile(forestPath(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	foreignRunLog := forestPath(root, "runs", "foreign.log")
	if err := os.MkdirAll(filepath.Dir(foreignRunLog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreignRunLog, []byte("Run retention owns this\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cleanupReservedResidue(root, NewRunner(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manualWorktree); err != nil {
		t.Fatalf("foreign worktree removed: %v", err)
	}
	registry := string(runGitDir(t, root, "worktree", "list", "--porcelain"))
	if !strings.Contains(registry, manualWorktree) {
		t.Fatalf("foreign worktree registry removed:\n%s", registry)
	}
	for _, ref := range foreignRefs {
		if got := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", ref))); got != revision {
			t.Fatalf("foreign ref %s=%s, want %s", ref, got, revision)
		}
	}
	for name, content := range foreignFiles {
		if data, err := os.ReadFile(forestPath(root, name)); err != nil || string(data) != content {
			t.Fatalf("foreign file %s changed: data=%q err=%v", name, data, err)
		}
	}
	if data, err := os.ReadFile(foreignRunLog); err != nil || string(data) != "Run retention owns this\n" {
		t.Fatalf("foreign Run log changed: data=%q err=%v", data, err)
	}
}

func TestReservedGarbageCollectionJoinsAllAttemptedErrors(t *testing.T) {
	root, _ := testClone(t)
	runID := newRunID("builder", time.Unix(2, 0))
	if err := os.MkdirAll(forestPath(root, "worktrees", runID), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".audit.json-dead", ".audit.log-dead", "triggers.json.dead.tmp"} {
		if err := os.WriteFile(forestPath(root, name), []byte("stale\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	calls := filepath.Join(t.TempDir(), "calls")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLEANUP_CALLS", calls)
	t.Setenv("CLEANUP_REAL_GIT", realGit)
	failingGit := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CLEANUP_CALLS\"\nif [ \"${1-}\" = worktree ]; then exit 91; fi\nexec \"$CLEANUP_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(failingGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.GitPath = failingGit
	auditStateErr := errors.New("audit state temp removal failed")
	auditLogErr := errors.New("audit log temp removal failed")
	triggerErr := errors.New("trigger temp removal failed")
	removeErrors := map[string]error{
		".audit.json-dead":       auditStateErr,
		".audit.log-dead":        auditLogErr,
		"triggers.json.dead.tmp": triggerErr,
	}
	remove := func(path string) error { return removeErrors[filepath.Base(path)] }

	err = cleanupReservedResidueWith(context.Background(), root, runner, remove)
	if err == nil {
		t.Fatal("cleanup succeeded despite attempted worktree and temp failures")
	}
	for _, want := range []error{auditStateErr, auditLogErr, triggerErr} {
		if !errors.Is(err, want) {
			t.Fatalf("cleanup error %v does not join %v", err, want)
		}
	}
	for _, want := range []string{
		"remove reserved worktree " + runID,
		"prune reserved worktree registry",
		"remove reserved temp .audit.json-dead",
		"remove reserved temp .audit.log-dead",
		"remove reserved temp triggers.json.dead.tmp",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("cleanup error %q omitted %q", err, want)
		}
	}
	callLog, readErr := os.ReadFile(calls)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, want := range []string{"worktree remove --force", "worktree prune --expire=now", "for-each-ref --format=%(refname)"} {
		if !strings.Contains(string(callLog), want) {
			t.Fatalf("cleanup did not attempt %q; calls:\n%s", want, callLog)
		}
	}
}

func TestReservedGarbageCollectionNoResidueFastPath(t *testing.T) {
	root, _ := testClone(t)
	if err := cleanupReservedResidue(root, NewRunner(root)); err != nil {
		t.Fatalf("empty reserved garbage collection failed: %v", err)
	}
}

func TestReservedGarbageCollectionRemovesStaleLiveRunRecords(t *testing.T) {
	root, _ := testClone(t)
	runLog := forestPath(root, "runs", "1787410241954942809-builder.log")
	if err := os.MkdirAll(filepath.Dir(runLog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runLog, []byte("preserve run evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := liveRunRecord{RunID: "1787410241954942809-builder", Agent: "builder", StartedAt: "2026-08-22T14:50:41Z"}
	if err := writeLiveRun(liveRunPath(root, "builder"), record); err != nil {
		t.Fatal(err)
	}

	if err := cleanupReservedResidue(root, NewRunner(root)); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(liveRunPath(root, "builder")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale live run record survived: %v", err)
	}
	if data, err := os.ReadFile(runLog); err != nil || string(data) != "preserve run evidence\n" {
		t.Fatalf("Run log changed: data=%q err=%v", data, err)
	}
}
