package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePassingChecks(t *testing.T, root string) {
	t.Helper()
	config := []byte(`repo: owner/name
primary: refs/heads/master
agents:
  builder: {poll: "true", interval: 1}
  fixer: {poll: "true", interval: 1}
checks:
  - {name: test, run: "true"}
`)
	writeTree(t, root, profileName+"/config.yaml", string(config))
	runGitDir(t, root, "add", ".iron-forest/config.yaml")
	runGitDir(t, root, "commit", "-m", "passing checks")

}

func writeReviewPayload(t *testing.T, root, revision, branch, runID string) string {
	t.Helper()
	role := "builder"
	if strings.HasSuffix(runID, "-fixer") {
		role = "fixer"
	}
	runRoot, err := primaryCheckout(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	seedPublicationRun(t, runRoot, liveRunRecord{RunID: runID, Agent: role, StartedAt: "2026-08-15T00:00:00Z"})
	return writeReviewPayloadForRun(t, revision, branch, liveRunRecord{RunID: runID})
}

func writeReviewPayloadForRun(t *testing.T, revision, branch string, run liveRunRecord) string {
	t.Helper()
	payload, err := json.Marshal(reviewRequest{Schema: "forest.review-request.v3",
		Subject: reviewSubjectForTest(branch), Branch: branch, Revision: revision,
		Time: "2026-08-15T00:00:00Z", RunID: run.RunID, RequestID: run.RequestID, Work: run.Work})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(path, append(payload, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func pushRejectedRequest(t *testing.T, root, rejected, branch string) {
	t.Helper()
	payload := pollReviewNoteBranch(rejected, branch)
	pushEvidence(t, root, "request", rejected, payload+"\n", "Iron Forest Builder", "builder@forest.invalid")
	pushEvidence(t, root, "verdict", rejected, pollVerdictNote(rejected, "changes"), "Iron Forest Verifier", "verifier@forest.invalid")
}

func seedPublicationRun(t *testing.T, root string, run liveRunRecord) {
	t.Helper()
	if err := writeLiveRun(liveRunPath(root, run.Agent), run); err != nil {
		t.Fatal(err)
	}
	if run.RequestID != "" {
		data, err := json.Marshal(RunRequest{Schema: "forest.request.v1", ID: run.RequestID, Prompt: "Selected work", Work: run.Work})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(forestPath(root, "runs", run.RunID+".request.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPublishNativeWorkRoundTrip(t *testing.T) {
	for _, hasWork := range []bool{false, true} {
		name := "explicit request without work"
		var work *WorkReference
		if hasWork {
			name = "opaque Habitat work"
			work = &WorkReference{System: "https://habitat.example", ID: "immutable-uuid", Key: "RINGS-1", URL: "https://habitat.example/work/immutable-uuid"}
		}
		t.Run(name, func(t *testing.T) {
			root, origin := testClone(t)
			// Even an unrelated legacy pending landing must not cause generic
			// work to call a legacy tracker or mutate that older work.
			seedApprovedCurrent(t, root, "unrelated-legacy")
			writePassingChecks(t, root)
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			builder := liveRunRecord{RunID: "10-builder", Agent: "builder", StartedAt: "2026-09-09T00:00:00Z", RequestID: "build-request", Work: work}
			seedPublicationRun(t, root, builder)
			payload := writeReviewPayloadForRun(t, revision, "forest/rings/initial-slice", builder)
			input := publishReviewRequestInput{Root: root, Role: "builder", Branch: "forest/rings/initial-slice", PayloadPath: payload, RunID: builder.RunID}
			if result, err := publishReviewRequest(context.Background(), input); err != nil || result.Status != "published" {
				t.Fatalf("builder publication=%#v error=%v", result, err)
			}
			publishedRequest := fetchEvidenceFile(t, root, "request", revision, "request.json")
			if !bytes.Equal(publishedRequest, mustRead(t, payload)) {
				t.Fatal("publication lost exact Run/request/work evidence")
			}
			if err := os.Remove(liveRunPath(root, "builder")); err != nil {
				t.Fatal(err)
			}
			verifier := liveRunRecord{RunID: "11-verifier", Agent: "verifier", StartedAt: "2026-09-09T01:00:00Z", RequestID: "independent-review-request", Work: work}
			seedPublicationRun(t, root, verifier)
			checks, verdict := writeEvidencePayloads(t, revision, "approve")
			t.Setenv("POWDER_AGENT", "unrelated-legacy-owner")
			t.Setenv("POWDER_URL", "")
			t.Setenv("POWDER_API_BASE_URL", "")
			poller := NewPoller(root, "owner/name", Scope{})
			poller.PowderCommand = func(context.Context, ...string) ([]byte, []byte, error) {
				t.Fatal("generic publication invoked Powder mutation")
				return nil, nil, errors.New("unexpected tracker call")
			}
			verdictInput := publishVerdictInput{Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: verifier.RunID, Powder: poller}
			for _, status := range []string{"published", "identical"} {
				result, err := publishVerdict(context.Background(), verdictInput)
				if err != nil || result.Status != status || result.PowderStatus != "" {
					t.Fatalf("verifier %s=%#v error=%v", status, result, err)
				}
			}
			if got := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master"))); got != revision {
				t.Fatalf("primary=%s want candidate=%s", got, revision)
			}
			if !bytes.Equal(publishedRequest, fetchEvidenceFile(t, root, "request", revision, "request.json")) {
				t.Fatal("approval rewrote immutable request")
			}
		})
	}
}

func TestPublishReviewRequestBindsActualRunContext(t *testing.T) {
	cases := []struct {
		name string
		change func(*liveRunRecord)
	}{
		{"run", func(run *liveRunRecord) { run.RunID = "other-builder" }},
		{"request", func(run *liveRunRecord) { run.RequestID = "other-request" }},
		{"missing request", func(run *liveRunRecord) { run.RequestID = "" }},
		{"work system", func(run *liveRunRecord) { run.Work.System = "other-system" }},
		{"work identity", func(run *liveRunRecord) { run.Work.ID = "other-work" }},
		{"work display key", func(run *liveRunRecord) { run.Work.Key = "OTHER-1" }},
		{"work URL", func(run *liveRunRecord) { run.Work.URL = "https://other.example" }},
		{"missing work", func(run *liveRunRecord) { run.Work = nil }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root, _ := testClone(t)
			writePassingChecks(t, root)
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			run := liveRunRecord{RunID: "10-builder", Agent: "builder", StartedAt: "2026-09-09T00:00:00Z", RequestID: "selected-request",
				Work: &WorkReference{System: "opaque", ID: "selected-work", Key: "WORK-1", URL: "https://work.example/1"}}
			seedPublicationRun(t, root, run)
			test.change(&run)
			payload := writeReviewPayloadForRun(t, revision, "forest/work/implementation", run)
			before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
			_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
				Root: root, Role: "builder", Branch: "forest/work/implementation", PayloadPath: payload, RunID: "10-builder",
			})
			if err == nil || publishConflict(err) {
				t.Fatalf("mismatched provenance error=%v", err)
			}
			if after := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); after != before {
				t.Fatal("mismatched provenance changed remote refs")
			}
		})
	}
}

func TestPublishReviewRequestRequiresOwnedRunBeforeIdentical(t *testing.T) {
	for _, state := range []string{"ended", "wrong role", "finalizing", "cancelled", "missing retained request"} {
		t.Run(state, func(t *testing.T) {
			root, _ := testClone(t)
			writePassingChecks(t, root)
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			run := liveRunRecord{RunID: "10-builder", Agent: "builder", StartedAt: "2026-09-09T00:00:00Z", RequestID: "selected-request"}
			seedPublicationRun(t, root, run)
			payload := writeReviewPayloadForRun(t, revision, "forest/work/implementation", run)
			input := publishReviewRequestInput{Root: root, Role: "builder", Branch: "forest/work/implementation", PayloadPath: payload, RunID: run.RunID}
			invalidate := func() {
				switch state {
				case "ended":
					if err := os.Remove(liveRunPath(root, "builder")); err != nil { t.Fatal(err) }
				case "wrong role":
					wrong := run
					wrong.Agent = "fixer"
					if err := writeLiveRun(liveRunPath(root, "builder"), wrong); err != nil { t.Fatal(err) }
				case "finalizing":
					ended := run
					ended.Result = &RunRecord{Outcome: runOutcomeCompleted}
					if err := writeLiveRun(liveRunPath(root, "builder"), ended); err != nil { t.Fatal(err) }
				case "cancelled":
					if err := writeRunCancellationMarker(root, run.RunID); err != nil { t.Fatal(err) }
				case "missing retained request":
					if err := os.Remove(forestPath(root, "runs", run.RunID+".request.json")); err != nil { t.Fatal(err) }
				}
			}
			before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
			invalidate()
			if _, err := publishReviewRequest(context.Background(), input); err == nil {
				t.Fatal("inactive owner published")
			}
			if after := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); after != before {
				t.Fatal("inactive owner changed refs")
			}
			_ = os.Remove(runCancellationMarkerPath(root, run.RunID))
			seedPublicationRun(t, root, run)
			if _, err := publishReviewRequest(context.Background(), input); err != nil { t.Fatal(err) }
			before = string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
			invalidate()
			if _, err := publishReviewRequest(context.Background(), input); err == nil {
				t.Fatal("identical evidence bypassed owner check")
			}
			if after := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); after != before {
				t.Fatal("inactive identical retry changed refs")
			}
		})
	}
}

