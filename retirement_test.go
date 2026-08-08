package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRetirementRecord(branch, revision, itemID string) retirementRecord {
	return retirementRecord{
		Branch: branch, Revision: revision, ItemID: itemID,
		Transport: "git", Strategy: "squash", Title: "change", State: "landed",
		Agent: "verifier", Model: "verifier-model", DefSHA: strings.Repeat("a", 16),
	}
}

func TestRetirementRecordRejectsUnsafeIdentity(t *testing.T) {
	revision := strings.Repeat("b", 40)
	tests := map[string]func(*retirementRecord){
		"non-forest branch": func(r *retirementRecord) { r.Branch = "master" },
		"wrong item":        func(r *retirementRecord) { r.ItemID = "10" },
		"symbolic revision": func(r *retirementRecord) { r.Revision = "HEAD" },
		"short revision":    func(r *retirementRecord) { r.Revision = strings.Repeat("b", 39) },
		"non-hex revision":  func(r *retirementRecord) { r.Revision = strings.Repeat("z", 40) },
		"null revision":     func(r *retirementRecord) { r.Revision = strings.Repeat("0", 40) },
		"missing agent":     func(r *retirementRecord) { r.Agent = "" },
		"missing model":     func(r *retirementRecord) { r.Model = "" },
		"bad definition":    func(r *retirementRecord) { r.DefSHA = "not-hex" },
		"bad transport":     func(r *retirementRecord) { r.Transport = "carrier" },
		"bad strategy":      func(r *retirementRecord) { r.Strategy = "merge" },
		"bad state":         func(r *retirementRecord) { r.State = "started" },
		"pending native":    func(r *retirementRecord) { r.State = "pending" },
		"fast-forward Host": func(r *retirementRecord) { r.Transport, r.Strategy = "host", "ff" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			record := testRetirementRecord("forest/9-change", revision, "9")
			mutate(&record)
			if _, err := recordRetirement(newRefGitRepo(t), record); err == nil {
				t.Fatal("unsafe retirement record was accepted")
			}
		})
	}
}

