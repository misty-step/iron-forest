package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	scanEnv.runGeneric = func(string, string, []string) ([]secretFinding, error) { return nil, nil }
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
	scanEnv.runGeneric = func(string, string, []string) ([]secretFinding, error) {
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
	scanEnv.runGeneric = func(_ string, _ string, excludes []string) ([]secretFinding, error) {
		got = excludes
		return nil, nil
	}
	defer func() { scanEnv.lookPath, scanEnv.runGeneric = origLook, origRun }()

	dir := t.TempDir()
	writeTree(t, dir, "forest.secrets.yaml", "exclude:\n  - testdata/fixture.txt\n  - config_test.go\n")
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
	path, err := writeExcludePaths([]string{".git", ".forest", "config_test.go"})
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
		if !strings.Contains(s, want) {
			t.Fatalf("exclusion file should contain %q, got %q", want, s)
		}
	}
}
