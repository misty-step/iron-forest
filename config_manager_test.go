package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagerConfigShipsDisabled(t *testing.T) {
	cfg := defaultConfig()
	if cfg.Flows.Manager.Enabled {
		t.Fatal("manager lane must ship disabled")
	}
	if cfg.Flows.Manager.MaxOpenReady != 1 {
		t.Fatalf("max_open_ready = %d, want 1", cfg.Flows.Manager.MaxOpenReady)
	}
	if cfg.Flows.Manager.PromoteTag != "forest:ready" {
		t.Fatalf("promote_tag = %q, want forest:ready", cfg.Flows.Manager.PromoteTag)
	}
}

func TestManagerConfigLoadsFromFile(t *testing.T) {
	dir := t.TempDir()
	body := `
repo: owner/repo
checks:
  - name: x
    run: echo ok
flows:
  manager:
    enabled: false
`
	if err := os.WriteFile(filepath.Join(dir, "forest.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(filepath.Join(dir, "forest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Flows.Manager.Agent != "manager" {
		t.Fatalf("manager agent = %q, want default manager", cfg.Flows.Manager.Agent)
	}
	if cfg.Flows.Manager.IntervalSec != 120 {
		t.Fatalf("interval = %d, want default 120", cfg.Flows.Manager.IntervalSec)
	}
	if cfg.Flows.Manager.Enabled {
		t.Fatal("manager must stay disabled as declared")
	}
}

// TestManagerAgentDeclaredLoads proves the manager declaration under
// agents/manager parses: a model is required, and the instructions, prompt, and
// schema files are all present, so selfcheck and a real pass can build a run.
func TestManagerAgentDeclaredLoads(t *testing.T) {
	a, err := loadAgent(".", "manager")
	if err != nil {
		t.Fatalf("loadAgent(manager): %v", err)
	}
	if a.Model == "" {
		t.Fatal("manager agent must declare a model")
	}
	if a.PromptTmpl == "" || a.Instructions == "" || a.ReportSchema == "" {
		t.Fatal("manager agent must declare instructions, prompt, and report schema")
	}
}
