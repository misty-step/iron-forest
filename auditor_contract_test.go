package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAuditorRejectsMissingAndMalformedNotes(t *testing.T) {
	t.Run("missing verdict", func(t *testing.T) {
		root, sha := newAdvancedAuditFixture(t, "")
		addValidReviewAndChecks(t, root, sha, "Iron Forest Builder", "builder@forest.invalid")
		result, err := Audit(root)
		if err != nil {
			t.Fatal(err)
		}
		if !containsViolation(result.Violations, "no approve verdict") {
			t.Fatalf("violations=%v", result.Violations)
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		root, _ := testClone(t)
		sha := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
		addRemoteNote(t, root, "refs/notes/forest/review-request", sha, "garbage", "Iron Forest Builder", "builder@forest.invalid")
		result, err := Audit(root)
		if err != nil {
			t.Fatal(err)
		}
		if !containsViolation(result.Violations, "malformed JSON") {
			t.Fatalf("violations=%v", result.Violations)
		}
	})
	t.Run("fixer repaired review request", func(t *testing.T) {
		root, _ := testClone(t)
		sha := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
		addValidReviewAndChecks(t, root, sha, "Iron Forest Fixer", "fixer@forest.invalid")
		addRemoteNote(t, root, "refs/notes/forest/verdict", sha, `{"schema":"forest.verdict.v1","revision":"`+sha+`","verdict":"approve","summary":"fixed","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")
		result, err := Audit(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Violations) != 0 {
			t.Fatalf("violations=%v", result.Violations)
		}
	})
}

func TestAuditorRejectsEveryWrongWriterWithoutAdvancingLastMaster(t *testing.T) {
	cases := []struct {
		name string
		ref  string
	}{
		{name: "review-request", ref: "refs/notes/forest/review-request"},
		{name: "checks", ref: "refs/notes/forest/checks"},
		{name: "verdict", ref: "refs/notes/forest/verdict"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root, sha := newAdvancedAuditFixture(t, "")
			before, err := readAuditState(root)
			if err != nil {
				t.Fatal(err)
			}
			evidence := []struct {
				ref     string
				payload string
				name    string
				email   string
			}{
				{
					ref:     "refs/notes/forest/review-request",
					payload: `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`,
					name:    "Iron Forest Builder",
					email:   "builder@forest.invalid",
				},
				{
					ref:     "refs/notes/forest/checks",
					payload: `{"schema":"forest.checks.v1","revision":"` + sha + `","results":[{"name":"test","ok":true,"exit":0}],"time":"2026-08-10T00:00:00Z"}`,
					name:    "Iron Forest Verifier",
					email:   "verifier@forest.invalid",
				},
				{
					ref:     "refs/notes/forest/verdict",
					payload: `{"schema":"forest.verdict.v1","revision":"` + sha + `","verdict":"approve","summary":"ok","time":"2026-08-10T00:00:00Z"}`,
					name:    "Iron Forest Verifier",
					email:   "verifier@forest.invalid",
				},
			}
			for _, entry := range evidence {
				if entry.ref == testCase.ref {
					entry.name = "Unexpected"
					entry.email = "unexpected@forest.invalid"
				}
				addRemoteNote(t, root, entry.ref, sha, entry.payload, entry.name, entry.email)
			}

			result, err := Audit(root)
			if err != nil {
				t.Fatal(err)
			}
			if !containsViolation(result.Violations, "wrong author identity on "+testCase.name) {
				t.Fatalf("wrong %s writer passed: %v", testCase.name, result.Violations)
			}
			after, err := readAuditState(root)
			if err != nil {
				t.Fatal(err)
			}
			if after.LastMaster != before.LastMaster {
				t.Fatalf("wrong %s writer advanced last-good from %s to %s", testCase.name, before.LastMaster, after.LastMaster)
			}
		})
	}
}

func TestAuditorRejectsPaddedNoteActorsAtAuditEntrypoint(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		suffix string
	}{
		{name: "leading author space", prefix: " "},
		{name: "trailing email space", suffix: " "},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, _ := testClone(t)
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			addRemoteNote(t, root, "refs/notes/forest/review-request", revision, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+revision+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")

			previous := gitRun
			defer func() { gitRun = previous }()
			gitRun = func(ctx context.Context, root string, args ...string) ([]byte, error) {
				output, err := previous(ctx, root, args...)
				if err != nil || len(args) == 0 || args[0] != "log" {
					return output, err
				}
				if len(output) == 0 || output[len(output)-1] != '\n' {
					t.Fatalf("note actor output has no terminal LF: %q", output)
				}
				padded := make([]byte, 0, len(test.prefix)+len(output)+len(test.suffix))
				padded = append(padded, test.prefix...)
				padded = append(padded, output[:len(output)-1]...)
				padded = append(padded, test.suffix...)
				padded = append(padded, '\n')
				return padded, nil
			}

			result, err := Audit(root)
			if err != nil {
				t.Fatal(err)
			}
			if !containsViolation(result.Violations, "wrong author identity on review-request") {
				t.Fatalf("padded note actor passed Audit: %#v", result)
			}
		})
	}
}

func TestAuditorRejectsChecksAndApproveWithoutReviewRequest(t *testing.T) {
	root, sha := newAdvancedAuditFixture(t, "")
	state, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	addVerifierGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)
	result, err := Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	if !containsViolation(result.Violations, "exactly one valid review-request note") {
		t.Fatalf("checks and approve passed without a review request: %v", result.Violations)
	}
	current, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if current.LastMaster != state.LastMaster {
		t.Fatalf("master without a review request became last-good: before=%#v after=%#v", state, current)
	}
}

func TestAuditorRequiresExactlyOneValidReviewRequest(t *testing.T) {
	master := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	validPayload := `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"` + master + `","time":"2026-08-10T00:00:00Z"}`
	validRequest := noteEntry{
		Ref:      "refs/notes/forest/review-request",
		Revision: master,
		Payload:  []byte(validPayload),
		Author:   "Iron Forest Builder",
		Email:    "builder@forest.invalid",
	}
	fixerRequest := validRequest
	fixerRequest.Author = "Iron Forest Fixer"
	fixerRequest.Email = "fixer@forest.invalid"
	verifierEvidence := []noteEntry{
		{
			Ref:      "refs/notes/forest/checks",
			Revision: master,
			Payload:  []byte(`{"schema":"forest.checks.v1","revision":"` + master + `","results":[{"name":"test","ok":true,"exit":0}],"time":"2026-08-10T00:00:00Z"}`),
			Author:   "Iron Forest Verifier",
			Email:    "verifier@forest.invalid",
		},
		{
			Ref:      "refs/notes/forest/verdict",
			Revision: master,
			Payload:  []byte(`{"schema":"forest.verdict.v1","revision":"` + master + `","verdict":"approve","summary":"ok","time":"2026-08-10T00:00:00Z"}`),
			Author:   "Iron Forest Verifier",
			Email:    "verifier@forest.invalid",
		},
	}
	malformed := validRequest
	malformed.Payload = []byte("garbage")
	wrongTarget := validRequest
	wrongTarget.Revision = other
	wrongTarget.Payload = []byte(`{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"` + other + `","time":"2026-08-10T00:00:00Z"}`)
	wrongIdentity := validRequest
	wrongIdentity.Author = "Unexpected"
	wrongIdentity.Email = "unexpected@forest.invalid"
	wrongBranch := validRequest
	wrongBranch.Payload = []byte(`{"schema":"forest.review-request.v1","issue":1,"branch":"forest/2-wrong","revision":"` + master + `","time":"2026-08-10T00:00:00Z"}`)
	cases := []struct {
		name     string
		requests []noteEntry
		valid    bool
	}{
		{name: "missing"},
		{name: "malformed", requests: []noteEntry{malformed}},
		{name: "wrong target", requests: []noteEntry{wrongTarget}},
		{name: "wrong identity", requests: []noteEntry{wrongIdentity}},
		{name: "wrong branch", requests: []noteEntry{wrongBranch}},
		{name: "duplicate", requests: []noteEntry{validRequest, fixerRequest}},
		{name: "duplicate with malformed", requests: []noteEntry{validRequest, malformed}},
		{name: "single fixer", requests: []noteEntry{fixerRequest}, valid: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			entries := append([]noteEntry(nil), testCase.requests...)
			entries = append(entries, verifierEvidence...)
			err := verifyGate(entries, master, Config{Checks: []Check{{Name: "test"}}})
			if testCase.valid && err != nil {
				t.Fatalf("verifyGate rejected valid Fixer request: %v", err)
			}
			if !testCase.valid && (err == nil || !strings.Contains(err.Error(), "exactly one valid review-request note")) {
				t.Fatalf("verifyGate error=%v", err)
			}
		})
	}
}

func TestAuditorBindsNoteIdentityToExactTargetPath(t *testing.T) {
	root, _ := testClone(t)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	other := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD^{tree}")))
	snapshot, err := newAuditSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	canonical := "refs/notes/forest/review-request"
	ref := auditorNoteRef(snapshot, canonical)
	payload := `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"` + revision + `","time":"2026-08-10T00:00:00Z"}`
	addNote(t, root, ref, revision, payload, "Unexpected", "unexpected@forest.invalid")
	addNote(t, root, ref, other, payload, "Iron Forest Builder", "builder@forest.invalid")
	snapshot.Notes[canonical] = strings.TrimSpace(string(runGitDir(t, root, "rev-parse", ref)))

	entries, err := readNotes(context.Background(), root, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	identities := make(map[string]string)
	for _, entry := range entries {
		identities[entry.Revision] = entry.Author + "\x00" + entry.Email
	}
	if got := identities[revision]; got != "Unexpected\x00unexpected@forest.invalid" {
		t.Fatalf("review target identity=%q", got)
	}
	if got := identities[other]; got != "Iron Forest Builder\x00builder@forest.invalid" {
		t.Fatalf("other target identity=%q", got)
	}
}

func TestAuditorRejectsDuplicateNoteTreePaths(t *testing.T) {
	root, _ := testClone(t)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	payloadPath := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(payloadPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blob := strings.TrimSpace(string(runGitDir(t, root, "hash-object", "-w", payloadPath)))
	index := filepath.Join(t.TempDir(), "index")
	runIndexGit := func(args ...string) []byte {
		commandArgs := append([]string{"-C", root}, args...)
		cmd := exec.Command("git", commandArgs...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return output
	}
	runIndexGit("read-tree", "--empty")
	runIndexGit("update-index", "--add", "--cacheinfo", "100644,"+blob+","+revision)
	fanoutPath := revision[:2] + "/" + revision[2:]
	runIndexGit("update-index", "--add", "--cacheinfo", "100644,"+blob+","+fanoutPath)
	tree := strings.TrimSpace(string(runIndexGit("write-tree")))
	commit := strings.TrimSpace(string(runGitDir(t, root, "commit-tree", tree, "-m", "crafted duplicate note paths")))
	snapshot, err := newAuditSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	ref := auditorNoteRef(snapshot, "refs/notes/forest/review-request")
	runGitDir(t, root, "update-ref", ref, commit)

	if _, err := notePaths(context.Background(), root, ref); err == nil || !strings.Contains(err.Error(), "duplicate note paths for "+revision) {
		t.Fatalf("duplicate note paths error=%v", err)
	}
}

func addValidReviewAndChecks(t *testing.T, root, sha, name, email string) {
	t.Helper()
	addRemoteNote(t, root, "refs/notes/forest/review-request", sha, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-fixed","revision":"`+sha+`","time":"2026-08-10T00:00:00Z"}`, name, email)
	addRemoteNote(t, root, "refs/notes/forest/checks", sha, `{"schema":"forest.checks.v1","revision":"`+sha+`","results":[{"name":"test","ok":true,"exit":0}],"time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")
}

func addRemoteNote(t *testing.T, root, ref, sha, payload, name, email string) {
	t.Helper()
	addNote(t, root, ref, sha, payload, name, email)
	runGitDir(t, root, "push", "origin", ref+":"+ref)
}

func containsViolation(values []string, fragment string) bool {
	for _, value := range values {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}

func TestAuditorIgnoresLocalOnlyWorkflowNotes(t *testing.T) {
	root, sha := newAdvancedAuditFixture(t, "")
	addNote(t, root, "refs/notes/forest/review-request", sha, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-local","revision":"`+sha+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
	addNote(t, root, "refs/notes/forest/checks", sha, `{"schema":"forest.checks.v1","revision":"`+sha+`","results":[{"name":"test","ok":true,"exit":0}],"time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")
	addNote(t, root, "refs/notes/forest/verdict", sha, `{"schema":"forest.verdict.v1","revision":"`+sha+`","verdict":"approve","summary":"local only","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")
	result, err := Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	if !containsViolation(result.Violations, "exactly one valid review-request note") {
		t.Fatalf("local-only notes affected audit: %v", result.Violations)
	}
}

func TestAuditorRejectsChecksSetMismatch(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		results string
		want    string
	}{
		{name: "empty configured", config: "repo: owner/name\nagents:\n  builder: {poll: forest poll builder, interval: 1, timeout: 1}\nchecks: []\n", results: `[{"name":"test","ok":true,"exit":0}]`, want: "no configured checks"},
		{name: "empty reported", config: "", results: `[]`, want: "invalid checks note"},
		{name: "partial", config: "repo: owner/name\nagents:\n  builder: {poll: forest poll builder, interval: 1, timeout: 1}\nchecks:\n  - {name: test, run: test}\n  - {name: vet, run: vet}\n", results: `[{"name":"test","ok":true,"exit":0}]`, want: "does not match configured checks"},
		{name: "extra", config: "", results: `[{"name":"test","ok":true,"exit":0},{"name":"vet","ok":true,"exit":0}]`, want: "does not match configured checks"},
		{name: "order mismatch", config: "repo: owner/name\nagents:\n  builder: {poll: forest poll builder, interval: 1, timeout: 1}\nchecks:\n  - {name: test, run: test}\n  - {name: vet, run: vet}\n", results: `[{"name":"vet","ok":true,"exit":0},{"name":"test","ok":true,"exit":0}]`, want: "does not match configured checks"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root, sha := newAdvancedAuditFixture(t, testCase.config)
			addGateNotes(t, root, sha, testCase.results)
			result, err := Audit(root)
			if err != nil {
				t.Fatal(err)
			}
			if !containsViolation(result.Violations, testCase.want) {
				t.Fatalf("violations=%v", result.Violations)
			}
		})
	}
}

func TestNoteDecodersRejectMalformedTime(t *testing.T) {
	sha := strings.Repeat("a", 40)
	review := `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-time","revision":"` + sha + `","time":"not-a-time"}`
	if _, err := decodeReview([]byte(review), sha); err == nil {
		t.Fatal("decodeReview accepted malformed time")
	}
	checks := `{"schema":"forest.checks.v1","revision":"` + sha + `","results":[{"name":"test","ok":true,"exit":0}],"time":"not-a-time"}`
	if _, err := decodeChecks([]byte(checks), sha); err == nil {
		t.Fatal("decodeChecks accepted malformed time")
	}
	verdict := `{"schema":"forest.verdict.v1","revision":"` + sha + `","verdict":"approve","summary":"ok","time":"not-a-time"}`
	if _, err := decodeVerdict([]byte(verdict), sha); err == nil {
		t.Fatal("decodeVerdict accepted malformed time")
	}
	blankSummary := `{"schema":"forest.verdict.v1","revision":"` + sha + `","verdict":"approve","summary":"  ","time":"2026-08-10T00:00:00Z"}`
	if _, err := decodeVerdict([]byte(blankSummary), sha); err == nil {
		t.Fatal("decodeVerdict accepted blank summary")
	}
}

func TestAuditorAcceptsAdvancedFastForwardGate(t *testing.T) {
	root, sha := newAdvancedAuditFixture(t, "")
	addGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)
	result, err := Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Advanced || len(result.Violations) != 0 {
		t.Fatalf("advanced gate result=%#v", result)
	}
}

