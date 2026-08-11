package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunChecksHardStopKillsDescendants(t *testing.T) {
	defer func() {
		runProcesses.Lock()
		runProcesses.stopping = false
		runProcesses.Unlock()
	}()
	workDir := t.TempDir()
	heartbeat := filepath.Join(workDir, "heartbeat")
	stop := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if body, err := os.ReadFile(heartbeat); err == nil && len(body) > 0 {
				break
			}
			if time.Now().After(deadline) {
				stop <- errors.New("check descendant did not start")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		if errs := hardStopRunCommands(); len(errs) != 0 {
			stop <- fmt.Errorf("hard stop: %v", errs)
			return
		}
		stop <- nil
	}()

	note, err := runChecks(Config{Checks: []Check{{
		Name: "blocking",
		Run:  "setsid /bin/sh -c 'while :; do printf x >> heartbeat; sleep 0.05; done' & wait",
	}}}, workDir, "run-hard-stop")
	if err != nil {
		t.Fatalf("runChecks: %v", err)
	}
	if stopErr := <-stop; stopErr != nil {
		t.Fatal(stopErr)
	}
	if note.Status != "fail" {
		t.Fatalf("hard-stopped check status = %q, want fail", note.Status)
	}
	before, err := os.ReadFile(heartbeat)
	if err != nil || len(before) == 0 {
		t.Fatalf("check descendant produced no heartbeat: %d bytes, %v", len(before), err)
	}
	time.Sleep(300 * time.Millisecond)
	after, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("check descendant survived hard stop: heartbeat grew from %d to %d bytes", len(before), len(after))
	}
}

func TestRunChecksAllPassing(t *testing.T) {
	cfg := Config{Checks: []Check{
		{Name: "first", Run: "exit 0"},
		{Name: "second", Run: "printf ok"},
	}}

	note, err := runChecks(cfg, t.TempDir(), "run-pass")
	if err != nil {
		t.Fatalf("runChecks returned error: %v", err)
	}
	if note.Status != "pass" {
		t.Fatalf("status = %q, want pass", note.Status)
	}
	if note.RunID != "run-pass" {
		t.Fatalf("run id = %q, want run-pass", note.RunID)
	}
	if _, err := time.Parse(time.RFC3339, note.Time); err != nil {
		t.Fatalf("time = %q is not RFC3339: %v", note.Time, err)
	}
	if len(note.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(note.Results))
	}
	for _, result := range note.Results {
		if result.Code != 0 {
			t.Errorf("%s exit code = %d, want 0", result.Name, result.Code)
		}
	}
}

