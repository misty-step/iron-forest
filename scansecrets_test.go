package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func writeTree(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestScanSecretsMissingScannerFailsClosed proves the check fails closed when
// the generic scanner is absent and names the missing tool rather than hiding
// it, so a host without the scanner can never silently skip the scan.
func TestScanSecretsMissingScannerFailsClosed(t *testing.T) {
	orig := scanEnv.lookPath
	scanEnv.lookPath = func(string, string) (string, error) {
		return "", errors.New("executable file not found")
	}
	defer func() { scanEnv.lookPath = orig }()

	outcome := runScanSecrets([]string{t.TempDir()}, cliFlags{})
	if outcome.Exit == exitOK {
		t.Fatal("expected a failure when the generic scanner is missing")
	}
	if !strings.Contains(outcome.ErrText, secretScanner) {
		t.Fatalf("error should name the missing tool %q, got %q", secretScanner, outcome.ErrText)
	}
}

func TestScanSecretsCleanWorktreePasses(t *testing.T) {
	origLook, origRun := scanEnv.lookPath, scanEnv.runGeneric
	scanEnv.lookPath = func(string, string) (string, error) { return "stub", nil }
	scanEnv.runGeneric = func(context.Context, string, string, []string) ([]secretFinding, error) { return nil, nil }
	defer func() { scanEnv.lookPath, scanEnv.runGeneric = origLook, origRun }()

	dir := t.TempDir()
	writeTree(t, dir, "README.md", "no secrets here\n")
	outcome := runScanSecrets([]string{dir}, cliFlags{})
	if outcome.Exit != exitOK {
		t.Fatalf("clean worktree should pass, got exit %d: %s", outcome.Exit, outcome.ErrText)
	}
}

func TestScanSecretsFindingFails(t *testing.T) {
	origLook, origRun := scanEnv.lookPath, scanEnv.runGeneric
	scanEnv.lookPath = func(string, string) (string, error) { return "stub", nil }
	scanEnv.runGeneric = func(context.Context, string, string, []string) ([]secretFinding, error) {
		return []secretFinding{{Path: ".opencode/agents/builder.md", Rule: "Mint"}}, nil
	}
	defer func() { scanEnv.lookPath, scanEnv.runGeneric = origLook, origRun }()

	outcome := runScanSecrets([]string{t.TempDir()}, cliFlags{})
	if outcome.Exit == exitOK {
		t.Fatal("a worktree with leaked credential material must fail the check")
	}
	if !strings.Contains(outcome.ErrText, ".opencode/agents/builder.md") {
		t.Fatalf("finding should name the offending path, got %q", outcome.ErrText)
	}
	if strings.Contains(outcome.ErrText, "sk-or-abc") {
		t.Fatalf("finding leaked the credential: %q", outcome.ErrText)
	}
}

func TestScanSecretsLoadsFixtureExclusions(t *testing.T) {
	origLook, origRun := scanEnv.lookPath, scanEnv.runGeneric
	scanEnv.lookPath = func(string, string) (string, error) { return "stub", nil }
	var got []string
	scanEnv.runGeneric = func(_ context.Context, _ string, _ string, excludes []string) ([]secretFinding, error) {
		got = excludes
		return nil, nil
	}
	defer func() { scanEnv.lookPath, scanEnv.runGeneric = origLook, origRun }()

	dir := t.TempDir()
	writeTree(t, dir, "forest.secrets.yaml", "exclude:\n  - testdata/fixture.txt\n  - config_test.go\n")
	writeTree(t, dir, "testdata/fixture.txt", "validated fixture placeholder\n")
	writeTree(t, dir, "config_test.go", "package main\n")
	if _, err := scanSecretsTree(dir); err != nil {
		t.Fatal(err)
	}
	want := []string{".git", ".forest", "testdata/fixture.txt", "config_test.go"}
	for _, pattern := range want {
		found := false
		for _, g := range got {
			if g == pattern {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("scanner should have received exclusion %q, got %v", pattern, got)
		}
	}
}

func TestScanSecretsRefusesRepositoryScanner(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, secretScanner)
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho PLANTED\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	outcome := runScanSecrets([]string{dir}, cliFlags{})
	if outcome.Exit == exitOK || !strings.Contains(outcome.ErrText, "refuse repository executable") {
		t.Fatalf("code=%d err=%q", outcome.Exit, outcome.ErrText)
	}
}

func TestScanSecretsMalformedScannerOutputFailsClosed(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(t.TempDir(), secretScanner)
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'not-json\\n'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(script)+string(os.PathListSeparator)+os.Getenv("PATH"))
	outcome := runScanSecrets([]string{dir}, cliFlags{})
	if outcome.Exit == exitOK || !strings.Contains(outcome.ErrText, "parse") {
		t.Fatalf("code=%d err=%q", outcome.Exit, outcome.ErrText)
	}
}