func TestRetirementFactRejectsNullRevision(t *testing.T) {
	repo := newRefGitRepo(t)
	record := testRetirementRecord("forest/9-change", strings.Repeat("0", 40), "9")
	body, err := retirementPayload(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := putBlobRef(repo, retirementRef(record.Branch, record.Revision), body, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := listRetirements(repo); err == nil || !strings.Contains(err.Error(), "invalid Revision") {
		t.Fatalf("null retirement fact = %v, want named refusal", err)
	}
}

func TestRetirementFactRejectsRefContentMismatch(t *testing.T) {
	repo := newRefGitRepo(t)
	record := testRetirementRecord("forest/9-change", strings.Repeat("b", 40), "9")
	body, err := retirementPayload(record)
	if err != nil {
		t.Fatal(err)
	}
	wrongRef := retirementRef(record.Branch, strings.Repeat("c", 40))
	if err := putBlobRef(repo, wrongRef, body, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := listRetirements(repo); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched retirement fact = %v, want named refusal", err)
	}
}

func TestRetirementFactsRejectConflictingBranchRevisions(t *testing.T) {
	repo := newRefGitRepo(t)
	for _, revision := range []string{strings.Repeat("b", 40), strings.Repeat("c", 40)} {
		if _, err := recordRetirement(repo, testRetirementRecord("forest/9-change", revision, "9")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := listRetirements(repo); err == nil || !strings.Contains(err.Error(), "conflicting facts") {
		t.Fatalf("conflicting retirement list = %v, want named refusal", err)
	}
}

func TestRetirementFactsRejectConflictingItemBranches(t *testing.T) {
	repo := newRefGitRepo(t)
	records := []retirementRecord{
		testRetirementRecord("forest/9-first", strings.Repeat("b", 40), "9"),
		testRetirementRecord("forest/9-second", strings.Repeat("c", 40), "9"),
	}
	for _, record := range records {
		if _, err := recordRetirement(repo, record); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := listRetirements(repo); err == nil || !strings.Contains(err.Error(), "retirement item 9 has conflicting facts") {
		t.Fatalf("conflicting retirement item list = %v, want named refusal", err)
	}
}

func TestRetirementRecordAllowsEmptyTitleSlug(t *testing.T) {
	repo := newRefGitRepo(t)
	record := testRetirementRecord("forest/9-", strings.Repeat("b", 40), "9")
	if _, err := recordRetirement(repo, record); err != nil {
		t.Fatalf("empty title slug record: %v", err)
	}
}

// TestMergeGitPathPinsReviewedRevision pins the retirement transaction. Master
// and its landed marker advance atomically. The marker then guards Tracker
// retirement and exact source deletion across failures and process restarts.
func TestMergeGitPathPinsReviewedRevision(t *testing.T) {
	// Tracker retirement talks to the host; stub it so the merge path runs
	// without a host CLI.
	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) { return []byte(`{}`), nil }
	defer func() { ghJSON = oldGH }()
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{}
	cfg.Flows.Verifier.Merge = "squash"
	agent := testVerifierAgent()
	it := Item{ID: "9", Title: "change"}

	t.Run("unchanged branch lands and retires", func(t *testing.T) {
		repo, branch, reviewed, masterBefore := newVerifierBranch(t, "forest/9-change")
		runGitTest(t, repo, "checkout", "-q", "master")
		runGitTest(t, repo, "branch", "-D", branch)
		if err := mergeGitPath(cfg, repo, branch, reviewed, it, agent); err != nil {
			t.Fatalf("mergeGitPath on an unchanged branch = %v, want nil", err)
		}
		if got := remoteBranchHead(t, repo, "master"); got == masterBefore {
			t.Fatal("master did not advance on an unchanged branch")
		}
		master := remoteBranchHead(t, repo, "master")
		author := runGitTest(t, repo, "show", "-s", "--format=%an <%ae>", master)
		if author != "forest-test <forest-test@example.com>" {
			t.Fatalf("squash commit author = %q, want Verifier identity", author)
		}
		if out := runGitTest(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out != "" {
			t.Fatalf("source branch %q still on origin after landing: %s", branch, out)
		}
	})

	t.Run("rejected marker keeps transaction atomic", func(t *testing.T) {
		repo, branch, reviewed, masterBefore := newVerifierBranch(t, "forest/9-change")
		origin := runGitTest(t, repo, "remote", "get-url", "origin")
		hook := filepath.Join(origin, "hooks", "update")
		script := "#!/bin/sh\ncase \"$1\" in\n  refs/forest/retirement/*) exit 1 ;;\nesac\nexit 0\n"
		if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := mergeGitPath(cfg, repo, branch, reviewed, it, agent); err == nil {
			t.Fatal("merge transaction succeeded though the retirement marker was rejected")
		}
		if got := remoteBranchHead(t, repo, "master"); got != masterBefore {
			t.Fatalf("rejected atomic transaction advanced master to %s, want %s", got, masterBefore)
		}
		if got := remoteBranchHead(t, repo, branch); got != reviewed {
			t.Fatalf("rejected atomic transaction changed source to %s, want %s", got, reviewed)
		}
		if out := runGitTest(t, repo, "ls-remote", "origin", "refs/forest/retirement/*"); out != "" {
			t.Fatalf("rejected atomic transaction published retirement fact: %s", out)
		}
		if err := os.Remove(hook); err != nil {
			t.Fatal(err)
		}
		if err := mergeGitPath(cfg, repo, branch, reviewed, it, agent); err != nil {
			t.Fatalf("retry after rejected atomic transaction: %v", err)
		}
		if got := remoteBranchHead(t, repo, "master"); got == masterBefore {
			t.Fatal("retry did not advance master")
		}
	})

	t.Run("tracker failure preserves durable recovery", func(t *testing.T) {
		repo, branch, reviewed, masterBefore := newVerifierBranch(t, "forest/9-change")
		successGH := ghJSON
		ghJSON = func(args ...string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "issue" && args[1] == "close" {
				return nil, errors.New("tracker unavailable")
			}
			return []byte(`{}`), nil
		}
		t.Cleanup(func() { ghJSON = successGH })
		if err := mergeGitPath(cfg, repo, branch, reviewed, it, agent); err == nil ||
			!strings.Contains(err.Error(), "close item") {
			t.Fatalf("mergeGitPath tracker failure = %v, want close item error", err)
		}
		if got := remoteBranchHead(t, repo, "master"); got == masterBefore {
			t.Fatal("master did not advance before Tracker retirement")
		}
		if out := runGitTest(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out != "" {
			t.Fatalf("source branch survived the durable retirement marker: %s", out)
		}
		masterAfterFailure := remoteBranchHead(t, repo, "master")
		ghJSON = successGH
		restartRoot := t.TempDir()
		restarted := filepath.Join(restartRoot, "restart")
		origin := runGitTest(t, repo, "remote", "get-url", "origin")
		runGitTest(t, restartRoot, "clone", "-q", origin, restarted)
		subjects, err := (verifierFlow{}).Select(cfg, restarted)
		if err != nil {
			t.Fatalf("restart select: %v", err)
		}
		if len(subjects) != 1 || subjects[0].Kind != "retirement" {
			t.Fatalf("restart subjects = %#v, want one retirement", subjects)
		}
		out, err := (verifierFlow{}).Act(cfg, restarted, subjects[0], "restart-run")
		if err != nil {
			t.Fatalf("retirement after restart: %v", err)
		}
		if out.Agent != agent.Name || out.Model != agent.Model || out.DefSHA != agent.DefSHA {
			t.Fatalf("recovery attribution = %#v, want marker identity %#v", out, agent)
		}
		if got := remoteBranchHead(t, restarted, "master"); got != masterAfterFailure {
			t.Fatalf("retirement retry advanced master again: got %s, want %s", got, masterAfterFailure)
		}
		if out := runGitTest(t, restarted, "ls-remote", "origin", "refs/heads/"+branch); out != "" {
			t.Fatalf("source branch survived retirement retry: %s", out)
		}
		if facts, err := listRetirements(restarted); err != nil || len(facts) != 0 {
			t.Fatalf("retirement facts after retry = (%#v, %v), want none", facts, err)
		}
	})

	t.Run("advanced branch is refused and survives", func(t *testing.T) {
		repo, branch, reviewed, masterBefore := newVerifierBranch(t, "forest/9-change")
		// The operator pushes newer, unreviewed work after the Verdict and after
		// the fence was read, simulating the review-to-merge window.
		runGitTest(t, repo, "checkout", "-q", branch)
		rebaseTestWriteFile(t, filepath.Join(repo, "later.txt"), "later\n")
		runGitTest(t, repo, "add", "later.txt")
		runGitTest(t, repo, "commit", "-q", "-m", "newer unreviewed work")
		runGitTest(t, repo, "push", "-q", "origin", branch)
		observed := remoteBranchHead(t, repo, branch)
		if observed == reviewed {
			t.Fatal("branch did not advance, cannot probe the race")
		}

		if err := mergeGitPath(cfg, repo, branch, reviewed, it, agent); err == nil {
			t.Fatal("mergeGitPath merged a branch that advanced past its reviewed Revision")
		}
		if got := remoteBranchHead(t, repo, "master"); got != masterBefore {
			t.Fatalf("master advanced to %s after the refused merge, want %s", got, masterBefore)
		}
		if got := remoteBranchHead(t, repo, branch); got != observed {
			t.Fatalf("branch tip = %s, want the newer commits %s intact", got, observed)
		}
	})
}

// TestMergeViaHostPinsReviewedHead pins the host path of item #188: a host merge
// receives the expected-head facility pinned to the reviewed Revision, and when
// the host refuses (head mismatch), the branch is not deleted so the newer,
// unreviewed commits survive for re-review.
func TestMergeViaHostPinsReviewedHead(t *testing.T) {
	branch := "forest/9-change"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)

	oldProj := projectionCommand
	defer func() { projectionCommand = oldProj }()
	var mergeArgs []string
	projectionCommand = func(args ...string) ([]byte, error) {
		switch args[1] {
		case "list":
			return []byte(`[{"number":9,"url":"https://github.com/owner/repo/pull/9","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		case "merge":
			mergeArgs = append([]string(nil), args...)
			return nil, errors.New("host refused: head does not match reviewed Revision")
		default:
			return nil, nil
		}
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Merge = "squash"

	if err := mergeVerified(cfg, repo, branch, reviewed, Item{ID: "9", Title: "change"},
		testVerifierAgent()); err == nil {
		t.Fatal("mergeVerified merged via host despite the host refusing the head")
	}
	pinned := false
	for i := 0; i+1 < len(mergeArgs); i++ {
		if mergeArgs[i] == "--match-head-commit" && mergeArgs[i+1] == reviewed {
			pinned = true
		}
	}
	if !pinned {
		t.Fatalf("host merge args %v do not pin the reviewed Revision %s with --match-head-commit", mergeArgs, reviewed)
	}
	if out := runGitTest(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out == "" {
		t.Fatal("mergeVerified deleted the branch even though the host merge refused")
	}
}

func TestVerifierRecoversAlreadyMergedProjectionWithoutDuplicate(t *testing.T) {
	branch := "forest/9-already-merged"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	runGitTest(t, repo, "branch", "-D", branch)
	writeAgentFixture(t, repo, "verifier", "verifier-model")
	if err := writeChecks(repo, reviewed, checksNote{Status: "pass", RunID: "seed"}); err != nil {
		t.Fatal(err)
	}
	if err := writeVerdict(repo, reviewed, verdictNote{
		Verdict: "approve", Reviewer: "verifier", Model: "verifier-model", DefSHA: "def", RunID: "seed",
	}); err != nil {
		t.Fatal(err)
	}
	oldTracker := trackerFor
	tk := newMemoryTracker()
	tk.seed(Item{ID: "9", Title: "already merged"})
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	createCalls, mergeCalls := 0, 0
	projectionCommand = func(args ...string) ([]byte, error) {
		state := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--state" {
				state = args[i+1]
			}
		}
		switch {
		case args[1] == "list" && state == "open":
			return []byte(`[]`), nil
		case args[1] == "list" && state == "merged":
			return []byte(`[{"number":9,"url":"https://github.com/owner/repo/pull/9","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		case args[1] == "create":
			createCalls++
			return nil, errors.New("duplicate pull request")
		case args[1] == "merge":
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
	cfg.Flows.Verifier.AutoMerge = true
	out, err := (verifierFlow{}).Act(cfg, repo, Subject{
		Key: "branch-" + branch, Kind: "branch", Revision: reviewed,
		ID: "9", Branch: branch, Head: reviewed,
	}, "recover")
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

func TestMergeViaHostRecoversTrackerCloseFailure(t *testing.T) {
	branch := "forest/9-host-recovery"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	merged := false
	mergeCalls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		if len(args) < 2 {
			return nil, errors.New("unexpected host command")
		}
		if args[1] == "merge" {
			merged = true
			mergeCalls++
			if err := deleteRef(repo, "refs/heads/"+branch, reviewed); err != nil {
				return nil, err
			}
			return nil, nil
		}
		if args[1] != "list" {
			return nil, errors.New("unexpected host command")
		}
		state := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--state" {
				state = args[i+1]
			}
		}
		switch {
		case state == "open" && !merged:
			return []byte(`[{"number":9,"url":"https://github.com/owner/repo/pull/9","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		case state == "merged" && merged:
			return []byte(`[{"number":9,"url":"https://github.com/owner/repo/pull/9","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		default:
			return []byte(`[]`), nil
		}
	}

	oldGH := ghJSON
	defer func() { ghJSON = oldGH }()
	closeCalls := 0
	ghJSON = func(args ...string) ([]byte, error) {
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
	item := Item{ID: "9", Title: "change"}
	if err := mergeVerified(cfg, repo, branch, reviewed, item,
		testVerifierAgent()); err == nil ||
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

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	mergeCalls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[1] == "merge" {
			mergeCalls++
			return nil, errors.New("recovery attempted a duplicate Host merge")
		}
		if len(args) >= 2 && args[1] == "list" {
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--state" && args[i+1] == "merged" {
					return []byte(`[{"number":10,"url":"https://github.com/owner/repo/pull/10","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
				}
			}
			return []byte(`[]`), nil
		}
		return nil, errors.New("unexpected Host command")
	}
	oldGH := ghJSON
	defer func() { ghJSON = oldGH }()
	closeCalls := 0
	ghJSON = func(args ...string) ([]byte, error) {
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

// TestPendingHostRetirementObservesRecordedStrategy proves recovery observes
// the recorded merge effect without asking an open request to merge again.
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
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	mergeCalls, listCalls := 0, 0
	projectionCommand = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[1] == "merge" {
			mergeCalls++
			return nil, errors.New("recovery attempted a duplicate Host merge")
		}
		if len(args) >= 2 && args[1] == "list" {
			listCalls++
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--state" && args[i+1] == "open" {
					return []byte(`[{"number":11,"url":"https://github.com/owner/repo/pull/11","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
				}
			}
			return []byte(`[]`), nil
		}
		return nil, errors.New("unexpected Host command")
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Merge = "ff"
	err = recoverRetirementFact(cfg, repo, fact, Item{ID: "11", Title: "strategy"})
	if !errors.Is(err, errHostMergePending) {
		t.Fatalf("pending retirement observation = %v, want merge pending", err)
	}
	if mergeCalls != 0 || listCalls == 0 {
		t.Fatalf("recovery effects: merge=%d list=%d, want no merge and an observation", mergeCalls, listCalls)
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
