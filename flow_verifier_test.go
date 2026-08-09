package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerifierSkipsHeadOwnedByTheFixer pins the spin this factory already paid
// for: a head whose checks failed carries a fact, and re-offering it re-runs
// every check and re-reviews the same commit forever. The lane that can repair
// it must be the one that selects it, and a new head must clear the fact.
func TestVerifierSkipsHeadOwnedByTheFixer(t *testing.T) {
	remote, work, _ := notesTestRepository(t)
	branch := "forest/9-conflicted"
	runGitTest(t, work, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, work, "commit", "-qam", "branch work")
	runGitTest(t, work, "push", "-q", "-u", "origin", branch)
	head := runGitTest(t, work, "rev-parse", "HEAD")
	noteSource := filepath.Join(t.TempDir(), "note-source")
	runGitTest(t, "", "clone", remote, noteSource)
	runGitTest(t, noteSource, "config", "user.name", "note-source")
	runGitTest(t, noteSource, "config", "user.email", "note-source@example.com")
	fixerWork := filepath.Join(t.TempDir(), "fixer-work")
	runGitTest(t, "", "clone", remote, fixerWork)

	cfg := defaultConfig()
	cfg.Repo = "example/repo"

	// With no notes at all the head is fresh work for the Verifier.
	subjects, err := verifierFlow{}.Select(cfg, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].Revision != head || subjects[0].ID != "9" ||
		canonicalAdmissionKey(subjects[0]) != "item-9" {
		t.Fatalf("fresh head = %#v, want item-9 at %s", subjects, head)
	}

	// A failing check is the fact a rebase conflict or a broken build leaves.
	fail := checksNote{
		Status:  "fail",
		RunID:   "run-1",
		Time:    nowRFC(),
		Results: []checkResult{{Name: "rebase", Code: 1, Output: "conflicts in file.txt"}},
	}
	if err := writeChecks(noteSource, head, fail); err != nil {
		t.Fatal(err)
	}
	subjects, err = verifierFlow{}.Select(cfg, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 0 {
		t.Fatalf("verifier still offers a failed head: %#v", subjects)
	}
	repairs, err := fixerFlow{}.Select(cfg, fixerWork)
	if err != nil {
		t.Fatal(err)
	}
	if len(repairs) != 1 || repairs[0].Revision != head || repairs[0].ID != "9" ||
		canonicalAdmissionKey(repairs[0]) != "item-9" {
		t.Fatalf("fixer subjects = %#v, want item-9 at %s", repairs, head)
	}
	for range stalledRunLimit {
		if err := recordStalled(fixerWork, "fixer", repairs[0].Key, head); err != nil {
			t.Fatal(err)
		}
	}
	if repairs, err = (fixerFlow{}).Select(cfg, fixerWork); err != nil || len(repairs) != 0 {
		t.Fatalf("fixer selected stalled head = (%#v, %v), want none", repairs, err)
	}

	// A repair moves the branch, and notes key to the commit, so the new head is
	// fresh work again without deleting anything.
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("repaired\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, work, "commit", "-qam", "repair")
	runGitTest(t, work, "push", "-q", "origin", branch)
	newHead := runGitTest(t, work, "rev-parse", "HEAD")
	subjects, err = verifierFlow{}.Select(cfg, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].Revision != newHead || subjects[0].ID != "9" ||
		canonicalAdmissionKey(subjects[0]) != "item-9" {
		t.Fatalf("repaired head = %#v, want item-9 at %s", subjects, newHead)
	}
	if err := writeVerdict(work, newHead, verdictNote{Verdict: "approve", Reviewer: "verifier", Model: "m", DefSHA: strings.Repeat("a", 16)}); err != nil {
		t.Fatal(err)
	}
	if subjects, err = (verifierFlow{}).Select(cfg, work); err != nil ||
		len(subjects) != 1 || subjects[0].Revision != newHead {
		t.Fatalf("approved head missing Checks = (%#v, %v), want check requalification", subjects, err)
	}
	if err := writeChecks(work, newHead, checksNote{Status: "pass"}); err != nil {
		t.Fatal(err)
	}
	for range stalledRunLimit {
		if err := recordStalled(work, "verifier", subjects[0].Key, newHead); err != nil {
			t.Fatal(err)
		}
	}
	if subjects, err = (verifierFlow{}).Select(cfg, work); err != nil || len(subjects) != 0 {
		t.Fatalf("verifier selected stalled approved head = (%#v, %v), want none", subjects, err)
	}
}

// TestVerifierMergeRequiresApproveAndPassingChecks is the falsifier for the
// merge admission gate: a branch reaches master only when its checks pass and
// its verdict approves, and auto_merge alone must never outweigh a rejection.
// It drives the full Act path for a rejected verdict, a failing check, and one
// successful merge. Flipping the operator at flow_verifier.go:186 to a logical
// AND would let a rejected branch merge, so this test must fail on that
// mutation: a test that survives the flip does not defend the property.
func TestVerifierMergeRequiresApproveAndPassingChecks(t *testing.T) {
	type result struct {
		out          Outcome
		repo         string
		branch       string
		masterBefore string
	}
	// runAct builds a one-branch repository, seeds an optional verdict on the
	// branch head, and drives the Verifier's Act to the finish. The tracker is
	// stubbed so the item read and the close after a merge never touch the host.
	runAct := func(t *testing.T, verdict string, checks []Check, autoMerge bool) result {
		t.Helper()
		repo := setupTestRepo(t)
		branch := "forest/9-change"
		runGitTest(t, repo, "checkout", "-q", "-b", branch)
		rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
		runGitTest(t, repo, "add", "branch.txt")
		runGitTest(t, repo, "commit", "-q", "-m", "branch work")
		runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
		head := remoteBranchHead(t, repo, branch)
		runGitTest(t, repo, "checkout", "-q", "master")
		masterBefore := remoteBranchHead(t, repo, "master")

		writeAgentFixture(t, repo, "verifier", "verifier-model")

		oldGH := ghJSON
		ghJSON = func(args ...string) ([]byte, error) {
			return []byte(`{"number":9,"title":"change","body":"","updatedAt":"u1","comments":[],"labels":[]}`), nil
		}
		defer func() { ghJSON = oldGH }()

		cfg := defaultConfig()
		cfg.Repo = "owner/repo"
		cfg.Checks = checks
		cfg.Flows.Verifier.Agent = "verifier"
		cfg.Flows.Verifier.AutoMerge = autoMerge
		cfg.Projection = ProjectionConfig{}

		if verdict != "" {
			if err := writeVerdict(repo, head, verdictNote{
				Verdict: verdict, Reviewer: "verifier", Model: "verifier-model",
				DefSHA: strings.Repeat("a", 16), RunID: "seed",
			}); err != nil {
				t.Fatalf("seed verdict: %v", err)
			}
		}

		out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: "branch", Revision: head,
			Label: branch, ID: "9", Branch: branch}, "run-1")
		if err != nil {
			t.Fatalf("Act: %v", err)
		}
		return result{out: out, repo: repo, branch: branch, masterBefore: masterBefore}
	}

	passing := []Check{{Name: "true", Run: "true"}}

	t.Run("rejected verdict never merges", func(t *testing.T) {
		r := runAct(t, "changes", passing, true)
		if r.out.Status == "merged" {
			t.Fatalf("a changes verdict merged: %#v", r.out)
		}
		if r.out.Status != "reviewed" {
			t.Fatalf("changes verdict status = %q, want reviewed", r.out.Status)
		}
		if got := remoteBranchHead(t, r.repo, "master"); got != r.masterBefore {
			t.Fatalf("master advanced to %s after a rejection, want %s", got, r.masterBefore)
		}
	})

	t.Run("failing check never merges", func(t *testing.T) {
		failing := []Check{{Name: "false", Run: "false"}}
		r := runAct(t, "approve", failing, true)
		if r.out.Status == "merged" {
			t.Fatalf("a failing check merged: %#v", r.out)
		}
		if r.out.Status != "checks_failed" {
			t.Fatalf("failing check status = %q, want checks_failed", r.out.Status)
		}
		if got := remoteBranchHead(t, r.repo, "master"); got != r.masterBefore {
			t.Fatalf("master advanced to %s despite a failed check, want %s", got, r.masterBefore)
		}
	})

	t.Run("approve with passing checks merges exactly once", func(t *testing.T) {
		r := runAct(t, "approve", passing, true)
		if r.out.Status != "merged" {
			t.Fatalf("approved, passing head status = %q, want merged", r.out.Status)
		}
		if got := remoteBranchHead(t, r.repo, "master"); got == r.masterBefore {
			t.Fatalf("master did not advance after a verified merge (%q)", got)
		}
		if out := runGitTest(t, r.repo, "ls-remote", "origin", "refs/heads/"+r.branch); out != "" {
			t.Fatalf("merged branch %q still exists on origin: %s", r.branch, out)
		}
	})
}

