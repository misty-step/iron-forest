package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newVerifierBranch(t *testing.T, branch string) (repo, namedBranch, reviewed, masterBefore string) {
	t.Helper()
	repo = setupTestRepo(t)
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), branch+"\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	return repo, branch, remoteBranchHead(t, repo, branch), remoteBranchHead(t, repo, "master")
}

func testRetirementRecord(branch, revision, itemID string) retirementRecord {
	return retirementRecord{
		Branch: branch, Revision: revision, ItemID: itemID,
		Transport: "git", Strategy: "squash", Title: "change", State: "landed",
		Agent: "verifier", Model: "verifier-model", DefSHA: strings.Repeat("a", 16),
	}
}

func writeApprovalNotes(t *testing.T, repo, revision string, agent *Agent) {
	t.Helper()
	if err := writeChecks(repo, revision, checksNote{Status: "pass", RunID: "seed"}); err != nil {
		t.Fatal(err)
	}
	if err := writeVerdict(repo, revision, verdictNote{
		Verdict: "approve", Reviewer: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA, RunID: "seed",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRetirementRecordRejectsUnsafeIdentity(t *testing.T) {
	revision := strings.Repeat("b", 40)
	const secret = "sk-AAAAAAAAAAAAAAAA"
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
		"secret control": func(r *retirementRecord) {
			r.ItemID = secret
			r.Branch = BranchPrefix + encodeBranchID(secret) + "-change"
		},
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
	if _, err := listRetirements(repo); err == nil || !strings.Contains(err.Error(), "retirement Item 9 has conflicting facts") {
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

func TestRetirementWritersRedactMutableText(t *testing.T) {
	const secret = "sk-AAAAAAAAAAAAAAAA"
	for name, writeFact := range map[string]func(string, retirementRecord) (retirementFact, error){
		"prepared": prepareRetirement,
		"recorded": recordRetirement,
	} {
		t.Run(name, func(t *testing.T) {
			repo := newRefGitRepo(t)
			record := testRetirementRecord("forest/9-change", strings.Repeat("b", 40), "9")
			record.Title = "change " + secret
			record.Agent = "verifier-" + secret
			record.Model = "model-" + secret
			fact, err := writeFact(repo, record)
			if err != nil {
				t.Fatal(err)
			}
			body := runGitTest(t, repo, "cat-file", "-p", fact.SHA)
			if strings.Contains(body, secret) || !strings.Contains(body, secretRedacted) {
				t.Fatalf("retirement fact retained mutable credential-shaped text: %s", body)
			}
			if strings.Contains(fact.Record.Title, secret) ||
				strings.Contains(fact.Record.Agent, secret) ||
				strings.Contains(fact.Record.Model, secret) {
				t.Fatalf("returned retirement record retained mutable credential-shaped text: %#v", fact.Record)
			}
		})
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
			return []byte(`{"state":"OPEN"}`), nil
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

	t.Run("post-close cleanup failure remains resumable", func(t *testing.T) {
		repo, branch, reviewed, masterBefore := newVerifierBranch(t, "forest/9-cleanup")
		if _, err := bumpAttempts(repo, "branch-"+branch); err != nil {
			t.Fatal(err)
		}
		tk := newMemoryTracker()
		tk.seed(it)
		oldTracker := trackerFor
		trackerFor = func(string) Tracker { return tk }
		defer func() { trackerFor = oldTracker }()
		origin := runGitTest(t, repo, "remote", "get-url", "origin")
		hook := filepath.Join(origin, "hooks", "update")
		attemptRef := "refs/forest/attempt/branch-" + branch
		script := "#!/bin/sh\nif [ \"$1\" = '" + attemptRef + "' ]; then exit 1; fi\nexit 0\n"
		if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := mergeGitPath(cfg, repo, branch, reviewed, it, agent); err == nil ||
			!strings.Contains(err.Error(), "retire durable refs") {
			t.Fatalf("post-close cleanup failure = %v, want atomic ref cleanup error", err)
		}
		if got := remoteBranchHead(t, repo, "master"); got == masterBefore {
			t.Fatal("cleanup failure prevented the durable merge")
		}
		masterAfter := remoteBranchHead(t, repo, "master")
		if _, err := tk.Get(it.ID); err == nil {
			t.Fatal("cleanup failure did not preserve the successful Tracker close")
		}
		if facts, err := listRetirements(repo); err != nil || len(facts) != 1 ||
			facts[0].Record.State != "landed" {
			t.Fatalf("cleanup failure facts = (%#v, %v), want landed recovery fact", facts, err)
		}
		if attempts, err := readAttempts(repo, "branch-"+branch); err != nil || attempts != 1 {
			t.Fatalf("atomic cleanup failure attempts = (%d, %v), want retained", attempts, err)
		}
		if err := os.Remove(hook); err != nil {
			t.Fatal(err)
		}
		restartRoot := t.TempDir()
		restarted := filepath.Join(restartRoot, "restart")
		runGitTest(t, restartRoot, "clone", "-q", origin, restarted)
		subjects, err := (verifierFlow{}).Select(cfg, restarted)
		if err != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement {
			t.Fatalf("cleanup restart Select = (%#v, %v), want one retirement", subjects, err)
		}
		out, err := (verifierFlow{}).Act(cfg, restarted, subjects[0], "cleanup-restart")
		if err != nil || out.Status != "merged" {
			t.Fatalf("cleanup restart Act = (%#v, %v), want merged cleanup", out, err)
		}
		if got := remoteBranchHead(t, restarted, "master"); got != masterAfter {
			t.Fatalf("cleanup retry duplicated merge: got %s, want %s", got, masterAfter)
		}
		if facts, err := listRetirements(restarted); err != nil || len(facts) != 0 {
			t.Fatalf("cleanup retry facts = (%#v, %v), want none", facts, err)
		}
		if attempts, err := readAttempts(restarted, "branch-"+branch); err != nil || attempts != 0 {
			t.Fatalf("cleanup retry attempts = (%d, %v), want removed", attempts, err)
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

// TestFenceMergeOnReviewedRevision pins item #188: a merge may land only the
// Revision that carried the approving Verdict. An unchanged remote branch passes
// the fence; a branch that advanced after the Verdict is refused with both
// Revisions named, and mergeVerified leaves the branch with its newer commits
// intact rather than deleting it.
func TestFenceMergeOnReviewedRevision(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-change"
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	reviewed := remoteBranchHead(t, repo, branch)

	// An unchanged remote branch is exactly the reviewed Revision: it passes.
	if err := fenceMergeOnRevision(repo, branch, reviewed); err != nil {
		t.Fatalf("unchanged branch refused by the fence: %v", err)
	}

	// The operator pushes newer, unreviewed work after the Verdict was written.
	runGitTest(t, repo, "checkout", "-q", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "later.txt"), "later\n")
	runGitTest(t, repo, "add", "later.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "newer unreviewed work")
	runGitTest(t, repo, "push", "-q", "origin", branch)
	observed := remoteBranchHead(t, repo, branch)
	if observed == reviewed {
		t.Fatalf("branch did not advance, cannot probe the fence")
	}

	if err := fenceMergeOnRevision(repo, branch, reviewed); err == nil {
		t.Fatalf("advanced branch passed the fence")
	} else if !strings.Contains(err.Error(), reviewed[:8]) || !strings.Contains(err.Error(), observed[:8]) {
		t.Fatalf("refusal %q does not name both the reviewed (%s) and observed (%s) Revisions", err, reviewed[:8], observed[:8])
	}

	// mergeVerified must refuse without deleting the branch, so the newer,
	// unreviewed commits survive for the next pass to review.
	if err := mergeVerified(defaultConfig(), repo, branch, reviewed,
		Item{ID: "9", Title: "change"}, testVerifierAgent()); err == nil {
		t.Fatal("mergeVerified merged a branch that advanced past its reviewed Revision")
	}
	if out := runGitTest(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out == "" {
		t.Fatal("mergeVerified deleted the branch despite refusing the merge")
	}
	if got := remoteBranchHead(t, repo, branch); got != observed {
		t.Fatalf("branch tip = %s, want the newer commits %s intact", got, observed)
	}
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
		switch {
		case len(args) > 0 && args[0] == "api":
			return []byte(`[]`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
			return []byte(`[{"number":9,"url":"https://github.com/owner/repo/pull/9","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "merge":
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
	cfg.Flows.Verifier.AutoMerge = true
	reviewAgent := testVerifierAgent()
	writeApprovalNotes(t, repo, reviewed, reviewAgent)

	if err := mergeVerified(cfg, repo, branch, reviewed, Item{ID: "9", Title: "change"},
		reviewAgent); err == nil {
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

type retirementTransientTracker struct{ item Item }

func (t retirementTransientTracker) ListOpen() ([]Item, error)    { return []Item{t.item}, nil }
func (t retirementTransientTracker) Get(string) (Item, error)     { return t.item, nil }
func (t retirementTransientTracker) Comment(string, string) error { return nil }
func (t retirementTransientTracker) Close(string) error {
	return errTrackerEffectNotApplied
}
func (t retirementTransientTracker) SetTags(string, []string, []string) error { return nil }

func TestRetirementTransientFailuresRemainSelectable(t *testing.T) {
	branch := "forest/14-transient"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	if _, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "14", Transport: "git",
		Strategy: "squash", Title: "transient", State: "landed",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	}); err != nil {
		t.Fatal(err)
	}
	oldTracker := trackerFor
	trackerFor = func(string) Tracker {
		return retirementTransientTracker{item: Item{ID: "14", Title: "transient"}}
	}
	defer func() { trackerFor = oldTracker }()
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	subject := Subject{Key: retirementSubjectKey(branch), Kind: subjectRetirement, Revision: reviewed, ID: "14", Branch: branch, Item: Item{ID: "14", Title: "transient"}}
	for pass := range stalledRunLimit + 2 {
		if code := actOnSubject(verifierFlow{}, cfg, repo, subject, nil); code != 1 {
			t.Fatalf("transient retirement pass %d code = %d, want failure", pass+1, code)
		}
	}
	if stalled, err := stalledOn(repo, "verifier", subject.Key, reviewed); err != nil || stalled {
		t.Fatalf("transient retirement brake = %v, %v; want no terminal brake", stalled, err)
	}
	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) == 0 || subjects[0].Kind != subjectRetirement {
		t.Fatalf("transient retirement subjects = %#v, want selectable recovery", subjects)
	}
}

func TestRetirementStaleAdvancedBranchRecordsTerminalBrake(t *testing.T) {
	branch := "forest/15-advanced"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	if _, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "15", Transport: "git",
		Strategy: "squash", Title: "advanced", State: "landed",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	}); err != nil {
		t.Fatal(err)
	}
	rebaseTestWriteFile(t, filepath.Join(repo, "later.txt"), "later\n")
	runGitTest(t, repo, "add", "later.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "later")
	runGitTest(t, repo, "push", "-q", "origin", branch)

	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return newMemoryTracker() }
	defer func() { trackerFor = oldTracker }()
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	subject := Subject{Key: retirementSubjectKey(branch), Kind: subjectRetirement, Revision: reviewed, ID: "15", Branch: branch, Item: Item{ID: "15", Title: "advanced"}}
	if code := actOnSubject(verifierFlow{}, cfg, repo, subject, nil); code != 1 {
		t.Fatalf("advanced retirement code = %d, want failure", code)
	}
	if stalled, err := stalledOn(repo, "verifier", subject.Key, reviewed); err != nil || !stalled {
		t.Fatalf("advanced retirement brake = %v, %v; want terminal brake", stalled, err)
	}
}

func TestStaleHostRetirementRetainsFact(t *testing.T) {
	branch := "forest/16-stale-host"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	tk := newMemoryTracker()
	tk.seed(Item{ID: "16", Title: "stale host", UpdatedAt: "u1"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	if _, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "16", Transport: "host",
		Strategy: "squash", Title: "stale host", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	}); err != nil {
		t.Fatal(err)
	}
	writeApprovalNotes(t, repo, reviewed, agent)
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	projectionCommand = func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "api" {
			return []byte(`[]`), nil
		}
		return []byte(`[{"number":16,"url":"https://github.com/owner/repo/pull/16","headRefOid":"` +
			strings.Repeat("c", 40) + `","headRefName":"` + branch +
			`","baseRefName":"master","isCrossRepository":false}]`), nil
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	subject := Subject{Key: retirementSubjectKey(branch), Kind: subjectRetirement, Revision: reviewed,
		ID: "16", Branch: branch, Item: Item{ID: "16", Title: "stale host"}}
	out, err := (verifierFlow{}).Act(cfg, repo, subject, "stale-host")
	if !errors.Is(err, errHostMergeUnavailable) || out.Status != "merge_failed" {
		t.Fatalf("stale Host recovery = (status=%q, err=%v), want retained failure", out.Status, err)
	}
	if facts, err := listRetirements(repo); err != nil || len(facts) != 1 {
		t.Fatalf("stale Host fact = (%#v, %v), want retained intent", facts, err)
	}
	if candidates, err := (builderFlow{}).Select(cfg, repo); err != nil || len(candidates) != 0 {
		t.Fatalf("stale Host fact re-exposed Builder work = (%#v, %v)", candidates, err)
	}
}

func TestRepeatedHostObservationPreservesLandedAttribution(t *testing.T) {
	repo := newRefGitRepo(t)
	record := retirementRecord{
		Branch: "forest/44-landed", Revision: strings.Repeat("a", 40),
		ItemID: "44", Transport: "host", Strategy: "squash",
		Title: "landed", State: "landed",
		Agent: "verifier", Model: "verifier-model", DefSHA: strings.Repeat("b", 16),
	}
	fact, err := recordRetirement(repo, record)
	if err != nil {
		t.Fatal(err)
	}
	got, err := recordObservedHostRetirement(
		Config{Flows: Flows{Verifier: VerifierFlowCfg{Merge: "squash"}}},
		repo, record.Branch, record.Revision, Item{ID: record.ItemID, Title: record.Title})
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA != fact.SHA || got.Record != fact.Record {
		t.Fatalf("repeated Host observation = %#v, want unchanged landed fact %#v", got, fact)
	}
}
