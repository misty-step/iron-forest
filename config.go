package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const workspaceName = ".forest"

var repoNamePattern = regexp.MustCompile(`^[^/\s]+/[^/\s]+$`)

type Config struct {
	Repo   string                 `yaml:"repo" json:"repo"`
	Agents map[string]AgentConfig `yaml:"agents" json:"agents"`
	Checks []Check                `yaml:"checks" json:"checks"`
}

type AgentConfig struct {
	Poll     string `yaml:"poll" json:"poll"`
	Interval int    `yaml:"interval" json:"interval"`
	Timeout  int    `yaml:"timeout" json:"timeout"`
}

type Check struct {
	Name string `yaml:"name" json:"name"`
	Run  string `yaml:"run" json:"run"`
}

type yamlInt int

func (i *yamlInt) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode || value.ShortTag() != "!!int" {
		return fmt.Errorf("must be a YAML integer scalar, got %s", value.ShortTag())
	}
	decoded, err := strconv.ParseInt(value.Value, 0, strconv.IntSize)
	if err != nil {
		return fmt.Errorf("YAML integer scalar %q is outside the Go int range: %w", value.Value, err)
	}
	*i = yamlInt(decoded)
	return nil
}

type configYAML struct {
	Repo   yamlString                      `yaml:"repo"`
	Agents map[*yamlString]agentConfigYAML `yaml:"agents"`
	Checks []checkYAML                     `yaml:"checks"`
}

type agentConfigYAML struct {
	Poll     yamlString `yaml:"poll"`
	Interval yamlInt    `yaml:"interval"`
	Timeout  yamlInt    `yaml:"timeout"`
}

type checkYAML struct {
	Name yamlString `yaml:"name"`
	Run  yamlString `yaml:"run"`
}

func configPath(root string) string { return filepath.Join(root, "forest.yaml") }
func forestPath(root string, parts ...string) string {
	return filepath.Join(append([]string{root, workspaceName}, parts...)...)
}

// Defaults are the operator's instance-level values for this Kernel. They sit
// below every declaration: a declaration field wins over a default, and a
// default wins over the built-in. The file is optional; an absent file is the
// zero Defaults, not an error.
type Defaults struct {
	Model    string `yaml:"model" json:"model,omitempty"`
	Thinking string `yaml:"thinking" json:"thinking,omitempty"`
}

type defaultsYAML struct {
	Model    yamlString `yaml:"model"`
	Thinking yamlString `yaml:"thinking"`
}

// defaultsPath locates the instance defaults: FOREST_DEFAULTS names the file
// when set, and the checkout's forest.defaults.yaml is the fallback. A relative
// override resolves against the checkout, so --root and the engine agree.
func defaultsPath(root string) string {
	if path := strings.TrimSpace(os.Getenv("FOREST_DEFAULTS")); path != "" {
		if !filepath.IsAbs(path) {
			return filepath.Join(root, path)
		}
		return path
	}
	return filepath.Join(root, "forest.defaults.yaml")
}

// loadDefaults reads the instance defaults. The source is returned so the read
// surface can state where a value came from. An explicit FOREST_DEFAULTS that
// is missing is an error: the operator named a file, so silence would hide a
// typo.
func loadDefaults(root string) (Defaults, string, error) {
	path := defaultsPath(root)
	explicit := strings.TrimSpace(os.Getenv("FOREST_DEFAULTS")) != ""
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if explicit {
			return Defaults{}, "", fmt.Errorf("read %s: %w", path, err)
		}
		return Defaults{}, "", nil
	}
	if err != nil {
		return Defaults{}, "", err
	}
	var document defaultsYAML
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		// An empty or comment-only file is the same as an absent one: the
		// operator created the slot and has not filled it yet.
		if errors.Is(err, io.EOF) {
			return Defaults{}, path, nil
		}
		return Defaults{}, "", fmt.Errorf("parse %s: %w", path, err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Defaults{}, "", fmt.Errorf("parse %s: %w", path, err)
		}
		return Defaults{}, "", fmt.Errorf("parse %s: multiple YAML documents", path)
	}
	defaults := Defaults{
		Model:    strings.TrimSpace(string(document.Model)),
		Thinking: strings.TrimSpace(string(document.Thinking)),
	}
	return defaults, path, nil
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return decodeConfig(data, path)
}

func decodeConfig(data []byte, source string) (Config, error) {
	var document configYAML
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", source, err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", source, err)
		}
		return Config{}, fmt.Errorf("parse %s: multiple YAML documents", source)
	}
	cfg := Config{Repo: string(document.Repo)}
	if document.Agents != nil {
		cfg.Agents = make(map[string]AgentConfig, len(document.Agents))
		for name, agent := range document.Agents {
			if name == nil {
				return Config{}, fmt.Errorf("parse %s: agent name must be a YAML string scalar, got !!null", source)
			}
			cfg.Agents[string(*name)] = AgentConfig{
				Poll:     string(agent.Poll),
				Interval: int(agent.Interval),
				Timeout:  int(agent.Timeout),
			}
		}
	}
	if document.Checks != nil {
		cfg.Checks = make([]Check, len(document.Checks))
		for i, check := range document.Checks {
			cfg.Checks[i] = Check{Name: string(check.Name), Run: string(check.Run)}
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate %s: %w", source, err)
	}
	return cfg, nil
}

const maxDurationSeconds = (1<<63 - 1) / int64(time.Second)

func durationFromSeconds(seconds int) (time.Duration, error) {
	if seconds <= 0 {
		return 0, fmt.Errorf("seconds must be greater than zero")
	}
	if int64(seconds) > maxDurationSeconds {
		return 0, fmt.Errorf("seconds overflow time.Duration: %d", seconds)
	}
	return time.Duration(seconds) * time.Second, nil
}

func (c Config) Validate() error {
	if !repoNamePattern.MatchString(c.Repo) {
		return fmt.Errorf("repo must have owner/name shape: %q", c.Repo)
	}
	if len(c.Agents) == 0 {
		return errors.New("agents must not be empty")
	}
	for name, agent := range c.Agents {
		if strings.TrimSpace(name) == "" {
			return errors.New("agent name must not be empty")
		}
		if strings.TrimSpace(agent.Poll) == "" {
			return fmt.Errorf("agent %q poll is required", name)
		}
		if agent.Interval <= 0 {
			return fmt.Errorf("agent %q interval must be greater than zero", name)
		}
		if _, err := durationFromSeconds(agent.Interval); err != nil {
			return fmt.Errorf("agent %q interval: %w", name, err)
		}
		if agent.Timeout <= 0 {
			return fmt.Errorf("agent %q timeout must be greater than zero", name)
		}
		if _, err := durationFromSeconds(agent.Timeout); err != nil {
			return fmt.Errorf("agent %q timeout: %w", name, err)
		}
	}
	if len(c.Checks) == 0 {
		return errors.New("checks must not be empty")
	}
	seen := make(map[string]struct{}, len(c.Checks))
	for i, check := range c.Checks {
		name := strings.TrimSpace(check.Name)
		if name == "" {
			return fmt.Errorf("check %d name is required", i+1)
		}
		if strings.TrimSpace(check.Run) == "" {
			return fmt.Errorf("check %q run is required", name)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("check %q is duplicated", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func agentNames(cfg Config) []string {
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