// TestVerifierPreflightFailureWritesNoPassNote proves the durable-fact outcome
// of item #187: with the check environment forced to fail, no declared check
// runs, no pass note is written on the Revision, the failure is classified as
// a mechanical one for an operator, and Select does not offer the head as
// mergeable. This is the falsifier for the old bug that initialised the note to
// "pass" before resolving the environment and then wrote that note anyway.
func TestVerifierPreflightFailureWritesNoPassNote(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-change"
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	head := remoteBranchHead(t, repo, branch)
	runGitTest(t, repo, "checkout", "-q", "master")

	writeAgentFixture(t, repo, "verifier", "verifier-model")

	// An approve verdict makes the head a merge candidate that only a
	// trustworthy checks note may admit; a preflight failure must never pass it.
	if err := writeVerdict(repo, head, verdictNote{
		Verdict: "approve", Reviewer: "verifier", Model: "verifier-model",
		DefSHA: strings.Repeat("a", 16), RunID: "seed",
	}); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}

	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) {
		return []byte(`{"number":9,"title":"change","body":"","updatedAt":"u1","comments":[],"labels":[]}`), nil
	}
	defer func() { ghJSON = oldGH }()

	oldEnv := checkEnvironment
	checkEnvironment = func() ([]string, func(), error) {
		return nil, func() {}, errors.New("locate mise: missing")
	}
	defer func() { checkEnvironment = oldEnv }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "true", Run: "true"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = true
	cfg.Projection = ProjectionConfig{}

	out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: "branch", Revision: head,
		Label: branch, ID: "9", Branch: branch}, "run-1")
	if err == nil {
		t.Fatalf("preflight failure returned no error: %#v", out)
	}
	if out.Status != "checks_environment_failed" {
		t.Fatalf("preflight failure status = %q, want checks_environment_failed", out.Status)
	}

	if _, ok, err := readChecks(repo, head); err != nil || ok {
		t.Fatalf("checks note on head = (found=%v, err=%v), want no note when checks never ran", ok, err)
	}

	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].Branch != branch {
		t.Fatalf("preflight-failed head = %#v, want one bounded check requalification", subjects)
	}
	repairs, err := (fixerFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range repairs {
		if s.Branch == branch {
			t.Fatalf("fixer took a preflight-failed head as if its code were wrong: %#v", s)
		}
	}
}

