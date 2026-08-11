package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
	wtDir, _, _, err := createWorktree(repo, workspace, "44", "leak")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("worktree %s was not created: %v", wtDir, err)
	}
	// Leak it: deliberately skip removeWorktree, the way an os.Exit would.
	untrackWorktree(wtDir)
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

func TestReapOrphanWorktreesPrunesMissingDirectoryRegistration(t *testing.T) {
	repo := setupTestRepo(t)
	workspace := filepath.Join(repo, WorkspaceDir)
	wtDir, _, _, err := createWorktree(repo, workspace, "45", "missing")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(wtDir); err != nil {
		t.Fatal(err)
	}
	// Simulate a restart: process-local ownership is gone, but Git still has
	// the linked-worktree registration.
	untrackWorktree(wtDir)
	registered := false
	for _, wt := range currentWorktrees(t, repo) {
		registered = registered || filepath.Clean(wt) == filepath.Clean(wtDir)
	}
	if !registered {
		t.Fatal("fixture lost the stale Git worktree registration")
	}
	reapOrphanWorktrees(repo)
	for _, wt := range currentWorktrees(t, repo) {
		if filepath.Clean(wt) == filepath.Clean(wtDir) {
			t.Fatalf("missing worktree %s remains registered after reap", wtDir)
		}
	}
}

func TestRemoveWorktreeUntracksAfterGitFailure(t *testing.T) {
	wtDir := filepath.Join(t.TempDir(), "worktree")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := trackWorktree(wtDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { untrackWorktree(wtDir) })

	if err := removeWorktree(filepath.Join(t.TempDir(), "missing-repo"), wtDir); err == nil {
		t.Fatal("Git removal failure returned nil")
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("worktree directory still exists: %v", err)
	}
	if err := trackWorktree(wtDir); err != nil {
		t.Fatalf("removed worktree retained its ownership lock: %v", err)
	}
	untrackWorktree(wtDir)
}

