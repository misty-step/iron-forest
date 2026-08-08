package main

import (
	"errors"
	"strings"
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

// TestBuilderPromptDeliveryFailureParksNotFixes drives #204's mechanical
// classification end to end. A prompt that cannot be delivered is a mechanical
// failure: the same prompt fails identically on every retry, so it must be
// named prompt_failed for an operator and must never become a Fixer subject
// that spends a repair attempt on an unchanged situation. The builder flow
// fails inside runPhase before it ever publishes a branch, so nothing on
// origin offers the head to the Fixer and the attempt counter stays at zero.
func TestBuilderPromptDeliveryFailureParksNotFixes(t *testing.T) {
	repo := setupTestRepo(t)
	writeAgentFixture(t, repo, "builder", "builder-model")

	tk := newMemoryTracker()
	tk.seed(Item{ID: "9", Title: "wide change", UpdatedAt: "u1"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	oldRun := runPhase
	runPhase = func(_ string, _ string, _ *Agent, userPrompt, tracePath string) (runStats, error) {
		return runStats{}, &promptDeliveryError{size: len(userPrompt), limit: maxArgLen}
	}
	defer func() { runPhase = oldRun }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{}

	it := Item{ID: "9", Title: "wide change", UpdatedAt: "u1"}
	out, err := (builderFlow{}).Act(cfg, repo, Subject{
		Key: "item-9", Kind: "item", Revision: "u1", ID: "9", Item: it,
	}, "run-prompt")
	if err == nil {
		t.Fatalf("a prompt-delivery failure returned no error: %#v", out)
	}
	if !isPromptDelivery(err) {
		t.Fatalf("error %v does not wrap a promptDeliveryError", err)
	}
	if out.Status != "prompt_failed" {
		t.Fatalf("prompt-delivery status = %q, want prompt_failed (mechanical)", out.Status)
	}

	// The mechanical failure must not enter the Fixer. Because the run never
	// published a branch, the Fixer has nothing to repair on origin, and no
	// repair attempt was spent on the subject.
	subjects, err := (fixerFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatalf("fixer Select: %v", err)
	}
	if len(subjects) != 0 {
		t.Fatalf("a prompt-delivery failure was offered to the Fixer: %#v", subjects)
	}
	if n, err := readAttempts(repo, "branch-forest/9-wide-change"); err != nil || n != 0 {
		t.Fatalf("fixer attempts = (%d, %v), want 0; a mechanical failure must not spend a repair attempt", n, err)
	}
}

// TestBuilderTimeoutFailureParksNotFixes drives #207's mechanical classification
// end to end. A run that exceeds its declared wall-clock deadline is a
// mechanical failure: the same run keeps exceeding the same declared bound, so
// it must be named timeout_failed for an operator — never treated as a rejected
// change — and must never become a Fixer subject that spends a repair attempt on
// an unchanged situation. The builder flow fails inside runPhase before it ever
// publishes a branch, so nothing on origin offers the head to the Fixer and the
// attempt counter stays at zero.
func TestBuilderTimeoutFailureParksNotFixes(t *testing.T) {
	repo := setupTestRepo(t)
	writeAgentFixture(t, repo, "builder", "builder-model")

	tk := newMemoryTracker()
	tk.seed(Item{ID: "9", Title: "wide change", UpdatedAt: "u1"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	oldRun := runPhase
	runPhase = func(_ string, _ string, _ *Agent, userPrompt, tracePath string) (runStats, error) {
		return runStats{}, &runTimeoutError{elapsed: 3 * time.Minute, lastEvent: "step_finish"}
	}
	defer func() { runPhase = oldRun }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{}

	it := Item{ID: "9", Title: "wide change", UpdatedAt: "u1"}
	out, err := (builderFlow{}).Act(cfg, repo, Subject{
		Key: "item-9", Kind: "item", Revision: "u1", ID: "9", Item: it,
	}, "run-timeout")
	if err == nil {
		t.Fatalf("a runaway run returned no error: %#v", out)
	}
	if !isRunTimeout(err) {
		t.Fatalf("error %v does not wrap a runTimeoutError", err)
	}
	if out.Status != "timeout_failed" {
		t.Fatalf("timeout status = %q, want timeout_failed (mechanical)", out.Status)
	}
	if !strings.Contains(err.Error(), "3m0s") || !strings.Contains(err.Error(), "step_finish") {
		t.Errorf("error %q did not name the elapsed time and last trace event", err)
	}

	// The mechanical failure must not enter the Fixer. Because the run never
	// published a branch, the Fixer has nothing to repair on origin, and no
	// repair attempt was spent on the subject.
	subjects, err := (fixerFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatalf("fixer Select: %v", err)
	}
	if len(subjects) != 0 {
		t.Fatalf("a timeout failure was offered to the Fixer: %#v", subjects)
	}
	if n, err := readAttempts(repo, "branch-forest/9-wide-change"); err != nil || n != 0 {
		t.Fatalf("fixer attempts = (%d, %v), want 0; a timeout must not spend a repair attempt", n, err)
	}
}
