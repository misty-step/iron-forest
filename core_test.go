package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/misty-step/iron-forest/core"
)

// coreFixture builds a checkout on a local remote with a valid forest.yaml and
// returns the API plus the checkout dir and target commit sha.
func coreFixture(t *testing.T) (core.API, string, string) {
	t.Helper()
	_, work, sha := notesTestRepository(t)
	if err := os.WriteFile(filepath.Join(work, "forest.yaml"), []byte(
		"repo: owner/repo\n"+
			"checks:\n"+
			"  - name: test\n"+
			"    run: \"true\"\n"+
			"flows:\n"+
			"  builder:\n"+
			"    enabled: true\n"+
			"    agent: builder\n"+
			"    interval_seconds: 30\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "add", "forest.yaml")
	notesTestGit(t, work, "commit", "-m", "config")
	notesTestGit(t, work, "push", "-q", "origin", "HEAD:master")
	return NewCore(work), work, sha
}

func TestCoreConfigReadsForestYAML(t *testing.T) {
	api, _, _ := coreFixture(t)
	cfg, err := api.Config()
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.Repo != "owner/repo" {
		t.Fatalf("Config repo = %q, want owner/repo", cfg.Repo)
	}
	if len(cfg.Checks) != 1 || cfg.Checks[0].Name != "test" {
		t.Fatalf("Config checks = %+v, want one test check", cfg.Checks)
	}
}

func TestCoreAgentsListsDeclaredAgents(t *testing.T) {
	api, work, _ := coreFixture(t)
	dir := filepath.Join(work, DefaultAgentsDir, "builder")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"),
		[]byte("description: test agent\nmodel: test-model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"),
		[]byte("Be helpful.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agents, err := api.Agents()
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("Agents returned %d, want 1", len(agents))
	}
	if agents[0].Name != "builder" || agents[0].Model != "test-model" {
		t.Fatalf("agent = %+v, want name=builder model=test-model", agents[0])
	}
	if agents[0].DefSHA == "" {
		t.Fatalf("agent def_sha is empty, want a digest")
	}
}

func TestCoreLedgerReadsRowsAndFiltersByFlow(t *testing.T) {
	api, work, _ := coreFixture(t)
	ws := filepath.Join(work, WorkspaceDir, "runs.jsonl")
	if err := os.MkdirAll(filepath.Dir(ws), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := "{\"time\":\"2026-01-01T00:00:00Z\",\"run_id\":\"r1\",\"flow\":\"builder\",\"subject\":\"item-1\",\"revision\":\"a\",\"status\":\"built\",\"tokens_in\":10,\"tokens_out\":2}\n" +
		"{\"time\":\"2026-01-01T00:00:01Z\",\"run_id\":\"r2\",\"flow\":\"verifier\",\"subject\":\"item-2\",\"revision\":\"b\",\"status\":\"merged\",\"tokens_in\":5,\"tokens_out\":1}\n"
	if err := os.WriteFile(ws, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	all, err := api.Ledger(core.LedgerQuery{})
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("Ledger returned %d rows, want 2", len(all))
	}
	if all[0].Flow != "builder" || all[0].TokensIn != 10 {
		t.Fatalf("row 0 = %+v, want builder with 10 tokens in", all[0])
	}
	builder, err := api.Ledger(core.LedgerQuery{Flow: "builder"})
	if err != nil {
		t.Fatalf("Ledger(builder): %v", err)
	}
	if len(builder) != 1 || builder[0].RunID != "r1" {
		t.Fatalf("Ledger(builder) = %+v, want only r1", builder)
	}
}

func TestCoreTraceReturnsTheBytesTheHarnessWrote(t *testing.T) {
	api, work, _ := coreFixture(t)
	runsDir := filepath.Join(work, WorkspaceDir, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("{\"step\":1}\n{\"step\":2}\n")
	if err := os.WriteFile(filepath.Join(runsDir, "run-abc.builder.jsonl"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := api.Trace("run-abc")
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Trace = %q, want %q", got, want)
	}
	if _, err := api.Trace("missing-run"); err == nil {
		t.Fatal("Trace of an absent run must error")
	}
}

func TestCoreNotesReadsVerdictAndChecks(t *testing.T) {
	api, work, sha := coreFixture(t)
	if v, c, err := api.Notes(sha); err != nil || v.Verdict != "" || c.Status != "" {
		t.Fatalf("absent notes = (%+v, %+v, %v), want empty without error", v, c, err)
	}
	if err := writeVerdict(work, sha, verdictNote{
		Verdict: "approve", Notes: "looks good", Reviewer: "reviewer-a",
		Model: "model-a", DefSHA: "def-a", RunID: "run-a",
	}); err != nil {
		t.Fatalf("writeVerdict: %v", err)
	}
	if err := writeChecks(work, sha, checksNote{
		Status:  "pass",
		Results: []checkResult{{Name: "test", Code: 0, Output: "ok"}},
		RunID:   "run-checks",
	}); err != nil {
		t.Fatalf("writeChecks: %v", err)
	}
	v, c, err := api.Notes(sha)
	if err != nil {
		t.Fatalf("Notes: %v", err)
	}
	if v.Verdict != "approve" || v.Reviewer != "reviewer-a" || v.RunID != "run-a" {
		t.Fatalf("verdict = %+v, want approve from reviewer-a", v)
	}
	if c.Status != "pass" || len(c.Results) != 1 || c.Results[0].Name != "test" {
		t.Fatalf("checks = %+v, want one passing test result", c)
	}
}

func TestCoreItemsReturnsBuilderSelectorBacklog(t *testing.T) {
	old := trackerFor
	trackerFor = func(repo string) Tracker {
		return trackerStub{items: []Item{{ID: "hab_01J9X", Title: "opaque", UpdatedAt: "r"}}}
	}
	defer func() { trackerFor = old }()

	api, _, _ := coreFixture(t)
	items, err := api.Items()
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if len(items) != 1 || items[0].ID != "hab_01J9X" {
		t.Fatalf("Items = %+v, want the opaque item", items)
	}
}

func TestCoreBranchesListsForestBranches(t *testing.T) {
	api, work, _ := coreFixture(t)
	notesTestGit(t, work, "checkout", "-q", "-b", "forest/7-example")
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "commit", "-qam", "branch work")
	notesTestGit(t, work, "push", "-q", "-u", "origin", "forest/7-example")
	notesTestGit(t, work, "checkout", "-q", "master")

	branches, err := api.Branches()
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}
	if len(branches) != 1 || branches[0].Name != "forest/7-example" {
		t.Fatalf("Branches = %+v, want forest/7-example", branches)
	}
	if branches[0].Head == "" {
		t.Fatalf("branch head is empty, want a sha")
	}
}

func TestCoreDaemonPresentWithNoDaemon(t *testing.T) {
	api, _, _ := coreFixture(t)
	up, err := api.DaemonPresent()
	if err != nil {
		t.Fatalf("DaemonPresent: %v", err)
	}
	if up {
		t.Fatal("DaemonPresent reported a daemon in a bare test environment")
	}
}