func newAdvancedAuditFixture(t *testing.T, config string) (string, string) {
	t.Helper()
	root, _ := testClone(t)
	if _, err := Audit(root); err != nil {
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
	addRemoteNote(t, root, "refs/notes/forest/review-request", sha, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+sha+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
	addVerifierGateNotes(t, root, sha, results)
}

func addVerifierGateNotes(t *testing.T, root, sha, results string) {
	t.Helper()
	addRemoteNote(t, root, "refs/notes/forest/checks", sha, `{"schema":"forest.checks.v1","revision":"`+sha+`","results":`+results+`,"time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")
	addRemoteNote(t, root, "refs/notes/forest/verdict", sha, `{"schema":"forest.verdict.v1","revision":"`+sha+`","verdict":"approve","summary":"ok","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")
}

func TestAuditorStableSnapshotRetriesOnRemoteChange(t *testing.T) {
	root, origin := testClone(t)
	if _, err := Audit(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "first-change"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "first-change")
	runGitDir(t, root, "commit", "-m", "first change")
	first := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/staged-first")
	if err := os.WriteFile(filepath.Join(root, "second-change"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "second-change")
	runGitDir(t, root, "commit", "-m", "second change")
	second := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/staged-second")
	addGateNotes(t, root, second, `[{"name":"test","ok":true,"exit":0}]`)

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	wrapperPath := filepath.Join(wrapperDir, "git")
	callCount := filepath.Join(t.TempDir(), "calls")
	wrapper := `#!/bin/sh
set -eu
root=""
if [ "$1" = "-C" ]; then
  root=$2
  shift 2
fi
if [ "$1" != "ls-remote" ]; then
  exec "$AUDIT_REAL_GIT" -C "$root" "$@"
fi
count=0
if [ -e "$AUDIT_WRAP_COUNT" ]; then
  count=$(cat "$AUDIT_WRAP_COUNT")
fi
count=$((count + 1))
printf '%s\n' "$count" > "$AUDIT_WRAP_COUNT"
if [ "$count" -eq 2 ]; then
  "$AUDIT_REAL_GIT" --git-dir "$AUDIT_ORIGIN" update-ref refs/heads/master "$AUDIT_FIRST_MASTER"
elif [ "$count" -eq 4 ]; then
  "$AUDIT_REAL_GIT" --git-dir "$AUDIT_ORIGIN" update-ref refs/heads/master "$AUDIT_SECOND_MASTER"
fi
exec "$AUDIT_REAL_GIT" -C "$root" "$@"
`
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AUDIT_REAL_GIT", gitPath)
	t.Setenv("AUDIT_ORIGIN", origin)
	t.Setenv("AUDIT_FIRST_MASTER", first)
	t.Setenv("AUDIT_SECOND_MASTER", second)
	t.Setenv("AUDIT_WRAP_COUNT", callCount)
	result, err := Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Master != second || len(result.Violations) != 0 {
		t.Fatalf("third-attempt snapshot result=%#v", result)
	}
	countData, err := os.ReadFile(callCount)
	if err != nil {
		t.Fatal(err)
	}
	if advertisements := strings.TrimSpace(string(countData)); advertisements != "6" {
		t.Fatalf("snapshot advertisements=%s, want 6 across three total attempts", advertisements)
	}
}

func TestAuditorStableSnapshotFailsAfterThreeTotalAttempts(t *testing.T) {
	root, origin := testClone(t)
	if _, err := Audit(root); err != nil {
		t.Fatal(err)
	}
	baseline := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	if err := os.WriteFile(filepath.Join(root, "race-change"), []byte("race\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "race-change")
	runGitDir(t, root, "commit", "-m", "race change")
	raced := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/race-staged")

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	wrapperPath := filepath.Join(wrapperDir, "git")
	callCount := filepath.Join(t.TempDir(), "calls")
	wrapper := `#!/bin/sh
set -eu
root=""
if [ "$1" = "-C" ]; then
  root=$2
  shift 2
fi
if [ "$1" != "ls-remote" ]; then
  exec "$AUDIT_REAL_GIT" -C "$root" "$@"
fi
count=0
if [ -e "$AUDIT_WRAP_COUNT" ]; then
  count=$(cat "$AUDIT_WRAP_COUNT")
fi
count=$((count + 1))
printf '%s\n' "$count" > "$AUDIT_WRAP_COUNT"
if [ $((count % 2)) -eq 0 ]; then
  current=$("$AUDIT_REAL_GIT" --git-dir "$AUDIT_ORIGIN" rev-parse refs/heads/master)
  next="$AUDIT_BASELINE"
  if [ "$current" = "$AUDIT_BASELINE" ]; then
    next="$AUDIT_RACED_MASTER"
  fi
  "$AUDIT_REAL_GIT" --git-dir "$AUDIT_ORIGIN" update-ref refs/heads/master "$next"
fi
exec "$AUDIT_REAL_GIT" -C "$root" "$@"
`
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AUDIT_REAL_GIT", gitPath)
	t.Setenv("AUDIT_ORIGIN", origin)
	t.Setenv("AUDIT_BASELINE", baseline)
	t.Setenv("AUDIT_RACED_MASTER", raced)
	t.Setenv("AUDIT_WRAP_COUNT", callCount)
	if _, err := Audit(root); err == nil || !strings.Contains(err.Error(), "remote snapshot changed during audit") {
		t.Fatalf("perpetual snapshot race error=%v", err)
	}
	countData, err := os.ReadFile(callCount)
	if err != nil {
		t.Fatal(err)
	}
	if advertisements := strings.TrimSpace(string(countData)); advertisements != "6" {
		t.Fatalf("snapshot advertisements=%s, want 6 across three total attempts", advertisements)
	}
}

func TestAuditRejectsFetchedPrivateRefABA(t *testing.T) {
	root, _ := testClone(t)
	if _, err := Audit(root); err != nil {
		t.Fatal(err)
	}
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	ref := "refs/notes/forest/review-request"
	addRemoteNote(t, root, ref, revision, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+revision+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
	advertised := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", ref)))
	other := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD^{tree}")))
	addNote(t, root, ref, other, `{"schema":"forest.review-request.v1","issue":2,"branch":"forest/2-gate","revision":"`+other+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
	wrong := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", ref)))
	if wrong == advertised {
		t.Fatal("crafted private note ref did not change")
	}
	before, err := os.ReadFile(auditStatePath(root))
	if err != nil {
		t.Fatal(err)
	}

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	wrapperPath := filepath.Join(wrapperDir, "git")
	callCount := filepath.Join(t.TempDir(), "calls")
	wrapper := `#!/bin/sh
set -eu
root=""
if [ "$1" = "-C" ]; then
  root=$2
  shift 2
fi
if [ "$1" = "fetch" ]; then
  for arg in "$@"; do
    case "$arg" in
      "$AUDIT_SOURCE_REF":*)
        private_ref=${arg#*:}
        "$AUDIT_REAL_GIT" -C "$root" "$@"
        "$AUDIT_REAL_GIT" -C "$root" update-ref "$private_ref" "$AUDIT_WRONG_OID"
        count=0
        if [ -e "$AUDIT_WRAP_COUNT" ]; then
          count=$(cat "$AUDIT_WRAP_COUNT")
        fi
        printf '%s\n' $((count + 1)) > "$AUDIT_WRAP_COUNT"
        exit 0
        ;;
    esac
  done
fi
exec "$AUDIT_REAL_GIT" -C "$root" "$@"
`
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AUDIT_REAL_GIT", gitPath)
	t.Setenv("AUDIT_SOURCE_REF", ref)
	t.Setenv("AUDIT_WRONG_OID", wrong)
	t.Setenv("AUDIT_WRAP_COUNT", callCount)

	if _, err := Audit(root); err == nil || !strings.Contains(err.Error(), "does not match advertised object") {
		t.Fatalf("fetched private ref ABA error=%v", err)
	}
	countData, err := os.ReadFile(callCount)
	if err != nil {
		t.Fatal(err)
	}
	if attempts := strings.TrimSpace(string(countData)); attempts != "3" {
		t.Fatalf("wrong private ref fetch attempts=%s, want 3", attempts)
	}
	after, err := os.ReadFile(auditStatePath(root))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("failed fetched-OID audit changed state\nbefore=%s\nafter=%s", before, after)
	}
}

func TestAuditorBaselineAndLastGoodRevision(t *testing.T) {
	root, _ := testClone(t)
	baseline := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	result, err := Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Violations) != 0 {
		t.Fatalf("baseline violations=%v", result.Violations)
	}
	state, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Baseline != baseline || state.LastMaster != baseline {
		t.Fatalf("baseline state=%#v", state)
	}
	if state.LastResult != "pass" || len(state.Violations) != 0 {
		t.Fatalf("baseline status=%#v", state)
	}
	if state.LastAt == "" {
		t.Fatal("baseline LastAt is empty")
	}
	if _, err := time.Parse(time.RFC3339Nano, state.LastAt); err != nil {
		t.Fatalf("baseline LastAt=%q: %v", state.LastAt, err)
	}
	passedAt := state.LastAt
	if err := os.WriteFile(filepath.Join(root, "later"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "later")
	runGitDir(t, root, "commit", "-m", "later")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/master")
	result, err = Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	if !containsViolation(result.Violations, "exactly one valid review-request note") {
		t.Fatalf("later violations=%v", result.Violations)
	}
	state, err = readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Baseline != baseline || state.LastMaster != baseline {
		t.Fatalf("unsafe revision became last-good: %#v", state)
	}
	if state.LastResult != "violations" || !containsViolation(state.Violations, "exactly one valid review-request note") {
		t.Fatalf("unsafe revision status=%#v", state)
	}
	if state.LastAt == "" {
		t.Fatal("violation LastAt is empty")
	}
	if _, err := time.Parse(time.RFC3339Nano, state.LastAt); err != nil {
		t.Fatalf("violation LastAt=%q: %v", state.LastAt, err)
	}
	if state.LastAt == passedAt {
		t.Fatalf("violation did not update LastAt from %q", passedAt)
	}
}

func TestAuditorBaselineViolationFencesForceReplacedTip(t *testing.T) {
	root, _ := testClone(t)
	baseline := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	addRemoteNote(t, root, "refs/notes/forest/review-request", baseline, "garbage", "Iron Forest Builder", "builder@forest.invalid")
	result, err := Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	if !containsViolation(result.Violations, "malformed JSON") {
		t.Fatalf("baseline violations=%v", result.Violations)
	}
	state, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Baseline != baseline || state.LastMaster != "" {
		t.Fatalf("baseline state=%#v", state)
	}

	runGitDir(t, root, "checkout", "--orphan", "rewritten")
	runGitDir(t, root, "rm", "-rf", ".")
	config := []byte(`repo: owner/name
agents:
  builder: {poll: "forest poll builder", interval: 1, timeout: 1}
checks:
  - {name: test, run: "go test ./..."}
`)
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unrelated"), []byte("unrelated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "forest.yaml", "unrelated")
	runGitDir(t, root, "commit", "-m", "unrelated tip")
	unrelated := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))

	runGitDir(t, root, "notes", "--ref=refs/notes/forest/review-request", "remove", baseline)
	addNote(t, root, "refs/notes/forest/review-request", unrelated, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-unrelated","revision":"`+unrelated+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
	addNote(t, root, "refs/notes/forest/checks", unrelated, `{"schema":"forest.checks.v1","revision":"`+unrelated+`","results":[{"name":"test","ok":true,"exit":0}],"time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")
	addNote(t, root, "refs/notes/forest/verdict", unrelated, `{"schema":"forest.verdict.v1","revision":"`+unrelated+`","verdict":"approve","summary":"ok","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")
	for _, ref := range []string{"review-request", "checks", "verdict"} {
		runGitDir(t, root, "push", "--force", "origin", "refs/notes/forest/"+ref+":refs/notes/forest/"+ref)
	}
	runGitDir(t, root, "push", "--force", "origin", "HEAD:refs/heads/master")

	for attempt := range 2 {
		result, err = Audit(root)
		if err != nil {
			t.Fatal(err)
		}
		if !containsViolation(result.Violations, "non-fast-forward") {
			t.Fatalf("attempt %d violations=%v", attempt+1, result.Violations)
		}
		state, err = readAuditState(root)
		if err != nil {
			t.Fatal(err)
		}
		if state.Baseline != baseline || state.LastMaster != "" {
			t.Fatalf("attempt %d state=%#v", attempt+1, state)
		}
	}
}

func TestAuditorRevalidatesUnchangedNonBaselineAndClearsCurrentViolations(t *testing.T) {
	root, sha := newAdvancedAuditFixture(t, "")
	addGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)
	if result, err := Audit(root); err != nil || len(result.Violations) != 0 {
		t.Fatalf("initial passing audit result=%#v err=%v", result, err)
	}
	runGitDir(t, root, "notes", "--ref=refs/notes/forest/checks", "remove", sha)
	runGitDir(t, root, "push", "--force", "origin", "refs/notes/forest/checks:refs/notes/forest/checks")
	result, err := Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Advanced || !containsViolation(result.Violations, "no passing checks") {
		t.Fatalf("unchanged evidence loss result=%#v", result)
	}
	state, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastMaster != sha || len(state.Violations) == 0 {
		t.Fatalf("violation state=%#v", state)
	}
	if state.LastResult != "violations" || !containsViolation(state.Violations, "no passing checks") {
		t.Fatalf("violation status=%#v", state)
	}
	if state.LastAt == "" {
		t.Fatal("violation LastAt is empty")
	}
	if _, err := time.Parse(time.RFC3339Nano, state.LastAt); err != nil {
		t.Fatalf("violation LastAt=%q: %v", state.LastAt, err)
	}
	violatingAt := state.LastAt
	checksPayload := `{"schema":"forest.checks.v1","revision":"` + sha + `","results":[{"name":"test","ok":true,"exit":0}],"time":"2026-08-10T00:00:00Z"}`
	addRemoteNote(t, root, "refs/notes/forest/checks", sha, checksPayload, "Iron Forest Verifier", "verifier@forest.invalid")
	result, err = Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Violations) != 0 {
		t.Fatalf("cleared violations=%v", result.Violations)
	}
	persisted, err := os.ReadFile(auditStatePath(root))
	if err != nil {
		t.Fatal(err)
	}
	var current AuditState
	if err := json.Unmarshal(persisted, &current); err != nil {
		t.Fatal(err)
	}
	if len(current.Violations) != 0 {
		t.Fatalf("stale current violations=%v", current.Violations)
	}
	if current.LastResult != "pass" {
		t.Fatalf("cleared status=%#v", current)
	}
	if current.LastAt == "" {
		t.Fatal("cleared LastAt is empty")
	}
	if _, err := time.Parse(time.RFC3339Nano, current.LastAt); err != nil {
		t.Fatalf("cleared LastAt=%q: %v", current.LastAt, err)
	}
	if current.LastAt == violatingAt {
		t.Fatalf("passing audit did not update LastAt from %q", violatingAt)
	}
	logData, err := os.ReadFile(auditLogPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "no passing checks") {
		t.Fatal("audit history did not retain prior violation")
	}
}

func TestAuditorRetriesWhenChecksNoteChangesDuringSnapshot(t *testing.T) {
	root, sha := newAdvancedAuditFixture(t, "")
	origin := strings.TrimSpace(string(runGitDir(t, root, "remote", "get-url", "origin")))
	addGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)

	mutator := filepath.Join(t.TempDir(), "mutator")
	runGit(t, "clone", origin, mutator)
	configGit(t, mutator, "Iron Forest Verifier", "verifier@forest.invalid")
	runGitDir(t, mutator, "fetch", "origin", "refs/notes/forest/checks:refs/notes/forest/checks")
	runGitDir(t, mutator, "notes", "--ref=refs/notes/forest/checks", "remove", sha)
	addNote(t, mutator, "refs/notes/forest/checks", sha, `{"schema":"forest.checks.v1","revision":"`+sha+`","results":[{"name":"test","ok":false,"exit":1}],"time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	wrapperPath := filepath.Join(wrapperDir, "git")
	callCount := filepath.Join(t.TempDir(), "calls")
	wrapper := `#!/bin/sh
set -eu
root=""
if [ "$1" = "-C" ]; then
  root=$2
  shift 2
fi
if [ "$1" != "ls-remote" ]; then
  exec "$AUDIT_REAL_GIT" -C "$root" "$@"
fi
count=0
if [ -e "$AUDIT_WRAP_COUNT" ]; then
  count=$(cat "$AUDIT_WRAP_COUNT")
fi
count=$((count + 1))
printf '%s\n' "$count" > "$AUDIT_WRAP_COUNT"
if [ "$count" -eq 2 ]; then
  "$AUDIT_REAL_GIT" -C "$AUDIT_MUTATOR" push --force origin refs/notes/forest/checks:refs/notes/forest/checks
fi
exec "$AUDIT_REAL_GIT" -C "$root" "$@"
`
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AUDIT_REAL_GIT", gitPath)
	t.Setenv("AUDIT_MUTATOR", mutator)
	t.Setenv("AUDIT_WRAP_COUNT", callCount)
	result, err := Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	if !containsViolation(result.Violations, "no passing checks") {
		t.Fatalf("changed checks note was not observed: %#v", result)
	}
	countData, err := os.ReadFile(callCount)
	if err != nil {
		t.Fatal(err)
	}
	advertisements, err := strconv.Atoi(strings.TrimSpace(string(countData)))
	if err != nil || advertisements < 4 {
		t.Fatalf("snapshot advertisements=%q, want retry after note change", strings.TrimSpace(string(countData)))
	}
}

func TestAuditorAnchorsAncestryAtLastGoodRevision(t *testing.T) {
	root, lastGood := newAdvancedAuditFixture(t, "")
	state, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	baseline := state.Baseline
	if baseline == "" || baseline == lastGood || state.LastMaster != baseline || len(state.Violations) != 0 {
		t.Fatalf("baseline fixture state=%#v", state)
	}

	addGateNotes(t, root, lastGood, `[{"name":"test","ok":true,"exit":0}]`)
	result, err := Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Violations) != 0 {
		t.Fatalf("last-good audit violations=%v", result.Violations)
	}
	state, err = readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Baseline != baseline || state.LastMaster != lastGood {
		t.Fatalf("last-good fixture state=%#v", state)
	}

	runGitDir(t, root, "checkout", "-b", "replacement", baseline)
	if err := os.WriteFile(filepath.Join(root, "replacement"), []byte("replacement\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "replacement")
	runGitDir(t, root, "commit", "-m", "replacement")
	replaced := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "merge-base", "--is-ancestor", baseline, replaced)
	if cmd := exec.Command("git", "-C", root, "merge-base", "--is-ancestor", lastGood, replaced); cmd.Run() == nil {
		t.Fatalf("replacement %s unexpectedly descends from last-good %s", replaced, lastGood)
	}

	addGateNotes(t, root, replaced, `[{"name":"test","ok":true,"exit":0}]`)
	runGitDir(t, root, "push", "--force", "origin", "HEAD:refs/heads/master")
	result, err = Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	want := "non-fast-forward from " + lastGood + " to " + replaced
	if !result.Advanced || len(result.Violations) != 1 || !containsViolation(result.Violations, want) {
		t.Fatalf("replacement audit result=%#v, want %s", result, want)
	}
	state, err = readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Baseline != baseline || state.LastMaster != lastGood {
		t.Fatalf("replacement changed ancestry anchor: %#v", state)
	}
}

func TestAuditorRequiresDurableStateAndHistory(t *testing.T) {
	t.Run("unique state temp", func(t *testing.T) {
		root, _ := testClone(t)
		if err := os.MkdirAll(filepath.Dir(auditStatePath(root)), 0o755); err != nil {
			t.Fatal(err)
		}
		sentinel := auditStatePath(root) + ".tmp"
		if err := os.WriteFile(sentinel, []byte("sentinel\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Audit(root); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(sentinel)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "sentinel\n" {
			t.Fatalf("fixed audit temp was overwritten: %q", data)
		}
	})

	t.Run("state file sync", func(t *testing.T) {
		root, sha := newAdvancedAuditFixture(t, "")
		addGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)
		before, err := os.ReadFile(auditStatePath(root))
		if err != nil {
			t.Fatal(err)
		}
		previous := syncAuditFile
		syncAuditFile = func(file *os.File) error {
			if strings.HasPrefix(filepath.Base(file.Name()), ".audit.json-") {
				return errors.New("injected state sync failure")
			}
			return file.Sync()
		}
		defer func() { syncAuditFile = previous }()

		if _, err := Audit(root); err == nil || !strings.Contains(err.Error(), "injected state sync failure") {
			t.Fatalf("state sync error=%v", err)
		}
		after, err := os.ReadFile(auditStatePath(root))
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatalf("failed state sync changed audit state\nbefore=%s\nafter=%s", before, after)
		}
	})

	t.Run("history sync", func(t *testing.T) {
		root, _ := newAdvancedAuditFixture(t, "")
		before, err := os.ReadFile(auditStatePath(root))
		if err != nil {
			t.Fatal(err)
		}
		previous := syncAuditFile
		syncAuditFile = func(file *os.File) error {
			if filepath.Base(file.Name()) == "audit.log" {
				return errors.New("injected history sync failure")
			}
			return file.Sync()
		}
		defer func() { syncAuditFile = previous }()

		if _, err := Audit(root); err == nil || !strings.Contains(err.Error(), "injected history sync failure") {
			t.Fatalf("history sync error=%v", err)
		}
		after, err := os.ReadFile(auditStatePath(root))
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatalf("failed history sync changed audit state\nbefore=%s\nafter=%s", before, after)
		}
	})

	t.Run("audit directory sync", func(t *testing.T) {
		root, sha := newAdvancedAuditFixture(t, "")
		addGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)
		previous := syncAuditFile
		syncAuditFile = func(file *os.File) error {
			if filepath.Clean(file.Name()) == filepath.Clean(filepath.Dir(auditStatePath(root))) {
				return errors.New("injected directory sync failure")
			}
			return file.Sync()
		}
		defer func() { syncAuditFile = previous }()

		if _, err := Audit(root); err == nil || !strings.Contains(err.Error(), "injected directory sync failure") {
			t.Fatalf("directory sync error=%v", err)
		}
	})
}

func TestAuditorChangesVerdictDoesNotApproveMaster(t *testing.T) {
	root, sha := newAdvancedAuditFixture(t, "")
	addRemoteNote(t, root, "refs/notes/forest/review-request", sha, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+sha+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
	addRemoteNote(t, root, "refs/notes/forest/checks", sha, `{"schema":"forest.checks.v1","revision":"`+sha+`","results":[{"name":"test","ok":true,"exit":0}],"time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")
	addRemoteNote(t, root, "refs/notes/forest/verdict", sha, `{"schema":"forest.verdict.v1","revision":"`+sha+`","verdict":"changes","summary":"repair required","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")

	result, err := Audit(root)
	if err != nil {
		t.Fatal(err)
	}
	if !containsViolation(result.Violations, "has no approve verdict note") {
		t.Fatalf("changes verdict result=%#v", result)
	}
	state, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastMaster == sha {
		t.Fatalf("changes verdict advanced last good master to %s", sha)
	}
}

func TestConcurrentAuditsUseIsolatedLinkedWorktreeSnapshots(t *testing.T) {
	root, _ := testClone(t)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	addRemoteNote(t, root, "refs/notes/forest/review-request", revision, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+revision+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")

	var linked [2]string
	for index := range linked {
		linked[index] = filepath.Join(t.TempDir(), "linked")
		runGitDir(t, root, "worktree", "add", "--detach", linked[index], revision)
		if _, err := Audit(linked[index]); err != nil {
			t.Fatal(err)
		}
	}

	readyA := filepath.Join(t.TempDir(), "ready-a")
	readyB := filepath.Join(t.TempDir(), "ready-b")
	releaseA := filepath.Join(t.TempDir(), "release-a")
	releaseB := filepath.Join(t.TempDir(), "release-b")
	defer func() {
		_ = os.WriteFile(releaseA, nil, 0o644)
		_ = os.WriteFile(releaseB, nil, 0o644)
	}()
	installAuditGitWrapper(t, `#!/bin/sh
set -eu
root=""
if [ "$1" = "-C" ]; then
  root=$2
  shift 2
fi
ready=""
release=""
if [ "$root" = "$AUDIT_ROOT_A" ]; then
  ready=$AUDIT_READY_A
  release=$AUDIT_RELEASE_A
elif [ "$root" = "$AUDIT_ROOT_B" ]; then
  ready=$AUDIT_READY_B
  release=$AUDIT_RELEASE_B
fi
if [ -n "$ready" ] && [ "$1" = "show" ]; then
  : > "$ready"
  while [ ! -e "$release" ]; do
    /bin/sleep 0.01
  done
fi
exec "$AUDIT_REAL_GIT" -C "$root" "$@"
`)
	t.Setenv("AUDIT_ROOT_A", linked[0])
	t.Setenv("AUDIT_ROOT_B", linked[1])
	t.Setenv("AUDIT_READY_A", readyA)
	t.Setenv("AUDIT_READY_B", readyB)
	t.Setenv("AUDIT_RELEASE_A", releaseA)
	t.Setenv("AUDIT_RELEASE_B", releaseB)

	type outcome struct {
		result AuditResult
		err    error
	}
	firstDone := make(chan outcome, 1)
	go func() {
		result, err := Audit(linked[0])
		firstDone <- outcome{result: result, err: err}
	}()
	waitForAuditSignal(t, readyA)

	other := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD^{tree}")))
	addRemoteNote(t, root, "refs/notes/forest/review-request", other, "garbage", "Iron Forest Builder", "builder@forest.invalid")

	secondDone := make(chan outcome, 1)
	go func() {
		result, err := Audit(linked[1])
		secondDone <- outcome{result: result, err: err}
	}()
	waitForAuditSignal(t, readyB)

	live := auditPrivateRefs(t, root)
	if len(live) != 4 {
		t.Fatalf("overlapping private refs=%v want four", live)
	}
	owners := make(map[string]int)
	for _, ref := range live {
		owner := auditPrivateOwner(t, ref)
		owners[owner]++
	}
	if len(owners) != 2 {
		t.Fatalf("overlapping Audits share private namespace: refs=%v owners=%v", live, owners)
	}
	for owner, count := range owners {
		if count != 2 {
			t.Fatalf("private namespace %s has %d refs, want master and notes", owner, count)
		}
	}

	if err := os.WriteFile(releaseB, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var second outcome
	select {
	case second = <-secondDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for second Audit")
	}
	if second.err != nil {
		t.Fatal(second.err)
	}
	if !containsViolation(second.result.Violations, "malformed JSON note") {
		t.Fatalf("second Audit did not read final note snapshot: %#v", second.result)
	}
	remaining := auditPrivateRefs(t, root)
	if len(remaining) != 2 {
		t.Fatalf("second Audit cleared first Audit refs: %v", remaining)
	}
	if auditPrivateOwner(t, remaining[0]) != auditPrivateOwner(t, remaining[1]) {
		t.Fatalf("remaining refs cross private namespaces: %v", remaining)
	}

	if err := os.WriteFile(releaseA, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var first outcome
	select {
	case first = <-firstDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for first Audit")
	}
	if first.err != nil {
		t.Fatal(first.err)
	}
	if len(first.result.Violations) != 0 {
		t.Fatalf("first Audit read second Audit snapshot: %#v", first.result)
	}
	if refs := auditPrivateRefs(t, root); len(refs) != 0 {
		t.Fatalf("private Audit refs remain: %v", refs)
	}
}

func TestAuditCleanupUsesFreshBoundedContextAndJoinsErrors(t *testing.T) {
	root, _ := testClone(t)
	if _, err := Audit(root); err != nil {
		t.Fatal(err)
	}
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	addRemoteNote(t, root, "refs/notes/forest/review-request", revision, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+revision+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
	before, err := os.ReadFile(auditStatePath(root))
	if err != nil {
		t.Fatal(err)
	}

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	previous := gitRun
	defer func() { gitRun = previous }()
	cleanupErr := errors.New("injected cleanup failure")
	cleanupCalls := 0
	gitRun = func(ctx context.Context, root string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "show" {
			cancel()
			return nil, context.Canceled
		}
		if len(args) >= 3 && args[0] == "update-ref" && args[1] == "-d" && parent.Err() == context.Canceled {
			if err := ctx.Err(); err != nil {
				t.Fatalf("cleanup context is already done: %v", err)
			}
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("cleanup context has no deadline")
			}
			if remaining := time.Until(deadline); remaining <= 0 || remaining > time.Second {
				t.Fatalf("cleanup deadline remaining=%v want within one second", remaining)
			}
			cleanupCalls++
			output, err := previous(ctx, root, args...)
			if err == nil && cleanupCalls == 1 {
				return output, cleanupErr
			}
			return output, err
		}
		return previous(ctx, root, args...)
	}

	if _, err := audit(parent, root); !errors.Is(err, context.Canceled) || !errors.Is(err, cleanupErr) {
		t.Fatalf("canceled Audit error=%v", err)
	}
	if cleanupCalls != len(forestNoteRefs)+1 {
		t.Fatalf("cleanup calls=%d want %d", cleanupCalls, len(forestNoteRefs)+1)
	}
	after, err := os.ReadFile(auditStatePath(root))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("canceled Audit changed durable state\nbefore=%s\nafter=%s", before, after)
	}
	if refs := auditPrivateRefs(t, root); len(refs) != 0 {
		t.Fatalf("canceled Audit left private refs: %v", refs)
	}
}

func TestAuditNoteAuthorFailureDoesNotMutateDurableState(t *testing.T) {
	root, _ := testClone(t)
	if _, err := Audit(root); err != nil {
		t.Fatal(err)
	}
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	addRemoteNote(t, root, "refs/notes/forest/review-request", revision, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+revision+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
	before, err := os.ReadFile(auditStatePath(root))
	if err != nil {
		t.Fatal(err)
	}

	previous := gitRun
	defer func() { gitRun = previous }()
	authorErr := errors.New("injected note author failure")
	gitRun = func(ctx context.Context, root string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "log" {
			return nil, authorErr
		}
		return previous(ctx, root, args...)
	}

	if _, err := Audit(root); !errors.Is(err, authorErr) || !strings.Contains(err.Error(), "read note author") {
		t.Fatalf("note author error=%v", err)
	}
	after, err := os.ReadFile(auditStatePath(root))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("note author failure changed audit state\nbefore=%s\nafter=%s", before, after)
	}
	if _, err := os.Stat(auditLogPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("note author failure created policy history: %v", err)
	}
	if refs := auditPrivateRefs(t, root); len(refs) != 0 {
		t.Fatalf("note author failure left private refs: %v", refs)
	}
}

func TestAuditTransportFailuresPreserveStateAndCleanup(t *testing.T) {
	tests := []struct {
		name               string
		failFetch          bool
		ancestryErr        error
		wantAdvertisements int
		wantNoteFetches    int
	}{
		{name: "canonical note fetch", failFetch: true, wantAdvertisements: 3, wantNoteFetches: 3},
		{name: "second final advertisement", wantAdvertisements: 2, wantNoteFetches: 1},
		{name: "joined deadline ancestry", ancestryErr: errors.Join(missingPollNote(t), context.DeadlineExceeded), wantAdvertisements: 2, wantNoteFetches: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, revision := newAdvancedAuditFixture(t, "")
			noteRef := "refs/notes/forest/review-request"
			addRemoteNote(t, root, noteRef, revision, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+revision+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
			before, err := os.ReadFile(auditStatePath(root))
			if err != nil {
				t.Fatal(err)
			}

			previous := gitRun
			defer func() { gitRun = previous }()
			sentinel := errors.New("injected " + test.name + " failure")
			wantErr := error(sentinel)
			if test.ancestryErr != nil {
				wantErr = context.DeadlineExceeded
			}
			advertisements := 0
			noteFetches := 0
			gitRun = func(ctx context.Context, root string, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "ls-remote" {
					advertisements++
					if !test.failFetch && test.ancestryErr == nil && advertisements == 2 {
						return nil, sentinel
					}
				}
				if len(args) > 0 && args[0] == "fetch" {
					for _, arg := range args {
						if strings.HasPrefix(arg, noteRef+":") {
							noteFetches++
							if test.failFetch {
								return nil, sentinel
							}
						}
					}
				}
				if len(args) > 0 && args[0] == "merge-base" && test.ancestryErr != nil {
					return nil, test.ancestryErr
				}
				return previous(ctx, root, args...)
			}

			if _, err := Audit(root); !errors.Is(err, wantErr) {
				t.Fatalf("Audit error=%v, want %v identity", err, wantErr)
			}
			if advertisements != test.wantAdvertisements || noteFetches != test.wantNoteFetches {
				t.Fatalf("advertisements=%d note fetches=%d, want %d and %d", advertisements, noteFetches, test.wantAdvertisements, test.wantNoteFetches)
			}
			after, err := os.ReadFile(auditStatePath(root))
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("transport failure changed audit state\nbefore=%s\nafter=%s", before, after)
			}
			if _, err := os.Stat(auditLogPath(root)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("transport failure created policy history: %v", err)
			}
			if refs := auditPrivateRefs(t, root); len(refs) != 0 {
				t.Fatalf("transport failure left private refs: %v", refs)
			}
		})
	}
}

func TestAuditSyncsRootWhenItCreatesForest(t *testing.T) {
	root, _ := testClone(t)
	previous := syncAuditFile
	defer func() { syncAuditFile = previous }()
	rootSyncs := 0
	syncAuditFile = func(file *os.File) error {
		if filepath.Clean(file.Name()) == filepath.Clean(root) {
			rootSyncs++
		}
		return file.Sync()
	}

	if _, err := Audit(root); err != nil {
		t.Fatal(err)
	}
	if _, err := Audit(root); err != nil {
		t.Fatal(err)
	}
	if rootSyncs != 1 {
		t.Fatalf("repository root syncs=%d want one first-creation sync", rootSyncs)
	}
}

func TestAuditorRetriesAbsentPresentNoteRefRacesToFinalTuple(t *testing.T) {
	tests := []struct {
		name           string
		startPresent   bool
		finalPresent   bool
		wantViolations bool
	}{
		{name: "absent to present", finalPresent: true, wantViolations: true},
		{name: "present to absent", startPresent: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, origin := testClone(t)
			if _, err := Audit(root); err != nil {
				t.Fatal(err)
			}
			ref := "refs/notes/forest/review-request"
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD^{tree}")))
			addNote(t, root, ref, revision, "garbage", "Iron Forest Builder", "builder@forest.invalid")
			oid := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", ref)))
			runGitDir(t, root, "push", "origin", ref+":refs/notes/forest-test/race")
			if test.startPresent {
				runGitDir(t, root, "push", "origin", ref+":"+ref)
			}

			callCount := filepath.Join(t.TempDir(), "calls")
			installAuditGitWrapper(t, `#!/bin/sh
set -eu
root=""
if [ "$1" = "-C" ]; then
  root=$2
  shift 2
fi
if [ "$1" != "ls-remote" ]; then
  exec "$AUDIT_REAL_GIT" -C "$root" "$@"
fi
count=0
if [ -e "$AUDIT_WRAP_COUNT" ]; then
  count=$(cat "$AUDIT_WRAP_COUNT")
fi
count=$((count + 1))
printf '%s\n' "$count" > "$AUDIT_WRAP_COUNT"
if [ "$count" -eq 2 ]; then
  if [ "$AUDIT_FINAL_PRESENT" = "true" ]; then
    "$AUDIT_REAL_GIT" --git-dir "$AUDIT_ORIGIN" update-ref "$AUDIT_NOTE_REF" "$AUDIT_NOTE_OID"
  else
    "$AUDIT_REAL_GIT" --git-dir "$AUDIT_ORIGIN" update-ref -d "$AUDIT_NOTE_REF"
  fi
fi
exec "$AUDIT_REAL_GIT" -C "$root" "$@"
`)
			t.Setenv("AUDIT_WRAP_COUNT", callCount)
			t.Setenv("AUDIT_ORIGIN", origin)
			t.Setenv("AUDIT_NOTE_REF", ref)
			t.Setenv("AUDIT_NOTE_OID", oid)
			t.Setenv("AUDIT_FINAL_PRESENT", strconv.FormatBool(test.finalPresent))

			result, err := Audit(root)
			if err != nil {
				t.Fatal(err)
			}
			hasMalformed := containsViolation(result.Violations, "malformed JSON note")
			if hasMalformed != test.wantViolations {
				t.Fatalf("final tuple result=%#v want malformed=%t", result, test.wantViolations)
			}
			countData, err := os.ReadFile(callCount)
			if err != nil {
				t.Fatal(err)
			}
			if advertisements := strings.TrimSpace(string(countData)); advertisements != "4" {
				t.Fatalf("snapshot advertisements=%s want four across retry", advertisements)
			}
			if refs := auditPrivateRefs(t, root); len(refs) != 0 {
				t.Fatalf("note-ref race left private refs: %v", refs)
			}
		})
	}
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

func TestAuditorGitRunStopsDescendants(t *testing.T) {
	testGitTransportStopsDescendants(t, "Audit", "audit-output", func(ctx context.Context, root string) ([]byte, error) {
		return gitRun(ctx, root, "--version")
	})
}
