package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

var worktreeSet = struct {
	mu    sync.Mutex
	locks map[string]*os.File
}{locks: make(map[string]*os.File)}

func worktreeLockPath(dir string) string {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		absolute = dir
	}
	root := filepath.Join("/tmp", fmt.Sprintf("iron-forest-%d", os.Getuid()), "worktree")
	return filepath.Join(root, blobSHA(absolute)+".lock")
}

func trackWorktree(dir string) error {
	if dir == "" {
		return errors.New("track worktree: empty path")
	}
	lock, err := holdLock(worktreeLockPath(dir))
	if err != nil {
		return fmt.Errorf("worktree %s is active: %w", dir, err)
	}
	worktreeSet.mu.Lock()
	if _, exists := worktreeSet.locks[dir]; exists {
		worktreeSet.mu.Unlock()
		dropLock(lock)
		return fmt.Errorf("worktree %s is already tracked", dir)
	}
	worktreeSet.locks[dir] = lock
	worktreeSet.mu.Unlock()
	return nil
}

func untrackWorktree(dir string) {
	worktreeSet.mu.Lock()
	lock := worktreeSet.locks[dir]
	delete(worktreeSet.locks, dir)
	worktreeSet.mu.Unlock()
	dropLock(lock)
}

func git(repo string, args ...string) error {
	_, err := gitCommand(repo, args...)
	return err
}

