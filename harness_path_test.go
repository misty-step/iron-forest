package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeDriver writes an executable named name into dir and returns its path.
func writeDriver(t *testing.T, dir, name, body string) string {
	t.Helper()
	fake := filepath.Join(dir, name)
	if err := os.WriteFile(fake, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return fake
}

// TestCheckHostBinsParsesPathList pins the generic, stack-agnostic mechanism:
// FOREST_CHECK_PATH is a platform path-list the operator fills with host
// toolchain directories, and blank entries do not leak into the child PATH.
func TestCheckHostBinsParsesPathList(t *testing.T) {
	cargoBin := filepath.Join("home", "cargo", "bin")
	optBin := filepath.Join("opt", "tools", "bin")
	t.Setenv("FOREST_CHECK_PATH", cargoBin+string(os.PathListSeparator)+string(os.PathListSeparator)+optBin)
	dirs := checkHostBins()
	if len(dirs) != 2 {
		t.Fatalf("checkHostBins() = %v, want 2 entries", dirs)
	}
	if dirs[0] != cargoBin {
		t.Errorf("first entry = %q, want %q", dirs[0], cargoBin)
	}
	if dirs[1] != optBin {
		t.Errorf("second entry = %q, want %q", dirs[1], optBin)
	}
}

func TestLocateRequiredExecutableRejectsRelativePath(t *testing.T) {
	root := t.TempDir()
	tools := filepath.Join(root, "tools")
	if err := os.Mkdir(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDriver(t, tools, "mise", "#!/bin/sh\nexit 0\n")
	t.Chdir(root)
	t.Setenv("PATH", "tools")
	t.Setenv("GODEBUG", "execerrdot=0")
	if _, err := locateRequiredExecutable("mise"); err == nil ||
		!strings.Contains(err.Error(), "is not absolute") {
		t.Fatalf("relative mise lookup error = %v, want refusal", err)
	}
}

func TestValidateOpenCodeVersionRejectsWrongVersion(t *testing.T) {
	tools := t.TempDir()
	path := writeDriver(t, tools, "opencode", "#!/bin/sh\nprintf '1.18.10\\n'\n")
	if err := validateOpenCodeVersion(path); err == nil ||
		!strings.Contains(err.Error(), "does not match required 1.18.11") {
		t.Fatalf("wrong opencode version error = %v, want refusal", err)
	}
}

func TestChildEnvironmentIgnoresSameVersionAmbientOpenCode(t *testing.T) {
	tools := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "ambient-ran")
	writeDriver(t, tools, "opencode", "#!/bin/sh\n"+
		"touch "+sentinel+"\nprintf '1.18.11\\n'\n")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))

	env, cleanup, err := childEnvironment(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("ambient opencode ran during pinned staging: %v", err)
	}
	staged := filepath.Join(envValue(t, env, "HOME"), "bin", "opencode")
	cmd := exec.Command(staged, "--version")
	cmd.Env = []string{"PATH=" + childSystemPath}
	if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != requiredOpenCodeVersion {
		t.Fatalf("staged opencode = (%q, %v), want %s", out, err, requiredOpenCodeVersion)
	}
}

func runAgentShell(t *testing.T, allow []string, worktree, command string) ([]byte, error) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installAgentShell(home, allow); err != nil {
		t.Fatal(err)
	}
	mise, err := locateRequiredExecutable("mise")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(home, "bin", "forest-shell"), "-c", command)
	cmd.Dir = worktree
	cmd.Env = []string{"PATH=" + filepath.Dir(mise) + string(os.PathListSeparator) + childSystemPath}
	return cmd.CombinedOutput()
}

