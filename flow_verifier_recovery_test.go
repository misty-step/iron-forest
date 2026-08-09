package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type responseLossCommentTracker struct {
	*memoryTracker
	commentCalls int
}

func (t *responseLossCommentTracker) Comment(id, body string) error {
	t.commentCalls++
	if err := t.memoryTracker.Comment(id, body); err != nil {
		return err
	}
	return errors.New("response lost after acceptance")
}

type invisibleCommentTracker struct {
	*memoryTracker
	commentCalls int
}

func (t *invisibleCommentTracker) Comment(string, string) error {
	t.commentCalls++
	return errors.New("response lost without visible comment")
}

type responseLossTagTracker struct {
	*memoryTracker
	tagCalls int
}

func (t *responseLossTagTracker) SetTags(id string, add, remove []string) error {
	t.tagCalls++
	if err := t.memoryTracker.SetTags(id, add, remove); err != nil {
		return err
	}
	return errors.New("response lost after acceptance")
}

func TestPublishTrackerCommentIsIdempotentAfterResponseLoss(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	const id = `/../../outside\trace?[x]`
	memory := newMemoryTracker()
	memory.seed(Item{ID: id, Title: "change"})
	tracker := &responseLossCommentTracker{memoryTracker: memory}
	revision := strings.Repeat("a", 40)

	it, err := tracker.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	marker := "<!-- iron-forest:merge-blocked revision=" + revision + " -->"
	if err := publishTrackerComment(repo, tracker, it, "Tracker-comment", revision,
		"Merge blocked: merge failed", marker); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	it, err = tracker.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishTrackerComment(repo, tracker, it, "Tracker-comment", revision,
		"Merge blocked: merge failed", marker); err != nil {
		t.Fatalf("retry publish: %v", err)
	}
	if tracker.commentCalls != 1 || len(it.Comments) != 1 ||
		!strings.Contains(it.Comments[0].Body, "<!-- iron-forest:merge-blocked revision="+revision+" -->") {
		t.Fatalf("comments = (%d calls, %#v), want one exact-Revision effect",
			tracker.commentCalls, it.Comments)
	}
}

func TestPublishTrackerCommentDoesNotRepeatInvisibleOutcome(t *testing.T) {
	_, repo, revision := notesTestRepository(t)
	item := Item{ID: "9", Title: "change"}
	memory := newMemoryTracker()
	memory.seed(item)
	tracker := &invisibleCommentTracker{memoryTracker: memory}
	marker := "<!-- iron-forest:built revision=" + revision + " -->"
	for range 2 {
		err := publishTrackerComment(repo, tracker, item,
			"Tracker-builder-comment", revision, "Built branch `forest/9-change`.", marker)
		if !errors.Is(err, errHostMergeUnavailable) {
			t.Fatalf("invisible Tracker comment = %v, want hard uncertainty", err)
		}
	}
	if tracker.commentCalls != 1 {
		t.Fatalf("Tracker comment calls = %d, want one", tracker.commentCalls)
	}
	if attempts, err := readAttempts(repo,
		effectAttemptKey("Tracker-builder-comment", item.ID, revision)); err != nil || attempts != 1 {
		t.Fatalf("Tracker comment claim = (%d, %v), want one", attempts, err)
	}
}

