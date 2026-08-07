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
		"report.json":               `{"summary":"s","changed_files":["forest.yaml"]}`,
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

	changed, _, err := gate(wtDir, baseSHA, "")
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
