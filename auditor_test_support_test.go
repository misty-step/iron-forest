package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func addRemoteNote(t *testing.T, root, ref, sha, payload, name, email string) {
	t.Helper()
	addNote(t, root, ref, sha, payload, name, email)
	runGitDir(t, root, "push", "origin", ref+":"+ref)
}

func containsViolation(values []string, fragment string) bool {
	return slices.ContainsFunc(values, func(value string) bool {
		return strings.Contains(value, fragment)
	})
}

func newAdvancedAuditFixture(t *testing.T, config string) (string, string) {
	t.Helper()
	root, _ := testClone(t)
	if _, err := audit(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if config != "" {
		if err := os.WriteFile(filepath.Join(root, "forest.yaml"), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "advance"), []byte("advance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if config != "" {
		runGitDir(t, root, "add", "advance", "forest.yaml")
	} else {
		runGitDir(t, root, "add", "advance")
	}
	runGitDir(t, root, "commit", "-m", "advance")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/master")
	return root, strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
}

func addGateNotes(t *testing.T, root, sha, results string) {
	t.Helper()
	pushEvidence(t, root, "request", sha, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+sha+`","time":"2026-08-10T00:00:00Z"}`+"\n", "Iron Forest Builder", "builder@forest.invalid")
	addVerifierGateNotes(t, root, sha, results)
}

func addVerifierGateNotes(t *testing.T, root, sha, results string) {
	t.Helper()
	pushEvidence(t, root, "checks", sha, `{"schema":"forest.checks.v1","revision":"`+sha+`","results":`+results+`,"time":"2026-08-10T00:00:00Z"}`+"\n", "Iron Forest Verifier", "verifier@forest.invalid")
	pushEvidence(t, root, "verdict", sha, `{"schema":"forest.verdict.v1","revision":"`+sha+`","verdict":"approve","summary":"ok","time":"2026-08-10T00:00:00Z"}`+"\n", "Iron Forest Verifier", "verifier@forest.invalid")
}
