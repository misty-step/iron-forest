package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCLIReviewPublishedEvidence(t *testing.T) {
	cases := []struct {
		name          string
		decision      string
		verdict       string
		request       string
		withoutWork   bool
		packed        bool
		verifierRunID string
	}{
		{name: "approve", decision: "approve"},
		{name: "changes", decision: "changes"},
		{name: "bound verdict", decision: "approve", verifierRunID: "verifier-run"},
		{name: "missing verdict"},
		{name: "malformed verdict", verdict: `{"verdict":"approve"}`},
		{name: "malformed request", request: `{"branch":"forest/4/guessed"}`},
		{name: "packed refs", decision: "approve", packed: true},
		{name: "absent work", decision: "approve", withoutWork: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, origin := testClone(t)
			sha := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			work := &WorkReference{System: "linear", ID: "work-id", Key: "MIS-83"}
			request := reviewRequest{Schema: "forest.review-request.v3", Subject: "4", Branch: "forest/4/work", Revision: sha, Time: "2026-09-12T00:00:00Z", RunID: "builder-run", Work: work}
			if tc.withoutWork {
				request.Work = nil
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			payload := string(encoded)
			if tc.request != "" {
				payload = tc.request
			}
			pushEvidence(t, root, "request", sha, payload, "Iron Forest Builder", "builder@forest.invalid")
			pushEvidence(t, root, "checks", sha, fmt.Sprintf(`{"schema":"forest.checks.v1","revision":%q,"results":[{"name":"test","ok":true,"exit":0}],"time":"2026-09-12T00:00:00Z"}`, sha), "Iron Forest Verifier", "verifier@forest.invalid")
			verdict := tc.verdict
			if tc.decision != "" {
				verdict = fmt.Sprintf(`{"schema":"forest.verdict.v1","revision":%q,"verdict":%q,"summary":"Reviewed exact revision","time":"2026-09-12T00:00:00Z"}`, sha, tc.decision)
				if tc.verifierRunID != "" {
					verdict = strings.Replace(verdict, `"schema":`, `"verifier_run_id":"`+tc.verifierRunID+`","schema":`, 1)
				}
			}
			if verdict != "" {
				pushEvidence(t, root, "verdict", sha, verdict, "Iron Forest Verifier", "verifier@forest.invalid")
			}
			if tc.packed {
				runGitDir(t, origin, "pack-refs", "--all", "--prune")
			}
			for _, record := range []RunRecord{
				{RunID: "verifier-run", Agent: "verifier", Started: "2026-09-12T00:00:00Z", Duration: 30, Work: work, Outcome: "success"},
				{RunID: "other-work", Agent: "verifier", Work: &WorkReference{System: "linear", ID: "other-id"}},
				{RunID: "no-work", Agent: "verifier"},
			} {
				if err := AppendRun(root, record); err != nil {
					t.Fatal(err)
				}
			}
			// A local-only ref is not published evidence and cannot add a row.
			runGitDir(t, root, "update-ref", evidenceKindRef("request", strings.Repeat("f", 40)), sha)
			code, envelope, stderr := decodeEnvelope(t, "review", "list", "--json", "--root", root)
			if code != exitOK {
				t.Fatalf("list exit=%d error=%v stderr=%s", code, envelope.Error, stderr)
			}
			var data struct {
				Reviews []reviewRow `json:"reviews"`
			}
			decodePayload(t, envelope, &data)
			if len(data.Reviews) != 1 {
				t.Fatalf("published reviews=%+v", data.Reviews)
			}
			row := data.Reviews[0]
			if row.Revision != sha || row.Decision != tc.decision || row.ChecksState != "readable" {
				t.Fatalf("review=%+v", row)
			}
			wantVerdictState := "missing"
			if tc.decision != "" {
				wantVerdictState = "readable"
			}
			if tc.verdict != "" {
				wantVerdictState = "unreadable"
			}
			if row.VerdictState != wantVerdictState {
				t.Fatalf("verdict state=%s", row.VerdictState)
			}
			if tc.verdict != "" && row.Errors["verdict"] == "" {
				t.Fatal("malformed verdict has no error")
			}
			if tc.request != "" {
				if row.RequestState != "unreadable" || row.Errors["request"] == "" || row.Branch != "" || row.Work != nil {
					t.Fatalf("malformed request inferred fields: %+v", row)
				}
			} else if row.Branch != request.Branch || row.RequestState != "readable" {
				t.Fatalf("request=%+v", row)
			}
			if tc.withoutWork || tc.request != "" {
				if row.Work != nil || row.Runs == nil || len(row.Runs) != 0 {
					t.Fatalf("guessed work/runs: %+v", row)
				}
			} else if !sameWorkReference(row.Work, work) || len(row.Runs) != 1 || row.Runs[0].RunID != "verifier-run" || row.Runs[0].Duration != 30 || row.Runs[0].Outcome != "success" {
				t.Fatalf("matching runs=%+v", row.Runs)
			}
			wantOID := strings.Fields(string(runGitDir(t, root, "ls-remote", "origin", row.RequestRef)))[0]
			if row.RequestCommit == nil || row.RequestCommit.SHA != wantOID || row.RequestCommit.Committer.Email != "builder@forest.invalid" || !validNoteTime(row.RequestCommit.Committer.Time) {
				t.Fatalf("request commit=%+v", row.RequestCommit)
			}
			raw := payloadKeys(t, envelope)["reviews"].([]any)[0].(map[string]any)
			if id, exists := raw["verifier_run_id"]; exists != (tc.verifierRunID != "") || (exists && id != tc.verifierRunID) {
				t.Fatalf("verifier binding=%+v", raw)
			}
			if _, exists := raw["decision"]; exists != (tc.decision != "") {
				t.Fatalf("decision omission=%+v", raw)
			}
			if _, exists := raw["work"]; exists != (request.Work != nil && tc.request == "") {
				t.Fatalf("work omission=%+v", raw)
			}
			code, shown, _ := decodeEnvelope(t, "review", "show", sha, "--json", "--root", root)
			if code != exitOK {
				t.Fatalf("show exit=%d error=%v", code, shown.Error)
			}
			var show struct {
				Review reviewRow `json:"review"`
			}
			decodePayload(t, shown, &show)
			listedJSON, _ := json.Marshal(row)
			shownJSON, _ := json.Marshal(show.Review)
			if string(listedJSON) != string(shownJSON) {
				t.Fatalf("show disagrees with list: %s != %s", listedJSON, shownJSON)
			}
			code, human, _ := captureCLIOutput(t, func() int { return runSurfaceCommand([]string{"review", "show", sha, "--root", root}) })
			if code != exitOK || !strings.Contains(human, sha) || !strings.Contains(human, "verdict="+wantVerdictState) {
				t.Fatalf("human output=%q exit=%d", human, code)
			}
			if tc.verifierRunID != "" && !strings.Contains(human, "verifier_run_id="+tc.verifierRunID) {
				t.Fatalf("human binding absent: %s", human)
			}
			if wantVerdictState == "readable" && tc.verifierRunID == "" && !strings.Contains(human, "verifier=unbound") {
				t.Fatalf("legacy verdict not marked unbound: %s", human)
			}
		})
	}
}