func TestHostHandoffReconcilesAcceptedTagResponseLoss(t *testing.T) {
	_, repo, revision := notesTestRepository(t)
	item := Item{ID: "9", Title: "change"}
	memory := newMemoryTracker()
	memory.seed(item)
	tracker := &responseLossTagTracker{memoryTracker: memory}
	old := trackerFor
	trackerFor = func(string) Tracker { return tracker }
	defer func() { trackerFor = old }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	record := retirementRecord{Branch: "forest/9-change", Revision: revision, ItemID: item.ID}
	err := recordHostMergeHandoff(cfg, repo, record, item, errors.New("Host refused merge"))
	if !errors.Is(err, errRetirementRecoveryHard) {
		t.Fatalf("Host handoff = %v, want durable hard handoff", err)
	}
	err = recordHostMergeHandoff(cfg, repo, record,
		Item{ID: item.ID, Title: item.Title}, errors.New("Host refused merge"))
	if !errors.Is(err, errRetirementRecoveryHard) {
		t.Fatalf("reconstructed Host handoff = %v, want retained hard handoff", err)
	}
	got, getErr := tracker.Get(item.ID)
	if getErr != nil || tracker.tagCalls != 1 || !got.hasTag(failedLabel) || len(got.Comments) != 1 {
		t.Fatalf("reconciled handoff = (%#v, tags=%d, err=%v), want one tag call and one comment",
			got, tracker.tagCalls, getErr)
	}
	if stalled, stallErr := stalledOn(repo, (verifierFlow{}).Name(),
		retirementSubjectKey(record.Branch), revision); stallErr != nil || !stalled {
		t.Fatalf("Host handoff brake = (%v, %v), want terminal", stalled, stallErr)
	}
}

