package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupTestRepo builds a throwaway git repo on a master branch with one commit
// so worktree helpers have something real to operate on.
func setupTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	must := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", name, err, strings.TrimSpace(string(out)))
		}
	}
	must("init", "init", "-b", "master")
	must("config", "config", "user.email", "test@example.com")
	must("config", "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	must("add", "add", "file.txt")
	must("commit", "commit", "-m", "init")
	return repo
}

// currentWorktrees lists the worktree paths git has registered for a repo.
func currentWorktrees(t *testing.T, repo string) []string {
	t.Helper()
	out, err := gitOut(repo, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	var wts []string
	for _, entry := range strings.Split(out, "\n\n") {
		for _, l := range strings.Split(entry, "\n") {
			if strings.HasPrefix(l, "worktree ") {
				wts = append(wts, strings.TrimPrefix(l, "worktree "))
			}
		}
	}
	return wts
}

// TestReapOrphanWorktreesRemovesStaleRun proves a worktree left behind by an
// abnormal exit is removed at the next startup. It drives real git so the
// registry entry, not just the directory, goes away.
func TestReapOrphanWorktreesRemovesStaleRun(t *testing.T) {
	repo := setupTestRepo(t)
	workspace := filepath.Join(repo, WorkspaceDir)
	wtDir, _, _, err := createWorktree(repo, workspace, issue{Number: 44, Title: "leak"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("worktree %s was not created: %v", wtDir, err)
	}
	// Leak it: deliberately skip removeWorktree, the way an os.Exit would.
	reapOrphanWorktrees(repo)
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still exists after reap", wtDir)
	}
	for _, wt := range currentWorktrees(t, repo) {
		if filepath.Clean(wt) == filepath.Clean(wtDir) {
			t.Fatalf("stale worktree %s still registered after reap", wtDir)
		}
	}
}

// TestCurrentWorktreeTracksInFlight pins the handler-facing contract: the
// run's worktree is recorded so an abrupt os.Exit can still remove it, and it
// is cleared once the run no longer owns it.
func TestCurrentWorktreeTracksInFlight(t *testing.T) {
	repo := setupTestRepo(t)
	workspace := filepath.Join(repo, WorkspaceDir)
	wtDir, _, _, err := createWorktree(repo, workspace, issue{Number: 7, Title: "track"})
	if err != nil {
		t.Fatal(err)
	}
	defer removeWorktree(repo, wtDir)
	// createWorktree registers the worktree in the tracker before git adds it,
	// so there is no window in which the second-signal handler would leak it.
	if got := currentWorktreeDir(); got != wtDir {
		t.Fatalf("currentWorktreeDir() right after createWorktree = %q, want %q", got, wtDir)
	}
	setCurrentWorktree("")
	if got := currentWorktreeDir(); got != "" {
		t.Fatalf("currentWorktreeDir() = %q after clear, want empty", got)
	}
}
