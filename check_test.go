package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// checkRun mirrors the subset of a GitHub check-run the factory reads.
type checkRun struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Name       string `json:"name"`
}

// rollupStub returns an issue/PR/API responder that serves the given check-runs
// through the same jq projections ghJSON would run, so both checkRollup and
// failingChecks stay offline.
func rollupStub(t *testing.T, runsJSON string) {
	t.Helper()
	if strings.TrimSpace(runsJSON) == "" {
		stubGH(t, func(args []string) []byte {
			if args[0] == "api" {
				return []byte{}
			}
			return nil
		})
		return
	}
	var runs []checkRun
	if err := json.Unmarshal([]byte(runsJSON), &runs); err != nil {
		t.Fatalf("bad runs fixture: %v", err)
	}
	stubGH(t, func(args []string) []byte {
		if args[0] != "api" {
			return nil
		}
		query := args[3]
		if strings.Contains(query, "select(") {
			var lines []string
			for _, r := range runs {
				if r.Status == "completed" && r.Conclusion != "success" &&
					r.Conclusion != "neutral" && r.Conclusion != "skipped" {
					lines = append(lines, r.Name+": "+r.Conclusion)
				}
			}
			out, _ := json.Marshal(lines)
			return out
		}
		out, _ := json.Marshal(runs)
		return out
	})
}

// TestCheckRollupSummary pins the rollup verdicts on representative check-run
// sets: each must round-trip through checkRollup as pass/fail/pending/"".
func TestCheckRollupSummary(t *testing.T) {
	cases := []struct {
		name string
		runs string
		want string
	}{
		{"no checks", `[]`, ""},
		{"third-party only, green", `[
			{"name":"CodeRabbit","status":"completed","conclusion":"success"}
		]`, ""},
		{"ci green", `[
			{"name":"CI","status":"completed","conclusion":"success"}
		]`, "pass"},
		{"ci failing", `[
			{"name":"Go Test","status":"completed","conclusion":"failure"}
		]`, "fail"},
		{"ci pending", `[
			{"name":"CI","status":"in_progress","conclusion":null}
		]`, "pending"},
		{"fail beats pending", `[
			{"name":"CI","status":"completed","conclusion":"failure"},
			{"name":"Lint","status":"queued","conclusion":null}
		]`, "fail"},
		{"skipped is green", `[
			{"name":"CI","status":"completed","conclusion":"skipped"}
		]`, "pass"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rollupStub(t, tc.runs)
			got, err := checkRollup("owner/repo", "deadbeef")
			if err != nil {
				t.Fatalf("checkRollup: %v", err)
			}
			if got != tc.want {
				t.Fatalf("rollup = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCheckRollupEmptyOutput pins the no-CI-api result: an empty JSON payload
// (no check-runs key) must read as "" rather than an error.
func TestCheckRollupEmptyOutput(t *testing.T) {
	rollupStub(t, "")
	got, err := checkRollup("owner/repo", "deadbeef")
	if err != nil {
		t.Fatalf("checkRollup: %v", err)
	}
	if got != "" {
		t.Fatalf("rollup = %q, want empty for no check-runs", got)
	}
}

// TestFailingChecksListsBrokenRuns pins failingChecks: it must name the broken
// check and only the broken one.
func TestFailingChecksListsBrokenRuns(t *testing.T) {
	rollupStub(t, `[
		{"name":"Go Test","status":"completed","conclusion":"failure"},
		{"name":"Vet","status":"completed","conclusion":"success"}
	]`)
	got, err := failingChecks("owner/repo", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	want := "Go Test: failure"
	if got != want {
		t.Fatalf("failingChecks = %q, want %q", got, want)
	}
}
