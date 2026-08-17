package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func addValidReviewAndChecks(t *testing.T, root, sha, name, email string) {
	t.Helper()
	pushEvidence(t, root, "request", sha, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-fixed","revision":"`+sha+`","time":"2026-08-10T00:00:00Z"}`+"\n", name, email)
	pushEvidence(t, root, "checks", sha, `{"schema":"forest.checks.v1","revision":"`+sha+`","results":[{"name":"test","ok":true,"exit":0}],"time":"2026-08-10T00:00:00Z"}`+"\n", "Iron Forest Verifier", "verifier@forest.invalid")
}

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

// assertPersistedViolationAudit runs the live audit twice and requires a
// persisted non-pass AuditResult with exactly the wanted violation fragments,
// a nil operational error, snapshot cleanup, an unchanged violation set, and
// one deduplicated history entry per fragment.
func assertPersistedViolationAudit(t *testing.T, root string, fragments []string) {
	t.Helper()
	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatalf("violation Audit error=%v", err)
	}
	if len(result.Violations) != len(fragments) {
		t.Fatalf("violations=%v, want one per fragment %v", result.Violations, fragments)
	}
	for _, fragment := range fragments {
		if !containsViolation(result.Violations, fragment) {
			t.Fatalf("violations=%v missing %q", result.Violations, fragment)
		}
	}
	state, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastResult != "violations" || !sameViolationSet(state.Violations, result.Violations) {
		t.Fatalf("persisted audit state=%#v", state)
	}
	if refs := auditPrivateRefs(t, root); len(refs) != 0 {
		t.Fatalf("violation Audit left private refs: %v", refs)
	}
	again, err := audit(context.Background(), root)
	if err != nil {
		t.Fatalf("second violation Audit error=%v", err)
	}
	if !sameViolationSet(result.Violations, again.Violations) {
		t.Fatalf("violation set changed across Audits: %v then %v", result.Violations, again.Violations)
	}
	history, err := os.ReadFile(auditLogPath(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range fragments {
		if count := strings.Count(string(history), fragment); count != 1 {
			t.Fatalf("history entries=%d for %q want 1\n%s", count, fragment, history)
		}
	}
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

func installAuditGitWrapper(t *testing.T, script string) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wrapperDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AUDIT_REAL_GIT", gitPath)
}

func waitForAuditSignal(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func auditPrivateRefs(t *testing.T, root string) []string {
	t.Helper()
	output := runGitDir(t, root, "for-each-ref", "--format=%(refname)", auditorNotesNamespace+"/", auditorMasterNamespace+"/")
	return strings.Fields(string(output))
}

func auditPrivateOwner(t *testing.T, ref string) string {
	t.Helper()
	var rest string
	switch {
	case strings.HasPrefix(ref, auditorNotesNamespace+"/"):
		rest = strings.TrimPrefix(ref, auditorNotesNamespace+"/")
	case strings.HasPrefix(ref, auditorMasterNamespace+"/"):
		rest = strings.TrimPrefix(ref, auditorMasterNamespace+"/")
	default:
		t.Fatalf("not a private Audit ref: %s", ref)
	}
	owner, _, ok := strings.Cut(rest, "/")
	if !ok || owner == "" {
		t.Fatalf("malformed private Audit ref: %s", ref)
	}
	return owner
}
