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
