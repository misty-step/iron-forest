package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGateRejectsRenameOfProtectedPath pins the rename guard: parseChanged
// keeps only the destination of a rename, so the protected list never sees a
// path being moved away. A rename whose original path is protected must be
// rejected by the gate, or the protection would be trivially dodged.
func TestGateRejectsRenameOfProtectedPath(t *testing.T) {
	wtDir := t.TempDir()
	gitT(t, wtDir, "init")
	if err := os.WriteFile(filepath.Join(wtDir, "cfg.yaml"), []byte("k: v\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "report.json"),
		[]byte(`{"summary":"s","changed_files":["cfg2.yaml"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, wtDir, "add", ".")
	gitT(t, wtDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
	baseSHA, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	// Move the protected path somewhere new; the change stays uncommitted.
	gitT(t, wtDir, "mv", "cfg.yaml", "cfg2.yaml")

	protected := []string{"cfg.yaml"}
	if _, _, err := gate(wtDir, baseSHA, protected, ""); err == nil {
		t.Fatal("gate accepted a rename of a protected path")
	} else if !strings.Contains(err.Error(), "protected") {
		t.Fatalf("gate error = %q, want it to mention a protected path", err)
	}
}

// TestParseChangedExposesRenameSource pins that parseChanged surfaces the
// original path of a rename so the gate can guard it.
func TestParseChangedExposesRenameSource(t *testing.T) {
	changed, renamed := parseChanged("R  forest.yaml -> forest2.yaml\nA  new.go\n")
	if len(changed) != 2 || changed[0] != "forest2.yaml" || changed[1] != "new.go" {
		t.Fatalf("changed = %v, want [forest2.yaml new.go]", changed)
	}
	if len(renamed) != 1 || renamed[0] != "forest.yaml" {
		t.Fatalf("renamed = %v, want [forest.yaml]", renamed)
	}
	if !isProtected(renamed[0], defaultConfig().Protected) {
		t.Fatalf("original path %q of rename is not considered protected", renamed[0])
	}
}

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, strings.TrimSpace(string(out)))
	}
}
