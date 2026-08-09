package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rendered(t *testing.T, cfgDir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cfgDir, "agents", name+".md"))
	if err != nil {
		t.Fatalf("read rendered agent: %v", err)
	}
	return string(b)
}

// renderCfg builds a per-run opencode config directory outside any worktree and
// returns it, so a caller can assert both that the declaration rendered where
// the factory owns it and that the worktree stayed clean.
func renderCfg(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestRenderMarkdownCarriesVariant(t *testing.T) {
	cfgDir := renderCfg(t, "forest-opencode-config-")
	wtDir := t.TempDir()
	a := &Agent{
		Name: "verifier", Dir: t.TempDir(), Description: "independent reviewer",
		Model: "openrouter/openai/gpt-5.6-luna", Variant: "max", Mode: "primary",
		Instructions: "judge the change",
		Permission:   map[string]string{"read": "allow", "edit": "deny"},
	}
	if err := renderMarkdown(cfgDir, a); err != nil {
		t.Fatal(err)
	}
	md := rendered(t, cfgDir, "verifier")
	for _, want := range []string{
		"variant: max", "model: openrouter/openai/gpt-5.6-luna",
		"read: allow", "edit: deny",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("rendered agent lacks %q:\n%s", want, md)
		}
	}
	if _, err := os.Stat(filepath.Join(wtDir, ".opencode")); !os.IsNotExist(err) {
		t.Fatalf("renderMarkdown wrote .opencode into the worktree: %v", err)
	}
}

func TestRenderMarkdownTemperatureIsOptional(t *testing.T) {
	base := func() *Agent {
		return &Agent{Name: "verifier", Dir: t.TempDir(), Model: "m", Mode: "primary",
			Instructions: "judge the change"}
	}
	omitted := renderCfg(t, "forest-opencode-config-")
	if err := renderMarkdown(omitted, base()); err != nil {
		t.Fatal(err)
	}
	if md := rendered(t, omitted, "verifier"); strings.Contains(md, "temperature") {
		t.Fatalf("undeclared temperature was rendered:\n%s", md)
	}

	temp := 0.2
	a := base()
	a.Temperature = &temp
	declared := renderCfg(t, "forest-opencode-config-")
	if err := renderMarkdown(declared, a); err != nil {
		t.Fatal(err)
	}
	if md := rendered(t, declared, "verifier"); !strings.Contains(md, "temperature: 0.2") {
		t.Fatalf("declared temperature was dropped:\n%s", md)
	}
}

func TestIssueDataFeedsComments(t *testing.T) {
	it := Item{
		ID:    "92",
		Title: "feed a rejection back",
		Body:  "implement the change",
		Comments: []comment{
			{CreatedAt: "2026-08-05T10:00:00Z", Body: "review rejected: keep ordering deterministic"},
			{CreatedAt: "2026-08-05T09:00:00Z", Body: "operator note: cap the volume"},
		},
	}
	data := issueData(it, "")
	body, _ := data["Body"].(string)
	if !strings.Contains(body, "operator note") || !strings.Contains(body, "review rejected") {
		t.Fatalf("prompt body dropped a comment:\n%s", body)
	}
	if strings.Index(body, "operator note") > strings.Index(body, "review rejected") {
		t.Fatalf("comments are not ordered oldest first:\n%s", body)
	}
	if _, ok := data["Comments"]; !ok {
		t.Fatalf("prompt data did not expose Comments")
	}
	// The prompt exposes the opaque string id as "ID", not the GitHub number.
	if id, _ := data["ID"].(string); id != "92" {
		t.Fatalf("prompt data ID = %q, want 92", id)
	}
}

func TestRenderCommentsCapsVolume(t *testing.T) {
	long := strings.Repeat("x", maxCommentBytes+100)
	out := renderComments([]comment{{CreatedAt: "t", Body: long}, {CreatedAt: "u", Body: long}})
	if len(out) > maxCommentBytes+10 {
		t.Fatalf("comment block grew past the cap: %d bytes", len(out))
	}
}

func TestRenderCommentsCapsShortComments(t *testing.T) {
	var cs []comment
	for i := range maxCommentBytes {
		cs = append(cs, comment{CreatedAt: fmt.Sprintf("t%04d", i), Body: "x"})
	}
	out := renderComments(cs)
	if len(out) > maxCommentBytes {
		t.Fatalf("short-comment block exceeded the cap: %d bytes", len(out))
	}
}

func TestRenderCommentsKeepsNewest(t *testing.T) {
	var cs []comment
	for i := range 4000 {
		cs = append(cs, comment{CreatedAt: fmt.Sprintf("t%04d", i), Body: "o"})
	}
	cs = append(cs, comment{CreatedAt: "zzzzz", Body: "review rejected: do it differently"})
	out := renderComments(cs)
	if !strings.Contains(out, "review rejected") {
		t.Fatalf("newest rejection was dropped:\n%s", out)
	}
	if !strings.HasPrefix(out, "- o") {
		t.Fatalf("block is not ordered oldest first:\n%s", out)
	}
}

