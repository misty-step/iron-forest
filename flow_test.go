package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// failingFlow is a Flow stub whose Act always fails like a crashed agent. Tests
// use it to drive actOnSubject without talking to a real tracker or opencode.
type failingFlow struct{}

func (failingFlow) Name() string                             { return "builder" }
func (failingFlow) Select(Config, string) ([]Subject, error) { return aSubject, nil }
func (failingFlow) Interval(Config) time.Duration            { return 0 }
func (failingFlow) Enabled(Config) bool                      { return true }
func (failingFlow) Act(Config, string, Subject, string) (Outcome, error) {
	return Outcome{TokIn: 7}, errAgentCrash
}

var errAgentCrash = errors.New("agent: agent exited \"signal: terminated\"")

// aSubject is the sole subject a failingFlow selects.
var aSubject = []Subject{{Key: "item-1", Revision: "rev-1", ID: "1"}}

// TestShutdownIsNotAnAgentFailure pins the operator-shutdown card: a run that
// ends while the daemon is draining records a distinct shutdown status, keeps
// the tokens spent, and never increments the repeat-failure brake.
func TestShutdownIsNotAnAgentFailure(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	var drain int32 = 1
	for range stalledRunLimit + 1 {
		if code := actOnSubject(failingFlow{}, Config{}, repo, aSubject[0], &drain); code != 1 {
			t.Fatalf("draining failing run code = %d, want 1", code)
		}
	}
	rows, _, err := loadLedger(ledgerPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	last := rows[len(rows)-1]
	if last.Status != shutdownStatus {
		t.Errorf("draining run status = %q, want %q", last.Status, shutdownStatus)
	}
	if last.TokensIn != 7 {
		t.Errorf("draining run tokensIn = %d, want 7 (spend kept)", last.TokensIn)
	}
	stalled, err := stalledOn(repo, "builder", aSubject[0].Key, aSubject[0].Revision)
	if err != nil {
		t.Fatal(err)
	}
	if stalled {
		t.Fatalf("shutdown reached the failure brake; it must not count")
	}
}

// TestAgentFailureStillCounts pins the other side of the boundary: a real
// non-zero agent exit, with no shutdown in progress, still records agent_failed
// and still drives the repeat-failure brake.
func TestAgentFailureStillCounts(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	for range stalledRunLimit {
		if code := actOnSubject(failingFlow{}, Config{}, repo, aSubject[0], nil); code != 1 {
			t.Fatalf("failing run code = %d, want 1", code)
		}
	}
	rows, _, err := loadLedger(ledgerPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	if rows[len(rows)-1].Status != "agent_failed" {
		t.Errorf("real failure status = %q, want agent_failed", rows[len(rows)-1].Status)
	}
	stalled, err := stalledOn(repo, "builder", aSubject[0].Key, aSubject[0].Revision)
	if err != nil {
		t.Fatal(err)
	}
	if !stalled {
		t.Fatalf("real failures did not reach the brake; they must count")
	}
}

// TestClaimKeyNormalizesAcrossFlows pins the no-double-claim invariant across
// lanes. The Builder reserves a subject as "item-<id>" while the Verifier and
// Fixer reserve the same work as "branch-<branch>"; without normalization these
// are different keys, so a Builder that has just published could hold "item-9"
// while another Flow claims "branch-forest/9-x" for the same Subject. claimKey
// reduces every subject to its opaque item id, so both reservations collide and
// the in-process guard refuses the second claim.
func TestClaimKeyNormalizesAcrossFlows(t *testing.T) {
	builder := Subject{Key: "item-9", Kind: "item", ID: "9"}
	verifier := Subject{Key: "branch-forest/9-add", Kind: "branch", ID: "9", Branch: "forest/9-add"}
	if claimKey(builder) != claimKey(verifier) {
		t.Fatalf("claimKey(builder)=%q claimKey(verifier)=%q; same item must share one claim", claimKey(builder), claimKey(verifier))
	}
	if got := claimKey(verifier); got != "item-9" {
		t.Fatalf("claimKey(verifier) = %q, want item-9", got)
	}

	// The real reservation path honors the normalized key: a second subject for
	// the same item is refused once one is in flight.
	set := newSubjectSet()
	if !set.claim(claimKey(builder)) {
		t.Fatal("first claim of the item was refused")
	}
	if set.claim(claimKey(verifier)) {
		t.Fatal("second claim of the same item as a different key was accepted; double-claim")
	}
	set.release(claimKey(builder))
	if !set.claim(claimKey(verifier)) {
		t.Fatal("claim after release was refused")
	}
}

// seedBuilderAgent writes a minimal agents/builder declaration into a test repo
// so builderFlow.Act and fixerFlow.Act can loadAgent and (if no report.schema.json
// is present) pass the gate on a stub-written report.json.
func seedBuilderAgent(t *testing.T, repo string) {
	t.Helper()
	dir := filepath.Join(repo, DefaultAgentsDir, "builder")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	agent := "harness: opencode\nmodel: openrouter-mint/deepseek-v4-flash-0731\nmode: primary\n"
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(agent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("build it\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubRunProduces writes the minimal artifacts a real phase would, so the gate
// passes afterward: a real change file and a report.json that satisfies the
// (absent) declared schema.
func stubRunProduces(wtDir string) error {
	if err := os.WriteFile(filepath.Join(wtDir, "change.txt"), []byte("work\n"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(wtDir, "report.json"), []byte(
		`{"summary":"done","changed_files":["change.txt"],"notes":"none"}`), 0o644)
}

// TestBuilderPublishGuardBlocksLabelLandedDuringRun is the race-focused falsifier
// for the Builder publish boundary. The build is legal when Act starts, but a
// forest:failed label lands while runPhase is already in flight. failed is
// terminal and no effect leaves it, so the durable publish must be refused: no
// new branch head may be pushed on a now-failed Subject. Before this guard Act
// checked the tracker only at entry and then published whatever the run produced.
func TestBuilderPublishGuardBlocksLabelLandedDuringRun(t *testing.T) {
	_, work, _ := notesTestRepository(t)
	seedBuilderAgent(t, work)

	mem := newMemoryTracker()
	mem.seed(Item{ID: "9", Title: "change", UpdatedAt: "r"})
	oldTracker := trackerFor
	trackerFor = func(repo string) Tracker { return mem }
	defer func() { trackerFor = oldTracker }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"

	subjects, err := (builderFlow{}).Select(cfg, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 {
		t.Fatalf("builder selected %d subjects, want the eligible item", len(subjects))
	}
	s := subjects[0]

	oldRun := runPhase
	var phaseRan bool
	runPhase = func(repoDir, wtDir string, a *Agent, userPrompt, tracePath string) (runStats, error) {
		phaseRan = true
		// The label lands while the build is in flight.
		if err := mem.SetTags("9", []string{failedLabel}, nil); err != nil {
			return runStats{}, err
		}
		if err := stubRunProduces(wtDir); err != nil {
			return runStats{}, err
		}
		return runStats{}, nil
	}
	defer func() { runPhase = oldRun }()

	out, err := (builderFlow{}).Act(cfg, work, s, "run-1")
	if !phaseRan {
		t.Fatal("runPhase stub never ran; the race was not exercised")
	}
	if err == nil {
		t.Fatalf("Builder published a head on a failed Subject: %#v", out)
	}
	if out.Status != "item_failed" {
		t.Fatalf("Builder status = %q, want item_failed", out.Status)
	}
	branches, err := forestBranches(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 0 {
		t.Fatalf("Builder pushed a branch on a failed Subject: %v", branches)
	}
}

// TestFixerPublishGuardBlocksCapSpentDuringRun is the race-focused falsifier
// for the Fixer publish boundary. The fix is legal when Act starts (attempts
// below the cap), but the attempt ref reaches the cap while runPhase is in
// flight. A fresh branch head must not be published once the subject is failed,
// so the durable push is refused and the branch tip stays put.
func TestFixerPublishGuardBlocksCapSpentDuringRun(t *testing.T) {
	_, work, _ := notesTestRepository(t)
	seedBuilderAgent(t, work)

	const branch = "forest/9-test"
	notesTestGit(t, work, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, "seed.txt"), []byte("broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "add", "seed.txt")
	notesTestGit(t, work, "commit", "-qm", "broken work")
	notesTestGit(t, work, "push", "-q", "-u", "origin", branch)
	head := notesTestGitOutput(t, work, "rev-parse", "HEAD")
	notesTestGit(t, work, "checkout", "-q", "master")
	if err := writeChecks(work, head, checksNote{
		Status: "fail", RunID: "seed", Time: nowRFC(),
		Results: []checkResult{{Name: "c", Code: 1, Output: "no"}},
	}); err != nil {
		t.Fatal(err)
	}

	mem := newMemoryTracker()
	mem.seed(Item{ID: "9", Title: "test", UpdatedAt: "r"})
	oldTracker := trackerFor
	trackerFor = func(repo string) Tracker { return mem }
	defer func() { trackerFor = oldTracker }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Flows.Fixer.Attempts = 1 // one fix is the whole cap

	oldRun := runPhase
	var phaseRan bool
	runPhase = func(repoDir, wtDir string, a *Agent, userPrompt, tracePath string) (runStats, error) {
		phaseRan = true
		// A concurrent Fixer (or config change) spends the cap mid-run.
		if _, err := bumpAttempts(repoDir, "branch-"+branch); err != nil {
			return runStats{}, err
		}
		if err := stubRunProduces(wtDir); err != nil {
			return runStats{}, err
		}
		return runStats{}, nil
	}
	defer func() { runPhase = oldRun }()

	s := Subject{
		Key: "branch-" + branch, Kind: "branch", Revision: head,
		Label: branch, ID: "9", Branch: branch, Head: head,
	}
	out, err := (fixerFlow{}).Act(cfg, work, s, "run-1")
	if !phaseRan {
		t.Fatal("runPhase stub never ran; the race was not exercised")
	}
	if err == nil {
		t.Fatalf("Fixer published a head on a cap-exhausted Subject: %#v", out)
	}
	if out.Status != "item_failed" {
		t.Fatalf("Fixer status = %q, want item_failed", out.Status)
	}
	if got := remoteBranchHead(t, work, branch); got != head {
		t.Fatalf("Fixer moved the branch %s -> %s despite a spent cap; want %s", head, got, head)
	}
}

// TestFixerPublishProceedsWhenCapNotSpent is the positive control for the guard:
// with the cap not reached mid-run, the identical pipeline publishes a bare head
// and reports fixed. It pins the guard as a real race check, not a blanket block.
func TestFixerPublishProceedsWhenCapNotSpent(t *testing.T) {
	_, work, _ := notesTestRepository(t)
	seedBuilderAgent(t, work)

	const branch = "forest/9-ok"
	notesTestGit(t, work, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, "seed.txt"), []byte("broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "add", "seed.txt")
	notesTestGit(t, work, "commit", "-qm", "broken work")
	notesTestGit(t, work, "push", "-q", "-u", "origin", branch)
	head := notesTestGitOutput(t, work, "rev-parse", "HEAD")
	notesTestGit(t, work, "checkout", "-q", "master")
	if err := writeChecks(work, head, checksNote{
		Status: "fail", RunID: "seed", Time: nowRFC(),
		Results: []checkResult{{Name: "c", Code: 1, Output: "no"}},
	}); err != nil {
		t.Fatal(err)
	}

	mem := newMemoryTracker()
	mem.seed(Item{ID: "9", Title: "ok", UpdatedAt: "r"})
	oldTracker := trackerFor
	trackerFor = func(repo string) Tracker { return mem }
	defer func() { trackerFor = oldTracker }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Flows.Fixer.Attempts = 5 // room for many repairs

	oldRun := runPhase
	runPhase = func(repoDir, wtDir string, a *Agent, userPrompt, tracePath string) (runStats, error) {
		if err := stubRunProduces(wtDir); err != nil {
			return runStats{}, err
		}
		return runStats{}, nil
	}
	defer func() { runPhase = oldRun }()

	s := Subject{
		Key: "branch-" + branch, Kind: "branch", Revision: head,
		Label: branch, ID: "9", Branch: branch, Head: head,
	}
	out, err := (fixerFlow{}).Act(cfg, work, s, "run-1")
	if err != nil {
		t.Fatalf("Fixer refused a lawful repair: %v", err)
	}
	if out.Status != "fixed" {
		t.Fatalf("Fixer status = %q, want fixed", out.Status)
	}
	if got := remoteBranchHead(t, work, branch); got == head {
		t.Fatalf("Fixer did not publish a new head (%s)", got)
	}
}
