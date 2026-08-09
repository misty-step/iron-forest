package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type responseLossCommentTracker struct {
	*memoryTracker
	commentCalls int
}

func (t *responseLossCommentTracker) Comment(id, body string) error {
	t.commentCalls++
	if err := t.memoryTracker.Comment(id, body); err != nil {
		return err
	}
	return errors.New("response lost after acceptance")
}

type invisibleCommentTracker struct {
	*memoryTracker
	commentCalls int
}

func (t *invisibleCommentTracker) Comment(string, string) error {
	t.commentCalls++
	return errors.New("response lost without visible comment")
}

type responseLossTagTracker struct {
	*memoryTracker
	tagCalls int
}

func (t *responseLossTagTracker) SetTags(id string, add, remove []string) error {
	t.tagCalls++
	if err := t.memoryTracker.SetTags(id, add, remove); err != nil {
		return err
	}
	return errors.New("response lost after acceptance")
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

func TestPublishTrackerCommentIsIdempotentAfterResponseLoss(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	const id = `/../../outside\trace?[x]`
	memory := newMemoryTracker()
	memory.seed(Item{ID: id, Title: "change"})
	tracker := &responseLossCommentTracker{memoryTracker: memory}
	revision := strings.Repeat("a", 40)

	it, err := tracker.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	marker := "<!-- iron-forest:merge-blocked revision=" + revision + " -->"
	if err := publishTrackerComment(repo, tracker, it, "Tracker-comment", revision,
		"Merge blocked: merge failed", marker); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	it, err = tracker.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishTrackerComment(repo, tracker, it, "Tracker-comment", revision,
		"Merge blocked: merge failed", marker); err != nil {
		t.Fatalf("retry publish: %v", err)
	}
	if tracker.commentCalls != 1 || len(it.Comments) != 1 ||
		!strings.Contains(it.Comments[0].Body, "<!-- iron-forest:merge-blocked revision="+revision+" -->") {
		t.Fatalf("comments = (%d calls, %#v), want one exact-Revision effect",
			tracker.commentCalls, it.Comments)
	}
}
func TestPublishTrackerCommentDoesNotRepeatInvisibleOutcome(t *testing.T) {
	_, repo, revision := notesTestRepository(t)
	item := Item{ID: "9", Title: "change"}
	memory := newMemoryTracker()
	memory.seed(item)
	tracker := &invisibleCommentTracker{memoryTracker: memory}
	marker := "<!-- iron-forest:built revision=" + revision + " -->"
	for range 2 {
		err := publishTrackerComment(repo, tracker, item,
			"Tracker-builder-comment", revision, "Built branch `forest/9-change`.", marker)
		if !errors.Is(err, errHostMergeUnavailable) {
			t.Fatalf("invisible Tracker comment = %v, want hard uncertainty", err)
		}
	}
	if tracker.commentCalls != 1 {
		t.Fatalf("Tracker comment calls = %d, want one", tracker.commentCalls)
	}
	if attempts, err := readAttempts(repo,
		effectAttemptKey("Tracker-builder-comment", item.ID, revision)); err != nil || attempts != 1 {
		t.Fatalf("Tracker comment claim = (%d, %v), want one", attempts, err)
	}
}

func TestHostHandoffReconcilesAcceptedTagResponseLoss(t *testing.T) {
	_, repo, revision := notesTestRepository(t)
	item := Item{ID: "9", Title: "change"}
	memory := newMemoryTracker()
	memory.seed(item)
	tracker := &responseLossTagTracker{memoryTracker: memory}
	old := trackerFor
	trackerFor = func(string) Tracker { return tracker }
	defer func() { trackerFor = old }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	record := retirementRecord{Branch: "forest/9-change", Revision: revision, ItemID: item.ID}
	err := recordHostMergeHandoff(cfg, repo, record, item, errors.New("Host refused merge"))
	if !errors.Is(err, errRetirementRecoveryHard) {
		t.Fatalf("Host handoff = %v, want durable hard handoff", err)
	}
	err = recordHostMergeHandoff(cfg, repo, record,
		Item{ID: item.ID, Title: item.Title}, errors.New("Host refused merge"))
	if !errors.Is(err, errRetirementRecoveryHard) {
		t.Fatalf("reconstructed Host handoff = %v, want retained hard handoff", err)
	}
	got, getErr := tracker.Get(item.ID)
	if getErr != nil || tracker.tagCalls != 1 || !got.hasTag(failedLabel) || len(got.Comments) != 1 {
		t.Fatalf("reconciled handoff = (%#v, tags=%d, err=%v), want one tag call and one comment",
			got, tracker.tagCalls, getErr)
	}
	if stalled, stallErr := stalledOn(repo, (verifierFlow{}).Name(),
		retirementSubjectKey(record.Branch), revision); stallErr != nil || !stalled {
		t.Fatalf("Host handoff brake = (%v, %v), want terminal", stalled, stallErr)
	}
}

func TestVerifierSelectionDoesNotLetRetirementRecoveryStarveBranchWork(t *testing.T) {
	const branch = "forest/2-fresh"
	repo, _, _, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	if _, err := recordRetirement(repo, retirementRecord{
		Branch: "forest/1-pending", Revision: strings.Repeat("a", 40), ItemID: "1",
		Transport: "host", Strategy: "squash", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	}); err != nil {
		t.Fatal(err)
	}
	subjects, err := (verifierFlow{}).Select(defaultConfig(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 2 || subjects[0].Kind != subjectBranch || subjects[0].Branch != branch ||
		subjects[1].Kind != subjectRetirement {
		t.Fatalf("Verifier selection order = %#v, want branch work before pending retirement", subjects)
	}
}
func TestMalformedRetirementDoesNotStarveUnrelatedBranchOrReopenItem(t *testing.T) {
	const freshBranch = "forest/2-fresh"
	repo, _, freshRevision, _ := newVerifierBranch(t, freshBranch)
	badBranch := "forest/1-bad"
	badRevision := strings.Repeat("b", 40)
	if err := putBlobRef(repo, retirementRef(badBranch, badRevision), "{", ""); err != nil {
		t.Fatal(err)
	}

	subjects, err := (verifierFlow{}).Select(defaultConfig(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 2 || subjects[0].Branch != freshBranch ||
		subjects[1].Failure == nil || subjects[1].Branch != badBranch {
		t.Fatalf("Verifier subjects = %#v, want fresh branch then invalid retirement", subjects)
	}

	tk := newMemoryTracker()
	tk.seed(Item{ID: "1", Title: "bad retirement"})
	tk.seed(Item{ID: "2", Title: "covered branch"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	if builderSubjects, err := (builderFlow{}).Select(defaultConfig(), repo); err != nil || len(builderSubjects) != 0 {
		t.Fatalf("Builder subjects = (%#v, %v), want malformed retirement and branch excluded",
			builderSubjects, err)
	}
	cfg := defaultConfig()
	cfg.Flows.Verifier.Merge = "squash"
	if _, err := recordPreparingHostRetirement(
		cfg,
		repo,
		freshBranch,
		freshRevision,
		Item{ID: "2", Title: "covered branch"},
	); err != nil {
		t.Fatalf("unrelated retirement preparation = %v, want success", err)
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
	if len(subjects) != 1 || subjects[0].Revision != head || subjects[0].ID != "9" ||
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
	if len(repairs) != 1 || repairs[0].Revision != head || repairs[0].ID != "9" ||
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
	if len(subjects) != 1 || subjects[0].Revision != newHead || subjects[0].ID != "9" ||
		canonicalAdmissionKey(subjects[0]) != "item-9" {
		t.Fatalf("repaired head = %#v, want item-9 at %s", subjects, newHead)
	}
	if err := writeVerdict(work, newHead, verdictNote{Verdict: "approve", Reviewer: "verifier", Model: "m", DefSHA: strings.Repeat("a", 16)}); err != nil {
		t.Fatal(err)
	}
	if subjects, err = (verifierFlow{}).Select(cfg, work); err != nil ||
		len(subjects) != 1 || subjects[0].Revision != newHead {
		t.Fatalf("approved head missing Checks = (%#v, %v), want check requalification", subjects, err)
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
			return []byte(`{"number":9,"title":"change","body":"","updatedAt":"u1","comments":[],"labels":[]}`), nil
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
				DefSHA: strings.Repeat("a", 16), RunID: "seed",
			}); err != nil {
				t.Fatalf("seed verdict: %v", err)
			}
		}

		out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: "branch", Revision: head,
			Label: branch, ID: "9", Branch: branch}, "run-1")
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
		DefSHA: strings.Repeat("a", 16), RunID: "seed",
	}); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}

	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) {
		return []byte(`{"number":9,"title":"change","body":"","updatedAt":"u1","comments":[],"labels":[]}`), nil
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

	out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: "branch", Revision: head,
		Label: branch, ID: "9", Branch: branch}, "run-1")
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
	if len(subjects) != 1 || subjects[0].Branch != branch {
		t.Fatalf("preflight-failed head = %#v, want one bounded check requalification", subjects)
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
		DefSHA: strings.Repeat("a", 16), RunID: "seed",
	}); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}
	// A stale pass note from an earlier, buggy pass already keys this Revision.
	if err := writeChecks(repo, head, checksNote{Status: "pass", RunID: "stale", Time: nowRFC()}); err != nil {
		t.Fatalf("seed stale checks: %v", err)
	}

	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) {
		return []byte(`{"number":9,"title":"change","body":"","updatedAt":"u1","comments":[],"labels":[]}`), nil
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

	out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: "branch", Revision: head,
		Label: branch, ID: "9", Branch: branch}, "run-2")
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
				return []byte(`{"number":9,"title":"change","body":"","updatedAt":"u1","comments":[],"labels":[]}`), nil
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

			out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: "branch", Revision: head,
				Label: branch, ID: "9", Branch: branch}, "run-1")
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
		return []byte(`{"number":9,"title":"change","body":"","updatedAt":"u1","comments":[],"labels":[]}`), nil
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

	out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: "branch", Revision: head,
		Label: branch, ID: "9", Branch: branch}, "run-1")
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
		Verdict: "approve", Reviewer: "verifier", Model: "m", DefSHA: strings.Repeat("a", 16), RunID: "seed",
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

