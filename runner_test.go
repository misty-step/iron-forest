package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunChecksAllPassing(t *testing.T) {
	cfg := Config{Checks: []Check{
		{Name: "first", Run: "exit 0"},
		{Name: "second", Run: "printf ok"},
	}}

	note, err := runChecks(cfg, t.TempDir(), "run-pass")
	if err != nil {
		t.Fatalf("runChecks returned error: %v", err)
	}
	if note.Status != "pass" {
		t.Fatalf("status = %q, want pass", note.Status)
	}
	if note.RunID != "run-pass" {
		t.Fatalf("run id = %q, want run-pass", note.RunID)
	}
	if _, err := time.Parse(time.RFC3339, note.Time); err != nil {
		t.Fatalf("time = %q is not RFC3339: %v", note.Time, err)
	}
	if len(note.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(note.Results))
	}
	for _, result := range note.Results {
		if result.Code != 0 {
			t.Errorf("%s exit code = %d, want 0", result.Name, result.Code)
		}
	}
}

func TestRunChecksCannotUseOperatorGitHubCredential(t *testing.T) {
	t.Setenv("GH_TOKEN", "operator-token")
	t.Setenv("GITHUB_TOKEN", "operator-token")
	t.Setenv("FOREST_OPERATOR_SECRET", "operator-secret")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")
	t.Setenv("SSH_AUTH_SOCK", "/run/user/1000/keyring/ssh")
	cfg := Config{Checks: []Check{{
		Name: "credential-isolation",
		Run:  `printf 'gh=%s bus=%s ssh=%s\n' "$(command -v gh || true)" "$DBUS_SESSION_BUS_ADDRESS" "$SSH_AUTH_SOCK"; test -z "$GH_TOKEN" && test -z "$GITHUB_TOKEN" && test -z "$FOREST_OPERATOR_SECRET" && test -z "$DBUS_SESSION_BUS_ADDRESS" && test -z "$SSH_AUTH_SOCK" && ! command -v gh >/dev/null 2>&1`,
	}}}
	note, err := runChecks(cfg, t.TempDir(), "run-isolated")
	if err != nil {
		t.Fatalf("runChecks returned error: %v", err)
	}
	if note.Status != "pass" {
		t.Fatalf("status = %q, want pass; output = %q", note.Status, note.Results[0].Output)
	}
	t.Logf("credential lookup output: %q", note.Results[0].Output)
}

func TestRunChecksExecutesDeclaredBuild(t *testing.T) {
	repoDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	note, err := runChecks(Config{Checks: []Check{{
		Name: "build",
		Run:  "mise exec -- go build ./...",
	}}}, repoDir, "run-build")
	if err != nil {
		t.Fatalf("runChecks returned error: %v", err)
	}
	if note.Status != "pass" || len(note.Results) != 1 || note.Results[0].Code != 0 {
		t.Fatalf("declared build = %+v, want pass", note)
	}
}

func TestRunChecksContinuesAfterFailure(t *testing.T) {
	wtDir := t.TempDir()
	later := filepath.Join(wtDir, "later.txt")
	cfg := Config{Checks: []Check{
		{Name: "first", Run: "printf failed; exit 7"},
		{Name: "later", Run: "touch later.txt"},
	}}

	note, err := runChecks(cfg, wtDir, "run-fail")
	if err != nil {
		t.Fatalf("runChecks returned error: %v", err)
	}
	if note.Status != "fail" {
		t.Fatalf("status = %q, want fail", note.Status)
	}
	if len(note.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(note.Results))
	}
	if note.Results[0].Code != 7 || note.Results[1].Code != 0 {
		t.Fatalf("result codes = [%d %d], want [7 0]", note.Results[0].Code, note.Results[1].Code)
	}
	if _, err := os.Stat(later); err != nil {
		t.Fatalf("later check did not run: %v", err)
	}
}

func TestRunChecksKeepsOutputTail(t *testing.T) {
	cfg := Config{Checks: []Check{{
		Name: "output",
		Run:  "head -c 5000 /dev/zero | tr '\\0' x; printf tail-marker",
	}}}

	note, err := runChecks(cfg, t.TempDir(), "run-output")
	if err != nil {
		t.Fatalf("runChecks returned error: %v", err)
	}
	if len(note.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(note.Results))
	}
	output := note.Results[0].Output
	if len(output) != checkOutputTailBytes {
		t.Fatalf("output bytes = %d, want %d", len(output), checkOutputTailBytes)
	}
	if !strings.HasSuffix(output, "tail-marker") {
		t.Fatalf("output tail lost marker: %q", output[len(output)-len("tail-marker"):])
	}
}

func TestRunChecksCommandFailureIsResult(t *testing.T) {
	cfg := Config{Checks: []Check{{Name: "missing", Run: "command-that-does-not-exist-iron-forest"}}}

	note, err := runChecks(cfg, t.TempDir(), "run-missing")
	if err != nil {
		t.Fatalf("runChecks returned error for failed command: %v", err)
	}
	if note.Status != "fail" {
		t.Fatalf("status = %q, want fail", note.Status)
	}
	if len(note.Results) != 1 || note.Results[0].Code == 0 {
		t.Fatalf("results = %+v, want one nonzero result", note.Results)
	}
}

func TestChecksSummaryNamesFailures(t *testing.T) {
	got := checksSummary(checksNote{
		Status: "fail",
		Results: []checkResult{
			{Name: "build", Code: 0},
			{Name: "test", Code: 1},
		},
	})
	if got != "checks fail: test(exit 1)" {
		t.Fatalf("summary = %q, want checks fail: test(exit 1)", got)
	}
}
