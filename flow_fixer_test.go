package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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
	if err := writeChecks(repo, head, checksNote{Status: "fail"}); err != nil {
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
			if !errors.Is(actErr, errRetirementEvidenceInvalid) || out.Status != "notes_failed" {
				t.Fatalf("malformed %s Act = (%#v, %v), want hard notes failure", tc.name, out, actErr)
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