// TestVerifierPreflightRetryIgnoresExistingNote proves item #187's second pass:
// once a (stale) checks note exists on a Revision, a retry over the same head
// with a failing preflight must not clear checkErr because a note exists. The
// old code read the existing note as the winner and set checkErr to nil, which
// let an approved head review and merge without a single executed check. A
// preflight failure is classified early, before any note is read or written.
func TestVerifierPreflightRetryIgnoresExistingNote(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-change"
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	head := remoteBranchHead(t, repo, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	masterBefore := remoteBranchHead(t, repo, "master")

	writeAgentFixture(t, repo, "verifier", "verifier-model")

	if err := writeVerdict(repo, head, verdictNote{
		Verdict: "approve", Reviewer: "verifier", Model: "verifier-model",
		DefSHA: strings.Repeat("a", 16), RunID: "seed",
	}); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}
	// A stale pass note from an earlier, buggy pass already keys this Revision.
	if err := writeChecks(repo, head, checksNote{Status: "pass", RunID: "stale", Time: nowRFC()}); err != nil {
		t.Fatalf("seed stale checks: %v", err)
	}

	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) {
		return []byte(`{"number":9,"title":"change","body":"","updatedAt":"u1","comments":[],"labels":[]}`), nil
	}
	defer func() { ghJSON = oldGH }()

	oldEnv := checkEnvironment
	checkEnvironment = func() ([]string, func(), error) {
		return nil, func() {}, errors.New("locate mise: missing")
	}
	defer func() { checkEnvironment = oldEnv }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "true", Run: "true"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = true
	cfg.Projection = ProjectionConfig{}

	out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: "branch", Revision: head,
		Label: branch, ID: "9", Branch: branch}, "run-2")
	if err == nil {
		t.Fatalf("retry over an existing note returned no error: %#v", out)
	}
	if out.Status != "checks_environment_failed" {
		t.Fatalf("retry over an existing note status = %q, want checks_environment_failed (note must not clear checkErr)", out.Status)
	}
	if out.Status == "merged" {
		t.Fatalf("retry merged a Revision whose checks never ran once: %#v", out)
	}
	if got := remoteBranchHead(t, repo, "master"); got != masterBefore {
		t.Fatalf("master advanced to %s on a retry whose checks never ran, want %s", got, masterBefore)
	}
}

