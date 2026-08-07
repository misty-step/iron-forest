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

// TestGateRejectsRenameOutOfProtectedPath pins the destructive side of the
// protected-path rule: isProtectedPath alone examines only the post-rename
// destination, so a staged rename that moves a file out of agents/ (or another
// protected path) would dodge the check. gateRejectedPaths inspects the rename
// source too, so the Gate refuses such a rename even though the destination is
// outside the control plane.
func TestGateRejectsRenameOutOfProtectedPath(t *testing.T) {
	wtDir, baseSHA := gateProtectedRepo(t, map[string]string{
		"agents/builder/agent.yaml": "v1\n",
		"report.json":               `{"summary":"s","changed_files":["src/agent.yaml"]}`,
	})
	if err := os.MkdirAll(filepath.Join(wtDir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, wtDir, "mv", "agents/builder/agent.yaml", "src/agent.yaml")
	if _, _, err := gate(wtDir, baseSHA, ""); err == nil {
		t.Fatal("gate accepted a rename that moves a protected path out of the control plane")
	}
}

// TestGateRejectsProtoProtectedSourceDirectly pins the pure helper: a porcelain
// rename whose source is protected is refused even when the destination is not.
func TestGateRejectsProtoProtectedSourceDirectly(t *testing.T) {
	if err := gateRejectedPaths("R  forest.yaml -> src/ok.yaml\n"); err == nil {
		t.Fatal("gateRejectedPaths allowed a rename out of a protected path")
	}
	if err := gateRejectedPaths("R  work.go -> agents/builder/x\n"); err == nil {
		t.Fatal("gateRejectedPaths allowed a rename into a protected path")
	}
	if err := gateRejectedPaths(" M work.go\n A new.go\n"); err != nil {
		t.Fatalf("gateRejectedPaths refused ordinary changes: %v", err)
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

// TestGateRejectsIgnoredProtectedPathAlongsideWork pins the hole the reviewer
// named: /.forest/ is git-ignored, so git status --porcelain never lists a
// change inside it. A run that mutates .forest/foo while also changing an
// ordinary source file would therefore hide the control-plane mutation and pass
// the Gate. The Gate must refuse it when the protected path is ignored, not pass
// it for the wrong reason (because the ignored change left no visible one).
func TestGateRejectsIgnoredProtectedPathAlongsideWork(t *testing.T) {
	wtDir, baseSHA := gateProtectedRepo(t, map[string]string{
		"work.go":     "package main\n",
		".gitignore":  "/.forest/\n",
		"report.json": `{"summary":"s","changed_files":["work.go",".forest/foo"]}`,
	})
	// Change an ordinary tracked file and, in the same run, a git-ignored
	// protected path. Git reports only the tracked file, so the Gate must find
	// the ignored mutation itself.
	if err := os.WriteFile(filepath.Join(wtDir, "work.go"), []byte("package main\n// v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wtDir, ".forest"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, ".forest", "foo"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gate(wtDir, baseSHA, ""); err == nil {
		t.Fatal("gate accepted a change to an ignored protected path (.forest/foo) alongside source work")
	}
}

// TestGateRejectsIgnoredProtectedPathWithoutWork pins the same rule when the
// ignored protected mutation is the only change: it must still be refused, and
// not evade the Gate by leaving no tracked change at all.
func TestGateRejectsIgnoredProtectedPathWithoutWork(t *testing.T) {
	wtDir, baseSHA := gateProtectedRepo(t, map[string]string{
		".gitignore": "/.forest/\n",
	})
	if err := os.MkdirAll(filepath.Join(wtDir, ".forest"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, ".forest", "foo"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gate(wtDir, baseSHA, ""); err == nil {
		t.Fatal("gate accepted a change to an ignored protected path (.forest/foo)")
	}
}

// TestGateAcceptsIgnoredNonProtectedPath pins that the ignored scan does not
// over-reach: a run that only writes an ignored file outside the control plane
// along with real source work is still ordinary, not a rejected run.
func TestGateAcceptsIgnoredNonProtectedPath(t *testing.T) {
	wtDir, baseSHA := gateProtectedRepo(t, map[string]string{
		"work.go":     "package main\n",
		".gitignore":  "*.log\n",
		"report.json": `{"summary":"s","changed_files":["work.go"]}`,
	})
	if err := os.WriteFile(filepath.Join(wtDir, "work.go"), []byte("package main\n// v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "build.log"), []byte("noise\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gate(wtDir, baseSHA, ""); err != nil {
		t.Fatalf("gate rejected an ignored non-protected change: %v", err)
	}
}

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, strings.TrimSpace(string(out)))
	}
}