func TestVerifierSelectionDoesNotLetRetirementRecoveryStarveBranchWork(t *testing.T) {
	const branch = "forest/2-fresh"
	repo, _, _, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	if _, err := recordRetirement(repo, retirementRecord{
		Branch: "forest/1-pending", Revision: strings.Repeat("a", 40), ItemID: "1",
		Transport: "host", Strategy: "squash", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	}); err != nil {
		t.Fatal(err)
	}
	subjects, err := (verifierFlow{}).Select(defaultConfig(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 2 || subjects[0].Kind != subjectBranch || subjects[0].Branch != branch ||
		subjects[1].Kind != subjectRetirement {
		t.Fatalf("Verifier selection order = %#v, want branch work before pending retirement", subjects)
	}
}

func TestMalformedRetirementDoesNotStarveUnrelatedBranchOrReopenItem(t *testing.T) {
	const freshBranch = "forest/2-fresh"
	repo, _, freshRevision, _ := newVerifierBranch(t, freshBranch)
	badBranch := "forest/1"
	badRevision := strings.Repeat("b", 40)
	if err := putBlobRef(repo, retirementRef(badBranch, badRevision), "{", ""); err != nil {
		t.Fatal(err)
	}

	subjects, err := (verifierFlow{}).Select(defaultConfig(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 2 || subjects[0].Branch != freshBranch ||
		subjects[1].Failure == nil || subjects[1].Branch != badBranch {
		t.Fatalf("Verifier subjects = %#v, want fresh branch then invalid retirement", subjects)
	}

	tk := newMemoryTracker()
	tk.seed(Item{ID: "1", Title: "bad retirement"})
	tk.seed(Item{ID: "2", Title: "covered branch"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	if builderSubjects, err := (builderFlow{}).Select(defaultConfig(), repo); err != nil || len(builderSubjects) != 0 {
		t.Fatalf("Builder subjects = (%#v, %v), want malformed retirement and branch excluded",
			builderSubjects, err)
	}
	cfg := defaultConfig()
	cfg.Flows.Verifier.Merge = "squash"
	if _, err := recordPreparingHostRetirement(
		cfg,
		repo,
		freshBranch,
		freshRevision,
		Item{ID: "2", Title: "covered branch"},
	); err != nil {
		t.Fatalf("unrelated retirement preparation = %v, want success", err)
	}
}

func TestVerifierActRefreshesWinningVerdictBeforeReview(t *testing.T) {
	branch := "forest/32-winning-verdict"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "32", Title: "winning verdict", UpdatedAt: "u1"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "test", Run: "true"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = false
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 {
		t.Fatalf("stale Verdict Select = (%#v, %v), want one branch", subjects, err)
	}

	root := t.TempDir()
	writer := filepath.Join(root, "writer")
	origin := runGitTest(t, repo, "remote", "get-url", "origin")
	runGitTest(t, root, "clone", "-q", origin, writer)
	if err := writeChecks(writer, reviewed, checksNote{Status: "pass", RunID: "winner"}, testCommitIdentity()); err != nil {
		t.Fatal(err)
	}
	if err := writeVerdict(writer, reviewed, verdictNote{
		Verdict: "changes", Notes: "winning rejection", Reviewer: "other-verifier",
		Model: "other-model", DefSHA: strings.Repeat("b", 16), RunID: "winner",
	}, testCommitIdentity()); err != nil {
		t.Fatal(err)
	}

	oldRun := runPhase
	runPhase = func(string, string, *Agent, string, string) (runStats, error) {
		t.Fatal("stale checkout paid for a duplicate Verifier review")
		return runStats{}, nil
	}
	defer func() { runPhase = oldRun }()
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	listCalls, commentCalls := 0, 0
	projectionCommand = func(args ...string) ([]byte, error) {
		if args[0] == "pr" && args[1] == "list" {
			listCalls++
			head := reviewed
			if listCalls > 1 {
				head = strings.Repeat("a", 40)
			}
			return []byte(`[{"number":32,"url":"https://github.com/owner/repo/pull/32","headRefOid":"` +
				head + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		}
		if args[0] == "api" {
			commentCalls++
			return nil, nil
		}
		return nil, errors.New("unexpected Host command")
	}

	out, err := (verifierFlow{}).Act(cfg, repo, subjects[0], "stale-winner")
	if !errors.Is(err, errHostMergeUnavailable) || out.Status != "projection_failed" || out.Verdict != "changes" {
		t.Fatalf("stale winning Verdict Act = (%#v, %v), want fenced winning rejection", out, err)
	}
	if commentCalls != 0 {
		t.Fatalf("head-drifted winning Verdict issued %d Host comments", commentCalls)
	}
	fact, found, err := readRetirement(repo, branch, reviewed)
	if err != nil || !found || fact.Record.State != "preparing" {
		t.Fatalf("stale winning Verdict preparation = (%#v, %v, %v), want no pending intent", fact, found, err)
	}
}

func TestVerifierHostPreparationRetriesWithoutStall(t *testing.T) {
	branch := "forest/35-host-retry"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "35", Title: "host retry"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	projectionCommand = func(args ...string) ([]byte, error) {
		return nil, errors.Join(errHostMergeUnavailable, errHostRevisionMoved)
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Agent = "verifier"
	subject := Subject{Key: "branch-" + branch, Kind: subjectBranch, Revision: reviewed,
		ID: "35", Branch: branch}
	for range stalledRunLimit {
		if code := actOnSubject(verifierFlow{}, cfg, repo, subject, nil); code != 1 {
			t.Fatalf("transient Host pass code = %d, want retryable failure", code)
		}
	}
	if stalled, err := stalledOn(repo, "verifier", subject.Key, reviewed); err != nil || stalled {
		t.Fatalf("transient Host preparation stalled = (%v, %v), want retryable", stalled, err)
	}
	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement {
		t.Fatalf("post-transient Select = (%#v, %v), want durable retirement retry", subjects, err)
	}
}

func TestVerifierPreparationMigrationRetriesTransientHostQuery(t *testing.T) {
	tests := []struct {
		name, id, state string
		command         func(string, string) func(...string) ([]byte, error)
	}{
		{
			name: "merged query", id: "43", state: "preparing",
			command: func(_, _ string) func(...string) ([]byte, error) {
				return func(...string) ([]byte, error) {
					return nil, errors.New("transient merged Host query failure")
				}
			},
		},
		{
			name: "open query", id: "44", state: "preparing",
			command: func(_, _ string) func(...string) ([]byte, error) {
				return func(args ...string) ([]byte, error) {
					if args[0] == "api" {
						return []byte(`[[]]`), nil
					}
					return nil, errors.New("transient open Host query failure")
				}
			},
		},
		{
			name: "advanced target query", id: "45", state: "pending",
			command: func(branch, advanced string) func(...string) ([]byte, error) {
				prCalls := 0
				return func(args ...string) ([]byte, error) {
					if args[0] == "api" {
						return []byte(`[[]]`), nil
					}
					if len(args) >= 2 && args[0] == "pr" && args[1] == "list" {
						prCalls++
						if prCalls%2 == 1 {
							return []byte(`[{"number":45,"url":"https://github.com/owner/repo/pull/45","headRefOid":"` +
								advanced + `","headRefName":"` + branch +
								`","baseRefName":"master","isCrossRepository":false}]`), nil
						}
					}
					return nil, errors.New("transient advanced Host query failure")
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			branch := "forest/" + tc.id + "-migration-retry"
			repo, _, reviewed, _ := newVerifierBranch(t, branch)
			cfg := defaultConfig()
			cfg.Repo = "owner/repo"
			cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
			item := Item{ID: tc.id, Title: "migration retry"}
			if tc.state == "preparing" {
				if _, err := recordPreparingHostRetirement(cfg, repo, branch, reviewed, item); err != nil {
					t.Fatal(err)
				}
			} else {
				agent := testVerifierAgent()
				if _, err := recordRetirement(repo, retirementRecord{
					Branch: branch, Revision: reviewed, ItemID: item.ID, Transport: "host",
					Strategy: "squash", Title: item.Title, State: tc.state,
					Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
				}); err != nil {
					t.Fatal(err)
				}
			}
			runGitTest(t, repo, "checkout", "-q", branch)
			rebaseTestWriteFile(t, filepath.Join(repo, "advanced.txt"), "advanced\n")
			runGitTest(t, repo, "add", "advanced.txt")
			runGitTest(t, repo, "commit", "-q", "-m", "advance before Host query")
			runGitTest(t, repo, "push", "-q", "origin", branch)
			advanced := remoteBranchHead(t, repo, branch)

			tk := newMemoryTracker()
			tk.seed(item)
			oldTracker := trackerFor
			trackerFor = func(string) Tracker { return tk }
			defer func() { trackerFor = oldTracker }()
			oldProjection := projectionCommand
			projectionCommand = tc.command(branch, advanced)
			defer func() { projectionCommand = oldProjection }()
			subject := Subject{Key: retirementSubjectKey(branch), Kind: subjectRetirement,
				Revision: reviewed, ID: item.ID, Branch: branch, Item: item}
			for range stalledRunLimit {
				if code := actOnSubject(verifierFlow{}, cfg, repo, subject, nil); code != 0 {
					t.Fatalf("preparation migration pass code = %d, want pending retry", code)
				}
			}
			for _, key := range []string{subject.Key, retirementAgentSubjectKey(branch)} {
				if stalled, err := stalledOn(repo, "verifier", key, reviewed); err != nil || stalled {
					t.Fatalf("transient migration query stalled %s = (%v, %v), want retryable",
						key, stalled, err)
				}
			}
			fact, found, err := readRetirement(repo, branch, reviewed)
			if err != nil || !found || fact.Record.State != tc.state {
				t.Fatalf("transient migration retained fact = (%#v, found=%v, err=%v)", fact, found, err)
			}
			subjects, err := (verifierFlow{}).Select(cfg, repo)
			if err != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement {
				t.Fatalf("post-transient Select = (%#v, %v), want durable retirement retry", subjects, err)
			}
		})
	}
}

func TestVerifierMalformedHostProjectionUsesFailureBrake(t *testing.T) {
	branch := "forest/41-malformed-projection"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "41", Title: "malformed Projection"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	projectionCommand = func(args ...string) ([]byte, error) {
		if args[0] == "pr" && args[1] == "list" {
			return []byte(`[{"number":0,"url":"","headRefOid":"` + reviewed +
				`","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		}
		return nil, errors.New("unexpected Host command")
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Agent = "verifier"
	subject := Subject{Key: "branch-" + branch, Kind: subjectBranch, Revision: reviewed,
		ID: "41", Branch: branch}
	if code := actOnSubject(verifierFlow{}, cfg, repo, subject, nil); code != 1 {
		t.Fatalf("malformed Host pass code = %d, want failure", code)
	}
	if stalled, err := stalledOn(repo, "verifier", subject.Key, reviewed); err != nil || !stalled {
		t.Fatalf("malformed Host Projection stalled = (%v, %v), want hard brake", stalled, err)
	}
}

func TestVerifierRetirementIgnoresBranchFailureBrake(t *testing.T) {
	branch := "forest/38-pending-braked"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	writeApprovalNotes(t, repo, reviewed, agent)
	if _, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "38", Transport: "host",
		Strategy: "squash", Title: "pending", State: "pending",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	}); err != nil {
		t.Fatal(err)
	}
	for range stalledRunLimit {
		if err := recordStalled(repo, "verifier", "branch-"+branch, reviewed); err != nil {
			t.Fatal(err)
		}
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement {
		t.Fatalf("braked branch retirement Select = (%#v, %v), want recovery", subjects, err)
	}
}

func TestVerifierBranchLossRetainsObservedHostMergeUntilApproval(t *testing.T) {
	branch := "forest/31-branch-loss"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")

	tk := newMemoryTracker()
	tk.seed(Item{ID: "31", Title: "branch loss", UpdatedAt: "u1", Tags: []string{readyTag}})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.AutoMerge = false
	if _, err := recordPreparingHostRetirement(cfg, repo, branch, reviewed,
		Item{ID: "31", Title: "branch loss"}); err != nil {
		t.Fatal(err)
	}
	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement {
		t.Fatalf("branch-loss Select = (%#v, %v), want one durable retirement", subjects, err)
	}

	runGitTest(t, repo, "branch", "-D", branch)
	if err := deleteRef(repo, "refs/heads/"+branch, reviewed); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "fetch", "-q", "--prune", "origin")

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	writes := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "api":
			return mergedProjectionPage(`{"number":31,"html_url":"https://github.com/owner/repo/pull/31","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"` + reviewed + `","ref":"` + branch + `","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
			return []byte(`[]`), nil
		case len(args) >= 2 && args[0] == "pr" && (args[1] == "create" || args[1] == "merge"):
			writes++
			return nil, errors.New("duplicate Host write")
		default:
			return nil, errors.New("unexpected Host command")
		}
	}

	recoveries, selectErr := (verifierFlow{}).Select(cfg, repo)
	if selectErr != nil || len(recoveries) != 1 || recoveries[0].Kind != subjectRetirement {
		t.Fatalf("fresh branch-loss Select = (%#v, %v), want durable retirement", recoveries, selectErr)
	}
	out, err := (verifierFlow{}).Act(cfg, repo, recoveries[0], "observe-merge")
	if err != nil || out.Status != "merge_pending" {
		t.Fatalf("branch-loss Act = (status=%q, err=%v), want retained merge_pending", out.Status, err)
	}
	facts, err := listRetirements(repo)
	if err != nil || len(facts) != 1 || facts[0].Record.State != "observed" {
		t.Fatalf("observed Host retirement = (%#v, %v), want one durable observation", facts, err)
	}
	if candidates, err := (builderFlow{}).Select(cfg, repo); err != nil || len(candidates) != 0 {
		t.Fatalf("Builder re-exposed Host-merged Item = (%#v, %v)", candidates, err)
	}

	agent := testVerifierAgent()
	writeApprovalNotes(t, repo, reviewed, agent)
	recoveries, err = (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(recoveries) != 1 || recoveries[0].Kind != subjectRetirement {
		t.Fatalf("observed recovery Select = (%#v, %v), want one retirement", recoveries, err)
	}
	out, err = (verifierFlow{}).Act(cfg, repo, recoveries[0], "recover-merge")
	if err != nil || out.Status != "merged" {
		t.Fatalf("observed recovery Act = (status=%q, err=%v), want merged", out.Status, err)
	}
	if out.Agent != agent.Name || out.Model != agent.Model || out.DefSHA != agent.DefSHA {
		t.Fatalf("observed recovery attribution = %#v, want %#v", out, agent)
	}
	if writes != 0 {
		t.Fatalf("observed recovery made %d duplicate Host writes", writes)
	}
	if _, err := tk.Get("31"); err == nil {
		t.Fatal("observed recovery left the Tracker Item open")
	}
	if facts, err := listRetirements(repo); err != nil || len(facts) != 0 {
		t.Fatalf("observed recovery facts = (%#v, %v), want retired", facts, err)
	}
}

// TestVerifierActMigratesHostPreparationAndPinsPostRebaseEvidence drives the
// full Verifier Act path through Host preparation, rebase, Checks, and Verdict.
func TestVerifierActMigratesHostPreparationAndPinsPostRebaseEvidence(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/54-host-rebase"
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	oldHead := remoteBranchHead(t, repo, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	rebaseTestWriteFile(t, filepath.Join(repo, "master.txt"), "master\n")
	runGitTest(t, repo, "add", "master.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "master work")
	runGitTest(t, repo, "push", "-q", "origin", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")

	item := Item{ID: "54", Title: "Host rebase"}
	tk := newMemoryTracker()
	tk.seed(item)
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	created := false
	var commitIDs []string
	projectionCommand = func(args ...string) ([]byte, error) {
		if !created {
			fact, found, err := readRetirement(repo, branch, oldHead)
			if err != nil || !found || fact.Record.State != "preparing" {
				t.Fatalf("first Host sink preparation = (%#v, %v, %v), want preparing fact", fact, found, err)
			}
		}
		switch {
		case args[0] == "pr" && args[1] == "list":
			if !created {
				return []byte(`[]`), nil
			}
			head := remoteBranchHead(t, repo, branch)
			return []byte(`[{"number":54,"url":"https://github.com/owner/repo/pull/54","headRefOid":"` + head +
				`","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		case args[0] == "api" && hasArgumentPair(args, "--method", "GET"):
			return []byte(`[[]]`), nil
		case args[0] == "pr" && args[1] == "create":
			created = true
			return []byte("https://github.com/owner/repo/pull/54"), nil
		case args[0] == "api" && hasArgumentPair(args, "--method", "POST"):
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--field" && strings.HasPrefix(args[i+1], "commit_id=") {
					commitIDs = append(commitIDs, strings.TrimPrefix(args[i+1], "commit_id="))
				}
			}
			return nil, nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	oldRun := runPhase
	runPhase = func(_ string, wtDir string, _ *Agent, _ string, _ string) (runStats, error) {
		if err := os.WriteFile(filepath.Join(wtDir, "review.json"), []byte(`{"verdict":"approve","summary":"approved","notes":""}`), 0o644); err != nil {
			return runStats{}, err
		}
		return runStats{}, nil
	}
	defer func() { runPhase = oldRun }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "true", Run: "true"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = false
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: subjectBranch, Revision: oldHead,
		ID: item.ID, Branch: branch}, "host-rebase")
	if err != nil || out.Status != "reviewed" {
		t.Fatalf("Host rebase Verifier Act = (%#v, %v), want reviewed", out, err)
	}
	newHead := remoteBranchHead(t, repo, branch)
	if newHead == oldHead || out.BaseSHA != newHead {
		t.Fatalf("Host rebase heads = (old=%s, new=%s, outcome=%s), want post-rebase outcome", oldHead, newHead, out.BaseSHA)
	}
	if len(commitIDs) != 1 || commitIDs[0] != newHead {
		t.Fatalf("Host Verdict/Checks commit ID = %v, want %s", commitIDs, newHead)
	}
	if _, found, err := readRetirement(repo, branch, oldHead); err != nil || found {
		t.Fatalf("pre-rebase preparation = (found=%v, err=%v), want migrated away", found, err)
	}
	fact, found, err := readRetirement(repo, branch, newHead)
	if err != nil || !found || fact.Record.State != "pending" || fact.Record.BuiltComment {
		t.Fatalf("post-rebase preparation = (%#v, found=%v, err=%v), want incomplete pending fact",
			fact, found, err)
	}
	fact, err = recoverRetirementBuiltComment(cfg, repo, fact, item)
	if err != nil || !fact.Record.BuiltComment {
		t.Fatalf("post-rebase Builder comment recovery = (%#v, %v), want complete", fact, err)
	}
	fresh, err := tk.Get(item.ID)
	if err != nil || len(fresh.Comments) == 0 ||
		!strings.Contains(fresh.Comments[len(fresh.Comments)-1].Body, "revision="+newHead) {
		t.Fatalf("post-rebase Builder comment = (%#v, %v), want Revision %s", fresh.Comments, err, newHead)
	}
}

// TestVerifierActProjectsChecksAtPostRebaseHead drives the full Verifier Act
// failure path and pins the Host Checks comment to the rebased Revision.
func TestVerifierActProjectsChecksAtPostRebaseHead(t *testing.T) {
	branch := "forest/55-host-checks"
	repo, _, oldHead, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	rebaseTestWriteFile(t, filepath.Join(repo, "master.txt"), "master\n")
	runGitTest(t, repo, "add", "master.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "master work")
	runGitTest(t, repo, "push", "-q", "origin", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")

	item := Item{ID: "55", Title: "Host Checks"}
	tk := newMemoryTracker()
	tk.seed(item)
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	created := false
	var commitIDs []string
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "pr" && args[1] == "list":
			if !created {
				return []byte(`[]`), nil
			}
			head := remoteBranchHead(t, repo, branch)
			return []byte(`[{"number":55,"url":"https://github.com/owner/repo/pull/55","headRefOid":"` + head +
				`","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		case args[0] == "api" && hasArgumentPair(args, "--method", "GET"):
			return []byte(`[[]]`), nil
		case args[0] == "pr" && args[1] == "create":
			created = true
			return []byte("https://github.com/owner/repo/pull/55"), nil
		case args[0] == "api" && hasArgumentPair(args, "--method", "POST"):
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--field" && strings.HasPrefix(args[i+1], "commit_id=") {
					commitIDs = append(commitIDs, strings.TrimPrefix(args[i+1], "commit_id="))
				}
			}
			return nil, nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "failing", Run: "false"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: subjectBranch, Revision: oldHead,
		ID: item.ID, Branch: branch}, "host-checks")
	if err != nil || out.Status != "checks_failed" {
		t.Fatalf("Host Checks Verifier Act = (%#v, %v), want checks_failed", out, err)
	}
	newHead := remoteBranchHead(t, repo, branch)
	if newHead == oldHead || out.BaseSHA != newHead {
		t.Fatalf("Host Checks heads = (old=%s, new=%s, outcome=%s), want post-rebase outcome", oldHead, newHead, out.BaseSHA)
	}
	if len(commitIDs) != 1 || commitIDs[0] != newHead {
		t.Fatalf("Host Checks commit ID = %v, want %s", commitIDs, newHead)
	}
	fact, found, err := readRetirement(repo, branch, newHead)
	if err != nil || !found || fact.Record.State != "preparing" {
		t.Fatalf("failed Checks preparation = (%#v, found=%v, err=%v), want retained", fact, found, err)
	}
	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement {
		t.Fatalf("failed Checks Verifier Select = (%#v, %v), want retained retirement", subjects, err)
	}
	retry, err := (verifierFlow{}).Act(cfg, repo, subjects[0], "host-checks-retry")
	if err != nil || retry.Status != "checks_failed" || len(commitIDs) != 1 {
		t.Fatalf("failed Checks retry = (%#v, comments=%v, err=%v), want no repeated work", retry, commitIDs, err)
	}
	if subjects, err := (fixerFlow{}).Select(cfg, repo); err != nil ||
		len(subjects) != 1 || subjects[0].Branch != branch {
		t.Fatalf("failed Checks Fixer Select = (%#v, %v), want repair branch", subjects, err)
	}
}

func TestVerifierRecoversBuilderCommentBeforeAgentWork(t *testing.T) {
	repo, branch, head := fixerBranch(t)
	runGitTest(t, repo, "checkout", "-q", "master")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "9", Title: "comment recovery"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection.Enabled = true
	cfg.Projection.MergeViaHost = true
	out, err := (verifierFlow{}).Act(cfg, repo, Subject{
		Key: "branch-" + branch, Kind: subjectBranch, Revision: head,
		Label: branch, ID: "9", Branch: branch,
	}, "comment-recovery")
	if err == nil || out.Status != "agent_failed" {
		t.Fatalf("Verifier after comment recovery = (%#v, %v), want later agent failure", out, err)
	}
	item, getErr := tk.Get("9")
	if getErr != nil || len(item.Comments) != 1 ||
		!strings.Contains(item.Comments[0].Body, "iron-forest:built revision="+head) {
		t.Fatalf("recovered Builder comment = (%#v, %v), want exact Revision marker", item.Comments, getErr)
	}
}

func TestVerifierRetirementRecoversBuilderCommentAfterPreparationStop(t *testing.T) {
	repo, branch, head := fixerBranch(t)
	runGitTest(t, repo, "checkout", "-q", "master")
	tk := newMemoryTracker()
	tk.seed(Item{ID: "9", Title: "comment recovery"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection.Enabled = true
	cfg.Projection.MergeViaHost = true
	item, err := tk.Get("9")
	if err != nil {
		t.Fatal(err)
	}
	fact, err := recordPreparingHostRetirement(cfg, repo, branch, head, item)
	if err != nil {
		t.Fatal(err)
	}
	legacy := fact.Record
	legacy.BuiltComment = false
	if _, err := replaceRetirement(repo, fact, legacy); err != nil {
		t.Fatal(err)
	}

	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement {
		t.Fatalf("restart selection = (%#v, %v), want one retirement recovery", subjects, err)
	}
	out, actErr := (verifierFlow{}).Act(cfg, repo, subjects[0], "retirement-comment-recovery")
	if actErr == nil || out.Status != "agent_failed" {
		t.Fatalf("retirement recovery = (%#v, %v), want comment before later agent failure", out, actErr)
	}
	got, err := tk.Get("9")
	if err != nil || len(got.Comments) != 1 ||
		!strings.Contains(got.Comments[0].Body, "iron-forest:built revision="+head) {
		t.Fatalf("retirement recovered comment = (%#v, %v), want exact marker", got.Comments, err)
	}
}

func TestMergedHostRecoveryPersistsObservationBeforeComment(t *testing.T) {
	branch := "forest/9-observed-before-comment"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	tk := &invisibleCommentTracker{memoryTracker: newMemoryTracker()}
	item := Item{ID: "9", Title: "observed recovery"}
	tk.seed(item)
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}

	out, err := recoverHostMergedProjection(
		cfg, repo, branch, reviewed, item,
		Outcome{Branch: branch, BaseSHA: reviewed},
	)
	if err == nil || out.Status != "comment_failed" {
		t.Fatalf("merged recovery = (%#v, %v), want comment failure after observation", out, err)
	}
	fact, found, readErr := readRetirement(repo, branch, reviewed)
	if readErr != nil || !found || fact.Record.State != "observed" ||
		fact.Record.BuiltComment {
		t.Fatalf("durable observation = (%#v, found=%v, err=%v), want uncompleted observed fact",
			fact, found, readErr)
	}
	if err := deleteRef(repo, "refs/heads/"+branch, reviewed); err != nil {
		t.Fatal(err)
	}
	subjects, err := (builderFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 0 {
		t.Fatalf("Builder after lost branch and comment = (%#v, %v), want retirement coverage", subjects, err)
	}
}

func TestMalformedStallDoesNotSuppressHealthyVerifierBranch(t *testing.T) {
	_, repo, revision := notesTestRepository(t)
	badBranch := "forest/31-bad-stall"
	goodBranch := "forest/32-good"
	runGitTest(t, repo, "push", "-q", "origin",
		revision+":refs/heads/"+badBranch,
		revision+":refs/heads/"+goodBranch)
	ref := stalledRef("verifier", "branch-"+badBranch)
	blob, err := writeBlob(repo, "{malformed")
	if err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "push", "-q", "origin", blob+":"+ref)

	subjects, err := (verifierFlow{}).Select(defaultConfig(), repo)
	if err != nil {
		t.Fatal(err)
	}
	badFailures := 0
	goodHealthy := 0
	for _, subject := range subjects {
		if subject.Branch == badBranch && subject.Failure != nil {
			badFailures++
		}
		if subject.Branch == goodBranch && subject.Failure == nil {
			goodHealthy++
		}
	}
	if badFailures != 1 || goodHealthy != 1 {
		t.Fatalf("Verifier Subjects = %#v, want one quarantined brake and one healthy branch", subjects)
	}
}
