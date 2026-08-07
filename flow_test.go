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
