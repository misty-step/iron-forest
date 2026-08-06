package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// rebaseTestGit runs git against a test repository, failing the test on error.
func rebaseTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", cmdArgs...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(cmdArgs, " "), err, strings.TrimSpace(string(out)))
	}
}

// rebaseTestGitOut runs git and returns its trimmed stdout, failing the test on error.
func rebaseTestGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", cmdArgs...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(cmdArgs, " "), err)
	}
	return strings.TrimSpace(string(out))
}

func rebaseTestWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRebaseOntoMasterRebasesBehindBranch proves a branch behind master by a
// non-conflicting commit is rebased onto origin/master, its new head is pushed
// with force, and a checks note written at that head keys to the post-rebase
// head and not the pre-rebase one.
func TestRebaseOntoMasterRebasesBehindBranch(t *testing.T) {
	repo := setupTestRepo(t)
	workspace := filepath.Join(repo, WorkspaceDir)

	// Build a branch from the current master, then advance master.
	rebaseTestGit(t, repo, "checkout", "-q", "-b", "forest/9-change")
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/forest/9-change")
	rebaseTestGit(t, repo, "checkout", "-q", "master")
	rebaseTestWriteFile(t, filepath.Join(repo, "master.txt"), "master\n")
	rebaseTestGit(t, repo, "add", "master.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "master work")
	rebaseTestGit(t, repo, "push", "-q", "origin", "master")

	wtDir, oldHead, err := createWorktreeAtBranch(repo, workspace, "forest/9-change")
	if err != nil {
		t.Fatal(err)
	}
	defer removeWorktree(repo, wtDir)

	newHead, err := rebaseOntoMaster(wtDir, "forest/9-change")
	if err != nil {
		t.Fatal(err)
	}
	if newHead == oldHead {
		t.Fatalf("rebase produced no new head: both %q", newHead)
	}
	if _, err := os.Stat(filepath.Join(wtDir, "master.txt")); err != nil {
		t.Fatalf("rebased worktree lacks the master commit: %v", err)
	}
	if remoteHead := remoteBranchHead(t, repo, "forest/9-change"); remoteHead != newHead {
		t.Fatalf("origin branch = %q, want pushed post-rebase head %q", remoteHead, newHead)
	}

	// The checks note written by Act must key to the post-rebase head.
	note := checksNote{Status: "pass", RunID: "run-1", Results: []checkResult{{Name: "go test", Code: 0, Seconds: 1, Output: "ok"}}}
	if err := writeChecks(repo, newHead, note); err != nil {
		t.Fatalf("write checks on post-rebase head: %v", err)
	}
	if got, ok, err := readChecks(repo, newHead); err != nil || !ok || got.Status != "pass" {
		t.Fatalf("checks on post-rebase head = (found=%v, err=%v), want found pass note", ok, err)
	}
	if _, ok, err := readChecks(repo, oldHead); err != nil || ok {
		t.Fatalf("checks on pre-rebase head = (found=%v, err=%v), want not found", ok, err)
	}
}

// TestRebaseOntoMasterConflictNamesPaths proves a conflicting rebase returns an
// error naming the conflicting paths rather than a bare exit status, so a human
// sees what a robot could not resolve.
func TestRebaseOntoMasterConflictNamesPaths(t *testing.T) {
	repo := setupTestRepo(t)
	workspace := filepath.Join(repo, WorkspaceDir)

	// The branch edits file.txt its own way; master edits it differently.
	rebaseTestGit(t, repo, "checkout", "-q", "-b", "forest/9-conflict")
	rebaseTestWriteFile(t, filepath.Join(repo, "file.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "file.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch edit")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/forest/9-conflict")
	rebaseTestGit(t, repo, "checkout", "-q", "master")
	rebaseTestWriteFile(t, filepath.Join(repo, "file.txt"), "master\n")
	rebaseTestGit(t, repo, "add", "file.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "master edit")
	rebaseTestGit(t, repo, "push", "-q", "origin", "master")

	wtDir, _, err := createWorktreeAtBranch(repo, workspace, "forest/9-conflict")
	if err != nil {
		t.Fatal(err)
	}
	defer removeWorktree(repo, wtDir)

	_, err = rebaseOntoMaster(wtDir, "forest/9-conflict")
	if err == nil {
		t.Fatal("conflicting rebase returned no error")
	}
	if !strings.Contains(err.Error(), "file.txt") {
		t.Errorf("error %q does not name the conflicting path", err)
	}
	if strings.TrimSpace(err.Error()) == "exit status 1" {
		t.Errorf("error %q is a bare exit status", err)
	}
}

// TestRebaseOntoMasterLeavesCurrentBranchUntouched proves a branch that already
// contains origin/master is not rebased and its head does not change.
func TestRebaseOntoMasterLeavesCurrentBranchUntouched(t *testing.T) {
	repo := setupTestRepo(t)
	workspace := filepath.Join(repo, WorkspaceDir)

	rebaseTestGit(t, repo, "checkout", "-q", "-b", "forest/9-current")
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/forest/9-current")
	rebaseTestGit(t, repo, "checkout", "-q", "master")

	wtDir, oldHead, err := createWorktreeAtBranch(repo, workspace, "forest/9-current")
	if err != nil {
		t.Fatal(err)
	}
	defer removeWorktree(repo, wtDir)

	newHead, err := rebaseOntoMaster(wtDir, "forest/9-current")
	if err != nil {
		t.Fatal(err)
	}
	if newHead != oldHead {
		t.Fatalf("current branch was rebased: head changed %q -> %q", oldHead, newHead)
	}
	if remoteHead := remoteBranchHead(t, repo, "forest/9-current"); remoteHead != oldHead {
		t.Fatalf("origin branch changed to %q, want unchanged %q", remoteHead, oldHead)
	}
}

// remoteBranchHead reads the head of one branch advertised by origin.
func remoteBranchHead(t *testing.T, repo, branch string) string {
	t.Helper()
	out := rebaseTestGitOut(t, repo, "ls-remote", "origin", "refs/heads/"+branch)
	fields := strings.Fields(out)
	if len(fields) == 0 {
		t.Fatalf("origin branch %q not found", branch)
	}
	return fields[0]
}
