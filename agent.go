package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// CommitIdentity is the author an agent uses for every commit it produces.
// It is independent of the host account that pushes the branch.
type CommitIdentity struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email"`
}

// Agent is one declared agent under agents/<name>/. A directory is the agent,
// not a prompt file: agent.yaml declares the runtime, instructions.md is the
// system prompt, prompt.md is the user-prompt template, skills/*.md add
// context, and report.schema.json is the output contract the gate enforces.
type Agent struct {
	Dir         string         `yaml:"-"`
	Name        string         `yaml:"-"`
	Description string         `yaml:"description"`
	Commit      CommitIdentity `yaml:"commit"`
	Harness     string         `yaml:"harness"`
	Model       string         `yaml:"model"`
	Variant     string         `yaml:"variant"`
	Mode        string         `yaml:"mode"`
	Temperature *float64       `yaml:"temperature"`
	// DeadlineSeconds is the required positive wall-clock bound on one run, in
	// seconds, taken from the agent declaration so each lane can set its own. A
	// run that exceeds it is cancelled and recorded as a mechanical timeout (see
	// runTimeoutError) rather than left to hold a lane forever. It bounds wall
	// time only: the step ceiling stays deleted, and token spend stays the
	// provider key's concern. loadAgent refuses a declaration that omits it or
	// sets it to zero, so every loaded agent carries a finite bound and a
	// missing configuration can never yield an unbounded run.
	DeadlineSeconds int `yaml:"deadline_seconds"`
	// No step ceiling: opencode treats an absent `steps` as unbounded, which is
	// the honest shape for work whose size is not known before an agent reads
	// the item. The wall-time bound that ends every stall lives in
	// DeadlineSeconds, which is required and finite.
	Permission map[string]string `yaml:"permission"`
	MCP        []McpSpec         `yaml:"mcp"`

	Instructions string
	PromptTmpl   string
	ReportSchema string
	DefSHA       string
}

