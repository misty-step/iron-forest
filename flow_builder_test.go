package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuilderBlockedOnCredentialShapedReport is item #120's oracle at the flow
// boundary: a forced report.json carrying sk-AAAAAAAAAAAAAAAA must make the
// Builder record the run as blocked, so no branch is pushed and no pull request
// body reaches the host.
func TestBuilderBlockedOnCredentialShapedReport(t *testing.T) {
	repo := setupTestRepo(t)
	writeAgentFixture(t, repo, "builder", "builder-model")

	tk := newMemoryTracker()
	tk.seed(Item{ID: "9", Title: "secret change", UpdatedAt: "u1"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	oldRun := runPhase
	runPhase = func(_ string, wtDir string, _ *Agent, userPrompt, tracePath string) (runStats, error) {
		// The agent is a stub: it edits a file and writes a report whose summary
		// carries a credential-shaped token, exactly the forced report.json of the
		// oracle.
		if err := os.WriteFile(filepath.Join(wtDir, "file.txt"), []byte("secret change\n"), 0o644); err != nil {
			return runStats{}, err
		}
		rep := `{"summary":"fixed it, key is sk-AAAAAAAAAAAAAAAA","changed_files":["file.txt"],"notes":"none"}`
		if err := os.WriteFile(filepath.Join(wtDir, "report.json"), []byte(rep), 0o644); err != nil {
			return runStats{}, err
		}
		return runStats{}, nil
	}
	defer func() { runPhase = oldRun }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{}

	it := Item{ID: "9", Title: "secret change", UpdatedAt: "u1"}
	out, err := (builderFlow{}).Act(cfg, repo, Subject{
		Key: "item-9", Kind: "item", Revision: "u1", ID: "9", Item: it,
	}, "run-blocked")
	if err == nil {
		t.Fatalf("a credential-shaped report returned no error: %#v", out)
	}
	if out.Status != "blocked" {
		t.Fatalf("credential-shaped report status = %q, want blocked", out.Status)
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("blocked error did not name the block: %v", err)
	}
	// No branch for this run may reach the remote as a projection target.
	out2, perr := gitOut(repo, "ls-remote", "origin", "refs/heads/forest/*")
	if perr != nil {
		t.Fatal(perr)
	}
	if strings.TrimSpace(out2) != "" {
		t.Fatalf("a blocked run pushed branches:\n%s", out2)
	}
}

func TestBuilderCommitUsesDeclaredIdentity(t *testing.T) {
	repo := setupTestRepo(t)
	writeAgentFixture(t, repo, "builder", "builder-model")

	tk := newMemoryTracker()
	it := Item{ID: "9", Title: "identity change", UpdatedAt: "u1"}
	tk.seed(it)
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	oldRun := runPhase
	runPhase = func(_ string, wtDir string, _ *Agent, _, _ string) (runStats, error) {
		if err := os.WriteFile(filepath.Join(wtDir, "identity.txt"), []byte("agent\n"), 0o644); err != nil {
			return runStats{}, err
		}
		report := `{"summary":"add identity","changed_files":["identity.txt"],"notes":""}`
		if err := os.WriteFile(filepath.Join(wtDir, "report.json"), []byte(report), 0o644); err != nil {
			return runStats{}, err
		}
		return runStats{}, nil
	}
	defer func() { runPhase = oldRun }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{}
	out, err := (builderFlow{}).Act(cfg, repo, Subject{
		Key: "item-9", Kind: "item", Revision: "u1", ID: "9", Item: it,
	}, "run-identity")
	if err != nil {
		t.Fatalf("builder Act: %v", err)
	}
	if out.Status != "built" {
		t.Fatalf("builder status = %q, want built", out.Status)
	}
	head := remoteBranchHead(t, repo, out.Branch)
	author := runGitTest(t, repo, "show", "-s", "--format=%an <%ae>", head)
	if author != "builder <builder@example.invalid>" {
		t.Fatalf("builder commit author = %q, want declared identity", author)
	}
}

func TestBuilderActRevalidatesEligibilityAfterSelect(t *testing.T) {
	repo := setupTestRepo(t)
	tk := newMemoryTracker()
	tk.seed(Item{ID: "9", Title: "stale assignment", UpdatedAt: "u1", Tags: []string{readyTag}})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Flows.Builder.RequireLabels = []string{readyTag}
	cfg.Flows.Builder.ExcludeLabels = []string{"parked"}
	subjects, err := (builderFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 {
		t.Fatalf("initial Builder Select = (%#v, %v), want one Subject", subjects, err)
	}
	if err := tk.SetTags("9", []string{"parked"}, []string{readyTag}); err != nil {
		t.Fatal(err)
	}
	if code := actOnSubject(builderFlow{}, cfg, repo, subjects[0], nil); code != 0 {
		t.Fatalf("stale Builder Subject exit = %d, want success without agent spend", code)
	}
	runs, invalid, err := loadLedger(ledgerPath(repo))
	if err != nil || invalid != 0 || len(runs) != 1 || runs[0].Status != "stale" {
		t.Fatalf("stale Builder Ledger = (runs=%#v, invalid=%d, err=%v), want stale", runs, invalid, err)
	}
	if out := runGitTest(t, repo, "ls-remote", "origin", "refs/heads/forest/*"); out != "" {
		t.Fatalf("stale Builder Subject published a branch: %s", out)
	}
}