// TestVerifierRefusesPassNoteWhenCheckMutatesTree is the end-to-end regression
// for item #191's check-phase guard. A declared check that rewrites or stages a
// tracked file taints the very tree the check was meant to judge; even a
// reviewer that would only write review.json must yield no Verdict and no pass
// Checks note, because the green belongs to an uncommitted edit, never the
// Review revision.
func TestVerifierRefusesPassNoteWhenCheckMutatesTree(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  string
	}{
		{"rewrites a tracked file", `printf 'tainted\n' > file.txt`},
		{"stages a tracked file", `printf 'tainted\n' > file.txt && git add file.txt`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := setupTestRepo(t)
			branch := "forest/9-change"
			runGitTest(t, repo, "checkout", "-q", "-b", branch)
			rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
			runGitTest(t, repo, "add", "branch.txt")
			runGitTest(t, repo, "commit", "-q", "-m", "branch work")
			runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
			head := remoteBranchHead(t, repo, branch)
			masterBefore := remoteBranchHead(t, repo, "master")
			runGitTest(t, repo, "checkout", "-q", "master")

			writeAgentFixture(t, repo, "verifier", "verifier-model")

			oldGH := ghJSON
			ghJSON = func(args ...string) ([]byte, error) {
				return []byte(`{"number":9,"title":"change","body":"","updatedAt":"u1","comments":[],"labels":[]}`), nil
			}
			defer func() { ghJSON = oldGH }()

			// The reviewer is blameless: it would write only review.json and approve.
			reviewRan := false
			oldRun := runPhase
			runPhase = func(_ string, wtDir string, _ *Agent, _ string, _ string) (runStats, error) {
				reviewRan = true
				if err := os.WriteFile(filepath.Join(wtDir, "review.json"), []byte(`{"verdict":"approve","summary":"looks fine","notes":""}`), 0o644); err != nil {
					return runStats{}, err
				}
				return runStats{}, nil
			}
			defer func() { runPhase = oldRun }()

			cfg := defaultConfig()
			cfg.Repo = "owner/repo"
			cfg.Checks = []Check{{Name: "tainted", Run: tc.run}}
			cfg.Flows.Verifier.Agent = "verifier"
			cfg.Flows.Verifier.AutoMerge = true
			cfg.Projection = ProjectionConfig{}

			out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: "branch", Revision: head,
				Label: branch, ID: "9", Branch: branch}, "run-1")
			if err == nil {
				t.Fatalf("a check that tainted the tree returned no error: %#v", out)
			}
			if !strings.Contains(err.Error(), "file.txt") {
				t.Fatalf("refusal %q does not name the tainted tracked path", err)
			}
			if out.Status != "checks_refused" {
				t.Fatalf("status = %q, want checks_refused", out.Status)
			}

			// No pass Checks note and no Verdict, and the reviewer never ran: the
			// tainted green is refused before anything records it or acts on it.
			if _, ok, err := readChecks(repo, head); err != nil || ok {
				t.Fatalf("checks note on head = (found=%v, err=%v), want no note when a check tainted the tree", ok, err)
			}
			if _, ok, err := readVerdict(repo, head); err != nil || ok {
				t.Fatalf("verdict note on head = (found=%v, err=%v), want no Verdict when a check tainted the tree", ok, err)
			}
			if reviewRan {
				t.Fatalf("reviewer ran for a head whose checks tainted the tree")
			}
			if got := remoteBranchHead(t, repo, "master"); got != masterBefore {
				t.Fatalf("master advanced to %s, want %s, when a check tainted the tree", got, masterBefore)
			}
		})
	}
}

