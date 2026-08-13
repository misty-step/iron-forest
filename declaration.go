package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

type yamlString string

func stringScalar(value *yaml.Node) (string, error) {
	if value.Kind != yaml.ScalarNode || value.ShortTag() != "!!str" {
		return "", fmt.Errorf("must be a YAML string scalar, got %s", value.ShortTag())
	}
	return value.Value, nil
}

func (s *yamlString) UnmarshalYAML(value *yaml.Node) error {
	decoded, err := stringScalar(value)
	if err != nil {
		return err
	}
	*s = yamlString(decoded)
	return nil
}

type StringList []string

func (s *StringList) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		decoded, err := stringScalar(value)
		if err != nil {
			return err
		}
		if strings.TrimSpace(decoded) == "" {
			*s = nil
			return nil
		}
		*s = strings.FieldsFunc(decoded, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
		return nil
	}
	if value.Kind != yaml.SequenceNode || value.ShortTag() != "!!seq" {
		return fmt.Errorf("must be a YAML string scalar or sequence of string scalars, got %s", value.ShortTag())
	}
	values := make([]string, len(value.Content))
	for i, item := range value.Content {
		decoded, err := stringScalar(item)
		if err != nil {
			return fmt.Errorf("item %d: %w", i+1, err)
		}
		values[i] = decoded
	}
	*s = values
	return nil
}

type Declaration struct {
	Name         string   `json:"name"`
	Model        string   `json:"model"`
	Tools        []string `json:"tools"`
	Thinking     string   `json:"thinking"`
	SystemPrompt string   `json:"system_prompt"`
	TaskPrompt   string   `json:"task_prompt"`
	// ModelSource names the layer that supplied the model: the declaration, the
	// instance defaults, or the built-in. A consumer can audit the chain instead
	// of guessing which file won.
	ModelSource string `json:"model_source,omitempty"`
	// Env carries opaque string values declared for the Run. JSON never prints
	// them.
	Env map[string]string `json:"-"`
	// EnvKeys lists declared environment names for the read surface.
	EnvKeys []string `json:"env,omitempty"`
	// ProfileFiles lists the files in this declaration's profile layer, so the
	// read surface can show what the layer contributes without opening it.
	ProfileFiles []string `json:"profile_files,omitempty"`
}

type declarationFrontmatter struct {
	Model    yamlString `yaml:"model"`
	Tools    yaml.Node  `yaml:"tools"`
	Thinking yaml.Node  `yaml:"thinking"`
	Env      yaml.Node  `yaml:"env"`
}

func declarationDir(root, name string) string { return filepath.Join(root, "agents", name) }