func validAgentName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		filepath.Base(name) == name && !strings.ContainsAny(name, `/\`)
}
func loadAgent(repoDir, name string) (*Agent, error) {
	if !validAgentName(name) {
		return nil, fmt.Errorf("agent name %q must be one path segment", name)
	}
	dir := filepath.Join(repoDir, DefaultAgentsDir, name)
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("agent %s: %w", name, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("agent %s: declaration path is not a directory", name)
	}
	a := &Agent{Dir: dir, Name: name}
	b, err := os.ReadFile(filepath.Join(dir, "agent.yaml"))
	if err != nil {
		return nil, fmt.Errorf("agent %s: %w", name, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(b))
	decoder.KnownFields(true)
	if err := decoder.Decode(a); err != nil {
		return nil, fmt.Errorf("agent %s agent.yaml: %w", name, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("agent %s agent.yaml: %w", name, err)
		}
		return nil, fmt.Errorf("agent %s agent.yaml: multiple YAML documents are not allowed", name)
	}
	if a.Harness == "" {
		a.Harness = "opencode"
	}
	// No default model: a default would name one operator's provider route and
	// silently spend on it. The declaration owns this choice.
	if a.Model == "" {
		return nil, fmt.Errorf("agent %s: model is required", name)
	}
	// The wall-clock deadline is required and finite: a run that never ends
	// holds a lane forever (see #207), so a declaration that omits it or sets it
	// to zero cannot be loaded. Every loaded agent therefore carries a positive,
	// bounded run, and no configuration can create an unbounded one.
	if a.DeadlineSeconds <= 0 {
		return nil, fmt.Errorf("agent %s: deadline_seconds is required and must be a positive number of seconds", name)
	}
	if strings.TrimSpace(a.Commit.Name) == "" || strings.TrimSpace(a.Commit.Email) == "" {
		return nil, fmt.Errorf("agent %s: commit.name and commit.email are required", name)
	}
	if a.Mode == "" {
		a.Mode = "primary"
	}
	ins, err := os.ReadFile(filepath.Join(dir, "instructions.md"))
	if err != nil {
		return nil, fmt.Errorf("agent %s instructions.md: %w", name, err)
	}
	if strings.TrimSpace(string(ins)) == "" {
		return nil, fmt.Errorf("agent %s instructions.md is empty", name)
	}
	a.Instructions = string(ins)
	prompt, err := os.ReadFile(filepath.Join(dir, "prompt.md"))
	if err != nil {
		return nil, fmt.Errorf("agent %s prompt.md: %w", name, err)
	}
	if strings.TrimSpace(string(prompt)) == "" {
		return nil, fmt.Errorf("agent %s prompt.md is empty", name)
	}
	if _, err := template.New("prompt").Option("missingkey=error").Parse(string(prompt)); err != nil {
		return nil, fmt.Errorf("agent %s prompt.md: %w", name, err)
	}
	a.PromptTmpl = string(prompt)
	schema, err := os.ReadFile(filepath.Join(dir, "report.schema.json"))
	if err != nil {
		return nil, fmt.Errorf("agent %s report.schema.json: %w", name, err)
	}
	var schemaDoc struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(schema, &schemaDoc); err != nil || schemaDoc.Type != "object" {
		return nil, fmt.Errorf("agent %s report.schema.json must be a JSON object schema", name)
	}
	a.ReportSchema = string(schema)
	a.DefSHA = composeDigest(repoDir, name)
	return a, nil
}

// variantSuffix renders the reasoning variant beside the model for operator
// output, so a listing states the effort a model actually runs at instead of
// leaving it to be inferred from the model name.
func variantSuffix(variant string) string {
	if variant == "" {
		return ""
	}
	return "@" + variant
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

// renderMarkdown writes the agent's opencode markdown declaration into cfgDir's
// agents/ so `opencode run --agent <name>` loads the real system prompt and
// permissions under XDG_CONFIG_HOME. cfgDir is opencode's global config
// directory for the run and lives outside the managed worktree: nothing the
// factory renders is ever placed under the worktree's .opencode/, where a
// managed hook or a working-tree secret scanner would read it. The declaration
// is generated per run and lives only in the factory-owned config space, never
// in a repository the factory commits to.
func renderMarkdown(cfgDir string, a *Agent) error {
	if !validAgentName(a.Name) {
		return fmt.Errorf("render agent name %q must be one path segment", a.Name)
	}
	dir := filepath.Join(cfgDir, "agents")
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

// newRunConfigDir builds the per-run opencode configuration root that the child
// environment points opencode at with XDG_CONFIG_HOME. opencode reads its global
// config from <root>/opencode/: the rendered agent declaration under agents/ and
// the provider configuration under opencode.json, and it installs the provider
// packages it needs under node_modules/ there too. Because the whole root lives
// outside every worktree, moving the run's config out of the managed worktree
// neither loses the provider route the run needs (the config the factory project
// actually uses is preserved) nor leaves a dependency in a working tree a hook
// or a filesystem scanner reads. The directory is created outside every worktree
// and the caller removes it when the run completes.
func newRunConfigDir(repoDir string, a *Agent) (string, error) {
	cfgDir, err := os.MkdirTemp("", "forest-opencode-config-")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(cfgDir)
		}
	}()
	ocDir := filepath.Join(cfgDir, "opencode")
	if err := os.MkdirAll(ocDir, 0o755); err != nil {
		return "", err
	}
	if err := preserveProviderConfig(ocDir, repoDir); err != nil {
		return "", err
	}
	if err := renderMarkdown(ocDir, a); err != nil {
		return "", err
	}
	ok = true
	return cfgDir, nil
}

// preserveProviderConfig copies the provider configuration a real run actually
// uses into cfgDir as opencode.json, so the per-run config root keeps the
// provider route the run needs under XDG_CONFIG_HOME. The first source is the
// factory project's own .opencode/opencode.json (the one this program ships and
// where the operator declares the provider route and key alias); if the factory
// ships none, the operator's global opencode config is the fallback. Every
// source is outside every worktree, so a managed repository never has to carry
// it. A configuration the run can supply from elsewhere (for example the managed
// repository's own project config) does not require one here: a missing file is
// tolerated, not an error.
func preserveProviderConfig(cfgDir, repoDir string) error {
	src := projectProviderConfigPath(repoDir)
	if src == "" {
		src = openCodeProviderConfigPath()
	}
	if src == "" {
		return nil
	}
	b, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read provider config %s: %w", src, err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "opencode.json"), b, 0o600); err != nil {
		return fmt.Errorf("stage opencode provider config into run config: %w", err)
	}
	return nil
}

// projectProviderConfigPath returns the factory project's own opencode provider
// configuration — the one a real run actually uses when opencode reads its
// global config — or "" when the factory ships none.
func projectProviderConfigPath(repoDir string) string {
	p := filepath.Join(repoDir, ".opencode", "opencode.json")
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p
	}
	return ""
}

// openCodeProviderConfigPath returns the operator's global opencode provider
// configuration path, or "" when the process has no usable config directory. It
// honours XDG_CONFIG_HOME and otherwise falls back to the user's ~/.config.
func openCodeProviderConfigPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode", "opencode.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "opencode", "opencode.json")
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
// The stored item body stays unchanged. The id is the opaque string the
// controller carries, with no GitHub integer behind it.
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
// id is the same opaque string issueData exposes, so one prompt template
// renders either lane.
func reviewData(it Item, rep report, diff string) map[string]any {
	return map[string]any{
		"ID":     it.ID,
		"Title":  it.Title,
		"Body":   it.Body,
		"Report": rep.Summary,
		"Diff":   diff,
	}
}
