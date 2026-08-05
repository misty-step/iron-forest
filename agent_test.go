package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rendered returns the agent markdown renderMarkdown wrote into a worktree.
func rendered(t *testing.T, wtDir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(wtDir, ".opencode", "agents", name+".md"))
	if err != nil {
		t.Fatalf("read rendered agent: %v", err)
	}
	return string(b)
}

// writeAgentFixture creates the minimum agent directory loadAgent accepts.
func writeAgentFixture(t *testing.T, repoDir, name, model string) {
	t.Helper()
	dir := filepath.Join(repoDir, DefaultAgentsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "description: " + name + "\nmodel: " + model + "\n"
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("do the work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRenderMarkdownCarriesVariant pins the reasoning-effort contract. opencode
// accepts a variant it cannot honour and then drops it without a word, so a
// variant missing from the frontmatter buys the dearest model on the menu with
// none of the reasoning it was chosen for, and no run would report the loss.
func TestRenderMarkdownCarriesVariant(t *testing.T) {
	wtDir := t.TempDir()
	a := &Agent{
		Name: "owl", Dir: t.TempDir(), Description: "reviewer",
		Model: "openrouter/openai/gpt-5.6-luna", Variant: "max",
		Mode: "primary", Steps: 30, Instructions: "judge the change",
	}
	if err := renderMarkdown(wtDir, a); err != nil {
		t.Fatal(err)
	}
	if md := rendered(t, wtDir, "owl"); !strings.Contains(md, "variant: max") {
		t.Fatalf("rendered agent dropped its variant:\n%s", md)
	}
}

// TestRenderMarkdownTemperatureIsOptional pins temperature to the declaration.
// Reasoning models publish no temperature parameter, so the renderer must send
// one only when an agent asks for it, and must still send a declared value.
func TestRenderMarkdownTemperatureIsOptional(t *testing.T) {
	base := func() *Agent {
		return &Agent{Name: "owl", Dir: t.TempDir(), Model: "m", Mode: "primary",
			Steps: 30, Instructions: "judge the change"}
	}
	omitted := t.TempDir()
	if err := renderMarkdown(omitted, base()); err != nil {
		t.Fatal(err)
	}
	if md := rendered(t, omitted, "owl"); strings.Contains(md, "temperature") {
		t.Fatalf("undeclared temperature was rendered anyway:\n%s", md)
	}

	temp := 0.2
	a := base()
	a.Temperature = &temp
	declared := t.TempDir()
	if err := renderMarkdown(declared, a); err != nil {
		t.Fatal(err)
	}
	if md := rendered(t, declared, "owl"); !strings.Contains(md, "temperature: 0.2") {
		t.Fatalf("declared temperature was dropped:\n%s", md)
	}
}

// TestIdentifyRecordsBothPhases pins ledger attribution. The reviewer runs on a
// different model family from the builder and costs several times more per
// token, so a run row naming one model cannot say who judged the change or
// where the money went.
func TestIdentifyRecordsBothPhases(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "forest.yaml"), []byte("repo: owner/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeAgentFixture(t, repoDir, "beaver", "openrouter-mint/deepseek-v4-flash-0731")
	writeAgentFixture(t, repoDir, "owl", "openrouter/openai/gpt-5.6-luna")
	cfg := defaultConfig()

	id := identify(repoDir, cfg)
	if id.Agents != "beaver,owl" {
		t.Errorf("agents = %q, want beaver,owl", id.Agents)
	}
	want := "openrouter-mint/deepseek-v4-flash-0731,openrouter/openai/gpt-5.6-luna"
	if id.Models != want {
		t.Errorf("models = %q, want %q", id.Models, want)
	}
	digests := strings.Split(id.DefSHA, ",")
	if len(digests) != 2 {
		t.Fatalf("def_sha = %q, want one digest per agent", id.DefSHA)
	}
	if digests[0] == digests[1] || digests[0] == "" {
		t.Errorf("def_sha = %q, want a distinct digest per agent", id.DefSHA)
	}
}

// TestIdentifySkipsUnreadableAgent pins attribution as non-blocking: a review
// agent that cannot be loaded must not erase the builder's identity, because a
// run that produced work still has to be recorded.
func TestIdentifySkipsUnreadableAgent(t *testing.T) {
	repoDir := t.TempDir()
	writeAgentFixture(t, repoDir, "beaver", "openrouter-mint/deepseek-v4-flash-0731")
	cfg := defaultConfig()
	cfg.Workflow.Review = "ghost"

	id := identify(repoDir, cfg)
	if id.Agents != "beaver" {
		t.Errorf("agents = %q, want beaver alone", id.Agents)
	}
	if strings.Contains(id.Models, ",") {
		t.Errorf("models = %q, want no empty trailing entry", id.Models)
	}
}
