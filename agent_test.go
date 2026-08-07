package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rendered(t *testing.T, wtDir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(wtDir, ".opencode", "agents", name+".md"))
	if err != nil {
		t.Fatalf("read rendered agent: %v", err)
	}
	return string(b)
}

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

func TestRenderMarkdownCarriesVariant(t *testing.T) {
	wtDir := t.TempDir()
	a := &Agent{
		Name: "verifier", Dir: t.TempDir(), Description: "independent reviewer",
		Model: "openrouter/openai/gpt-5.6-luna", Variant: "max", Mode: "primary",
		Instructions: "judge the change",
		Permission:   map[string]string{"read": "allow", "edit": "deny"},
	}
	if err := renderMarkdown(wtDir, a); err != nil {
		t.Fatal(err)
	}
	md := rendered(t, wtDir, "verifier")
	for _, want := range []string{
		"variant: max", "model: openrouter/openai/gpt-5.6-luna",
		"read: allow", "edit: deny",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("rendered agent lacks %q:\n%s", want, md)
		}
	}
}

func TestRenderMarkdownTemperatureIsOptional(t *testing.T) {
	base := func() *Agent {
		return &Agent{Name: "verifier", Dir: t.TempDir(), Model: "m", Mode: "primary",
			Instructions: "judge the change"}
	}
	omitted := t.TempDir()
	if err := renderMarkdown(omitted, base()); err != nil {
		t.Fatal(err)
	}
	if md := rendered(t, omitted, "verifier"); strings.Contains(md, "temperature") {
		t.Fatalf("undeclared temperature was rendered:\n%s", md)
	}

	temp := 0.2
	a := base()
	a.Temperature = &temp
	declared := t.TempDir()
	if err := renderMarkdown(declared, a); err != nil {
		t.Fatal(err)
	}
	if md := rendered(t, declared, "verifier"); !strings.Contains(md, "temperature: 0.2") {
		t.Fatalf("declared temperature was dropped:\n%s", md)
	}
}

func TestIssueDataFeedsComments(t *testing.T) {
	it := issue{
		Number: 92,
		Title:  "feed a rejection back",
		Body:   "implement the change",
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
	if a.DefSHA == "" || a.Model != "builder-model" {
		t.Fatalf("loaded agent = %#v", a)
	}
	first := a.DefSHA
	path := filepath.Join(repoDir, DefaultAgentsDir, "builder", "agent.yaml")
	if err := os.WriteFile(path, []byte("description: builder\nmodel: changed-model\n"), 0o644); err != nil {
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
	for _, name := range []string{"builder", "verifier"} {
		a, err := loadAgent(repoDir, name)
		if err != nil {
			t.Fatalf("loadAgent(%q): %v", name, err)
		}
		if a.Name != name || a.Model == "" || a.DefSHA == "" {
			t.Fatalf("incomplete %s declaration: %#v", name, a)
		}
	}
}

func TestRunCategory(t *testing.T) {
	for _, status := range []string{"built", "reviewed", "merged", "fixed"} {
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
