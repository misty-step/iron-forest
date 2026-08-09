package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSingletonLockExcludesASecondDaemon pins the oracle for #42: a manual
// `forest once` must not run while the daemon holds the singleton lock. The
// acquisition is non-blocking and fails with a named lock error, so the
// holder is undisturbed and the runner exits non-zero.
func TestSingletonLockExcludesASecondDaemon(t *testing.T) {
	repoDir := t.TempDir()

	holder, err := acquireSingletonLock(repoDir)
	if err != nil {
		t.Fatalf("first acquisition: %v", err)
	}

	blocked, err := acquireSingletonLock(repoDir)
	if err == nil {
		blocked.Close()
		t.Fatal("second acquisition while the lock is held must fail non-blocking")
	}
	if !strings.Contains(err.Error(), "daemon.lock") {
		t.Fatalf("error %q must name the lock", err)
	}

	// The holder is undisturbed; closing its fd releases the lock.
	if err := holder.Close(); err != nil {
		t.Fatalf("holder close: %v", err)
	}
	released, err := acquireSingletonLock(repoDir)
	if err != nil {
		t.Fatalf("acquisition after release: %v", err)
	}
	released.Close()
}

func TestFactorySourceLockIsCanonicalAndExclusive(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(source, alias); err != nil {
		t.Fatal(err)
	}
	sourcePath := factorySourceLockPath(source)
	if aliasPath := factorySourceLockPath(alias); aliasPath != sourcePath {
		t.Fatalf("factory source aliases use different locks: %q != %q", aliasPath, sourcePath)
	}
	holder, err := holdLock(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer dropLock(holder)
	if contender, err := holdLock(factorySourceLockPath(alias)); err == nil {
		dropLock(contender)
		t.Fatal("second factory source owner acquired the held lock")
	} else if !errors.Is(err, errAdmissionHeld) {
		t.Fatalf("factory source contender = %v, want named busy refusal", err)
	}
}

func TestRunUpdateCheckHonorsFactorySourceLock(t *testing.T) {
	source := newSelfUpdateSource(t)
	holder, err := holdLock(factorySourceLockPath(source))
	if err != nil {
		t.Fatal(err)
	}
	defer dropLock(holder)
	oldFactory, oldExit := factoryDir, exitSelf
	factoryDir = source
	exitCalls := 0
	exitSelf = func(int) { exitCalls++ }
	defer func() { factoryDir, exitSelf = oldFactory, oldExit }()
	managed := t.TempDir()
	runUpdateCheck(managed, new(int32))
	if exitCalls != 0 {
		t.Fatalf("update under held source lock made %d restart calls", exitCalls)
	}
	for _, name := range []string{"forest", "forest.next"} {
		if _, err := os.Stat(filepath.Join(managed, name)); !os.IsNotExist(err) {
			t.Fatalf("update under held source lock created %s: %v", name, err)
		}
	}
}

func TestSelfcheckRejectsUnusedMalformedAgent(t *testing.T) {
	repo := t.TempDir()
	config := "repo: owner/repo\nchecks:\n  - name: test\n    run: \"true\"\n"
	if err := os.WriteFile(filepath.Join(repo, "forest.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"builder", "verifier", "manager"} {
		writeAgentFixture(t, repo, name, name+"-model")
	}
	extra := filepath.Join(repo, DefaultAgentsDir, "unused")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extra, "agent.yaml"),
		[]byte("description: unused\nmodel: unused-model\ndeadline_seconds: 3600\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extra, "instructions.md"), []byte("unused\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := 0
	_, stderr := captureOutput(t, func() { code = cmdSelfcheck(repo) })
	if code != 1 || !strings.Contains(stderr, "commit.name and commit.email are required") {
		t.Fatalf("selfcheck malformed unused agent = code %d stderr %q", code, stderr)
	}
}

func newSelfUpdateSource(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module updatefixture\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	program := "package main\n\nimport \"os\"\n\nvar version = \"fixture\"\n\n" +
		"func main() {\n\t_ = version\n\tif len(os.Args) == 2 && os.Args[1] == \"selfcheck\" { return }\n\tos.Exit(2)\n}\n"
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, src, "init", "-b", "master")
	runGitTest(t, src, "config", "user.name", "update-test")
	runGitTest(t, src, "config", "user.email", "update-test@example.com")
	runGitTest(t, src, "add", ".")
	runGitTest(t, src, "commit", "-m", "fixture")
	return src
}

func TestRunUpdateCheckInstallsAndRequestsRestart(t *testing.T) {
	src := newSelfUpdateSource(t)
	repo := t.TempDir()
	current := filepath.Join(repo, "forest")
	if err := os.WriteFile(current, []byte("current binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldFactory, oldVersion, oldExit := factoryDir, version, exitSelf
	factoryDir, version = src, "old"
	exitCalls := 0
	exitSelf = func(code int) {
		exitCalls++
		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
	}
	t.Cleanup(func() { factoryDir, version, exitSelf = oldFactory, oldVersion, oldExit })

	var drain int32
	runUpdateCheck(repo, &drain)
	if exitCalls != 1 {
		t.Fatalf("restart exits = %d, want one", exitCalls)
	}
	body, err := os.ReadFile(current)
	if err != nil || string(body) == "current binary\n" {
		t.Fatalf("installed binary = %d bytes, %v", len(body), err)
	}
	if _, err := os.Stat(filepath.Join(repo, "forest.prev")); !os.IsNotExist(err) {
		t.Fatalf("atomic install left obsolete forest.prev: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "forest.next")); !os.IsNotExist(err) {
		t.Fatalf("installed update left forest.next: %v", err)
	}
	updates, err := os.ReadFile(filepath.Join(repo, WorkspaceDir, "updates.jsonl"))
	if err != nil || !strings.Contains(string(updates), `"from_sha":"old"`) ||
		!strings.Contains(string(updates), `"status":"swapped"`) {
		t.Fatalf("update record = %q, %v", updates, err)
	}
}

func TestRunUpdateCheckDoesNotSwapWhileDraining(t *testing.T) {
	src := newSelfUpdateSource(t)
	repo := t.TempDir()
	current := filepath.Join(repo, "forest")
	if err := os.WriteFile(current, []byte("current binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldFactory, oldVersion, oldExit := factoryDir, version, exitSelf
	factoryDir, version = src, "old"
	exitCalls := 0
	exitSelf = func(int) { exitCalls++ }
	t.Cleanup(func() { factoryDir, version, exitSelf = oldFactory, oldVersion, oldExit })

	drain := int32(1)
	runUpdateCheck(repo, &drain)
	if exitCalls != 0 {
		t.Fatalf("draining update made %d restart calls", exitCalls)
	}
	body, err := os.ReadFile(current)
	if err != nil || string(body) != "current binary\n" {
		t.Fatalf("current binary while draining = %q, %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "forest.next")); !os.IsNotExist(err) {
		t.Fatalf("draining update retained forest.next: %v", err)
	}
}

func TestRunUpdateCheckRemovesFailedSmoke(t *testing.T) {
	src := newSelfUpdateSource(t)
	program := "package main\n\nimport \"os\"\n\nvar version = \"fixture\"\n\n" +
		"func main() { if len(os.Args) == 2 && os.Args[1] == \"selfcheck\" { os.Exit(2) } }\n"
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, src, "commit", "-qam", "failing smoke")

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "forest"), []byte("current\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldFactory, oldVersion, oldExit := factoryDir, version, exitSelf
	factoryDir, version = src, "old"
	exitSelf = func(int) { t.Fatal("failed smoke requested restart") }
	t.Cleanup(func() { factoryDir, version, exitSelf = oldFactory, oldVersion, oldExit })

	var drain int32
	runUpdateCheck(repo, &drain)
	if _, err := os.Stat(filepath.Join(repo, "forest.next")); !os.IsNotExist(err) {
		t.Fatalf("failed smoke retained forest.next: %v", err)
	}
}

func TestRemoveInterruptedUpdateArtifacts(t *testing.T) {
	repo := t.TempDir()
	for _, name := range []string{"forest.prev", "forest.next", "forest.next.tmp"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("stale"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeInterruptedUpdateArtifacts(repo); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"forest.prev", "forest.next", "forest.next.tmp"} {
		if _, err := os.Stat(filepath.Join(repo, name)); !os.IsNotExist(err) {
			t.Fatalf("interrupted update artifact %s survived cleanup: %v", name, err)
		}
	}
}

func TestBuildSelfRemovesFailedTemporaryBinary(t *testing.T) {
	src := t.TempDir()
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module broken\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("not go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := buildSelf(src, out, "broken"); err == nil {
		t.Fatal("buildSelf accepted invalid source")
	}
	if _, err := os.Stat(filepath.Join(out, "forest.next.tmp")); !os.IsNotExist(err) {
		t.Fatalf("failed build left forest.next.tmp: %v", err)
	}
}
