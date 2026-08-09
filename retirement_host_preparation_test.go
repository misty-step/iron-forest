package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNewHostApprovalPreparesBeforeProjectedVerdict(t *testing.T) {
	branch := "forest/19-new-approval"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")
	if err := writeChecks(repo, reviewed, checksNote{Status: "pass", RunID: "seed"}, testCommitIdentity()); err != nil {
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
	created := false
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "pr" && args[1] == "list" && !created:
			return []byte(`[]`), nil
		case args[0] == "pr" && args[1] == "list":
			return []byte(`[{"number":19,"url":"https://github.com/owner/repo/pull/19","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		case args[0] == "api" && hasArgumentPair(args, "--method", "GET"):
			return []byte(`[[]]`), nil
		case args[0] == "pr" && args[1] == "create":
			fact, found, err := readRetirement(repo, branch, reviewed)
			if err != nil || !found || fact.Record.State != "preparing" {
				t.Fatalf("Host create preparation = (%#v, %v, %v), want durable preparing fact", fact, found, err)
			}
			created = true
			return []byte("https://github.com/owner/repo/pull/19"), nil
		case args[0] == "api" && hasArgumentPair(args, "--method", "POST"):
			return nil, nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = false
	out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: subjectBranch, Revision: reviewed,
		ID: "19", Branch: branch}, "new-approval")
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
func TestPreparingHostRetirementPreservesMergedPriorRevision(t *testing.T) {
	branch := "forest/33-prior-merged"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	item := Item{ID: "33", Title: "prior merged"}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	if _, err := recordPreparingHostRetirement(cfg, repo, branch, reviewed, item); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "checkout", "-q", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "advanced.txt"), "advanced\n")
	runGitTest(t, repo, "add", "advanced.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "advance")
	runGitTest(t, repo, "push", "-q", "origin", branch)
	advanced := remoteBranchHead(t, repo, branch)
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	projectionCommand = func(args ...string) ([]byte, error) {
		if args[0] == "api" {
			return mergedProjectionPage(`{"number":33,"html_url":"https://github.com/owner/repo/pull/33","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"` +
				reviewed + `","ref":"` + branch + `","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}`), nil
		}
		return nil, errors.New("unexpected Host command")
	}
	if _, err := recordPreparingHostRetirement(cfg, repo, branch, advanced, item); !errors.Is(err, errHostMergePending) {
		t.Fatalf("advance over merged preparation = %v, want retained merge observation", err)
	}
	oldFact, found, err := readRetirement(repo, branch, reviewed)
	if err != nil || !found || oldFact.Record.State != "observed" {
		t.Fatalf("prior merged fact = (%#v, %v, %v), want observed", oldFact, found, err)
	}
	if _, found, err := readRetirement(repo, branch, advanced); err != nil || found {
		t.Fatalf("advanced preparation = (found=%v, err=%v), want none", found, err)
	}
}
func TestPreparingHostRetirementMovesRevisionAtomically(t *testing.T) {
	branch := "forest/34-atomic-move"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	item := Item{ID: "34", Title: "atomic move"}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	if _, err := recordPreparingHostRetirement(cfg, repo, branch, reviewed, item); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "checkout", "-q", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "advanced.txt"), "advanced\n")
	runGitTest(t, repo, "add", "advanced.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "advance")
	runGitTest(t, repo, "push", "-q", "origin", branch)
	advanced := remoteBranchHead(t, repo, branch)
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "api":
			return []byte(`[[]]`), nil
		case args[0] == "pr" && args[1] == "list":
			return []byte(`[{"number":34,"url":"https://github.com/owner/repo/pull/34","headRefOid":"` +
				advanced + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	origin := runGitTest(t, repo, "remote", "get-url", "origin")
	hook := filepath.Join(origin, "hooks", "update")
	newRef := retirementRef(branch, advanced)
	script := "#!/bin/sh\nif [ \"$1\" = '" + newRef + "' ]; then exit 1; fi\nexit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := recordPreparingHostRetirement(cfg, repo, branch, advanced, item); err == nil {
		t.Fatal("rejected atomic migration returned success")
	}
	if _, found, err := readRetirement(repo, branch, reviewed); err != nil || !found {
		t.Fatalf("rejected migration lost old fact = (found=%v, err=%v)", found, err)
	}
	if _, found, err := readRetirement(repo, branch, advanced); err != nil || found {
		t.Fatalf("rejected migration published new fact = (found=%v, err=%v)", found, err)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if fact, err := recordPreparingHostRetirement(cfg, repo, branch, advanced, item); err != nil ||
		fact.Record.Revision != advanced || fact.Record.BuiltComment {
		t.Fatalf("atomic migration retry = (%#v, %v), want incomplete new-Revision comment", fact, err)
	}
	if _, found, err := readRetirement(repo, branch, reviewed); err != nil || found {
		t.Fatalf("atomic migration retained old fact = (found=%v, err=%v)", found, err)
	}
}
func TestPreparingHostRetirementMovesBeforeFirstProjection(t *testing.T) {
	branch := "forest/39-pre-projection-move"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	item := Item{ID: "39", Title: "move before Projection"}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	fact, err := recordPreparingHostRetirement(cfg, repo, branch, reviewed, item)
	if err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "checkout", "-q", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "advanced.txt"), "advanced\n")
	runGitTest(t, repo, "add", "advanced.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "advance before Projection")
	runGitTest(t, repo, "push", "-q", "origin", branch)
	advanced := remoteBranchHead(t, repo, branch)

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "api":
			return []byte(`[[]]`), nil
		case args[0] == "pr" && args[1] == "list":
			return []byte(`[]`), nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	record, err := recoverRetirement(cfg, repo, fact, item)
	if !errors.Is(err, errRetirementPreparation) ||
		record.Revision != advanced || record.State != "preparing" || record.BuiltComment {
		t.Fatalf("pre-Projection preparation recovery = (%#v, %v), want incomplete new-Revision comment",
			record, err)
	}
	if _, found, err := readRetirement(repo, branch, reviewed); err != nil || found {
		t.Fatalf("pre-Projection move retained old fact = (found=%v, err=%v)", found, err)
	}
}
func TestPreparingHostRetirementRecreatesMissingProjection(t *testing.T) {
	branch := "forest/42-recreate-projection"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	item := Item{ID: "42", Title: "recreate Projection"}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	fact, err := recordPreparingHostRetirement(cfg, repo, branch, reviewed, item)
	if err != nil {
		t.Fatal(err)
	}

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	created := false
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "api":
			return []byte(`[[]]`), nil
		case args[0] == "pr" && args[1] == "list":
			if !created {
				return []byte(`[]`), nil
			}
			return []byte(`[{"number":42,"url":"https://github.com/owner/repo/pull/42","headRefOid":"` +
				reviewed + `","headRefName":"` + branch +
				`","baseRefName":"master","isCrossRepository":false}]`), nil
		case args[0] == "pr" && args[1] == "create":
			if created {
				return nil, errors.New("duplicate Projection")
			}
			created = true
			return []byte("https://github.com/owner/repo/pull/42"), nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}

	record, err := recoverRetirement(cfg, repo, fact, item)
	if !errors.Is(err, errRetirementPreparation) || !created ||
		record.Revision != reviewed || record.State != "preparing" {
		t.Fatalf("missing Projection recovery = (%#v, created=%v, err=%v)", record, created, err)
	}
	retained, found, readErr := readRetirement(repo, branch, reviewed)
	if readErr != nil || !found || retained.Record.State != "preparing" {
		t.Fatalf("recreated Projection fact = (%#v, found=%v, err=%v)", retained, found, readErr)
	}
}
func TestPreparingHostRetirementRetriesTransientBranchLookup(t *testing.T) {
	branch := "forest/45-transient-branch"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	item := Item{ID: "45", Title: "transient branch"}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	fact, err := recordPreparingHostRetirement(cfg, repo, branch, reviewed, item)
	if err != nil {
		t.Fatal(err)
	}

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "api":
			return []byte(`[[]]`), nil
		case args[0] == "pr" && args[1] == "list":
			return []byte(`[]`), nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	wrapper := filepath.Join(wrapperDir, "git")
	if err := os.WriteFile(wrapper, []byte(`#!/bin/sh
case " $* " in
  *" ls-remote "*) echo "transient branch lookup failure" >&2; exit 1 ;;
esac
exec "$FOREST_REAL_GIT" "$@"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOREST_REAL_GIT", realGit)
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	record, err := recoverRetirement(cfg, repo, fact, item)
	if !errors.Is(err, errHostMergePending) ||
		record.Revision != reviewed || record.State != "preparing" {
		t.Fatalf("transient branch lookup recovery = (%#v, %v), want pending preparation", record, err)
	}
}
func TestPendingHostRetirementMovesToAdvancedProjectionForFreshReview(t *testing.T) {
	branch := "forest/40-pending-drift"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	if _, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "40", Transport: "host",
		Strategy: "squash", Title: "pending drift", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	}); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "checkout", "-q", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "advanced.txt"), "advanced\n")
	runGitTest(t, repo, "add", "advanced.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "advance pending Projection")
	runGitTest(t, repo, "push", "-q", "origin", branch)
	advanced := remoteBranchHead(t, repo, branch)

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "api":
			return []byte(`[[]]`), nil
		case args[0] == "pr" && args[1] == "list":
			return []byte(`[{"number":40,"url":"https://github.com/owner/repo/pull/40","headRefOid":"` +
				advanced + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: retirementSubjectKey(branch), Kind: subjectRetirement, Revision: reviewed,
		ID: "40", Branch: branch, Item: Item{ID: "40", Title: "pending drift"}}, "pending-drift")
	if err != nil || out.Status != "skipped" {
		t.Fatalf("pending drift Act = (status=%q, err=%v), want preparation handoff", out.Status, err)
	}
	fact, found, err := readRetirement(repo, branch, advanced)
	if err != nil || !found || fact.Record.State != "preparing" ||
		fact.Record.Agent != "" || fact.Record.Model != "" || fact.Record.DefSHA != "" {
		t.Fatalf("advanced preparation = (%#v, found=%v, err=%v)", fact, found, err)
	}
	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement ||
		subjects[0].Revision != advanced {
		t.Fatalf("advanced review Select = (%#v, %v), want fresh retirement Revision", subjects, err)
	}
}
func TestPendingHostRetirementIncompleteApprovalRetainsPreparation(t *testing.T) {
	tests := map[string]func(*testing.T, string, string, *Agent){
		"no notes": nil,
		"Verdict only": func(t *testing.T, repo, reviewed string, agent *Agent) {
			if err := writeVerdict(repo, reviewed, verdictNote{
				Verdict: "approve", Reviewer: agent.Name, Model: agent.Model,
				DefSHA: agent.DefSHA, RunID: "verdict-only",
			}, testCommitIdentity()); err != nil {
				t.Fatal(err)
			}
		},
		"Checks only": func(t *testing.T, repo, reviewed string, _ *Agent) {
			if err := writeChecks(repo, reviewed, checksNote{Status: "pass", RunID: "checks-only"}, testCommitIdentity()); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, prepare := range tests {
		t.Run(name, func(t *testing.T) {
			branch := "forest/17-" + slug(name)
			repo, _, reviewed, _ := newVerifierBranch(t, branch)
			agent := testVerifierAgent()
			if _, err := recordRetirement(repo, retirementRecord{
				Branch: branch, Revision: reviewed, ItemID: "17", Transport: "host",
				Strategy: "squash", Title: name, State: "pending",
				Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
			}); err != nil {
				t.Fatal(err)
			}
			if prepare != nil {
				prepare(t, repo, reviewed, agent)
			}
			oldProjection := projectionCommand
			defer func() { projectionCommand = oldProjection }()
			mergeCalls := 0
			projectionCommand = func(args ...string) ([]byte, error) {
				switch {
				case args[0] == "api":
					return []byte(`[]`), nil
				case args[0] == "pr" && args[1] == "list":
					return []byte(`[{"number":17,"url":"https://github.com/owner/repo/pull/17","headRefOid":"` +
						reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
				case args[0] == "pr" && args[1] == "merge":
					mergeCalls++
					return nil, errors.New("unexpected Host merge")
				default:
					return nil, errors.New("unexpected Host call")
				}
			}
			cfg := defaultConfig()
			cfg.Repo = "owner/repo"
			cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
			subject := Subject{Key: retirementSubjectKey(branch), Kind: subjectRetirement, Revision: reviewed,
				ID: "17", Branch: branch, Item: Item{ID: "17", Title: name}}
			out, err := (verifierFlow{}).Act(cfg, repo, subject, "preparation-recovery")
			if err != nil || out.Status != "skipped" {
				t.Fatalf("incomplete approval recovery = (status=%q, err=%v), want skipped success", out.Status, err)
			}
			if mergeCalls != 0 {
				t.Fatalf("incomplete approval issued %d Host merges", mergeCalls)
			}
			facts, listErr := listRetirements(repo)
			if listErr != nil || len(facts) != 1 || facts[0].Record.State != "preparing" ||
				facts[0].Record.Agent != "" || facts[0].Record.Model != "" || facts[0].Record.DefSHA != "" {
				t.Fatalf("retained preparation = (%#v, %v), want one unattributed preparing fact", facts, listErr)
			}
			subjects, selectErr := (verifierFlow{}).Select(cfg, repo)
			if selectErr != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement {
				t.Fatalf("preparing retirement Select = (%#v, %v), want durable retirement work", subjects, selectErr)
			}
		})
	}
}
func TestPreparingHostRetirementWithoutBranchOrViewHardBrakes(t *testing.T) {
	branch := "forest/20-lost-preparation"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	item := Item{ID: "20", Title: "lost preparation"}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	fact, err := recordPreparingHostRetirement(cfg, repo, branch, reviewed, item)
	if err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "checkout", "-q", "master")
	runGitTest(t, repo, "branch", "-D", branch)
	if err := deleteRef(repo, "refs/heads/"+branch, reviewed); err != nil {
		t.Fatal(err)
	}

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "api":
			return []byte(`[]`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
			return []byte(`[]`), nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	if _, err := recoverRetirement(cfg, repo, fact, item); !errors.Is(err, errRetirementRecoveryHard) {
		t.Fatalf("lost preparing retirement = %v, want hard recovery brake", err)
	}
}
