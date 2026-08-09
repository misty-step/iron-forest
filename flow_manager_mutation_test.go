package main

import (
	"strings"
	"testing"
)

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

func TestManagerDoesNotPromoteWhenAssignmentNeedsReapingDuringJudgement(t *testing.T) {
	repo := newRefGitRepo(t)
	writeAgentFixture(t, repo, "manager", "manager-model")
	tk := newMemoryTracker()
	picked := Item{ID: "1", Title: "alpha", UpdatedAt: "u1"}
	tk.seed(picked)
	tk.seed(Item{ID: "2", Title: "current assignment", UpdatedAt: "u2", Tags: []string{readyTag}})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldJudge := managerJudge
	managerJudge = func(_ string, _ []Item, _ *Agent, _ string) (managerReport, runStats, error) {
		return managerReport{Pick: "1"}, runStats{}, tk.SetTags("2", []string{"parked"}, nil)
	}
	defer func() { managerJudge = oldJudge }()

	cfg := managerFlowConfig(repo)
	cfg.Flows.Manager.ReadyDepth = 2
	cfg.Flows.Manager.ExcludeLabels = []string{"parked"}
	out, err := (managerFlow{}).Act(cfg, repo, Subject{
		Key: managerSubject, Revision: itemSetStamp([]Item{picked}),
	}, "r1")
	if err != nil || out.Status != "stale" {
		t.Fatalf("new withdrawal during judgement = (%q, %v), want stale", out.Status, err)
	}
	if tk.items["1"].hasTag(readyTag) {
		t.Fatal("Manager promoted an Item before reaping a newly dead assignment")
	}
	if !tk.items["2"].hasTag(readyTag) {
		t.Fatal("Manager reap happened outside the deterministic next pass")
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

func TestManagerReapRefusesChangedTargetRevision(t *testing.T) {
	repo := newRefGitRepo(t)
	tk := newMemoryTracker()
	tk.seed(Item{ID: "1", UpdatedAt: "u2", Tags: []string{readyTag, "parked"}})
	cfg := managerFlowConfig(repo)
	cfg.Flows.Manager.ExcludeLabels = []string{"parked"}
	stale := Item{ID: "1", UpdatedAt: "u1", Tags: []string{readyTag, "parked"}}

	changed, err := mutateManagerItem(cfg, repo, tk, stale, nil, []string{readyTag}, "")
	if err != nil {
		t.Fatal(err)
	}
	if changed || !tk.items["1"].hasTag(readyTag) {
		t.Fatalf("Manager reaped changed assignment revision: %v", tk.items["1"].Tags)
	}
}