func TestPublishReviewRequestRechecksOwnerAfterChecks(t *testing.T) {
	root, _ := testClone(t)
	writeTree(t, root, profileName+"/config.yaml", "repo: owner/name\nagents:\n  builder: {poll: 'true', interval: 1}\nchecks:\n  - {name: test, run: 'rm \"$FOREST_TEST_BUILDER_LIVE\"'}\n")
	runGitDir(t, root, "commit", "-am", "end builder during check")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	payload := writeReviewPayload(t, root, revision, "forest/work/implementation", "10-builder")
	t.Setenv("FOREST_TEST_BUILDER_LIVE", liveRunPath(root, "builder"))
	before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/work/implementation", PayloadPath: payload, RunID: "10-builder",
	})
	if err == nil { t.Fatal("ended builder published after checks") }
	if _, err := os.Stat(liveRunPath(root, "builder")); !os.IsNotExist(err) { t.Fatalf("check did not end owner: %v", err) }
	if after := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); after != before {
		t.Fatal("ended builder published refs")
	}
}

func TestPublishFixerPreservesWorkAndBranch(t *testing.T) {
	for _, drift := range []string{"none", "subject", "branch", "work"} {
		t.Run(drift, func(t *testing.T) {
			root, _ := testClone(t)
			writePassingChecks(t, root)
			rejected := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			builder := liveRunRecord{RunID: "10-builder", Agent: "builder", StartedAt: "2026-09-09T00:00:00Z", RequestID: "build-request", Work: &WorkReference{System: "opaque", ID: "work-id", Key: "WORK-1"}}
			seedPublicationRun(t, root, builder)
			branch := "forest/work/implementation"
			payload := writeReviewPayloadForRun(t, rejected, branch, builder)
			if _, err := publishReviewRequest(context.Background(), publishReviewRequestInput{Root: root, Role: "builder", Branch: branch, PayloadPath: payload, RunID: builder.RunID}); err != nil { t.Fatal(err) }
			verifier := liveRunRecord{RunID: "11-verifier", Agent: "verifier", StartedAt: builder.StartedAt, RequestID: "review-request", Work: builder.Work}
			seedPublicationRun(t, root, verifier)
			checks, verdict := writeEvidencePayloads(t, rejected, "changes")
			if _, err := publishVerdict(context.Background(), publishVerdictInput{Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: verifier.RunID}); err != nil { t.Fatal(err) }
			runGitDir(t, root, "commit", "--allow-empty", "-m", "repair")
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			work := *builder.Work
			fixer := liveRunRecord{RunID: "12-fixer", Agent: "fixer", StartedAt: builder.StartedAt, RequestID: "repair-request", Work: &work}
			switch drift {
			case "subject": branch = "forest/other/implementation"
			case "branch": branch = "forest/work/other"
			case "work": fixer.Work.Key = "WORK-2"
			}
			seedPublicationRun(t, root, fixer)
			payload = writeReviewPayloadForRun(t, revision, branch, fixer)
			before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
			result, err := publishReviewRequest(context.Background(), publishReviewRequestInput{Root: root, Role: "fixer", Branch: branch, PayloadPath: payload, RunID: fixer.RunID, Rejected: rejected})
			if drift == "none" {
				if err != nil || result.Status != "published" { t.Fatalf("repair=%#v error=%v", result, err) }
				verifier.RunID, verifier.RequestID = "13-verifier", "repaired-review-request"
				seedPublicationRun(t, root, verifier)
				checks, verdict = writeEvidencePayloads(t, revision, "approve")
				if _, err := publishVerdict(context.Background(), publishVerdictInput{Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: verifier.RunID}); err != nil { t.Fatal(err) }
				return
			}
			if err == nil { t.Fatal("fixer changed rejected work identity") }
			if after := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); after != before { t.Fatal("drifted fixer changed refs") }
		})
	}
}