func TestAgentShellAllowsOnlyPlainNamedCommandsInsideWorktree(t *testing.T) {
	worktree := t.TempDir()
	if out, err := runAgentShell(t, []string{"true *"}, worktree, "true"); err != nil {
		t.Fatalf("bare allowlisted command = (%q, %v), want pass", out, err)
	}
	if out, err := runAgentShell(t, []string{"printf *"}, worktree, "printf ALLOWED"); err != nil ||
		string(out) != "ALLOWED" {
		t.Fatalf("allowlisted command = (%q, %v), want ALLOWED", out, err)
	}
	repo, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runAgentShell(
		t,
		[]string{"go version *", "mise *"},
		repo,
		"mise exec -- go version",
	); err != nil || !strings.Contains(string(out), "go version go1.26.5") {
		t.Fatalf("allowlisted nested toolchain command = (%q, %v), want pinned Go", out, err)
	}
	inside := filepath.Join(worktree, "inside")
	if out, err := runAgentShell(t, []string{"touch *"}, worktree, "touch "+inside); err != nil {
		t.Fatalf("inside-worktree command = (%q, %v), want pass", out, err)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("allowlisted inside-worktree write did not run: %v", err)
	}
}

func TestAgentShellRejectsUnlistedSyntaxAndOutsidePaths(t *testing.T) {
	worktree := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	tests := []struct {
		name    string
		allow   []string
		command string
	}{
		{"unlisted", []string{"printf *"}, "curl https://example.com"},
		{"chain", []string{"printf *", "touch *"}, "printf OK; touch " + outside},
		{"redirection", []string{"printf *"}, "printf OK > " + outside},
		{"absolute-path", []string{"touch *"}, "touch " + outside},
		{"traversal", []string{"touch *"}, "touch ../outside"},
		{"absolute-equals", []string{"touch *"}, "touch " + outside + "=name"},
		{"traversal-equals", []string{"touch *"}, "touch ../outside=name"},
		{"attached-absolute-path", []string{"git *"}, "git -C" + filepath.Dir(outside) + " status"},
		{"attached-traversal", []string{"git *"}, "git -C../outside status"},
		{"nested-unlisted", []string{"true *", "mise *"}, "mise exec -- curl https://example.com"},
		{"nested-shell", []string{"true *", "mise *"}, "mise exec -- /bin/sh -c true"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out, err := runAgentShell(t, test.allow, worktree, test.command)
			if err == nil || !strings.Contains(string(out), "forest: denied") {
				t.Fatalf("shell command = (%q, %v), want denial", out, err)
			}
			if _, err := os.Stat(outside); !os.IsNotExist(err) {
				t.Fatalf("denied command reached outside path: %v", err)
			}
		})
	}
}

// TestChildPathHostToolchainWinsOverDeadShim pins child PATH resolution for a
// non-Go driver layout: a working host binary on a declared toolchain directory
// resolves before a dead mise shim that would otherwise shadow it.
func TestChildPathHostToolchainWinsOverDeadShim(t *testing.T) {
	binDir := t.TempDir()
	shims := filepath.Join(t.TempDir(), "shims")
	toolchain := filepath.Join(t.TempDir(), "toolchain-bin")
	for _, dir := range []string{shims, toolchain} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	hostCargo := writeDriver(t, toolchain, "cargo", "#!/bin/sh\necho host-cargo-ok\n")
	deadCargo := writeDriver(t, shims, "cargo", "#!/bin/sh\necho 'mise ERROR cargo is not a valid shim' >&2\nexit 1\n")

	path := childPath(binDir, shims, []string{toolchain})

	// The resolved driver path must be the working host binary, not the dead shim.
	cmd := exec.Command("sh", "-c", "command -v cargo")
	cmd.Env = append(os.Environ(), "PATH="+path)
	got, err := cmd.Output()
	if err != nil {
		t.Fatalf("command -v cargo failed: %v", err)
	}
	if strings.TrimSpace(string(got)) != hostCargo {
		t.Fatalf("cargo resolved to %q, want host binary %q", strings.TrimSpace(string(got)), hostCargo)
	}
	if filepath.Clean(strings.TrimSpace(string(got))) == deadCargo {
		t.Fatal("dead shim silently won over the working host binary")
	}

	// The driver must actually execute, not just appear on the path.
	run := exec.Command("sh", "-c", "cargo")
	run.Env = append(os.Environ(), "PATH="+path)
	out, err := run.Output()
	if err != nil {
		t.Fatalf("cargo did not run at process spawn: %v", err)
	}
	if string(out) != "host-cargo-ok\n" {
		t.Fatalf("cargo output = %q, want %q", string(out), "host-cargo-ok\n")
	}
}

