package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo builds a throwaway git worktree with one base commit and returns
// its path, base SHA, and a config-level protected-path list.
func initRepo(t *testing.T) (wtDir, baseSHA string, protected []string) {
	t.Helper()
	wtDir = t.TempDir()
	if err := git(wtDir, "init"); err != nil {
		t.Fatal(err)
	}
	if err := git(wtDir, "config", "user.email", "t@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := git(wtDir, "config", "user.name", "test"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "README"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := git(wtDir, "add", "README"); err != nil {
		t.Fatal(err)
	}
	if err := gitCommit(wtDir, "base"); err != nil {
		t.Fatal(err)
	}
	var err error
	baseSHA, err = gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return wtDir, baseSHA, []string{".forest/", "forest.yaml", "agents/", ".opencode/opencode.json"}
}

// writeReport drops a schema-satisfying report.json into the worktree.
func writeReport(t *testing.T, wtDir string) {
	t.Helper()
	rep := `{"summary":"work","changed_files":["main.go"],"notes":""}`
	if err := os.WriteFile(filepath.Join(wtDir, "report.json"), []byte(rep), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGatePassesCleanChange pins the happy path: an uncommitted real change
// with a valid report.json passes and yields the changed file list.
func TestGatePassesCleanChange(t *testing.T) {
	wtDir, baseSHA, protected := initRepo(t)
	if err := os.WriteFile(filepath.Join(wtDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeReport(t, wtDir)

	changed, rep, err := gate(wtDir, baseSHA, protected, "")
	if err != nil {
		t.Fatalf("clean change was rejected: %v", err)
	}
	if len(changed) != 1 || changed[0] != "main.go" {
		t.Fatalf("changed = %v, want [main.go]", changed)
	}
	if rep.Summary != "work" {
		t.Fatalf("report summary = %q, want %q", rep.Summary, "work")
	}
}

// TestGateRejectsCommit pins the no-commit contract: an agent that moved HEAD
// must fail the gate even if it also produced a real change.
func TestGateRejectsCommit(t *testing.T) {
	wtDir, baseSHA, protected := initRepo(t)
	if err := os.WriteFile(filepath.Join(wtDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := git(wtDir, "add", "main.go"); err != nil {
		t.Fatal(err)
	}
	if err := gitCommit(wtDir, "agent commit"); err != nil {
		t.Fatal(err)
	}
	writeReport(t, wtDir)

	if _, _, err := gate(wtDir, baseSHA, protected, ""); err == nil {
		t.Fatal("gate must reject a worktree whose HEAD moved")
	} else if !strings.Contains(err.Error(), "committed") {
		t.Fatalf("error = %v, want a 'committed' diagnosis", err)
	}
}

// TestGateRejectsProtectedPath pins the protected-path contract: touching
// agents/ must fail the gate even though the agent produced work.
func TestGateRejectsProtectedPath(t *testing.T) {
	wtDir, baseSHA, protected := initRepo(t)
	dir := filepath.Join(wtDir, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("model: m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeReport(t, wtDir)

	if _, _, err := gate(wtDir, baseSHA, protected, ""); err == nil {
		t.Fatal("gate must reject a protected path")
	} else if !strings.Contains(err.Error(), "protected") {
		t.Fatalf("error = %v, want a 'protected' diagnosis", err)
	}
}

// TestGateRejectsNoRealChange pins the empty-change contract: a run that only
// produced run artifacts (report.json) must not pass as a change.
func TestGateRejectsNoRealChange(t *testing.T) {
	wtDir, baseSHA, protected := initRepo(t)
	writeReport(t, wtDir)

	if _, _, err := gate(wtDir, baseSHA, protected, ""); err == nil {
		t.Fatal("gate must reject a run with no real changes")
	} else if !strings.Contains(err.Error(), "no real changes") {
		t.Fatalf("error = %v, want a 'no real changes' diagnosis", err)
	}
}

// TestGateRejectsMissingReport pins the report contract: no report.json means
// no pass even when the working tree has a real change.
func TestGateRejectsMissingReport(t *testing.T) {
	wtDir, baseSHA, protected := initRepo(t)
	if err := os.WriteFile(filepath.Join(wtDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := gate(wtDir, baseSHA, protected, ""); err == nil {
		t.Fatal("gate must reject a missing report.json")
	} else if !strings.Contains(err.Error(), "report.json") {
		t.Fatalf("error = %v, want a report.json diagnosis", err)
	}
}
