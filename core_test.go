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
			"commit:\n"+
			"  name: forest\n"+
			"  email: forest@invalid\n"+
			"checks:\n"+
			"  - name: test\n"+
			"    run: \"true\"\n"+
			"flows:\n"+
			"  builder:\n"+
			"    enabled: true\n"+
			"    agent: builder\n"+
			"    interval_seconds: 30\n"+
			"    exclude_labels: [parked]\n"+
			"  verifier:\n"+
			"    enabled: true\n"+
			"    agent: verifier\n"+
			"    interval_seconds: 20\n"+
			"    merge: squash\n"+
			"    auto_merge: false\n"+
			"  fixer:\n"+
			"    enabled: true\n"+
			"    agent: builder\n"+
			"    interval_seconds: 40\n"+
			"    attempts: 5\n"+
			"projection:\n"+
			"  enabled: true\n"+
			"  merge_via_host: false\n",
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
	if cfg.Commit.Name != "forest" || cfg.Commit.Email != "forest@invalid" {
		t.Fatalf("Config commit = %+v, want forest/forest@invalid", cfg.Commit)
	}
	if len(cfg.Flows.Builder.ExcludeLabels) != 1 || cfg.Flows.Builder.ExcludeLabels[0] != "parked" {
		t.Fatalf("Config builder labels = %v, want [parked]", cfg.Flows.Builder.ExcludeLabels)
	}
	if cfg.Flows.Verifier.Merge != "squash" || cfg.Flows.Verifier.AutoMerge {
		t.Fatalf("Config verifier = %+v, want squash no automerge", cfg.Flows.Verifier)
	}
	if cfg.Flows.Fixer.Attempts != 5 {
		t.Fatalf("Config fixer attempts = %d, want 5", cfg.Flows.Fixer.Attempts)
	}
	if !cfg.Projection.Enabled || cfg.Projection.MergeViaHost {
		t.Fatalf("Config projection = %+v, want enabled without merge_via_host", cfg.Projection)
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

// TestCoreAgentsUsesDirectoryNameNotDeclarationName pins the #176 regression:
// coreImpl.Agents reports the discovered directory name even when the agent's
// declaration carries its own `name:` field. The agents command always printed
// the directory name, and Agent.Name is not yaml:"-", so a `name:` in agent.yaml
// must not change a surface's output.
func TestCoreAgentsUsesDirectoryNameNotDeclarationName(t *testing.T) {
	api, work, _ := coreFixture(t)
	dir := filepath.Join(work, DefaultAgentsDir, "builder")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"),
		[]byte("name: renamed-in-yaml\ndescription: test agent\nmodel: test-model\n"), 0o644); err != nil {
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
	if agents[0].Name != "builder" {
		t.Fatalf("agent name = %q, want the discovered directory name %q", agents[0].Name, "builder")
	}
}

func TestCoreLedgerReadsRowsAndFiltersByFlow(t *testing.T) {
	api, work, _ := coreFixture(t)
	ws := filepath.Join(work, WorkspaceDir, "runs.jsonl")
	if err := os.MkdirAll(filepath.Dir(ws), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := "{\"time\":\"2026-01-01T00:00:00Z\",\"run_id\":\"r1\",\"flow\":\"builder\",\"subject\":\"item-1\",\"revision\":\"a\",\"status\":\"built\",\"tokens_in\":10,\"tokens_out\":2,\"cache_read\":30,\"cache_write\":4,\"reasoning\":5}\n" +
		"{\"time\":\"2026-01-01T00:00:01Z\",\"run_id\":\"r2\",\"flow\":\"verifier\",\"subject\":\"item-2\",\"revision\":\"b\",\"status\":\"merged\",\"tokens_in\":5,\"tokens_out\":1}\n"
	if err := os.WriteFile(ws, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	all, invalid, err := api.Ledger(core.LedgerQuery{})
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	if invalid != 0 {
		t.Fatalf("Ledger invalid = %d, want 0", invalid)
	}
	if len(all) != 2 {
		t.Fatalf("Ledger returned %d rows, want 2", len(all))
	}
	if all[0].Flow != "builder" || all[0].TokensIn != 10 || all[0].CacheRead != 30 || all[0].CacheWrite != 4 || all[0].Reasoning != 5 {
		t.Fatalf("row 0 = %+v, want builder with 10 fresh, 30 cached-read, 4 cached-write, 5 reasoning in", all[0])
	}
	builder, _, err := api.Ledger(core.LedgerQuery{Flow: "builder"})
	if err != nil {
		t.Fatalf("Ledger(builder): %v", err)
	}
	if len(builder) != 1 || builder[0].RunID != "r1" {
		t.Fatalf("Ledger(builder) = %+v, want only r1", builder)
	}
}

// TestCoreRunRecordCopiesEveryTokenClass guards the #176 regression where the
// stats aggregator's adapter dropped CacheRead, CacheWrite, and Reasoning. The
// ledger carries all five classes, and stats text/JSON must total and break
// them down byte-identically, so every class must survive the core->runRecord
// mapping unchanged, not silently zero.
func TestCoreRunRecordCopiesEveryTokenClass(t *testing.T) {
	got := coreRunRecord(core.RunRecord{
		Time: "2026-01-01T00:00:00Z", RunID: "r1", Flow: "builder",
		Status: "built", TokensIn: 10, TokensOut: 2,
		CacheRead: 30, CacheWrite: 4, Reasoning: 5,
	})
	if got.TokensIn != 10 || got.TokOut != 2 {
		t.Fatalf("input/output mismapped: got %+v", got)
	}
	if got.CacheRead != 30 || got.CacheWrite != 4 || got.Reasoning != 5 {
		t.Fatalf("cached/reasoning classes dropped to zero: got %+v", got)
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

func TestCoreTraceMatchesMetacharactersLiterally(t *testing.T) {
	api, work, _ := coreFixture(t)
	runsDir := filepath.Join(work, WorkspaceDir, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A run id may embed opaque item identities that carry glob metacharacters.
	// The id must match literally, never as a glob, and never escape runsDir.
	special := "20260807T150405Z-subject[1].*?"
	specialTrace := []byte("{\"step\":1}\n")
	if err := os.WriteFile(filepath.Join(runsDir, special+".builder.jsonl"), specialTrace, 0o644); err != nil {
		t.Fatal(err)
	}
	// A sibling lookalike must not be matched by treating the id as a glob.
	if err := os.WriteFile(filepath.Join(runsDir, "20260807T150405Z-subjectX.builder.jsonl"), []byte("wrong\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := api.Trace(special)
	if err != nil {
		t.Fatalf("Trace of metacharacter id: %v", err)
	}
	if !bytes.Equal(got, specialTrace) {
		t.Fatalf("Trace = %q, want %q", got, specialTrace)
	}
	// A path-like id must not escape the runs directory.
	if _, err := api.Trace("../outside"); err == nil {
		t.Fatal("Trace of a path-like id must error, not escape runsDir")
	}
}

func TestCoreTraceReadsNestedVerifierTrace(t *testing.T) {
	api, work, _ := coreFixture(t)
	// The verifier flow writes run ids that embed the branch
	// ("branch-forest/<branch>"), so filepath.Join in that flow nests the trace
	// below a subdirectory of the runs dir. Trace must reach that nested file.
	runID := "20260807T150405Z-branch-forest/7-example"
	nested := filepath.Join(work, WorkspaceDir, "runs", "20260807T150405Z-branch-forest")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("{\"step\":1}\n{\"step\":2}\n")
	if err := os.WriteFile(filepath.Join(nested, "7-example.verifier.jsonl"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := api.Trace(runID)
	if err != nil {
		t.Fatalf("Trace of nested verifier run: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Trace = %q, want %q", got, want)
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
		return trackerStub{items: []Item{
			{
				ID: "hab_01J9X", Title: "opaque", UpdatedAt: "r",
				Comments: []comment{{Body: "a note", CreatedAt: "t"}},
			},
		}}
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
	if len(items[0].Comments) != 1 || items[0].Comments[0].Body != "a note" {
		t.Fatalf("Items comments = %+v, want the discussion", items[0].Comments)
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
	d, err := api.Daemon()
	if err != nil {
		t.Fatalf("Daemon: %v", err)
	}
	if d.Active {
		t.Fatal("Daemon reported active in a bare test environment")
	}
}
