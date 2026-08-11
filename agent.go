package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"

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
	// BashAllow names command patterns that may use OpenCode's shell tool.
	// A present empty list denies the shell. The child sandbox remains the
	// security boundary because an allowed compiler or Git command can execute
	// other programs.
	BashAllow []string  `yaml:"bash_allow"`
	MCP       []McpSpec `yaml:"mcp"`

	Instructions string
	PromptTmpl   string
	ReportSchema string
	DefSHA       string
}

func validAgentName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		filepath.Base(name) == name && !strings.ContainsAny(name, `/\`)
}

func bashPatternPrefix(pattern string) (string, error) {
	if pattern == "" || pattern != strings.TrimSpace(pattern) ||
		strings.ContainsAny(pattern, "\r\n") || !strings.HasSuffix(pattern, " *") {
		return "", fmt.Errorf("bash_allow pattern %q must be a plain command prefix ending in \" *\"", pattern)
	}
	prefix := strings.TrimSuffix(pattern, " *")
	if prefix == "" || strings.Join(strings.Fields(prefix), " ") != prefix {
		return "", fmt.Errorf("bash_allow pattern %q must use single spaces", pattern)
	}
	for _, r := range prefix {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || strings.ContainsRune(`._+:/%@=,- `, r) {
			continue
		}
		return "", fmt.Errorf("bash_allow pattern %q contains a shell metacharacter", pattern)
	}
	return prefix, nil
}

func validateBashAllow(patterns []string) error {
	seen := make(map[string]struct{}, len(patterns))
	for _, pattern := range patterns {
		if _, err := bashPatternPrefix(pattern); err != nil {
			return err
		}
		if _, exists := seen[pattern]; exists {
			return fmt.Errorf("bash_allow pattern %q is duplicated", pattern)
		}
		seen[pattern] = struct{}{}
	}
	return nil
}

// bashPermissionNode preserves rule order because OpenCode applies the last
// matching rule. Named commands follow the catch-all deny. Shell operators
// follow every allow and therefore cannot smuggle a second command through a
// broad command prefix. Bubblewrap still contains every allowed executable.
func bashPermissionNode(patterns []string) *yaml.Node {
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	add := func(pattern, effect string) {
		style := yaml.SingleQuotedStyle
		if strings.ContainsAny(pattern, "\r\n") {
			style = yaml.DoubleQuotedStyle
		}
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: pattern, Style: style},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: effect},
		)
	}
	add("*", "deny")
	for _, pattern := range patterns {
		add(pattern, "allow")
	}
	for _, pattern := range []string{
		"*\n*", "*\r*", "*;*", "*|*", "*&*", "*`*", "*$(*", "*>*", "*<*", "*!*",
	} {
		add(pattern, "deny")
	}
	return node
}

// readRepositoryFile opens one regular file through an os.Root. The rooted
// lookup rejects every symlink that escapes the repository before any Host
// bytes can enter an agent declaration or per-run provider configuration.
func readRepositoryFile(repoDir, path string) ([]byte, error) {
	rel, err := filepath.Rel(repoDir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("repository file %q is outside %q", path, repoDir)
	}
	root, err := os.OpenRoot(repoDir)
	if err != nil {
		return nil, fmt.Errorf("open repository root: %w", err)
	}
	defer root.Close()
	info, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("repository file %q must not be a symbolic link", rel)
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("repository file %q is not regular", rel)
	}
	return io.ReadAll(file)
}

const maxAgentDeadlineSeconds = int64(1<<63-1) / int64(time.Second)

