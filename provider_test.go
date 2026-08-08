package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveProviderEnvWinsWithoutMint pins the first-install path: an operator
// with no Mint route supplies an OpenRouter key by environment variable, and the
// resolver reports the env mechanism and produces a config even though no
// opencode configuration file exists.
func TestResolveProviderEnvWinsWithoutMint(t *testing.T) {
	t.Setenv(openRouterKeyVar, "sk-aaabbb")
	factory := t.TempDir() // ships no .opencode/opencode.json at all
	mech, cfg, ok := resolveProvider(factory)
	if !ok {
		t.Fatal("env mechanism must resolve with OPENROUTER_API_KEY set")
	}
	if mech != mechEnv {
		t.Fatalf("mech = %v, want env", mech)
	}
	if string(cfg) == "" {
		t.Fatal("env mechanism must produce a provider config")
	}
	// The raw key never lands on disk: the staged config references the
	// environment variable rather than embedding the value.
	if strings.Contains(string(cfg), "sk-aaabbb") {
		t.Fatalf("env config leaked the key value: %s", cfg)
	}
	if !strings.Contains(string(cfg), "{env:"+openRouterKeyVar+"}") {
		t.Fatalf("env config does not reference %s: %s", openRouterKeyVar, cfg)
	}
	if !strings.Contains(string(cfg), defaultOpenRouterBaseURL) {
		t.Fatalf("env config does not default the baseURL to OpenRouter: %s", cfg)
	}
}

// TestResolveProviderEnvOverridesMint pins the precedence: when both an
// environment key and a Mint-marker config are present, the enemy mechanism
// wins, so a stranger's env var is not ignored by a shipped Mint config.
func TestResolveProviderEnvOverridesMint(t *testing.T) {
	t.Setenv(openRouterKeyVar, "sk-xxxx")
	factory := t.TempDir()
	oc := filepath.Join(factory, ".opencode")
	if err := os.MkdirAll(oc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oc, "opencode.json"),
		[]byte(`{"provider":{"mint":{"options":{"apiKey":"__mint.x__"}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mech, _, ok := resolveProvider(factory)
	if !ok || mech != mechEnv {
		t.Fatalf("env must win over Mint, got ok=%v mech=%v", ok, mech)
	}
}

// TestResolveProviderFallsBackToMint pins that a Mint-marker config is used
// when no environment key is set, preserving the existing route unchanged.
func TestResolveProviderFallsBackToMint(t *testing.T) {
	t.Setenv(openRouterKeyVar, "")
	factory := t.TempDir()
	oc := filepath.Join(factory, ".opencode")
	if err := os.MkdirAll(oc, 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"provider":{"mint":{"options":{"apiKey":"__mint.tests__"}}}}`)
	if err := os.WriteFile(filepath.Join(oc, "opencode.json"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	mech, cfg, ok := resolveProvider(factory)
	if !ok || mech != mechMint {
		t.Fatalf("Mint config must resolve when no env key set, got ok=%v mech=%v", ok, mech)
	}
	if string(cfg) != string(want) {
		t.Fatalf("Mint config must be preserved verbatim, got %s", cfg)
	}
}

// TestResolveProviderNoneFails pins that with no env key and no opencode config
// the resolver reports no mechanism, so selfcheck can fail loudly.
func TestResolveProviderNoneFails(t *testing.T) {
	t.Setenv(openRouterKeyVar, "")
	// Isolate the global opencode config so a host-level config can't resolve as
	// the fallback in this environment.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	mech, _, ok := resolveProvider(t.TempDir())
	if ok {
		t.Fatalf("expected no mechanism, got %v", mech)
	}
	if mech != mechNone {
		t.Fatalf("mech = %v, want none", mech)
	}
	if _, err := providerMechanismReport(t.TempDir()); err == nil {
		t.Fatal("providerMechanismReport must error when no credential resolves")
	}
}

// TestEnvProviderConfigBaseURLOverride pins the broker base-URL override: with
// FOREST_PROVIDER_BASE_URL set, the env config points at the broker proxy while
// still taking the key from the environment.
func TestEnvProviderConfigBaseURLOverride(t *testing.T) {
	override := "http://broker.example/proxy/openrouter"
	t.Setenv(providerBaseURLOverride, override)
	cfg := envProviderConfig()
	if !strings.Contains(string(cfg), override) {
		t.Fatalf("env config did not apply the base-URL override: %s", cfg)
	}
	if strings.Contains(string(cfg), openRouterKeyVar) == false {
		t.Fatalf("env config lost the key reference: %s", cfg)
	}
}

// TestHasMintMarker exercises the detector across marker and non-marker configs.
func TestHasMintMarker(t *testing.T) {
	if !hasMintMarker([]byte(`{"provider":{"mint":{"options":{"apiKey":"__mint.tests__"}}}}`)) {
		t.Error("mint marker not detected")
	}
	if hasMintMarker([]byte(`{"provider":{"openrouter":{"options":{"apiKey":"{env:OPENROUTER_API_KEY}"}}}}`)) {
		t.Error("non-marker config wrongly reported as Mint")
	}
	if hasMintMarker([]byte(`not json`)) {
		t.Error("unparseable config wrongly reported as Mint")
	}
}
