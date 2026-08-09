package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type failOnceTagTracker struct {
	*memoryTracker
	failures int
}

func (t *failOnceTagTracker) SetTags(id string, add, remove []string) error {
	if t.failures > 0 {
		t.failures--
		return errors.New("injected Tracker handoff failure")
	}
	return t.memoryTracker.SetTags(id, add, remove)
}

func fixerBranch(t *testing.T) (string, string, string) {
	t.Helper()
	repo := setupTestRepo(t)
	branch := "forest/9-fixer"
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "commit", "-qam", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", branch)
	return repo, branch, runGitTest(t, repo, "rev-parse", "HEAD")
}

func writeFixerNote(t *testing.T, repo, ref, head, body string) {
	t.Helper()
	runGitTest(t, repo, "notes", "--ref="+ref, "add", "-m", body, head)
	runGitTest(t, repo, "push", "-q", "origin", notesRef(ref)+":"+notesRef(ref))
}

func removeFixerNote(t *testing.T, repo, ref, head string) {
	t.Helper()
	runGitTest(t, repo, "notes", "--ref="+ref, "remove", head)
	runGitTest(t, repo, "push", "-q", "origin", notesRef(ref)+":"+notesRef(ref))
}

func TestFixerActSkipsWhenSelectedEvidenceDisappears(t *testing.T) {
	repo, _, head := fixerBranch(t)
	if err := writeChecks(repo, head, checksNote{Status: "fail"}, testCommitIdentity()); err != nil {
		t.Fatal(err)
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	selected, err := (fixerFlow{}).Select(cfg, repo)
	if err != nil || len(selected) != 1 {
		t.Fatalf("Fixer Select = (%#v, %v), want one qualified Subject", selected, err)
	}
	removeFixerNote(t, repo, checksNotesRef, head)

	tk := newMemoryTracker()
	tk.seed(Item{ID: "9", Title: "stale evidence"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	writeAgentFixture(t, repo, "builder", "builder-model")
	oldRun := runPhase
	runPhase = func(string, string, *Agent, string, string) (runStats, error) {
		t.Fatal("Fixer spent a model run after evidence stopped qualifying")
		return runStats{}, nil
	}
	defer func() { runPhase = oldRun }()

	out, err := (fixerFlow{}).Act(cfg, repo, selected[0], "run-stale-evidence")
	if err != nil || out.Status != "skipped" {
		t.Fatalf("Fixer Act after evidence removal = (%#v, %v), want skipped", out, err)
	}
}
func TestFixerActRechecksBrakeAfterSelection(t *testing.T) {
	repo, _, head := fixerBranch(t)
	if err := writeChecks(repo, head, checksNote{Status: "fail"}, testCommitIdentity()); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	selected, err := (fixerFlow{}).Select(cfg, repo)
	if err != nil || len(selected) != 1 {
		t.Fatalf("Fixer Select = (%#v, %v), want one qualified Subject", selected, err)
	}
	for range stalledRunLimit {
		if err := recordStalled(repo, "fixer", selected[0].Key, head); err != nil {
			t.Fatal(err)
		}
	}
	oldTracker := trackerFor
	trackerFor = func(string) Tracker {
		t.Fatal("Fixer read the Tracker after a concurrent brake")
		return nil
	}
	defer func() { trackerFor = oldTracker }()
	out, err := (fixerFlow{}).Act(cfg, repo, selected[0], "run-braked")
	if err != nil || out.Status != "skipped" {
		t.Fatalf("Fixer Act after brake = (%#v, %v), want skipped", out, err)
	}
}

func TestFixerSelectCarriesMalformedEvidenceToRevisionFailure(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		body string
	}{
		{
			name: "Verdict",
			ref:  verdictNotesRef,
			body: `{"verdict":"bogus","reviewer":"reviewer","model":"model","def_sha":"aaaaaaaaaaaaaaaa"}`,
		},
		{
			name: "Checks",
			ref:  checksNotesRef,
			body: `{"status":"bogus"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, branch, head := fixerBranch(t)
			writeFixerNote(t, repo, tc.ref, head, tc.body)
			cfg := defaultConfig()
			cfg.Repo = "owner/repo"

			subjects, err := (fixerFlow{}).Select(cfg, repo)
			if err != nil || len(subjects) != 1 {
				t.Fatalf("Fixer Select malformed %s = (%#v, %v), want one Subject", tc.name, subjects, err)
			}
			subject := subjects[0]
			if subject.Branch != branch || subject.Revision != head || subject.Failure == nil {
				t.Fatalf("malformed %s Subject = %#v, want exact Revision failure", tc.name, subject)
			}
			if !errors.Is(subject.Failure, errRetirementEvidenceInvalid) {
				t.Fatalf("malformed %s failure = %v, want hard evidence error", tc.name, subject.Failure)
			}

			tk := newMemoryTracker()
			tk.seed(Item{ID: "9", Title: "malformed evidence"})
			trackerCalls := 0
			oldTracker := trackerFor
			trackerFor = func(string) Tracker {
				trackerCalls++
				return tk
			}
			defer func() { trackerFor = oldTracker }()

			out, actErr := (fixerFlow{}).Act(cfg, repo, subject, "run-malformed-"+tc.name)
			if !errors.Is(actErr, errRetirementEvidenceInvalid) || out.Status != "evidence_failed" {
				t.Fatalf("malformed %s Act = (%#v, %v), want hard evidence failure", tc.name, out, actErr)
			}
			if trackerCalls != 0 {
				t.Fatalf("malformed %s called Tracker %d times before failing", tc.name, trackerCalls)
			}

			if code := actOnSubject(fixerFlow{}, cfg, repo, subject, nil); code != 1 {
				t.Fatalf("malformed %s Act admission code = %d, want failure", tc.name, code)
			}
			stalled, stallErr := stalledOn(repo, "fixer", subject.Key, subject.Revision)
			if stallErr != nil || !stalled {
				t.Fatalf("malformed %s Revision stalled = (%v, %v), want hard brake", tc.name, stalled, stallErr)
			}
		})
	}
}

func TestFixerPublicationAtomicallyAdvancesBranchAndAttempt(t *testing.T) {
	for _, rejectAttempt := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "rejected"}[rejectAttempt], func(t *testing.T) {
			repo, branch, oldHead := fixerBranch(t)
			runGitTest(t, repo, "checkout", "-q", "master")
			wtDir, baseSHA, err := createWorktreeAtBranch(repo, workspaceDir(repo), branch)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanupWorktree(repo, wtDir)
			if baseSHA != oldHead {
				t.Fatalf("worktree base = %s, want %s", baseSHA, oldHead)
			}
			if err := os.WriteFile(filepath.Join(wtDir, "repair.txt"), []byte("repair\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			const key = "branch-forest/9-fixer"
			if rejectAttempt {
				remote, err := gitOut(repo, "remote", "get-url", "origin")
				if err != nil {
					t.Fatal(err)
				}
				hook := filepath.Join(remote, "hooks", "pre-receive")
				script := "#!/bin/sh\nwhile read old new ref; do\n" +
					"  [ \"$ref\" = \"refs/forest/attempt/" + key + "\" ] && exit 1\n" +
					"done\nexit 0\n"
				if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			head, count, publishErr := commitFixAndPush(
				repo, wtDir, branch, oldHead, key, 0,
				testVerifierAgent().Commit,
				Item{ID: "9", Title: "fixer"},
			)
			remoteHead, present, headErr := lookupBranchHead(repo, branch)
			if headErr != nil || !present {
				t.Fatalf("remote branch = (%s, %v, %v)", remoteHead, present, headErr)
			}
			attempts, attemptsErr := readAttempts(repo, key)
			if attemptsErr != nil {
				t.Fatal(attemptsErr)
			}
			if rejectAttempt {
				if publishErr == nil || remoteHead != oldHead || attempts != 0 {
					t.Fatalf("rejected atomic push = (head=%s count=%d err=%v), want unchanged branch and attempts", remoteHead, attempts, publishErr)
				}
				return
			}
			if publishErr != nil || head == oldHead || remoteHead != head || count != 1 || attempts != 1 {
				t.Fatalf("atomic push = (head=%s remote=%s count=%d stored=%d err=%v), want both refs advanced once", head, remoteHead, count, attempts, publishErr)
			}
		})
	}
}

func TestFixerRecoversChecksProjectionAndFailedHandoff(t *testing.T) {
	repo, branch, head := fixerBranch(t)
	runGitTest(t, repo, "checkout", "-q", "master")
	writeFixerNote(t, repo, checksNotesRef, head,
		`{"status":"fail","results":[],"run_id":"prior"}`)
	key := "branch-" + branch
	if count, err := bumpAttempts(repo, key); err != nil || count != 1 {
		t.Fatalf("seed attempts = (%d, %v)", count, err)
	}
	tk := newMemoryTracker()
	tk.seed(Item{ID: "9", Title: "fixer"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	posts := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		if args[0] == "pr" && args[1] == "list" {
			return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefOid":"` +
				head + `","headRefName":"` + branch +
				`","baseRefName":"master","isCrossRepository":false}]`), nil
		}
		if args[0] == "api" && hasArgumentPair(args, "--method", "GET") {
			return []byte(`[[]]`), nil
		}
		if args[0] == "api" && hasArgumentPair(args, "--method", "POST") {
			posts++
			return nil, nil
		}
		return nil, errors.New("unexpected host command")
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection.Enabled = true
	cfg.Flows.Fixer.Attempts = 1
	out, actErr := (fixerFlow{}).Act(cfg, repo, Subject{
		Key: key, Kind: subjectBranch, Revision: head,
		Label: branch, ID: "9", Branch: branch,
	}, "recover-handoff")
	if actErr != nil || out.Status != "exhausted" || posts != 1 {
		t.Fatalf("Fixer recovery = (%#v, %v, posts=%d), want projected Checks and exhausted handoff", out, actErr, posts)
	}
	item, err := tk.Get("9")
	if err != nil || !item.hasTag(failedLabel) || len(item.Comments) != 1 {
		t.Fatalf("failed handoff Item = (%#v, %v), want tag and one comment", item, err)
	}
	stalled, err := stalledOn(repo, "fixer", key, head)
	if err != nil || !stalled {
		t.Fatalf("failed handoff brake = (%v, %v), want terminal", stalled, err)
	}
}

func TestFixerRestartCompletesFailedHandoffAfterLastAtomicPublish(t *testing.T) {
	repo, branch, oldHead := fixerBranch(t)
	runGitTest(t, repo, "checkout", "-q", "master")
	wtDir, _, err := createWorktreeAtBranch(repo, workspaceDir(repo), branch)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "repair.txt"), []byte("last repair\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	key := "branch-" + branch
	newHead, count, err := commitFixAndPush(
		repo, wtDir, branch, oldHead, key, 0,
		testVerifierAgent().Commit,
		Item{ID: "9", Title: "fixer"},
	)
	cleanupWorktree(repo, wtDir)
	if err != nil || count != 1 || newHead == oldHead {
		t.Fatalf("last atomic publish = (%s, %d, %v)", newHead, count, err)
	}

	tk := &failOnceTagTracker{memoryTracker: newMemoryTracker(), failures: 1}
	tk.seed(Item{ID: "9", Title: "fixer"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	item, err := tk.Get("9")
	if err != nil {
		t.Fatal(err)
	}
	if err := markFixerFailed("owner/repo", repo, newHead, item); err == nil {
		t.Fatal("injected first handoff did not fail")
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Flows.Fixer.Attempts = 1
	subjects, err := (fixerFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 || subjects[0].Revision != newHead {
		t.Fatalf("restart selection = (%#v, %v), want exhausted new head", subjects, err)
	}
	out, actErr := (fixerFlow{}).Act(cfg, repo, subjects[0], "recover-handoff")
	if actErr != nil || out.Status != "exhausted" {
		t.Fatalf("restart handoff = (%#v, %v), want exhausted", out, actErr)
	}
	got, err := tk.Get("9")
	if err != nil || !got.hasTag(failedLabel) || len(got.Comments) != 1 {
		t.Fatalf("recovered failed handoff = (%#v, %v), want tag and one comment", got, err)
	}
	stalled, err := stalledOn(repo, "fixer", key, newHead)
	if err != nil || !stalled {
		t.Fatalf("recovered handoff brake = (%v, %v), want terminal", stalled, err)
	}
}
