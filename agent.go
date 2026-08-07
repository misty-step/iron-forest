package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// McpSpec declares one MCP server the agent may reach. The server wiring
// (type/url/headers) lives in the opencode config; this per-agent entry names
// it and switches it on or off. The header value is a Mint marker, never a
// credential byte.
type McpSpec struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"`
	URL     string `yaml:"url"`
	Header  string `yaml:"header"`
	Enabled bool   `yaml:"enabled"`
}

// Agent is one declared agent under agents/<name>/. A directory is the agent,
// not a prompt file: agent.yaml declares the runtime, instructions.md is the
// system prompt, prompt.md is the user-prompt template, skills/*.md add
// context, and report.schema.json is the output contract the gate enforces.
type Agent struct {
	Dir         string
	Name        string
	Description string   `yaml:"description"`
	Harness     string   `yaml:"harness"`
	Model       string   `yaml:"model"`
	Variant     string   `yaml:"variant"`
	Mode        string   `yaml:"mode"`
	Temperature *float64 `yaml:"temperature"`
	// No step ceiling and no deadline: opencode treats an absent `steps` as
	// unbounded, which is the honest shape for work whose size is not known
	// before an agent reads the item.
	Permission map[string]string `yaml:"permission"`
	MCP        []McpSpec         `yaml:"mcp"`

	Instructions string
	PromptTmpl   string
	ReportSchema string
	DefSHA       string
}

func loadAgent(repoDir, name string) (*Agent, error) {
	dir := filepath.Join(repoDir, DefaultAgentsDir, name)
	a := &Agent{Dir: dir, Name: name}
	b, err := os.ReadFile(filepath.Join(dir, "agent.yaml"))
	if err != nil {
		return nil, fmt.Errorf("agent %s: %w", name, err)
	}
	if err := yaml.Unmarshal(b, a); err != nil {
		return nil, fmt.Errorf("agent %s agent.yaml: %w", name, err)
	}
	if a.Harness == "" {
		a.Harness = "opencode"
	}
	// No default model: a default would name one operator's provider route and
	// silently spend on it. The declaration owns this choice.
	if a.Model == "" {
		return nil, fmt.Errorf("agent %s: model is required", name)
	}
	if a.Mode == "" {
		a.Mode = "primary"
	}
	ins, err := os.ReadFile(filepath.Join(dir, "instructions.md"))
	if err != nil {
		return nil, fmt.Errorf("agent %s instructions.md: %w", name, err)
	}
	a.Instructions = string(ins)
	if t, err := os.ReadFile(filepath.Join(dir, "prompt.md")); err == nil {
		a.PromptTmpl = string(t)
	}
	if s, err := os.ReadFile(filepath.Join(dir, "report.schema.json")); err == nil {
		a.ReportSchema = string(s)
	}
	a.DefSHA = composeDigest(repoDir, name)
	return a, nil
}

// variantSuffix renders the reasoning variant beside the model for operator
// output, so a listing states the effort a model actually runs at instead of
// leaving it to be inferred from the model name.
func variantSuffix(a *Agent) string {
	if a.Variant == "" {
		return ""
	}
	return "@" + a.Variant
}

