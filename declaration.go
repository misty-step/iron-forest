package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
	// SkillPaths lists the explicit repository-relative skill directories that
	// Pi receives for this declaration.
	SkillPaths []string `json:"skills"`
	// DefinitionSHA is the digest over the ordered declaration pair (agent.md
	// then task.md) as loaded. The Runner recomputes it immediately before exec
	// so a run executes only the declaration bytes the Kernel loaded (see #144).
	DefinitionSHA string `json:"definition_sha,omitempty"`
}

type declarationFrontmatter struct {
	Model    yamlString `yaml:"model"`
	Tools    yaml.Node  `yaml:"tools"`
	Thinking yaml.Node  `yaml:"thinking"`
}

func declarationDir(root, name string) string { return filepath.Join(root, "agents", name) }

func declarationSkillPaths(root, name string) ([]string, error) {
	relativePaths := []string{
		filepath.Join("agents", "_shared", "skills"),
		filepath.Join("agents", name, "skills"),
	}
	paths := make([]string, 0, len(relativePaths))
	for _, relative := range relativePaths {
		info, err := os.Lstat(filepath.Join(root, relative))
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			return nil, fmt.Errorf("inspect skill directory %s: %w", filepath.ToSlash(relative), err)
		case !info.IsDir():
			return nil, fmt.Errorf("skill path %s must be a real directory", filepath.ToSlash(relative))
		}
		if err := validateSkillDirectory(root, relative); err != nil {
			return nil, err
		}
		paths = append(paths, filepath.ToSlash(relative))
	}
	return paths, nil
}

func validateDeclarationSkillPaths(root, name string, paths []string) error {
	allowed := map[string]struct{}{
		filepath.ToSlash(filepath.Join("agents", "_shared", "skills")): {},
		filepath.ToSlash(filepath.Join("agents", name, "skills")):      {},
	}
	for _, relative := range paths {
		normalized := filepath.ToSlash(filepath.Clean(relative))
		if _, ok := allowed[normalized]; !ok {
			return fmt.Errorf("invalid skill path %q for agent %s", relative, name)
		}
		local := filepath.FromSlash(normalized)
		info, err := os.Lstat(filepath.Join(root, local))
		if err != nil {
			return fmt.Errorf("inspect skill directory %s: %w", normalized, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("skill path %s must be a real directory", normalized)
		}
		if err := validateSkillDirectory(root, local); err != nil {
			return err
		}
	}
	return nil
}

func validateSkillDirectory(root, relative string) error {
	return filepath.WalkDir(filepath.Join(root, relative), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("inspect skill path %s: %w", filepath.ToSlash(relative), err)
		}
		if entry.Type()&os.ModeSymlink == 0 {
			return nil
		}
		child, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return fmt.Errorf("skill path %s contains symlink %s", filepath.ToSlash(relative), filepath.ToSlash(child))
	})
}

func loadDeclaration(root, name string) (Declaration, error) {
	defaults, _, err := loadDefaults(root)
	if err != nil {
		return Declaration{}, fmt.Errorf("agent %s: %w", name, err)
	}
	return loadDeclarationWithDefaults(root, name, defaults)
}

func loadDeclarationWithDefaults(root, name string, defaults Defaults) (Declaration, error) {
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
	if err := decoder.Decode(&metadata); err != nil && !errors.Is(err, io.EOF) {
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
	if len(bytes.TrimSpace(taskData)) == 0 {
		return Declaration{}, fmt.Errorf("agent %s task.md is empty", name)
	}
	if strings.TrimSpace(body) == "" {
		return Declaration{}, fmt.Errorf("agent %s system prompt is empty", name)
	}
	// The model resolves through three layers, declaration first, so an operator
	// can set one fleet default and a repository can still override it.
	definitionSHA := declarationPairDigest(agentData, taskData)
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
	skillPaths, err := declarationSkillPaths(root, name)
	if err != nil {
		return Declaration{}, err
	}
	return Declaration{
		Name:          name,
		Model:         model,
		Tools:         tools,
		Thinking:      strings.TrimSpace(thinking),
		SystemPrompt:  body,
		TaskPrompt:    string(taskData),
		ModelSource:   modelSource,
		SkillPaths:    skillPaths,
		DefinitionSHA: definitionSHA,
	}, nil
}

// declarationPairDigest fingerprints the ordered declaration pair: the bytes of
// agent.md followed by the bytes of task.md. It is the per-dispatch digest the
// Kernel verifies immediately before exec so a Run executes only the declared
// files, unchanged since they were loaded (see #144).
func declarationPairDigest(agentData, taskData []byte) string {
	hash := sha256.New()
	hash.Write(agentData)
	hash.Write([]byte{0})
	hash.Write(taskData)
	return hex.EncodeToString(hash.Sum(nil))
}

// defaultModel is the built-in last layer of the model chain.
const defaultModel = "openrouter/deepseek/deepseek-v4-pro-0813"

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
