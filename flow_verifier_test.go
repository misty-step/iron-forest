package main

import (
	"errors"
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

// TestVerifierPreflightFailureWritesNoPassNote proves the durable-fact outcome
// of item #187: with the check environment forced to fail, no declared check
// runs, no pass note is written on the Revision, the failure is classified as
// a mechanical one for an operator, and Select does not offer the head as
// mergeable. This is the falsifier for the old bug that initialised the note to
// "pass" before resolving the environment and then wrote that note anyway.
func TestVerifierPreflightFailureWritesNoPassNote(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-change"
	rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	head := remoteBranchHead(t, repo, branch)
	rebaseTestGit(t, repo, "checkout", "-q", "master")

	writeAgentFixture(t, repo, "verifier", "verifier-model")

	// An approve verdict makes the head a merge candidate that only a
	// trustworthy checks note may admit; a preflight failure must never pass it.
	if err := writeVerdict(repo, head, verdictNote{
		Verdict: "approve", Reviewer: "verifier", Model: "verifier-model",
		DefSHA: "def", RunID: "seed",
	}); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}

	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) {
		return []byte(`{"number":9,"title":"change","body":"","updatedAt":"","comments":[],"labels":[]}`), nil
	}
	defer func() { ghJSON = oldGH }()

	oldEnv := checkEnvironment
	checkEnvironment = func() ([]string, func(), error) {
		return nil, func() {}, errors.New("locate mise: missing")
	}
	defer func() { checkEnvironment = oldEnv }()

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
		t.Fatalf("preflight failure returned no error: %#v", out)
	}
	if out.Status != "checks_environment_failed" {
		t.Fatalf("preflight failure status = %q, want checks_environment_failed", out.Status)
	}

	if _, ok, err := readChecks(repo, head); err != nil || ok {
		t.Fatalf("checks note on head = (found=%v, err=%v), want no note when checks never ran", ok, err)
	}

	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range subjects {
		if s.Branch == branch {
			t.Fatalf("verifier offered the preflight-failed head as mergeable: %#v", s)
		}
	}
	repairs, err := (fixerFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range repairs {
		if s.Branch == branch {
			t.Fatalf("fixer took a preflight-failed head as if its code were wrong: %#v", s)
		}
	}
}

// TestVerifierPreflightRetryIgnoresExistingNote proves item #187's second pass:
// once a (stale) checks note exists on a Revision, a retry over the same head
// with a failing preflight must not clear checkErr because a note exists. The
// old code read the existing note as the winner and set checkErr to nil, which
// let an approved head review and merge without a single executed check. A
// preflight failure is classified early, before any note is read or written.
func TestVerifierPreflightRetryIgnoresExistingNote(t *testing.T) {
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

	if err := writeVerdict(repo, head, verdictNote{
		Verdict: "approve", Reviewer: "verifier", Model: "verifier-model",
		DefSHA: "def", RunID: "seed",
	}); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}
	// A stale pass note from an earlier, buggy pass already keys this Revision.
	if err := writeChecks(repo, head, checksNote{Status: "pass", RunID: "stale", Time: nowRFC()}); err != nil {
		t.Fatalf("seed stale checks: %v", err)
	}

	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) {
		return []byte(`{"number":9,"title":"change","body":"","updatedAt":"","comments":[],"labels":[]}`), nil
	}
	defer func() { ghJSON = oldGH }()

	oldEnv := checkEnvironment
	checkEnvironment = func() ([]string, func(), error) {
		return nil, func() {}, errors.New("locate mise: missing")
	}
	defer func() { checkEnvironment = oldEnv }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "true", Run: "true"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = true
	cfg.Projection = ProjectionConfig{}

	out, err := (verifierFlow{}).Act(cfg, repo, Subject{
		Key: "branch-" + branch, Kind: "branch", Revision: head,
		Label: branch, ID: "9", Branch: branch, Head: head,
	}, "run-2")
	if err == nil {
		t.Fatalf("retry over an existing note returned no error: %#v", out)
	}
	if out.Status != "checks_environment_failed" {
		t.Fatalf("retry over an existing note status = %q, want checks_environment_failed (note must not clear checkErr)", out.Status)
	}
	if out.Status == "merged" {
		t.Fatalf("retry merged a Revision whose checks never ran once: %#v", out)
	}
	if got := remoteBranchHead(t, repo, "master"); got != masterBefore {
		t.Fatalf("master advanced to %s on a retry whose checks never ran, want %s", got, masterBefore)
	}
}