func TestVerifierActRefreshesWinningVerdictBeforeReview(t *testing.T) {
	branch := "forest/32-winning-verdict"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "32", Title: "winning verdict", UpdatedAt: "u1"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "test", Run: "true"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = false
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 {
		t.Fatalf("stale Verdict Select = (%#v, %v), want one branch", subjects, err)
	}

	root := t.TempDir()
	writer := filepath.Join(root, "writer")
	origin := runGitTest(t, repo, "remote", "get-url", "origin")
	runGitTest(t, root, "clone", "-q", origin, writer)
	if err := writeChecks(writer, reviewed, checksNote{Status: "pass", RunID: "winner"}); err != nil {
		t.Fatal(err)
	}
	if err := writeVerdict(writer, reviewed, verdictNote{
		Verdict: "changes", Notes: "winning rejection", Reviewer: "other-verifier",
		Model: "other-model", DefSHA: strings.Repeat("b", 16), RunID: "winner",
	}); err != nil {
		t.Fatal(err)
	}

	oldRun := runPhase
	runPhase = func(string, string, *Agent, string, string) (runStats, error) {
		t.Fatal("stale checkout paid for a duplicate Verifier review")
		return runStats{}, nil
	}
	defer func() { runPhase = oldRun }()
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	listCalls, commentCalls := 0, 0
	projectionCommand = func(args ...string) ([]byte, error) {
		if args[0] == "pr" && args[1] == "list" {
			listCalls++
			head := reviewed
			if listCalls > 1 {
				head = strings.Repeat("a", 40)
			}
			return []byte(`[{"number":32,"url":"https://github.com/owner/repo/pull/32","headRefOid":"` +
				head + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		}
		if args[0] == "api" {
			commentCalls++
			return nil, nil
		}
		return nil, errors.New("unexpected Host command")
	}

	out, err := (verifierFlow{}).Act(cfg, repo, subjects[0], "stale-winner")
	if !errors.Is(err, errHostMergeUnavailable) || out.Status != "projection_failed" || out.Verdict != "changes" {
		t.Fatalf("stale winning Verdict Act = (%#v, %v), want fenced winning rejection", out, err)
	}
	if commentCalls != 0 {
		t.Fatalf("head-drifted winning Verdict issued %d Host comments", commentCalls)
	}
	fact, found, err := readRetirement(repo, branch, reviewed)
	if err != nil || !found || fact.Record.State != "preparing" {
		t.Fatalf("stale winning Verdict preparation = (%#v, %v, %v), want no pending intent", fact, found, err)
	}
}

func TestVerifierHostPreparationRetriesWithoutStall(t *testing.T) {
	branch := "forest/35-host-retry"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "35", Title: "host retry"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	projectionCommand = func(args ...string) ([]byte, error) {
		return nil, errors.Join(errHostMergeUnavailable, errHostRevisionMoved)
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Agent = "verifier"
	subject := Subject{Key: "branch-" + branch, Kind: subjectBranch, Revision: reviewed,
		ID: "35", Branch: branch}
	for range stalledRunLimit {
		if code := actOnSubject(verifierFlow{}, cfg, repo, subject, nil); code != 1 {
			t.Fatalf("transient Host pass code = %d, want retryable failure", code)
		}
	}
	if stalled, err := stalledOn(repo, "verifier", subject.Key, reviewed); err != nil || stalled {
		t.Fatalf("transient Host preparation stalled = (%v, %v), want retryable", stalled, err)
	}
	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement {
		t.Fatalf("post-transient Select = (%#v, %v), want durable retirement retry", subjects, err)
	}
}

func TestVerifierPreparationMigrationRetriesTransientHostQuery(t *testing.T) {
	branch := "forest/43-migration-retry"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Agent = "verifier"
	item := Item{ID: "43", Title: "migration retry"}
	if _, err := recordPreparingHostRetirement(cfg, repo, branch, reviewed, item); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "checkout", "-q", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "advanced.txt"), "advanced\n")
	runGitTest(t, repo, "add", "advanced.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "advance before Host query")
	runGitTest(t, repo, "push", "-q", "origin", branch)
	advanced := remoteBranchHead(t, repo, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")

	tk := newMemoryTracker()
	tk.seed(item)
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	projectionCommand = func(args ...string) ([]byte, error) {
		return nil, errors.New("transient Host query failure")
	}
	subject := Subject{Key: "branch-" + branch, Kind: subjectBranch, Revision: advanced,
		ID: "43", Branch: branch}
	for range stalledRunLimit {
		if code := actOnSubject(verifierFlow{}, cfg, repo, subject, nil); code != 1 {
			t.Fatalf("preparation migration pass code = %d, want retryable failure", code)
		}
	}
	if stalled, err := stalledOn(repo, "verifier", subject.Key, advanced); err != nil || stalled {
		t.Fatalf("transient migration query stalled = (%v, %v), want retryable", stalled, err)
	}
	oldFact, found, err := readRetirement(repo, branch, reviewed)
	if err != nil || !found || oldFact.Record.State != "preparing" {
		t.Fatalf("transient migration retained preparation = (%#v, found=%v, err=%v)", oldFact, found, err)
	}
}

func TestVerifierMalformedHostProjectionUsesFailureBrake(t *testing.T) {
	branch := "forest/41-malformed-projection"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "41", Title: "malformed Projection"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	projectionCommand = func(args ...string) ([]byte, error) {
		if args[0] == "pr" && args[1] == "list" {
			return []byte(`[{"number":0,"url":"","headRefOid":"` + reviewed +
				`","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		}
		return nil, errors.New("unexpected Host command")
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Agent = "verifier"
	subject := Subject{Key: "branch-" + branch, Kind: subjectBranch, Revision: reviewed,
		ID: "41", Branch: branch}
	if code := actOnSubject(verifierFlow{}, cfg, repo, subject, nil); code != 1 {
		t.Fatalf("malformed Host pass code = %d, want failure", code)
	}
	if stalled, err := stalledOn(repo, "verifier", subject.Key, reviewed); err != nil || !stalled {
		t.Fatalf("malformed Host Projection stalled = (%v, %v), want hard brake", stalled, err)
	}
}

func TestVerifierRetirementIgnoresBranchFailureBrake(t *testing.T) {
	branch := "forest/38-pending-braked"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	writeApprovalNotes(t, repo, reviewed, agent)
	if _, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "38", Transport: "host",
		Strategy: "squash", Title: "pending", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	}); err != nil {
		t.Fatal(err)
	}
	for range stalledRunLimit {
		if err := recordStalled(repo, "verifier", "branch-"+branch, reviewed); err != nil {
			t.Fatal(err)
		}
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement {
		t.Fatalf("braked branch retirement Select = (%#v, %v), want recovery", subjects, err)
	}
}

func TestVerifierBranchLossRetainsObservedHostMergeUntilApproval(t *testing.T) {
	branch := "forest/31-branch-loss"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")

	tk := newMemoryTracker()
	tk.seed(Item{ID: "31", Title: "branch loss", UpdatedAt: "u1", Tags: []string{readyTag}})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.AutoMerge = false
	if _, err := recordPreparingHostRetirement(cfg, repo, branch, reviewed,
		Item{ID: "31", Title: "branch loss"}); err != nil {
		t.Fatal(err)
	}
	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement {
		t.Fatalf("branch-loss Select = (%#v, %v), want one durable retirement", subjects, err)
	}

	runGitTest(t, repo, "branch", "-D", branch)
	if err := deleteRef(repo, "refs/heads/"+branch, reviewed); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "fetch", "-q", "--prune", "origin")

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	writes := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "api":
			return mergedProjectionPage(`{"number":31,"html_url":"https://github.com/owner/repo/pull/31","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"` + reviewed + `","ref":"` + branch + `","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
			return []byte(`[]`), nil
		case len(args) >= 2 && args[0] == "pr" && (args[1] == "create" || args[1] == "merge"):
			writes++
			return nil, errors.New("duplicate Host write")
		default:
			return nil, errors.New("unexpected Host command")
		}
	}

	recoveries, selectErr := (verifierFlow{}).Select(cfg, repo)
	if selectErr != nil || len(recoveries) != 1 || recoveries[0].Kind != subjectRetirement {
		t.Fatalf("fresh branch-loss Select = (%#v, %v), want durable retirement", recoveries, selectErr)
	}
	out, err := (verifierFlow{}).Act(cfg, repo, recoveries[0], "observe-merge")
	if err != nil || out.Status != "merge_pending" {
		t.Fatalf("branch-loss Act = (status=%q, err=%v), want retained merge_pending", out.Status, err)
	}
	facts, err := listRetirements(repo)
	if err != nil || len(facts) != 1 || facts[0].Record.State != "observed" {
		t.Fatalf("observed Host retirement = (%#v, %v), want one durable observation", facts, err)
	}
	if candidates, err := (builderFlow{}).Select(cfg, repo); err != nil || len(candidates) != 0 {
		t.Fatalf("Builder re-exposed Host-merged Item = (%#v, %v)", candidates, err)
	}

	agent := testVerifierAgent()
	writeApprovalNotes(t, repo, reviewed, agent)
	recoveries, err = (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(recoveries) != 1 || recoveries[0].Kind != subjectRetirement {
		t.Fatalf("observed recovery Select = (%#v, %v), want one retirement", recoveries, err)
	}
	out, err = (verifierFlow{}).Act(cfg, repo, recoveries[0], "recover-merge")
	if err != nil || out.Status != "merged" {
		t.Fatalf("observed recovery Act = (status=%q, err=%v), want merged", out.Status, err)
	}
	if out.Agent != agent.Name || out.Model != agent.Model || out.DefSHA != agent.DefSHA {
		t.Fatalf("observed recovery attribution = %#v, want %#v", out, agent)
	}
	if writes != 0 {
		t.Fatalf("observed recovery made %d duplicate Host writes", writes)
	}
	if _, err := tk.Get("31"); err == nil {
		t.Fatal("observed recovery left the Tracker Item open")
	}
	if facts, err := listRetirements(repo); err != nil || len(facts) != 0 {
		t.Fatalf("observed recovery facts = (%#v, %v), want retired", facts, err)
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

// TestVerifierActMigratesHostPreparationAndPinsPostRebaseEvidence drives the
// full Verifier Act path through Host preparation, rebase, Checks, and Verdict.
func TestVerifierActMigratesHostPreparationAndPinsPostRebaseEvidence(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/54-host-rebase"
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	oldHead := remoteBranchHead(t, repo, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	rebaseTestWriteFile(t, filepath.Join(repo, "master.txt"), "master\n")
	runGitTest(t, repo, "add", "master.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "master work")
	runGitTest(t, repo, "push", "-q", "origin", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")

	item := Item{ID: "54", Title: "Host rebase"}
	tk := newMemoryTracker()
	tk.seed(item)
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	created := false
	var commitIDs []string
	projectionCommand = func(args ...string) ([]byte, error) {
		if !created {
			fact, found, err := readRetirement(repo, branch, oldHead)
			if err != nil || !found || fact.Record.State != "preparing" {
				t.Fatalf("first Host sink preparation = (%#v, %v, %v), want preparing fact", fact, found, err)
			}
		}
		switch {
		case args[0] == "pr" && args[1] == "list":
			if !created {
				return []byte(`[]`), nil
			}
			head := remoteBranchHead(t, repo, branch)
			return []byte(`[{"number":54,"url":"https://github.com/owner/repo/pull/54","headRefOid":"` + head +
				`","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		case args[0] == "api" && hasArgumentPair(args, "--method", "GET"):
			return []byte(`[[]]`), nil
		case args[0] == "pr" && args[1] == "create":
			created = true
			return []byte("https://github.com/owner/repo/pull/54"), nil
		case args[0] == "api" && hasArgumentPair(args, "--method", "POST"):
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--field" && strings.HasPrefix(args[i+1], "commit_id=") {
					commitIDs = append(commitIDs, strings.TrimPrefix(args[i+1], "commit_id="))
				}
			}
			return nil, nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	oldRun := runPhase
	runPhase = func(_ string, wtDir string, _ *Agent, _ string, _ string) (runStats, error) {
		if err := os.WriteFile(filepath.Join(wtDir, "review.json"), []byte(`{"verdict":"approve","summary":"approved","notes":""}`), 0o644); err != nil {
			return runStats{}, err
		}
		return runStats{}, nil
	}
	defer func() { runPhase = oldRun }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "true", Run: "true"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = false
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: subjectBranch, Revision: oldHead,
		ID: item.ID, Branch: branch}, "host-rebase")
	if err != nil || out.Status != "reviewed" {
		t.Fatalf("Host rebase Verifier Act = (%#v, %v), want reviewed", out, err)
	}
	newHead := remoteBranchHead(t, repo, branch)
	if newHead == oldHead || out.BaseSHA != newHead {
		t.Fatalf("Host rebase heads = (old=%s, new=%s, outcome=%s), want post-rebase outcome", oldHead, newHead, out.BaseSHA)
	}
	if len(commitIDs) != 1 || commitIDs[0] != newHead {
		t.Fatalf("Host Verdict/Checks commit ID = %v, want %s", commitIDs, newHead)
	}
	if _, found, err := readRetirement(repo, branch, oldHead); err != nil || found {
		t.Fatalf("pre-rebase preparation = (found=%v, err=%v), want migrated away", found, err)
	}
	fact, found, err := readRetirement(repo, branch, newHead)
	if err != nil || !found || fact.Record.State != "pending" {
		t.Fatalf("post-rebase preparation = (%#v, found=%v, err=%v), want pending fact", fact, found, err)
	}
}

// TestVerifierActProjectsChecksAtPostRebaseHead drives the full Verifier Act
// failure path and pins the Host Checks comment to the rebased Revision.
func TestVerifierActProjectsChecksAtPostRebaseHead(t *testing.T) {
	branch := "forest/55-host-checks"
	repo, _, oldHead, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	rebaseTestWriteFile(t, filepath.Join(repo, "master.txt"), "master\n")
	runGitTest(t, repo, "add", "master.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "master work")
	runGitTest(t, repo, "push", "-q", "origin", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")

	item := Item{ID: "55", Title: "Host Checks"}
	tk := newMemoryTracker()
	tk.seed(item)
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	created := false
	var commitIDs []string
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "pr" && args[1] == "list":
			if !created {
				return []byte(`[]`), nil
			}
			head := remoteBranchHead(t, repo, branch)
			return []byte(`[{"number":55,"url":"https://github.com/owner/repo/pull/55","headRefOid":"` + head +
				`","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		case args[0] == "api" && hasArgumentPair(args, "--method", "GET"):
			return []byte(`[[]]`), nil
		case args[0] == "pr" && args[1] == "create":
			created = true
			return []byte("https://github.com/owner/repo/pull/55"), nil
		case args[0] == "api" && hasArgumentPair(args, "--method", "POST"):
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--field" && strings.HasPrefix(args[i+1], "commit_id=") {
					commitIDs = append(commitIDs, strings.TrimPrefix(args[i+1], "commit_id="))
				}
			}
			return nil, nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "failing", Run: "false"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: subjectBranch, Revision: oldHead,
		ID: item.ID, Branch: branch}, "host-checks")
	if err != nil || out.Status != "checks_failed" {
		t.Fatalf("Host Checks Verifier Act = (%#v, %v), want checks_failed", out, err)
	}
	newHead := remoteBranchHead(t, repo, branch)
	if newHead == oldHead || out.BaseSHA != newHead {
		t.Fatalf("Host Checks heads = (old=%s, new=%s, outcome=%s), want post-rebase outcome", oldHead, newHead, out.BaseSHA)
	}
	if len(commitIDs) != 1 || commitIDs[0] != newHead {
		t.Fatalf("Host Checks commit ID = %v, want %s", commitIDs, newHead)
	}
	fact, found, err := readRetirement(repo, branch, newHead)
	if err != nil || !found || fact.Record.State != "preparing" {
		t.Fatalf("failed Checks preparation = (%#v, found=%v, err=%v), want retained", fact, found, err)
	}
	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement {
		t.Fatalf("failed Checks Verifier Select = (%#v, %v), want retained retirement", subjects, err)
	}
	retry, err := (verifierFlow{}).Act(cfg, repo, subjects[0], "host-checks-retry")
	if err != nil || retry.Status != "checks_failed" || len(commitIDs) != 1 {
		t.Fatalf("failed Checks retry = (%#v, comments=%v, err=%v), want no repeated work", retry, commitIDs, err)
	}
	if subjects, err := (fixerFlow{}).Select(cfg, repo); err != nil ||
		len(subjects) != 1 || subjects[0].Branch != branch {
		t.Fatalf("failed Checks Fixer Select = (%#v, %v), want repair branch", subjects, err)
	}
}

func TestVerifierRecoversBuilderCommentBeforeAgentWork(t *testing.T) {
	repo, branch, head := fixerBranch(t)
	runGitTest(t, repo, "checkout", "-q", "master")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "9", Title: "comment recovery"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection.Enabled = true
	cfg.Projection.MergeViaHost = true
	out, err := (verifierFlow{}).Act(cfg, repo, Subject{
		Key: "branch-" + branch, Kind: subjectBranch, Revision: head,
		Label: branch, ID: "9", Branch: branch,
	}, "comment-recovery")
	if err == nil || out.Status != "agent_failed" {
		t.Fatalf("Verifier after comment recovery = (%#v, %v), want later agent failure", out, err)
	}
	item, getErr := tk.Get("9")
	if getErr != nil || len(item.Comments) != 1 ||
		!strings.Contains(item.Comments[0].Body, "iron-forest:built revision="+head) {
		t.Fatalf("recovered Builder comment = (%#v, %v), want exact Revision marker", item.Comments, getErr)
	}
}

func TestVerifierRetirementRecoversBuilderCommentAfterPreparationStop(t *testing.T) {
	repo, branch, head := fixerBranch(t)
	runGitTest(t, repo, "checkout", "-q", "master")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "9", Title: "comment recovery"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection.Enabled = true
	cfg.Projection.MergeViaHost = true
	item, err := tk.Get("9")
	if err != nil {
		t.Fatal(err)
	}
	fact, err := recordPreparingHostRetirement(cfg, repo, branch, head, item)
	if err != nil {
		t.Fatal(err)
	}
	legacy := fact.Record
	legacy.BuiltComment = false
	if _, err := replaceRetirement(repo, fact, legacy); err != nil {
		t.Fatal(err)
	}

	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement {
		t.Fatalf("restart selection = (%#v, %v), want one retirement recovery", subjects, err)
	}
	out, actErr := (verifierFlow{}).Act(cfg, repo, subjects[0], "retirement-comment-recovery")
	if actErr == nil || out.Status != "agent_failed" {
		t.Fatalf("retirement recovery = (%#v, %v), want comment before later agent failure", out, actErr)
	}
	got, err := tk.Get("9")
	if err != nil || len(got.Comments) != 1 ||
		!strings.Contains(got.Comments[0].Body, "iron-forest:built revision="+head) {
		t.Fatalf("retirement recovered comment = (%#v, %v), want exact marker", got.Comments, err)
	}
}

func TestMergedHostRecoveryPersistsObservationBeforeComment(t *testing.T) {
	branch := "forest/9-observed-before-comment"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	tk := &invisibleCommentTracker{memoryTracker: newMemoryTracker()}
	item := Item{ID: "9", Title: "observed recovery"}
	tk.seed(item)
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}

	out, err := recoverHostMergedProjection(
		cfg, repo, branch, reviewed, item,
		Outcome{Branch: branch, BaseSHA: reviewed},
	)
	if err == nil || out.Status != "comment_failed" {
		t.Fatalf("merged recovery = (%#v, %v), want comment failure after observation", out, err)
	}
	fact, found, readErr := readRetirement(repo, branch, reviewed)
	if readErr != nil || !found || fact.Record.State != "observed" ||
		fact.Record.BuiltComment {
		t.Fatalf("durable observation = (%#v, found=%v, err=%v), want uncompleted observed fact",
			fact, found, readErr)
	}
	if err := deleteRef(repo, "refs/heads/"+branch, reviewed); err != nil {
		t.Fatal(err)
	}
	subjects, err := (builderFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 0 {
		t.Fatalf("Builder after lost branch and comment = (%#v, %v), want retirement coverage", subjects, err)
	}
}

func TestMalformedStallDoesNotSuppressHealthyVerifierBranch(t *testing.T) {
	_, repo, revision := notesTestRepository(t)
	badBranch := "forest/31-bad-stall"
	goodBranch := "forest/32-good"
	runGitTest(t, repo, "push", "-q", "origin",
		revision+":refs/heads/"+badBranch,
		revision+":refs/heads/"+goodBranch)
	ref := stalledRef("verifier", "branch-"+badBranch)
	blob, err := writeBlob(repo, "{malformed")
	if err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "push", "-q", "origin", blob+":"+ref)

	subjects, err := (verifierFlow{}).Select(defaultConfig(), repo)
	if err != nil {
		t.Fatal(err)
	}
	badFailures := 0
	goodHealthy := 0
	for _, subject := range subjects {
		if subject.Branch == badBranch && subject.Failure != nil {
			badFailures++
		}
		if subject.Branch == goodBranch && subject.Failure == nil {
			goodHealthy++
		}
	}
	if badFailures != 1 || goodHealthy != 1 {
		t.Fatalf("Verifier Subjects = %#v, want one quarantined brake and one healthy branch", subjects)
	}
}
