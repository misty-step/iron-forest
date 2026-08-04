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
	Dir             string
	Name            string
	Description     string            `yaml:"description"`
	Harness         string            `yaml:"harness"`
	Model           string            `yaml:"model"`
	Mode            string            `yaml:"mode"`
	Temperature     float64           `yaml:"temperature"`
	Steps           int               `yaml:"steps"`
	BudgetSec       int               `yaml:"budget_seconds"`
	PriceInUSDPerM  float64           `yaml:"price_usd_per_m_input"`
	PriceOutUSDPerM float64           `yaml:"price_usd_per_m_output"`
	Permission      map[string]string `yaml:"permission"`
	MCP             []McpSpec         `yaml:"mcp"`

	Instructions string // instructions.md body (the system prompt)
	PromptTmpl   string // prompt.md body (the user-prompt template)
	ReportSchema string // report.schema.json document
	DefSHA       string // composition digest of the whole definition
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
	if a.Model == "" {
		a.Model = modelDefault
	}
	if a.Mode == "" {
		a.Mode = "primary"
	}
	if a.Temperature == 0 {
		a.Temperature = 0.2
	}
	if a.Steps == 0 {
		a.Steps = 50
	}
	if a.BudgetSec == 0 {
		a.BudgetSec = 1200
	}
	if a.PriceInUSDPerM == 0 {
		a.PriceInUSDPerM = 0.09
	}
	if a.PriceOutUSDPerM == 0 {
		a.PriceOutUSDPerM = 0.18
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
		Mode        string            `yaml:"mode"`
		Temperature float64           `yaml:"temperature"`
		Steps       int               `yaml:"steps"`
		Permission  map[string]string `yaml:"permission"`
	}
	fm, err := yaml.Marshal(frontmatter{
		Description: a.Description, Model: a.Model, Mode: a.Mode,
		Temperature: a.Temperature, Steps: a.Steps, Permission: perm,
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

// issueData feeds a chew run: the item plus an optional revision request.
func issueData(it issue, revision string) map[string]any {
	return map[string]any{
		"Number":   it.Number,
		"Title":    it.Title,
		"Body":     it.Body,
		"Revision": revision,
	}
}

// reviewData feeds a review run: the item plus the author report and diff.
func reviewData(it issue, rep report, diff string) map[string]any {
	return map[string]any{
		"Number": it.Number,
		"Title":  it.Title,
		"Body":   it.Body,
		"Report": rep.Summary,
		"Diff":   diff,
	}
}

