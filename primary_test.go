package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newPrimaryBranchFixture(t *testing.T, branch string) (string, string) {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	root := filepath.Join(t.TempDir(), "clone")
	runGit(t, "init", "--bare", "--initial-branch="+branch, origin)
	runGit(t, "clone", origin, root)
	configGit(t, root, "Builder", "builder@forest.invalid")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := []byte(`repo: owner/name
agents:
  builder: {poll: "forest poll builder", interval: 1}
checks:
  - {name: test, run: "true"}
`)
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "file", "forest.yaml")
	runGitDir(t, root, "commit", "-m", "initial")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/"+branch)
	return root, origin
}

func TestResolvePrimaryReadsRemoteHeadSymref(t *testing.T) {
	root, _ := newPrimaryBranchFixture(t, "main")
	cfg, err := loadConfig(configPath(root))
	if err != nil {
		t.Fatal(err)
	}
	ref, source, err := resolvePrimary(context.Background(), root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "refs/heads/main" || source != PrimarySourceRemote {
		t.Fatalf("resolvePrimary() = (%q, %q), want (refs/heads/main, %s)", ref, source, PrimarySourceRemote)
	}
}

func TestResolvePrimaryPrefersConfigOverrideWithoutRemote(t *testing.T) {
	root := t.TempDir()
	config := `repo: owner/name
primary: refs/heads/main
agents:
  builder: {poll: "true", interval: 1}
checks:
  - {name: test, run: "true"}
`
	if err := os.WriteFile(configPath(root), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(configPath(root))
	if err != nil {
		t.Fatal(err)
	}
	// There is no `origin` remote in this checkout; the override must win
	// without touching the remote.
	ref, source, err := resolvePrimary(context.Background(), root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "refs/heads/main" || source != PrimarySourceConfig {
		t.Fatalf("resolvePrimary() = (%q, %q), want (refs/heads/main, %s)", ref, source, PrimarySourceConfig)
	}
}

func TestResolvePrimaryRejectsInvalidConfigRef(t *testing.T) {
	for _, ref := range []string{"main", "refs/heads/", "refs/heads/a b", "refs/heads/a..b", "refs/tags/v1"} {
		cfg := Config{Primary: ref}
		if _, _, err := resolvePrimary(context.Background(), t.TempDir(), cfg); err == nil {
			t.Fatalf("resolvePrimary() accepted invalid override %q", ref)
		}
	}
}

func TestConfigShowPublishesResolvedPrimaryAndSource(t *testing.T) {
	root, _ := newPrimaryBranchFixture(t, "main")
	outcome := runConfigShow(nil, cliFlags{root: root})
	if outcome.Exit != exitOK {
		t.Fatalf("runConfigShow() exit=%d err=%q", outcome.Exit, outcome.ErrText)
	}
	payload, ok := outcome.Data.(configShowPayload)
	if !ok {
		t.Fatalf("runConfigShow() data=%#v, want configShowPayload", outcome.Data)
	}
	if payload.Primary != "refs/heads/main" || payload.PrimarySource != PrimarySourceRemote {
		t.Fatalf("config show primary=%q source=%q, want remote main", payload.Primary, payload.PrimarySource)
	}
	if !strings.Contains(outcome.Human, "primary: refs/heads/main (remote)") {
		t.Fatalf("config show human=%q, want resolved primary", outcome.Human)
	}
}

func TestSelfcheckRefusesWhenPrimaryCannotResolve(t *testing.T) {
	root := t.TempDir()
	config := `repo: owner/name
agents:
  builder: {poll: "true", interval: 1}
checks:
  - {name: test, run: "true"}
`
	if err := os.WriteFile(configPath(root), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	// The checkout has no forest.yaml primary override and no `origin` remote
	// advertises a HEAD symref, so selfcheck must fail rather than guess.
	outcome := runSelfcheck(nil, cliFlags{root: root})
	if outcome.Exit == exitOK {
		t.Fatalf("runSelfcheck() succeeded without a resolvable primary")
	}
	if !strings.Contains(outcome.ErrText, "HEAD symref") {
		t.Fatalf("runSelfcheck() error=%q, want remote HEAD symref refusal", outcome.ErrText)
	}
}

func TestAuditSnapshotsPrimaryBranch(t *testing.T) {
	root, _ := newPrimaryBranchFixture(t, "main")
	heads := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Master != heads {
		t.Fatalf("audited primary tip=%s want %s", result.Master, heads)
	}
	state, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Baseline != heads || state.LastMaster != heads {
		t.Fatalf("audit state baseline=%s last_good=%s want %s", state.Baseline, state.LastMaster, heads)
	}
}

func TestPublishVerdictApproveFastForwardsPrimaryBranch(t *testing.T) {
	root, origin := newPrimaryBranchFixture(t, "main")
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	checks, verdict := writeEvidencePayloads(t, revision, "approve")
	result, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, ChecksPath: checks, VerdictPath: verdict,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "published" || result.Verdict != "approve" {
		t.Fatalf("result=%#v", result)
	}
	got := strings.TrimSpace(string(runGit(t, "--git-dir="+origin, "rev-parse", "refs/heads/main")))
	if got != revision {
		t.Fatalf("main=%s want %s", got, revision)
	}
}
