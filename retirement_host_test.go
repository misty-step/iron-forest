package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifierRecoversAlreadyMergedProjectionWithoutDuplicate(t *testing.T) {
	branch := "forest/9-already-merged"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")
	if err := writeChecks(repo, reviewed, checksNote{Status: "pass", RunID: "seed"}); err != nil {
		t.Fatal(err)
	}
	if err := writeVerdict(repo, reviewed, verdictNote{
		Verdict: "approve", Reviewer: "verifier", Model: "verifier-model", DefSHA: strings.Repeat("a", 16), RunID: "seed",
	}); err != nil {
		t.Fatal(err)
	}
	oldTracker := trackerFor
	tk := newMemoryTracker()
	tk.seed(Item{ID: "9", Title: "renamed after build"})
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	createCalls, mergeCalls := 0, 0
	merged := false
	projectionCommand = func(args ...string) ([]byte, error) {
		state := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--state" {
				state = args[i+1]
			}
		}
		switch {
		case len(args) > 0 && args[0] == "api" && merged:
			return mergedProjectionPage(`{"number":9,"html_url":"https://github.com/owner/repo/pull/9","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"` + reviewed + `","ref":"` + branch + `","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}`), nil
		case len(args) > 0 && args[0] == "api":
			return []byte(`[]`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "list" && state == "open" && !merged:
			return []byte(`[{"number":9,"url":"https://github.com/owner/repo/pull/9","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
			return []byte(`[]`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "comment":
			return nil, nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "create":
			createCalls++
			return nil, errors.New("duplicate pull request")
		case len(args) >= 2 && args[0] == "pr" && args[1] == "merge":
			mergeCalls++
			return nil, errors.New("duplicate Host merge")
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = false
	selected, selectErr := (verifierFlow{}).Select(cfg, repo)
	if selectErr != nil || len(selected) != 1 || selected[0].Kind != subjectBranch {
		t.Fatalf("manual Host Select before intent = (subjects=%#v, err=%v), want one branch subject", selected, selectErr)
	}
	out, err := (verifierFlow{}).Act(cfg, repo, Subject{
		Key: "branch-" + branch, Kind: "branch", Revision: reviewed,
		ID: "9", Branch: branch, Head: reviewed,
	}, "intent")
	if err != nil || out.Status != "reviewed" {
		t.Fatalf("initial Host intent = (status=%q, err=%v)", out.Status, err)
	}
	if createCalls != 0 || mergeCalls != 0 {
		t.Fatalf("intent made create/merge calls = %d/%d", createCalls, mergeCalls)
	}
	if _, found, err := readRetirement(repo, branch, reviewed); err != nil || !found {
		t.Fatalf("pending Host retirement = (found=%v, err=%v)", found, err)
	}
	selected, selectErr = (verifierFlow{}).Select(cfg, repo)
	if selectErr != nil || len(selected) != 1 || selected[0].Kind != subjectRetirement {
		t.Fatalf("manual Host Select after intent = (subjects=%#v, err=%v), want one retirement subject", selected, selectErr)
	}
	runGitTest(t, repo, "branch", "-D", branch)
	if err := deleteRef(repo, "refs/heads/"+branch, reviewed); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "fetch", "-q", "--prune", "origin")
	builderSubjects, err := (builderFlow{}).Select(cfg, repo)
	if err != nil || len(builderSubjects) != 0 {
		t.Fatalf("Builder re-exposed retired item = (subjects=%#v, err=%v)", builderSubjects, err)
	}
	if err := os.RemoveAll(filepath.Join(repo, DefaultAgentsDir, "builder")); err != nil {
		t.Fatal(err)
	}
	out, err = (builderFlow{}).Act(cfg, repo, Subject{
		Key: "item-9", Kind: subjectItem, Revision: reviewed, ID: "9",
		Item: Item{ID: "9", Title: "renamed after build"},
	}, "stale-builder")
	if err != nil || out.Status != "stale" {
		t.Fatalf("stale Builder Act = (status=%q, err=%v), want stale without agent spend", out.Status, err)
	}
	if err := os.RemoveAll(filepath.Join(repo, DefaultAgentsDir, "verifier")); err != nil {
		t.Fatal(err)
	}
	merged = true

	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement {
		t.Fatalf("retirement recovery Select = (subjects=%#v, err=%v)", subjects, err)
	}
	out, err = (verifierFlow{}).Act(cfg, repo, subjects[0], "recover")
	if err != nil || out.Status != "merged" {
		t.Fatalf("Verifier merged-Projection recovery = (status=%q, err=%v)", out.Status, err)
	}
	if createCalls != 0 || mergeCalls != 0 {
		t.Fatalf("recovery made create/merge calls = %d/%d", createCalls, mergeCalls)
	}
	if _, err := tk.Get("9"); err == nil {
		t.Fatal("recovery did not close the Tracker Item")
	}
}

func TestNewHostApprovalPreparesBeforeProjectedVerdict(t *testing.T) {
	branch := "forest/19-new-approval"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")
	if err := writeChecks(repo, reviewed, checksNote{Status: "pass", RunID: "seed"}); err != nil {
		t.Fatal(err)
	}
	origin := runGitTest(t, repo, "remote", "get-url", "origin")
	hook := filepath.Join(origin, "hooks", "update")
	retirement := retirementRef(branch, reviewed)
	script := "#!/bin/sh\ncase \"$1\" in\n  refs/notes/forest/verdict)\n    git show-ref --verify --quiet '" + retirement + "' || { echo 'Verdict published before Host retirement preparation' >&2; exit 1; }\n    ;;\nesac\nexit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	oldTracker := trackerFor
	tk := newMemoryTracker()
	tk.seed(Item{ID: "19", Title: "new approval"})
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldRun := runPhase
	runPhase = func(_ string, wtDir string, _ *Agent, _ string, _ string) (runStats, error) {
		if err := os.WriteFile(filepath.Join(wtDir, "review.json"), []byte(`{"verdict":"approve","summary":"approved","notes":""}`), 0o644); err != nil {
			return runStats{}, err
		}
		return runStats{}, nil
	}
	defer func() { runPhase = oldRun }()
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	projectionCommand = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[1] == "list" {
			return []byte(`[{"number":19,"url":"https://github.com/owner/repo/pull/19","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		}
		if len(args) >= 2 && args[1] == "comment" {
			return nil, nil
		}
		return nil, errors.New("unexpected Host command")
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = false
	out, err := (verifierFlow{}).Act(cfg, repo, Subject{
		Key: "branch-" + branch, Kind: subjectBranch, Revision: reviewed,
		ID: "19", Branch: branch, Head: reviewed,
	}, "new-approval")
	if err != nil || out.Status != "reviewed" {
		t.Fatalf("new Host approval = (status=%q, err=%v), want reviewed", out.Status, err)
	}
	facts, err := listRetirements(repo)
	if err != nil || len(facts) != 1 {
		t.Fatalf("new approval retirement facts = (%#v, %v), want exactly one", facts, err)
	}
	verdict, found, err := readVerdict(repo, reviewed)
	if err != nil || !found {
		t.Fatalf("new approval Verdict = (found=%v, err=%v), want durable Verdict", found, err)
	}
	if facts[0].Record.Agent != verdict.Reviewer || facts[0].Record.Model != verdict.Model || facts[0].Record.DefSHA != verdict.DefSHA {
		t.Fatalf("new approval attribution = %#v, want Verdict %#v", facts[0].Record, verdict)
	}
}

// TestPendingHostRetirementWaitsForOperatorMerge proves AutoMerge=false only
// observes an open Host request and never issues a merge command in recovery.
func TestPendingHostRetirementWaitsForOperatorMerge(t *testing.T) {
	branch := "forest/14-manual-pending"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	fact, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "14", Transport: "host",
		Strategy: "squash", Title: "manual pending", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeApprovalNotes(t, repo, reviewed, agent)
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	mergeCalls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		if len(args) < 2 {
			return nil, errors.New("unexpected Host command")
		}
		if args[1] == "merge" {
			mergeCalls++
			return nil, errors.New("unexpected manual Host merge")
		}
		if args[1] != "list" {
			return nil, errors.New("unexpected Host command")
		}
		state := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--state" {
				state = args[i+1]
			}
		}
		if state == "open" {
			return []byte(`[{"number":14,"url":"https://github.com/owner/repo/pull/14","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		}
		return []byte(`[]`), nil
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	if err := recoverRetirementFact(cfg, repo, fact, Item{ID: "14", Title: "manual pending"}); !errors.Is(err, errHostMergePending) {
		t.Fatalf("pending manual recovery = %v, want Host merge pending", err)
	}
	if mergeCalls != 0 {
		t.Fatalf("pending manual recovery issued %d Host merge calls", mergeCalls)
	}
}
func TestPendingHostRetirementWithoutApprovalReleasesFact(t *testing.T) {
	branch := "forest/17-unapproved-pending"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	if _, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "17", Transport: "host",
		Strategy: "squash", Title: "unapproved pending", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	}); err != nil {
		t.Fatal(err)
	}
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	hostCalls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		hostCalls++
		return nil, errors.New("unexpected Host call")
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	subject := Subject{
		Key: "branch-" + branch, Kind: subjectRetirement, Revision: reviewed,
		ID: "17", Branch: branch, Head: reviewed,
		Item: Item{ID: "17", Title: "unapproved pending"},
	}
	out, err := (verifierFlow{}).Act(cfg, repo, subject, "preparation-recovery")
	if err != nil || out.Status != "skipped" {
		t.Fatalf("unapproved pending recovery = (status=%q, err=%v), want skipped success", out.Status, err)
	}
	if hostCalls != 0 {
		t.Fatalf("unapproved pending recovery made %d Host calls", hostCalls)
	}
	if stalled, stallErr := stalledOn(repo, "verifier", subject.Key, reviewed); stallErr != nil || stalled {
		t.Fatalf("preparation recovery brake = (%v, %v), want no terminal brake", stalled, stallErr)
	}
	if facts, listErr := listRetirements(repo); listErr != nil || len(facts) != 0 {
		t.Fatalf("released pending facts = (%#v, %v), want none", facts, listErr)
	}
}

func TestPendingHostRetirementDropsNonMatchingApproval(t *testing.T) {
	tests := map[string]func(*testing.T, string, string, *Agent){
		"rejected Verdict": func(t *testing.T, repo, reviewed string, agent *Agent) {
			if err := writeChecks(repo, reviewed, checksNote{Status: "pass", RunID: "rejected"}); err != nil {
				t.Fatal(err)
			}
			if err := writeVerdict(repo, reviewed, verdictNote{
				Verdict: "changes", Reviewer: agent.Name, Model: agent.Model,
				DefSHA: agent.DefSHA, RunID: "rejected",
			}); err != nil {
				t.Fatal(err)
			}
		},
		"failing Checks": func(t *testing.T, repo, reviewed string, agent *Agent) {
			if err := writeChecks(repo, reviewed, checksNote{Status: "fail", RunID: "failed"}); err != nil {
				t.Fatal(err)
			}
			if err := writeVerdict(repo, reviewed, verdictNote{
				Verdict: "approve", Reviewer: agent.Name, Model: agent.Model,
				DefSHA: agent.DefSHA, RunID: "failed",
			}); err != nil {
				t.Fatal(err)
			}
		},
		"Agent mismatch": func(t *testing.T, repo, reviewed string, agent *Agent) {
			if err := writeChecks(repo, reviewed, checksNote{Status: "pass", RunID: "mismatch"}); err != nil {
				t.Fatal(err)
			}
			if err := writeVerdict(repo, reviewed, verdictNote{
				Verdict: "approve", Reviewer: "other-agent", Model: agent.Model,
				DefSHA: agent.DefSHA, RunID: "mismatch",
			}); err != nil {
				t.Fatal(err)
			}
		},
		"Model mismatch": func(t *testing.T, repo, reviewed string, agent *Agent) {
			if err := writeChecks(repo, reviewed, checksNote{Status: "pass", RunID: "mismatch"}); err != nil {
				t.Fatal(err)
			}
			if err := writeVerdict(repo, reviewed, verdictNote{
				Verdict: "approve", Reviewer: agent.Name, Model: "other-model",
				DefSHA: agent.DefSHA, RunID: "mismatch",
			}); err != nil {
				t.Fatal(err)
			}
		},
		"DefSHA mismatch": func(t *testing.T, repo, reviewed string, agent *Agent) {
			if err := writeChecks(repo, reviewed, checksNote{Status: "pass", RunID: "mismatch"}); err != nil {
				t.Fatal(err)
			}
			if err := writeVerdict(repo, reviewed, verdictNote{
				Verdict: "approve", Reviewer: agent.Name, Model: agent.Model,
				DefSHA: strings.Repeat("b", 16), RunID: "mismatch",
			}); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, prepareNotes := range tests {
		t.Run(name, func(t *testing.T) {
			branch := "forest/18-" + strings.ToLower(strings.ReplaceAll(name, " ", "-"))
			repo, _, reviewed, _ := newVerifierBranch(t, branch)
			agent := testVerifierAgent()
			fact, err := recordRetirement(repo, retirementRecord{
				Branch: branch, Revision: reviewed, ItemID: "18", Transport: "host",
				Strategy: "squash", Title: "approval", State: "pending",
				Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
			})
			if err != nil {
				t.Fatal(err)
			}
			prepareNotes(t, repo, reviewed, agent)
			oldProjection := projectionCommand
			defer func() { projectionCommand = oldProjection }()
			hostCalls := 0
			projectionCommand = func(args ...string) ([]byte, error) {
				hostCalls++
				return nil, errors.New("unexpected Host call")
			}
			cfg := defaultConfig()
			cfg.Repo = "owner/repo"
			cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
			err = recoverRetirementFact(cfg, repo, fact, Item{ID: "18", Title: "approval"})
			if !errors.Is(err, errRetirementPreparation) {
				t.Fatalf("non-matching approval recovery = %v, want preparation sentinel", err)
			}
			if hostCalls != 0 {
				t.Fatalf("non-matching approval recovery made %d Host calls", hostCalls)
			}
			if facts, listErr := listRetirements(repo); listErr != nil || len(facts) != 0 {
				t.Fatalf("non-matching approval facts = (%#v, %v), want none", facts, listErr)
			}
		})
	}
}

func TestMergeViaHostRecoversTrackerCloseFailure(t *testing.T) {
	branch := "forest/9-host-recovery"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	merged := false
	mergeCalls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		state := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--state" {
				state = args[i+1]
			}
		}
		switch {
		case len(args) > 0 && args[0] == "api" && merged:
			return mergedProjectionPage(`{"number":9,"html_url":"https://github.com/owner/repo/pull/9","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"` + reviewed + `","ref":"` + branch + `","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}`), nil
		case len(args) > 0 && args[0] == "api":
			return []byte(`[]`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "merge":
			merged = true
			mergeCalls++
			if err := deleteRef(repo, "refs/heads/"+branch, reviewed); err != nil {
				return nil, err
			}
			return nil, nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
			switch {
			case state == "open" && !merged:
				return []byte(`[{"number":9,"url":"https://github.com/owner/repo/pull/9","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
			case state == "merged" && merged:
				return []byte(`[]`), nil
			default:
				return []byte(`[]`), nil
			}
		default:
			return nil, errors.New("unexpected host command")
		}
	}

	oldGH := ghJSON
	defer func() { ghJSON = oldGH }()
	closeCalls := 0
	ghJSON = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "issue" && args[1] == "list" {
			return []byte(`[]`), nil
		}
		if len(args) >= 2 && args[0] == "issue" && args[1] == "close" {
			closeCalls++
			if closeCalls == 1 {
				return nil, errors.New("tracker unavailable")
			}
		}
		return []byte(`{}`), nil
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Merge = "squash"
	cfg.Flows.Verifier.AutoMerge = true
	item := Item{ID: "9", Title: "change"}
	reviewAgent := testVerifierAgent()
	writeApprovalNotes(t, repo, reviewed, reviewAgent)
	if err := mergeVerified(cfg, repo, branch, reviewed, item,
		reviewAgent); err == nil ||
		!strings.Contains(err.Error(), "close item") {
		t.Fatalf("first host merge = %v, want Tracker close failure", err)
	}
	// A landed retirement fact is sufficient after restart. Remove the Host
	// oracle before cloning so recovery cannot reuse process-local merge state
	// or ask the Host to merge a second time.
	projectionCommand = func(...string) ([]byte, error) {
		return nil, errors.New("Host called during landed retirement recovery")
	}
	if out := runGitTest(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out != "" {
		t.Fatalf("host did not auto-delete source branch: %s", out)
	}
	restartRoot := t.TempDir()
	restarted := filepath.Join(restartRoot, "restart")
	origin := runGitTest(t, repo, "remote", "get-url", "origin")
	runGitTest(t, restartRoot, "clone", "-q", origin, restarted)
	subjects, err := (verifierFlow{}).Select(cfg, restarted)
	if err != nil {
		t.Fatalf("restart select: %v", err)
	}
	if len(subjects) != 1 || subjects[0].Kind != "retirement" {
		t.Fatalf("recovery subjects = %#v, want one retirement", subjects)
	}
	out, err := (verifierFlow{}).Act(cfg, restarted, subjects[0], "recovery-run")
	if err != nil {
		t.Fatalf("host merge recovery: %v", err)
	}
	if out.Status != "merged" {
		t.Fatalf("recovery status = %q, want merged", out.Status)
	}
	agent := testVerifierAgent()
	if out.Agent != agent.Name || out.Model != agent.Model || out.DefSHA != agent.DefSHA {
		t.Fatalf("host recovery attribution = %#v, want marker identity %#v", out, agent)
	}
	if mergeCalls != 1 || closeCalls != 2 {
		t.Fatalf("recovery effects: merge=%d close=%d, want merge=1 close=2", mergeCalls, closeCalls)
	}
	if facts, err := listRetirements(restarted); err != nil || len(facts) != 0 {
		t.Fatalf("retirement facts after recovery = (%#v, %v), want none", facts, err)
	}
}

func TestPendingHostRetirementRecoversAfterBranchAutoDelete(t *testing.T) {
	branch := "forest/10-pending-host"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	if _, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "10", Transport: "host",
		Strategy: "squash", Title: "pending host", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	}); err != nil {
		t.Fatal(err)
	}
	if err := deleteRef(repo, "refs/heads/"+branch, reviewed); err != nil {
		t.Fatal(err)
	}
	writeApprovalNotes(t, repo, reviewed, agent)
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	mergeCalls := 0

	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "api":
			return mergedProjectionPage(`{"number":10,"html_url":"https://github.com/owner/repo/pull/10","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"` + reviewed + `","ref":"` + branch + `","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "merge":
			mergeCalls++
			return nil, errors.New("recovery attempted a duplicate Host merge")
		case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
			return []byte(`[]`), nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	oldGH := ghJSON
	defer func() { ghJSON = oldGH }()
	closeCalls := 0
	ghJSON = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "issue" && args[1] == "list" {
			return []byte(`[]`), nil
		}
		if len(args) >= 2 && args[0] == "issue" && args[1] == "close" {
			closeCalls++
		}
		return []byte(`{}`), nil
	}

	restartRoot := t.TempDir()
	restarted := filepath.Join(restartRoot, "restart")
	origin := runGitTest(t, repo, "remote", "get-url", "origin")
	runGitTest(t, restartRoot, "clone", "-q", origin, restarted)
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Merge = "squash"
	subjects, err := (verifierFlow{}).Select(cfg, restarted)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].Kind != "retirement" {
		t.Fatalf("recovery subjects = %#v, want one retirement", subjects)
	}
	if _, err := (verifierFlow{}).Act(cfg, restarted, subjects[0], "pending-recovery"); err != nil {
		t.Fatalf("pending retirement recovery: %v", err)
	}
	if mergeCalls != 0 || closeCalls != 1 {
		t.Fatalf("recovery effects: merge=%d close=%d, want merge=0 close=1", mergeCalls, closeCalls)
	}
	if facts, err := listRetirements(restarted); err != nil || len(facts) != 0 {
		t.Fatalf("retirement facts after recovery = (%#v, %v), want none", facts, err)
	}
}

// TestPendingHostRetirementObservesRecordedStrategy proves recovery uses the
// recorded merge strategy and exact reviewed head on a retry.
func TestPendingHostRetirementObservesRecordedStrategy(t *testing.T) {
	branch := "forest/11-strategy"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	fact, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "11", Transport: "host",
		Strategy: "squash", Title: "strategy", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeApprovalNotes(t, repo, reviewed, agent)
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	mergeCalls, listCalls := 0, 0
	var mergeArgs []string
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "api":
			return []byte(`[]`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "merge":
			mergeCalls++
			mergeArgs = append([]string(nil), args...)
			return nil, errors.New("Host merge queued")
		case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
			listCalls++
			return []byte(`[{"number":11,"url":"https://github.com/owner/repo/pull/11","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Merge = "ff"
	cfg.Flows.Verifier.AutoMerge = true
	err = recoverRetirementFact(cfg, repo, fact, Item{ID: "11", Title: "strategy"})
	if !errors.Is(err, errHostMergePending) {
		t.Fatalf("pending retirement retry = %v, want merge pending", err)
	}
	if mergeCalls != 1 || listCalls == 0 {
		t.Fatalf("recovery effects: merge=%d list=%d, want one merge and observation", mergeCalls, listCalls)
	}
	if !hasArgumentPair(mergeArgs, "--match-head-commit", reviewed) {
		t.Fatalf("queued Host merge args %v do not pin reviewed Revision %s", mergeArgs, reviewed)
	}
}

func TestRetirementFactBlocksAdvancedBranch(t *testing.T) {
	branch := "forest/12-advanced"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	if _, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "12", Transport: "host",
		Strategy: "squash", Title: "advanced", State: "landed",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	}); err != nil {
		t.Fatal(err)
	}
	rebaseTestWriteFile(t, filepath.Join(repo, "later.txt"), "later\n")
	runGitTest(t, repo, "add", "later.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "later")
	runGitTest(t, repo, "push", "-q", "origin", branch)
	advanced := remoteBranchHead(t, repo, branch)

	oldGH := ghJSON
	defer func() { ghJSON = oldGH }()
	closeCalls := 0
	ghJSON = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "issue" && args[1] == "close" {
			closeCalls++
		}
		return []byte(`{}`), nil
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].Kind != subjectRetirement || subjects[0].Revision != reviewed {
		t.Fatalf("advanced retirement subjects = %#v, want old durable fact", subjects)
	}
	if _, err := (verifierFlow{}).Act(cfg, repo, subjects[0], "advanced-retirement"); err == nil ||
		!strings.Contains(err.Error(), "advanced") {
		t.Fatalf("advanced retirement = %v, want named refusal", err)
	}
	if closeCalls != 0 {
		t.Fatalf("advanced retirement closed Tracker %d time(s)", closeCalls)
	}
	if got := remoteBranchHead(t, repo, branch); got != advanced {
		t.Fatalf("advanced branch = %s, want %s intact", got, advanced)
	}
	for range stalledRunLimit {
		if err := recordStalled(repo, "verifier", "branch-"+branch, reviewed); err != nil {
			t.Fatal(err)
		}
	}
	subjects, err = (verifierFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 0 {
		t.Fatalf("stalled retirement remained selectable: %#v", subjects)
	}
}

func TestRetirementRecoveryRejectsMismatchedItem(t *testing.T) {
	repo, branch, reviewed, _ := newVerifierBranch(t, "forest/12-mismatch")
	agent := testVerifierAgent()
	fact, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "12", Transport: "git",
		Strategy: "squash", Title: "mismatch", State: "landed",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	oldGH := ghJSON
	defer func() { ghJSON = oldGH }()
	hostCalls := 0
	ghJSON = func(args ...string) ([]byte, error) {
		hostCalls++
		return nil, errors.New("unexpected Host call")
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	err = recoverRetirementFact(cfg, repo, fact, Item{ID: "99", Title: "wrong Item"})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched Item recovery = %v, want refusal", err)
	}
	if hostCalls != 0 {
		t.Fatalf("mismatched Item recovery made %d Host calls", hostCalls)
	}
	if got := remoteBranchHead(t, repo, branch); got != reviewed {
		t.Fatalf("mismatched Item recovery changed branch to %s, want %s", got, reviewed)
	}
	if facts, err := listRetirements(repo); err != nil || len(facts) != 1 {
		t.Fatalf("retirement facts after refusal = (%#v, %v), want one", facts, err)
	}
}

func TestPendingHostRetirementRetriesVisibleOpenMerge(t *testing.T) {
	branch := "forest/13-queued"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	fact, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "13", Transport: "host",
		Strategy: "squash", Title: "queued", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	tk := newMemoryTracker()
	tk.seed(Item{ID: "13", Title: "queued"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	writeApprovalNotes(t, repo, reviewed, agent)
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	merged := false
	mergeCalls := 0
	var mergeHeads []string
	projectionCommand = func(args ...string) ([]byte, error) {
		state := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--state" {
				state = args[i+1]
			}
		}
		pr := `[{"number":13,"url":"https://github.com/owner/repo/pull/13","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`
		switch {
		case len(args) > 0 && args[0] == "api" && merged:
			return mergedProjectionPage(`{"number":13,"html_url":"https://github.com/owner/repo/pull/13","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"` + reviewed + `","ref":"` + branch + `","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}`), nil
		case len(args) > 0 && args[0] == "api":
			return []byte(`[]`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "merge":
			mergeCalls++
			if hasArgumentPair(args, "--match-head-commit", reviewed) {
				mergeHeads = append(mergeHeads, reviewed)
			}
			return nil, nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
			if state == "open" && !merged {
				return []byte(pr), nil
			}
			return []byte(`[]`), nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Merge = "squash"
	cfg.Flows.Verifier.AutoMerge = true
	item := Item{ID: "13", Title: "queued"}

	for pass := range 2 {
		err = recoverRetirementFact(cfg, repo, fact, item)
		if !errors.Is(err, errHostMergePending) {
			t.Fatalf("queued recovery pass %d = %v, want pending", pass+1, err)
		}
	}
	if mergeCalls != 2 {
		t.Fatalf("queued recovery merge effects = %d, want one exact merge per pass", mergeCalls)
	}
	if len(mergeHeads) != mergeCalls {
		t.Fatalf("queued merge heads = %v, want reviewed head on every attempt", mergeHeads)
	}

	merged = true
	if err := recoverRetirementFact(cfg, repo, fact, item); err != nil {
		t.Fatalf("observed queued merge recovery: %v", err)
	}
	if mergeCalls != 2 {
		t.Fatalf("observed recovery repeated %d merge effects", mergeCalls)
	}
	if _, err := tk.Get(item.ID); err == nil {
		t.Fatal("observed recovery did not close the Tracker Item")
	}
	if facts, err := listRetirements(repo); err != nil || len(facts) != 0 {
		t.Fatalf("queued retirement facts after observation = (%#v, %v), want none", facts, err)
	}
}

func TestPendingHostRetirementNoViewAfterAcceptedMergeRetainsIntent(t *testing.T) {
	branch := "forest/20-no-view"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	item := Item{ID: "20", Title: "no-view"}
	tk := newMemoryTracker()
	tk.seed(item)
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	writeApprovalNotes(t, repo, reviewed, agent)

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	phase := "initial"
	createCalls, mergeCalls := 0, 0
	projectionCommand = func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "api" {
			if phase == "merged" {
				return mergedProjectionPage(`{"number":20,"html_url":"https://github.com/owner/repo/pull/20","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"` + reviewed + `","ref":"` + branch + `","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}`), nil
			}
			return []byte(`[]`), nil
		}
		if len(args) >= 2 && args[0] == "pr" && args[1] == "create" {
			createCalls++
			phase = "open"
			return []byte("https://github.com/owner/repo/pull/20"), nil
		}
		if len(args) >= 2 && args[0] == "pr" && args[1] == "merge" {
			mergeCalls++
			if !hasArgumentPair(args, "--match-head-commit", reviewed) {
				t.Fatalf("Host merge args %v do not pin reviewed Revision %s", args, reviewed)
			}
			phase = "no-view"
			return nil, nil
		}
		if len(args) >= 2 && args[0] == "pr" && args[1] == "list" {
			if phase == "open" {
				return []byte(`[{"number":20,"url":"https://github.com/owner/repo/pull/20","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
			}
			return []byte(`[]`), nil
		}
		return nil, errors.New("unexpected Host command")
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Merge = "squash"
	cfg.Flows.Verifier.AutoMerge = true
	verdict := verdictNote{Verdict: "approve", Reviewer: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA}
	if err := recordHostRetirement(cfg, repo, branch, reviewed, item, verdict); err != nil {
		t.Fatalf("initial Host retirement preparation: %v", err)
	}
	if createCalls != 1 {
		t.Fatalf("initial Host projection creates = %d, want one", createCalls)
	}
	fact, found, err := readRetirement(repo, branch, reviewed)
	if err != nil || !found || fact.Record.State != "pending" {
		t.Fatalf("initial pending fact = (found=%v, state=%q, err=%v)", found, fact.Record.State, err)
	}

	if err := recoverRetirementFact(cfg, repo, fact, item); !errors.Is(err, errHostMergePending) {
		t.Fatalf("accepted Host merge with no post-request view = %v, want pending", err)
	}
	if createCalls != 1 || mergeCalls != 1 {
		t.Fatalf("post-request no-view effects = create %d, merge %d; want one each", createCalls, mergeCalls)
	}

	subject := Subject{
		Key: "branch-" + branch, Kind: subjectRetirement, Revision: reviewed,
		ID: item.ID, Branch: branch, Head: reviewed, Item: item,
	}
	if code := actOnSubject(verifierFlow{}, cfg, repo, subject, nil); code != 0 {
		t.Fatalf("next no-view retirement retry code = %d, want success", code)
	}
	runs, invalid, err := loadLedger(ledgerPath(repo))
	if err != nil || invalid != 0 || len(runs) == 0 || runs[len(runs)-1].Status != "merge_pending" {
		t.Fatalf("no-view retry Ledger = (runs=%#v, invalid=%d, err=%v), want merge_pending", runs, invalid, err)
	}
	if createCalls != 1 || mergeCalls != 1 {
		t.Fatalf("no-view retry effects = create %d, merge %d; want no new Host writes", createCalls, mergeCalls)
	}
	retained, found, err := readRetirement(repo, branch, reviewed)
	if err != nil || !found || retained.Record.State != "pending" {
		t.Fatalf("no-view pending fact = (found=%v, state=%q, err=%v)", found, retained.Record.State, err)
	}
	if _, err := tk.Get(item.ID); err != nil {
		t.Fatalf("no-view retry retired Tracker Item: %v", err)
	}
	if got := remoteBranchHead(t, repo, branch); got != reviewed {
		t.Fatalf("no-view retry branch = %s, want reviewed Revision %s", got, reviewed)
	}
	if facts, err := listRetirements(repo); err != nil || len(facts) != 1 {
		t.Fatalf("no-view retirement facts = (%#v, %v), want one retained fact", facts, err)
	}

	phase = "merged"
	if err := recoverRetirementFact(cfg, repo, fact, item); err != nil {
		t.Fatalf("exact merged recovery: %v", err)
	}
	if createCalls != 1 || mergeCalls != 1 {
		t.Fatalf("exact merged recovery effects = create %d, merge %d; want no new Host writes", createCalls, mergeCalls)
	}
	if _, err := tk.Get(item.ID); err == nil {
		t.Fatal("exact merged recovery did not retire Tracker Item")
	}
	if facts, err := listRetirements(repo); err != nil || len(facts) != 0 {
		t.Fatalf("exact merged retirement facts = (%#v, %v), want none", facts, err)
	}
}