// TestFenceMergeOnReviewedRevision pins item #188: a merge may land only the
// Revision that carried the approving Verdict. An unchanged remote branch passes
// the fence; a branch that advanced after the Verdict is refused with both
// Revisions named, and mergeVerified leaves the branch with its newer commits
// intact rather than deleting it.
func TestFenceMergeOnReviewedRevision(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-change"
	rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	reviewed := remoteBranchHead(t, repo, branch)

	// An unchanged remote branch is exactly the reviewed Revision: it passes.
	if err := fenceMergeOnRevision(repo, branch, reviewed); err != nil {
		t.Fatalf("unchanged branch refused by the fence: %v", err)
	}

	// The operator pushes newer, unreviewed work after the Verdict was written.
	rebaseTestGit(t, repo, "checkout", "-q", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "later.txt"), "later\n")
	rebaseTestGit(t, repo, "add", "later.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "newer unreviewed work")
	rebaseTestGit(t, repo, "push", "-q", "origin", branch)
	observed := remoteBranchHead(t, repo, branch)
	if observed == reviewed {
		t.Fatalf("branch did not advance, cannot probe the fence")
	}

	if err := fenceMergeOnRevision(repo, branch, reviewed); err == nil {
		t.Fatalf("advanced branch passed the fence")
	} else if !strings.Contains(err.Error(), reviewed[:8]) || !strings.Contains(err.Error(), observed[:8]) {
		t.Fatalf("refusal %q does not name both the reviewed (%s) and observed (%s) Revisions", err, reviewed[:8], observed[:8])
	}

	// mergeVerified must refuse without deleting the branch, so the newer,
	// unreviewed commits survive for the next pass to review.
	if err := mergeVerified(defaultConfig(), repo, branch, reviewed, Item{ID: "9", Title: "change"}); err == nil {
		t.Fatal("mergeVerified merged a branch that advanced past its reviewed Revision")
	}
	if out := rebaseTestGitOut(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out == "" {
		t.Fatal("mergeVerified deleted the branch despite refusing the merge")
	}
	if got := remoteBranchHead(t, repo, branch); got != observed {
		t.Fatalf("branch tip = %s, want the newer commits %s intact", got, observed)
	}
}

// TestMergeGitPathPinsReviewedRevision pins the acting step that closes the gap
// between fenceMergeOnRevision's read and the merge push: the git path updates
// master and deletes the source branch in a single atomic compare-and-set push,
// so a branch that advances after the fence was read is neither merged nor
// deleted — master is untouched and the newer, unreviewed commits survive, while
// an unchanged branch lands and is retired as before.
func TestMergeGitPathPinsReviewedRevision(t *testing.T) {
	newRepo := func(t *testing.T) (repo, branch, reviewed, masterBefore string) {
		t.Helper()
		repo = setupTestRepo(t)
		branch = "forest/9-change"
		rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
		rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
		rebaseTestGit(t, repo, "add", "branch.txt")
		rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
		rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
		reviewed = remoteBranchHead(t, repo, branch)
		masterBefore = remoteBranchHead(t, repo, "master")
		return
	}
	// retireItem closes the item through the tracker, which talks to the host;
	// stub it so the merge path is exercised without a host CLI.
	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) { return []byte(`{}`), nil }
	defer func() { ghJSON = oldGH }()
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{}
	cfg.Flows.Verifier.Merge = "squash"
	cfg.Commit = CommitIdentity{Name: "forest-test", Email: "forest-test@example.com"}
	it := Item{ID: "9", Title: "change"}

	t.Run("unchanged branch lands and retires", func(t *testing.T) {
		repo, branch, reviewed, masterBefore := newRepo(t)
		if err := mergeGitPath(cfg, repo, branch, reviewed, it); err != nil {
			t.Fatalf("mergeGitPath on an unchanged branch = %v, want nil", err)
		}
		if got := remoteBranchHead(t, repo, "master"); got == masterBefore {
			t.Fatal("master did not advance on an unchanged branch")
		}
		if out := rebaseTestGitOut(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out != "" {
			t.Fatalf("source branch %q still on origin after landing: %s", branch, out)
		}
	})

	t.Run("advanced branch is refused and survives", func(t *testing.T) {
		repo, branch, reviewed, masterBefore := newRepo(t)
		// The operator pushes newer, unreviewed work after the Verdict and after
		// the fence was read, simulating the review-to-merge window.
		rebaseTestGit(t, repo, "checkout", "-q", branch)
		rebaseTestWriteFile(t, filepath.Join(repo, "later.txt"), "later\n")
		rebaseTestGit(t, repo, "add", "later.txt")
		rebaseTestGit(t, repo, "commit", "-q", "-m", "newer unreviewed work")
		rebaseTestGit(t, repo, "push", "-q", "origin", branch)
		observed := remoteBranchHead(t, repo, branch)
		if observed == reviewed {
			t.Fatal("branch did not advance, cannot probe the race")
		}

		if err := mergeGitPath(cfg, repo, branch, reviewed, it); err == nil {
			t.Fatal("mergeGitPath merged a branch that advanced past its reviewed Revision")
		}
		if got := remoteBranchHead(t, repo, "master"); got != masterBefore {
			t.Fatalf("master advanced to %s after the refused merge, want %s", got, masterBefore)
		}
		if got := remoteBranchHead(t, repo, branch); got != observed {
			t.Fatalf("branch tip = %s, want the newer commits %s intact", got, observed)
		}
	})
}

// TestMergeViaHostPinsReviewedHead pins the host path of item #188: a host merge
// receives the expected-head facility pinned to the reviewed Revision, and when
// the host refuses (head mismatch), the branch is not deleted so the newer,
// unreviewed commits survive for re-review.
func TestMergeViaHostPinsReviewedHead(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-change"
	rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	reviewed := remoteBranchHead(t, repo, branch)

	oldProj := projectionCommand
	defer func() { projectionCommand = oldProj }()
	var mergeArgs []string
	projectionCommand = func(args ...string) ([]byte, error) {
		switch args[1] {
		case "list":
			return []byte(`[{"number":9,"url":"https://github.com/owner/repo/pull/9"}]`), nil
		case "merge":
			mergeArgs = append([]string(nil), args...)
			return nil, errors.New("host refused: head does not match reviewed Revision")
		default:
			return nil, nil
		}
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Merge = "squash"

	if err := mergeVerified(cfg, repo, branch, reviewed, Item{ID: "9", Title: "change"}); err == nil {
		t.Fatal("mergeVerified merged via host despite the host refusing the head")
	}
	pinned := false
	for i := 0; i+1 < len(mergeArgs); i++ {
		if mergeArgs[i] == "--match-head-commit" && mergeArgs[i+1] == reviewed {
			pinned = true
		}
	}
	if !pinned {
		t.Fatalf("host merge args %v do not pin the reviewed Revision %s with --match-head-commit", mergeArgs, reviewed)
	}
	if out := rebaseTestGitOut(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out == "" {
		t.Fatal("mergeVerified deleted the branch even though the host merge refused")
	}
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

// TestMergeBlockedNamesEveryReason pins the single authority for merge policy.
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
