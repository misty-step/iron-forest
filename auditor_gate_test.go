package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAuditorRejectsMissingAndMalformedNotes(t *testing.T) {
	t.Run("missing verdict", func(t *testing.T) {
		root, sha := newAdvancedAuditFixture(t, "")
		addValidReviewAndChecks(t, root, sha, "Iron Forest Builder", "builder@forest.invalid")
		result, err := audit(context.Background(), root)
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
		addRemoteNote(t, root, reviewRequestNoteRef, sha, "garbage", "Iron Forest Builder", "builder@forest.invalid")
		result, err := audit(context.Background(), root)
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
		addRemoteNote(t, root, verdictNoteRef, sha, `{"schema":"forest.verdict.v1","revision":"`+sha+`","verdict":"approve","summary":"fixed","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")
		result, err := audit(context.Background(), root)
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
		{name: "review-request", ref: reviewRequestNoteRef},
		{name: "checks", ref: checksNoteRef},
		{name: "verdict", ref: verdictNoteRef},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root, sha := newAdvancedAuditFixture(t, "")
			before, err := readAuditState(root)
			if err != nil {
				t.Fatal(err)
			}
			type evidenceNote struct {
				ref     string
				payload string
				name    string
				email   string
			}
			evidence := []evidenceNote{
				{
					ref:     reviewRequestNoteRef,
					payload: `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`,
					name:    "Iron Forest Builder",
					email:   "builder@forest.invalid",
				},
				{
					ref:     checksNoteRef,
					payload: `{"schema":"forest.checks.v1","revision":"` + sha + `","results":[{"name":"test","ok":true,"exit":0}],"time":"2026-08-10T00:00:00Z"}`,
					name:    "Iron Forest Verifier",
					email:   "verifier@forest.invalid",
				},
				{
					ref:     verdictNoteRef,
					payload: `{"schema":"forest.verdict.v1","revision":"` + sha + `","verdict":"approve","summary":"ok","time":"2026-08-10T00:00:00Z"}`,
					name:    "Iron Forest Verifier",
					email:   "verifier@forest.invalid",
				},
			}
			index := slices.IndexFunc(evidence, func(entry evidenceNote) bool {
				return entry.ref == testCase.ref
			})
			if index < 0 {
				t.Fatalf("missing evidence for %s", testCase.ref)
			}
			evidence[index].name = "Unexpected"
			evidence[index].email = "unexpected@forest.invalid"
			for _, entry := range evidence {
				addRemoteNote(t, root, entry.ref, sha, entry.payload, entry.name, entry.email)
			}

			result, err := audit(context.Background(), root)
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
			addRemoteNote(t, root, reviewRequestNoteRef, revision, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"`+revision+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")

			deps := defaultAuditDependencies()
			runGit := deps.runGit
			deps.runGit = func(ctx context.Context, root string, args ...string) ([]byte, error) {
				output, err := runGit(ctx, root, args...)
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

			result, err := auditWithDependencies(context.Background(), root, deps)
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
	result, err := audit(context.Background(), root)
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
		Ref:      reviewRequestNoteRef,
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
			Ref:      checksNoteRef,
			Revision: master,
			Payload:  []byte(`{"schema":"forest.checks.v1","revision":"` + master + `","results":[{"name":"test","ok":true,"exit":0}],"time":"2026-08-10T00:00:00Z"}`),
			Author:   "Iron Forest Verifier",
			Email:    "verifier@forest.invalid",
		},
		{
			Ref:      verdictNoteRef,
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
	forbiddenSpace := validRequest
	forbiddenSpace.Payload = []byte(`{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-bad branch","revision":"` + master + `","time":"2026-08-10T00:00:00Z"}`)
	forbiddenDots := validRequest
	forbiddenDots.Payload = []byte(`{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-bad..branch","revision":"` + master + `","time":"2026-08-10T00:00:00Z"}`)
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
		{name: "forbidden branch space", requests: []noteEntry{forbiddenSpace}},
		{name: "forbidden branch dots", requests: []noteEntry{forbiddenDots}},
		{name: "duplicate", requests: []noteEntry{validRequest, fixerRequest}},
		{name: "duplicate with malformed", requests: []noteEntry{validRequest, malformed}},
		{name: "single fixer", requests: []noteEntry{fixerRequest}, valid: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			entries := slices.Clone(testCase.requests)
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
	canonical := reviewRequestNoteRef
	ref := auditorNoteRef(snapshot, canonical)
	payload := `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-gate","revision":"` + revision + `","time":"2026-08-10T00:00:00Z"}`
	addNote(t, root, ref, revision, payload, "Unexpected", "unexpected@forest.invalid")
	addNote(t, root, ref, other, payload, "Iron Forest Builder", "builder@forest.invalid")
	snapshot.Notes[canonical] = strings.TrimSpace(string(runGitDir(t, root, "rev-parse", ref)))

	entries, _, err := readNotes(context.Background(), root, snapshot, defaultAuditDependencies())
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

func TestAuditorFlagsDuplicateNoteTreePaths(t *testing.T) {
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
	gitRef := auditorNoteRef(snapshot, reviewRequestNoteRef)
	runGitDir(t, root, "update-ref", gitRef, commit)

	paths, violations, err := notePaths(context.Background(), root, gitRef, reviewRequestNoteRef, defaultAuditDependencies())
	if err != nil {
		t.Fatalf("duplicate note paths error=%v", err)
	}
	if len(paths) != 1 || paths[revision].blob != blob {
		t.Fatalf("duplicate note paths map=%v", paths)
	}
	if len(violations) != 1 || !containsViolation(violations, "duplicate note paths for "+revision) {
		t.Fatalf("duplicate note paths violations=%v", violations)
	}
}

func TestAuditorIgnoresLocalOnlyWorkflowNotes(t *testing.T) {
	root, sha := newAdvancedAuditFixture(t, "")
	addNote(t, root, reviewRequestNoteRef, sha, `{"schema":"forest.review-request.v1","issue":1,"branch":"forest/1-local","revision":"`+sha+`","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Builder", "builder@forest.invalid")
	addNote(t, root, checksNoteRef, sha, `{"schema":"forest.checks.v1","revision":"`+sha+`","results":[{"name":"test","ok":true,"exit":0}],"time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")
	addNote(t, root, verdictNoteRef, sha, `{"schema":"forest.verdict.v1","revision":"`+sha+`","verdict":"approve","summary":"local only","time":"2026-08-10T00:00:00Z"}`, "Iron Forest Verifier", "verifier@forest.invalid")
	result, err := audit(context.Background(), root)
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
		{name: "empty reported", config: "", results: `[]`, want: "invalid checks note"},
		{name: "partial", config: "repo: owner/name\nagents:\n  builder: {poll: forest poll builder, interval: 1}\nchecks:\n  - {name: test, run: test}\n  - {name: vet, run: vet}\n", results: `[{"name":"test","ok":true,"exit":0}]`, want: "does not match configured checks"},
		{name: "extra", config: "", results: `[{"name":"test","ok":true,"exit":0},{"name":"vet","ok":true,"exit":0}]`, want: "does not match configured checks"},
		{name: "order mismatch", config: "repo: owner/name\nagents:\n  builder: {poll: forest poll builder, interval: 1}\nchecks:\n  - {name: test, run: test}\n  - {name: vet, run: vet}\n", results: `[{"name":"vet","ok":true,"exit":0},{"name":"test","ok":true,"exit":0}]`, want: "does not match configured checks"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root, sha := newAdvancedAuditFixture(t, testCase.config)
			addGateNotes(t, root, sha, testCase.results)
			result, err := audit(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if !containsViolation(result.Violations, testCase.want) {
				t.Fatalf("violations=%v", result.Violations)
			}
		})
	}
}

func TestAuditorAcceptsAdvancedFastForwardGate(t *testing.T) {
	root, sha := newAdvancedAuditFixture(t, "")
	addGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)
	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Advanced || len(result.Violations) != 0 {
		t.Fatalf("advanced gate result=%#v", result)
	}
}