func agentDeadline(seconds int) (time.Duration, error) {
	if seconds <= 0 || int64(seconds) > maxAgentDeadlineSeconds {
		return 0, fmt.Errorf("deadline_seconds must be positive and fit a time.Duration")
	}
	return time.Duration(seconds) * time.Second, nil
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
	b, err := readRepositoryFile(repoDir, filepath.Join(dir, "agent.yaml"))
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
	if a.Harness != "opencode" {
		return nil, fmt.Errorf("agent %s: harness %q is unsupported", name, a.Harness)
	}
	// No default model: a default would name one operator's provider route and
	// silently spend on it. The declaration owns this choice.
	if a.Model == "" {
		return nil, fmt.Errorf("agent %s: model is required", name)
	}
	// A positive declaration can still overflow time.Duration during conversion.
	// Validate the exact duration here so every loaded agent has a real bound.
	if _, err := agentDeadline(a.DeadlineSeconds); err != nil {
		return nil, fmt.Errorf("agent %s: %w", name, err)
	}
	if strings.TrimSpace(a.Commit.Name) == "" || strings.TrimSpace(a.Commit.Email) == "" {
		return nil, fmt.Errorf("agent %s: commit.name and commit.email are required", name)
	}
	if secretShaped(a.Commit.Name) || secretShaped(a.Commit.Email) {
		return nil, fmt.Errorf("agent %s: commit identity contains credential-shaped text", name)
	}
	if a.Mode == "" {
		a.Mode = "primary"
	}
	if _, hasBashPermission := a.Permission["bash"]; hasBashPermission {
		return nil, fmt.Errorf("agent %s: permission.bash is unsupported; declare bash_allow", name)
	}
	if a.BashAllow == nil {
		a.BashAllow = []string{}
	}
	if err := validateBashAllow(a.BashAllow); err != nil {
		return nil, fmt.Errorf("agent %s: %w", name, err)
	}
	ins, err := readRepositoryFile(repoDir, filepath.Join(dir, "instructions.md"))
	if err != nil {
		return nil, fmt.Errorf("agent %s instructions.md: %w", name, err)
	}
	if strings.TrimSpace(string(ins)) == "" {
		return nil, fmt.Errorf("agent %s instructions.md is empty", name)
	}
	a.Instructions = string(ins)
	prompt, err := readRepositoryFile(repoDir, filepath.Join(dir, "prompt.md"))
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
	schema, err := readRepositoryFile(repoDir, filepath.Join(dir, "report.schema.json"))
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
	a.DefSHA, err = composeDigest(repoDir, name)
	if err != nil {
		return nil, fmt.Errorf("agent %s digest: %w", name, err)
	}
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

// composeDigest fingerprints the whole agent definition plus forest.yaml. It
// walks through os.Root, so an extra tracked symlink cannot make the controller
// hash a Host file outside the declaration.
func composeDigest(repoDir, name string) (string, error) {
	h := sha256.New()
	dir := filepath.Join(repoDir, DefaultAgentsDir, name)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("declaration file %q must not be a symbolic link", path)
		}
		body, err := root.ReadFile(path)
		if err != nil {
			return err
		}
		h.Write([]byte(filepath.Join(DefaultAgentsDir, name, filepath.FromSlash(path))))
		h.Write(body)
		return nil
	}); err != nil {
		return "", err
	}
	if body, err := readRepositoryFile(repoDir, filepath.Join(repoDir, "forest.yaml")); err == nil {
		h.Write([]byte{0})
		h.Write(body)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
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
	perm := make(map[string]any, len(a.Permission)+1)
	for k, v := range a.Permission {
		perm[k] = v
	}
	if a.BashAllow != nil {
		perm["bash"] = bashPermissionNode(a.BashAllow)
	}
	// Deny every declared-but-disabled MCP, even if a later provider
	// configuration makes matching tool names available.
	for _, m := range a.MCP {
		if !m.Enabled {
			perm["mcp__"+m.Name+"__*"] = "deny"
			perm["mcp__"+m.Name+"_*"] = "deny"
		}
	}
	type frontmatter struct {
		Description string         `yaml:"description"`
		Model       string         `yaml:"model"`
		Variant     string         `yaml:"variant,omitempty"`
		Mode        string         `yaml:"mode"`
		Temperature *float64       `yaml:"temperature,omitempty"`
		Permission  map[string]any `yaml:"permission"`
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
		b, err := readRepositoryFile(a.Dir, sf)
		if err != nil {
			return fmt.Errorf("render skill %s: %w", filepath.Base(sf), err)
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
func newRunConfigDir(home, repoDir string, a *Agent) (string, error) {
	cfgDir := filepath.Join(home, "config")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
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
	if a.BashAllow != nil {
		shell := filepath.Join(home, "bin", "forest-shell")
		if err := configureRunShell(filepath.Join(ocDir, "opencode.json"), shell); err != nil {
			return "", err
		}
	}
	if err := renderMarkdown(ocDir, a); err != nil {
		return "", err
	}
	ok = true
	return cfgDir, nil
}

// readProviderConfig reads and sanitizes the repository-owned provider route.
// It never consults an operator's ambient OpenCode configuration.
func readProviderConfig(repoDir string) ([]byte, error) {
	src := filepath.Join(repoDir, ".opencode", "opencode.json")
	body, err := readRepositoryFile(repoDir, src)
	if err != nil {
		return nil, fmt.Errorf("read provider config %s: %w", src, err)
	}
	clean, err := sanitizeProviderConfig(body)
	if err != nil {
		return nil, fmt.Errorf("validate provider config %s: %w", src, err)
	}
	return clean, nil
}

// preserveProviderConfig copies the validated repository route into the
// private per-run config root.
func preserveProviderConfig(cfgDir, repoDir string) error {
	clean, err := readProviderConfig(repoDir)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "opencode.json"), clean, 0o600); err != nil {
		return fmt.Errorf("stage opencode provider config into run config: %w", err)
	}
	return nil
}

func configureRunShell(path, shell string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read staged opencode config: %w", err)
	}
	var config map[string]any
	if err := json.Unmarshal(body, &config); err != nil {
		return fmt.Errorf("decode staged opencode config: %w", err)
	}
	config["shell"] = shell
	body, err = json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encode staged opencode config: %w", err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("write staged opencode config: %w", err)
	}
	return nil
}
func sanitizeProviderConfig(body []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode provider config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("decode provider config: trailing content")
	}
	if err := validateProviderOptions(value); err != nil {
		return nil, err
	}
	clean, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode provider config: %w", err)
	}
	return append(clean, '\n'), nil
}

