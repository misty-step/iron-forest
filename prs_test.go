package main

import (
	"path/filepath"
	"testing"
)

// TestFixPRStallsAtLimit pins the reaction bound: fixPR must refuse to
// re-enter a PR whose recorded fix count already consumed the configured
// budget, parking it as stalled instead of paying for another attempt. The
// bound check runs before any gh or agent work, so this stays offline.
func TestFixPRStallsAtLimit(t *testing.T) {
	repoDir := t.TempDir()
	cfg := defaultConfig()
	cfg.Workflow.MaxReactionFixes = 2
	op := pulledPR{PR: 41, URL: "https://example.test/pr/41", Branch: "forest/41-fix"}
	prior := prState{PR: 41, Issue: 7, State: "fixing", Fixes: 2}

	if code := fixPR(cfg, repoDir, op, prior, "feedback"); code == 0 {
		t.Fatal("fixPR reported success for an already-exhausted fix budget")
	}
	last, ok := lastPRState(filepath.Join(repoDir, WorkspaceDir), 41)
	if !ok {
		t.Fatal("no PR state row was written for the stalled PR")
	}
	if last.State != "stalled" {
		t.Fatalf("state = %q, want stalled", last.State)
	}
	if last.Fixes != 2 {
		t.Fatalf("fixes = %d, want 2 (the count must not change once parked)", last.Fixes)
	}
	if last.Error != "too many fix cycles" {
		t.Fatalf("error = %q, want %q", last.Error, "too many fix cycles")
	}
}

// TestFixLimitReachedHonorsConfig pins the bound to cfg.Workflow.MaxReactionFixes
// instead of the removed hardcoded default of 2: a configured budget of 5 must
// admit four fixes and reject the fifth.
func TestFixLimitReachedHonorsConfig(t *testing.T) {
	cfg := defaultConfig()
	cfg.Workflow.MaxReactionFixes = 5
	for fixes := 0; fixes < 5; fixes++ {
		if fixLimitReached(cfg, prState{Fixes: fixes}) {
			t.Fatalf("fixes=%d: a budget of 5 must still admit a fix", fixes)
		}
	}
	if !fixLimitReached(cfg, prState{Fixes: 5}) {
		t.Fatal("fixes=5: a budget of 5 must be exhausted")
	}
	if !fixLimitReached(cfg, prState{Fixes: 6}) {
		t.Fatal("fixes=6: past the budget must be exhausted")
	}
}

// TestRecordFixFailureCountsAndPersists pins the failed-fix path: an errored
// runPick must increment the recorded fix count and write a PR state row, so
// the loop cannot retry the same failure without the ledger noticing.
func TestRecordFixFailureCountsAndPersists(t *testing.T) {
	workspace := t.TempDir()
	cfg := defaultConfig()
	cfg.Workflow.MaxReactionFixes = 3
	prior := prState{PR: 41, Issue: 7, State: "opened", Fixes: 1}

	next, stalled := recordFixFailure(cfg, workspace, prior, "agent: gummed up")
	if stalled {
		t.Fatal("fixes=1 -> 2 under a budget of 3 must not stall the PR")
	}
	if next.Fixes != 2 {
		t.Fatalf("fixes = %d, want 2", next.Fixes)
	}
	if next.State != "fixing" {
		t.Fatalf("state = %q, want fixing while the budget remains", next.State)
	}
	if next.Error != "agent: gummed up" {
		t.Fatalf("error = %q, want %q", next.Error, "agent: gummed up")
	}
	last, ok := lastPRState(workspace, 41)
	if !ok {
		t.Fatal("no PR state row was persisted for the failed fix")
	}
	if last.Fixes != 2 || last.Error != "agent: gummed up" || last.State != "fixing" {
		t.Fatalf("persisted row = %+v, want fixes=2 error=%q state=fixing", last, "agent: gummed up")
	}
}

// TestRollupChecksPinsConclusions guards the merge gate's conclusion mapping:
// a run that is still pending, or completed with an empty conclusion, must roll
// up to pending — never pass — so an automatic merge cannot be unlocked by a
// check that never decided.
func TestRollupChecksPinsConclusions(t *testing.T) {
	tests := []struct {
		name string
		runs []checkRun
		want string
	}{
		{"no checks", nil, ""},
		{"passing ci", []checkRun{{Status: "completed", Conclusion: "success", Name: "ci"}}, "pass"},
		{"neutral and skipped count as pass", []checkRun{
			{Status: "completed", Conclusion: "neutral", Name: "ci"},
			{Status: "completed", Conclusion: "skipped", Name: "test"},
		}, "pass"},
		{"failed ci", []checkRun{{Status: "completed", Conclusion: "failure", Name: "ci"}}, "fail"},
		{"pending ci", []checkRun{
			{Status: "queued", Name: "ci"},
			{Status: "completed", Conclusion: "success", Name: "ci"},
		}, "pending"},
		{"completed with empty conclusion", []checkRun{{Status: "completed", Conclusion: "", Name: "ci"}}, "pending"},
		{"empty conclusion cannot pass next to a green check", []checkRun{
			{Status: "completed", Conclusion: "success", Name: "ci"},
			{Status: "completed", Conclusion: "", Name: "test"},
		}, "pending"},
		{"non-ci bot only", []checkRun{{Status: "completed", Conclusion: "success", Name: "CodeRabbit"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rollupChecks(tt.runs); got != tt.want {
				t.Fatalf("rollupChecks(%+v) = %q, want %q", tt.runs, got, tt.want)
			}
		})
	}
}

// TestRecordFixFailureStallsAtLimit pins the other end of the failed-fix path:
// when the increment exhausts the budget the PR is parked in the ledger.
func TestRecordFixFailureStallsAtLimit(t *testing.T) {
	workspace := t.TempDir()
	cfg := defaultConfig()
	cfg.Workflow.MaxReactionFixes = 2
	prior := prState{PR: 41, Issue: 7, State: "fixing", Fixes: 1}

	next, stalled := recordFixFailure(cfg, workspace, prior, "gate: lint broke")
	if !stalled {
		t.Fatal("fixes=1 -> 2 with a budget of 2 must stall the PR")
	}
	if next.State != "stalled" || next.Fixes != 2 {
		t.Fatalf("row = %+v, want state=stalled fixes=2", next)
	}
	last, ok := lastPRState(workspace, 41)
	if !ok || last.State != "stalled" || last.Fixes != 2 {
		t.Fatalf("persisted row = %+v ok=%v, want stalled fixes=2", last, ok)
	}
}
