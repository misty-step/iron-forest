package main

import (
	"bytes"
	"fmt"
	"io"
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
}

type declarationFrontmatter struct {
	Model    yamlString `yaml:"model"`
	Tools    yaml.Node  `yaml:"tools"`
	Thinking yaml.Node  `yaml:"thinking"`
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
	if strings.TrimSpace(string(metadata.Model)) == "" {
		return Declaration{}, fmt.Errorf("agent %s model is required", name)
	}
	if len(bytes.TrimSpace(taskData)) == 0 {
		return Declaration{}, fmt.Errorf("agent %s task.md is empty", name)
	}
	if strings.TrimSpace(body) == "" {
		return Declaration{}, fmt.Errorf("agent %s system prompt is empty", name)
	}
	return Declaration{
		Name:         name,
		Model:        strings.TrimSpace(string(metadata.Model)),
		Tools:        tools,
		Thinking:     strings.TrimSpace(thinking),
		SystemPrompt: body,
		TaskPrompt:   string(taskData),
	}, nil
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
