package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

var worktreeSet = struct {
	mu   sync.Mutex
	dirs map[string]struct{}
}{dirs: make(map[string]struct{})}

func trackWorktree(dir string) {
	if dir == "" {
		return
	}
	worktreeSet.mu.Lock()
	worktreeSet.dirs[dir] = struct{}{}
	worktreeSet.mu.Unlock()
}

func untrackWorktree(dir string) {
	if dir == "" {
		return
	}
	worktreeSet.mu.Lock()
	delete(worktreeSet.dirs, dir)
	worktreeSet.mu.Unlock()
}

func trackedWorktrees() []string {
	worktreeSet.mu.Lock()
	out := make([]string, 0, len(worktreeSet.dirs))
	for dir := range worktreeSet.dirs {
		out = append(out, dir)
	}
	worktreeSet.mu.Unlock()
	sort.Strings(out)
	return out
}

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

// gitOutRaw returns git's stdout byte for byte. Porcelain output is
// column-significant: a status line is two status characters, a space, then the
// path, and an unmodified index leaves the first column blank. Trimming that
// blank shifts every field left and eats the first character of the path, so
// any caller parsing by column must use this instead of gitOut.
func gitOutRaw(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.Output()
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
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git commit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// createWorktree makes a fresh linked worktree for one item at the remote tip.
// The branch keeps the forest/<id>-<slug> shape so numeric GitHub ids read as
// they always have; the id segment is opaque, so a non-numeric tracker id
// derives an equally valid branch.
func createWorktree(repo, workspace, id, title string) (wtDir, branch, baseSHA string, err error) {
	branch = fmt.Sprintf("forest/%s-%s", id, slug(title))
	wtDir = filepath.Join(workspace, "worktrees", branch)
	trackWorktree(wtDir)
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

func removeWorktree(repo, wtDir string) {
	_ = git(repo, "worktree", "remove", "--force", wtDir)
	_ = os.RemoveAll(wtDir)
	untrackWorktree(wtDir)
}

// reapOrphanWorktrees removes linked worktrees left by an interrupted process.
func reapOrphanWorktrees(repoDir string) {
	wtRoot, err := filepath.Abs(filepath.Join(repoDir, WorkspaceDir, "worktrees"))
	if err != nil {
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
		fmt.Fprintf(os.Stderr, "forest: removing stale worktree %s\n", wtDir)
		removeWorktree(repoDir, wtDir)
	}
	if err := os.RemoveAll(wtRoot); err != nil {
		fmt.Fprintf(os.Stderr, "forest: reap worktrees: %v\n", err)
	}
}

// createWorktreeAtBranch adds a linked worktree at an existing branch tip.
func createWorktreeAtBranch(repo, workspace, branch string) (wtDir, baseSHA string, err error) {
	wtDir = filepath.Join(workspace, "worktrees", branch)
	trackWorktree(wtDir)
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
