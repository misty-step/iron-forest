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
// non-conflicting commit is rebased onto origin/master and its new head pushed
// with force, and that Act writes the checks note at the post-rebase head and
// records that same head in its outcome.
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

	// A verifier agent directory is required for Act to build a run.
	writeAgentFixture(t, repo, "verifier", "verifier-model")

	// Learn the post-rebase head in a worktree so a verdict can be seeded there
	// and Act stops after review; Act then rebases again, a no-op because the
	// branch is already current.
	wtDir, oldHead, err := createWorktreeAtBranch(repo, workspace, "forest/9-change")
	if err != nil {
		t.Fatal(err)
	}
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
	removeWorktree(repo, wtDir)
	if remoteHead := remoteBranchHead(t, repo, "forest/9-change"); remoteHead != newHead {
		t.Fatalf("origin branch = %q, want pushed post-rebase head %q", remoteHead, newHead)
	}

	// A verdict already recorded at the post-rebase head lets Act pass the
	// review phase without a model call; the checks note Act writes is the
	// signal under test.
	if err := writeVerdict(repo, newHead, verdictNote{
		Verdict: "approve", Reviewer: "verifier", Model: "verifier-model",
		DefSHA: "def", RunID: "seed",
	}); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}
	if _, ok, err := readChecks(repo, newHead); err != nil || ok {
		t.Fatalf("checks already present on post-rebase head before Act: (found=%v, err=%v)", ok, err)
	}

	// Stub the tracker so Act's item read does not touch the host CLI.
	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) {
		return []byte(`{"number":9,"title":"change","body":"","updatedAt":"","comments":[],"labels":[]}`), nil
	}
	defer func() { ghJSON = oldGH }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "true", Run: "true"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = false
	cfg.Projection = Projection{}

	out, err := (verifierFlow{}).Act(cfg, repo, Subject{
		Key: "branch-forest/9-change", Kind: "branch", Revision: oldHead,
		Label: "forest/9-change", Issue: 9, Branch: "forest/9-change", Head: oldHead,
	}, "run-1")
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if out.BaseSHA != newHead {
		t.Fatalf("out.BaseSHA = %q, want post-rebase head %q", out.BaseSHA, newHead)
	}
	if got, ok, err := readChecks(repo, newHead); err != nil || !ok || got.Status != "pass" {
		t.Fatalf("checks on post-rebase head = (found=%v, status=%q, err=%v), want pass", ok, got.Status, err)
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

// TestVerifierSkipsHeadOwnedByTheFixer pins the spin this factory already paid
// for: a head whose checks failed carries a fact, and re-offering it re-runs
// every check and re-reviews the same commit forever. The lane that can repair
// it must be the one that selects it, and a new head must clear the fact.
func TestVerifierSkipsHeadOwnedByTheFixer(t *testing.T) {
	_, work, _ := notesTestRepository(t)
	branch := "forest/9-conflicted"
	notesTestGit(t, work, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "commit", "-qam", "branch work")
	notesTestGit(t, work, "push", "-q", "-u", "origin", branch)
	head := notesTestGitOutput(t, work, "rev-parse", "HEAD")

	cfg := defaultConfig()
	cfg.Repo = "example/repo"

	// With no notes at all the head is fresh work for the Verifier.
	subjects, err := verifierFlow{}.Select(cfg, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].Head != head {
		t.Fatalf("fresh head = %#v, want one subject at %s", subjects, head)
	}

	// A failing check is the fact a rebase conflict or a broken build leaves.
	fail := checksNote{
		Status:  "fail",
		RunID:   "run-1",
		Time:    nowRFC(),
		Results: []checkResult{{Name: "rebase", Code: 1, Output: "conflicts in file.txt"}},
	}
	if err := writeChecks(work, head, fail); err != nil {
		t.Fatal(err)
	}
	subjects, err = verifierFlow{}.Select(cfg, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 0 {
		t.Fatalf("verifier still offers a failed head: %#v", subjects)
	}
	repairs, err := fixerFlow{}.Select(cfg, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(repairs) != 1 || repairs[0].Head != head {
		t.Fatalf("fixer subjects = %#v, want the failed head %s", repairs, head)
	}

	// A repair moves the branch, and notes key to the commit, so the new head is
	// fresh work again without deleting anything.
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("repaired\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "commit", "-qam", "repair")
	notesTestGit(t, work, "push", "-q", "origin", branch)
	newHead := notesTestGitOutput(t, work, "rev-parse", "HEAD")
	subjects, err = verifierFlow{}.Select(cfg, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].Head != newHead {
		t.Fatalf("repaired head = %#v, want one subject at %s", subjects, newHead)
	}
}

// TestStalledOnCountsFailuresPerRevision pins the progress rule: a lane stops
// retrying one unchanged situation, and a real repair clears the count because
// it moves the revision.
func TestStalledOnCountsFailuresPerRevision(t *testing.T) {
	rows := []runRecord{
		{Flow: "fixer", Subject: "branch-forest/9-x", Revision: "aaa", Status: "publish_failed"},
		{Flow: "fixer", Subject: "branch-forest/9-x", Revision: "aaa", Status: "agent_failed"},
		{Flow: "verifier", Subject: "branch-forest/9-x", Revision: "aaa", Status: "checks_failed"},
		{Flow: "fixer", Subject: "branch-forest/9-x", Revision: "aaa", Status: "fixed"},
	}
	if stalledOn(rows, "fixer", "branch-forest/9-x", "aaa") {
		t.Fatal("two failures and one success must not stall a lane")
	}
	rows = append(rows, runRecord{Flow: "fixer", Subject: "branch-forest/9-x", Revision: "aaa", Status: "gate_failed"})
	if !stalledOn(rows, "fixer", "branch-forest/9-x", "aaa") {
		t.Fatal("three failures on one revision must stall the lane")
	}
	if stalledOn(rows, "fixer", "branch-forest/9-x", "bbb") {
		t.Fatal("a new revision must start the count over")
	}
	if stalledOn(rows, "verifier", "branch-forest/9-x", "aaa") {
		t.Fatal("one lane's failures must not stall another lane")
	}
}

// TestCommitAndPushLeaseLandsARewrittenBranch pins the capability the Fixer
// needs to resolve a conflict: a rebased branch must be able to land, and only
// against the commit the run actually observed.
func TestCommitAndPushLeaseLandsARewrittenBranch(t *testing.T) {
	_, work, _ := notesTestRepository(t)
	branch := "forest/9-rewrite"
	notesTestGit(t, work, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, "branch.txt"), []byte("branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "add", "branch.txt")
	notesTestGit(t, work, "commit", "-qm", "branch work")
	notesTestGit(t, work, "push", "-q", "-u", "origin", branch)
	observed := notesTestGitOutput(t, work, "rev-parse", "HEAD")

	// Master moves, and the branch is rebased onto it, so the branch's history is
	// rewritten and a plain push would be rejected.
	notesTestGit(t, work, "checkout", "-q", "master")
	if err := os.WriteFile(filepath.Join(work, "master.txt"), []byte("master\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "add", "master.txt")
	notesTestGit(t, work, "commit", "-qm", "master work")
	notesTestGit(t, work, "push", "-q", "origin", "master")
	notesTestGit(t, work, "checkout", "-q", branch)
	notesTestGit(t, work, "rebase", "-q", "master")

	id := CommitIdentity{Name: "forest-test", Email: "forest-test@example.com"}
	it := issue{Number: 9, Title: "rewrite"}
	// Each attempt needs its own change: a failed push leaves its commit behind,
	// and a run that has nothing to commit is a different failure.
	if err := os.WriteFile(filepath.Join(work, "stale.txt"), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAndPush(work, work, branch, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", id, it); err == nil {
		t.Fatal("a stale lease must lose the push")
	}
	if err := os.WriteFile(filepath.Join(work, "fix.txt"), []byte("repair\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAndPush(work, work, branch, observed, id, it); err != nil {
		t.Fatalf("rebased branch with the observed lease = %v, want nil", err)
	}
	remote := notesTestGitOutput(t, work, "rev-parse", "refs/remotes/origin/"+branch)
	local := notesTestGitOutput(t, work, "rev-parse", "HEAD")
	if remote != local {
		t.Fatalf("remote %s != local %s after a leased push", remote, local)
	}
}