// TestCreateWorktreeStartsAtTheRemoteTip pins the isolation invariant: a run
// begins at the exact sha the record reports, so the run is reproducible even
// when the remote branch moves during the pass.
func TestCreateWorktreeStartsAtTheRemoteTip(t *testing.T) {
	repo := setupTestRepo(t)
	workspace := filepath.Join(repo, WorkspaceDir)
	wtDir, branch, baseSHA, err := createWorktree(repo, workspace, "5", "tip")
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
func TestCreateWorktreeRedactsTitleBeforeBranchSlug(t *testing.T) {
	const secret = "sk-AAAAAAAAAAAAAAAA"
	repo := setupTestRepo(t)
	wtDir, branch, _, err := createWorktree(repo, filepath.Join(repo, WorkspaceDir), "6", "change "+secret)
	if err != nil {
		t.Fatal(err)
	}
	defer removeWorktree(repo, wtDir)
	if strings.Contains(branch, secret) {
		t.Fatalf("branch %q retained the title secret", branch)
	}
	if branch != "forest/6-change-redacted" {
		t.Fatalf("branch = %q, want redacted slug forest/6-change-redacted", branch)
	}
}

// TestItemIdentityRoundTripNonNumeric proves branch derivation and the reverse
// lookup tolerate an opaque, non-numeric tracker id: the branch keeps the
// forest/<id>-<slug> shape for a Habitat-style id and itemIDFromBranch returns
// that id unchanged, without assuming the segment is an integer.
func TestItemIdentityRoundTripNonNumeric(t *testing.T) {
	const id = "hab_01J9X"
	branch := "forest/" + id + "-parking"

	if got := itemIDFromBranch(branch); got != id {
		t.Fatalf("itemIDFromBranch(%q) = %q, want %q", branch, got, id)
	}
	// A numeric GitHub id keeps its readable, unchanged shape too.
	if got := itemIDFromBranch("forest/69-notes"); got != "69" {
		t.Fatalf("itemIDFromBranch(numeric) = %q, want 69", got)
	}

	repo := setupTestRepo(t)
	workspace := filepath.Join(repo, WorkspaceDir)
	wtDir, derived, _, err := createWorktree(repo, workspace, id, "parking")
	if err != nil {
		t.Fatalf("createWorktree with opaque id: %v", err)
	}
	defer removeWorktree(repo, wtDir)
	if derived != branch {
		t.Fatalf("branch = %q, want %q", derived, branch)
	}
}

// TestEncodeBranchIDBijective pins that encodeBranchID and itemIDFromBranch are
// genuinely inverse for every opaque id, including one whose characters spell
// out an escape sequence. An id containing the literal `%2D` encodes to `%252D`;
// the decoder must turn that back into `%2D`, not into a stray `-`, so the
// round-trip survives ids that collide with the escaping vocabulary.
func TestEncodeBranchIDBijective(t *testing.T) {
	cases := []string{
		"69",            // numeric GitHub id, unchanged
		"hab_01J9X",     // hyphen-free Habitat id, unchanged
		"a-b",           // a '-' must be escaped to survive parse
		"100%",          // a '%' must be escaped
		"%2D",           // literal escape sequence as a real id
		"%252D",         // a double-escaped id, keeps its percent
		"x%2D-y%25z",    // mixed '-' and '%' escapes
		"hab/01J9X",     // a '/' is a path separator and git-invalid, must escape
		"a b",           // a space is git-invalid
		"~^:?*[\\",      // git's special refname characters
		"../evil",       // '..' segments and leading dots must not survive literally
		"@",             // a lone '@' is not a valid refname tail
		"id\x07ctl\x7f", // control bytes are git-invalid
	}
	for _, id := range cases {
		enc := encodeBranchID(id)
		if strings.ContainsAny(enc, "/ ~^:?*[\\\x07\x7f") {
			t.Errorf("encodeBranchID(%q) = %q still contains a git/path-invalid byte", id, enc)
		}
		if got := itemIDFromBranch("forest/" + enc + "-slug"); got != id {
			t.Errorf("round-trip(%q): encode = %q, decode = %q, want %q", id, enc, got, id)
		}
	}
}

func TestWorktreeOwnerProcessHelper(t *testing.T) {
	repo := os.Getenv("FOREST_WORKTREE_OWNER_REPO")
	if repo == "" {
		return
	}
	wtDir, _, _, err := createWorktree(repo, workspaceDir(repo), "901", "live-owner")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(wtDir)
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := removeWorktree(repo, wtDir); err != nil {
		t.Fatal(err)
	}
}

// TestReaperPreservesWorktreeOwnedByAnotherProcess pins startup cleanup to
// process-independent ownership. A new daemon must not reap a live manual Run.
func TestReaperPreservesWorktreeOwnedByAnotherProcess(t *testing.T) {
	repo := setupTestRepo(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorktreeOwnerProcessHelper$")
	cmd.Env = append(os.Environ(), "FOREST_WORKTREE_OWNER_REPO="+repo)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	done, markWaited := startTestProcess(t, cmd)
	t.Cleanup(func() { _ = stdin.Close() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read owner worktree: %v: %s", err, stderr.String())
	}
	wtDir := strings.TrimSpace(line)
	reapOrphanWorktrees(repo)
	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("reaper removed live foreign-process worktree %s: %v", wtDir, err)
	}
	list, err := gitOut(repo, "worktree", "list", "--porcelain")
	if err != nil || !strings.Contains(list, "worktree "+wtDir) {
		t.Fatalf("live worktree registration = (%q, %v), want %s", list, err, wtDir)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("owner helper: %v: %s", err, stderr.String())
	}
	markWaited()
}

func TestGitCommitIgnoresAmbientIdentity(t *testing.T) {
	repo := setupTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "identity.txt"), []byte("declared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "identity.txt")
	t.Setenv("GIT_AUTHOR_NAME", "ambient author")
	t.Setenv("GIT_AUTHOR_EMAIL", "ambient-author@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "ambient committer")
	t.Setenv("GIT_COMMITTER_EMAIL", "ambient-committer@example.com")
	id := CommitIdentity{Name: "declared agent", Email: "declared@example.invalid"}
	if err := gitCommit(repo, id, "declared identity"); err != nil {
		t.Fatal(err)
	}
	got := runGitTest(t, repo, "show", "-s", "--format=%an <%ae>|%cn <%ce>", "HEAD")
	want := "declared agent <declared@example.invalid>|declared agent <declared@example.invalid>"
	if got != want {
		t.Fatalf("commit identities = %q, want %q", got, want)
	}
}

func TestHostGitWrappersDisableConfiguredHooks(t *testing.T) {
	repo := setupTestRepo(t)
	hookDir := filepath.Join(repo, "configured-hooks")
	marker := filepath.Join(repo, "hook-marker")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"post-checkout", "pre-commit"} {
		hook := filepath.Join(hookDir, name)
		if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf ran > hook-marker\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runGitTest(t, repo, "config", "core.hooksPath", "configured-hooks")
	if err := git(repo, "checkout", "-q", "-b", "forest/hooks-disabled"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "hook-input"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := git(repo, "add", "hook-input"); err != nil {
		t.Fatal(err)
	}
	if err := gitCommit(repo, testCommitIdentity(), "hook isolation"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository hook marker = %v, want no marker", err)
	}
}
func TestGitCommitRedactsMessage(t *testing.T) {
	const secret = "sk-AAAAAAAAAAAAAAAA"
	repo := setupTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "secret-message.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "secret-message.txt")
	id := CommitIdentity{Name: "declared agent", Email: "declared@example.invalid"}
	if err := gitCommit(repo, id, "publish "+secret); err != nil {
		t.Fatal(err)
	}
	got := runGitTest(t, repo, "show", "-s", "--format=%s", "HEAD")
	if strings.Contains(got, secret) || !strings.Contains(got, secretRedacted) {
		t.Fatalf("commit message = %q, want marker without original", got)
	}
}