func TestPublishGreenfieldChecksFailClosedUntilAuthored(t *testing.T) {
	root, _ := testClone(t)
	writeTree(t, root, profileName+"/config.yaml", "repo: owner/name\nagents:\n  builder: {poll: 'true', interval: 1}\nchecks:\n  - {name: product, run: 'sh product-check.sh'}\n")
	runGitDir(t, root, "commit", "-am", "declare required product check")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	payload := writeReviewPayload(t, root, revision, "forest/work/implementation", "10-builder")
	input := publishReviewRequestInput{Root: root, Role: "builder", Branch: "forest/work/implementation", PayloadPath: payload, RunID: "10-builder"}
	before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
	if _, err := publishReviewRequest(context.Background(), input); err == nil { t.Fatal("missing product check passed") }
	if after := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); after != before { t.Fatal("greenfield failure published refs") }
	writeTree(t, root, "product-check.sh", "test \"$(cat value.txt)\" = implemented\n")
	writeTree(t, root, "value.txt", "implemented\n")
	runGitDir(t, root, "add", "product-check.sh", "value.txt")
	runGitDir(t, root, "commit", "-m", "author product and executable check")
	revision = strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	input.PayloadPath = writeReviewPayload(t, root, revision, input.Branch, input.RunID)
	if result, err := publishReviewRequest(context.Background(), input); err != nil || result.Revision != revision { t.Fatalf("authored candidate=%#v error=%v", result, err) }
}

func fetchEvidence(t *testing.T, root, kind, sha string) string {
	t.Helper()
	ref := evidenceKindRef(kind, sha)
	if ref == "" || !isSHA(sha) {
		t.Fatalf("invalid evidence identity kind=%s sha=%s", kind, sha)
	}
	local := "refs/forest/test/" + kind + "-" + sha
	runGitDir(t, root, "fetch", "origin", "+"+ref+":"+local)
	t.Cleanup(func() { _ = exec.Command("git", "-C", root, "update-ref", "-d", local).Run() })
	return local
}

func fetchEvidenceFile(t *testing.T, root, kind, sha, name string) []byte {
	t.Helper()
	local := fetchEvidence(t, root, kind, sha)
	return runGitDir(t, root, "show", local+":"+name)
}

func fetchEvidenceIdentity(t *testing.T, root, kind, sha string) []byte {
	t.Helper()
	local := fetchEvidence(t, root, kind, sha)
	return runGitDir(t, root, "log", "-1", "--format=%an <%ae>", local)
}

func TestPublishReviewRequestCreatesBranchAndRequestRef(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	t.Setenv("FOREST_RUN_ID", "1-builder")
	result, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder"),
		RunID:       "1-builder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "published" || result.Revision != revision {
		t.Fatalf("result=%#v", result)
	}
	remote := strings.TrimSpace(string(runGitDir(t, root, "ls-remote", "origin", "refs/heads/forest/1/ready")))
	if !strings.HasPrefix(remote, revision) {
		t.Fatalf("remote branch=%q", remote)
	}
	if noteRef := strings.TrimSpace(string(runGitDir(t, root, "ls-remote", "origin", "refs/notes/forest/review-request"))); noteRef != "" {
		t.Fatalf("unexpected review-request note ref=%q", noteRef)
	}
	shown := string(fetchEvidenceFile(t, root, "request", revision, "request.json"))
	if !strings.Contains(shown, `"schema":"forest.review-request.v3"`) {
		t.Fatalf("request evidence=%q", shown)
	}
}


func TestPublishReviewRequestRejectsHistoricalWriter(t *testing.T) {
	for _, schema := range []string{"forest.review-request.v1", "forest.review-request.v2"} {
		t.Run(schema, func(t *testing.T) {
			root, _ := testClone(t)
			writePassingChecks(t, root)
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			path := writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder")
			payload := `{"schema":"` + schema + `","subject":"1","branch":"forest/1/ready","revision":"` + revision + `","time":"2026-08-15T00:00:00Z"}`
			if schema == "forest.review-request.v1" {
				payload = strings.Replace(payload, `"subject":"1"`, `"issue":1`, 1)
			}
			if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
				t.Fatal(err)
			}
			before := string(runGitDir(t, root, "ls-remote", "--refs", "origin"))
			_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
				Root: root, Role: "builder", Branch: "forest/1/ready", PayloadPath: path, RunID: "1-builder",
			})
			if err == nil {
				t.Fatal("historical request accepted by new writer")
			}
			if after := string(runGitDir(t, root, "ls-remote", "--refs", "origin")); after != before {
				t.Fatal("historical writer changed remote evidence")
			}
		})
	}
}