// TestVerifierReviewNamesMutationWhenPhaseErrors pins #191's verifier-review
// fix: the post-run clean-tree assertion must run even when the phase reports an
// error. A verifier that edits a tracked file and then crashes or times out would
// otherwise return only the harness error, never the required named clean-tree
// refusal, and the mutation would go unreported. The refusal must name the
// edited path even though the phase itself failed.
func TestVerifierReviewNamesMutationWhenPhaseErrors(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-change"
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	head := remoteBranchHead(t, repo, branch)
	runGitTest(t, repo, "checkout", "-q", "master")
	writeAgentFixture(t, repo, "verifier", "verifier-model")

	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) {
		return []byte(`{"number":9,"title":"change","body":"","updatedAt":"u1","comments":[],"labels":[]}`), nil
	}
	defer func() { ghJSON = oldGH }()

	// The verifier edits a tracked file and then crashes, so the harness error
	// and the mutation arrive together.
	phaseErr := errors.New("verifier crashed")
	mutated := false
	oldRun := runPhase
	runPhase = func(_ string, wtDir string, _ *Agent, _ string, _ string) (runStats, error) {
		mutated = true
		if err := os.WriteFile(filepath.Join(wtDir, "file.txt"), []byte("tainted\n"), 0o644); err != nil {
			return runStats{}, err
		}
		return runStats{}, phaseErr
	}
	defer func() { runPhase = oldRun }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Checks = []Check{{Name: "ok", Run: "true"}}
	cfg.Flows.Verifier.Agent = "verifier"
	cfg.Flows.Verifier.AutoMerge = true
	cfg.Projection = ProjectionConfig{}

	out, err := (verifierFlow{}).Act(cfg, repo, Subject{Key: "branch-" + branch, Kind: "branch", Revision: head,
		Label: branch, ID: "9", Branch: branch}, "run-1")
	if err == nil {
		t.Fatalf("a review that mutated the tree and crashed returned no error: %#v", out)
	}
	// The required named clean-tree refusal, not the bare harness error alone.
	if !strings.Contains(err.Error(), "file.txt") {
		t.Fatalf("refusal %q does not name the mutated tracked path; the phase error swallowed the tree check", err)
	}
	if !mutated {
		t.Fatal("the verifier did not run")
	}
	// No Verdict may rest on a tree the review itself changed.
	if _, ok, rerr := readVerdict(repo, head); rerr != nil || ok {
		t.Fatalf("verdict note on head = (found=%v, err=%v), want none when the review mutated the tree", ok, rerr)
	}
}