func TestRunChecksCannotUseOperatorCredentials(t *testing.T) {
	t.Setenv("GH_TOKEN", "operator-token")
	t.Setenv("GITHUB_TOKEN", "operator-token")
	t.Setenv("FOREST_OPERATOR_SECRET", "operator-secret")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")
	t.Setenv("SSH_AUTH_SOCK", "/run/user/1000/keyring/ssh")
	t.Setenv("GOMODCACHE", "/")
	t.Setenv("GOCACHE", "/home/operator/.cache")

	operatorHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	hostProbeDir, err := os.MkdirTemp(operatorHome, ".forest-sandbox-probe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(hostProbeDir) })
	hostProbe := filepath.Join(hostProbeDir, "host-only")
	if err := os.WriteFile(hostProbe, []byte("not for the child\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	varProbe := filepath.Join("/var/tmp", fmt.Sprintf("forest-sandbox-probe-%d", os.Getpid()))
	if err := os.WriteFile(varProbe, []byte("not for the child\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(varProbe) })

	hostBin := t.TempDir()
	hostGH := writeDriver(t, hostBin, "gh", "#!/bin/sh\n[ -S /run/user/$(id -u)/bus ]\n")
	t.Setenv("FOREST_CHECK_PATH", hostBin)
	cfg := Config{Checks: []Check{{
		Name: "credential-isolation",
		Run: fmt.Sprintf(
			`test -z "$GH_TOKEN" && test -z "$GITHUB_TOKEN" && test -z "$FOREST_OPERATOR_SECRET" && `+
				`test "$XDG_RUNTIME_DIR" = "$HOME/run" && `+
				`test "$DBUS_SESSION_BUS_ADDRESS" = "unix:path=$HOME/run/no-session-bus" && `+
				`test "$SSH_AUTH_SOCK" = "$HOME/run/no-ssh-agent" && `+
				`test "$GOMODCACHE" = "$HOME/cache/go-mod" && test "$GOCACHE" = "$HOME/cache/go-build" && `+
				`test "$(command -v gh)" = "$HOME/bin/gh" && ! gh auth token >/dev/null 2>&1 && `+
				`! /usr/bin/gh auth token >/dev/null 2>&1 && `+
				`test ! -e %q && test ! -e %q && test ! -e /proc/%d && test ! -e /etc/machine-id && `+
				`test ! -e /etc/ssl/private && test ! -e /etc/pki/private && `+
				`test ! -e /usr/local/bin/1password-mcp && `+
				`test ! -S /run/user/$(id -u)/bus && ! %q auth token >/dev/null 2>&1`,
			hostProbe, varProbe, os.Getpid(), hostGH),
	}}}
	note, err := runChecks(cfg, t.TempDir(), "run-isolated")
	if err != nil {
		t.Fatalf("runChecks returned error: %v", err)
	}
	if note.Status != "pass" {
		t.Fatalf("status = %q, want pass; output = %q", note.Status, note.Results[0].Output)
	}
}

func TestRunChecksKeepsLinkedWorktreeGitButMasksCheckoutConfig(t *testing.T) {
	repo := setupTestRepo(t)
	runGitTest(t, repo, "config", "http.https://example.invalid/.extraheader", "AUTHORIZATION: secret")
	hookPath := filepath.Join(repo, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\noperator-secret\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	wtDir, _, _, err := createWorktree(repo, workspaceDir(repo), "91", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "file.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDir, err := gitOut(wtDir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	worktreeConfig := filepath.Join(gitDir, "config.worktree")
	configBody := []byte("[http \"https://example.invalid/\"]\n\textraheader = AUTHORIZATION: secret\n")
	if err := os.WriteFile(worktreeConfig, configBody, 0o600); err != nil {
		t.Fatal(err)
	}
	siblingDir, _, _, err := createWorktree(repo, workspaceDir(repo), "92", "sibling")
	if err != nil {
		t.Fatal(err)
	}
	siblingGitDir, err := gitOut(siblingDir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	siblingHook := filepath.Join(siblingGitDir, "hooks", "host-only")
	if err := os.MkdirAll(filepath.Dir(siblingHook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(siblingHook, []byte("host-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitEntry := filepath.Join(wtDir, ".git")
	gitEntryBefore, err := os.ReadFile(gitEntry)
	if err != nil {
		t.Fatal(err)
	}

	note, err := runChecks(Config{Checks: []Check{{
		Name: "linked-worktree",
		Run: fmt.Sprintf(
			`! git diff --quiet -- file.txt && ! git config --local --get-regexp 'http\..*\.extraheader' && `+
				`test ! -e %q && test ! -e %q && test ! -s %q && ! printf attack > .git 2>/dev/null && `+
				`! printf attack > %q 2>/dev/null && printf ok > sandbox-created.txt`,
			hookPath, siblingHook, worktreeConfig, worktreeConfig),
	}}}, wtDir, "run-linked-worktree", repo)
	if err != nil {
		t.Fatalf("runChecks returned error: %v", err)
	}
	if note.Status != "pass" {
		t.Fatalf("linked worktree check = %+v, want pass", note)
	}
	if body, err := os.ReadFile(filepath.Join(wtDir, "sandbox-created.txt")); err != nil || string(body) != "ok" {
		t.Fatalf("sandbox-created.txt = %q, %v", body, err)
	}
	if after, err := os.ReadFile(gitEntry); err != nil || string(after) != string(gitEntryBefore) {
		t.Fatalf("worktree .git changed to %q, %v", after, err)
	}
	if after, err := os.ReadFile(worktreeConfig); err != nil || string(after) != string(configBody) {
		t.Fatalf("config.worktree changed to %q, %v", after, err)
	}
	if body, err := os.ReadFile(siblingHook); err != nil || string(body) != "host-only\n" {
		t.Fatalf("sibling worktree hook changed to %q, %v", body, err)
	}
}

func TestRunChecksMasksSiblingGitAdminAtRootAlias(t *testing.T) {
	repo := setupTestRepo(t)
	sibling, _, _, err := createWorktree(repo, workspaceDir(repo), "95", "root-alias")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := gitOut(sibling, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(admin, "hooks", "host-only")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("host-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	note, err := runChecks(Config{Checks: []Check{{
		Name: "root-alias",
		Run:  fmt.Sprintf("test ! -e %q", hook),
	}}}, repo, "run-root-alias", repo)
	if err != nil || note.Status != "pass" {
		t.Fatalf("root alias check = (%+v, %v), want sibling administration hidden", note, err)
	}
}

func TestRunChecksRejectsForeignGitWorkspace(t *testing.T) {
	owner := setupTestRepo(t)
	foreign := setupTestRepo(t)
	wtDir, _, _, err := createWorktree(foreign, workspaceDir(foreign), "93", "foreign")
	if err != nil {
		t.Fatal(err)
	}
	_, err = runChecks(Config{Checks: []Check{{Name: "never", Run: "true"}}}, wtDir, "run-foreign", owner)
	if err == nil || !strings.Contains(err.Error(), "owning checkout") {
		t.Fatalf("foreign Git workspace error = %v, want owning-checkout refusal", err)
	}
}

func TestRunChecksRejectsSymlinkedWorkspaceRoot(t *testing.T) {
	repo := setupTestRepo(t)
	wtDir, _, _, err := createWorktree(repo, workspaceDir(repo), "94", "real")
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(workspaceDir(repo), "worktrees", "alias")
	if err := os.Symlink(wtDir, alias); err != nil {
		t.Fatal(err)
	}
	_, err = runChecks(Config{Checks: []Check{{Name: "never", Run: "true"}}}, alias, "run-symlink", repo)
	if err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlinked workspace error = %v, want real-directory refusal", err)
	}
}

func TestRunChecksRejectsNestedGitSymlink(t *testing.T) {
	repo := setupTestRepo(t)
	info := filepath.Join(repo, ".git", "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "host-only")
	if err := os.WriteFile(outside, []byte("host-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(info, "host-link")); err != nil {
		t.Fatal(err)
	}
	_, err := runChecks(Config{Checks: []Check{{Name: "never", Run: "true"}}}, repo, "run-git-link", repo)
	if err == nil || !strings.Contains(err.Error(), "Git common directory") ||
		!strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("nested Git link error = %v, want refusal", err)
	}
}

func TestRunChecksHidesUnrecognizedGitCommonFiles(t *testing.T) {
	repo := setupTestRepo(t)
	common := filepath.Join(repo, ".git")
	for _, name := range []string{"credentials", "FETCH_HEAD", "omp-coordination.md"} {
		if err := os.WriteFile(filepath.Join(common, name), []byte("host-only"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := fmt.Sprintf(
		"git status --short >/dev/null && test ! -e %q && test ! -e %q && test ! -e %q && "+
			"test ! -e /workspace/.git/credentials && test ! -e /workspace/.git/FETCH_HEAD && "+
			"test ! -e /workspace/.git/omp-coordination.md && "+
			"! printf changed > \"$HOME/git/config\" 2>/dev/null && test ! -s \"$HOME/git/config\"",
		filepath.Join(common, "credentials"),
		filepath.Join(common, "FETCH_HEAD"),
		filepath.Join(common, "omp-coordination.md"),
	)
	note, err := runChecks(Config{Checks: []Check{{Name: "git-view", Run: run}}}, repo, "run-git-view", repo)
	if err != nil || note.Status != "pass" {
		t.Fatalf("sanitized Git view check = (%+v, %v), want unrecognized files hidden", note, err)
	}
}

func TestRunChecksExecutesDeclaredBuild(t *testing.T) {
	repoDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	note, err := runChecks(Config{Checks: []Check{{
		Name: "build",
		Run:  "mise exec -- go build ./...",
	}}}, repoDir, "run-build")
	if err != nil {
		t.Fatalf("runChecks returned error: %v", err)
	}
	if note.Status != "pass" || len(note.Results) != 1 || note.Results[0].Code != 0 {
		t.Fatalf("declared build = %+v, want pass", note)
	}
}

func TestRunChecksExecutesSelfcheckFromLinkedWorktree(t *testing.T) {
	repoDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(repoDir, WorkspaceDir, "worktrees",
		"selfcheck-"+filepath.Base(t.TempDir()))
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repoDir, "worktree", "add", "--detach", worktree, "HEAD")
	t.Cleanup(func() {
		runGitTest(t, repoDir, "worktree", "remove", "--force", worktree)
	})
	note, err := runChecks(Config{Checks: []Check{{
		Name: "selfcheck",
		Run:  "mise exec -- go run . selfcheck",
	}}}, worktree, "run-selfcheck", repoDir)
	if err != nil {
		t.Fatalf("runChecks returned error: %v", err)
	}
	if note.Status != "pass" || len(note.Results) != 1 || note.Results[0].Code != 0 {
		t.Fatalf("declared selfcheck = %+v, want pass", note)
	}
}

func TestRunChecksClassifiesSandboxBootstrapFailureAsEnvironmentError(t *testing.T) {
	wtFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(wtFile, []byte("file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	note, err := runChecks(Config{Checks: []Check{{Name: "never-ran", Run: "exit 7"}}}, wtFile, "run-bootstrap")
	if err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("runChecks error = %v, want sandbox bootstrap error", err)
	}
	if note.Status != "" || len(note.Results) != 0 {
		t.Fatalf("bootstrap note = %+v, want no durable check result", note)
	}
}

func TestRunChecksRejectsProtectedHostMounts(t *testing.T) {
	tests := []struct {
		name string
		set  func(*testing.T)
	}{
		{name: "check path", set: func(t *testing.T) { t.Setenv("FOREST_CHECK_PATH", "/run") }},
		{name: "rustup home", set: func(t *testing.T) { t.Setenv("FOREST_CHECK_ENV", "RUSTUP_HOME=/run") }},
		{name: "mise data", set: func(t *testing.T) { t.Setenv("MISE_DATA_DIR", "/home") }},
		{name: "toolchain subtree", set: func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "mise")
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(root, "installs")); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MISE_DATA_DIR", root)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.set(t)
			note, err := runChecks(Config{Checks: []Check{{Name: "never-ran", Run: "exit 0"}}}, t.TempDir(), "run-mount")
			if err == nil {
				t.Fatal("runChecks accepted a protected Host mount")
			}
			if note.Status != "" || len(note.Results) != 0 {
				t.Fatalf("preflight note = %+v, want no durable check result", note)
			}
		})
	}
}

func TestRunChecksContinuesAfterFailure(t *testing.T) {
	wtDir := t.TempDir()
	later := filepath.Join(wtDir, "later.txt")
	cfg := Config{Checks: []Check{
		{Name: "first", Run: "printf failed; exit 7"},
		{Name: "later", Run: "touch later.txt"},
	}}

	note, err := runChecks(cfg, wtDir, "run-fail")
	if err != nil {
		t.Fatalf("runChecks returned error: %v", err)
	}
	if note.Status != "fail" {
		t.Fatalf("status = %q, want fail", note.Status)
	}
	if len(note.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(note.Results))
	}
	if note.Results[0].Code != 7 || note.Results[1].Code != 0 {
		t.Fatalf("result codes = [%d %d], want [7 0]", note.Results[0].Code, note.Results[1].Code)
	}
	if _, err := os.Stat(later); err != nil {
		t.Fatalf("later check did not run: %v", err)
	}
}

func TestRunChecksPreflightFailureHasNoStatus(t *testing.T) {
	oldEnv := checkEnvironment
	checkEnvironment = func() ([]string, func(), error) {
		return nil, func() {}, errors.New("locate mise: missing")
	}
	defer func() { checkEnvironment = oldEnv }()

	note, err := runChecks(Config{Checks: []Check{{Name: "true", Run: "true"}}}, t.TempDir(), "run-preflight")
	if err == nil {
		t.Fatal("runChecks returned no error for a preflight failure")
	}
	if note.Status != "" {
		t.Fatalf("preflight note status = %q, want empty so no pass is written", note.Status)
	}
	if len(note.Results) != 0 {
		t.Fatalf("preflight note has %d results, want none: no declared check ran", len(note.Results))
	}
}

func TestRunChecksKeepsOutputTail(t *testing.T) {
	cfg := Config{Checks: []Check{{
		Name: "output",
		Run:  "head -c 5000 /dev/zero | tr '\\0' x; printf tail-marker",
	}}}

	note, err := runChecks(cfg, t.TempDir(), "run-output")
	if err != nil {
		t.Fatalf("runChecks returned error: %v", err)
	}
	if len(note.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(note.Results))
	}
	output := note.Results[0].Output
	if len(output) != checkOutputTailBytes {
		t.Fatalf("output bytes = %d, want %d", len(output), checkOutputTailBytes)
	}
	if !strings.HasSuffix(output, "tail-marker") {
		t.Fatalf("output tail lost marker: %q", output[len(output)-len("tail-marker"):])
	}
}

func TestRunChecksCommandFailureIsResult(t *testing.T) {
	cfg := Config{Checks: []Check{{Name: "missing", Run: "command-that-does-not-exist-iron-forest"}}}

	note, err := runChecks(cfg, t.TempDir(), "run-missing")
	if err != nil {
		t.Fatalf("runChecks returned error for failed command: %v", err)
	}
	if note.Status != "fail" {
		t.Fatalf("status = %q, want fail", note.Status)
	}
	if len(note.Results) != 1 || note.Results[0].Code == 0 {
		t.Fatalf("results = %+v, want one nonzero result", note.Results)
	}
}

func TestChecksSummaryNamesFailures(t *testing.T) {
	got := checksSummary(checksNote{
		Status: "fail",
		Results: []checkResult{
			{Name: "build", Code: 0},
			{Name: "test", Code: 1},
		},
	})
	if got != "checks fail: test(exit 1)" {
		t.Fatalf("summary = %q, want checks fail: test(exit 1)", got)
	}
}
