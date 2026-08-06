package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// currentWorktree holds the directory of the in-flight run's linked worktree.
// The second-signal handler reads it so an abrupt os.Exit can still remove the
// worktree that deferred cleanup would otherwise leak.
var currentWorktree atomic.Value // holds string

// setCurrentWorktree records the directory of the in-flight run's worktree.
// Pass "" to clear it once the run no longer needs the worktree.
func setCurrentWorktree(dir string) {
	currentWorktree.Store(dir)
}

// currentWorktreeDir returns the in-flight worktree directory, or "" if the
// loop is not inside a run.
func currentWorktreeDir() string {
	v := currentWorktree.Load()
	if v == nil {
		return ""
	}
	return v.(string)
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
	// Register the path in the tracker before git creates the worktree so the
	// second-signal handler can still remove it even if the signal lands in the
	// window between git registering the worktree and the caller reading wtDir.
	setCurrentWorktree(wtDir)
	defer func() {
		if err != nil {
			setCurrentWorktree("")
		}
	}()
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
	return wtDir, branch, baseSHA, nil
}

func removeWorktree(repo, wtDir string) {
	_ = git(repo, "worktree", "remove", "--force", wtDir)
	_ = os.RemoveAll(wtDir)
}

// reapOrphanWorktrees removes every linked worktree a previous run left behind
// on abnormal exit. It is called once at daemon startup, before any new run,
// so a worktree leaked by a SIGTERM mid-run is cleaned on the next start.
// Each removal is logged.
func reapOrphanWorktrees(repoDir string) {
	// Remove every registered worktree that lives under the workspace. git's
	// registry is authoritative, so this also catches registry-only entries a
	// leaked directory wipe left behind.
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
		for _, l := range strings.Split(entry, "\n") {
			if strings.HasPrefix(l, "worktree ") {
				wtDir = strings.TrimPrefix(l, "worktree ")
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
	// Leftover intermediate directories are not registered worktrees (e.g. the
	// forest/ parent created by git worktree add); drop the whole tree so none
	// survive a restart. Any directory here is already an orphan at startup.
	if err := os.RemoveAll(wtRoot); err != nil {
		fmt.Fprintf(os.Stderr, "forest: reap worktrees: %v\n", err)
	}
}

// createWorktreeAtBranch adds a linked worktree on an existing remote branch
// at its current tip. The reaction loop uses it to re-enter a PR that needs a
// fix (human change request or failing CI). It owns the branch name.
func createWorktreeAtBranch(repo, workspace, branch string) (wtDir, baseSHA string, err error) {
	wtDir = filepath.Join(workspace, "worktrees", branch)
	// Register before git creates the worktree, as createWorktree does, so a
	// second signal in the creation window still finds it in the tracker.
	setCurrentWorktree(wtDir)
	defer func() {
		if err != nil {
			setCurrentWorktree("")
		}
	}()
	_ = os.RemoveAll(wtDir)
	_ = git(repo, "worktree", "prune")
	_ = git(repo, "fetch", "origin")
	// Drop a stale local branch so `worktree add -b` starts at origin head.
	if _, err := gitOut(repo, "rev-parse", "-q", "--verify", "refs/heads/"+branch); err == nil {
		_ = git(repo, "branch", "-q", "-D", branch)
	}
	baseSHA, err = gitOut(repo, "rev-parse", "origin/"+branch)
	if err != nil {
		return "", "", fmt.Errorf("branch %s not on origin: %w", branch, err)
	}
	if err := git(repo, "worktree", "add", "-b", branch, wtDir, "origin/"+branch); err != nil {
		return "", "", fmt.Errorf("worktree add: %w", err)
	}
	return wtDir, baseSHA, nil
}
