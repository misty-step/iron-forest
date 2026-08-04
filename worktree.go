package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func git(repo string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gitOut(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func gitStrings(repo string, args ...string) ([]string, error) {
	raw, err := gitOut(repo, args...)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	return strings.Split(raw, "\n"), nil
}

func gitCommit(wtDir, msg string) error {
	cmd := exec.Command("git",
		"-c", "user.name="+commitAuthorName,
		"-c", "user.email="+commitAuthorMail,
		"-C", wtDir, "commit", "-m", msg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git commit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// createWorktree makes a fresh linked worktree for one issue at the tip of
// master, on an isolated branch. It owns the branch name.
func createWorktree(repo, workspace string, it issue) (wtDir, branch, baseSHA string, err error) {
	branch = fmt.Sprintf("forest/%d-%s", it.Number, slug(it.Title))
	wtDir = filepath.Join(workspace, "worktrees", branch)
	baseSHA, err = gitOut(repo, "rev-parse", "HEAD")
	if err != nil {
		return "", "", "", err
	}
	// Wipe any stale partial run for this branch.
	_ = os.RemoveAll(wtDir)
	_ = git(repo, "worktree", "prune")
	// Drop a stale local branch so `worktree add -b` starts clean.
	if _, err := gitOut(repo, "rev-parse", "-q", "--verify", "refs/heads/"+branch); err == nil {
		_ = git(repo, "branch", "-q", "-D", branch)
	}
	if err := git(repo, "worktree", "add", "-b", branch, wtDir, "master"); err != nil {
		return "", "", "", fmt.Errorf("worktree add: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(wtDir, ".git"), 0o755); err != nil {
		// linked worktrees have a .git file; ignore a absent dir error
		_ = err
	}
	return wtDir, branch, baseSHA, nil
}

func removeWorktree(repo, wtDir string) {
	_ = git(repo, "worktree", "remove", "--force", wtDir)
	_ = os.RemoveAll(wtDir)
}
