package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func managerCfg() ManagerFlowCfg {
	return ManagerFlowCfg{
		FlowCfg:       FlowCfg{Enabled: true, Agent: "manager", IntervalSec: 120},
		ReadyDepth:    1,
		ExcludeLabels: []string{failedLabel, "parked"},
	}
}

// TestManagerPromotesExactlyOneWhenSlotEmpty proves that an empty slot yields
// one model judgement over the complete candidate set.
func TestManagerPromotesExactlyOneWhenSlotEmpty(t *testing.T) {
	repo := newRefGitRepo(t)
	items := []Item{
		{ID: "1", Title: "alpha", UpdatedAt: "u1", Body: "clear scope"},
		{ID: "2", Title: "beta", UpdatedAt: "u2", Body: "clear scope"},
		{ID: "3", Title: "gamma", UpdatedAt: "u3", Body: "clear scope"},
	}
	plan, err := buildManagerPlan(managerCfg(), repo, items, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.needModel {
		t.Fatal("an empty slot with candidates must plan a model judgement")
	}
	if len(plan.cands) != 3 {
		t.Fatalf("candidates = %d, want 3", len(plan.cands))
	}
	if !plan.hasWork() {
		t.Fatal("plan with candidates must have work")
	}
	if plan.revision == "" {
		t.Fatal("plan revision must be a non-empty stamp over the candidate set")
	}

}

func TestManagerPlanOrderDoesNotDependOnTrackerOrder(t *testing.T) {
	repo := newRefGitRepo(t)
	items := []Item{
		{ID: "9", UpdatedAt: "u9", Tags: []string{readyTag, failedLabel}},
		{ID: "3", UpdatedAt: "u3"},
		{ID: "7", UpdatedAt: "u7", Tags: []string{readyTag, failedLabel}},
		{ID: "1", UpdatedAt: "u1"},
	}
	reversed := []Item{items[3], items[2], items[1], items[0]}
	first, err := buildManagerPlan(managerCfg(), repo, items, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildManagerPlan(managerCfg(), repo, reversed, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, plan := range map[string]managerPlan{"first": first, "second": second} {
		if len(plan.cands) != 2 || plan.cands[0].ID != "1" || plan.cands[1].ID != "3" {
			t.Fatalf("%s candidate order = %#v, want [1 3]", name, plan.cands)
		}
		if len(plan.reap) != 2 || plan.reap[0].ID != "7" || plan.reap[1].ID != "9" {
			t.Fatalf("%s reap order = %#v, want [7 9]", name, plan.reap)
		}
	}
	if first.revision != second.revision {
		t.Fatalf("candidate-set revision changed with Tracker order: %q != %q", first.revision, second.revision)
	}
}

func TestManagerPromptOrderDoesNotDependOnTrackerLabelOrder(t *testing.T) {
	a := &Agent{PromptTmpl: "{{.Task}}"}
	first, err := renderManagerPrompt(a, []Item{{ID: "1", UpdatedAt: "u1", Tags: []string{"z", "a"}}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := renderManagerPrompt(a, []Item{{ID: "1", UpdatedAt: "u1", Tags: []string{"a", "z"}}})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("Manager prompt changed with Tracker label order:\n%s\n---\n%s", first, second)
	}
}

func TestManagerJudgeEnforcesDeclaredReportSchema(t *testing.T) {
	repo := t.TempDir()
	agentDir := t.TempDir()
	schema := `{"type":"object","required":["pick","reason"],"properties":{"pick":{"type":"string"},"reason":{"type":"string"}}}`
	if err := os.WriteFile(filepath.Join(agentDir, "report.schema.json"), []byte(schema), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{Dir: agentDir, Name: "manager", PromptTmpl: "{{.Task}}"}
	oldRun := runPhase
	runPhase = func(_ string, runDir string, _ *Agent, _, _ string) (runStats, error) {
		return runStats{}, os.WriteFile(filepath.Join(runDir, "report.json"), []byte(`{"pick":"1"}`), 0o644)
	}
	defer func() { runPhase = oldRun }()
	if _, _, err := runManagerJudge(repo, []Item{{ID: "1", Title: "one"}}, a, "r1"); err == nil ||
		!strings.Contains(err.Error(), `missing required field "reason"`) {
		t.Fatalf("Manager judge without schema field = %v, want refusal", err)
	}
}

func TestManagerPlanExcludesRetiringItems(t *testing.T) {
	repo := newRefGitRepo(t)
	retiring := Item{ID: "9", Title: "retiring", UpdatedAt: "u9", Tags: []string{readyTag}}
	candidate := Item{ID: "10", Title: "candidate", UpdatedAt: "u10"}
	plan, err := buildManagerPlan(managerCfg(), repo, []Item{retiring, candidate}, nil, []string{"9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.reap) != 0 || len(plan.cands) != 1 || plan.cands[0].ID != "10" {
		t.Fatalf("retirement-covered Manager plan = %#v, want only item 10 candidate", plan)
	}
}

// TestManagerSlotOccupiedPromotesNothing proves a Manager pass that finds the
// slot occupied (one healthy ready, unbranched assignment in flight) plans no
// model call and Select returns no subject, so no model runs.
func TestManagerSlotOccupiedPromotesNothing(t *testing.T) {
	repo := newRefGitRepo(t)
	items := []Item{
		{ID: "1", Title: "in flight build", UpdatedAt: "u1", Tags: []string{readyTag}},
		{ID: "2", Title: "next", UpdatedAt: "u2", Body: "clear scope"},
	}
	plan, err := buildManagerPlan(managerCfg(), repo, items, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.needModel {
		t.Fatal("an occupied slot must not call the model")
	}
	if plan.hasWork() {
		t.Fatal("an occupied healthy slot and no reap is no work")
	}

	tk := newMemoryTracker()
	for _, it := range items {
		tk.seed(it)
	}
	old := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = old }()

	subjects, err := (managerFlow{}).Select(managerFlowConfig(repo), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 0 {

		t.Fatalf("Select returned %d subjects, want 0 (no model)", len(subjects))
	}
}

func TestManagerReportPreservesOpaquePick(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.json"), []byte(`{"pick":" item-1 "}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := readManagerReportFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pick != " item-1 " {
		t.Fatalf("Manager pick = %q, want opaque identity unchanged", rep.Pick)
	}
}

// TestManagerInProgressBuildKeepsSlotOccupied pins the load-bearing constraint:
// an item that carries readyTag yet has no remote forest branch occupies the
// slot for the whole of a build, with no liveness probe or lease. It is healthy
// (not stalled, not failed, unblocked), so it is counted and no promotion or
// model call happens.
func TestManagerInProgressBuildKeepsSlotOccupied(t *testing.T) {
	tk := newMemoryTracker()
	tk.seed(Item{ID: "1", Title: "building", UpdatedAt: "u1", Tags: []string{readyTag}})
	repo := newRefGitRepo(t)
	// A real pass reads open items from the tracker and branches from the repo;
	// there is no branch yet, so the building item still owns the slot.
	freshItems, err := tk.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildManagerPlan(managerCfg(), repo, freshItems, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.needModel {
		t.Fatal("an in-progress build must keep the slot occupied and call no model")
	}
	wantLabel := "manager: slot occupied, nothing to do"
	if plan.label != wantLabel {
		t.Fatalf("label = %q, want %q", plan.label, wantLabel)
	}
}

// TestManagerNeverPromotesOpenBlocker proves that an item blocked by another
// open item never reaches the candidate set.
func TestManagerNeverPromotesOpenBlocker(t *testing.T) {
	repo := newRefGitRepo(t)
	items := []Item{
		{ID: "149", Title: "still open", UpdatedAt: "u1"},
		{ID: "70", Title: "waiting", UpdatedAt: "u2", Body: "Blocked by: #149"},
	}
	plan, err := buildManagerPlan(managerCfg(), repo, items, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range plan.cands {
		if c.ID == "70" {
			t.Fatal("a blocked item must be filtered out of the candidate set")
		}
	}

}

// TestManagerNeverOffersRepositoryExcludedLabel proves the deployed YAML
// policy reaches the production selector before the model sees candidates.
func TestManagerNeverOffersRepositoryExcludedLabel(t *testing.T) {
	cfg, err := loadConfig("forest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	epic := Item{ID: "15", Title: "umbrella", UpdatedAt: "u1", Tags: []string{"epic"}}
	leaf := Item{ID: "50", Title: "leaf", UpdatedAt: "u2"}
	tk := newMemoryTracker()
	tk.seed(epic)
	tk.seed(leaf)
	old := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = old }()

	subjects, err := (managerFlow{}).Select(cfg, newRefGitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 {
		t.Fatalf("subjects = %+v, want one leaf candidate", subjects)
	}
	if got, want := subjects[0].Revision, itemSetStamp([]Item{leaf}); got != want {
		t.Fatalf("candidate revision = %q, want leaf revision %q", got, want)
	}
}

// TestManagerReapsStalledAssignment proves that a stalled, assigned item enters
// the deterministic reap plan without another model judgement.
func TestManagerReapsStalledAssignment(t *testing.T) {
	repo := newRefGitRepo(t)
	item := Item{ID: "7", Title: "stalled build", UpdatedAt: "u1", Tags: []string{readyTag}}
	// Drive the builder brake: 3 failures on item-7 at u1 makes it stalled.
	for range stalledRunLimit {
		if err := recordStalled(repo, "builder", "item-7", "u1"); err != nil {
			t.Fatal(err)
		}
	}

	plan, err := buildManagerPlan(managerCfg(), repo, []Item{item}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.needModel {
		t.Fatal("a pass that must reap should not call the model")
	}
	if len(plan.reap) != 1 || plan.reap[0].ID != "7" {
		t.Fatalf("plan reaps %v, want item 7", plan.reap)
	}

}

// TestManagerFlowsForKeepsAllLanes proves the Manager lane is added without
// removing or changing the Builder, Verifier, and Fixer selectors: each lane is
// still present in the declared flow set.
func TestManagerFlowsForKeepsAllLanes(t *testing.T) {
	seen := make(map[string]bool)
	for _, f := range flowsFor() {
		if seen[f.Name()] {
			t.Fatalf("flow %q declared twice", f.Name())
		}
		seen[f.Name()] = true
	}
	for _, want := range []string{"builder", "verifier", "fixer", "manager"} {
		if !seen[want] {
			t.Fatalf("flow set is missing %q", want)
		}
	}
}

// TestManagerSelectPerformsNoWrites proves Select is a pure read: it lays no
// tag, writes no ledger row, and leaves the repository untouched.
func TestManagerSelectPerformsNoWrites(t *testing.T) {
	repo := newRefGitRepo(t)
	tk := newMemoryTracker()
	tk.seed(Item{ID: "1", Title: "alpha", UpdatedAt: "u1", Body: "clear scope"})
	tk.seed(Item{ID: "2", Title: "beta", UpdatedAt: "u2", Body: "clear scope"})

	old := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = old }()

	cfg := managerFlowConfig(repo)
	subjects, err := (managerFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].Key != managerSubject {
		t.Fatalf("Select returned %v, want one manager subject", subjects)
	}
	for _, id := range []string{"1", "2"} {
		if tk.items[id].hasTag(readyTag) {
			t.Fatalf("Select wrote the ready tag onto item %s", id)
		}
	}
	rows, _, err := loadLedger(ledgerPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("Select wrote %d ledger rows, want 0", len(rows))
	}
}

// TestManagerRewisesWhenBacklogMoves proves the revision is a deterministic
// stamp over the judged candidate set, so it is non-empty and changes when the
// set changes instead of braking the lane forever.
func TestManagerRewisesWhenBacklogMoves(t *testing.T) {
	repo := newRefGitRepo(t)
	cfg := managerCfg()
	a := []Item{{ID: "1", Title: "a", UpdatedAt: "u1"}}
	ab := []Item{
		{ID: "1", Title: "a", UpdatedAt: "u1"},
		{ID: "2", Title: "b", UpdatedAt: "u2"},
	}
	pa, err := buildManagerPlan(cfg, repo, a, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pab, err := buildManagerPlan(cfg, repo, ab, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pa.revision == "" || pab.revision == "" {
		t.Fatal("plan revision must never be empty for a judging pass")
	}
	if pa.revision == pab.revision {
		t.Fatalf("revision did not move with the backlog: %q", pa.revision)
	}
}

func TestManagerRevisionDistinguishesEqualTimestampSets(t *testing.T) {
	a := itemSetStamp([]Item{{ID: "1", UpdatedAt: "same"}})
	b := itemSetStamp([]Item{{ID: "2", UpdatedAt: "same"}})
	if a == b {
		t.Fatalf("different candidate sets share Revision %q", a)
	}
	c := itemSetStamp([]Item{{ID: "a:b", UpdatedAt: "c"}})
	d := itemSetStamp([]Item{{ID: "a", UpdatedAt: "b:c"}})
	if c == d {
		t.Fatalf("delimiter-bearing candidate sets share Revision %q", c)
	}
}

func TestManagerActRefusesChangedSelection(t *testing.T) {
	repo := newRefGitRepo(t)
	tk := newMemoryTracker()
	tk.seed(Item{ID: "1", Title: "alpha", UpdatedAt: "u1"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	cfg := managerFlowConfig(repo)
	subjects, err := (managerFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 {
		t.Fatalf("Select = (%v, %v), want one Subject", subjects, err)
	}
	tk.seed(Item{ID: "1", Title: "changed", UpdatedAt: "u2"})
	called := false
	oldJudge := managerJudge
	managerJudge = func(_ string, _ []Item, _ *Agent, _ string) (managerReport, runStats, error) {
		called = true
		return managerReport{Pick: "1"}, runStats{}, nil
	}
	defer func() { managerJudge = oldJudge }()

	out, err := (managerFlow{}).Act(cfg, repo, subjects[0], "r1")
	if err != nil || out.Status != "stale" {
		t.Fatalf("Act changed selection = (%q, %v), want stale", out.Status, err)
	}
	if called || tk.items["1"].hasTag(readyTag) {
		t.Fatal("stale Manager selection ran the model or wrote a tag")
	}
}

// TestManagerRefusalEngagesBrake proves an out-of-candidate pick promotes
// nothing (a refusal) and engages the repeat-failure brake, so the unchanged
// candidate set is not re-judged every pass: after the limit, the lane stops
// selecting until the backlog moves.
func TestManagerRefusalEngagesBrake(t *testing.T) {
	repo := newRefGitRepo(t)
	writeAgentFixture(t, repo, "manager", "manager-model")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "1", Title: "alpha", UpdatedAt: "u1", Body: "clear scope"})
	tk.seed(Item{ID: "2", Title: "beta", UpdatedAt: "u2", Body: "clear scope"})

	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	oldJudge := managerJudge
	managerJudge = func(_ string, _ []Item, _ *Agent, _ string) (managerReport, runStats, error) {
		return managerReport{Pick: "424242"}, runStats{}, nil
	}
	defer func() { managerJudge = oldJudge }()

	cfg := managerFlowConfig(repo)
	cfg.Flows.Manager.Agent = "manager"

	subjects, err := (managerFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 {
		t.Fatalf("Select returned %d subjects, want 1", len(subjects))
	}

	// Each refused pass returns a failure and records a stall on the manager
	// subject's revision. After the limit the lane stops selecting it.
	for range stalledRunLimit {
		code := actOnSubject(managerFlow{}, cfg, repo, subjects[0], nil)
		if code == 0 {
			t.Fatal("a refused pass must return a failure code")
		}
	}
	stalled, err := stalledOn(repo, "manager", managerSubject, subjects[0].Revision)
	if err != nil {
		t.Fatal(err)
	}
	if !stalled {
		t.Fatal("a refused pick must be recorded as stalled after the limit")
	}
	if got, err := (managerFlow{}).Select(cfg, repo); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Fatalf("a braked lane must select nothing, got %d subjects", len(got))
	}
	for _, id := range []string{"1", "2"} {
		if tk.items[id].hasTag(readyTag) {
			t.Fatalf("item %s promoted by a refused pick", id)
		}
	}
}

// managerFlowConfig builds a full Config with the Manager lane enabled for
// Select and Act tests that read cfg.Repo through the tracker stub.
func managerFlowConfig(repo string) Config {
	return Config{
		Repo: repo,
		Flows: Flows{
			Manager: managerCfg(),
		},
	}
}

// TestManagerReapsDespiteModelBrake proves the deterministic reap bypasses the
// promote-judgement brake. After the Manager's judgement on an unchanged
// candidate set is braked (three refusals), a ready branchless item that later
// becomes a dead assignment (stalled) must still be reaped: Select must return
// the subject so Act frees the slot, and Act must withdraw the dead item
// without retrying the braked judgement or calling the model again.
func TestManagerReapsDespiteModelBrake(t *testing.T) {
	repo := newRefGitRepo(t)
	tk := newMemoryTracker()
	tk.seed(Item{ID: "1", Title: "alpha", UpdatedAt: "u1", Body: "clear scope"})
	tk.seed(Item{ID: "7", Title: "dead build", UpdatedAt: "u7", Tags: []string{readyTag}})

	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	oldJudge := managerJudge
	called := false
	managerJudge = func(_ string, _ []Item, _ *Agent, _ string) (managerReport, runStats, error) {
		called = true
		return managerReport{Pick: "1"}, runStats{}, nil
	}
	defer func() { managerJudge = oldJudge }()

	// item 7 is a dead assignment: it is stalled on the builder flow.
	for range stalledRunLimit {
		if err := recordStalled(repo, "builder", "item-7", "u7"); err != nil {
			t.Fatal(err)
		}
	}
	items, err := tk.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	plan0, err := buildManagerPlan(managerCfg(), repo, items, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan0.needModel || len(plan0.reap) != 1 {
		t.Fatalf("setup plan = needModel %v, reap %v, want both a judgement and a reap",
			plan0.needModel, plan0.reap)
	}
	// Brake the promote judgement on the unchanged candidate set.
	for range stalledRunLimit {
		if err := recordStalled(repo, "manager", managerSubject, plan0.revision); err != nil {
			t.Fatal(err)
		}
	}

	cfg := managerFlowConfig(repo)
	cfg.Flows.Manager.Agent = "manager"

	subjects, err := (managerFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 {
		t.Fatalf("Select returned %d subjects, want 1 so the deterministic reap runs despite the brake",
			len(subjects))
	}

	out, err := (managerFlow{}).Act(cfg, repo, subjects[0], "r1")
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("Act must not retry the braked promote judgement")
	}
	if out.Status != "reaped" {
		t.Fatalf("status = %q, want reaped", out.Status)
	}
	fresh, err := tk.Get("7")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.hasTag(readyTag) {
		t.Fatal("a reaped item must lose the ready tag")
	}
	if !fresh.hasTag(failedLabel) {
		t.Fatal("a reaped item must gain the failed tag")
	}
	// The dead assignment is gone but the candidate set is unchanged, so the
	// promote judgement stays braked and no model runs until the backlog moves.
	if got, err := (managerFlow{}).Select(cfg, repo); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Fatalf("after reaping, Select = %d subjects, want 0 (candidate set unchanged, judgement still braked)",
			len(got))
	}
	if called {
		t.Fatal("the braked judgement must not run while the candidate set is unchanged")
	}
}

// TestManagerRecordsTokensOnPromotion proves every Manager model invocation is
// recorded on the ledger with every measured token class: output, cache read,
// cache write, and reasoning as well as the fresh input and output. Act routes
// the run's token accounting through Outcome.addTokens (the same path the other
// lanes use) so the ledger does not show zeros for a judgement that actually
// spent tokens.
func TestManagerRecordsTokensOnPromotion(t *testing.T) {
	repo := newRefGitRepo(t)
	writeAgentFixture(t, repo, "manager", "manager-model")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "1", Title: "alpha", UpdatedAt: "u1", Body: "clear scope"})

	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	oldJudge := managerJudge
	managerJudge = func(_ string, _ []Item, _ *Agent, _ string) (managerReport, runStats, error) {
		return managerReport{Pick: "1"}, runStats{
			tokensIn: 123, tokensOut: 45,
			cacheRead: 67, cacheWrite: 89, reasoning: 34,
		}, nil
	}
	defer func() { managerJudge = oldJudge }()

	cfg := managerFlowConfig(repo)
	cfg.Flows.Manager.Agent = "manager"
	subjects, err := (managerFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 {
		t.Fatalf("Select returned %d subjects, want 1", len(subjects))
	}
	out, err := (managerFlow{}).Act(cfg, repo, subjects[0], "r1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("status = %q, want done", out.Status)
	}
	if out.TokIn != 123 || out.TokOut != 45 {
		t.Fatalf("tokens = in %d / out %d, want 123 / 45", out.TokIn, out.TokOut)
	}
	if out.CacheRead != 67 || out.CacheWrite != 89 || out.Reasoning != 34 {
		t.Fatalf("cached tokens = read %d / write %d / reasoning %d, want 67 / 89 / 34",
			out.CacheRead, out.CacheWrite, out.Reasoning)
	}
	if !tk.items["1"].hasTag(readyTag) {
		t.Fatal("the picked item should carry the ready tag")
	}
}

// TestManagerWithdrawsReadyForNewExclusion proves a policy label added after
// promotion withdraws only the Manager ready label, while another assignment
// without that label keeps its ready slot.
func TestManagerWithdrawsReadyForNewExclusion(t *testing.T) {
	repo := newRefGitRepo(t)
	writeAgentFixture(t, repo, "manager", "manager-model")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "1", Title: "alpha", UpdatedAt: "u1", Body: "clear scope"})

	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldJudge := managerJudge
	managerJudge = func(_ string, _ []Item, _ *Agent, _ string) (managerReport, runStats, error) {
		return managerReport{Pick: "1"}, runStats{}, nil
	}
	defer func() { managerJudge = oldJudge }()

	cfg := managerFlowConfig(repo)
	cfg.Flows.Manager.Agent = "manager"
	first, err := (managerFlow{}).Select(cfg, repo)
	if err != nil || len(first) != 1 {
		t.Fatalf("first Select = (%v, %v), want one subject", first, err)
	}
	if out, err := (managerFlow{}).Act(cfg, repo, first[0], "r1"); err != nil || out.Status != "done" {
		t.Fatalf("first Act = (%q, %v), want done", out.Status, err)
	}
	if !tk.items["1"].hasTag(readyTag) {
		t.Fatal("first Manager pass did not promote item 1")
	}
	if err := tk.SetTags("1", []string{"parked"}, nil); err != nil {
		t.Fatal(err)
	}
	tk.seed(Item{ID: "2", Title: "beta", UpdatedAt: "u2", Tags: []string{readyTag}})

	second, err := (managerFlow{}).Select(cfg, repo)
	if err != nil || len(second) != 1 {
		t.Fatalf("second Select = (%v, %v), want one withdrawal subject", second, err)
	}
	out, err := (managerFlow{}).Act(cfg, repo, second[0], "r2")
	if err != nil || out.Status != "reaped" {
		t.Fatalf("second Act = (%q, %v), want reaped", out.Status, err)
	}
	if tk.items["1"].hasTag(readyTag) {
		t.Fatal("Manager kept ready on an item with a configured exclusion")
	}
	if !tk.items["1"].hasTag("parked") {
		t.Fatal("Manager removed the configured exclusion label")
	}
	if tk.items["1"].hasTag(failedLabel) {
		t.Fatal("Manager marked a policy withdrawal as failed")
	}
	if !tk.items["2"].hasTag(readyTag) {
		t.Fatal("Manager withdrew ready from a non-excluded item")
	}
}

func TestManagerDoesNotPromoteItemChangedDuringJudgement(t *testing.T) {
	repo := newRefGitRepo(t)
	writeAgentFixture(t, repo, "manager", "manager-model")
	tk := newMemoryTracker()
	original := Item{ID: "1", Title: "alpha", UpdatedAt: "u1"}
	tk.seed(original)
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldJudge := managerJudge
	managerJudge = func(_ string, _ []Item, _ *Agent, _ string) (managerReport, runStats, error) {
		tk.seed(Item{ID: "1", Title: "changed", UpdatedAt: "u2"})
		return managerReport{Pick: "1"}, runStats{}, nil
	}
	defer func() { managerJudge = oldJudge }()

	cfg := managerFlowConfig(repo)
	out, err := (managerFlow{}).Act(cfg, repo, Subject{
		Key: managerSubject, Revision: itemSetStamp([]Item{original}),
	}, "r1")
	if err != nil || out.Status != "stale" {
		t.Fatalf("changed item promotion = (%q, %v), want stale", out.Status, err)
	}
	if tk.items["1"].hasTag(readyTag) {
		t.Fatal("Manager promoted an Item changed during judgement")
	}
}

func TestManagerDoesNotPromoteWhenBlockerOpensDuringJudgement(t *testing.T) {
	repo := newRefGitRepo(t)
	writeAgentFixture(t, repo, "manager", "manager-model")
	tk := newMemoryTracker()
	picked := Item{ID: "1", Title: "alpha", UpdatedAt: "u1", Body: "Blocked by: 2"}
	tk.seed(picked)
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldJudge := managerJudge
	managerJudge = func(_ string, _ []Item, _ *Agent, _ string) (managerReport, runStats, error) {
		tk.seed(Item{ID: "2", Title: "blocker", UpdatedAt: "u2"})
		return managerReport{Pick: "1"}, runStats{}, nil
	}
	defer func() { managerJudge = oldJudge }()

	cfg := managerFlowConfig(repo)
	out, err := (managerFlow{}).Act(cfg, repo, Subject{
		Key: managerSubject, Revision: itemSetStamp([]Item{picked}),
	}, "r1")
	if err != nil || out.Status != "stale" {
		t.Fatalf("opened blocker promotion = (%q, %v), want stale", out.Status, err)
	}
	if tk.items["1"].UpdatedAt != "u1" {
		t.Fatalf("blocker test changed the picked stamp to %q, state must move, not the item", tk.items["1"].UpdatedAt)
	}
	if tk.items["1"].hasTag(readyTag) {
		t.Fatal("Manager promoted an Item after its blocker opened")
	}
}

func TestManagerDoesNotPromoteWhenReadyDepthFillsDuringJudgement(t *testing.T) {
	repo := newRefGitRepo(t)
	writeAgentFixture(t, repo, "manager", "manager-model")
	tk := newMemoryTracker()
	picked := Item{ID: "1", Title: "alpha", UpdatedAt: "u1"}
	tk.seed(picked)
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldJudge := managerJudge
	managerJudge = func(_ string, _ []Item, _ *Agent, _ string) (managerReport, runStats, error) {
		tk.seed(Item{ID: "2", Title: "already ready", UpdatedAt: "u2", Tags: []string{readyTag}})
		return managerReport{Pick: "1"}, runStats{}, nil
	}
	defer func() { managerJudge = oldJudge }()

	cfg := managerFlowConfig(repo)
	out, err := (managerFlow{}).Act(cfg, repo, Subject{
		Key: managerSubject, Revision: itemSetStamp([]Item{picked}),
	}, "r1")
	if err != nil || out.Status != "stale" {
		t.Fatalf("filled ready depth promotion = (%q, %v), want stale", out.Status, err)
	}
	if !tk.items["2"].hasTag(readyTag) {
		t.Fatal("the ready item that filled the slot lost its ready tag")
	}
	if tk.items["1"].hasTag(readyTag) {
		t.Fatal("Manager promoted an Item after ready depth filled")
	}
}

func TestManagerDoesNotPromoteItemBranchedDuringJudgement(t *testing.T) {
	repo := newRefGitRepo(t)
	writeAgentFixture(t, repo, "manager", "manager-model")
	tk := newMemoryTracker()
	item := Item{ID: "1", Title: "alpha", UpdatedAt: "u1"}
	tk.seed(item)
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldJudge := managerJudge
	managerJudge = func(_ string, _ []Item, _ *Agent, _ string) (managerReport, runStats, error) {
		runGitTest(t, repo, "commit", "--allow-empty", "-m", "branch base")
		runGitTest(t, repo, "push", "-q", "origin", "HEAD:refs/heads/forest/1-change")
		return managerReport{Pick: "1"}, runStats{}, nil
	}
	defer func() { managerJudge = oldJudge }()

	cfg := managerFlowConfig(repo)
	out, err := (managerFlow{}).Act(cfg, repo, Subject{
		Key: managerSubject, Revision: itemSetStamp([]Item{item}),
	}, "r1")
	if err != nil || out.Status != "stale" {
		t.Fatalf("branched item promotion = (%q, %v), want stale", out.Status, err)
	}
	if tk.items["1"].hasTag(readyTag) {
		t.Fatal("Manager promoted an Item branched during judgement")
	}
}

func TestManagerDoesNotPromoteItemRetiredDuringJudgement(t *testing.T) {
	repo := newRefGitRepo(t)
	writeAgentFixture(t, repo, "manager", "manager-model")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "1", Title: "alpha", UpdatedAt: "u1", Body: "clear scope"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldJudge := managerJudge
	managerJudge = func(_ string, _ []Item, _ *Agent, _ string) (managerReport, runStats, error) {
		_, err := recordRetirement(repo, retirementRecord{
			Branch: "forest/1-change", Revision: strings.Repeat("b", 40), ItemID: "1",
			Transport: "git", Strategy: "squash", Title: "change", State: "landed",
			Agent: "verifier", Model: "verifier-model", DefSHA: strings.Repeat("a", 16),
		})
		return managerReport{Pick: "1"}, runStats{}, err
	}
	defer func() { managerJudge = oldJudge }()

	cfg := managerFlowConfig(repo)
	cfg.Flows.Manager.Agent = "manager"
	out, err := (managerFlow{}).Act(cfg, repo, Subject{
		Key: managerSubject, Revision: itemSetStamp([]Item{tk.items["1"]}),
	}, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "stale" {
		t.Fatalf("status = %q, want stale", out.Status)
	}
	if tk.items["1"].hasTag(readyTag) {
		t.Fatal("Manager promoted an Item retired while its judgement ran")
	}
}

func TestManagerDoesNotPromoteItemClaimedByBuilder(t *testing.T) {
	repo := newRefGitRepo(t)
	writeAgentFixture(t, repo, "manager", "manager-model")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "1", Title: "alpha", UpdatedAt: "u1", Body: "clear scope"})

	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldJudge := managerJudge
	managerJudge = func(_ string, _ []Item, _ *Agent, _ string) (managerReport, runStats, error) {
		return managerReport{Pick: "1"}, runStats{}, nil
	}
	defer func() { managerJudge = oldJudge }()

	release, err := claimAdmission(repo, repo, "builder", Subject{Key: "item-1", Kind: "item", ID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	cfg := managerFlowConfig(repo)
	cfg.Flows.Manager.Agent = "manager"
	out, err := (managerFlow{}).Act(cfg, repo, Subject{
		Key: managerSubject, Revision: itemSetStamp([]Item{tk.items["1"]}),
	}, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "stale" {
		t.Fatalf("status = %q, want stale", out.Status)
	}
	if tk.items["1"].hasTag(readyTag) {
		t.Fatal("Manager promoted an Item while the Builder held its admission")
	}
}

func TestManagerDoesNotReapItemClaimedByBuilder(t *testing.T) {
	repo := newRefGitRepo(t)
	tk := newMemoryTracker()
	tk.seed(Item{ID: "7", Title: "active build", UpdatedAt: "u7", Tags: []string{readyTag}})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	for range stalledRunLimit {
		if err := recordStalled(repo, "builder", "item-7", "u7"); err != nil {
			t.Fatal(err)
		}
	}
	release, err := claimAdmission(repo, repo, "builder", Subject{Key: "item-7", Kind: "item", ID: "7"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	out, err := (managerFlow{}).Act(managerFlowConfig(repo), repo, Subject{
		Key: managerSubject, Revision: itemSetStamp([]Item{tk.items["7"]}),
	}, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "stale" {
		t.Fatalf("status = %q, want stale", out.Status)
	}
	if !tk.items["7"].hasTag(readyTag) || tk.items["7"].hasTag(failedLabel) {
		t.Fatalf("Manager changed an Item while the Builder held its admission: %v", tk.items["7"].Tags)
	}
}

func TestManagerDoesNotPromoteWhenDeadAssignmentRemainsClaimed(t *testing.T) {
	repo := newRefGitRepo(t)
	writeAgentFixture(t, repo, "manager", "manager-model")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "7", Title: "active build", UpdatedAt: "u7", Tags: []string{readyTag}})
	tk.seed(Item{ID: "8", Title: "next build", UpdatedAt: "u8", Body: "clear scope"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	for range stalledRunLimit {
		if err := recordStalled(repo, "builder", "item-7", "u7"); err != nil {
			t.Fatal(err)
		}
	}
	release, err := claimAdmission(repo, repo, "builder", Subject{Key: "item-7", Kind: "item", ID: "7"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	judged := false
	oldJudge := managerJudge
	managerJudge = func(string, []Item, *Agent, string) (managerReport, runStats, error) {
		judged = true
		return managerReport{Pick: "8"}, runStats{}, nil
	}
	defer func() { managerJudge = oldJudge }()
	out, err := (managerFlow{}).Act(managerFlowConfig(repo), repo, Subject{
		Key: managerSubject, Revision: itemSetStamp([]Item{tk.items["8"]}),
	}, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "stale" {
		t.Fatalf("status = %q, want stale", out.Status)
	}
	if judged {
		t.Fatal("Manager judged a replacement while the dead assignment still owned the slot")
	}
	if !tk.items["7"].hasTag(readyTag) || tk.items["8"].hasTag(readyTag) {
		t.Fatalf("Manager overfilled the ready slot: item 7 %v, item 8 %v", tk.items["7"].Tags, tk.items["8"].Tags)
	}
}

func TestManagerReapRefusesDifferentFreshAssignment(t *testing.T) {
	repo := newRefGitRepo(t)
	tk := newMemoryTracker()
	tk.seed(Item{ID: "1", UpdatedAt: "u1", Tags: []string{readyTag}})
	tk.seed(Item{ID: "2", UpdatedAt: "u2", Tags: []string{readyTag, "parked"}})
	cfg := managerFlowConfig(repo)
	cfg.Flows.Manager.ExcludeLabels = []string{"parked"}

	changed, err := mutateManagerItem(cfg, repo, tk, tk.items["1"], nil, []string{readyTag}, "")
	if err != nil {
		t.Fatal(err)
	}
	if changed || !tk.items["1"].hasTag(readyTag) {
		t.Fatalf("Manager reaped a healthy assignment because another item needed withdrawal: %v", tk.items["1"].Tags)
	}
}