func TestStageHostExecutablesRejectsEscapingSymlink(t *testing.T) {
	source := t.TempDir()
	outside := writeDriver(t, t.TempDir(), "host-only", "#!/bin/sh\nprintf host-only\n")
	if err := os.Symlink(outside, filepath.Join(source, "leak")); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if _, err := stageHostExecutables(home, 0, source); err == nil ||
		!strings.Contains(err.Error(), "escapes FOREST_CHECK_PATH") {
		t.Fatalf("escaping Host executable error = %v, want refusal", err)
	}
	if _, err := os.Stat(filepath.Join(home, "host-bin", "0", "leak")); !os.IsNotExist(err) {
		t.Fatalf("escaping Host executable reached staging: %v", err)
	}
}

// TestRunChecksFindsHostToolchainDriver is the integration pin: a managed-repo
// check whose driver lives only on a host toolchain directory succeeds at
// process spawn once the operator declares that directory in FOREST_CHECK_PATH.
func TestRunChecksFindsHostToolchainDriver(t *testing.T) {
	toolchain := t.TempDir()
	writeDriver(t, toolchain, "cargo", "#!/bin/sh\necho fake-cargo-ok\n")
	t.Setenv("FOREST_CHECK_PATH", toolchain)

	cfg := Config{Checks: []Check{{Name: "tool", Run: "cargo"}}}
	note, err := runChecks(cfg, t.TempDir(), "run-tool")
	if err != nil {
		t.Fatalf("runChecks returned error: %v", err)
	}
	if note.Status != "pass" {
		t.Fatalf("status = %q, want pass; output = %q", note.Status, note.Results[0].Output)
	}
	if len(note.Results) != 1 || note.Results[0].Code != 0 {
		t.Fatalf("results = %+v, want one passing check", note.Results)
	}
	if !strings.Contains(note.Results[0].Output, "fake-cargo-ok") {
		t.Fatalf("check output = %q, want the fake driver to have run", note.Results[0].Output)
	}
}

func TestRunChecksUsesSandboxShellWhenHostPathOmitsSystemBins(t *testing.T) {
	mise, err := exec.LookPath("mise")
	if err != nil {
		t.Fatal(err)
	}
	miseData, _ := miseLocations(mise)
	hostPath := t.TempDir()
	if err := os.Symlink(mise, filepath.Join(hostPath, "mise")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MISE_DATA_DIR", miseData)
	t.Setenv("PATH", hostPath)

	note, err := runChecks(Config{Checks: []Check{{Name: "shell", Run: "printf ok"}}}, t.TempDir(), "run-shell")
	if err != nil {
		t.Fatalf("runChecks returned error: %v", err)
	}
	if note.Status != "pass" || note.Results[0].Output != "ok" {
		t.Fatalf("sandbox shell result = %+v, want pass", note)
	}
}

// TestCheckHostEnvParsesMetadata pins the generic metadata mechanism: a host
// toolchain proxy reads metadata that lives outside the private child HOME, so
// FOREST_CHECK_ENV carries newline-separated KEY=VALUE pairs into the child
// environment, ignoring blank lines and entries with no "=" separator. Only keys
// on the curated allowlist are carried in.
func TestCheckHostEnvParsesMetadata(t *testing.T) {
	rustup := filepath.Join("home", ".rustup")
	t.Setenv("FOREST_CHECK_ENV", "RUSTUP_HOME="+rustup+"\n\nNOT_A_PAIR")
	entries := checkHostEnv()
	want := []string{"RUSTUP_HOME=" + rustup}
	if len(entries) != len(want) {
		t.Fatalf("checkHostEnv() = %v, want %v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Fatalf("checkHostEnv()[%d] = %q, want %q", i, entries[i], want[i])
		}
	}
}

// TestCheckHostEnvAllowsOnlyAllowlistedMetadata pins the regression the reviewer
// flagged: the old substring denylist was unsound, so a credential under any
// unlisted key name (CI_JOB_JWT, AWS_ACCESS_KEY_ID) or any credential-bearing
// path (KUBECONFIG, GIT_CONFIG_GLOBAL) reached the child, and even the then
// allowed CARGO_HOME pointed at ~/.cargo where credentials.toml lives. The
// policy is now an explicit allowlist: only RUSTUP_HOME passes, and every other
// key — harness-managed, value-credential, path-credential, or the credential-
// bearing CARGO_HOME — is dropped. os/exec.Cmd.Env resolves duplicates by last
// occurrence, so any such key surviving into the env would defeat the harness;
// dropping everything outside the allowlist keeps the private environment
// authoritative and guarantees no secret reaches the child.
func TestCheckHostEnvAllowsOnlyAllowlistedMetadata(t *testing.T) {
	forbidden := []string{
		"HOME=/overridden",
		"PATH=/overridden",
		"MISE_CONFIG_DIR=/overridden",
		"MISE_DATA_DIR=/overridden",
		"GOMODCACHE=/overridden",
		"GOCACHE=/overridden",
		"GH_TOKEN=super-secret",
		"GITHUB_TOKEN=super-secret",
		"MY_SETUP_SECRET=super-secret",
		"CI_JOB_JWT=pastebin-token",
		"AWS_ACCESS_KEY_ID=AKIAEXAMPLE",
		"KUBECONFIG=/home/op/.kube/config",
		"GIT_CONFIG_GLOBAL=/home/op/.gitconfig",
		"CARGO_HOME=/home/op/.cargo",
	}
	t.Setenv("FOREST_CHECK_ENV", strings.Join(
		append(forbidden, "RUSTUP_HOME=/host/.rustup"), "\n"))
	entries := checkHostEnv()
	want := []string{"RUSTUP_HOME=/host/.rustup"}
	if len(entries) != len(want) {
		t.Fatalf("checkHostEnv() = %v, want %v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Fatalf("checkHostEnv()[%d] = %q, want %q", i, entries[i], want[i])
		}
	}
	for _, e := range entries {
		key, _, _ := strings.Cut(e, "=")
		if !hostEnvAllowed(key) {
			t.Fatalf("checkHostEnv() leaked non-allowlisted key %q", key)
		}
	}
}

// envValue returns the value for key in env, or "" if absent.
func envValue(t *testing.T, env []string, key string) string {
	t.Helper()
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if ok && k == key {
			return v
		}
	}
	return ""
}

