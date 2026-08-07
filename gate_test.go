package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gateProtectedRepo stages a small repository with its base committed and
// returns the worktree dir and base SHA for a gate call.
func gateProtectedRepo(t *testing.T, seed map[string]string) (string, string) {
	t.Helper()
	wtDir := t.TempDir()
	gitT(t, wtDir, "init")
	for path, body := range seed {
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

// TestGateRejectsProtectedPaths pins the delivery-machine invariant: a build
// run may never change the factory's control plane, so a change that touches
// .forest/, forest.yaml, agents/, or .opencode/opencode.json is refused here,
// before anything a report claims is trusted. A run that edits its own
// instructions, permissions, or composition has left the machine.
func TestGateRejectsProtectedPaths(t *testing.T) {
	for _, path := range []string{
		"forest.yaml",
		"agents/builder/agent.yaml",
		".forest/runs.jsonl",
		".opencode/opencode.json",
	} {
		t.Run(path, func(t *testing.T) {
			wtDir, baseSHA := gateProtectedRepo(t, map[string]string{path: "v1\n"})
			if err := os.WriteFile(filepath.Join(wtDir, path), []byte("v2\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, err := gate(wtDir, baseSHA, ""); err == nil {
				t.Fatalf("gate accepted a change to protected path %q", path)
			}
		})
	}
}

// TestGateAcceptsAChangeOutsideTheControlPlane pins the flip side of the
// protected-path rule: ordinary source work is not rejected, so a build run can
// still land a real change.
func TestGateAcceptsAChangeOutsideTheControlPlane(t *testing.T) {
	wtDir, baseSHA := gateProtectedRepo(t, map[string]string{
		"go.mod":      "module example\n",
		"report.json": `{"summary":"s","changed_files":["work.go"]}`,
	})
	if err := os.WriteFile(filepath.Join(wtDir, "work.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, _, err := gate(wtDir, baseSHA, "")
	if err != nil {
		t.Fatalf("gate rejected ordinary source work: %v", err)
	}
	if len(changed) != 1 || changed[0] != "work.go" {
		t.Fatalf("changed = %v, want [work.go]", changed)
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