// Iron Forest stages only a Mint-routed OpenRouter provider declaration. A
// strict allowlist prevents unrelated OpenCode configuration from crossing the
// repository-to-Runner boundary under an innocent-looking field name.
func validateProviderOptions(value any) error {
	root, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("provider config root must be an object")
	}
	for key, field := range root {
		switch key {
		case "$schema":
			if schema, ok := field.(string); !ok || schema != "https://opencode.ai/config.json" {
				return fmt.Errorf("provider config $schema is not supported")
			}
		case "provider":
		default:
			return fmt.Errorf("provider config field %q is not supported", key)
		}
	}
	providers, exists := root["provider"]
	if !exists {
		return fmt.Errorf("provider config requires a provider object")
	}
	providerMap, ok := providers.(map[string]any)
	if !ok || len(providerMap) == 0 {
		return fmt.Errorf("provider config provider field must be a non-empty object")
	}
	for name, declaration := range providerMap {
		spec, ok := declaration.(map[string]any)
		if !ok {
			return fmt.Errorf("provider %q declaration must be an object", name)
		}
		for key, field := range spec {
			switch key {
			case "npm":
				if npm, ok := field.(string); !ok || npm != "@ai-sdk/openai-compatible" {
					return fmt.Errorf("provider %q npm package is not supported", name)
				}
			case "name":
				if display, ok := field.(string); !ok || strings.TrimSpace(display) == "" {
					return fmt.Errorf("provider %q name must be a non-empty string", name)
				}
			case "options":
			case "models":
				if err := validateProviderModels(name, field); err != nil {
					return err
				}
			default:
				return fmt.Errorf("provider %q field %q is not supported", name, key)
			}
		}
		rawOptions, exists := spec["options"]
		if !exists {
			return fmt.Errorf("provider %q requires Mint options", name)
		}
		options, ok := rawOptions.(map[string]any)
		if !ok {
			return fmt.Errorf("provider %q options must be an object", name)
		}
		var baseURL, credential bool
		for key, option := range options {
			switch normalizedProviderKey(key) {
			case "baseurl":
				const prefix = "http://mint.tail5f5eb4.ts.net:4949/proxy/https/openrouter.ai/"
				text, ok := option.(string)
				if !ok || !strings.HasPrefix(text, prefix) || strings.ContainsAny(text, "@?#\r\n") {
					return fmt.Errorf("provider %q baseURL must use the Mint OpenRouter proxy", name)
				}
				baseURL = true
			case "headers":
				headers, ok := option.(map[string]any)
				if !ok || len(headers) == 0 {
					return fmt.Errorf("provider %q headers must be a non-empty object", name)
				}
				for header, value := range headers {
					text, ok := value.(string)
					if !ok || !mintConfigValue(text) {
						return fmt.Errorf("provider %q header %q must contain only a Mint marker", name, header)
					}
				}
				credential = true
			default:
				if !providerCredentialField(key) {
					return fmt.Errorf("provider %q option %q is not supported", name, key)
				}
				text, ok := option.(string)
				if !ok || !mintConfigValue(text) {
					return fmt.Errorf("credential field %q must contain only a Mint marker", key)
				}
				credential = true
			}
		}
		if !baseURL || !credential {
			return fmt.Errorf("provider %q requires a Mint baseURL and credential marker", name)
		}
	}
	return nil
}