// discoverAgents lists the declared agents under agents/.
func discoverAgents(repoDir string) ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(repoDir, DefaultAgentsDir))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// composeDigest fingerprints the whole agent definition: every file under
// agents/<name>/ plus forest.yaml. It is the per-run composition digest that
// makes a run reproducible and comparable.
func composeDigest(repoDir, name string) string {
	h := sha256.New()
	dir := filepath.Join(repoDir, DefaultAgentsDir, name)
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(repoDir, p)
		if err != nil {
			return nil
		}
		h.Write([]byte(rel))
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		_, _ = io.Copy(h, f)
		return nil
	})
	if b, err := os.ReadFile(filepath.Join(repoDir, "forest.yaml")); err == nil {
		h.Write([]byte{0})
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// renderMarkdown writes the agent's opencode markdown declaration into the
// worktree so `opencode run --agent <name>` loads the real system prompt and
// permissions. The file is generated per run and ignored by git.
func renderMarkdown(wtDir string, a *Agent) error {
	dir := filepath.Join(wtDir, ".opencode", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	perm := make(map[string]string, len(a.Permission))
	for k, v := range a.Permission {
		perm[k] = v
	}
	// Deny every declared-but-disabled MCP so the agent cannot reach it even
	// though the global opencode config still declares the server.
	for _, m := range a.MCP {
		if !m.Enabled {
			perm["mcp__"+m.Name+"__*"] = "deny"
			perm["mcp__"+m.Name+"_*"] = "deny"
		}
	}
	type frontmatter struct {
		Description string            `yaml:"description"`
		Model       string            `yaml:"model"`
		Variant     string            `yaml:"variant,omitempty"`
		Mode        string            `yaml:"mode"`
		Temperature *float64          `yaml:"temperature,omitempty"`
		Permission  map[string]string `yaml:"permission"`
	}
	fm, err := yaml.Marshal(frontmatter{
		Description: a.Description, Model: a.Model, Variant: a.Variant, Mode: a.Mode,
		Temperature: a.Temperature, Permission: perm,
	})
	if err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.Write(fm)
	sb.WriteString("---\n")
	sb.WriteString(strings.TrimSpace(a.Instructions))
	for _, sf := range agentSkills(a.Dir) {
		b, err := os.ReadFile(sf)
		if err != nil {
			continue
		}
		sb.WriteString("\n\n# Skill: " + filepath.Base(sf) + "\n")
		sb.WriteString(strings.TrimSpace(string(b)))
	}
	if a.ReportSchema != "" {
		sb.WriteString("\n\n# Your output contract\n")
		sb.WriteString(a.ReportSchema)
	}
	return os.WriteFile(filepath.Join(dir, a.Name+".md"), []byte(sb.String()), 0o644)
}

// agentSkills lists the agent's skill markdown files in stable order.
func agentSkills(dir string) []string {
	files, _ := filepath.Glob(filepath.Join(dir, "skills", "*.md"))
	sort.Strings(files)
	return files
}

// renderUserPrompt renders the agent's prompt.md template with per-run data,
// or a task-only default when the agent declares no template.
func renderUserPrompt(a *Agent, data map[string]any) (string, error) {
	src := a.PromptTmpl
	if strings.TrimSpace(src) == "" {
		src = "{{.Task}}"
	}
	tmpl, err := template.New(a.Name + "-prompt").Parse(src)
	if err != nil {
		return "", fmt.Errorf("agent %s prompt.md: %w", a.Name, err)
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, data); err != nil {
		return "", err
	}
	return strings.TrimSpace(sb.String()), nil
}

// issueData feeds a builder or fixer run: the item plus an optional revision
// request. Comments carry earlier feedback into the same prompt as the task.
// The stored item body stays unchanged. The id is exposed as "ID", the opaque
// string the controller carries, not as a GitHub number.
func issueData(it Item, revision string) map[string]any {
	body := it.Body
	if comments := renderComments(it.Comments); comments != "" {
		body += "\n\n## Item comments\n" + comments
	}
	return map[string]any{
		"ID":       it.ID,
		"Title":    it.Title,
		"Body":     body,
		"Comments": it.Comments,
		"Revision": revision,
	}
}

// maxCommentBytes caps how much of an item's comment thread a prompt carries,
// so a single noisy thread cannot crowd out the task text.
const maxCommentBytes = 2000

// renderComments joins an item's comments into one deterministic block, oldest
// first, capped in volume. Creation time keeps retries stable across runs.
// The full rendered block stays within maxCommentBytes.
// Newest comments are selected first so the latest objection remains visible.
func renderComments(cs []comment) string {
	sorted := append([]comment(nil), cs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt < sorted[j].CreatedAt
	})

	const overhead = 3 // "- " prefix plus trailing "\n" per rendered comment
	// keep records how many body bytes each selected comment renders. Walk the
	// newest comment first so an over-full thread holds onto the rejection the
	// next run must read rather than the oldest filler.
	remain := maxCommentBytes
	keep := make(map[int]string, len(sorted))
	for i := len(sorted) - 1; i >= 0; i-- {
		b := strings.TrimSpace(sorted[i].Body)
		if b == "" || remain <= overhead {
			continue
		}
		if head := len(b); head+overhead > remain {
			head = remain - overhead
			b = b[:head]
		}
		keep[i] = b
		remain -= len(b) + overhead
	}

	var sb strings.Builder
	for i := range sorted {
		if b, ok := keep[i]; ok {
			sb.WriteString("- ")
			sb.WriteString(b)
			sb.WriteString("\n")
		}
	}
	return strings.TrimSpace(sb.String())
}

// reviewData feeds a review run: the item plus the author report and diff. The
// id is exposed as "ID", matching issueData, so one prompt template renders
// either lane.
func reviewData(it Item, rep report, diff string) map[string]any {
	return map[string]any{
		"ID":     it.ID,
		"Title":  it.Title,
		"Body":   it.Body,
		"Report": rep.Summary,
		"Diff":   diff,
	}
}
