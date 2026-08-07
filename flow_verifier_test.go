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
	cfg.Projection = ProjectionConfig{}

	out, err := (verifierFlow{}).Act(cfg, repo, Subject{
		Key: "branch-forest/9-change", Kind: "branch", Revision: oldHead,
		Label: "forest/9-change", ID: "9", Branch: "forest/9-change", Head: oldHead,
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

	// Both the verifier and fixer selectors read the Tracker label, so stub the
	// tracker to return the item without the failure label.
	old := trackerFor
	trackerFor = func(repo string) Tracker {
		mem := newMemoryTracker()
		mem.seed(Item{ID: "9"})
		return mem
	}
	defer func() { trackerFor = old }()

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

// TestVerifierOffersStrandedVerdictHead pins the reviewer's alignment fix: a
// head carrying a Verdict but no green Checks is observed as pushed, so the
// Verifier must offer it -- the pushed -> check transition is the only way it
// can move -- while the Fixer must not (effectFix from pushed is refused).
// Before this fix the Verifier skipped every non-approve Verdict and skipped
// approve without Checks, so such a head was stuck.
func TestVerifierOffersStrandedVerdictHead(t *testing.T) {
	_, work, _ := notesTestRepository(t)
	branch := "forest/9-stranded"
	notesTestGit(t, work, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "commit", "-qam", "branch work")
	notesTestGit(t, work, "push", "-q", "-u", "origin", branch)
	head := notesTestGitOutput(t, work, "rev-parse", "HEAD")

	cfg := defaultConfig()
	cfg.Repo = "example/repo"

	old := trackerFor
	trackerFor = func(repo string) Tracker {
		mem := newMemoryTracker()
		mem.seed(Item{ID: "9"})
		return mem
	}
	defer func() { trackerFor = old }()

	// Seed a stranded "changes" Verdict with no Checks note on the head.
	if err := writeVerdict(work, head, verdictNote{
		Verdict: "changes", Reviewer: "verifier", Model: "verifier-model", DefSHA: "def", RunID: "seq",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readChecks(work, head); err != nil || ok {
		t.Fatalf("checks present before the test: (ok=%v err=%v), want a stranded bare head", ok, err)
	}

	subjects, err := verifierFlow{}.Select(cfg, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].Head != head {
		t.Fatalf("verifier subjects = %#v, want the stranded head %s", subjects, head)
	}
	repairs, err := fixerFlow{}.Select(cfg, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(repairs) != 0 {
		t.Fatalf("fixer offered a stranded pushed head: %#v", repairs)
	}
}

// TestVerifierMergeRequiresApproveAndPassingChecks is the falsifier for the
// merge admission gate: a branch reaches master only when its checks pass and
// its verdict approves, and auto_merge alone must never outweigh a rejection.
// It drives the full Act path for a rejected verdict, a failing check, and one
// successful merge. Flipping the operator at flow_verifier.go:186 to a logical
// AND would let a rejected branch merge, so this test must fail on that
// mutation: a test that survives the flip does not defend the property.
func TestVerifierMergeRequiresApproveAndPassingChecks(t *testing.T) {
	type result struct {
		out          Outcome
		repo         string
		branch       string
		masterBefore string
	}
	// runAct builds a one-branch repository, seeds an optional verdict on the
	// branch head, and drives the Verifier's Act to the finish. The tracker is
	// stubbed so the item read and the close after a merge never touch the host.
	runAct := func(t *testing.T, verdict string, checks []Check, autoMerge bool) result {
		t.Helper()
		repo := setupTestRepo(t)
		branch := "forest/9-change"
		rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
		rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
		rebaseTestGit(t, repo, "add", "branch.txt")
		rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
		rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
		head := remoteBranchHead(t, repo, branch)
		rebaseTestGit(t, repo, "checkout", "-q", "master")
		masterBefore := remoteBranchHead(t, repo, "master")

		writeAgentFixture(t, repo, "verifier", "verifier-model")

		oldGH := ghJSON
		ghJSON = func(args ...string) ([]byte, error) {
			return []byte(`{"number":9,"title":"change","body":"","updatedAt":"","comments":[],"labels":[]}`), nil
		}
		defer func() { ghJSON = oldGH }()

		cfg := defaultConfig()
		cfg.Repo = "owner/repo"
		cfg.Checks = checks
		cfg.Flows.Verifier.Agent = "verifier"
		cfg.Flows.Verifier.AutoMerge = autoMerge
		cfg.Projection = ProjectionConfig{}

		if verdict != "" {
			if err := writeVerdict(repo, head, verdictNote{
				Verdict: verdict, Reviewer: "verifier", Model: "verifier-model",
				DefSHA: "def", RunID: "seed",
			}); err != nil {
				t.Fatalf("seed verdict: %v", err)
			}
		}

		out, err := (verifierFlow{}).Act(cfg, repo, Subject{
			Key: "branch-" + branch, Kind: "branch", Revision: head,
			Label: branch, ID: "9", Branch: branch, Head: head,
		}, "run-1")
		if err != nil {
			t.Fatalf("Act: %v", err)
		}
		return result{out: out, repo: repo, branch: branch, masterBefore: masterBefore}
	}

	passing := []Check{{Name: "true", Run: "true"}}

	t.Run("rejected verdict never merges", func(t *testing.T) {
		r := runAct(t, "changes", passing, true)
		if r.out.Status == "merged" {
			t.Fatalf("a changes verdict merged: %#v", r.out)
		}
		if r.out.Status != "reviewed" {
			t.Fatalf("changes verdict status = %q, want reviewed", r.out.Status)
		}
		if got := remoteBranchHead(t, r.repo, "master"); got != r.masterBefore {
			t.Fatalf("master advanced to %s after a rejection, want %s", got, r.masterBefore)
		}
	})

	t.Run("failing check never merges", func(t *testing.T) {
		failing := []Check{{Name: "false", Run: "false"}}
		r := runAct(t, "approve", failing, true)
		if r.out.Status == "merged" {
			t.Fatalf("a failing check merged: %#v", r.out)
		}
		if r.out.Status != "checks_failed" {
			t.Fatalf("failing check status = %q, want checks_failed", r.out.Status)
		}
		if got := remoteBranchHead(t, r.repo, "master"); got != r.masterBefore {
			t.Fatalf("master advanced to %s despite a failed check, want %s", got, r.masterBefore)
		}
	})

	t.Run("approve with passing checks merges exactly once", func(t *testing.T) {
		r := runAct(t, "approve", passing, true)
		if r.out.Status != "merged" {
			t.Fatalf("approved, passing head status = %q, want merged", r.out.Status)
		}
		if got := remoteBranchHead(t, r.repo, "master"); got == r.masterBefore {
			t.Fatalf("master did not advance after a verified merge (%q)", got)
		}
		if out := rebaseTestGitOut(t, r.repo, "ls-remote", "origin", "refs/heads/"+r.branch); out != "" {
			t.Fatalf("merged branch %q still exists on origin: %s", r.branch, out)
		}
	})
}

// TestStalledOnPersistsOutsideLedger pins the durable progress brake: three
// failures on one revision stop a subject, a new revision resets it, and the
// decision remains after the host-local ledger is removed.
func TestStalledOnPersistsOutsideLedger(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	const subject = "branch-forest/9-x"
	const revision = "aaa"
	for range stalledRunLimit {
		if err := recordStalled(repo, "fixer", subject, revision); err != nil {
			t.Fatalf("record stalled: %v", err)
		}
	}
	if err := appendRun(workspaceDir(repo), runRecord{
		Flow: "fixer", Subject: subject, Revision: revision, Status: "gate_failed",
	}); err != nil {
		t.Fatalf("append ledger: %v", err)
	}
	if stalled, err := stalledOn(repo, "fixer", subject, revision); err != nil || !stalled {
		t.Fatalf("same revision stalled = %v, %v; want true", stalled, err)
	}
	if stalled, err := stalledOn(repo, "verifier", subject, revision); err != nil || stalled {
		t.Fatalf("other flow stalled = %v, %v; want false", stalled, err)
	}
	if stalled, err := stalledOn(repo, "fixer", subject, "bbb"); err != nil || stalled {
		t.Fatalf("changed revision stalled = %v, %v; want false", stalled, err)
	}
	if err := os.Remove(ledgerPath(repo)); err != nil {
		t.Fatalf("remove ledger: %v", err)
	}
	if stalled, err := stalledOn(repo, "fixer", subject, revision); err != nil || !stalled {
		t.Fatalf("stalled after ledger deletion = %v, %v; want true", stalled, err)
	}
}

// TestCommitAndPushCASLandsARewrittenBranch pins the capability the Fixer
// needs to resolve a conflict: a rebased branch must be able to land, and only
// against the commit the run actually observed.
func TestCommitAndPushCASLandsARewrittenBranch(t *testing.T) {
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
	it := Item{ID: "9", Title: "rewrite"}
	// Each attempt needs its own change: a failed push leaves its commit behind,
	// and a run that has nothing to commit is a different failure.
	if err := os.WriteFile(filepath.Join(work, "stale.txt"), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAndPush(work, work, branch, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", id, it); err == nil {
		t.Fatal("a stale observed ref must lose the push")
	}
	if err := os.WriteFile(filepath.Join(work, "fix.txt"), []byte("repair\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAndPush(work, work, branch, observed, id, it); err != nil {
		t.Fatalf("rebased branch with the observed ref = %v, want nil", err)
	}
	remote := notesTestGitOutput(t, work, "rev-parse", "refs/remotes/origin/"+branch)
	local := notesTestGitOutput(t, work, "rev-parse", "HEAD")
	if remote != local {
		t.Fatalf("remote %s != local %s after a compare-and-swap push", remote, local)
	}
}

// TestSelectOffersNoBranchItCannotMerge defines the hot loop out of existence.
// A branch that is approved and green but cannot land is not a Verifier subject:
// it is a state an operator reads. When Select offered it anyway, every pass
// rebased, rechecked and reviewed the same head and changed nothing, and the
// lane re-selected immediately because the pass had "succeeded". That produced
// 217 identical ledger rows and 217 build/vet/test runs on one branch.
func TestSelectOffersNoBranchItCannotMerge(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-change"
	rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	head := remoteBranchHead(t, repo, branch)
	rebaseTestGit(t, repo, "checkout", "-q", "master")

	if err := writeVerdict(repo, head, verdictNote{
		Verdict: "approve", Reviewer: "verifier", Model: "m", DefSHA: "def", RunID: "seed",
	}); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}
	if err := writeChecks(repo, head, checksNote{Status: "pass", RunID: "seed", Time: nowRFC()}); err != nil {
		t.Fatalf("seed checks: %v", err)
	}

	// The verifier selector reads the Tracker label, so stub the tracker to
	// return the item without the failure label.
	old := trackerFor
	trackerFor = func(repo string) Tracker {
		mem := newMemoryTracker()
		mem.seed(Item{ID: "9"})
		return mem
	}
	defer func() { trackerFor = old }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{}

	cfg.Flows.Verifier.AutoMerge = true
	withMerge, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatalf("Select with auto_merge on: %v", err)
	}
	if len(withMerge) != 1 {
		t.Fatalf("auto_merge on: got %d subjects, want the mergeable branch", len(withMerge))
	}

	cfg.Flows.Verifier.AutoMerge = false
	held, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatalf("Select with auto_merge off: %v", err)
	}
	if len(held) != 0 {
		t.Fatalf("auto_merge off: got %d subjects, want none; a branch that cannot land is not work", len(held))
	}
}

// TestVerifierNeverActsOnFailedLabel is the flow-level falsifier the reviewer
// asked for: an approved, green branch whose tracker item carries forest:failed
// is terminal and never resumed, so the Verifier must neither offer it nor act
// on it. Before this guard, Select derived every decision from git facts alone
// and never read the label, so such a branch could still pass mergeBlocked and
// admitMerge and land. Select must drop it, and Act must refuse it at the
// boundary with no merge and no effect on master.
func TestVerifierNeverActsOnFailedLabel(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-failed"
	rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	head := remoteBranchHead(t, repo, branch)
	rebaseTestGit(t, repo, "checkout", "-q", "master")
	masterBefore := remoteBranchHead(t, repo, "master")

	// An approved Verdict and green Checks on the exact head: everything the
	// merge path normally requires, about as actionable as a failed subject gets.
	if err := writeVerdict(repo, head, verdictNote{
		Verdict: "approve", Reviewer: "verifier", Model: "m", DefSHA: "def", RunID: "seed",
	}); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}
	if err := writeChecks(repo, head, checksNote{Status: "pass", RunID: "seed", Time: nowRFC()}); err != nil {
		t.Fatalf("seed checks: %v", err)
	}

	// The item carries the durable failure label, so the Selector and Act guard
	// must both refuse to treat this branch as work.
	old := trackerFor
	trackerFor = func(repo string) Tracker {
		mem := newMemoryTracker()
		mem.seed(Item{ID: "9", Tags: []string{failedLabel}})
		return mem
	}
	defer func() { trackerFor = old }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "true", Run: "true"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = true
	cfg.Projection = ProjectionConfig{}

	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(subjects) != 0 {
		t.Fatalf("Select offered a failed-labeled approved green branch: %#v", subjects)
	}

	// Drive Act anyway, as if the label arrived between Select and Act, and
	// confirm it refuses before any effect: no merge, no master advance.
	writeAgentFixture(t, repo, "verifier", "verifier-model")
	out, err := (verifierFlow{}).Act(cfg, repo, Subject{
		Key: "branch-" + branch, Kind: "branch", Revision: head,
		Label: branch, ID: "9", Branch: branch, Head: head,
	}, "run-1")
	if err == nil {
		t.Fatalf("Act accepted a failed-labeled approved green branch: %#v", out)
	}
	if out.Status != "item_failed" {
		t.Fatalf("Act status = %q, want item_failed", out.Status)
	}
	if got := remoteBranchHead(t, repo, "master"); got != masterBefore {
		t.Fatalf("master advanced to %s despite a failed-labeled subject, want %s", got, masterBefore)
	}
}

// TestVerifierSelectSkipsExhaustedBareHead pins the reviewer's Note 2 on the
// read side: the durable attempt cap is a cap fact for every branch, not only
// the approved merge path. If the Fixer bumps the attempt ref and then crashes
// or fails before applying forest:failed, the cap is spent but no label exists.
// Select must feed the spent cap through observe so such a bare head is reported
// failed and never offered for a fresh check.
func TestVerifierSelectSkipsExhaustedBareHead(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-exhausted"
	rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	rebaseTestGit(t, repo, "checkout", "-q", "master")

	// Spend the only attempt: a Fixer that bumped the ref and halted would leave
	// exactly this fact -- cap spent, no forest:failed label applied.
	if count, err := bumpAttempts(repo, "branch-"+branch); err != nil || count != 1 {
		t.Fatalf("bumpAttempts = (%d, %v), want (1, nil)", count, err)
	}

	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) {
		return []byte(`{"number":9,"title":"change","body":"","updatedAt":"","comments":[],"labels":[]}`), nil
	}
	defer func() { ghJSON = oldGH }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Flows.Fixer.Attempts = 1

	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	for _, s := range subjects {
		if s.Key == "branch-"+branch {
			t.Fatalf("Select offered an exhausted bare head %q as fresh check work", s.Key)
		}
	}
}

// TestVerifierActRefusesExhaustedBeforeCheckEffect pins the reviewer's Note 2
// on the write side: even when Act is driven directly (as if Select and Act
// raced), an exhausted bare head must be refused before any durable Checks note
// is written. Before Note 2's cap facts reached the boundary, the Verifier would
// check and record an exhausted subject, contradicting the claimed terminal
// state that a post-bump crash is observed as failed.
func TestVerifierActRefusesExhaustedBeforeCheckEffect(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-exhaustact"
	rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	head := remoteBranchHead(t, repo, branch)
	rebaseTestGit(t, repo, "checkout", "-q", "master")

	if count, err := bumpAttempts(repo, "branch-"+branch); err != nil || count != 1 {
		t.Fatalf("bumpAttempts = (%d, %v), want (1, nil)", count, err)
	}

	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) {
		return []byte(`{"number":9,"title":"change","body":"","updatedAt":"","comments":[],"labels":[]}`), nil
	}
	defer func() { ghJSON = oldGH }()

	writeAgentFixture(t, repo, "verifier", "verifier-model")

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "true", Run: "true"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Fixer.Attempts = 1
	cfg.Projection = ProjectionConfig{}

	out, err := (verifierFlow{}).Act(cfg, repo, Subject{
		Key: "branch-" + branch, Kind: "branch", Revision: head,
		Label: branch, ID: "9", Branch: branch, Head: head,
	}, "run-1")
	if err == nil {
		t.Fatalf("Act checked an exhausted bare head: %#v", out)
	}
	if out.Status != "item_failed" {
		t.Fatalf("Act status = %q, want item_failed", out.Status)
	}
	if _, ok, err := readChecks(repo, head); err != nil || ok {
		t.Fatalf("Act wrote a Checks note on an exhausted head: (found=%v, err=%v)", ok, err)
	}
}

// Select and Act both consult it; a precondition that lives in only one of them
// is how the two drifted and produced the hot loop above.
func TestMergeBlockedNamesEveryReason(t *testing.T) {
	cfg := defaultConfig()
	cfg.Flows.Verifier.AutoMerge = true
	cfg.Flows.Fixer.Attempts = 3

	if why := mergeBlocked(cfg, 0); why != "" {
		t.Errorf("an approved green branch under auto_merge is blocked: %q", why)
	}
	if why := mergeBlocked(cfg, 3); why == "" {
		t.Error("an exhausted branch is not blocked")
	}
	cfg.Flows.Verifier.AutoMerge = false
	if why := mergeBlocked(cfg, 0); why == "" {
		t.Error("auto_merge off does not block the merge")
	}
}

// TestAdmitMergeReadsTheExactNotes is the flow-level illegal-transition
// coverage the reviewer asked for: it drives the verifier's actual merge
// admission, which reads the Checks and Verdict notes on the exact head and
// asks the machine. A head missing either required note -- a flow that skipped
// a required note or Gate -- is refused here, and so is a failing or rejected
// head. A pure `transit` table cannot provide this; it would still pass if the
// flow never called the machine.
func TestAdmitMergeReadsTheExactNotes(t *testing.T) {
	type seed struct {
		checks  string // "" | pass | fail
		verdict string // "" | approve | changes
		wantErr bool
		name    string
	}
	run := func(t *testing.T, sc seed) {
		t.Helper()
		_, work, _ := notesTestRepository(t)
		branch := "forest/9-admit"
		notesTestGit(t, work, "checkout", "-q", "-b", branch)
		if err := os.WriteFile(filepath.Join(work, "branch.txt"), []byte("branch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		notesTestGit(t, work, "add", "branch.txt")
		notesTestGit(t, work, "commit", "-qm", "branch work")
		notesTestGit(t, work, "push", "-q", "-u", "origin", branch)
		head := notesTestGitOutput(t, work, "rev-parse", "HEAD")
		if sc.checks != "" {
			if err := writeChecks(work, head, checksNote{Status: sc.checks, RunID: "seed", Time: nowRFC()}); err != nil {
				t.Fatal(err)
			}
		}
		if sc.verdict != "" {
			if err := writeVerdict(work, head, verdictNote{Verdict: sc.verdict, Reviewer: "v", Model: "m", DefSHA: "d", RunID: "seed"}); err != nil {
				t.Fatal(err)
			}
		}
		err := admitMerge(work, head)
		if sc.wantErr && err == nil {
			t.Fatalf("admitMerge admitted a forbidden merge (checks=%q verdict=%q)", sc.checks, sc.verdict)
		}
		if !sc.wantErr && err != nil {
			t.Fatalf("admitMerge refused a legal merge: %v", err)
		}
	}

	for _, sc := range []seed{
		{name: "approved with green checks is admitted", checks: "pass", verdict: "approve"},
		{name: "skipped checks note is refused", checks: "", verdict: "approve", wantErr: true},
		{name: "skipped verdict note is refused", checks: "pass", verdict: "", wantErr: true},
		{name: "rejected verdict is refused", checks: "pass", verdict: "changes", wantErr: true},
		{name: "failing checks are refused", checks: "fail", verdict: "approve", wantErr: true},
	} {
		t.Run(sc.name, func(t *testing.T) { run(t, sc) })
	}
}

// TestAdmitCheckReadsTheExactNote is the flow-level illegal-transition coverage
// for the check effect: the verifier's check admission reads the Checks note on
// the exact head and asks the machine. A bare head is admitted; a head that
// already carries a Checks note is refused, so the effectCheck transition is
// actually called and enforced rather than skipped by the pass.
func TestAdmitCheckReadsTheExactNote(t *testing.T) {
	run := func(t *testing.T, seedChecks bool, wantErr bool) {
		t.Helper()
		_, work, _ := notesTestRepository(t)
		branch := "forest/9-admitcheck"
		notesTestGit(t, work, "checkout", "-q", "-b", branch)
		if err := os.WriteFile(filepath.Join(work, "branch.txt"), []byte("branch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		notesTestGit(t, work, "add", "branch.txt")
		notesTestGit(t, work, "commit", "-qm", "branch work")
		notesTestGit(t, work, "push", "-q", "-u", "origin", branch)
		head := notesTestGitOutput(t, work, "rev-parse", "HEAD")
		if seedChecks {
			if err := writeChecks(work, head, checksNote{Status: "pass", RunID: "seed", Time: nowRFC()}); err != nil {
				t.Fatal(err)
			}
		}
		err := admitCheck(work, head)
		if wantErr && err == nil {
			t.Fatal("admitCheck checked a head that already carries a checks note")
		}
		if !wantErr && err != nil {
			t.Fatalf("admitCheck refused a bare head: %v", err)
		}
	}

	for _, tc := range []struct {
		name       string
		seedChecks bool
		wantErr    bool
	}{
		{name: "bare head is checked", seedChecks: false},
		{name: "a head already carrying a checks note is refused", seedChecks: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) { run(t, tc.seedChecks, tc.wantErr) })
	}
}

// TestActReusesGreenChecksOnReviewRetry pins the stall the reviewer named: a
// head Select offers as fresh work -- green Checks, no Verdict, as left after a
// review failed before recording a Verdict -- must not be rejected at the check
// boundary. admitCheck refuses a head that already carries a Checks note, so Act
// must read that note first and reuse it verbatim (no re-check, no check
// transition) and proceed to review. The review is stubbed through reviewRunner
// so the whole Act path runs deterministically without a model.
func TestActReusesGreenChecksOnReviewRetry(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-reviewretry"
	rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	head := remoteBranchHead(t, repo, branch)
	rebaseTestGit(t, repo, "checkout", "-q", "master")

	// Seed only the green Checks note: a retry after a review that never wrote a
	// Verdict. The branch is current, so Act's rebase is a no-op and this exact
	// head (and its note) is the one Act must reuse.
	if err := writeChecks(repo, head, checksNote{Status: "pass", RunID: "seed", Time: nowRFC()}); err != nil {
		t.Fatalf("seed checks: %v", err)
	}

	writeAgentFixture(t, repo, "verifier", "verifier-model")

	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) {
		return []byte(`{"number":9,"title":"change","body":"","updatedAt":"","comments":[],"labels":[]}`), nil
	}
	defer func() { ghJSON = oldGH }()

	reviewed := false
	oldReview := reviewRunner
	reviewRunner = func(cfg Config, repoDir, wtDir string, it Item, s Subject, runID string, a *Agent) (verdictNote, runStats, error) {
		reviewed = true
		return verdictNote{Verdict: "changes", Reviewer: "verifier", Model: a.Model, DefSHA: "def", RunID: runID}, runStats{}, nil
	}
	defer func() { reviewRunner = oldReview }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "true", Run: "true"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = true
	cfg.Projection = ProjectionConfig{}

	out, err := (verifierFlow{}).Act(cfg, repo, Subject{
		Key: "branch-" + branch, Kind: "branch", Revision: head,
		Label: branch, ID: "9", Branch: branch, Head: head,
	}, "run-1")
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if !reviewed {
		t.Fatal("Act never reached review; it was stopped before review instead of reusing the green Checks note")
	}
	if out.Status != "reviewed" {
		t.Fatalf("out.Status = %q, want reviewed after reusing a green Checks note and reviewing changes", out.Status)
	}
	// The persisted note must be reused, not re-run: a re-check would overwrite
	// it with a fresh RunID.
	if got, ok, err := readChecks(repo, head); err != nil || !ok || got.RunID != "seed" {
		t.Fatalf("checks after Act = (found=%v, run=%q, err=%v), want seeded run %q", ok, got.RunID, err, "seed")
	}
}

// TestActReusesGreenChecksOnMergePass pins the other stall the reviewer named:
// a later pass returning to merge an already-approved branch offers a head that
// already carries green Checks and an approved Verdict. Act must reuse both
// notes (no re-check) and proceed straight to the merge, not fail at the check
// boundary because admitCheck refuses a head with an existing Checks note.
func TestActReusesGreenChecksOnMergePass(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-mergepass"
	rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	head := remoteBranchHead(t, repo, branch)
	rebaseTestGit(t, repo, "checkout", "-q", "master")
	masterBefore := remoteBranchHead(t, repo, "master")

	if err := writeChecks(repo, head, checksNote{Status: "pass", RunID: "seed", Time: nowRFC()}); err != nil {
		t.Fatalf("seed checks: %v", err)
	}
	if err := writeVerdict(repo, head, verdictNote{
		Verdict: "approve", Reviewer: "verifier", Model: "verifier-model", DefSHA: "def", RunID: "seed",
	}); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}

	writeAgentFixture(t, repo, "verifier", "verifier-model")

	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) {
		return []byte(`{"number":9,"title":"change","body":"","updatedAt":"","comments":[],"labels":[]}`), nil
	}
	defer func() { ghJSON = oldGH }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "true", Run: "true"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = true
	cfg.Projection = ProjectionConfig{}

	out, err := (verifierFlow{}).Act(cfg, repo, Subject{
		Key: "branch-" + branch, Kind: "branch", Revision: head,
		Label: branch, ID: "9", Branch: branch, Head: head,
	}, "run-1")
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if out.Status != "merged" {
		t.Fatalf("approved, green head status = %q, want merged", out.Status)
	}
	if got := remoteBranchHead(t, repo, "master"); got == masterBefore {
		t.Fatalf("master did not advance after the merge pass (%q)", got)
	}
	if out := rebaseTestGitOut(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out != "" {
		t.Fatalf("merged branch %q still exists on origin: %s", branch, out)
	}
	// The persisted Checks note was reused, not re-run and overwritten.
	if got, ok, err := readChecks(repo, head); err != nil || !ok || got.RunID != "seed" {
		t.Fatalf("checks after merge = (found=%v, run=%q, err=%v), want seeded run %q", ok, got.RunID, err, "seed")
	}
}

// closes the window between admitMerge and the merge itself: if the branch head
// moved after admission, its Checks and Verdict may no longer describe the head
// about to land, so landing it would violate the exact-revision invariant.
func TestMergeVerifiedRefusesMovedBranch(t *testing.T) {
	_, work, _ := notesTestRepository(t)
	branch := "forest/9-moved"
	notesTestGit(t, work, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, "branch.txt"), []byte("branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "add", "branch.txt")
	notesTestGit(t, work, "commit", "-qm", "branch work")
	notesTestGit(t, work, "push", "-q", "-u", "origin", branch)
	admitted := notesTestGitOutput(t, work, "rev-parse", "HEAD")

	// The branch advances after it was admitted; the merge now lands a different
	// head and must refuse on the expected-head check.
	if err := os.WriteFile(filepath.Join(work, "branch.txt"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "commit", "-qam", "branch moved")
	notesTestGit(t, work, "push", "-q", "origin", branch)

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{}
	err := mergeVerified(cfg, work, branch, Item{ID: "9", Title: "moved"}, admitted)
	if err == nil {
		t.Fatal("mergeVerified landed a branch that moved after admission")
	}
	if !strings.Contains(err.Error(), "after admission") {
		t.Errorf("error %q does not name the expected-head refusal", err)
	}
}

// TestFinishMergeRefusesToRetireAMovedBranch pins the compare-and-swap on the
// branch retirement: finishMerge deletes the branch only when it still points at
// the exact admitted head. A branch that landed elsewhere -- one a human or a
// concurrent flow moved after the merge -- must not be silently deleted, so the
// retirement is --force-with-lease against the admitted SHA rather than a plain
// delete.
func TestFinishMergeRefusesToRetireAMovedBranch(t *testing.T) {
	_, work, _ := notesTestRepository(t)
	branch := "forest/9-retire"
	notesTestGit(t, work, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, "branch.txt"), []byte("branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "add", "branch.txt")
	notesTestGit(t, work, "commit", "-qm", "branch work")
	notesTestGit(t, work, "push", "-q", "-u", "origin", branch)
	admitted := notesTestGitOutput(t, work, "rev-parse", "HEAD")

	// The branch advances after it was admitted, so the compare-and-swap delete
	// must refuse and leave the moved branch on origin untouched.
	if err := os.WriteFile(filepath.Join(work, "branch.txt"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "commit", "-qam", "branch moved")
	notesTestGit(t, work, "push", "-q", "origin", branch)
	moved := notesTestGitOutput(t, work, "rev-parse", "HEAD")

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	err := finishMerge(cfg, work, branch, Item{ID: "9", Title: "retire"}, admitted)
	if err == nil {
		t.Fatal("finishMerge deleted a branch that moved after admission")
	}
	if got := remoteBranchHead(t, work, branch); got != moved {
		t.Fatalf("branch head after refused retirement = %q, want the moved head %q", got, moved)
	}
}

// TestVerifierStopsAfterFailedLabelArrivesMidPass is the falsifier for the
// terminal-fact race the reviewer named: failed is checked at Act entry, but a
// Fixer (or a human) can apply forest:failed while a Verifier is already
// checking or reviewing. If the merge boundary did not re-read the label, an
// approved green review could land after the item was marked failed. This test
// applies the label from inside the review -- exactly when a concurrent flow
// would -- and requires the Verifier to refuse the merge with master unchanged.
func TestVerifierStopsAfterFailedLabelArrivesMidPass(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-midfail"
	rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	head := remoteBranchHead(t, repo, branch)
	rebaseTestGit(t, repo, "checkout", "-q", "master")
	masterBefore := remoteBranchHead(t, repo, "master")

	writeAgentFixture(t, repo, "verifier", "verifier-model")

	// The tracker starts without the label; the review then applies it, as a
	// Fixer exhausting its label would while this Verifier is in flight.
	mem := newMemoryTracker()
	mem.seed(Item{ID: "9"})
	oldTracker := trackerFor
	trackerFor = func(repo string) Tracker { return mem }
	defer func() { trackerFor = oldTracker }()

	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) {
		return []byte(`{"number":9,"title":"change","body":"","updatedAt":"","state":"open","comments":[],"labels":[]}`), nil
	}
	defer func() { ghJSON = oldGH }()

	oldReview := reviewRunner
	reviewRunner = func(cfg Config, repoDir, wtDir string, it Item, s Subject, runID string, a *Agent) (verdictNote, runStats, error) {
		if err := mem.SetTags("9", []string{failedLabel}, nil); err != nil {
			return verdictNote{}, runStats{}, err
		}
		return verdictNote{Verdict: "approve", Reviewer: "verifier", Model: a.Model, DefSHA: "def", RunID: runID}, runStats{}, nil
	}
	defer func() { reviewRunner = oldReview }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "true", Run: "true"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = true
	cfg.Projection = ProjectionConfig{}

	out, err := (verifierFlow{}).Act(cfg, repo, Subject{
		Key: "branch-" + branch, Kind: "branch", Revision: head,
		Label: branch, ID: "9", Branch: branch, Head: head,
	}, "run-1")
	if err == nil {
		t.Fatalf("Act merged despite forest:failed applied mid-pass: %#v", out)
	}
	if out.Status != "item_failed" {
		t.Fatalf("Act status = %q, want item_failed", out.Status)
	}
	if got := remoteBranchHead(t, repo, "master"); got != masterBefore {
		t.Fatalf("master advanced to %s despite a mid-pass failure label, want %s", got, masterBefore)
	}
}