// TestHostToolchainMechanismScopedToChecks pins that the operator-declared host
// toolchain mechanism (FOREST_CHECK_PATH and FOREST_CHECK_ENV) is applied only
// to the check environment. The old code fed one childEnvironment to runPhase and
// runChecks alike, so path and metadata reach leaked into the agent run; now the
// agent env (childEnvironment) carries neither, and only checkEnvironment does.
func TestHostToolchainMechanismScopedToChecks(t *testing.T) {
	toolchain := filepath.Join(t.TempDir(), "toolchain-bin")
	t.Setenv("FOREST_CHECK_PATH", toolchain)
	t.Setenv("FOREST_CHECK_ENV", "RUSTUP_HOME=/host/.rustup")

	agent, cleanup, err := childEnvironment(nil)
	if err != nil {
		t.Fatalf("childEnvironment returned error: %v", err)
	}
	defer cleanup()
	if got := envValue(t, agent, "RUSTUP_HOME"); got != "" {
		t.Fatalf("agent env leaked RUSTUP_HOME=%q, want none", got)
	}
	for _, dir := range strings.Split(envValue(t, agent, "PATH"), string(os.PathListSeparator)) {
		if filepath.Clean(dir) == filepath.Clean(toolchain) {
			t.Fatalf("agent env leaked host toolchain dir %q on PATH", dir)
		}
	}

	check, cleanup, err := checkEnvironment()
	if err != nil {
		t.Fatalf("checkEnvironment returned error: %v", err)
	}
	defer cleanup()
	if got := envValue(t, check, "RUSTUP_HOME"); got != "/host/.rustup" {
		t.Fatalf("check env RUSTUP_HOME = %q, want /host/.rustup", got)
	}
	var onPath bool
	for _, dir := range strings.Split(envValue(t, check, "PATH"), string(os.PathListSeparator)) {
		if filepath.Clean(dir) == filepath.Clean(toolchain) {
			onPath = true
		}
	}
	if !onPath {
		t.Fatalf("check env PATH missing host toolchain dir %q", toolchain)
	}
}

