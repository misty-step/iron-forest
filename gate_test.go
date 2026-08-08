package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGateAcceptsAChangeToItsOwnDeclarations pins the decision in ADR 0003: the
// factory may work on its own configuration and agent declarations. A run that
// edits them reaches the gate on the strength of its change, and independent
// review on the exact commit decides whether it lands.
func TestGateAcceptsAChangeToItsOwnDeclarations(t *testing.T) {
	wtDir := t.TempDir()
	gitT(t, wtDir, "init")
	if err := os.MkdirAll(filepath.Join(wtDir, "agents", "builder"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"forest.yaml":               "repo: o/r\n",
		"agents/builder/agent.yaml": "model: m\n",
		"report.json":               `{"summary":"s","changed_files":["agents/builder/agent.yaml"]}`,
	} {
		if err := os.WriteFile(filepath.Join(wtDir, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitT(t, wtDir, "add", ".")
	gitT(t, wtDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
	baseSHA, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "agents", "builder", "agent.yaml"),
		[]byte("model: m2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, _, err := gate(wtDir, baseSHA, "", "")
	if err != nil {
		t.Fatalf("gate rejected a change to the factory's own declarations: %v", err)
	}
	if len(changed) != 1 || changed[0] != "agents/builder/agent.yaml" {
		t.Fatalf("changed = %v, want [agents/builder/agent.yaml]", changed)
	}
}

// TestParseChangedKeepsRenameDestination pins that a rename reports the path
// that now exists, so the pull request body names real files.
func TestParseChangedKeepsRenameDestination(t *testing.T) {
	changed := parseChanged("R  forest.yaml -> forest2.yaml\nA  new.go\n")
	if len(changed) != 2 || changed[0] != "forest2.yaml" || changed[1] != "new.go" {
		t.Fatalf("changed = %v, want [forest2.yaml new.go]", changed)
	}
}

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, strings.TrimSpace(string(out)))
	}
}

// TestParseChangedKeepsFirstPathIntact pins the column contract. Porcelain
// leaves the first column blank for a change that is not staged, so a modified
// tracked file arrives as " M path". Trimming that blank shifts the fields left
// and silently eats the first character of the path, which produced a wrong
// changed-file list and let the first modified file dodge any path matching.
func TestParseChangedKeepsFirstPathIntact(t *testing.T) {
	changed := parseChanged(" M agents/builder/agent.yaml\n M forest.yaml\n")
	want := []string{"agents/builder/agent.yaml", "forest.yaml"}
	if len(changed) != len(want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}
	for i := range want {
		if changed[i] != want[i] {
			t.Fatalf("changed[%d] = %q, want %q", i, changed[i], want[i])
		}
	}
}

// gateBaseRepo initialises a git repo, commits a base with the given files, and
// returns the worktree dir and its base HEAD. The caller then introduces a
// working-tree change and calls gate.
func gateBaseRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	wtDir := t.TempDir()
	gitT(t, wtDir, "init")
	for path, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(wtDir, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wtDir, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitT(t, wtDir, "add", ".")
	gitT(t, wtDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
	baseSHA, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return wtDir, baseSHA
}

// writeReport writes a report.json into the worktree as a run artifact.
func writeReport(t *testing.T, wtDir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wtDir, "report.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGateRefusesReportNamingUnchangedFile pins that a report claiming a file
// that did not change is refused and the message names that file.
func TestGateRefusesReportNamingUnchangedFile(t *testing.T) {
	wtDir, baseSHA := gateBaseRepo(t, map[string]string{
		"forest.yaml": "repo: o/r\n",
		"a.go":        "package a\n",
		"b.go":        "package b\n",
	})
	if err := os.WriteFile(filepath.Join(wtDir, "b.go"), []byte("package b2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeReport(t, wtDir, `{"summary":"s","changed_files":["a.go"]}`)

	_, _, err := gate(wtDir, baseSHA, "", "")
	if err == nil {
		t.Fatal("gate accepted a report naming an unchanged file")
	}
	if !strings.Contains(err.Error(), "a.go") || !strings.Contains(err.Error(), "did not change") {
		t.Fatalf("error %q did not name the unchanged file a.go", err)
	}
}

// TestGateRefusesReportOmittingChangedFile pins that a report that fails to
// name a changed file is refused and the message names that omitted file.
func TestGateRefusesReportOmittingChangedFile(t *testing.T) {
	wtDir, baseSHA := gateBaseRepo(t, map[string]string{
		"forest.yaml": "repo: o/r\n",
		"b.go":        "package b\n",
		"c.go":        "package c\n",
	})
	if err := os.WriteFile(filepath.Join(wtDir, "b.go"), []byte("package b2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "c.go"), []byte("package c2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeReport(t, wtDir, `{"summary":"s","changed_files":["b.go"]}`)

	_, _, err := gate(wtDir, baseSHA, "", "")
	if err == nil {
		t.Fatal("gate accepted a report omitting a changed file")
	}
	if !strings.Contains(err.Error(), "c.go") || !strings.Contains(err.Error(), "omits") {
		t.Fatalf("error %q did not name the omitted file c.go", err)
	}
}

// TestGateNamesTraceTailWhenReportMissing pins that a run with no report fails
// with the trace tail so the operator sees where it stopped, not only
// "report.json missing".
func TestGateNamesTraceTailWhenReportMissing(t *testing.T) {
	wtDir, baseSHA := gateBaseRepo(t, map[string]string{
		"forest.yaml": "repo: o/r\n",
		"b.go":        "package b\n",
	})
	if err := os.WriteFile(filepath.Join(wtDir, "b.go"), []byte("package b2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracePath := filepath.Join(t.TempDir(), "agent.jsonl")
	if err := os.WriteFile(tracePath, []byte("{\"step\":1}\n{\"step\":2}\n{\"step\":3}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := gate(wtDir, baseSHA, "", tracePath)
	if err == nil {
		t.Fatal("gate accepted a run with no report")
	}
	if !strings.Contains(err.Error(), "report.json missing") {
		t.Fatalf("error %q did not name report.json missing", err)
	}
	if !strings.Contains(err.Error(), `"step":3`) {
		t.Fatalf("error %q did not include the trace tail", err)
	}
}

// TestCrossCheckNormalisesPaths pins that path normalisation lets a report and
// the porcelain agree on the same file despite a slash or dot difference.
func TestCrossCheckNormalisesPaths(t *testing.T) {
	if err := crossCheck([]string{"./a.go", "b/c.go"}, []string{"a.go", "b\\c.go"}); err != nil {
		t.Fatalf("crossCheck rejected a normalisable match: %v", err)
	}
}