// TestWriteExcludePaths confirms the exclusions reach the scanner through a file
// it reads, so a legitimate fixture cannot fail the generic pass.
func TestWriteExcludePaths(t *testing.T) {
	dir := t.TempDir()
	path, err := writeExcludePaths(dir, []string{".git", ".forest", "config_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{".git", ".forest", "config_test.go"} {
		if !strings.Contains(s, excludePathRule(dir, want)+"\n") {
			t.Fatalf("exclusion file should contain the anchored rule for %q, got %q", want, s)
		}
	}
}

func TestLoadSecretsConfigRejectsGlobExclusion(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, "forest.secrets.yaml", "exclude:\n  - '*'\n")
	_, err := loadSecretsConfig(dir)
	if err == nil || !strings.Contains(err.Error(), "glob metacharacters") {
		t.Fatalf("error=%v, want a glob metacharacter rejection", err)
	}
}

func TestLoadSecretsConfigRejectsParentExclusion(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, "forest.secrets.yaml", "exclude:\n  - ../x\n")
	_, err := loadSecretsConfig(dir)
	if err == nil || !strings.Contains(err.Error(), "parent path element") {
		t.Fatalf("error=%v, want a parent path rejection", err)
	}
}

func TestLoadSecretsConfigRejectsMissingExclusion(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, "forest.secrets.yaml", "exclude:\n  - missing.txt\n")
	_, err := loadSecretsConfig(dir)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error=%v, want a missing path rejection", err)
	}
}

func TestLoadSecretsConfigRejectsRootExclusion(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, "forest.secrets.yaml", "exclude:\n  - .\n")
	_, err := loadSecretsConfig(dir)
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("error=%v, want a scanned-tree-root rejection", err)
	}
}

func TestLoadSecretsConfigRejectsSymlinkExclusion(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "credentials.txt")
	link := filepath.Join(dir, "innocent_link")
	if err := os.WriteFile(target, []byte("sk-or-v1-abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, "forest.secrets.yaml", "exclude:\n  - innocent_link\n")
	_, err := loadSecretsConfig(dir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error=%v, want a symlink exclusion rejection", err)
	}
}

func TestLoadSecretsConfigRejectsSymlinkComponentExclusion(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, "real/credentials.txt", "sk-or-v1-abcdef")
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, "forest.secrets.yaml", "exclude:\n  - link/credentials.txt\n")
	_, err := loadSecretsConfig(dir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error=%v, want a symlink path component rejection", err)
	}
}

func TestLoadSecretsConfigRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, "forest.secrets.yaml", "exclude:\n  - config_test.go\nextra: true\n")
	writeTree(t, dir, "config_test.go", "package main\n")
	_, err := loadSecretsConfig(dir)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error=%v, want an unknown field rejection", err)
	}
}

func TestExcludePathRuleMatchesOnlyExactPath(t *testing.T) {
	dir := t.TempDir()
	rule := excludePathRule(dir, "a")
	for _, target := range []string{
		filepath.Join(dir, "a"),
		filepath.Join(dir, "config_test.go"),
		filepath.Join(dir, "a.txt"),
		filepath.Join(dir, "sub", "a"),
	} {
		matched := regexp.MustCompile(rule).MatchString(target)
		want := target == filepath.Join(dir, "a")
		if matched != want {
			t.Fatalf("rule %q matched=%t for %q, want %t", rule, matched, target, want)
		}
	}
}

func TestExcludePathRuleDoesNotResolveSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "credentials.txt")
	link := filepath.Join(dir, "innocent_link")
	if err := os.WriteFile(target, []byte("sk-or-v1-abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	rule := excludePathRule(dir, "innocent_link")
	configured := filepath.Join(dir, "innocent_link")
	if !regexp.MustCompile(rule).MatchString(configured) {
		t.Fatalf("rule %q should match configured symlink path %q", rule, configured)
	}
	if regexp.MustCompile(rule).MatchString(target) {
		t.Fatalf("rule %q must not resolve to symlink target %q", rule, target)
	}
}

func TestScanSecretsTreeRootRefusesManagedCheckoutScanner(t *testing.T) {
	managed := t.TempDir()
	scanned := t.TempDir()
	script := filepath.Join(managed, secretScanner)
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho PLANTED\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", managed+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := scanSecretsTreeRoot(context.Background(), managed, scanned)
	if err == nil || !strings.Contains(err.Error(), "refuse repository executable") {
		t.Fatalf("error=%v, want the managed checkout scanner to be refused", err)
	}
}

func TestRunTrufflehogCancelsBlockingScanner(t *testing.T) {
	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	t.Setenv("HEARTBEAT", heartbeat)
	bin := filepath.Join(t.TempDir(), secretScanner)
	script := "#!/bin/sh\nwhile :; do printf x >> \"$HEARTBEAT\"; /bin/sleep 0.02; done\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runTrufflehog(ctx, bin, t.TempDir(), nil)
		done <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(heartbeat); err == nil && len(b) > 0 {
			cancel()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("blocking scanner did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runTrufflehog did not return after cancel")
	}
}