func validateProviderModels(provider string, value any) error {
	models, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("provider %q models must be an object", provider)
	}
	for model, declaration := range models {
		spec, ok := declaration.(map[string]any)
		if !ok {
			return fmt.Errorf("provider %q model %q must be an object", provider, model)
		}
		for key, field := range spec {
			switch key {
			case "name":
				if name, ok := field.(string); !ok || strings.TrimSpace(name) == "" {
					return fmt.Errorf("provider %q model %q name must be a non-empty string", provider, model)
				}
			case "maxTokens":
				if tokens, ok := field.(json.Number); !ok || strings.HasPrefix(tokens.String(), "-") {
					return fmt.Errorf("provider %q model %q maxTokens must be a non-negative number", provider, model)
				}
			default:
				return fmt.Errorf("provider %q model %q field %q is not supported", provider, model, key)
			}
		}
	}
	return nil
}

func normalizedProviderKey(key string) string {
	var normalized strings.Builder
	for _, r := range strings.ToLower(key) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			normalized.WriteRune(r)
		}
	}
	return normalized.String()
}

func providerCredentialField(key string) bool {
	key = normalizedProviderKey(key)
	return key == "key" || key == "authorization" || key == "proxyauthorization" ||
		key == "cookie" || strings.Contains(key, "apikey") ||
		strings.HasSuffix(key, "token") || strings.HasSuffix(key, "secret") ||
		strings.HasSuffix(key, "secretkey") || strings.HasSuffix(key, "password") ||
		strings.HasSuffix(key, "privatekey") || strings.HasSuffix(key, "accesskey") ||
		strings.HasSuffix(key, "signingkey") || strings.HasSuffix(key, "clientkey") ||
		strings.HasSuffix(key, "clientcertificate") || strings.HasSuffix(key, "certificate") ||
		strings.HasSuffix(key, "pem") || strings.HasSuffix(key, "tlskey") ||
		strings.HasSuffix(key, "auth") || strings.HasSuffix(key, "credential") ||
		strings.HasSuffix(key, "credentials") || strings.HasSuffix(key, "signature")
}

const requiredProviderMarker = "__mint.openrouter.ironforest__"

func mintConfigValue(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) > len("Bearer ") && strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
		value = strings.TrimSpace(value[len("Bearer "):])
	}
	return value == requiredProviderMarker
}

// agentSkills lists the agent's skill markdown files in stable order.
func agentSkills(dir string) []string {
	files, _ := filepath.Glob(filepath.Join(dir, "skills", "*.md"))
	sort.Strings(files)
	return files
}

// renderUserPrompt renders the loaded agent's prompt.md template.
func renderUserPrompt(a *Agent, data map[string]any) (string, error) {
	src := a.PromptTmpl
	tmpl, err := template.New(a.Name + "-prompt").Option("missingkey=error").Parse(src)
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
