package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// rebaseFixture builds a worktree-less git repo with origin/master on "base",
// a factory branch "forest/1-topic" cut at that base, and an origin/master that
// then advances by the given committing closure. It returns the repo and the
// branch name. Real git drives the fixture so the fetch/rebase/push paths have
// something live to operate on.
func rebaseFixture(t *testing.T, masterAhead func(t *testing.T, dir string)) (repo, branch string) {
	t.Helper()
	origin := t.TempDir()
	mustBare := exec.Command("git", "init", "--bare", "-b", "master", origin)
	if out, err := mustBare.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	repo = t.TempDir()
	must := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", name, err, strings.TrimSpace(string(out)))
		}
	}
	must("init", "init", "-b", "master")
	must("config", "config", "user.email", "test@example.com")
	must("config", "config", "user.name", "test")
	must("remote", "remote", "add", "origin", origin)
	must("commit", "commit", "--allow-empty", "-m", "base")
	must("push", "push", "-u", "origin", "master")
	// Cut the topic branch off the same base, with its own change.
	must("checkout", "checkout", "-b", "forest/1-topic")
	if err := os.WriteFile(filepath.Join(repo, "topic.txt"), []byte("branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	must("add", "add", "topic.txt")
	must("commit", "commit", "-m", "topic")
	must("push", "push", "-u", "origin", "forest/1-topic")
	// Advance master past the branch base.
	must("checkout", "checkout", "master")
	masterAhead(t, repo)
	must("add", "add", "-A")
	must("commit", "commit", "-m", "master ahead")
	must("push", "push", "origin", "master")
	return repo, "forest/1-topic"
}

// TestRebaseOntoMasterRebasesAndMerges pins the whole point of the change: a
// branch cut from an older master is rebased onto origin/master (and
// force-pushed) so the approved change still reaches master, and the new head
// carries master's newer commit instead of the stale base.
func TestRebaseOntoMasterRebasesAndMerges(t *testing.T) {
	repo, branch := rebaseFixture(t, func(t *testing.T, dir string) {
		if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("master\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	workspace := filepath.Join(repo, WorkspaceDir)

	rebased, err := rebaseOntoMaster(repo, workspace, branch)
	if err != nil {
		t.Fatalf("rebaseOntoMaster: %v", err)
	}
	if !rebased {
		t.Fatal("branch behind origin/master was not reported as rebased")
	}
	// The branch head rewritten on top of master: it must contain the newer
	// commit, so a merge onto master is now conflict-free.
	if err := git(repo, "merge-base", "--is-ancestor", "origin/master", "origin/"+branch); err != nil {
		t.Fatalf("origin/master not an ancestor of the rebased branch: %v", err)
	}
	if err := git(repo, "merge-base", "--is-ancestor", "origin/"+branch, "origin/master"); err == nil {
		t.Fatal("rebase lost the branch's own change: branch is an ancestor of origin/master")
	}
	// A squash merge onto master now applies cleanly.
	if err := git(repo, "merge", "--no-commit", "--squash", "origin/"+branch); err != nil {
		t.Fatalf("merge after rebase: %v", err)
	}
}

// TestRebaseOntoMasterConflictNamesPaths pins the merge-failure contract: a
// rebase that conflicts returns an error naming the conflicting paths, never a
// bare exit status, so the Verifier's merge-failure path can label the item.
func TestRebaseOntoMasterConflictNamesPaths(t *testing.T) {
	repo, branch := rebaseFixture(t, func(t *testing.T, dir string) {
		if err := os.WriteFile(filepath.Join(dir, "topic.txt"), []byte("master edit\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	workspace := filepath.Join(repo, WorkspaceDir)

	_, err := rebaseOntoMaster(repo, workspace, branch)
	if err == nil {
		t.Fatal("conflicting rebase reported success")
	}
	msg := err.Error()
	if !strings.Contains(msg, "topic.txt") {
		t.Fatalf("conflict error %q does not name the conflicting path topic.txt", msg)
	}
	if strings.Contains(msg, "exit status") {
		t.Fatalf("conflict error %q leaks a bare exit status", msg)
	}
	// The worktree is cleaned up and the remote branch is untouched.
	out, werr := gitOut(repo, "worktree", "list", "--porcelain")
	if werr != nil {
		t.Fatal(werr)
	}
	if strings.Contains(out, filepath.Join(workspace, "worktrees")) {
		t.Fatalf("merge worktree leaked after failed rebase")
	}
	if _, lerr := gitOut(repo, "rev-list", "--count", "origin/master..origin/"+branch); lerr != nil {
		t.Fatalf("remote branch lost after failed rebase: %v", lerr)
	}
}

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

// TestFixPRStalledDoesNotDuplicateRow pins the reconcile oracle on the stalled
// fixPR paths: when a re-entrant watchPR pass reconstructs the same prior state
// (same SHA, checks, fixes, owl, and error) and calls fixPR again, the stalled
// row must be routed through prCurrent so the ledger records one current row
// per PR instead of appending an identical row on every pass.
func TestFixPRStalledDoesNotDuplicateRow(t *testing.T) {
	repoDir := t.TempDir()
	cfg := defaultConfig()
	cfg.Workflow.MaxReactionFixes = 2
	op := pulledPR{PR: 41, URL: "https://example.test/pr/41", Branch: "forest/41-fix"}
	prior := prState{PR: 41, Issue: 7, Branch: op.Branch, PRURL: op.URL, State: "fixing", Fixes: 2}

	for i := 0; i < 3; i++ {
		if code := fixPR(cfg, repoDir, op, prior, "feedback"); code == 0 {
			t.Fatalf("call %d: fixPR reported success for an exhausted fix budget", i+1)
		}
	}
	if n := countPRStateRows(t, filepath.Join(repoDir, WorkspaceDir), 41); n != 1 {
		t.Fatalf("ledger holds %d current rows for PR 41 after repeated stalled re-entry, want 1", n)
	}
}

// countPRStateRows returns how many prs.jsonl rows reference the given PR.
func countPRStateRows(t *testing.T, workspace string, n int) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(workspace, "prs.jsonl"))
	if err != nil {
		t.Fatalf("read prs.jsonl: %v", err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var s prState
		if json.Unmarshal([]byte(line), &s) == nil && s.PR == n {
			count++
		}
	}
	return count
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
