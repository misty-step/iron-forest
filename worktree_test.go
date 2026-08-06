package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupTestRepo builds a throwaway repository with a real origin, because
// createWorktree resolves its base from the remote tip and a fixture without a
// remote would prove nothing about the path the flows actually take.
func setupTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	repo := filepath.Join(root, "work")
	run := func(dir, name string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", name, err, strings.TrimSpace(string(out)))
		}
	}
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	run(origin, "init-bare", "init", "--bare", "-b", "master")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run(repo, "init", "init", "-b", "master")
	run(repo, "config", "config", "user.email", "test@example.com")
	run(repo, "config", "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "add", "file.txt")
	run(repo, "commit", "commit", "-m", "init")
	run(repo, "remote", "remote", "add", "origin", origin)
	run(repo, "push", "push", "-q", "-u", "origin", "master")
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

// TestCreateWorktreeStartsAtTheRemoteTip pins the isolation invariant: a run
// begins at the exact sha the record reports, so the run is reproducible even
// when the remote branch moves during the pass.
func TestCreateWorktreeStartsAtTheRemoteTip(t *testing.T) {
	repo := setupTestRepo(t)
	workspace := filepath.Join(repo, WorkspaceDir)
	wtDir, branch, baseSHA, err := createWorktree(repo, workspace, issue{Number: 5, Title: "tip"})
	if err != nil {
		t.Fatal(err)
	}
	defer removeWorktree(repo, wtDir)
	head, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if head != baseSHA {
		t.Fatalf("worktree head = %q, want the reported base %q", head, baseSHA)
	}
	if want := "forest/5-tip"; branch != want {
		t.Fatalf("branch = %q, want %q", branch, want)
	}
}

// TestTrackedWorktreesIsolateLanes pins the contract the drain handler depends
// on: every live worktree is listed, and clearing one lane's worktree never
// hides another lane's, which would leak it on an abrupt exit.
func TestTrackedWorktreesIsolateLanes(t *testing.T) {
	repo := setupTestRepo(t)
	workspace := filepath.Join(repo, WorkspaceDir)
	first, _, _, err := createWorktree(repo, workspace, issue{Number: 7, Title: "one"})
	if err != nil {
		t.Fatal(err)
	}
	defer removeWorktree(repo, first)
	second, _, _, err := createWorktree(repo, workspace, issue{Number: 8, Title: "two"})
	if err != nil {
		t.Fatal(err)
	}
	defer removeWorktree(repo, second)

	if got := trackedWorktrees(); len(got) != 2 {
		t.Fatalf("trackedWorktrees() = %v, want both lanes", got)
	}
	untrackWorktree(first)
	got := trackedWorktrees()
	if len(got) != 1 || got[0] != second {
		t.Fatalf("trackedWorktrees() = %v after clearing one lane, want only %q", got, second)
	}
}