func TestLoadAgentAndDigestChanges(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "forest.yaml"), []byte("repo: owner/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeAgentFixture(t, repoDir, "builder", "builder-model")
	a, err := loadAgent(repoDir, "builder")
	if err != nil {
		t.Fatal(err)
	}
	if a.DefSHA == "" || a.Model != "builder-model" ||
		a.Commit.Name != "builder" || a.Commit.Email != "builder@example.invalid" {
		t.Fatalf("loaded agent = %#v", a)
	}
	first := a.DefSHA
	path := filepath.Join(repoDir, DefaultAgentsDir, "builder", "agent.yaml")
	if err := os.WriteFile(path, []byte("description: builder\ncommit:\n  name: builder\n  email: builder@example.invalid\nmodel: changed-model\ndeadline_seconds: 3600\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := loadAgent(repoDir, "builder")
	if err != nil {
		t.Fatal(err)
	}
	if b.DefSHA == first {
		t.Fatalf("definition digest did not change: %q", first)
	}
}

func TestDeclaredAgentsLoad(t *testing.T) {
	repoDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(filepath.Join(repoDir, "forest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	agents := map[string]string{
		"builder":  cfg.Flows.Builder.Agent,
		"verifier": cfg.Flows.Verifier.Agent,
		"fixer":    cfg.Flows.Fixer.Agent,
		"manager":  cfg.Flows.Manager.Agent,
	}
	for _, f := range flowsFor() {
		name, ok := agents[f.Name()]
		if !ok || name == "" {
			t.Fatalf("flow %q has no configured agent", f.Name())
		}
		a, err := loadAgent(repoDir, name)
		if err != nil {
			t.Fatalf("flow %q loadAgent(%q): %v", f.Name(), name, err)
		}
		if a.Name != name || a.Model == "" || a.DefSHA == "" {
			t.Fatalf("incomplete %s declaration for flow %s: %#v", name, f.Name(), a)
		}
	}
}

func TestLoadAgentRequiresPromptAndSchema(t *testing.T) {
	for _, name := range []string{"prompt.md", "report.schema.json"} {
		t.Run(name, func(t *testing.T) {
			repoDir := t.TempDir()
			writeAgentFixture(t, repoDir, "builder", "builder-model")
			if err := os.Remove(filepath.Join(repoDir, DefaultAgentsDir, "builder", name)); err != nil {
				t.Fatal(err)
			}
			if _, err := loadAgent(repoDir, "builder"); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("loadAgent without %s = %v, want declaration error", name, err)
			}
		})
	}
}

func TestLoadAgentRejectsAdditionalYAMLDocument(t *testing.T) {
	repoDir := t.TempDir()
	writeAgentFixture(t, repoDir, "builder", "builder-model")
	path := filepath.Join(repoDir, DefaultAgentsDir, "builder", "agent.yaml")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("---\nmodel: ignored-model\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgent(repoDir, "builder"); err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("loadAgent with second YAML document = %v, want refusal", err)
	}
}

func TestLoadAgentRejectsMalformedPromptAndSchema(t *testing.T) {
	tests := []struct {
		file string
		body string
	}{
		{"prompt.md", "{{"},
		{"report.schema.json", `[]`},
		{"report.schema.json", `{"type":"array"}`},
	}
	for _, tc := range tests {
		repoDir := t.TempDir()
		writeAgentFixture(t, repoDir, "builder", "builder-model")
		path := filepath.Join(repoDir, DefaultAgentsDir, "builder", tc.file)
		if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAgent(repoDir, "builder"); err == nil {
			t.Fatalf("loadAgent accepted malformed %s: %s", tc.file, tc.body)
		}
	}
}

func TestLoadRejectsMissingOrZeroDeadline(t *testing.T) {
	// A run that never ends holds a lane forever (see #207), so an agent whose
	// declaration omits deadline_seconds or sets it to zero must not load: every
	// loaded agent has to carry a positive, finite wall-clock bound. Without
	// this guard a missing line would default to zero and silently open an
	// unbounded run.
	for _, deadline := range []string{"", "deadline_seconds: 0\n"} {
		repoDir := t.TempDir()
		writeAgentFixture(t, repoDir, "builder", "builder-model")
		body := "description: builder\ncommit:\n  name: builder\n  email: builder@example.invalid\n" +
			"model: builder-model\n" + deadline
		path := filepath.Join(repoDir, DefaultAgentsDir, "builder", "agent.yaml")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAgent(repoDir, "builder"); err == nil ||
			!strings.Contains(err.Error(), "deadline_seconds") {
			t.Fatalf("loadAgent without finite deadline = %v\n%s", err, body)
		}
	}
}

func TestLoadRejectsMissingCommitIdentity(t *testing.T) {
	for _, body := range []string{
		"description: builder\nmodel: builder-model\ndeadline_seconds: 3600\n",
		"description: builder\ncommit:\n  name: builder\nmodel: builder-model\ndeadline_seconds: 3600\n",
		"description: builder\ncommit:\n  email: builder@example.invalid\nmodel: builder-model\ndeadline_seconds: 3600\n",
	} {
		repoDir := t.TempDir()
		dir := filepath.Join(repoDir, DefaultAgentsDir, "builder")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("do the work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAgent(repoDir, "builder"); err == nil ||
			!strings.Contains(err.Error(), "commit.name and commit.email are required") {
			t.Fatalf("loadAgent missing commit identity = %v\n%s", err, body)
		}
	}
}

func TestLoadAgentRejectsPathAndIdentityOverrides(t *testing.T) {
	repoDir := t.TempDir()
	if _, err := loadAgent(repoDir, "../outside"); err == nil {
		t.Fatal("loadAgent accepted a path outside agents/")
	}
	for _, override := range []string{
		"name: ../../tmp/evil\n",
		"dir: ../../tmp/evil\n",
	} {
		repoDir := t.TempDir()
		writeAgentFixture(t, repoDir, "builder", "builder-model")
		path := filepath.Join(repoDir, DefaultAgentsDir, "builder", "agent.yaml")
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(override); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAgent(repoDir, "builder"); err == nil {
			t.Fatalf("loadAgent accepted reserved identity field %q", strings.TrimSpace(override))
		}
	}
	if err := renderMarkdown(t.TempDir(), &Agent{Name: "../evil"}); err == nil {
		t.Fatal("renderMarkdown accepted an escaping agent name")
	}
}

func TestRunCategory(t *testing.T) {
	for _, status := range []string{"built", "reviewed", "merged", "fixed", "done", "reaped"} {
		if got := runCategory(status); got != "progress" {
			t.Errorf("runCategory(%q) = %q, want progress", status, got)
		}
	}
	if got := runCategory("skipped"); got != "other" {
		t.Errorf("runCategory(skipped) = %q, want other", got)
	}
	for _, status := range []string{"agent_failed", "gate_failed", "merge_failed", "flow_failed"} {
		if got := runCategory(status); got != "failed" {
			t.Errorf("runCategory(%q) = %q, want failed", status, got)
		}
	}
	if got := runCategory("unknown"); got != "other" {
		t.Errorf("runCategory(unknown) = %q, want other", got)
	}
}

// TestPublishedBranchHasNoOpenCodePath pins the outcome invariant: after a full
// pass on a repository with no factory-specific ignore or exclude entries, the
// published branch must contain no .opencode path. The rendered declaration is
// written by renderMarkdown into a factory-owned directory outside the worktree,
// so even `git add -A` cannot reach it regardless of the managed repository's
// ignore rules. This is what keeps a hook or a working-tree secret scanner from
// ever reading a factory artifact inside a repository the factory commits to.
func TestPublishedBranchHasNoOpenCodePath(t *testing.T) {
	wtDir := t.TempDir()
	runGitTest(t, wtDir, "init", "-b", "master")
	runGitTest(t, wtDir, "config", "user.email", "test@example.com")
	runGitTest(t, wtDir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(wtDir, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, wtDir, "add", "-A")
	runGitTest(t, wtDir, "commit", "-m", "init")
	runGitTest(t, wtDir, "tag", "base")

	// Render the agent into a factory-owned config directory outside the
	// worktree, exactly as runPhase does, then stage the whole working tree the
	// way publish does. The rendered declaration must not appear in the commit.
	cfgDir := renderCfg(t, "forest-opencode-config-")
	a := &Agent{Name: "probe", Model: "m", Mode: "primary", Instructions: "do work"}
	if err := renderMarkdown(cfgDir, a); err != nil {
		t.Fatal(err)
	}
	decl := filepath.Join(cfgDir, "agents", "probe.md")
	if _, err := os.Stat(decl); err != nil {
		t.Fatalf("rendered declaration missing: %v", err)
	}

	// The agent's real change lands in the worktree; publish stages it with
	// git add -A. The rendered declaration must not ride along.
	if err := os.WriteFile(filepath.Join(wtDir, "result.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, wtDir, "add", "-A")
	runGitTest(t, wtDir, "commit", "-m", "publish")
	tree := runGitTest(t, wtDir, "ls-tree", "-r", "HEAD", "--name-only")
	for _, p := range strings.Split(strings.TrimSpace(tree), "\n") {
		if strings.HasPrefix(p, ".opencode") {
			t.Fatalf("published branch contains factory path %q", p)
		}
	}
	if strings.Contains(tree, "probe.md") {
		t.Fatalf("published branch contains the rendered declaration:\n%s", tree)
	}
	if !strings.Contains(tree, "result.go") {
		t.Fatalf("published branch lost the agent's change:\n%s", tree)
	}
}