func TestPublishReviewRequestConflictsMismatchedRequestRef(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	other := `{"schema":"forest.review-request.v3","run_id":"1-builder","subject":"9","branch":"forest/1/ready","revision":"` + revision + `","time":"2026-08-17T00:00:00Z"}` + "\n"
	pushEvidence(t, root, "request", revision, other, "Iron Forest Builder", "builder@forest.invalid")
	t.Setenv("FOREST_RUN_ID", "1-builder")
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder"),
		RunID:       "1-builder",
	})
	if !publishConflict(err) {
		t.Fatalf("error=%v, want conflicting request evidence", err)
	}
}

func TestPublishReviewRequestIgnoresHostileGitIdentity(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	t.Setenv("GIT_AUTHOR_NAME", "Eve")
	t.Setenv("GIT_AUTHOR_EMAIL", "eve@invalid")
	t.Setenv("GIT_COMMITTER_NAME", "Eve")
	t.Setenv("GIT_COMMITTER_EMAIL", "eve@invalid")
	if _, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder"), RunID: "1-builder",
	}); err != nil {
		t.Fatal(err)
	}
	identity := strings.TrimSpace(string(fetchEvidenceIdentity(t, root, "request", revision)))
	if identity != "Iron Forest Builder <builder@forest.invalid>" {
		t.Fatalf("request actor=%q", identity)
	}
}

func TestPublishReviewRequestAcceptsIdenticalRequest(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	payload := mustRead(t, writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder"))
	pushEvidence(t, root, "request", revision, string(payload), "Iron Forest Builder", "builder@forest.invalid")
	result, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder"), RunID: "1-builder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "identical" || result.Revision != revision {
		t.Fatalf("result=%#v", result)
	}
}

func TestPublishReviewRequestRefusesFailedCheck(t *testing.T) {
	root, _ := testClone(t)
	writeTree(t, root, profileName+"/config.yaml", "repo: owner/name\nagents:\n  builder: {poll: \"true\", interval: 1}\nchecks:\n  - {name: test, run: \"false\"}\n")
	runGitDir(t, root, "commit", "-am", "failing check")

	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder"),
		RunID:       "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), `check "test" failed`) {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestUsesTrustedSecretsScanner(t *testing.T) {
	root, _ := testClone(t)
	origLook, origRun := scanEnv.lookPath, scanEnv.runGeneric
	scanEnv.lookPath = func(string, string) (string, error) { return "stub", nil }
	scanEnv.runGeneric = func(context.Context, string, string, []string) ([]secretFinding, error) {
		return []secretFinding{{Path: "fixture.txt", Rule: "TestSecret"}}, nil
	}
	defer func() { scanEnv.lookPath, scanEnv.runGeneric = origLook, origRun }()

	// The candidate carries a neutered scanner and a planted fixture, and it
	// declares no `secrets` check. The Gate must still run its own scanner and
	// fail on the fixture rather than compiling candidate code or waiting for a
	// candidate-defined check name.
	writeTree(t, root, "scansecrets.go", "package main\nfunc scanSecretsTree(string) ([]secretFinding, error) { return nil, nil }\n")
	writeTree(t, root, "fixture.txt", "PLANTED-CREDENTIAL-FIXTURE\n")
	config := []byte(`repo: owner/name
primary: refs/heads/master
agents:
  builder: {poll: "true", interval: 1}
  fixer: {poll: "true", interval: 1}
checks:
  - {name: test, run: "true"}
`)
	writeTree(t, root, profileName+"/config.yaml", string(config))
	runGitDir(t, root, "add", ".iron-forest/config.yaml", "scansecrets.go", "fixture.txt")
	runGitDir(t, root, "commit", "-m", "candidate neutered scanner and planted fixture")
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	t.Setenv("FOREST_RUN_ID", "1-builder")
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder"),
		RunID:       "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "secrets scan") || !strings.Contains(err.Error(), "fixture.txt") {
		t.Fatalf("error=%v, want the trusted secrets scan to fail on the planted fixture", err)
	}
	if strings.Contains(err.Error(), "PLANTED-CREDENTIAL-FIXTURE") {
		t.Fatalf("error leaked the planted credential value: %v", err)
	}
}

func TestPublishReviewRequestCleansCanceledSecretsScanWorktree(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)

	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	t.Setenv("HEARTBEAT", heartbeat)
	binDir := t.TempDir()
	script := "#!/bin/sh\nwhile :; do printf x >> \"$HEARTBEAT\"; /bin/sleep 0.02; done\n"
	if err := os.WriteFile(filepath.Join(binDir, secretScanner), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := publishReviewRequest(ctx, publishReviewRequestInput{
			Root:        root,
			Role:        "builder",
			Branch:      "forest/1/ready",
			PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder"),
			RunID:       "1-builder",
		})
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		listed := string(runGitDir(t, root, "worktree", "list", "--porcelain"))
		if strings.Contains(listed, "-checks") {
			cancel()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("secrets scan worktree did not appear")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation")
		}
	case <-time.After(cleanupTimeout + 2*time.Second):
		t.Fatal("publish did not return after cancel")
	}
	listed := string(runGitDir(t, root, "worktree", "list", "--porcelain"))
	if strings.Contains(listed, "-checks") {
		t.Fatalf("stale worktree remains:\n%s", listed)
	}
}

