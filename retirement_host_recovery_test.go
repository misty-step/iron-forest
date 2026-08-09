package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifierRecoversAlreadyMergedProjectionWithoutDuplicate(t *testing.T) {
	branch := "forest/9-already-merged"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")
	if err := writeChecks(repo, reviewed, checksNote{Status: "pass", RunID: "seed"}, testCommitIdentity()); err != nil {
		t.Fatal(err)
	}
	if err := writeVerdict(repo, reviewed, verdictNote{
		Verdict: "approve", Reviewer: "verifier", Model: "verifier-model", DefSHA: strings.Repeat("a", 16), RunID: "seed",
	}, testCommitIdentity()); err != nil {
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
	out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: "branch", Revision: reviewed,
		ID: "9", Branch: branch}, "intent")
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
func TestPendingHostRetirementRefreshesNotesAfterSelect(t *testing.T) {
	branch := "forest/21-stale-notes"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	if _, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "21", Transport: "host",
		Strategy: "squash", Title: "stale notes", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	}); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	staleRepo := filepath.Join(root, "stale")
	origin := runGitTest(t, repo, "remote", "get-url", "origin")
	runGitTest(t, root, "clone", "-q", origin, staleRepo)
	tk := newMemoryTracker()
	tk.seed(Item{ID: "21", Title: "stale notes"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.AutoMerge = false

	subjects, err := (verifierFlow{}).Select(cfg, staleRepo)
	if err != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement {
		t.Fatalf("stale-note Select = (%#v, %v), want one retirement", subjects, err)
	}
	writeApprovalNotes(t, repo, reviewed, agent)
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "api":
			return []byte(`[]`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
			return []byte(`[{"number":21,"url":"https://github.com/owner/repo/pull/21","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	out, err := (verifierFlow{}).Act(cfg, staleRepo, subjects[0], "stale-note-recovery")
	if err != nil || out.Status != "merge_pending" {
		t.Fatalf("stale-note recovery = (status=%q, err=%v), want merge_pending", out.Status, err)
	}
	if facts, err := listRetirements(staleRepo); err != nil || len(facts) != 1 {
		t.Fatalf("stale-note retirement facts = (%#v, %v), want retained fact", facts, err)
	}
}
func TestPendingHostRetirementTransientNoteReadDoesNotBrake(t *testing.T) {
	branch := "forest/23-transient-note"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	if _, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "23", Transport: "host",
		Strategy: "squash", Title: "transient note", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	}); err != nil {
		t.Fatal(err)
	}
	tk := newMemoryTracker()
	tk.seed(Item{ID: "23", Title: "transient note"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "api":
			return []byte(`[]`), nil
		case args[0] == "pr" && args[1] == "list":
			return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefOid":"` +
				reviewed + `","headRefName":"` + branch +
				`","baseRefName":"master","isCrossRepository":false}]`), nil
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
  *" notes "*" show "*) echo "transient notes read failure" >&2; exit 1 ;;
esac
exec "$FOREST_REAL_GIT" "$@"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOREST_REAL_GIT", realGit)
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	subject := Subject{Key: retirementSubjectKey(branch), Kind: subjectRetirement, Revision: reviewed,
		ID: "23", Branch: branch, Item: Item{ID: "23", Title: "transient note"}}
	for range stalledRunLimit {
		if code := actOnSubject(verifierFlow{}, cfg, repo, subject, nil); code != 0 {
			t.Fatalf("transient note-read pass code = %d, want pending success", code)
		}
	}
	if stalled, err := stalledOn(repo, "verifier", subject.Key, reviewed); err != nil || stalled {
		t.Fatalf("transient note read stalled = (%v, %v), want retryable", stalled, err)
	}
	facts, err := listRetirements(repo)
	if err != nil || len(facts) != 1 || facts[0].Record.State != "pending" {
		t.Fatalf("transient note read retirement = (%#v, %v), want pending fact", facts, err)
	}
}
func TestMergedHostRetirementNoteReadFailureRecordsObservation(t *testing.T) {
	branch := "forest/22-bad-note"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	_, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "22", Transport: "host",
		Strategy: "squash", Title: "bad note", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeApprovalNotes(t, repo, reviewed, agent)
	runGitTest(t, repo, "notes", "--ref="+verdictNotesRef, "add", "-f", "-m", "{", reviewed)
	runGitTest(t, repo, "push", "-q", "--force", "origin", notesRef(verdictNotesRef))

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	projectionCommand = func(args ...string) ([]byte, error) {
		if args[0] == "api" {
			mergedAt := "2026-08-08T00:00:00Z"
			return []byte(`[[{"number":22,"html_url":"https://github.com/owner/repo/pull/22","merged_at":"` +
				mergedAt + `","head":{"sha":"` + reviewed + `","ref":"` + branch +
				`","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}]]`), nil
		}
		if args[0] == "pr" && args[1] == "list" {
			return []byte(`[{"number":22,"url":"https://github.com/owner/repo/pull/22","headRefOid":"` +
				reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		}
		return nil, errors.New("unexpected Host command")
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	subject := Subject{Key: retirementSubjectKey(branch), Kind: subjectRetirement, Revision: reviewed,
		ID: "22", Branch: branch, Item: Item{ID: "22", Title: "bad note"}}
	out, err := (verifierFlow{}).Act(cfg, repo, subject, "malformed-note")
	if err == nil || out.Status != "merge_failed" || !strings.Contains(err.Error(), "decode verdict note") {
		t.Fatalf("malformed durable Verdict Act = (status=%q, err=%v), want merge_failed read failure", out.Status, err)
	}
	facts, listErr := listRetirements(repo)
	if listErr != nil || len(facts) != 1 || facts[0].Record.State != "observed" ||
		facts[0].Record.Agent != "" || facts[0].Record.Model != "" || facts[0].Record.DefSHA != "" {
		t.Fatalf("read-failed retirement facts = (%#v, %v), want unattributed observed fact", facts, listErr)
	}
	for range stalledRunLimit {
		if code := actOnSubject(verifierFlow{}, cfg, repo, subject, nil); code != 1 {
			t.Fatalf("malformed evidence pass code = %d, want failure", code)
		}
	}
	if stalled, err := stalledOn(repo, "verifier", subject.Key, reviewed); err != nil || !stalled {
		t.Fatalf("malformed evidence brake = (%v, %v), want hard brake", stalled, err)
	}
}
func TestPendingHostRetirementReconcilesDurableApproval(t *testing.T) {
	tests := map[string]struct {
		approved bool
		prepare  func(*testing.T, string, string, *Agent)
	}{
		"rejected Verdict": {prepare: func(t *testing.T, repo, reviewed string, agent *Agent) {
			if err := writeChecks(repo, reviewed, checksNote{Status: "pass", RunID: "rejected"}, testCommitIdentity()); err != nil {
				t.Fatal(err)
			}
			if err := writeVerdict(repo, reviewed, verdictNote{
				Verdict: "changes", Reviewer: agent.Name, Model: agent.Model,
				DefSHA: agent.DefSHA, RunID: "rejected",
			}, testCommitIdentity()); err != nil {
				t.Fatal(err)
			}
		}},
		"failing Checks": {prepare: func(t *testing.T, repo, reviewed string, agent *Agent) {
			if err := writeChecks(repo, reviewed, checksNote{Status: "fail", RunID: "failed"}, testCommitIdentity()); err != nil {
				t.Fatal(err)
			}
			if err := writeVerdict(repo, reviewed, verdictNote{
				Verdict: "approve", Reviewer: agent.Name, Model: agent.Model,
				DefSHA: agent.DefSHA, RunID: "failed",
			}, testCommitIdentity()); err != nil {
				t.Fatal(err)
			}
		}},
		"Agent mismatch": {approved: true, prepare: func(t *testing.T, repo, reviewed string, agent *Agent) {
			if err := writeChecks(repo, reviewed, checksNote{Status: "pass", RunID: "mismatch"}, testCommitIdentity()); err != nil {
				t.Fatal(err)
			}
			if err := writeVerdict(repo, reviewed, verdictNote{
				Verdict: "approve", Reviewer: "other-agent", Model: agent.Model,
				DefSHA: agent.DefSHA, RunID: "mismatch",
			}, testCommitIdentity()); err != nil {
				t.Fatal(err)
			}
		}},
		"Model mismatch": {approved: true, prepare: func(t *testing.T, repo, reviewed string, agent *Agent) {
			if err := writeChecks(repo, reviewed, checksNote{Status: "pass", RunID: "mismatch"}, testCommitIdentity()); err != nil {
				t.Fatal(err)
			}
			if err := writeVerdict(repo, reviewed, verdictNote{
				Verdict: "approve", Reviewer: agent.Name, Model: "other-model",
				DefSHA: agent.DefSHA, RunID: "mismatch",
			}, testCommitIdentity()); err != nil {
				t.Fatal(err)
			}
		}},
		"DefSHA mismatch": {approved: true, prepare: func(t *testing.T, repo, reviewed string, agent *Agent) {
			if err := writeChecks(repo, reviewed, checksNote{Status: "pass", RunID: "mismatch"}, testCommitIdentity()); err != nil {
				t.Fatal(err)
			}
			if err := writeVerdict(repo, reviewed, verdictNote{
				Verdict: "approve", Reviewer: agent.Name, Model: agent.Model,
				DefSHA: strings.Repeat("b", 16), RunID: "mismatch",
			}, testCommitIdentity()); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for name, test := range tests {
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
			test.prepare(t, repo, reviewed, agent)
			oldProjection := projectionCommand
			defer func() { projectionCommand = oldProjection }()
			projectionCommand = func(args ...string) ([]byte, error) {
				if args[0] == "api" {
					return []byte(`[]`), nil
				}
				if args[0] == "pr" && args[1] == "list" {
					return []byte(`[{"number":18,"url":"https://github.com/owner/repo/pull/18","headRefOid":"` +
						reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
				}
				return nil, errors.New("unexpected Host call")
			}
			cfg := defaultConfig()
			cfg.Repo = "owner/repo"
			cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
			err = recoverRetirementFact(cfg, repo, fact, Item{ID: "18", Title: "approval"})
			current, found, readErr := readRetirement(repo, branch, reviewed)
			if readErr != nil || !found {
				t.Fatalf("reconciled fact = (%#v, %v, %v), want retained", current, found, readErr)
			}
			if test.approved {
				if !errors.Is(err, errHostMergePending) || current.Record.State != "pending" {
					t.Fatalf("winning approval recovery = (state=%q, err=%v), want pending", current.Record.State, err)
				}
				verdict, checks, approvalErr := readRetirementApproval(repo, reviewed)
				approved := verdict.Verdict == "approve" && checks.Status == "pass"
				if approvalErr != nil || !approved ||
					current.Record.Agent != verdict.Reviewer ||
					current.Record.Model != verdict.Model ||
					current.Record.DefSHA != verdict.DefSHA {
					t.Fatalf("winning attribution = (%#v, approved=%v, err=%v), want durable Verdict", current.Record, approved, approvalErr)
				}
			} else if !errors.Is(err, errRetirementPreparation) || current.Record.State != "preparing" {
				t.Fatalf("rejected preparation = (state=%q, err=%v), want preparing reset", current.Record.State, err)
			}
		})
	}
}

// A queued Host merge records its accepted request and observes it without
// issuing another request for the same reviewed Revision.
func TestQueuedHostMergeIsRequestedOnce(t *testing.T) {
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
			return nil, nil
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
	item := Item{ID: "11", Title: "strategy"}
	tk := newMemoryTracker()
	tk.seed(item)
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	err = recoverRetirementFact(cfg, repo, fact, item)
	if !errors.Is(err, errHostMergePending) {
		t.Fatalf("pending retirement retry = %v, want merge pending", err)
	}
	if err = recoverRetirementFact(cfg, repo, fact, item); !errors.Is(err, errHostMergePending) {
		t.Fatalf("queued Host merge retry = %v, want observation without a repeated request", err)
	}
	if mergeCalls != 1 || listCalls == 0 {
		t.Fatalf("recovery effects: merge=%d list=%d, want one merge request and observation", mergeCalls, listCalls)
	}
	if attempts, err := readAttempts(repo,
		effectAttemptKey("Host-merge-request", branch, reviewed)); err != nil || attempts != 1 {
		t.Fatalf("Host merge attempts = (%d, %v), want one", attempts, err)
	}
	if !hasArgumentPair(mergeArgs, "--match-head-commit", reviewed) {
		t.Fatalf("queued Host merge args %v do not pin reviewed Revision %s", mergeArgs, reviewed)
	}
}
func TestHostMergeCommandFailureGetsSingleRequestHandoff(t *testing.T) {
	branch := "forest/11-host-refusal"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	fact, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "11", Transport: "host",
		Strategy: "squash", Title: "host refusal", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeApprovalNotes(t, repo, reviewed, agent)
	item := Item{ID: "11", Title: "host refusal"}
	tk := newMemoryTracker()
	tk.seed(item)
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	mergeCalls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "api":
			return []byte(`[]`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
			return []byte(`[{"number":11,"url":"https://github.com/owner/repo/pull/11","headRefOid":"` +
				reviewed + `","headRefName":"` + branch +
				`","baseRefName":"master","isCrossRepository":false}]`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "merge":
			mergeCalls++
			if attempts, err := readAttempts(repo,
				effectAttemptKey("Host-merge-request", branch, reviewed)); err != nil || attempts != 1 {
				t.Fatalf("Host merge began before its durable claim: attempts=(%d, %v)", attempts, err)
			}
			return nil, errors.New("required approval is missing")
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.AutoMerge = true

	if err := recoverRetirementFact(cfg, repo, fact, item); !errors.Is(err, errRetirementRecoveryHard) {
		t.Fatalf("Host refusal = %v, want immediate recovery handoff", err)
	}
	if err := recoverRetirementFact(cfg, repo, fact, item); !errors.Is(err, errRetirementRecoveryHard) {
		t.Fatalf("repeated Host refusal = %v, want retained handoff", err)
	}
	if mergeCalls != 1 {
		t.Fatalf("Host merge calls = %d, want exactly one", mergeCalls)
	}
	if attempts, err := readAttempts(repo,
		effectAttemptKey("Host-merge-request", branch, reviewed)); err != nil || attempts != 1 {
		t.Fatalf("Host merge attempts = (%d, %v), want one", attempts, err)
	}
	if stalled, err := stalledOn(repo, (verifierFlow{}).Name(),
		retirementSubjectKey(branch), reviewed); err != nil || !stalled {
		t.Fatalf("Host refusal brake = (%v, %v), want durable terminal handoff", stalled, err)
	}
	got, err := tk.Get(item.ID)
	if err != nil || !got.hasTag(failedLabel) || len(got.Comments) != 1 ||
		!strings.Contains(got.Comments[0].Body, "revision="+reviewed+" -->") {
		t.Fatalf("Host refusal handoff = (%#v, %v), want failed tag and exact marker", got, err)
	}
}

type hostHandoffFailureTracker struct {
	*memoryTracker
	setCalls int
}

func (t *hostHandoffFailureTracker) SetTags(string, []string, []string) error {
	t.setCalls++
	return errTrackerUnavailable
}

func TestHostMergeHandoffUncertainTrackerTagDoesNotRepeat(t *testing.T) {
	_, repo, revision := notesTestRepository(t)
	const branch = "forest/11-host-handoff"
	item := Item{ID: "11", Title: "host handoff"}
	memory := newMemoryTracker()
	memory.seed(item)
	tk := &hostHandoffFailureTracker{memoryTracker: memory}
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	record := retirementRecord{Branch: branch, Revision: revision, ItemID: item.ID}
	for _, observed := range []Item{item, {ID: item.ID, Title: item.Title}} {
		if err := recordHostMergeHandoff(cfg, repo, record, observed,
			errors.New("Host refused merge")); !errors.Is(err, errRetirementRecoveryHard) {
			t.Fatalf("Tracker handoff failure = %v, want terminal uncertainty", err)
		}
	}
	if tk.setCalls != 1 {
		t.Fatalf("Tracker tag calls = %d, want one across reconstructed Item", tk.setCalls)
	}
	if attempts, err := readAttempts(repo,
		effectAttemptKey("Tracker-tag", item.ID, revision)); err != nil || attempts != 1 {
		t.Fatalf("Tracker tag claim = (%d, %v), want one", attempts, err)
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
func TestPendingHostRetirementObservesVisibleOpenMergeWithoutRepeating(t *testing.T) {
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
	if mergeCalls != 1 {
		t.Fatalf("queued recovery merge effects = %d, want one request", mergeCalls)
	}
	if len(mergeHeads) != mergeCalls {
		t.Fatalf("queued merge heads = %v, want one reviewed head", mergeHeads)
	}

	merged = true
	if err := recoverRetirementFact(cfg, repo, fact, item); err != nil {
		t.Fatalf("observed queued merge recovery: %v", err)
	}
	if mergeCalls != 1 {
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
	if err := recordHostRetirement(
		cfg, repo, branch, reviewed, item, verdict, checksNote{Status: "pass"},
	); err != nil {
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

	subject := Subject{Key: retirementSubjectKey(branch), Kind: subjectRetirement, Revision: reviewed,
		ID: item.ID, Branch: branch, Item: item}
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
