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

// preservedWorktrees records worktree directories a cancelled run left in place
// for inspection. Unlike the live registry entry — which is removed the moment
// the run ends — this is durable, so a later createWorktree pass over the (still
// unchanged) subject and a daemon restart's reapOrphanWorktrees both leave the
// inspection copy alone (see #163).
var preservedWorktrees = struct {
	mu   sync.Mutex
	dirs map[string]struct{}
}{dirs: make(map[string]struct{})}

// preserveWorktree marks a worktree directory as left for inspection by a
// cancelled run. Nothing removes a preserved worktree automatically: not the
// flow's own cleanup, not a later createWorktree, not a restart's
// reapOrphanWorktrees. The operator removes it by hand, at which point
// removeWorktree or prunePreservedWorktrees forgets the marker.
func preserveWorktree(dir string) {
	if dir == "" {
		return
	}
	preservedWorktrees.mu.Lock()
	preservedWorktrees.dirs[dir] = struct{}{}
	preservedWorktrees.mu.Unlock()
}

// unpreserveWorktree forgets a preserved marker, typically when the worktree is
// removed (so a subject whose inspection copy is gone becomes actionable again).
func unpreserveWorktree(dir string) {
	if dir == "" {
		return
	}
	preservedWorktrees.mu.Lock()
	delete(preservedWorktrees.dirs, dir)
	preservedWorktrees.mu.Unlock()
}

// isPreservedWorktree reports whether a worktree directory was left by a
// cancelled run and must not be removed or replaced automatically.
func isPreservedWorktree(dir string) bool {
	preservedWorktrees.mu.Lock()
	defer preservedWorktrees.mu.Unlock()
	_, ok := preservedWorktrees.dirs[dir]
	return ok
}

// hasPreservedWorktrees reports whether any worktree is currently preserved.
func hasPreservedWorktrees() bool {
	preservedWorktrees.mu.Lock()
	defer preservedWorktrees.mu.Unlock()
	return len(preservedWorktrees.dirs) > 0
}

// prunePreservedWorktrees forgets preserved markers whose worktree no longer
// exists on disk. It runs at startup so an operator who manually removed a
// cancelled worktree with git (git worktree list no longer shows it) unblocks
// the subject for a later pass. A marker is never stale-kept: the same directory
// can then be re-created, and a preserved copy that vanished is no copy at all.
func prunePreservedWorktrees() {
	preservedWorktrees.mu.Lock()
	for dir := range preservedWorktrees.dirs {
		if _, err := os.Stat(dir); err != nil && os.IsNotExist(err) {
			delete(preservedWorktrees.dirs, dir)
		}
	}
	preservedWorktrees.mu.Unlock()
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
// they always have; the id segment is opaque and escaped (encodeBranchID), so a
// non-numeric tracker id — even one containing the '-' delimiter — derives an
// equally valid, reverse-lookup-able branch.
func createWorktree(repo, workspace, id, title string) (wtDir, branch, baseSHA string, err error) {
	branch = fmt.Sprintf("%s%s-%s", BranchPrefix, encodeBranchID(id), slug(title))
	wtDir = filepath.Join(workspace, "worktrees", branch)
	// A worktree a cancelled run left for inspection must not be destroyed by a
	// later pass over the still-unchanged subject. Reuse it instead of removing
	// and recreating it, so the operator's copy survives re-selection (see #163).
	if isPreservedWorktree(wtDir) {
		baseSHA, err = gitOut(repo, "rev-parse", "origin/master")
		if err != nil {
			return "", "", "", fmt.Errorf("origin/master: %w", err)
		}
		return wtDir, branch, baseSHA, nil
	}
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
	unpreserveWorktree(wtDir)
}

// reapOrphanWorktrees removes linked worktrees left by an interrupted process.
// A worktree a cancelled run preserved for inspection is never reaped, so the
// operator's copy survives a daemon restart (see #163).
func reapOrphanWorktrees(repoDir string) {
	prunePreservedWorktrees()
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
		if isPreservedWorktree(abs) {
			fmt.Fprintf(os.Stderr, "forest: keeping preserved worktree %s\n", wtDir)
			continue
		}
		fmt.Fprintf(os.Stderr, "forest: removing stale worktree %s\n", wtDir)
		removeWorktree(repoDir, wtDir)
	}
	if hasPreservedWorktrees() {
		// A cancelled run's worktree is left for inspection; never remove the
		// whole worktrees directory while one survives (see #163).
		return
	}
	if err := os.RemoveAll(wtRoot); err != nil {
		fmt.Fprintf(os.Stderr, "forest: reap worktrees: %v\n", err)
	}
}

// createWorktreeAtBranch adds a linked worktree at an existing branch tip.
func createWorktreeAtBranch(repo, workspace, branch string) (wtDir, baseSHA string, err error) {
	wtDir = filepath.Join(workspace, "worktrees", branch)
	// Reuse a preserved inspection worktree rather than remove and recreate it
	// (see #163); see createWorktree.
	if isPreservedWorktree(wtDir) {
		baseSHA, err = gitOut(repo, "rev-parse", "origin/"+branch)
		if err != nil {
			return "", "", fmt.Errorf("branch %s not on origin: %w", branch, err)
		}
		return wtDir, baseSHA, nil
	}
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