func TestCLIReviewMissingAndInvalidRevision(t *testing.T) {
	root, _ := testClone(t)
	code, envelope, _ := decodeEnvelope(t, "review", "list", "--json", "--root", root)
	if code != exitOK {
		t.Fatalf("empty list error=%v", envelope.Error)
	}
	if rows, ok := payloadKeys(t, envelope)["reviews"].([]any); !ok || len(rows) != 0 {
		t.Fatalf("reviews must be []: %+v", envelope.Data)
	}
	for _, tc := range []struct {
		revision string
		exit     int
	}{{"abc", exitInvalidArg}, {strings.Repeat("a", 40), exitNotFound}} {
		code, _, _ := decodeEnvelope(t, "review", "show", tc.revision, "--json", "--root", root)
		if code != tc.exit {
			t.Fatalf("%s exit=%d want=%d", tc.revision, code, tc.exit)
		}
	}
}

func TestCLIReviewOrphanVerdictChecksCommitter(t *testing.T) {
	for _, trusted := range []bool{true, false} {
		t.Run(fmt.Sprint(trusted), func(t *testing.T) {
			root, _ := testClone(t)
			sha := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			payload := fmt.Sprintf(`{"schema":"forest.verdict.v1","revision":%q,"verdict":"approve","summary":"Reviewed","time":"2026-09-12T00:00:00Z"}`, sha)
			// Author alone is not publication identity; preserve the author
			// while varying only the actual committer.
			t.Setenv("GIT_AUTHOR_NAME", "Iron Forest Verifier")
			t.Setenv("GIT_AUTHOR_EMAIL", "verifier@forest.invalid")
			name, email := "Impostor", "impostor@example.com"
			if trusted {
				name, email = "Iron Forest Verifier", "verifier@forest.invalid"
			}
			t.Setenv("GIT_COMMITTER_NAME", name)
			t.Setenv("GIT_COMMITTER_EMAIL", email)
			writeTree(t, root, "verdict.json", payload)
			runGitDir(t, root, "add", "verdict.json")
			runGitDir(t, root, "commit", "-m", "orphan verdict")
			runGitDir(t, root, "push", "origin", "HEAD:"+evidenceKindRef("verdict", sha))
			code, envelope, _ := decodeEnvelope(t, "review", "show", sha, "--json", "--root", root)
			if code != exitOK {
				t.Fatalf("show exit=%d error=%v", code, envelope.Error)
			}
			var data struct {
				Review reviewRow `json:"review"`
			}
			decodePayload(t, envelope, &data)
			row := data.Review
			if row.Branch != "" || row.Work != nil || row.RequestState != "missing" || row.RequestRef != "" || len(row.Runs) != 0 {
				t.Fatalf("orphan invented request: %+v", row)
			}
			if trusted {
				if row.Decision != "approve" || row.VerdictState != "readable" {
					t.Fatalf("valid orphan verdict lost: %+v", row)
				}
			} else if row.Decision != "" || row.VerdictState != "unreadable" || row.Errors["verdict"] == "" {
				t.Fatalf("forged committer trusted: %+v", row)
			}
		})
	}
}
