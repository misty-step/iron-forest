package main

import (
	"testing"
)

func managerCfg() ManagerFlowCfg {
	return ManagerFlowCfg{
		FlowCfg:     FlowCfg{Enabled: true, Agent: "manager", IntervalSec: 120},
		ReadyDepth:  1,
		ExcludeTags: []string{failedLabel, "parked"},
	}
}

// TestManagerPromotesExactlyOneWhenSlotEmpty proves that with the slot empty and
// several unblocked candidates, one pass plans a single model-driven promotion
// and applying its single pick promotes exactly one item.
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

	tk := newMemoryTracker()
	for _, it := range items {
		tk.seed(it)
	}
	promoted, err := applyManagerPick(tk, plan.cands, "2")
	if err != nil {
		t.Fatal(err)
	}
	if !promoted {
		t.Fatal("a valid pick among the candidates must promote")
	}
	if !tk.items["2"].hasTag(readyTag) {
		t.Fatal("the picked item should carry the ready tag")
	}
	for _, id := range []string{"1", "3"} {
		if tk.items[id].hasTag(readyTag) {
			t.Fatalf("item %s promoted beyond the one pick", id)
		}
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
	plan, err := buildManagerPlan(managerCfg(), repo, items, []Item{items[0]}, nil)
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
	plan, err := buildManagerPlan(managerCfg(), repo, freshItems, freshItems, nil)
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

// TestManagerNeverPromotesOpenBlocker proves a hard filter: an item whose
// Blocked by references an open item is not a candidate and is never promoted,
// including when the report names it. The Controller refuses it as out of set.
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

	tk := newMemoryTracker()
	for _, it := range items {
		tk.seed(it)
	}
	// The model names the blocked item anyway; the Controller refuses it.
	promoted, err := applyManagerPick(tk, plan.cands, "70")
	if err != nil {
		t.Fatal(err)
	}
	if promoted {
		t.Fatal("a blocked item must never be promoted")
	}
	if tk.items["70"].hasTag(readyTag) {
		t.Fatal("a blocked item must not carry the ready tag")
	}
}

// TestManagerReportOutsideSetRefuses proves a report naming an id outside the
// candidate set promotes nothing (a refusal), rather than throwing an error that
// would drive the repeat-failure brake.
func TestManagerReportOutsideSetRefuses(t *testing.T) {
	tk := newMemoryTracker()
	items := []Item{
		{ID: "1", Title: "alpha", UpdatedAt: "u1"},
		{ID: "2", Title: "beta", UpdatedAt: "u2"},
	}
	for _, it := range items {
		tk.seed(it)
	}
	cands := []Item{items[0], items[1]}
	promoted, err := applyManagerPick(tk, cands, "424242")
	if err != nil {
		t.Fatal(err)
	}
	if promoted {
		t.Fatal("an out-of-set pick must promote nothing")
	}
	for _, id := range []string{"1", "2"} {
		if tk.items[id].hasTag(readyTag) {
			t.Fatalf("item %s promoted by an out-of-set pick", id)
		}
	}
}

// TestManagerReapsStalledAssignment proves an assigned, unbranched item that is
// stalled on the builder flow loses readyTag and gains forest:failed, freeing
// the slot, and that a pass plans the reap.
func TestManagerReapsStalledAssignment(t *testing.T) {
	repo := newRefGitRepo(t)
	item := Item{ID: "7", Title: "stalled build", UpdatedAt: "u1", Tags: []string{readyTag}}
	// Drive the builder brake: 3 failures on item-7 at u1 makes it stalled.
	for range stalledRunLimit {
		if err := recordStalled(repo, "builder", "item-7", "u1"); err != nil {
			t.Fatal(err)
		}
	}

	plan, err := buildManagerPlan(managerCfg(), repo, []Item{item}, []Item{item}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.needModel {
		t.Fatal("a pass that must reap should not call the model")
	}
	if len(plan.reap) != 1 || plan.reap[0].ID != "7" {
		t.Fatalf("plan reaps %v, want item 7", plan.reap)
	}

	tk := newMemoryTracker()
	tk.seed(item)
	if err := reapManagerItem(tk, item); err != nil {
		t.Fatal(err)
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

	// After reaping, the item is excluded and the tray is free.
	plan2, err := buildManagerPlan(managerCfg(), repo, []Item{fresh}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan2.cands) != 0 || plan2.needModel {
		t.Fatalf("after reaping, plan = %+v, want no candidates", plan2)
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

// TestManagerReapsClosedReadyItem proves the Manager enumerates ready
// assignments across closed items, not just the open list: an item closed by
// hand still holds the ready tag, and the plan withdraws it (removes ready,
// adds failed) so the slot it would otherwise silently vacate is not filled by
// a second promotion into a tray that was never empty.
func TestManagerReapsClosedReadyItem(t *testing.T) {
	repo := newRefGitRepo(t)
	tk := newMemoryTracker()
	item := Item{ID: "9", Title: "done by hand", UpdatedAt: "u1", Tags: []string{readyTag}}
	tk.seed(item)
	if err := tk.Close("9"); err != nil {
		t.Fatal(err)
	}

	// The open list hides the closed item; the tag query over all states finds it.
	items, err := tk.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	ready, err := tk.ListByTag(readyTag)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("open items = %d, want 0", len(items))
	}
	if len(ready) != 1 || ready[0].ID != "9" {
		t.Fatalf("ready assignments = %v, want the closed item 9", ready)
	}

	plan, err := buildManagerPlan(managerCfg(), repo, items, ready, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.reap) != 1 || plan.reap[0].ID != "9" {
		t.Fatalf("plan reaps %v, want the closed item 9", plan.reap)
	}
	if plan.needModel {
		t.Fatal("a pass that must reap must not call the model")
	}

	if err := reapManagerItem(tk, plan.reap[0]); err != nil {
		t.Fatal(err)
	}
	fresh, err := tk.Get("9")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.hasTag(readyTag) {
		t.Fatal("a closed item reaped must lose the ready tag")
	}
	if !fresh.hasTag(failedLabel) {
		t.Fatal("a reaped closed item must gain the failed tag")
	}
}

// TestManagerSelectReapsClosedReadyItem proves the end-to-end Manager wiring
// acts on a closed ready assignment: Select returns the singleton subject and
// Act withdraws the tag, so the dead assignment is reaped across the closed set.
func TestManagerSelectReapsClosedReadyItem(t *testing.T) {
	repo := newRefGitRepo(t)
	writeAgentFixture(t, repo, "manager", "manager-model")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "31", Title: "hand-closed", UpdatedAt: "u1", Tags: []string{readyTag}})
	if err := tk.Close("31"); err != nil {
		t.Fatal(err)
	}

	old := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = old }()

	cfg := managerFlowConfig(repo)
	cfg.Flows.Manager.Agent = "manager"

	subjects, err := (managerFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].Key != managerSubject {
		t.Fatalf("Select returned %v, want one manager subject to reap", subjects)
	}
	out, err := (managerFlow{}).Act(cfg, repo, subjects[0], "run-reap")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "reaped" {
		t.Fatalf("Act status = %q, want reaped", out.Status)
	}
	fresh, err := tk.Get("31")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.hasTag(readyTag) {
		t.Fatal("Act must withdraw the ready tag from the hand-closed item")
	}
	if !fresh.hasTag(failedLabel) {
		t.Fatal("Act must add the failed tag to the hand-closed item")
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
	managerJudge = func(_ string, _ []Item, _ *Agent, _ string) (managerReport, error) {
		return managerReport{Pick: "424242", Reason: "hallucinated"}, nil
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