// TestStalledOnPersistsOutsideLedger pins the durable progress brake: three
// failures on one revision stop a subject, a new revision resets it, and the
// decision remains after the host-local ledger is removed.
func TestStalledOnPersistsOutsideLedger(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	const subject = "branch-forest/../../opaque ?~^:[x]"
	const revision = "aaa"
	if got, want := stalledRef("fixer", "branch-forest/9-x"), "refs/forest/stalled/fixer/branch-forest/9-x"; got != want {
		t.Fatalf("legacy stalled ref = %q, want %q", got, want)
	}
	if got := stalledRef("fixer", subject); !strings.HasPrefix(got, "refs/forest/stalled-opaque/fixer/") {
		t.Fatalf("opaque stalled ref = %q, want encoded namespace", got)
	}
	for _, invalid := range []string{".item", "item.lock", "foo./bar", "item..x", "item@{x", "item?x", `item\x`, "item/"} {
		if got := stalledRef("fixer", invalid); !strings.HasPrefix(got, "refs/forest/stalled-opaque/fixer/") {
			t.Errorf("invalid Subject %q produced raw stalled ref %q", invalid, got)
		}
	}
	if a, b := stalledRef("fixer", "item?x"), stalledRef("fixer", "item*x"); a == b {
		t.Fatalf("distinct opaque Subjects share stalled ref %q", a)
	}
	for range stalledRunLimit {
		if err := recordStalled(repo, "fixer", subject, revision); err != nil {
			t.Fatalf("record stalled: %v", err)
		}
	}
	if err := appendRun(workspaceDir(repo), runRecord{
		Flow: "fixer", Subject: subject, Revision: revision, Status: "gate_failed",
	}); err != nil {
		t.Fatalf("append ledger: %v", err)
	}
	if stalled, err := stalledOn(repo, "fixer", subject, revision); err != nil || !stalled {
		t.Fatalf("same revision stalled = %v, %v; want true", stalled, err)
	}
	if stalled, err := stalledOn(repo, "verifier", subject, revision); err != nil || stalled {
		t.Fatalf("other flow stalled = %v, %v; want false", stalled, err)
	}
	if stalled, err := stalledOn(repo, "fixer", subject, "bbb"); err != nil || stalled {
		t.Fatalf("changed revision stalled = %v, %v; want false", stalled, err)
	}
	if err := os.Remove(ledgerPath(repo)); err != nil {
		t.Fatalf("remove ledger: %v", err)
	}
	if stalled, err := stalledOn(repo, "fixer", subject, revision); err != nil || !stalled {
		t.Fatalf("stalled after ledger deletion = %v, %v; want true", stalled, err)
	}
}

// TestSelectOffersNoBranchItCannotMerge defines the hot loop out of existence.
// A branch that is approved and green but cannot land is not a Verifier subject:
// it is a state an operator reads. When Select offered it anyway, every pass
// rebased, rechecked and reviewed the same head and changed nothing, and the
// lane re-selected immediately because the pass had "succeeded". That produced
// 217 identical ledger rows and 217 build/vet/test runs on one branch.
func TestSelectOffersNoBranchItCannotMerge(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-change"
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	runGitTest(t, repo, "add", "branch.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "branch work")
	runGitTest(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	head := remoteBranchHead(t, repo, branch)
	runGitTest(t, repo, "checkout", "-q", "master")

	if err := writeVerdict(repo, head, verdictNote{
		Verdict: "approve", Reviewer: "verifier", Model: "m", DefSHA: strings.Repeat("a", 16), RunID: "seed",
	}); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}
	if err := writeChecks(repo, head, checksNote{Status: "pass", RunID: "seed", Time: nowRFC()}); err != nil {
		t.Fatalf("seed checks: %v", err)
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{}

	cfg.Flows.Verifier.AutoMerge = true
	withMerge, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatalf("Select with auto_merge on: %v", err)
	}
	if len(withMerge) != 1 {
		t.Fatalf("auto_merge on: got %d subjects, want the mergeable branch", len(withMerge))
	}

	cfg.Flows.Verifier.AutoMerge = false
	held, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatalf("Select with auto_merge off: %v", err)
	}
	if len(held) != 0 {
		t.Fatalf("auto_merge off: got %d subjects, want none; a branch that cannot land is not work", len(held))
	}
}

// TestMergeBlockedNamesEveryReason pins the single authority for merge policy.
// Select and Act both consult it; a precondition that lives in only one of them
// is how the two drifted and produced the hot loop above.
func TestMergeBlockedNamesEveryReason(t *testing.T) {
	cfg := defaultConfig()
	cfg.Flows.Verifier.AutoMerge = true
	cfg.Flows.Fixer.Attempts = 3

	if why := mergeBlocked(cfg, 0); why != "" {
		t.Errorf("an approved green branch under auto_merge is blocked: %q", why)
	}
	if why := mergeBlocked(cfg, 3); why == "" {
		t.Error("an exhausted branch is not blocked")
	}
	cfg.Flows.Verifier.AutoMerge = false
	if why := mergeBlocked(cfg, 0); why == "" {
		t.Error("auto_merge off does not block the merge")
	}
}