func TestPublishReviewRequestDetectsBranchRace(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "move")
	other := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "--force", "origin", "HEAD:refs/heads/forest/1/ready")
	runGitDir(t, root, "reset", "--hard", revision)
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder"),
		RunID:       "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "branch race") {
		t.Fatalf("error=%v other=%s", err, other)
	}
}

func TestPublishReviewRequestKeepsPolicyRejectionDistinct(t *testing.T) {
	root, origin := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	hook := filepath.Join(origin, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'policy rejects this ref' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "builder",
		Branch:      "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder"),
		RunID:       "1-builder",
	})
	if err == nil {
		t.Fatal("expected remote policy rejection")
	}
	if publishConflict(err) {
		t.Fatalf("policy rejection must not be classified as a branch race: %v", err)
	}
	if strings.Contains(err.Error(), "branch race") {
		t.Fatalf("policy rejection must not emit a branch race: %v", err)
	}
	if !strings.Contains(err.Error(), "policy rejects this ref") {
		t.Fatalf("policy rejection diagnostic lost: %v", err)
	}
}

func TestClassifyReviewPushNarrowsToLeaseEvidence(t *testing.T) {
	tests := []struct {
		name         string
		output       string
		wantConflict bool
	}{
		{name: "stale lease", output: "! 0000000000000000000000000000000000000000:refs/heads/forest/1/ready\t[rejected] (stale info)", wantConflict: true},
		{name: "non fast forward", output: "! 0000000000000000000000000000000000000000:refs/heads/forest/1/ready\t[rejected] (non-fast-forward)", wantConflict: true},
		{name: "remote policy", output: "! 0000000000000000000000000000000000000000:refs/heads/forest/1/ready\t[remote rejected] (pre-receive hook declined)", wantConflict: false},
		{name: "transport failure without porcelain", output: "", wantConflict: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := errors.New("git push failed")
			got := classifyReviewPush([]byte(test.output), base)
			if publishConflict(got) != test.wantConflict {
				t.Fatalf("conflict=%v want %v (err=%v)", publishConflict(got), test.wantConflict, got)
			}
			if !test.wantConflict && !errors.Is(got, base) {
				t.Fatalf("original error lost: %v", got)
			}
		})
	}
}

func TestPublishReviewRequestRejectsAncestorBranchCreatedDuringPush(t *testing.T) {
	root, origin := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	parent := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", parent+":refs/heads/master")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("child\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "child")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	script := "#!/bin/sh\nset -e\n" +
		"if printf '%s' \"$*\" | grep -q -- '--atomic'; then\n" +
		"  \"" + realGit + "\" -C \"" + origin + "\" update-ref refs/heads/forest/1/ready " + parent + "\n" +
		"fi\n" +
		"exec \"" + realGit + "\" \"$@\"\n"

	if err := os.WriteFile(filepath.Join(wrapperDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err = publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder"), RunID: "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "branch race") {
		t.Fatalf("error=%v", err)
	}
	remote := strings.TrimSpace(string(runGitDir(t, origin, "rev-parse", "refs/heads/forest/1/ready")))
	if remote != parent {
		t.Fatalf("origin branch=%s want parent=%s", remote, parent)
	}
}

func TestPublishReviewRequestBuilderRejectsBranchWithoutRequestRef(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder"), RunID: "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "branch race") {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestFixerRejectsBranchWithoutRequestRef(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	rejected := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushRejectedRequest(t, root, rejected, "forest/1/ready")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "fix")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "fixer", Branch: "forest/1/ready", Rejected: rejected,
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "2-fixer"), RunID: "2-fixer",
	})
	if err == nil || !strings.Contains(err.Error(), "branch race") {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestFixerAdvancesRejectedBranch(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	rejected := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushRejectedRequest(t, root, rejected, "forest/1/ready")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "fix")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	result, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        root,
		Role:        "fixer",
		Branch:      "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "2-fixer"),
		Rejected:    rejected,
		RunID:       "2-fixer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "published" || result.Revision != revision {
		t.Fatalf("result=%#v", result)
	}
}


func TestPublishReviewRequestFixerPreservesTrackerlessHistoricalEvidence(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	rejected := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	historical := `{"schema":"forest.review-request.v2","subject":"1","branch":"forest/1/ready","revision":"` + rejected + `","time":"2026-08-15T00:00:00Z"}`
	pushEvidence(t, root, "request", rejected, historical, "Iron Forest Builder", "builder@forest.invalid")
	pushEvidence(t, root, "verdict", rejected, pollVerdictNote(rejected, "changes"), "Iron Forest Verifier", "verifier@forest.invalid")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "fix")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	result, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "fixer", Branch: "forest/1/ready", Rejected: rejected,
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "2-fixer"), RunID: "2-fixer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "published" {
		t.Fatalf("result=%#v", result)
	}
	if got := string(fetchEvidenceFile(t, root, "request", rejected, "request.json")); got != historical {
		t.Fatal("fixer rewrote historical request")
	}
}

func TestCLIPublishReviewRequestNeedsRunID(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	payload := writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder")
	t.Setenv("FOREST_RUN_ID", "")
	code, _, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"publish", "review-request", "builder", "forest/1/ready", payload, "--root", root})
	})
	if code != exitError || !strings.Contains(stderr, "FOREST_RUN_ID") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestCLIPublishReviewRequestPreflight(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	payload := filepath.Join(t.TempDir(), "review.json")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "role neither builder nor fixer",
			args: []string{"publish", "review-request", "verifier", "forest/1/ready", payload, "--root", root},
			want: "role must be builder or fixer",
		},
		{
			name: "fixer without rejected sha",
			args: []string{"publish", "review-request", "fixer", "forest/1/ready", payload, "--root", root},
			want: "fixer publication requires --rejected <sha>",
		},
		{
			name: "builder carrying rejected sha",
			args: []string{"publish", "review-request", "builder", "forest/1/ready", payload, "--rejected", "0123456789abcdef0123456789abcdef01234567", "--root", root},
			want: "builder publication does not accept --rejected",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("FOREST_RUN_ID", "")
			code, _, stderr := captureCLIOutput(t, func() int {
				return runSurfaceCommand(test.args)
			})
			if code != exitError {
				t.Fatalf("code=%d stderr=%q", code, stderr)
			}
			if !strings.Contains(stderr, test.want) {
				t.Fatalf("stderr=%q, want substring %q", stderr, test.want)
			}
		})
	}
}

