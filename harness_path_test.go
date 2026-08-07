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

// TestCheckHostEnvParsesMetadata pins the generic metadata mechanism: a host
// toolchain proxy reads metadata that lives outside the private child HOME, so
// FOREST_CHECK_ENV carries newline-separated KEY=VALUE pairs into the child
// environment, ignoring blank lines and entries with no "=" separator.
func TestCheckHostEnvParsesMetadata(t *testing.T) {
	rustup := filepath.Join("home", ".rustup")
	cargo := filepath.Join("home", ".cargo")
	t.Setenv("FOREST_CHECK_ENV", "RUSTUP_HOME="+rustup+"\n\nCARGO_HOME="+cargo+"\nNOT_A_PAIR")
	entries := checkHostEnv()
	want := []string{"RUSTUP_HOME=" + rustup, "CARGO_HOME=" + cargo}
	if len(entries) != len(want) {
		t.Fatalf("checkHostEnv() = %v, want %v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Fatalf("checkHostEnv()[%d] = %q, want %q", i, entries[i], want[i])
		}
	}
}

// TestRunChecksHostProxyResolvesWithMetadata pins the actual proxy behavior the
// item calls out, not a self-contained fake. A host toolchain "cargo" is a
// proxy that reads RUSTUP_HOME (falling back to $HOME/.rustup) to find its real
// driver. With a private HOME and only ~/.cargo/bin on PATH, that lookup finds
// nothing and the proxy reports "no default is configured"; pointing RUSTUP_HOME
// at the host rustup home makes the proxy resolve and run the real driver.
// FOREST_CHECK_PATH supplies the proxy, FOREST_CHECK_ENV supplies its metadata.
func TestRunChecksHostProxyResolvesWithMetadata(t *testing.T) {
	cargoBin := filepath.Join(t.TempDir(), "cargo", "bin")
	rustupHome := filepath.Join(t.TempDir(), ".rustup")
	realBin := filepath.Join(rustupHome, "toolchains", "stable-x86_64-unknown-linux-gnu", "bin")
	for _, dir := range []string{cargoBin, realBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeDriver(t, cargoBin, "cargo",
		"#!/bin/sh\n"+
			": \"${RUSTUP_HOME:=$HOME/.rustup}\"\n"+
			"for tc in \"$RUSTUP_HOME\"/toolchains/*/bin; do\n"+
			"\t[ -d \"$tc\" ] || continue\n"+
			"\tif [ -x \"$tc/cargo-real\" ]; then exec \"$tc/cargo-real\" \"$@\"; fi\n"+
			"done\n"+
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
