package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

// TestAssertCleanReviewTreeRefusesAnEdit pins the Verifier's clean-tree gate:
// a review that writes only review.json is accepted, while one that edits a
// tracked file is refused with the offending path named, and one that moves
// HEAD is refused too. Checks must back the committed Review revision, never an
// uncommitted experiment.
func TestAssertCleanReviewTreeRefusesAnEdit(t *testing.T) {
	wtDir := t.TempDir()
	gitT(t, wtDir, "init")
	if err := os.WriteFile(filepath.Join(wtDir, "source.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, wtDir, "add", "source.go")
	gitT(t, wtDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
	head, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	before, err := snapshotReviewTree(wtDir)
	if err != nil {
		t.Fatal(err)
	}

	if err := assertCleanReviewTree(wtDir, head, before); err != nil {
		t.Fatalf("a clean tree was refused: %v", err)
	}

	// review.json is the one file the Verifier is expected to write.
	if err := os.WriteFile(filepath.Join(wtDir, "review.json"), []byte(`{"verdict":"approve"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := assertCleanReviewTree(wtDir, head, before); err != nil {
		t.Fatalf("writing only review.json was refused: %v", err)
	}

	// Editing a tracked file must be refused with the path named.
	if err := os.WriteFile(filepath.Join(wtDir, "source.go"), []byte("package main\n// edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = assertCleanReviewTree(wtDir, head, before)
	if err == nil {
		t.Fatal("an edited tracked file was not refused")
	}
	if !strings.Contains(err.Error(), "source.go") {
		t.Fatalf("refusal %q does not name the offending path", err)
	}
}

// TestAssertCleanReviewTreeComparesFullStateBothWays is the regression test for
// the clean-tree gate's blind spot: when a tracked file is already dirty before
// the review (because checks ran in the same worktree), the gate must still
// refuse a review that changes its contents while keeping it dirty, stages it, or
// restores it to clean. A path-set diff that only reads new paths misses all of
// these, because the file is dirty before and dirty (or clean) after, so it never
// appears fresh. Comparing complete pre/post content and index state in both
// directions catches every one.
func TestAssertCleanReviewTreeComparesFullStateBothWays(t *testing.T) {
	wtDir := t.TempDir()
	gitT(t, wtDir, "init")
	if err := os.WriteFile(filepath.Join(wtDir, "source.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, wtDir, "add", "source.go")
	gitT(t, wtDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
	head, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	// A check runs in the worktree first and leaves source.go dirty, so the
	// pre-review snapshot is taken against a tree that is not clean to begin with.
	if err := os.WriteFile(filepath.Join(wtDir, "source.go"), []byte("package main\n// dirty by check\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := snapshotReviewTree(wtDir)
	if err != nil {
		t.Fatal(err)
	}

	// The review edits the already-dirty file: content changes, still dirty.
	if err := os.WriteFile(filepath.Join(wtDir, "source.go"), []byte("package main\n// edited by review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = assertCleanReviewTree(wtDir, head, before)
	if err == nil {
		t.Fatal("editing an already-dirty tracked file was not refused")
	}
	if !strings.Contains(err.Error(), "source.go") {
		t.Fatalf("refusal %q does not name the edited path", err)
	}

	// The review stages the dirty file: index state changed.
	gitT(t, wtDir, "add", "source.go")
	err = assertCleanReviewTree(wtDir, head, before)
	if err == nil {
		t.Fatal("staging an already-dirty tracked file was not refused")
	}
	if !strings.Contains(err.Error(), "source.go") {
		t.Fatalf("refusal %q does not name the staged path", err)
	}

	// The review restores the dirty file to clean: it vanishes from the dirt set,
	// which a forward-only path diff would never notice.
	gitT(t, wtDir, "checkout", "--", "source.go")
	err = assertCleanReviewTree(wtDir, head, before)
	if err == nil {
		t.Fatal("restoring an already-dirty tracked file to clean was not refused")
	}
	if !strings.Contains(err.Error(), "source.go") {
		t.Fatalf("refusal %q does not name the restored path", err)
	}
}

// TestAssertCleanReviewTreeRefusesRenameIntoSide pins that a review cannot hide
// an edit to a tracked file inside the review record. A rename of source.go into
// review.json changes the tracked source, so it must be refused naming the
// source, never accepted as if the run only wrote the record.
func TestAssertCleanReviewTreeRefusesRenameIntoReview(t *testing.T) {
	wtDir := t.TempDir()
	gitT(t, wtDir, "init")
	if err := os.WriteFile(filepath.Join(wtDir, "source.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, wtDir, "add", "source.go")
	gitT(t, wtDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
	head, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	before, err := snapshotReviewTree(wtDir)
	if err != nil {
		t.Fatal(err)
	}

	gitT(t, wtDir, "mv", "source.go", "review.json")
	err = assertCleanReviewTree(wtDir, head, before)
	if err == nil {
		t.Fatal("a rename into review.json was not refused")
	}
	if !strings.Contains(err.Error(), "source.go") {
		t.Fatalf("refusal %q does not name the renamed source", err)
	}
}

// TestAssertCleanReviewTreeRefusesMovedHead pins the HEAD half of the clean-tree
// gate: a review that commits must never yield a Verdict, because the checks
// that back it ran against the pre-move tree.
func TestAssertCleanReviewTreeRefusesMovedHead(t *testing.T) {
	wtDir := t.TempDir()
	gitT(t, wtDir, "init")
	if err := os.WriteFile(filepath.Join(wtDir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, wtDir, "add", "a.txt")
	gitT(t, wtDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
	head, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(wtDir, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, wtDir, "add", "b.txt")
	gitT(t, wtDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "review commit")

	before, err := snapshotReviewTree(wtDir)
	if err != nil {
		t.Fatal(err)
	}

	err = assertCleanReviewTree(wtDir, head, before)
	if err == nil {
		t.Fatal("a moved HEAD was not refused")
	}
	if !strings.Contains(err.Error(), "HEAD") {
		t.Fatalf("refusal %q does not name HEAD", err)
	}
}

// TestAssertCleanReviewTreeRefusesModeTypeChanges is the regression test for the
// clean-tree gate reading only bytes: a chmod, a swap of a tracked regular file
// for an equal-content symlink, and a swap for a FIFO all change git-tracked
// state even though the read bytes are identical or unreadable, so each must be
// refused naming the path. The FIFO case also pins that fingerprinting never
// blocks reading a non-regular object: opening it for content would hang.
func TestAssertCleanReviewTreeRefusesModeTypeChanges(t *testing.T) {
	wantRefused := func(t *testing.T, wtDir, head string, before reviewTreeSnapshot) {
		t.Helper()
		err := assertCleanReviewTree(wtDir, head, before)
		if err == nil {
			t.Fatal("a mode/type change to a tracked path was not refused")
		}
		if !strings.Contains(err.Error(), "mode.txt") {
			t.Fatalf("refusal %q does not name the changed path", err)
		}
	}

	// A chmod changes the tracked mode while leaving the bytes identical.
	t.Run("chmod", func(t *testing.T) {
		wtDir := t.TempDir()
		gitT(t, wtDir, "init")
		if err := os.WriteFile(filepath.Join(wtDir, "mode.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitT(t, wtDir, "add", "mode.txt")
		gitT(t, wtDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
		head, err := gitOut(wtDir, "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		before, err := snapshotReviewTree(wtDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(wtDir, "mode.txt"), 0o755); err != nil {
			t.Fatal(err)
		}
		wantRefused(t, wtDir, head, before)
	})

	// Replacing a tracked regular file with a symlink whose target has the same
	// content must still refuse: the old byte-read followed the link and would
	// have returned the identical bytes, missing the change.
	t.Run("equalContentSymlink", func(t *testing.T) {
		wtDir := t.TempDir()
		gitT(t, wtDir, "init")
		if err := os.WriteFile(filepath.Join(wtDir, "mode.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitT(t, wtDir, "add", "mode.txt")
		gitT(t, wtDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
		head, err := gitOut(wtDir, "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		before, err := snapshotReviewTree(wtDir)
		if err != nil {
			t.Fatal(err)
		}
		peer := filepath.Join(wtDir, "peer.txt")
		if err := os.WriteFile(peer, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(wtDir, "mode.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("peer.txt", filepath.Join(wtDir, "mode.txt")); err != nil {
			t.Fatal(err)
		}
		wantRefused(t, wtDir, head, before)
	})

	// Replacing a tracked regular file with a FIFO changes the object type; the
	// snapshot must name it without ever opening the FIFO, which would block.
	t.Run("fifo", func(t *testing.T) {
		wtDir := t.TempDir()
		gitT(t, wtDir, "init")
		if err := os.WriteFile(filepath.Join(wtDir, "mode.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitT(t, wtDir, "add", "mode.txt")
		gitT(t, wtDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
		head, err := gitOut(wtDir, "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		before, err := snapshotReviewTree(wtDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(wtDir, "mode.txt")); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(filepath.Join(wtDir, "mode.txt"), 0o644); err != nil {
			t.Fatal(err)
		}
		wantRefused(t, wtDir, head, before)
	})
}

// TestAssertCleanReviewTreeRefusesSymlinkedDirectory is the regression test for
// the directory-topology blind spot: a reviewer can move a tracked directory and
// put a symlink to an identical copy in its place, leaving the index and every
// tracked leaf resolving to the same bytes. The leaf fingerprints follow the
// symlinked parent and cannot see the swap, so the snapshot must also fingerprint
// each parent directory node; without it the run would be accepted. Git reports
// the tracked files deleted, but the gate must name the directory on its own.
func TestAssertCleanReviewTreeRefusesSymlinkedDirectory(t *testing.T) {
	wtDir := t.TempDir()
	gitT(t, wtDir, "init")
	if err := os.MkdirAll(filepath.Join(wtDir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "src", "a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "src", "b.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, wtDir, "add", ".")
	gitT(t, wtDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
	head, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	before, err := snapshotReviewTree(wtDir)
	if err != nil {
		t.Fatal(err)
	}

	// Replace src with a symlink to a byte-identical copy so every tracked leaf
	// still resolves to the same content.
	if err := os.MkdirAll(filepath.Join(wtDir, "copy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "copy", "a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "copy", "b.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(wtDir, "src")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("copy", filepath.Join(wtDir, "src")); err != nil {
		t.Fatal(err)
	}

	err = assertCleanReviewTree(wtDir, head, before)
	if err == nil {
		t.Fatal("a tracked directory swapped for a symlink was not refused")
	}
	if !strings.Contains(err.Error(), "src") {
		t.Fatalf("refusal %q does not name the symlinked directory", err)
	}
}

// TestAssertCleanReviewTreeRefusesUntrackedArtifact pins that only review.json
// may appear as a new untracked path: a reviewer cannot smuggle a fixture or an
// artifact into the worktree it later claims to have judged. Creating report.json
// alongside a valid review.json is refused too, because the Verifier contract
// names review.json as the one file to write.
func TestAssertCleanReviewTreeRefusesUntrackedArtifact(t *testing.T) {
	wtDir := t.TempDir()
	gitT(t, wtDir, "init")
	if err := os.WriteFile(filepath.Join(wtDir, "source.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, wtDir, "add", "source.go")
	gitT(t, wtDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
	head, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	before, err := snapshotReviewTree(wtDir)
	if err != nil {
		t.Fatal(err)
	}

	// Only review.json may be added untracked.
	if err := os.WriteFile(filepath.Join(wtDir, "review.json"), []byte(`{"verdict":"approve"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := assertCleanReviewTree(wtDir, head, before); err != nil {
		t.Fatalf("writing only review.json was refused: %v", err)
	}

	// report.json is not a run artifact a review may write: adding it alongside a
	// valid review.json must be refused and name the path even though the review
	// itself is well-formed.
	if err := os.WriteFile(filepath.Join(wtDir, "report.json"), []byte(`{"summary":"s","changed_files":["x"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err = assertCleanReviewTree(wtDir, head, before)
	if err == nil {
		t.Fatal("writing report.json was not refused")
	}
	if !strings.Contains(err.Error(), "report.json") {
		t.Fatalf("refusal %q does not name report.json", err)
	}

	// A non-review untracked file is a refusal naming it.
	if err := os.WriteFile(filepath.Join(wtDir, "fixture.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = assertCleanReviewTree(wtDir, head, before)
	if err == nil {
		t.Fatal("a non-review untracked file was not refused")
	}
	if !strings.Contains(err.Error(), "fixture.txt") {
		t.Fatalf("refusal %q does not name the untracked file", err)
	}
}

// TestAssertCleanReviewTreeRefusesEditedUntrackedFixture pins the untracked blind
// spot the path-set alone leaves open: editing an existing non-ignored fixture in
// place changes no path, so a review that alters what a check reads can slip past
// a membership-only comparison. The snapshot records each untracked path's type
// and content, so the in-place edit must be refused naming it.
func TestAssertCleanReviewTreeRefusesEditedUntrackedFixture(t *testing.T) {
	wtDir := t.TempDir()
	gitT(t, wtDir, "init")
	if err := os.WriteFile(filepath.Join(wtDir, "source.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "fixture.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, wtDir, "add", "source.go")
	gitT(t, wtDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
	head, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	before, err := snapshotReviewTree(wtDir)
	if err != nil {
		t.Fatal(err)
	}

	// The fixture is untracked and present before the review; the review edits it
	// in place. The path set is unchanged, yet the content drift must refuse.
	if err := os.WriteFile(filepath.Join(wtDir, "fixture.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = assertCleanReviewTree(wtDir, head, before)
	if err == nil {
		t.Fatal("editing an existing untracked fixture in place was not refused")
	}
	if !strings.Contains(err.Error(), "fixture.txt") {
		t.Fatalf("refusal %q does not name the edited fixture", err)
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

// TestAssertChecksCleanRefusesTrackedRewriteAllowsScratch pins the check-phase
// clean-tree gate: a check may create untracked scratch output without refusing
// the result, but rewriting or staging a tracked file must refuse the pass note
// naming the offending path, because the green would describe an uncommitted
// edit rather than the Review revision.
func TestAssertChecksCleanRefusesTrackedRewriteAllowsScratch(t *testing.T) {
	wtDir := t.TempDir()
	gitT(t, wtDir, "init")
	if err := os.WriteFile(filepath.Join(wtDir, "source.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, wtDir, "add", "source.go")
	gitT(t, wtDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
	head, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	before, err := snapshotReviewTree(wtDir)
	if err != nil {
		t.Fatal(err)
	}

	// Untracked scratch output (a build artifact) does not change the Revision,
	// so a check that leaves one behind is accepted.
	if err := os.WriteFile(filepath.Join(wtDir, "artifact.bin"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := assertChecksClean(wtDir, head, before); err != nil {
		t.Fatalf("a check that created untracked scratch was refused: %v", err)
	}

	// Rewriting a tracked file refuses, naming the path.
	if err := os.WriteFile(filepath.Join(wtDir, "source.go"), []byte("package main\n// tainted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = assertChecksClean(wtDir, head, before)
	if err == nil {
		t.Fatal("a check that rewrote a tracked file was not refused")
	}
	if !strings.Contains(err.Error(), "source.go") {
		t.Fatalf("refusal %q does not name the rewritten tracked path", err)
	}

	// Staging is refused too, even when the file is restored to HEAD in the
	// working tree: the index entry moved.
	gitT(t, wtDir, "checkout", "--", "source.go")
	gitT(t, wtDir, "add", "source.go")
	err = assertChecksClean(wtDir, head, before)
	if err == nil {
		t.Fatal("a check that staged a tracked file was not refused")
	}
	if !strings.Contains(err.Error(), "source.go") {
		t.Fatalf("refusal %q does not name the staged tracked path", err)
	}
}
