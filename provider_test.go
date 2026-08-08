package main

import (
	"encoding/json"
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

// TestEnvProviderConfigResolvesDeclaredModels pins the fix for the direct-key
// path: the Builder and Manager declarations name model
// openrouter-mint/deepseek-v4-flash-0731 and the Verifier names
// openrouter/openai/gpt-5.6-luna. With only OPENROUTER_API_KEY set, opencode has
// no externally-defined openrouter-mint provider, so the env config must define
// that provider and model itself or every Builder/Manager run fails before it
// can make a single call. The test loads the real agent declarations and proves
// the generated config resolves each declared model.
func TestEnvProviderConfigResolvesDeclaredModels(t *testing.T) {
	t.Setenv(openRouterKeyVar, "sk-aaaa")
	cfg := envProviderConfig()

	var oc openCodeConfig
	if err := json.Unmarshal(cfg, &oc); err != nil {
		t.Fatalf("env config is not parseable opencode config: %v", err)
	}
	if len(oc.Provider) == 0 {
		t.Fatal("env config defines no provider")
	}

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, DefaultAgentsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	// Copy the real agent directories so loadAgent reads the true declarations.
	for _, name := range []string{"builder", "manager", "verifier"} {
		src := filepath.Join(DefaultAgentsDir, name)
		dst := filepath.Join(repo, DefaultAgentsDir, name)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"agent.yaml", "instructions.md"} {
			b, err := os.ReadFile(filepath.Join(src, f))
			if err != nil {
				continue
			}
			if err := os.WriteFile(filepath.Join(dst, f), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, name := range []string{"builder", "manager", "verifier"} {
		a, err := loadAgent(repo, name)
		if err != nil {
			t.Fatalf("loadAgent %s: %v", name, err)
		}
		id, model := splitModelRef(a.Model)
		p, defined := oc.Provider[id]
		if !defined {
			t.Fatalf("env config does not define provider %q for %s (%s)", id, name, a.Model)
		}
		// A provider that carries an explicit models map must list the declared
		// model: opencode cannot invent it. The openrouter-mint alias needs this
		// for the Builder and Manager. A provider with no models map (openrouter)
		// resolves its models from opencode's native catalog, so none is required.
		if len(p.Models) > 0 && model != "" {
			if _, ok := p.Models[model]; !ok {
				t.Fatalf("env config provider %q lacks declared model %q for %s (%s)", id, model, name, a.Model)
			}
		}
	}
	// The key value still never lands in the rendered config.
	if strings.Contains(string(cfg), "sk-aaaa") {
		t.Fatalf("env config leaked the key value: %s", cfg)
	}
}

// splitModelRef splits an opencode model reference of the form
// "provider/model" into its provider id and model id. A reference without "/"
// names a whole provider and yields an empty model id.
func splitModelRef(ref string) (provider, model string) {
	if i := strings.Index(ref, "/"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
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