func TestCLIPublishReviewRequestFixerBranchRaceIsConflict(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	rejected := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	pushRejectedRequest(t, root, rejected, "forest/1/ready")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "commit", "-am", "fix")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/1/ready")
	payload := writeReviewPayload(t, root, revision, "forest/1/ready", "2-fixer")
	t.Setenv("FOREST_RUN_ID", "2-fixer")
	code, stdout, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"publish", "review-request", "fixer", "forest/1/ready", payload, "--rejected", rejected, "--json", "--root", root})
	})
	if code != exitConflict || !strings.Contains(stdout+stderr, "branch race") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestPublishReviewRequestConflictsOnWhitespaceOnlyRequestRef(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	runGitDir(t, root, "checkout", "-b", "forest/1/ready")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	payload := writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder")
	if _, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready", PayloadPath: payload, RunID: "1-builder",
	}); err != nil {
		t.Fatal(err)
	}
	padded := filepath.Join(t.TempDir(), "padded.json")
	raw, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(padded, append(raw, '\n', '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready", PayloadPath: padded, RunID: "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting request evidence") {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestRefusesRepositoryGit(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	payload := writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder")
	if err := os.WriteFile(filepath.Join(root, "git"), []byte("#!/bin/sh\necho PLANTED >&2\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready",
		PayloadPath: payload, RunID: "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "refuse repository executable") {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestRefusesRepositorySh(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	if err := os.WriteFile(filepath.Join(root, "sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder"), RunID: "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), "refuse repository executable") {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestIgnoresRepositoryGo(t *testing.T) {
	root, _ := testClone(t)
	writeTree(t, root, profileName+"/config.yaml", "repo: owner/name\nagents:\n  builder: {poll: \"true\", interval: 1}\nchecks:\n  - {name: test, run: \"go planted\"}\n")
	runGitDir(t, root, "commit", "-am", "check uses go")
	if err := os.WriteFile(filepath.Join(root, "go"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder"), RunID: "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), `check "test" failed`) {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestIgnoresUncommittedCheckPass(t *testing.T) {
	root, _ := testClone(t)
	config := []byte(`repo: owner/name
agents:
  builder: {poll: "true", interval: 1}
checks:
  - {name: test, run: "grep -qx dirty file"}
`)
	writeTree(t, root, profileName+"/config.yaml", string(config))
	runGitDir(t, root, "commit", "-am", "check requires dirty file")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	_, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready",
		PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder"), RunID: "1-builder",
	})
	if err == nil || !strings.Contains(err.Error(), `check "test" failed`) {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishReviewRequestKeepsCapturedPayloadIfCheckRewritesFile(t *testing.T) {
	root, _ := testClone(t)
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	payload := writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder")
	config := "repo: owner/name\nagents:\n  builder: {poll: \"true\", interval: 1}\nchecks:\n  - {name: test, run: \"printf TAMPERED > " + payload + "\"}\n"
	writeTree(t, root, profileName+"/config.yaml", config)
	runGitDir(t, root, "commit", "-am", "check rewrites payload")
	revision = strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	original := []byte(`{"schema":"forest.review-request.v3","subject":"1","branch":"forest/1/ready","revision":"` + revision + `","time":"2026-08-15T00:00:00Z","run_id":"1-builder"}` + "\n")
	if err := os.WriteFile(payload, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, Role: "builder", Branch: "forest/1/ready", PayloadPath: payload, RunID: "1-builder",
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(payload); err != nil || string(got) != "TAMPERED" {
		t.Fatalf("payload file after check=%q err=%v", got, err)
	}
	shown := fetchEvidenceFile(t, root, "request", revision, "request.json")
	if bytes.Contains(shown, []byte("TAMPERED")) {
		t.Fatalf("published tampered payload: %q", shown)
	}
	if !bytes.Contains(shown, []byte(`"revision":"`+revision+`"`)) {
		t.Fatalf("published request evidence=%q", shown)
	}
}
func TestPublishReviewRequestCleansCanceledCheckWorktree(t *testing.T) {
	root, _ := testClone(t)
	writeTree(t, root, profileName+"/config.yaml", "repo: owner/name\nagents:\n  builder: {poll: \"true\", interval: 1}\nchecks:\n  - {name: test, run: \"sleep 30\"}\n")
	runGitDir(t, root, "commit", "-am", "slow check")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := publishReviewRequest(ctx, publishReviewRequestInput{
			Root: root, Role: "builder", Branch: "forest/1/ready",
			PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "1-builder"), RunID: "1-builder",
		})
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		listed := string(runGitDir(t, root, "worktree", "list", "--porcelain"))
		if strings.Contains(listed, "-checks") {
			cancel()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("check worktree did not appear")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation")
		}
	case <-time.After(cleanupTimeout + 2*time.Second):
		t.Fatal("publish did not return after cancel")
	}
	listed := string(runGitDir(t, root, "worktree", "list", "--porcelain"))
	if strings.Contains(listed, "-checks") {
		t.Fatalf("stale worktree remains:\n%s", listed)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func reservedCheckDirs(root string) []string {
	entries, err := os.ReadDir(forestPath(root, "worktrees"))
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && isReservedRunID(entry.Name()) && strings.HasSuffix(entry.Name(), "-checks") {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestPublishCheckWorktreeIsReservedAndSweptAfterKill(t *testing.T) {
	root, _ := testClone(t)
	writePassingChecks(t, root)
	dir := forestPath(root, "worktrees", newRunID("checks", time.Unix(1, 0)))
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "worktree", "add", "--detach", dir, "HEAD")
	if !isReservedRunID(filepath.Base(dir)) {
		t.Fatal("check worktree name is not reserved")
	}
	if err := cleanupReservedResidue(root, NewRunner(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reserved check worktree survived: %v", err)
	}
}

func TestPublishCheckWorktreeFromLinkedRunIsSweptOnPrimary(t *testing.T) {
	if os.Getenv("FOREST_PUBLISH_CHILD") == "1" {
		root := os.Getenv("FOREST_PUBLISH_ROOT")
		revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
		_, _ = publishReviewRequest(context.Background(), publishReviewRequestInput{
			Root: root, Role: "builder", Branch: "forest/1/ready",
			PayloadPath: writeReviewPayload(t, root, revision, "forest/1/ready", "manual"),
			RunID:       "manual",
		})
		os.Exit(0)
	}
	primary, _ := testClone(t)
	writeTree(t, primary, profileName+"/config.yaml", "repo: owner/name\nagents:\n  builder: {poll: \"true\", interval: 1}\nchecks:\n  - {name: test, run: \"sleep 30\"}\n")
	runGitDir(t, primary, "commit", "-am", "slow check")
	linked := filepath.Join(t.TempDir(), "1786820000000000002-builder")
	runGitDir(t, primary, "worktree", "add", "--detach", linked, "HEAD")
	cmd := exec.Command(os.Args[0], "-test.run=^TestPublishCheckWorktreeFromLinkedRunIsSweptOnPrimary$", "-test.v=false")
	cmd.Env = append(os.Environ(), "FOREST_PUBLISH_CHILD=1", "FOREST_PUBLISH_ROOT="+linked)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var found []string
	for {
		found = reservedCheckDirs(primary)
		if len(found) > 0 {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("primary check worktree did not appear")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(forestPath(linked, "worktrees")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("check worktree was nested under the linked Run worktree")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
	if err := cleanupReservedResidue(primary, NewRunner(primary)); err != nil {
		t.Fatal(err)
	}
	if leftover := reservedCheckDirs(primary); len(leftover) != 0 {
		t.Fatalf("primary check worktrees survived: %v", leftover)
	}
}

func TestPublishVerdictRequiresOwnedVerifierRun(t *testing.T) {
	for _, invalidContext := range []string{"ended", "foreign_root", "wrong_agent", "missing_agent", "malformed"} {
		t.Run(invalidContext, func(t *testing.T) {
			root, origin := testClone(t)
			writePassingChecks(t, root)
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			request := pollReviewNoteBranch(revision, "forest/1/work")
			pushEvidence(t, root, "request", revision, request, "Iron Forest Builder", "builder@forest.invalid")
			runGitDir(t, root, "push", "origin", revision+":refs/heads/forest/1/work")
			checks, verdict := writeEvidencePayloads(t, revision, "approve")
			input := publishVerdictInput{
				Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier",
			}

			var foreign string
			if invalidContext == "foreign_root" {
				foreign, _ = testClone(t)
				seedVerdictRun(t, foreign, "1-verifier")
			}
			seedVerdictRun(t, root, "1-verifier")
			invalidate := func() {
				path := liveRunPath(root, "verifier")
				switch invalidContext {
				case "ended", "foreign_root":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if foreign != "" {
						t.Setenv("FOREST_ROOT", foreign)
					}
				case "wrong_agent", "missing_agent":
					var record liveRunRecord
					if err := json.Unmarshal(mustRead(t, path), &record); err != nil {
						t.Fatal(err)
					}
					record.Agent = "builder"
					if invalidContext == "missing_agent" {
						record.Agent = ""
					}
					if err := writeLiveRun(path, record); err != nil {
						t.Fatal(err)
					}
				case "malformed":
					if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}

			before := string(runGit(t, "--git-dir="+origin, "for-each-ref", "--format=%(refname) %(objectname)"))
			invalidate()
			if _, err := publishVerdict(context.Background(), input); err == nil || publishConflict(err) {
				t.Fatalf("first publish error=%v, want context refusal rather than a publication conflict", err)
			}
			if got := string(runGit(t, "--git-dir="+origin, "for-each-ref", "--format=%(refname) %(objectname)")); got != before {
				t.Fatalf("unauthorized publish changed remote refs:\nbefore:\n%safter:\n%s", before, got)
			}

			seedVerdictRun(t, root, "1-verifier")
			result, err := publishVerdict(context.Background(), input)
			if err != nil || result.Status != "published" {
				t.Fatalf("valid publish result=%#v error=%v", result, err)
			}
			if got := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master"))); got != revision {
				t.Fatalf("valid publish primary=%s, want %s", got, revision)
			}
			if got := fetchEvidenceFile(t, root, "checks", revision, "checks.json"); !bytes.Equal(got, mustRead(t, checks)) {
				t.Fatalf("published checks=%q", got)
			}
			if got := fetchEvidenceFile(t, root, "verdict", revision, "verdict.json"); !bytes.Equal(got, mustRead(t, verdict)) {
				t.Fatalf("published verdict=%q", got)
			}
			published := string(runGit(t, "--git-dir="+origin, "for-each-ref", "--format=%(refname) %(objectname)"))

			invalidate()
			if _, err := publishVerdict(context.Background(), input); err == nil || publishConflict(err) {
				t.Fatalf("identical retry error=%v, want context refusal before idempotency", err)
			}
			if got := string(runGit(t, "--git-dir="+origin, "for-each-ref", "--format=%(refname) %(objectname)")); got != published {
				t.Fatalf("unauthorized retry changed remote refs:\nbefore:\n%safter:\n%s", published, got)
			}
		})
	}
}

func TestCLIPublishVerdictFromOwnedLinkedWorktree(t *testing.T) {
	primary, origin := testClone(t)
	writePassingChecks(t, primary)
	revision := strings.TrimSpace(string(runGitDir(t, primary, "rev-parse", "HEAD")))
	request := pollReviewNoteBranch(revision, "forest/1/work")
	pushEvidence(t, primary, "request", revision, request, "Iron Forest Builder", "builder@forest.invalid")
	runGitDir(t, primary, "push", "origin", revision+":refs/heads/forest/1/work")
	requestBefore := string(runGit(t, "--git-dir="+origin, "rev-parse", evidenceRequestRefPrefix+revision))
	checks, verdict := writeEvidencePayloads(t, revision, "approve")
	linked := filepath.Join(t.TempDir(), "1-verifier")
	runGitDir(t, primary, "worktree", "add", "--detach", linked, revision)

	foreign, _ := testClone(t)
	seedVerdictRun(t, foreign, "9-verifier")
	seedVerdictRun(t, primary, "1-verifier")
	t.Setenv("FOREST_ROOT", foreign)
	t.Setenv("FOREST_RUN_ID", "1-verifier")
	code, envelope, stderr := decodeEnvelope(t, "publish", "verdict", checks, verdict, "--root", linked, "--json")
	if code != exitOK {
		t.Fatalf("owned linked-worktree publish code=%d stderr=%q", code, stderr)
	}
	var result publishVerdictResult
	decodePayload(t, envelope, &result)
	if result.Status != "published" || result.Revision != revision || result.Verdict != "approve" {
		t.Fatalf("linked-worktree publish result=%#v", result)
	}
	if got := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/master"))); got != revision {
		t.Fatalf("primary=%s, want %s", got, revision)
	}
	if got := string(runGit(t, "--git-dir="+origin, "rev-parse", evidenceRequestRefPrefix+revision)); got != requestBefore {
		t.Fatalf("request evidence changed from %s to %s", requestBefore, got)
	}
	if got := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/forest/1/work"))); got != revision {
		t.Fatalf("request branch=%s, want %s", got, revision)
	}
	if got := fetchEvidenceFile(t, primary, "checks", revision, "checks.json"); !bytes.Equal(got, mustRead(t, checks)) {
		t.Fatalf("published checks=%q", got)
	}
	if got := fetchEvidenceFile(t, primary, "verdict", revision, "verdict.json"); !bytes.Equal(got, mustRead(t, verdict)) {
		t.Fatalf("published verdict=%q", got)
	}
}

func TestPublishVerdictRechecksRunOwnershipAfterConfiguredChecks(t *testing.T) {
	for _, transition := range []struct {
		name      string
		nextRunID string
		nextAgent string
	}{
		{name: "ended"},
		{name: "replaced", nextRunID: "2-verifier", nextAgent: "verifier"},
		{name: "reattributed", nextRunID: "1-verifier", nextAgent: "builder"},
	} {
		t.Run(transition.name, func(t *testing.T) {
			root, origin := testClone(t)
			livePath := liveRunPath(root, "verifier")
			completed := filepath.Join(t.TempDir(), "check-completed")
			t.Setenv("FOREST_TEST_LIVE_RUN", livePath)
			t.Setenv("FOREST_TEST_CHECK_COMPLETE", completed)
			command := `rm "$FOREST_TEST_LIVE_RUN"`
			if transition.nextRunID != "" {
				seedVerdictRun(t, root, transition.nextRunID)
				var replacement liveRunRecord
				if err := json.Unmarshal(mustRead(t, livePath), &replacement); err != nil {
					t.Fatal(err)
				}
				replacement.Agent = transition.nextAgent
				replacementPath := filepath.Join(t.TempDir(), "next-run.json")
				if err := writeLiveRun(replacementPath, replacement); err != nil {
					t.Fatal(err)
				}
				t.Setenv("FOREST_TEST_NEXT_RUN", replacementPath)
				command = `cp "$FOREST_TEST_NEXT_RUN" "$FOREST_TEST_LIVE_RUN"`
			}
			seedVerdictRun(t, root, "1-verifier")
			config := `repo: owner/name
primary: refs/heads/master
agents:
  builder: {poll: "true", interval: 1}
checks:
  - name: test
    run: |
      set -e
      ` + command + `
      printf complete > "$FOREST_TEST_CHECK_COMPLETE"
`
			writeTree(t, root, profileName+"/config.yaml", config)
			runGitDir(t, root, "commit", "-am", "change Run ownership during checks")
			revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			request := pollReviewNoteBranch(revision, "forest/1/work")
			pushEvidence(t, root, "request", revision, request, "Iron Forest Builder", "builder@forest.invalid")
			runGitDir(t, root, "push", "origin", revision+":refs/heads/forest/1/work")
			checks, verdict := writeEvidencePayloads(t, revision, "approve")
			before := string(runGit(t, "--git-dir="+origin, "for-each-ref", "--format=%(refname) %(objectname)"))

			_, err := publishVerdict(context.Background(), publishVerdictInput{
				Root: root, ChecksPath: checks, VerdictPath: verdict, RunID: "1-verifier",
			})
			if err == nil || publishConflict(err) {
				t.Fatalf("publish error=%v, want context refusal after checks", err)
			}
			if got := string(mustRead(t, completed)); got != "complete" {
				t.Fatalf("configured check completion=%q", got)
			}
			if got := string(runGit(t, "--git-dir="+origin, "for-each-ref", "--format=%(refname) %(objectname)")); got != before {
				t.Fatalf("ended or changed Run published remote refs:\nbefore:\n%safter:\n%s", before, got)
			}
		})
	}
}
