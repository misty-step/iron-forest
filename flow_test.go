package main

import (
	"errors"
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
