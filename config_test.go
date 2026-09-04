package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestConfigAndDeclarationValidation(t *testing.T) {
	root := t.TempDir()
	config := `repo: owner/name
agents:
  builder: {poll: "forest poll builder", interval: 5}
checks:
  - {name: test, run: "go test ./..."}
`
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "agents", "builder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "builder", "agent.md"), []byte("---\nmodel: local/model\ntools: [git, read]\nthinking: low\n---\nSystem rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "builder", "task.md"), []byte("Select one item."), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(filepath.Join(root, "forest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := loadDeclaration(root, "builder")
	if err != nil {
		t.Fatal(err)
	}
	if declaration.Model != "local/model" || !reflect.DeepEqual(declaration.Tools, []string{"git", "read"}) || declaration.SystemPrompt != "System rules\n" {
		t.Fatalf("declaration parsed incorrectly: %#v", declaration)
	}
	cfg.Checks = append(cfg.Checks, Check{Name: "test", Run: "again"})
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("expected duplicate check error, got %v", err)
	}
	bad := Config{Repo: "owner", Agents: map[string]AgentConfig{"builder": {Poll: "x", Interval: 1}}}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected invalid repo error")
	}
}

func TestConfigYAMLIsStrictAndSingleDocument(t *testing.T) {
	const valid = `repo: owner/name
agents:
  builder: {poll: "forest poll builder", interval: 5}
checks:
  - {name: test, run: "go test ./..."}
`
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "unknown top-level key", data: valid + "repository: owner/name\n", want: "field repository not found"},
		{name: "unknown nested key", data: strings.Replace(valid, "interval: 5", "interval: 5, timeuot: 20", 1), want: "field timeuot not found"},
		{name: "removed timeout key", data: strings.Replace(valid, "interval: 5", "interval: 5, timeout: 20", 1), want: "field timeout not found"},
		{name: "extra document", data: valid + "---\nrepo: owner/other\nagents:\n  builder: {poll: x, interval: 1}\n", want: "multiple YAML documents"},
		{name: "boolean repo", data: strings.Replace(valid, "repo: owner/name", "repo: true", 1), want: "must be a YAML string scalar"},
		{name: "numeric agent name", data: strings.Replace(valid, "builder:", "1:", 1), want: "must be a YAML string scalar"},
		{name: "boolean poll", data: strings.Replace(valid, `"forest poll builder"`, "true", 1), want: "must be a YAML string scalar"},
		{name: "numeric check name", data: strings.Replace(valid, "name: test", "name: 1", 1), want: "must be a YAML string scalar"},
		{name: "fractional interval", data: strings.Replace(valid, "interval: 5", "interval: 1.5", 1), want: "must be a YAML integer scalar"},
		{name: "string interval", data: strings.Replace(valid, "interval: 5", `interval: "5"`, 1), want: "must be a YAML integer scalar"},
		{name: "mapping check command", data: strings.Replace(valid, `run: "go test ./..."`, "run: {command: test}", 1), want: "must be a YAML string scalar"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeConfig([]byte(test.data), "forest.yaml"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decodeConfig() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadConfigRequiresNonemptyChecks(t *testing.T) {
	const config = `repo: owner/name
agents:
  builder: {poll: "forest poll builder", interval: 5}
`
	tests := []struct {
		name   string
		checks string
	}{
		{name: "omitted"},
		{name: "explicit empty", checks: "checks: []\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "forest.yaml")
			if err := os.WriteFile(path, []byte(config+test.checks), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := loadConfig(path)
			want := "validate " + path + ": checks must not be empty"
			if err == nil || err.Error() != want {
				t.Fatalf("loadConfig() error = %v, want %q", err, want)
			}
		})
	}
}

func TestYAMLIntScalarBoundaries(t *testing.T) {
	max := ^uint(0) >> 1
	tests := []struct {
		name    string
		scalar  string
		want    int
		wantErr bool
	}{
		{name: "max int", scalar: strconv.FormatUint(uint64(max), 10), want: int(max)},
		{name: "max int plus one", scalar: strconv.FormatUint(uint64(max+1), 10), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got yamlInt
			err := yaml.Unmarshal([]byte(test.scalar), &got)
			if (err != nil) != test.wantErr {
				t.Fatalf("yaml.Unmarshal(%q) error = %v, wantErr %t", test.scalar, err, test.wantErr)
			}
			if int(got) != test.want {
				t.Fatalf("yaml.Unmarshal(%q) = %d, want %d", test.scalar, got, test.want)
			}
		})
	}
}

func TestDeclarationYAMLValidation(t *testing.T) {
	const validAgent = "---\nmodel: local/model\n---\nSystem rules\n"
	tests := []struct {
		name  string
		agent string
		task  string
		want  string
	}{
		{name: "unknown key", agent: "---\nmodel: local/model\nmodle: other/model\n---\nSystem rules\n", task: "Select one item.", want: "field modle not found"},
		{name: "declaration environment", agent: "---\nmodel: local/model\nenv:\n  OPENROUTER_API_KEY: committed-secret\n---\nSystem rules\n", task: "Select one item.", want: "field env not found"},
		{name: "empty task", agent: validAgent, task: " \n\t", want: "task.md is empty"},
		{name: "missing system prompt", agent: "---\nmodel: local/model\n---", task: "Select one item.", want: "system prompt is empty"},
		{name: "empty system prompt", agent: "---\nmodel: local/model\n---\n", task: "Select one item.", want: "system prompt is empty"},
		{name: "whitespace system prompt", agent: "---\nmodel: local/model\n---\n \n\t\r\n", task: "Select one item.", want: "system prompt is empty"},
		{name: "extra document", agent: "---\nmodel: local/model\n--- # appended document\nmodel: other/model\n---\nSystem rules\n", task: "Select one item.", want: "multiple YAML documents"},
		{name: "boolean model", agent: "---\nmodel: true\n---\nSystem rules\n", task: "Select one item.", want: "must be a YAML string scalar"},
		{name: "numeric thinking", agent: "---\nmodel: local/model\nthinking: 1\n---\nSystem rules\n", task: "Select one item.", want: "must be a YAML string scalar"},
		{name: "boolean tools scalar", agent: "---\nmodel: local/model\ntools: true\n---\nSystem rules\n", task: "Select one item.", want: "must be a YAML string scalar"},
		{name: "numeric tools scalar", agent: "---\nmodel: local/model\ntools: 1\n---\nSystem rules\n", task: "Select one item.", want: "must be a YAML string scalar"},
		{name: "null tools scalar", agent: "---\nmodel: local/model\ntools: null\n---\nSystem rules\n", task: "Select one item.", want: "must be a YAML string scalar"},
		{name: "mapping tools", agent: "---\nmodel: local/model\ntools: {git: true}\n---\nSystem rules\n", task: "Select one item.", want: "must be a YAML string scalar or sequence"},
		{name: "mixed tools sequence", agent: "---\nmodel: local/model\ntools: [git, true]\n---\nSystem rules\n", task: "Select one item.", want: "must be a YAML string scalar"},
		{name: "null thinking", agent: "---\nmodel: local/model\nthinking: null\n---\nSystem rules\n", task: "Select one item.", want: "must be a YAML string scalar"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "agents", "builder")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "agent.md"), []byte(test.agent), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte(test.task), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := loadDeclaration(root, "builder"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loadDeclaration() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDurationFromSecondsBoundaries(t *testing.T) {
	type testCase struct {
		name    string
		seconds int
		want    time.Duration
		wantErr bool
	}
	tests := []testCase{
		{name: "minus one", seconds: -1, wantErr: true},
		{name: "zero", seconds: 0, wantErr: true},
		{name: "one", seconds: 1, want: time.Second},
	}
	if int64(^uint(0)>>1) >= maxDurationSeconds+1 {
		max := int(maxDurationSeconds)
		tests = append(tests,
			testCase{name: "max", seconds: max, want: time.Duration(maxDurationSeconds) * time.Second},
			testCase{name: "max plus one", seconds: max + 1, wantErr: true},
		)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := durationFromSeconds(test.seconds)
			if (err != nil) != test.wantErr {
				t.Fatalf("durationFromSeconds(%d) error = %v, wantErr %t", test.seconds, err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("durationFromSeconds(%d) = %v, want %v", test.seconds, got, test.want)
			}
		})
	}
}

func TestConfigMaxDuration(t *testing.T) {
	const config = `repo: owner/name
agents:
  builder: {poll: "forest poll builder", interval: 5, max_duration: 90}
checks:
  - {name: test, run: "go test ./..."}
`
	cfg, err := decodeConfig([]byte(config), "forest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Agents["builder"].MaxDuration; got != 90 {
		t.Fatalf("max_duration=%d, want 90", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid max_duration rejected: %v", err)
	}

	off, err := decodeConfig([]byte(`repo: owner/name
agents:
  builder: {poll: x, interval: 1}
checks:
  - {name: test, run: "true"}
`), "forest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if off.Agents["builder"].MaxDuration != 0 {
		t.Fatalf("omitted max_duration=%d, want 0", off.Agents["builder"].MaxDuration)
	}

	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "negative", data: `repo: owner/name
agents:
  builder: {poll: x, interval: 1, max_duration: -1}
checks:
  - {name: test, run: "true"}
`, want: "max_duration must not be negative"},
		{name: "fractional", data: `repo: owner/name
agents:
  builder: {poll: x, interval: 1, max_duration: 1.5}
checks:
  - {name: test, run: "true"}
`, want: "must be a YAML integer scalar"},
		{name: "string", data: `repo: owner/name
agents:
  builder: {poll: x, interval: 1, max_duration: "90"}
checks:
  - {name: test, run: "true"}
`, want: "must be a YAML integer scalar"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeConfig([]byte(test.data), "forest.yaml")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decodeConfig() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestConfigRejectsDurationOverflow(t *testing.T) {
	overflow := int(^uint(0) >> 1)
	if int64(overflow) <= maxDurationSeconds {
		t.Skip("int range cannot overflow time.Duration seconds")
	}
	tests := []struct {
		name  string
		agent AgentConfig
	}{
		{name: "interval", agent: AgentConfig{Poll: "poll", Interval: overflow}},
		{name: "max_duration", agent: AgentConfig{Poll: "poll", Interval: 1, MaxDuration: overflow}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{
				Repo:   "owner/name",
				Agents: map[string]AgentConfig{"builder": test.agent},
				Checks: []Check{{Name: "test", Run: "true"}},
			}
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "overflow") {
				t.Fatalf("%s overflow error=%v", test.name, err)
			}
		})
	}
}
func TestConfigRejectsCollidingAgentSlugs(t *testing.T) {
	cfg := Config{
		Repo: "owner/name",
		Agents: map[string]AgentConfig{
			"builder.prod": {Poll: "forest poll builder", Interval: 1},
			"builder-prod": {Poll: "forest poll builder", Interval: 1},
		},
		Checks: []Check{{Name: "test", Run: "true"}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "builder-prod") || !strings.Contains(err.Error(), "builder.prod") {
		t.Fatalf("Validate() error = %v, want colliding agent names", err)
	}
}

func TestConfigAcceptsUniqueAgentSlugs(t *testing.T) {
	cfg := Config{
		Repo: "owner/name",
		Agents: map[string]AgentConfig{
			"builder":  {Poll: "forest poll builder", Interval: 1},
			"verifier": {Poll: "forest poll verifier", Interval: 1},
		},
		Checks: []Check{{Name: "test", Run: "true"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() unexpected error: %v", err)
	}
}

func writeAgentFiles(t *testing.T, root, name, frontmatter, body, task string) {
	t.Helper()
	dir := filepath.Join(root, "agents", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.md"), []byte("---\n"+frontmatter+"---\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte(task), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestModelChainResolvesDeclarationThenDefaultsThenBuiltIn(t *testing.T) {
	root := t.TempDir()
	writeAgentFiles(t, root, "builder", "thinking: low\n", "System rules\n", "Select one item.")
	declaration, err := loadDeclarationWithDefaults(root, "builder", Defaults{Model: "default/model", Thinking: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if declaration.Model != "default/model" || declaration.ModelSource != "defaults" || declaration.Thinking != "low" {
		t.Fatalf("declaration=%#v", declaration)
	}

	writeAgentFiles(t, root, "builder", "model: declared/model\n", "System rules\n", "Select one item.")
	declaration, err = loadDeclarationWithDefaults(root, "builder", Defaults{Model: "default/model", Thinking: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if declaration.Model != "declared/model" || declaration.ModelSource != "declaration" || declaration.Thinking != "high" {
		t.Fatalf("declaration=%#v", declaration)
	}

	writeAgentFiles(t, root, "builder", "", "System rules\n", "Select one item.")
	declaration, err = loadDeclarationWithDefaults(root, "builder", Defaults{})
	if err != nil {
		t.Fatal(err)
	}
	if declaration.Model != defaultModel || declaration.ModelSource != "built-in" {
		t.Fatalf("declaration=%#v", declaration)
	}
}

func TestDefaultsFileIsOptionalAndFOREST_DEFAULTSWins(t *testing.T) {
	root := t.TempDir()
	defaults, source, err := loadDefaults(root)
	if err != nil || defaults != (Defaults{}) || source != "" {
		t.Fatalf("optional defaults=%#v source=%q err=%v", defaults, source, err)
	}
	override := filepath.Join(t.TempDir(), "host.yaml")
	if err := os.WriteFile(override, []byte("model: host/model\nthinking: high\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOREST_DEFAULTS", override)
	defaults, source, err = loadDefaults(root)
	if err != nil || defaults.Model != "host/model" || defaults.Thinking != "high" || source != override {
		t.Fatalf("override defaults=%#v source=%q err=%v", defaults, source, err)
	}
	t.Setenv("FOREST_DEFAULTS", "missing.yaml")
	if _, _, err := loadDefaults(root); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing explicit defaults err=%v, want not exist", err)
	}
}

func TestDeclarationDiscoversConventionalSkillDirectoriesAndRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	writeAgentFiles(t, root, "builder", "model: local\n", "System rules\n", "Select one item.")
	declaration, err := loadDeclaration(root, "builder")
	if err != nil {
		t.Fatal(err)
	}
	if len(declaration.SkillPaths) != 0 {
		t.Fatalf("skills without directories=%v", declaration.SkillPaths)
	}

	shared := filepath.Join(root, "agents", "_shared", "skills")
	role := filepath.Join(root, "agents", "builder", "skills")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, role); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "agents", "other", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDeclaration(root, "builder"); err == nil || !strings.Contains(err.Error(), "must be a real directory") {
		t.Fatalf("symlinked skill path err=%v, want real-directory rejection", err)
	}

	if err := os.Remove(role); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(role, 0o755); err != nil {
		t.Fatal(err)
	}
	nestedLink := filepath.Join(shared, "outside")
	if err := os.Symlink(outside, nestedLink); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDeclaration(root, "builder"); err == nil || !strings.Contains(err.Error(), "contains symlink agents/_shared/skills/outside") {
		t.Fatalf("nested skill symlink err=%v, want rejection", err)
	}
	if err := os.Remove(nestedLink); err != nil {
		t.Fatal(err)
	}
	declaration, err = loadDeclaration(root, "builder")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"agents/_shared/skills", "agents/builder/skills"}
	if !reflect.DeepEqual(declaration.SkillPaths, want) {
		t.Fatalf("skills=%v, want %v", declaration.SkillPaths, want)
	}
}

func TestRunEvidencePublishesSkillsWithoutProfiles(t *testing.T) {
	line := runEvidenceLine(
		RunRecord{RunID: "1-builder"},
		Declaration{
			Name:        "builder",
			Model:       "local",
			ModelSource: "declaration",
			SkillPaths:  []string{"agents/_shared/skills", "agents/builder/skills"},
		},
	)
	var evidence map[string]any
	if err := json.Unmarshal([]byte(line), &evidence); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(line, "profile") {
		t.Fatalf("evidence leaked obsolete profile data: %s", line)
	}
	if got := evidence["skills"]; !reflect.DeepEqual(got, []any{"agents/_shared/skills", "agents/builder/skills"}) {
		t.Fatalf("skills=%v", got)
	}
	if _, exists := evidence["env"]; exists {
		t.Fatalf("evidence retained declaration env: %v", evidence)
	}
}