func gitOut(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := runOutput(cmd)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitOutRaw returns git's stdout byte for byte. Porcelain output is
// column-significant: a status line is two status characters, a space, then the
// path, and an unmodified index leaves the first column blank. Trimming that
// blank shifts every field left and eats the first character of the path, so
// any caller parsing by column must use this instead of gitOut.
func gitOutRaw(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := runOutput(cmd)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

func gitCommit(wtDir string, id CommitIdentity, msg string) error {
	cmd := exec.Command("git",
		"-c", "user.name="+id.Name,
		"-c", "user.email="+id.Email,
		"-C", wtDir, "commit", "-m", msg)
	cmd.Env = commitIdentityEnv(id)
	out, err := runCombinedOutput(cmd)
	if err != nil {
		return fmt.Errorf("git commit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func cleanIdentityEnv() []string {
	host := os.Environ()
	env := make([]string, 0, len(host)+4)
	for _, entry := range host {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL":
			continue
		}
		env = append(env, entry)
	}
	return env
}

func commitIdentityEnv(id CommitIdentity) []string {
	return append(cleanIdentityEnv(),
		"GIT_AUTHOR_NAME="+id.Name, "GIT_AUTHOR_EMAIL="+id.Email,
		"GIT_COMMITTER_NAME="+id.Name, "GIT_COMMITTER_EMAIL="+id.Email)
}

func committerIdentityEnv(id CommitIdentity) []string {
	return append(cleanIdentityEnv(),
		"GIT_COMMITTER_NAME="+id.Name, "GIT_COMMITTER_EMAIL="+id.Email)
}

func gitAsCommitter(repo string, id CommitIdentity, args ...string) error {
	cmdArgs := []string{"-c", "user.name=" + id.Name, "-c", "user.email=" + id.Email, "-C", repo}
	cmd := exec.Command("git", append(cmdArgs, args...)...)
	cmd.Env = committerIdentityEnv(id)
	out, err := runCombinedOutput(cmd)
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// createWorktree makes a fresh linked worktree for one item at the remote tip.
// The branch keeps the forest/<id>-<slug> shape so numeric GitHub ids read as
// they always have; the id segment is opaque and escaped (encodeBranchID), so a
// non-numeric tracker id — even one containing the '-' delimiter — derives an
// equally valid, reverse-lookup-able branch.
func createWorktree(repo, workspace, id, title string) (wtDir, branch, baseSHA string, err error) {
	branch = fmt.Sprintf("%s%s-%s", BranchPrefix, encodeBranchID(id), slug(title))
	wtDir = filepath.Join(workspace, "worktrees", branch)
	if err = trackWorktree(wtDir); err != nil {
		return "", "", "", err
	}
	defer func() {
		if err != nil {
			untrackWorktree(wtDir)
		}
	}()
	if err := git(repo, "fetch", "origin", "master"); err != nil {
		return "", "", "", fmt.Errorf("fetch origin/master: %w", err)
	}
	baseSHA, err = gitOut(repo, "rev-parse", "origin/master")
	if err != nil {
		return "", "", "", fmt.Errorf("origin/master: %w", err)
	}
	_ = os.RemoveAll(wtDir)
	_ = git(repo, "worktree", "prune")
	if _, err := gitOut(repo, "rev-parse", "-q", "--verify", "refs/heads/"+branch); err == nil {
		_ = git(repo, "branch", "-q", "-D", branch)
	}
	// Start at the exact sha, not at origin/master: the ref may move between
	// the rev-parse above and this add, and a run must be reproducible.
	if err := git(repo, "worktree", "add", "-b", branch, wtDir, baseSHA); err != nil {
		return "", "", "", fmt.Errorf("worktree add: %w", err)
	}
	return wtDir, branch, baseSHA, nil
}

func removeWorktree(repo, wtDir string) error {
	gitErr := git(repo, "worktree", "remove", "--force", wtDir)
	removeErr := os.RemoveAll(wtDir)
	if removeErr == nil {
		untrackWorktree(wtDir)
	}
	return errors.Join(gitErr, removeErr)
}

func cleanupWorktree(repo, wtDir string) {
	if err := removeWorktree(repo, wtDir); err != nil {
		fmt.Fprintf(os.Stderr, "forest: remove worktree %s: %v\n", wtDir, err)
	}
}

// reapOrphanWorktrees removes linked worktrees left by an interrupted process.
func reapOrphanWorktrees(repoDir string) {
	wtRoot, err := filepath.Abs(filepath.Join(repoDir, WorkspaceDir, "worktrees"))
	if err != nil {
		return
	}
	if err := git(repoDir, "worktree", "prune"); err != nil {
		fmt.Fprintf(os.Stderr, "forest: prune worktrees: %v\n", err)
		return
	}
	list, err := gitOut(repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: reap worktrees: %v\n", err)
		return
	}
	for _, entry := range strings.Split(list, "\n\n") {
		var wtDir string
		for _, line := range strings.Split(entry, "\n") {
			if strings.HasPrefix(line, "worktree ") {
				wtDir = strings.TrimPrefix(line, "worktree ")
			}
		}
		if wtDir == "" {
			continue
		}
		abs, aerr := filepath.Abs(wtDir)
		if aerr != nil || !strings.HasPrefix(abs, wtRoot+string(os.PathSeparator)) {
			continue
		}
		lock, lockErr := holdLock(worktreeLockPath(wtDir))
		if lockErr != nil {
			if !errors.Is(lockErr, errAdmissionHeld) {
				fmt.Fprintf(os.Stderr, "forest: inspect worktree owner %s: %v\n", wtDir, lockErr)
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "forest: removing stale worktree %s\n", wtDir)
		cleanupWorktree(repoDir, wtDir)
		dropLock(lock)
	}
}

// createWorktreeAtBranch adds a linked worktree at an existing branch tip.
func createWorktreeAtBranch(repo, workspace, branch string) (wtDir, baseSHA string, err error) {
	wtDir = filepath.Join(workspace, "worktrees", branch)
	if err = trackWorktree(wtDir); err != nil {
		return "", "", err
	}
	defer func() {
		if err != nil {
			untrackWorktree(wtDir)
		}
	}()
	_ = os.RemoveAll(wtDir)
	_ = git(repo, "worktree", "prune")
	if err := git(repo, "fetch", "origin"); err != nil {
		return "", "", fmt.Errorf("fetch branch %s: %w", branch, err)
	}
	if _, err := gitOut(repo, "rev-parse", "-q", "--verify", "refs/heads/"+branch); err == nil {
		_ = git(repo, "branch", "-q", "-D", branch)
	}
	baseSHA, err = gitOut(repo, "rev-parse", "origin/"+branch)
	if err != nil {
		return "", "", fmt.Errorf("branch %s not on origin: %w", branch, err)
	}
	if err := git(repo, "worktree", "add", "-b", branch, wtDir, baseSHA); err != nil {
		return "", "", fmt.Errorf("worktree add: %w", err)
	}
	return wtDir, baseSHA, nil
}