func loadDeclaration(root, name string) (Declaration, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return Declaration{}, fmt.Errorf("invalid agent name %q", name)
	}
	dir := declarationDir(root, name)
	agentData, err := os.ReadFile(filepath.Join(dir, "agent.md"))
	if err != nil {
		return Declaration{}, fmt.Errorf("read %s agent.md: %w", name, err)
	}
	taskData, err := os.ReadFile(filepath.Join(dir, "task.md"))
	if err != nil {
		return Declaration{}, fmt.Errorf("read %s task.md: %w", name, err)
	}
	front, body, err := splitFrontmatter(agentData)
	if err != nil {
		return Declaration{}, fmt.Errorf("agent %s: %w", name, err)
	}
	var metadata declarationFrontmatter
	decoder := yaml.NewDecoder(bytes.NewReader(front))
	decoder.KnownFields(true)
	if err := decoder.Decode(&metadata); err != nil {
		return Declaration{}, fmt.Errorf("agent %s frontmatter: %w", name, err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Declaration{}, fmt.Errorf("agent %s frontmatter: %w", name, err)
		}
		return Declaration{}, fmt.Errorf("agent %s frontmatter: multiple YAML documents", name)
	}
	// Tools is always a slice: the published declaration payload never carries
	// null in place of an empty list.
	tools := StringList{}
	if metadata.Tools.Kind != 0 {
		if err := tools.UnmarshalYAML(&metadata.Tools); err != nil {
			return Declaration{}, fmt.Errorf("agent %s frontmatter tools: %w", name, err)
		}
	}
	if tools == nil {
		tools = StringList{}
	}
	thinking := ""
	if metadata.Thinking.Kind != 0 {
		thinking, err = stringScalar(&metadata.Thinking)
		if err != nil {
			return Declaration{}, fmt.Errorf("agent %s frontmatter thinking: %w", name, err)
		}
	}
	env, err := decodeDeclarationEnv(name, &metadata.Env)
	if err != nil {
		return Declaration{}, err
	}
	if len(bytes.TrimSpace(taskData)) == 0 {
		return Declaration{}, fmt.Errorf("agent %s task.md is empty", name)
	}
	if strings.TrimSpace(body) == "" {
		return Declaration{}, fmt.Errorf("agent %s system prompt is empty", name)
	}
	// The model resolves through three layers, declaration first, so an operator
	// can set one fleet default and a repository can still override it.
	defaults, _, err := loadDefaults(root)
	if err != nil {
		return Declaration{}, fmt.Errorf("agent %s: %w", name, err)
	}
	model := strings.TrimSpace(string(metadata.Model))
	modelSource := "declaration"
	if model == "" {
		model, modelSource = strings.TrimSpace(defaults.Model), "defaults"
	}
	if model == "" {
		model, modelSource = defaultModel, "built-in"
	}
	if thinking == "" {
		thinking = strings.TrimSpace(defaults.Thinking)
	}
	profileFiles, err := declarationProfileFiles(root, name)
	if err != nil {
		return Declaration{}, err
	}
	return Declaration{
		Name:         name,
		Model:        model,
		Tools:        tools,
		Thinking:     strings.TrimSpace(thinking),
		SystemPrompt: body,
		TaskPrompt:   string(taskData),
		ModelSource:  modelSource,
		Env:          env,
		EnvKeys:      envKeys(env),
		ProfileFiles: profileFiles,
	}, nil
}

// defaultModel is the built-in last layer of the model chain. Local models are
// available by declaring one; they are never the default.
const defaultModel = "openrouter/deepseek/deepseek-v4-flash-0731"

// blockedEnvNames are the variables the Kernel owns. A declaration may not
// restate them, because the Run's identity, liveness, and profile depend on them.
var blockedEnvNames = []string{
	"PATH", "HOME", "FOREST_RUN_ID", "PI_CODING_AGENT_DIR",
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// decodeDeclarationEnv reads the optional env map. Values pass through
// unchanged. The read surfaces publish names only.
func decodeDeclarationEnv(name string, node *yaml.Node) (map[string]string, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode || node.ShortTag() != "!!map" {
		return nil, fmt.Errorf("agent %s frontmatter env: must be a YAML mapping of string scalars, got %s", name, node.ShortTag())
	}
	env := make(map[string]string, len(node.Content)/2)
	for index := 0; index+1 < len(node.Content); index += 2 {
		keyNode, valueNode := node.Content[index], node.Content[index+1]
		key, err := stringScalar(keyNode)
		if err != nil {
			return nil, fmt.Errorf("agent %s frontmatter env name %d: %w", name, index/2+1, err)
		}
		value, err := stringScalar(valueNode)
		if err != nil {
			return nil, fmt.Errorf("agent %s frontmatter env %q: %w", name, key, err)
		}
		if !envNamePattern.MatchString(key) {
			return nil, fmt.Errorf("agent %s frontmatter env %q is not a valid environment name", name, key)
		}
		if slices.Contains(blockedEnvNames, key) {
			return nil, fmt.Errorf("agent %s frontmatter env %q names a variable the Kernel owns", name, key)
		}
		env[key] = value
	}
	return env, nil
}

func envKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func splitFrontmatter(data []byte) ([]byte, string, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	text := string(data)
	if !strings.HasPrefix(text, "---\n") {
		return nil, "", fmt.Errorf("missing YAML frontmatter")
	}
	parts := strings.SplitAfter(text[4:], "\n")
	var front strings.Builder
	for index, part := range parts {
		line := strings.TrimSuffix(strings.TrimSuffix(part, "\n"), "\r")
		if line == "---" {
			return []byte(front.String()), strings.Join(parts[index+1:], ""), nil
		}
		front.WriteString(part)
	}
	return nil, "", fmt.Errorf("unterminated YAML frontmatter")
}
