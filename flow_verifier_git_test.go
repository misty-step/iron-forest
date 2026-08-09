package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRebaseOntoMasterRebasesBehindBranch proves a branch behind master by a
// non-conflicting commit is rebased onto origin/master and its new head pushed
// with force, and that Act writes the checks note at the post-rebase head and
// records that same head in its outcome.
func TestRebaseOntoMasterRebasesBehindBranch(t *testing.T) {
	repo := setupTestRepo(t)
	workspace := filepath.Join(repo, WorkspaceDir)

	// Build a branch from the current master, then advance master.
	runGitTest(t, repo, "checkout", "-q", "-b", "forest/9-change")
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/forest/9-change")
	runGitTest(t, repo, "checkout", "-q", "master")
	rebaseTestWriteFile(t, filepath.Join(repo, "master.txt"), "master\n")
	runGitTest(t, repo, "add", "master.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "master work")
	runGitTest(t, repo, "push", "-q", "origin", "master")

	// A verifier agent directory is required for Act to build a run.
	writeAgentFixture(t, repo, "verifier", "verifier-model")

	// Learn the post-rebase head in a worktree so a verdict can be seeded there
	// and Act stops after review; Act then rebases again, a no-op because the
	// branch is already current.
	wtDir, oldHead, err := createWorktreeAtBranch(repo, workspace, "forest/9-change")
	if err != nil {
		t.Fatal(err)
	}
	id := testVerifierAgent().Commit
	originalAuthor := runGitTest(t, wtDir, "show", "-s", "--format=%an <%ae>", oldHead)
	t.Setenv("GIT_COMMITTER_NAME", "ambient committer")
	t.Setenv("GIT_COMMITTER_EMAIL", "ambient@example.invalid")
	newHead, err := rebaseOntoMaster(wtDir, "forest/9-change", oldHead, id)
	if err != nil {
		t.Fatal(err)
	}
	if newHead == oldHead {
		t.Fatalf("rebase produced no new head: both %q", newHead)
	}
	identity := runGitTest(t, wtDir, "show", "-s", "--format=%an <%ae>|%cn <%ce>", newHead)
	wantIdentity := originalAuthor + "|" + id.Name + " <" + id.Email + ">"
	if identity != wantIdentity {
		t.Fatalf("rebased commit identity = %q, want %q", identity, wantIdentity)
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
		DefSHA: strings.Repeat("a", 16), RunID: "seed",
	}); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}
	if _, ok, err := readChecks(repo, newHead); err != nil || ok {
		t.Fatalf("checks already present on post-rebase head before Act: (found=%v, err=%v)", ok, err)
	}

	// Stub the tracker so Act's item read does not touch the host CLI.
	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) {
		return []byte(`{"number":9,"title":"change","body":"","updatedAt":"u1","comments":[],"labels":[]}`), nil
	}
	defer func() { ghJSON = oldGH }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "true", Run: "true"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = false
	cfg.Projection = ProjectionConfig{}

	out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-forest/9-change", Kind: "branch", Revision: newHead,
		Label: "forest/9-change", ID: "9", Branch: "forest/9-change"}, "run-1")
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
	runGitTest(t, repo, "checkout", "-q", "-b", "forest/9-conflict")
	rebaseTestWriteFile(t, filepath.Join(repo, "file.txt"), "branch\n")
	runGitTest(t, repo, "add", "file.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch edit")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/forest/9-conflict")
	runGitTest(t, repo, "checkout", "-q", "master")
	rebaseTestWriteFile(t, filepath.Join(repo, "file.txt"), "master\n")
	runGitTest(t, repo, "add", "file.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "master edit")
	runGitTest(t, repo, "push", "-q", "origin", "master")

	wtDir, oldHead, err := createWorktreeAtBranch(repo, workspace, "forest/9-conflict")
	if err != nil {
		t.Fatal(err)
	}
	defer removeWorktree(repo, wtDir)

	_, err = rebaseOntoMaster(wtDir, "forest/9-conflict", oldHead, testVerifierAgent().Commit)
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

	runGitTest(t, repo, "checkout", "-q", "-b", "forest/9-current")
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/forest/9-current")
	runGitTest(t, repo, "checkout", "-q", "master")

	wtDir, oldHead, err := createWorktreeAtBranch(repo, workspace, "forest/9-current")
	if err != nil {
		t.Fatal(err)
	}
	defer removeWorktree(repo, wtDir)

	newHead, err := rebaseOntoMaster(wtDir, "forest/9-current", oldHead, testVerifierAgent().Commit)
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

func TestRebaseOntoMasterFencesReviewedRemoteHead(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-lease"
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "reviewed\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "reviewed branch")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	rebaseTestWriteFile(t, filepath.Join(repo, "master.txt"), "master\n")
	runGitTest(t, repo, "add", "master.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "advance master")
	runGitTest(t, repo, "push", "-q", "origin", "master")

	wtDir, reviewed, err := createWorktreeAtBranch(repo, filepath.Join(repo, WorkspaceDir), branch)
	if err != nil {
		t.Fatal(err)
	}
	defer removeWorktree(repo, wtDir)

	attacker := filepath.Join(t.TempDir(), "attacker")
	origin := runGitTest(t, repo, "remote", "get-url", "origin")
	runGitTest(t, "", "clone", "-q", origin, attacker)
	runGitTest(t, attacker, "checkout", "-q", branch)
	rebaseTestWriteFile(t, filepath.Join(attacker, "advanced.txt"), "advanced\n")
	runGitTest(t, attacker, "add", "advanced.txt")
	runGitTest(t, attacker, "commit", "-q", "-m", "advance branch")
	runGitTest(t, attacker, "push", "-q", "origin", branch)
	advanced := remoteBranchHead(t, repo, branch)
	runGitTest(t, repo, "fetch", "-q", "origin", branch)

	if _, err := rebaseOntoMaster(wtDir, branch, reviewed, testVerifierAgent().Commit); err == nil {
		t.Fatal("stale rebase push returned success")
	}
	if got := remoteBranchHead(t, repo, branch); got != advanced {
		t.Fatalf("stale rebase overwrote remote branch: got %s, want %s", got, advanced)
	}
}

// TestCommitAndPushCASLandsARewrittenBranch pins the capability the Fixer
// needs to resolve a conflict: a rebased branch must be able to land, and only
// against the commit the run actually observed.
func TestCommitAndPushCASLandsARewrittenBranch(t *testing.T) {
	_, work, _ := notesTestRepository(t)
	branch := "forest/9-rewrite"
	runGitTest(t, work, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, "branch.txt"), []byte("branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, work, "add", "branch.txt")
	runGitTest(t, work, "commit", "-qm", "branch work")
	runGitTest(t, work, "push", "-q", "-u", "origin", branch)
	observed := runGitTest(t, work, "rev-parse", "HEAD")

	// Master moves, and the branch is rebased onto it, so the branch's history is
	// rewritten and a plain push would be rejected.
	runGitTest(t, work, "checkout", "-q", "master")
	if err := os.WriteFile(filepath.Join(work, "master.txt"), []byte("master\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, work, "add", "master.txt")
	runGitTest(t, work, "commit", "-qm", "master work")
	runGitTest(t, work, "push", "-q", "origin", "master")
	runGitTest(t, work, "checkout", "-q", branch)
	runGitTest(t, work, "rebase", "-q", "master")

	id := CommitIdentity{Name: "forest-test", Email: "forest-test@example.com"}
	it := Item{ID: "9", Title: "rewrite"}
	// Each attempt needs its own change: a failed push leaves its commit behind,
	// and a run that has nothing to commit is a different failure.
	if err := os.WriteFile(filepath.Join(work, "stale.txt"), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := commitAndPush(work, work, branch, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", id, it); err == nil {
		t.Fatal("a stale observed ref must lose the push")
	}
	if err := os.WriteFile(filepath.Join(work, "fix.txt"), []byte("repair\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := commitAndPush(work, work, branch, observed, id, it); err != nil {
		t.Fatalf("rebased branch with the observed ref = %v, want nil", err)
	}
	remote := runGitTest(t, work, "rev-parse", "refs/remotes/origin/"+branch)
	local := runGitTest(t, work, "rev-parse", "HEAD")
	if remote != local {
		t.Fatalf("remote %s != local %s after a compare-and-swap push", remote, local)
	}
}

func TestBranchFlowsRejectRevisionThatMovedAfterSelect(t *testing.T) {
	t.Run("Verifier", func(t *testing.T) {
		branch := "forest/36-stale-verifier"
		repo, _, selected, _ := newVerifierBranch(t, branch)
		tk := newMemoryTracker()
		tk.seed(Item{ID: "36", Title: "stale verifier", UpdatedAt: "u1"})
		oldTracker := trackerFor
		trackerFor = func(string) Tracker { return tk }
		defer func() { trackerFor = oldTracker }()

		rebaseTestWriteFile(t, filepath.Join(repo, "after-select.txt"), "new Revision\n")
		runGitTest(t, repo, "add", "after-select.txt")
		runGitTest(t, repo, "commit", "-q", "-m", "advance after Select")
		runGitTest(t, repo, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
		current := remoteBranchHead(t, repo, branch)
		runGitTest(t, repo, "checkout", "-q", "master")

		oldRun := runPhase
		runPhase = func(string, string, *Agent, string, string) (runStats, error) {
			t.Fatal("Verifier spent a model run on a stale Subject")
			return runStats{}, nil
		}
		defer func() { runPhase = oldRun }()

		cfg := defaultConfig()
		cfg.Repo = "owner/repo"
		cfg.Projection = ProjectionConfig{}
		out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: subjectBranch, Revision: selected,
			Label: branch, ID: "36", Branch: branch}, "stale-verifier")
		if !errors.Is(err, errSubjectRevisionStale) || out.Status != "stale" {
			t.Fatalf("Verifier stale Act = (status=%q, err=%v), want stale Revision refusal", out.Status, err)
		}
		if out.BaseSHA != current {
			t.Fatalf("Verifier stale Act observed %s, want current Revision %s", out.BaseSHA, current)
		}
	})

	t.Run("Fixer", func(t *testing.T) {
		branch := "forest/37-stale-fixer"
		repo, _, selected, _ := newVerifierBranch(t, branch)
		if err := writeChecks(repo, selected, checksNote{
			Status: "fail", RunID: "selected",
			Results: []checkResult{{Name: "test", Code: 1, Output: "failed"}},
		}); err != nil {
			t.Fatal(err)
		}
		writeAgentFixture(t, repo, "fixer", "fixer-model")
		tk := newMemoryTracker()
		tk.seed(Item{ID: "37", Title: "stale fixer", UpdatedAt: "u1"})
		oldTracker := trackerFor
		trackerFor = func(string) Tracker { return tk }
		defer func() { trackerFor = oldTracker }()

		rebaseTestWriteFile(t, filepath.Join(repo, "after-select.txt"), "new Revision\n")
		runGitTest(t, repo, "add", "after-select.txt")
		runGitTest(t, repo, "commit", "-q", "-m", "advance after Select")
		runGitTest(t, repo, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
		current := remoteBranchHead(t, repo, branch)
		runGitTest(t, repo, "checkout", "-q", "master")

		oldRun := runPhase
		runPhase = func(string, string, *Agent, string, string) (runStats, error) {
			t.Fatal("Fixer spent a model run on a stale Subject")
			return runStats{}, nil
		}
		defer func() { runPhase = oldRun }()

		cfg := defaultConfig()
		cfg.Repo = "owner/repo"
		cfg.Flows.Fixer.Agent = "fixer"
		out, err := (fixerFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: subjectBranch, Revision: selected,
			Label: branch, ID: "37", Branch: branch}, "stale-fixer")
		if !errors.Is(err, errSubjectRevisionStale) || out.Status != "stale" {
			t.Fatalf("Fixer stale Act = (status=%q, err=%v), want stale Revision refusal", out.Status, err)
		}
		if out.BaseSHA != current {
			t.Fatalf("Fixer stale Act observed %s, want current Revision %s", out.BaseSHA, current)
		}
		if attempts, err := readAttempts(repo, "branch-"+branch); err != nil || attempts != 0 {
			t.Fatalf("Fixer stale Act attempts = (%d, %v), want zero", attempts, err)
		}
	})
}
