package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rebaseTestWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testVerifierAgent() *Agent {
	return &Agent{
		Name: "verifier", Model: "verifier-model", DefSHA: strings.Repeat("a", 16),
		Commit: CommitIdentity{Name: "forest-test", Email: "forest-test@example.com"},
	}
}

func newVerifierBranch(t *testing.T, branch string) (repo, namedBranch, reviewed, masterBefore string) {
	t.Helper()
	repo = setupTestRepo(t)
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), branch+"\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	return repo, branch, remoteBranchHead(t, repo, branch), remoteBranchHead(t, repo, "master")
}

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
	newHead, err := rebaseOntoMaster(wtDir, "forest/9-change", id)
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

	wtDir, _, err := createWorktreeAtBranch(repo, workspace, "forest/9-conflict")
	if err != nil {
		t.Fatal(err)
	}
	defer removeWorktree(repo, wtDir)

	_, err = rebaseOntoMaster(wtDir, "forest/9-conflict", testVerifierAgent().Commit)
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

	newHead, err := rebaseOntoMaster(wtDir, "forest/9-current", testVerifierAgent().Commit)
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
	out := runGitTest(t, repo, "ls-remote", "origin", "refs/heads/"+branch)
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
	remote, work, _ := notesTestRepository(t)
	branch := "forest/9-conflicted"
	runGitTest(t, work, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, work, "commit", "-qam", "branch work")
	runGitTest(t, work, "push", "-q", "-u", "origin", branch)
	head := runGitTest(t, work, "rev-parse", "HEAD")
	noteSource := filepath.Join(t.TempDir(), "note-source")
	runGitTest(t, "", "clone", remote, noteSource)
	runGitTest(t, noteSource, "config", "user.name", "note-source")
	runGitTest(t, noteSource, "config", "user.email", "note-source@example.com")
	fixerWork := filepath.Join(t.TempDir(), "fixer-work")
	runGitTest(t, "", "clone", remote, fixerWork)

	cfg := defaultConfig()
	cfg.Repo = "example/repo"

	// With no notes at all the head is fresh work for the Verifier.
	subjects, err := verifierFlow{}.Select(cfg, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].Head != head || subjects[0].ID != "9" ||
		canonicalAdmissionKey(subjects[0]) != "item-9" {
		t.Fatalf("fresh head = %#v, want item-9 at %s", subjects, head)
	}

	// A failing check is the fact a rebase conflict or a broken build leaves.
	fail := checksNote{
		Status:  "fail",
		RunID:   "run-1",
		Time:    nowRFC(),
		Results: []checkResult{{Name: "rebase", Code: 1, Output: "conflicts in file.txt"}},
	}
	if err := writeChecks(noteSource, head, fail); err != nil {
		t.Fatal(err)
	}
	subjects, err = verifierFlow{}.Select(cfg, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 0 {
		t.Fatalf("verifier still offers a failed head: %#v", subjects)
	}
	repairs, err := fixerFlow{}.Select(cfg, fixerWork)
	if err != nil {
		t.Fatal(err)
	}
	if len(repairs) != 1 || repairs[0].Head != head || repairs[0].ID != "9" ||
		canonicalAdmissionKey(repairs[0]) != "item-9" {
		t.Fatalf("fixer subjects = %#v, want item-9 at %s", repairs, head)
	}
	for range stalledRunLimit {
		if err := recordStalled(fixerWork, "fixer", repairs[0].Key, head); err != nil {
			t.Fatal(err)
		}
	}
	if repairs, err = (fixerFlow{}).Select(cfg, fixerWork); err != nil || len(repairs) != 0 {
		t.Fatalf("fixer selected stalled head = (%#v, %v), want none", repairs, err)
	}

	// A repair moves the branch, and notes key to the commit, so the new head is
	// fresh work again without deleting anything.
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("repaired\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, work, "commit", "-qam", "repair")
	runGitTest(t, work, "push", "-q", "origin", branch)
	newHead := runGitTest(t, work, "rev-parse", "HEAD")
	subjects, err = verifierFlow{}.Select(cfg, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].Head != newHead || subjects[0].ID != "9" ||
		canonicalAdmissionKey(subjects[0]) != "item-9" {
		t.Fatalf("repaired head = %#v, want item-9 at %s", subjects, newHead)
	}
	if err := writeVerdict(work, newHead, verdictNote{Verdict: "approve", Reviewer: "verifier", Model: "m", DefSHA: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := writeChecks(work, newHead, checksNote{Status: "pass"}); err != nil {
		t.Fatal(err)
	}
	for range stalledRunLimit {
		if err := recordStalled(work, "verifier", subjects[0].Key, newHead); err != nil {
			t.Fatal(err)
		}
	}
	if subjects, err = (verifierFlow{}).Select(cfg, work); err != nil || len(subjects) != 0 {
		t.Fatalf("verifier selected stalled approved head = (%#v, %v), want none", subjects, err)
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
		runGitTest(t, repo, "checkout", "-q", "-b", branch)
		rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
		runGitTest(t, repo, "add", "branch.txt")
		runGitTest(t, repo, "commit", "-q", "-m", "branch work")
		runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
		head := remoteBranchHead(t, repo, branch)
		runGitTest(t, repo, "checkout", "-q", "master")
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
		if out := runGitTest(t, r.repo, "ls-remote", "origin", "refs/heads/"+r.branch); out != "" {
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
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	head := remoteBranchHead(t, repo, branch)
	runGitTest(t, repo, "checkout", "-q", "master")

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
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	head := remoteBranchHead(t, repo, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
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

// TestVerifierRefusesPassNoteWhenCheckMutatesTree is the end-to-end regression
// for item #191's check-phase guard. A declared check that rewrites or stages a
// tracked file taints the very tree the check was meant to judge; even a
// reviewer that would only write review.json must yield no Verdict and no pass
// Checks note, because the green belongs to an uncommitted edit, never the
// Review revision.
func TestVerifierRefusesPassNoteWhenCheckMutatesTree(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  string
	}{
		{"rewrites a tracked file", `printf 'tainted\n' > file.txt`},
		{"stages a tracked file", `printf 'tainted\n' > file.txt && git add file.txt`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := setupTestRepo(t)
			branch := "forest/9-change"
			runGitTest(t, repo, "checkout", "-q", "-b", branch)
			rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
			runGitTest(t, repo, "add", "branch.txt")
			runGitTest(t, repo, "commit", "-q", "-m", "branch work")
			runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
			head := remoteBranchHead(t, repo, branch)
			masterBefore := remoteBranchHead(t, repo, "master")
			runGitTest(t, repo, "checkout", "-q", "master")

			writeAgentFixture(t, repo, "verifier", "verifier-model")

			oldGH := ghJSON
			ghJSON = func(args ...string) ([]byte, error) {
				return []byte(`{"number":9,"title":"change","body":"","updatedAt":"","comments":[],"labels":[]}`), nil
			}
			defer func() { ghJSON = oldGH }()

			// The reviewer is blameless: it would write only review.json and approve.
			reviewRan := false
			oldRun := runPhase
			runPhase = func(_ string, wtDir string, _ *Agent, _ string, _ string) (runStats, error) {
				reviewRan = true
				if err := os.WriteFile(filepath.Join(wtDir, "review.json"), []byte(`{"verdict":"approve","summary":"looks fine","notes":""}`), 0o644); err != nil {
					return runStats{}, err
				}
				return runStats{}, nil
			}
			defer func() { runPhase = oldRun }()

			cfg := defaultConfig()
			cfg.Repo = "owner/repo"
			cfg.Checks = []Check{{Name: "tainted", Run: tc.run}}
			cfg.Flows.Verifier.Agent = "verifier"
			cfg.Flows.Verifier.AutoMerge = true
			cfg.Projection = ProjectionConfig{}

			out, err := (verifierFlow{}).Act(cfg, repo, Subject{
				Key: "branch-" + branch, Kind: "branch", Revision: head,
				Label: branch, ID: "9", Branch: branch, Head: head,
			}, "run-1")
			if err == nil {
				t.Fatalf("a check that tainted the tree returned no error: %#v", out)
			}
			if !strings.Contains(err.Error(), "file.txt") {
				t.Fatalf("refusal %q does not name the tainted tracked path", err)
			}
			if out.Status != "checks_refused" {
				t.Fatalf("status = %q, want checks_refused", out.Status)
			}

			// No pass Checks note and no Verdict, and the reviewer never ran: the
			// tainted green is refused before anything records it or acts on it.
			if _, ok, err := readChecks(repo, head); err != nil || ok {
				t.Fatalf("checks note on head = (found=%v, err=%v), want no note when a check tainted the tree", ok, err)
			}
			if _, ok, err := readVerdict(repo, head); err != nil || ok {
				t.Fatalf("verdict note on head = (found=%v, err=%v), want no Verdict when a check tainted the tree", ok, err)
			}
			if reviewRan {
				t.Fatalf("reviewer ran for a head whose checks tainted the tree")
			}
			if got := remoteBranchHead(t, repo, "master"); got != masterBefore {
				t.Fatalf("master advanced to %s, want %s, when a check tainted the tree", got, masterBefore)
			}
		})
	}
}

// TestVerifierReviewNamesMutationWhenPhaseErrors pins #191's verifier-review
// fix: the post-run clean-tree assertion must run even when the phase reports an
// error. A verifier that edits a tracked file and then crashes or times out would
// otherwise return only the harness error, never the required named clean-tree
// refusal, and the mutation would go unreported. The refusal must name the
// edited path even though the phase itself failed.
func TestVerifierReviewNamesMutationWhenPhaseErrors(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-change"
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	head := remoteBranchHead(t, repo, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")

	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) {
		return []byte(`{"number":9,"title":"change","body":"","updatedAt":"","comments":[],"labels":[]}`), nil
	}
	defer func() { ghJSON = oldGH }()

	// The verifier edits a tracked file and then crashes, so the harness error
	// and the mutation arrive together.
	phaseErr := errors.New("verifier crashed")
	mutated := false
	oldRun := runPhase
	runPhase = func(_ string, wtDir string, _ *Agent, _ string, _ string) (runStats, error) {
		mutated = true
		if err := os.WriteFile(filepath.Join(wtDir, "file.txt"), []byte("tainted\n"), 0o644); err != nil {
			return runStats{}, err
		}
		return runStats{}, phaseErr
	}
	defer func() { runPhase = oldRun }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "ok", Run: "true"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = true
	cfg.Projection = ProjectionConfig{}

	out, err := (verifierFlow{}).Act(cfg, repo, Subject{
		Key: "branch-" + branch, Kind: "branch", Revision: head,
		Label: branch, ID: "9", Branch: branch, Head: head,
	}, "run-1")
	if err == nil {
		t.Fatalf("a review that mutated the tree and crashed returned no error: %#v", out)
	}
	// The required named clean-tree refusal, not the bare harness error alone.
	if !strings.Contains(err.Error(), "file.txt") {
		t.Fatalf("refusal %q does not name the mutated tracked path; the phase error swallowed the tree check", err)
	}
	if !mutated {
		t.Fatal("the verifier did not run")
	}
	// No Verdict may rest on a tree the review itself changed.
	if _, ok, rerr := readVerdict(repo, head); rerr != nil || ok {
		t.Fatalf("verdict note on head = (found=%v, err=%v), want none when the review mutated the tree", ok, rerr)
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
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	reviewed := remoteBranchHead(t, repo, branch)

	// An unchanged remote branch is exactly the reviewed Revision: it passes.
	if err := fenceMergeOnRevision(repo, branch, reviewed); err != nil {
		t.Fatalf("unchanged branch refused by the fence: %v", err)
	}

	// The operator pushes newer, unreviewed work after the Verdict was written.
	runGitTest(t, repo, "checkout", "-q", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "later.txt"), "later\n")
	runGitTest(t, repo, "add", "later.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "newer unreviewed work")
	runGitTest(t, repo, "push", "-q", "origin", branch)
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
	if err := mergeVerified(defaultConfig(), repo, branch, reviewed,
		Item{ID: "9", Title: "change"}, testVerifierAgent()); err == nil {
		t.Fatal("mergeVerified merged a branch that advanced past its reviewed Revision")
	}
	if out := runGitTest(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out == "" {
		t.Fatal("mergeVerified deleted the branch despite refusing the merge")
	}
	if got := remoteBranchHead(t, repo, branch); got != observed {
		t.Fatalf("branch tip = %s, want the newer commits %s intact", got, observed)
	}
}

// TestMergeGitPathPinsReviewedRevision pins the retirement transaction. Master
// and its landed marker advance atomically. The marker then guards Tracker
// retirement and exact source deletion across failures and process restarts.
func TestMergeGitPathPinsReviewedRevision(t *testing.T) {
	// Tracker retirement talks to the host; stub it so the merge path runs
	// without a host CLI.
	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) { return []byte(`{}`), nil }
	defer func() { ghJSON = oldGH }()
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{}
	cfg.Flows.Verifier.Merge = "squash"
	agent := testVerifierAgent()
	it := Item{ID: "9", Title: "change"}

	t.Run("unchanged branch lands and retires", func(t *testing.T) {
		repo, branch, reviewed, masterBefore := newVerifierBranch(t, "forest/9-change")
		runGitTest(t, repo, "checkout", "-q", "master")
		runGitTest(t, repo, "branch", "-D", branch)
		if err := mergeGitPath(cfg, repo, branch, reviewed, it, agent); err != nil {
			t.Fatalf("mergeGitPath on an unchanged branch = %v, want nil", err)
		}
		if got := remoteBranchHead(t, repo, "master"); got == masterBefore {
			t.Fatal("master did not advance on an unchanged branch")
		}
		master := remoteBranchHead(t, repo, "master")
		author := runGitTest(t, repo, "show", "-s", "--format=%an <%ae>", master)
		if author != "forest-test <forest-test@example.com>" {
			t.Fatalf("squash commit author = %q, want Verifier identity", author)
		}
		if out := runGitTest(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out != "" {
			t.Fatalf("source branch %q still on origin after landing: %s", branch, out)
		}
	})

	t.Run("rejected marker keeps transaction atomic", func(t *testing.T) {
		repo, branch, reviewed, masterBefore := newVerifierBranch(t, "forest/9-change")
		origin := runGitTest(t, repo, "remote", "get-url", "origin")
		hook := filepath.Join(origin, "hooks", "update")
		script := "#!/bin/sh\ncase \"$1\" in\n  refs/forest/retirement/*) exit 1 ;;\nesac\nexit 0\n"
		if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := mergeGitPath(cfg, repo, branch, reviewed, it, agent); err == nil {
			t.Fatal("merge transaction succeeded though the retirement marker was rejected")
		}
		if got := remoteBranchHead(t, repo, "master"); got != masterBefore {
			t.Fatalf("rejected atomic transaction advanced master to %s, want %s", got, masterBefore)
		}
		if got := remoteBranchHead(t, repo, branch); got != reviewed {
			t.Fatalf("rejected atomic transaction changed source to %s, want %s", got, reviewed)
		}
		if out := runGitTest(t, repo, "ls-remote", "origin", "refs/forest/retirement/*"); out != "" {
			t.Fatalf("rejected atomic transaction published retirement fact: %s", out)
		}
		if err := os.Remove(hook); err != nil {
			t.Fatal(err)
		}
		if err := mergeGitPath(cfg, repo, branch, reviewed, it, agent); err != nil {
			t.Fatalf("retry after rejected atomic transaction: %v", err)
		}
		if got := remoteBranchHead(t, repo, "master"); got == masterBefore {
			t.Fatal("retry did not advance master")
		}
	})

	t.Run("tracker failure preserves durable recovery", func(t *testing.T) {
		repo, branch, reviewed, masterBefore := newVerifierBranch(t, "forest/9-change")
		successGH := ghJSON
		ghJSON = func(args ...string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "issue" && args[1] == "close" {
				return nil, errors.New("tracker unavailable")
			}
			return []byte(`{}`), nil
		}
		t.Cleanup(func() { ghJSON = successGH })
		if err := mergeGitPath(cfg, repo, branch, reviewed, it, agent); err == nil ||
			!strings.Contains(err.Error(), "close item") {
			t.Fatalf("mergeGitPath tracker failure = %v, want close item error", err)
		}
		if got := remoteBranchHead(t, repo, "master"); got == masterBefore {
			t.Fatal("master did not advance before Tracker retirement")
		}
		if out := runGitTest(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out != "" {
			t.Fatalf("source branch survived the durable retirement marker: %s", out)
		}
		masterAfterFailure := remoteBranchHead(t, repo, "master")
		ghJSON = successGH
		restartRoot := t.TempDir()
		restarted := filepath.Join(restartRoot, "restart")
		origin := runGitTest(t, repo, "remote", "get-url", "origin")
		runGitTest(t, restartRoot, "clone", "-q", origin, restarted)
		subjects, err := (verifierFlow{}).Select(cfg, restarted)
		if err != nil {
			t.Fatalf("restart select: %v", err)
		}
		if len(subjects) != 1 || subjects[0].Kind != "retirement" {
			t.Fatalf("restart subjects = %#v, want one retirement", subjects)
		}
		out, err := (verifierFlow{}).Act(cfg, restarted, subjects[0], "restart-run")
		if err != nil {
			t.Fatalf("retirement after restart: %v", err)
		}
		if out.Agent != agent.Name || out.Model != agent.Model || out.DefSHA != agent.DefSHA {
			t.Fatalf("recovery attribution = %#v, want marker identity %#v", out, agent)
		}
		if got := remoteBranchHead(t, restarted, "master"); got != masterAfterFailure {
			t.Fatalf("retirement retry advanced master again: got %s, want %s", got, masterAfterFailure)
		}
		if out := runGitTest(t, restarted, "ls-remote", "origin", "refs/heads/"+branch); out != "" {
			t.Fatalf("source branch survived retirement retry: %s", out)
		}
		if facts, err := listRetirements(restarted); err != nil || len(facts) != 0 {
			t.Fatalf("retirement facts after retry = (%#v, %v), want none", facts, err)
		}
	})

	t.Run("advanced branch is refused and survives", func(t *testing.T) {
		repo, branch, reviewed, masterBefore := newVerifierBranch(t, "forest/9-change")
		// The operator pushes newer, unreviewed work after the Verdict and after
		// the fence was read, simulating the review-to-merge window.
		runGitTest(t, repo, "checkout", "-q", branch)
		rebaseTestWriteFile(t, filepath.Join(repo, "later.txt"), "later\n")
		runGitTest(t, repo, "add", "later.txt")
		runGitTest(t, repo, "commit", "-q", "-m", "newer unreviewed work")
		runGitTest(t, repo, "push", "-q", "origin", branch)
		observed := remoteBranchHead(t, repo, branch)
		if observed == reviewed {
			t.Fatal("branch did not advance, cannot probe the race")
		}

		if err := mergeGitPath(cfg, repo, branch, reviewed, it, agent); err == nil {
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
	branch := "forest/9-change"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)

	oldProj := projectionCommand
	defer func() { projectionCommand = oldProj }()
	var mergeArgs []string
	projectionCommand = func(args ...string) ([]byte, error) {
		switch args[1] {
		case "list":
			return []byte(`[{"number":9,"url":"https://github.com/owner/repo/pull/9","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
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

	if err := mergeVerified(cfg, repo, branch, reviewed, Item{ID: "9", Title: "change"},
		testVerifierAgent()); err == nil {
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
	if out := runGitTest(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out == "" {
		t.Fatal("mergeVerified deleted the branch even though the host merge refused")
	}
}

func TestVerifierRecoversAlreadyMergedProjectionWithoutDuplicate(t *testing.T) {
	branch := "forest/9-already-merged"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	runGitTest(t, repo, "branch", "-D", branch)
	writeAgentFixture(t, repo, "verifier", "verifier-model")
	if err := writeChecks(repo, reviewed, checksNote{Status: "pass", RunID: "seed"}); err != nil {
		t.Fatal(err)
	}
	if err := writeVerdict(repo, reviewed, verdictNote{
		Verdict: "approve", Reviewer: "verifier", Model: "verifier-model", DefSHA: "def", RunID: "seed",
	}); err != nil {
		t.Fatal(err)
	}
	oldTracker := trackerFor
	tk := newMemoryTracker()
	tk.seed(Item{ID: "9", Title: "already merged"})
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	createCalls, mergeCalls := 0, 0
	projectionCommand = func(args ...string) ([]byte, error) {
		state := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--state" {
				state = args[i+1]
			}
		}
		switch {
		case args[1] == "list" && state == "open":
			return []byte(`[]`), nil
		case args[1] == "list" && state == "merged":
			return []byte(`[{"number":9,"url":"https://github.com/owner/repo/pull/9","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		case args[1] == "create":
			createCalls++
			return nil, errors.New("duplicate pull request")
		case args[1] == "merge":
			mergeCalls++
			return nil, errors.New("duplicate Host merge")
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = true
	out, err := (verifierFlow{}).Act(cfg, repo, Subject{
		Key: "branch-" + branch, Kind: "branch", Revision: reviewed,
		ID: "9", Branch: branch, Head: reviewed,
	}, "recover")
	if err != nil || out.Status != "merged" {
		t.Fatalf("Verifier merged-Projection recovery = (status=%q, err=%v)", out.Status, err)
	}
	if createCalls != 0 || mergeCalls != 0 {
		t.Fatalf("recovery made create/merge calls = %d/%d", createCalls, mergeCalls)
	}
	if _, err := tk.Get("9"); err == nil {
		t.Fatal("recovery did not close the Tracker Item")
	}
}

func TestMergeViaHostRecoversTrackerCloseFailure(t *testing.T) {
	branch := "forest/9-host-recovery"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	merged := false
	mergeCalls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		if len(args) < 2 {
			return nil, errors.New("unexpected host command")
		}
		if args[1] == "merge" {
			merged = true
			mergeCalls++
			if err := deleteRef(repo, "refs/heads/"+branch, reviewed); err != nil {
				return nil, err
			}
			return nil, nil
		}
		if args[1] != "list" {
			return nil, errors.New("unexpected host command")
		}
		state := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--state" {
				state = args[i+1]
			}
		}
		switch {
		case state == "open" && !merged:
			return []byte(`[{"number":9,"url":"https://github.com/owner/repo/pull/9","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		case state == "merged" && merged:
			return []byte(`[{"number":9,"url":"https://github.com/owner/repo/pull/9","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		default:
			return []byte(`[]`), nil
		}
	}

	oldGH := ghJSON
	defer func() { ghJSON = oldGH }()
	closeCalls := 0
	ghJSON = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "issue" && args[1] == "close" {
			closeCalls++
			if closeCalls == 1 {
				return nil, errors.New("tracker unavailable")
			}
		}
		return []byte(`{}`), nil
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Merge = "squash"
	item := Item{ID: "9", Title: "change"}
	if err := mergeVerified(cfg, repo, branch, reviewed, item,
		testVerifierAgent()); err == nil ||
		!strings.Contains(err.Error(), "close item") {
		t.Fatalf("first host merge = %v, want Tracker close failure", err)
	}
	// A landed retirement fact is sufficient after restart. Remove the Host
	// oracle before cloning so recovery cannot reuse process-local merge state
	// or ask the Host to merge a second time.
	projectionCommand = func(...string) ([]byte, error) {
		return nil, errors.New("Host called during landed retirement recovery")
	}
	if out := runGitTest(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out != "" {
		t.Fatalf("host did not auto-delete source branch: %s", out)
	}
	restartRoot := t.TempDir()
	restarted := filepath.Join(restartRoot, "restart")
	origin := runGitTest(t, repo, "remote", "get-url", "origin")
	runGitTest(t, restartRoot, "clone", "-q", origin, restarted)
	subjects, err := (verifierFlow{}).Select(cfg, restarted)
	if err != nil {
		t.Fatalf("restart select: %v", err)
	}
	if len(subjects) != 1 || subjects[0].Kind != "retirement" {
		t.Fatalf("recovery subjects = %#v, want one retirement", subjects)
	}
	out, err := (verifierFlow{}).Act(cfg, restarted, subjects[0], "recovery-run")
	if err != nil {
		t.Fatalf("host merge recovery: %v", err)
	}
	if out.Status != "merged" {
		t.Fatalf("recovery status = %q, want merged", out.Status)
	}
	agent := testVerifierAgent()
	if out.Agent != agent.Name || out.Model != agent.Model || out.DefSHA != agent.DefSHA {
		t.Fatalf("host recovery attribution = %#v, want marker identity %#v", out, agent)
	}
	if mergeCalls != 1 || closeCalls != 2 {
		t.Fatalf("recovery effects: merge=%d close=%d, want merge=1 close=2", mergeCalls, closeCalls)
	}
	if facts, err := listRetirements(restarted); err != nil || len(facts) != 0 {
		t.Fatalf("retirement facts after recovery = (%#v, %v), want none", facts, err)
	}
}

func TestPendingHostRetirementRecoversAfterBranchAutoDelete(t *testing.T) {
	branch := "forest/10-pending-host"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	if _, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "10", Transport: "host",
		Strategy: "squash", Title: "pending host", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	}); err != nil {
		t.Fatal(err)
	}
	if err := deleteRef(repo, "refs/heads/"+branch, reviewed); err != nil {
		t.Fatal(err)
	}

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	mergeCalls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[1] == "merge" {
			mergeCalls++
			return nil, errors.New("recovery attempted a duplicate Host merge")
		}
		if len(args) >= 2 && args[1] == "list" {
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--state" && args[i+1] == "merged" {
					return []byte(`[{"number":10,"url":"https://github.com/owner/repo/pull/10","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
				}
			}
			return []byte(`[]`), nil
		}
		return nil, errors.New("unexpected Host command")
	}
	oldGH := ghJSON
	defer func() { ghJSON = oldGH }()
	closeCalls := 0
	ghJSON = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "issue" && args[1] == "close" {
			closeCalls++
		}
		return []byte(`{}`), nil
	}

	restartRoot := t.TempDir()
	restarted := filepath.Join(restartRoot, "restart")
	origin := runGitTest(t, repo, "remote", "get-url", "origin")
	runGitTest(t, restartRoot, "clone", "-q", origin, restarted)
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Merge = "squash"
	subjects, err := (verifierFlow{}).Select(cfg, restarted)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].Kind != "retirement" {
		t.Fatalf("recovery subjects = %#v, want one retirement", subjects)
	}
	if _, err := (verifierFlow{}).Act(cfg, restarted, subjects[0], "pending-recovery"); err != nil {
		t.Fatalf("pending retirement recovery: %v", err)
	}
	if mergeCalls != 0 || closeCalls != 1 {
		t.Fatalf("recovery effects: merge=%d close=%d, want merge=0 close=1", mergeCalls, closeCalls)
	}
	if facts, err := listRetirements(restarted); err != nil || len(facts) != 0 {
		t.Fatalf("retirement facts after recovery = (%#v, %v), want none", facts, err)
	}
}

// TestPendingHostRetirementUsesRecordedStrategy proves recovery repeats the
// recorded merge effect, not a strategy changed in later configuration.
func TestPendingHostRetirementUsesRecordedStrategy(t *testing.T) {
	branch := "forest/11-strategy"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	fact, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "11", Transport: "host",
		Strategy: "squash", Title: "strategy", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	var mergeArgs []string
	merged := false
	projectionCommand = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[1] == "list" {
			state := ""
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--state" {
					state = args[i+1]
				}
			}
			if state == "open" && !merged {
				return []byte(`[{"number":11,"url":"https://github.com/owner/repo/pull/11","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
			}
			if state == "merged" && merged {
				return []byte(`[{"number":11,"url":"https://github.com/owner/repo/pull/11","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
			}
			return []byte(`[]`), nil
		}
		if len(args) >= 2 && args[1] == "merge" {
			mergeArgs = append([]string(nil), args...)
			merged = true
			return nil, nil
		}
		return nil, errors.New("unexpected Host command")
	}
	oldGH := ghJSON
	defer func() { ghJSON = oldGH }()
	ghJSON = func(...string) ([]byte, error) { return []byte(`{}`), nil }
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Merge = "ff"
	if err := recoverRetirementFact(cfg, repo, fact, Item{ID: "11", Title: "strategy"}); err != nil {
		t.Fatal(err)
	}
	foundSquash := false
	for _, arg := range mergeArgs {
		foundSquash = foundSquash || arg == "--squash"
	}
	if !foundSquash {
		t.Fatalf("recovery merge args %v do not use recorded squash strategy", mergeArgs)
	}
	for _, arg := range mergeArgs {
		if arg == "--rebase" {
			t.Fatalf("recovery merge args %v used current ff strategy", mergeArgs)
		}
	}
}

func TestRetirementFactBlocksAdvancedBranch(t *testing.T) {
	branch := "forest/12-advanced"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	if _, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "12", Transport: "host",
		Strategy: "squash", Title: "advanced", State: "landed",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	}); err != nil {
		t.Fatal(err)
	}
	rebaseTestWriteFile(t, filepath.Join(repo, "later.txt"), "later\n")
	runGitTest(t, repo, "add", "later.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "later")
	runGitTest(t, repo, "push", "-q", "origin", branch)
	advanced := remoteBranchHead(t, repo, branch)

	oldGH := ghJSON
	defer func() { ghJSON = oldGH }()
	closeCalls := 0
	ghJSON = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "issue" && args[1] == "close" {
			closeCalls++
		}
		return []byte(`{}`), nil
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].Kind != "retirement" || subjects[0].Revision != reviewed {
		t.Fatalf("advanced retirement subjects = %#v, want old durable fact", subjects)
	}
	if _, err := (verifierFlow{}).Act(cfg, repo, subjects[0], "advanced-retirement"); err == nil ||
		!strings.Contains(err.Error(), "advanced") {
		t.Fatalf("advanced retirement = %v, want named refusal", err)
	}
	if closeCalls != 0 {
		t.Fatalf("advanced retirement closed Tracker %d time(s)", closeCalls)
	}
	if got := remoteBranchHead(t, repo, branch); got != advanced {
		t.Fatalf("advanced branch = %s, want %s intact", got, advanced)
	}
}

// TestStalledOnPersistsOutsideLedger pins the durable progress brake: three
// failures on one revision stop a subject, a new revision resets it, and the
// decision remains after the host-local ledger is removed.
func TestStalledOnPersistsOutsideLedger(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	const subject = "branch-forest/../../opaque ?~^:[x]"
	const revision = "aaa"
	if got, want := stalledRef("fixer", "branch-forest/9-x"), "refs/forest/stalled/fixer/branch-forest/9-x"; got != want {
		t.Fatalf("legacy stalled ref = %q, want %q", got, want)
	}
	if got := stalledRef("fixer", subject); !strings.HasPrefix(got, "refs/forest/stalled-opaque/fixer/") {
		t.Fatalf("opaque stalled ref = %q, want encoded namespace", got)
	}
	for _, invalid := range []string{".item", "item.lock", "foo./bar", "item..x", "item@{x", "item?x", `item\x`, "item/"} {
		if got := stalledRef("fixer", invalid); !strings.HasPrefix(got, "refs/forest/stalled-opaque/fixer/") {
			t.Errorf("invalid Subject %q produced raw stalled ref %q", invalid, got)
		}
	}
	if a, b := stalledRef("fixer", "item?x"), stalledRef("fixer", "item*x"); a == b {
		t.Fatalf("distinct opaque Subjects share stalled ref %q", a)
	}
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
	if err := commitAndPush(work, work, branch, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", id, it); err == nil {
		t.Fatal("a stale observed ref must lose the push")
	}
	if err := os.WriteFile(filepath.Join(work, "fix.txt"), []byte("repair\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAndPush(work, work, branch, observed, id, it); err != nil {
		t.Fatalf("rebased branch with the observed ref = %v, want nil", err)
	}
	remote := runGitTest(t, work, "rev-parse", "refs/remotes/origin/"+branch)
	local := runGitTest(t, work, "rev-parse", "HEAD")
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
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	head := remoteBranchHead(t, repo, branch)
	runGitTest(t, repo, "checkout", "-q", "master")

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

func TestRetirementRecoveryRejectsMismatchedItem(t *testing.T) {
	repo, branch, reviewed, _ := newVerifierBranch(t, "forest/12-mismatch")
	agent := testVerifierAgent()
	fact, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "12", Transport: "git",
		Strategy: "squash", Title: "mismatch", State: "landed",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	oldGH := ghJSON
	defer func() { ghJSON = oldGH }()
	hostCalls := 0
	ghJSON = func(args ...string) ([]byte, error) {
		hostCalls++
		return nil, errors.New("unexpected Host call")
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	err = recoverRetirementFact(cfg, repo, fact, Item{ID: "99", Title: "wrong Item"})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched Item recovery = %v, want refusal", err)
	}
	if hostCalls != 0 {
		t.Fatalf("mismatched Item recovery made %d Host calls", hostCalls)
	}
	if got := remoteBranchHead(t, repo, branch); got != reviewed {
		t.Fatalf("mismatched Item recovery changed branch to %s, want %s", got, reviewed)
	}
	if facts, err := listRetirements(repo); err != nil || len(facts) != 1 {
		t.Fatalf("retirement facts after refusal = (%#v, %v), want one", facts, err)
	}
}