// TestRunChecksHostProxyResolvesWithMetadata pins the actual proxy behavior the
// item calls out, not a self-contained fake. A host toolchain "cargo" is a
// proxy that reads RUSTUP_HOME to find its real driver. With a private HOME and
// only ~/.cargo/bin on PATH, that lookup finds nothing and the proxy reports "no
// default is configured"; pointing RUSTUP_HOME at the host rustup home makes the
// proxy resolve and run the real driver. FOREST_CHECK_PATH supplies the proxy;
// FOREST_CHECK_ENV supplies the allowlisted metadata. A CARGO_HOME entry is also
// present to prove the allowlist drops it (it would point at ~/.cargo, which
// holds credentials.toml) without breaking the allowlisted RUSTUP_HOME lookup.

func TestRunChecksHostProxyResolvesWithMetadata(t *testing.T) {
	cargoBin := filepath.Join(t.TempDir(), "cargo", "bin")
	rustupHome := filepath.Join(t.TempDir(), ".rustup")
	realBin := filepath.Join(rustupHome, "toolchains", "stable-x86_64-unknown-linux-gnu", "bin")
	for _, dir := range []string{cargoBin, realBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(rustupHome, "settings.toml"),
		[]byte("default_toolchain = \"stable-x86_64-unknown-linux-gnu\"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	writeDriver(t, cargoBin, "cargo",
		"#!/bin/sh\n"+
			": \"${RUSTUP_HOME:=$HOME/.rustup}\"\n"+
			"tc=\"$RUSTUP_HOME/toolchains/$RUSTUP_TOOLCHAIN/bin\"\n"+
			"if [ -x \"$tc/cargo-real\" ]; then exec \"$tc/cargo-real\" \"$@\"; fi\n"+
			"echo 'error: no default toolchain configured' >&2\n"+
			"exit 1\n")
	writeDriver(t, realBin, "cargo-real", "#!/bin/sh\necho real-driver-ok\n")

	t.Setenv("FOREST_CHECK_PATH", cargoBin)
	t.Setenv("FOREST_CHECK_ENV", "RUSTUP_HOME="+rustupHome+"\nCARGO_HOME="+filepath.Join(t.TempDir(), ".cargo"))

	cfg := Config{Checks: []Check{{Name: "cargo-version", Run: "cargo --version"}}}
	note, err := runChecks(cfg, t.TempDir(), "run-proxy")
	if err != nil {
		t.Fatalf("runChecks returned error: %v", err)
	}
	if note.Status != "pass" {
		t.Fatalf("status = %q, want pass; output = %q", note.Status, note.Results[0].Output)
	}
	if len(note.Results) != 1 || note.Results[0].Code != 0 {
		t.Fatalf("results = %+v, want one passing check", note.Results)
	}
	if !strings.Contains(note.Results[0].Output, "real-driver-ok") {
		t.Fatalf("check output = %q, want the real driver behind the proxy to have run", note.Results[0].Output)
	}
}
func TestStageToolchainDataRejectsCrossMountSymlink(t *testing.T) {
	for _, target := range []string{"/etc/passwd", "/workspace/host-only"} {
		t.Run(target, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), ".rustup")
			toolchains := filepath.Join(root, "toolchains", "stable", "bin")
			if err := os.MkdirAll(toolchains, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(toolchains, "host-link")); err != nil {
				t.Fatal(err)
			}
			_, err := stageToolchainData(t.TempDir(), "rustup-data", root, root, "toolchains")
			if err == nil || !strings.Contains(err.Error(), "reaches another sandbox mount") {
				t.Fatalf("cross-mount toolchain link error = %v, want refusal", err)
			}
		})
	}
}

func TestRustupDefaultToolchainRejectsTraversalSelector(t *testing.T) {
	for _, value := range []string{".", ".."} {
		t.Run(value, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), ".rustup")
			if err := os.MkdirAll(filepath.Join(root, "toolchains"), 0o755); err != nil {
				t.Fatal(err)
			}
			body := []byte("default_toolchain = \"" + value + "\"\n")
			if err := os.WriteFile(filepath.Join(root, "settings.toml"), body, 0o600); err != nil {
				t.Fatal(err)
			}
			if got := rustupDefaultToolchain(root); got != "" {
				t.Fatalf("default toolchain %q resolved to %q, want refusal", value, got)
			}
		})
	}
}
