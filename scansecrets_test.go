package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree writes a file at rel under dir with the given content.
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

func TestScanSecretsContentDetectsMintMarker(t *testing.T) {
	dir := t.TempDir()
	// The live failure: a rendered agent declaration carrying a marker lands in
	// an untracked path that .gitignore could not help. The scan reads it anyway.
	writeTree(t, dir, ".opencode/agents/builder.md",
		"# builder\nmodel: openrouter-mint\nheader: __mint.exa.default__\n")

	findings, err := scanSecretsContent(dir, defaultScanExcludes)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 {
		t.Fatal("expected a finding for a Mint marker, got none")
	}
	if findings[0].Rule != "mint-marker" || !strings.Contains(findings[0].Match, "__mint.exa.default__") {
		t.Fatalf("unexpected finding: %+v", findings[0])
	}
}

func TestScanSecretsContentDetectsMintProxyHost(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, "rendered.md",
		"proxy: http://mint.tail5f5eb4.ts.net:4949/proxy/https/openrouter.ai/api/v1\n")

	findings, err := scanSecretsContent(dir, defaultScanExcludes)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 {
		t.Fatal("expected a finding for the Mint proxy host, got none")
	}
	if findings[0].Rule != "mint-proxy" {
		t.Fatalf("unexpected finding: %+v", findings[0])
	}
}

func TestScanSecretsContentExcludesFixture(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, ".opencode/opencode.json",
		`{"apiKey":"__mint.openrouter.ironforest__","baseURL":"http://mint.tail5f5eb4.ts.net:4949/proxy"}`)
	writeTree(t, dir, "agents/builder/agent.yaml",
		"url: http://mint.tail5f5eb4.ts.net:4949/proxy/https/mcp.exa.ai/mcp\nheader: __mint.exa.default__\n")

	excludes := append(append([]string(nil), defaultScanExcludes...),
		".opencode/opencode.json", "agents/")
	findings, err := scanSecretsContent(dir, excludes)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected excluded fixtures to pass, got %+v", findings)
	}
}

func TestScanGenericFailsClosedWhenScannerMissing(t *testing.T) {
	orig := scanEnv.lookPath
	scanEnv.lookPath = func(string) (string, error) {
		return "", errors.New("executable file not found")
	}
	defer func() { scanEnv.lookPath = orig }()

	_, err := scanGeneric(t.TempDir())
	if err == nil {
		t.Fatal("expected a failure when the generic scanner is missing")
	}
	if !strings.Contains(err.Error(), secretScanner) {
		t.Fatalf("error should name the missing tool %q, got %q", secretScanner, err.Error())
	}
}

// TestCmdScanSecretsEndToEnd drives the command without a generic scanner by
// stubbing the scanner as present and clean, so a content leak alone fails the
// gate and a clean tree passes.
func TestCmdScanSecretsEndToEnd(t *testing.T) {
	origLook, origRun := scanEnv.lookPath, scanEnv.runGeneric
	scanEnv.lookPath = func(string) (string, error) { return "stub", nil }
	scanEnv.runGeneric = func(bin, dir string) ([]secretFinding, error) { return nil, nil }
	defer func() { scanEnv.lookPath, scanEnv.runGeneric = origLook, origRun }()

	dir := t.TempDir()
	if code := cmdScanSecrets([]string{dir}); code != 0 {
		t.Fatalf("clean tree should pass, got exit %d", code)
	}

	writeTree(t, dir, "leak.md", "token __mint.openrouter.ironforest__ here")
	if code := cmdScanSecrets([]string{dir}); code == 0 {
		t.Fatal("worktree with a Mint marker should fail the gate")
	}
}
