package main

import (
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
  builder: {poll: "forest poll builder", interval: 5, timeout: 20}
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
	bad := Config{Repo: "owner", Agents: map[string]AgentConfig{"builder": {Poll: "x", Interval: 1, Timeout: 1}}}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected invalid repo error")
	}
}

func TestConfigYAMLIsStrictAndSingleDocument(t *testing.T) {
	const valid = `repo: owner/name
agents:
  builder: {poll: "forest poll builder", interval: 5, timeout: 20}
checks:
  - {name: test, run: "go test ./..."}
`
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "unknown top-level key", data: valid + "repository: owner/name\n", want: "field repository not found"},
		{name: "unknown nested key", data: strings.Replace(valid, "timeout: 20}", "timeout: 20, timeuot: 20}", 1), want: "field timeuot not found"},
		{name: "extra document", data: valid + "---\nrepo: owner/other\nagents:\n  builder: {poll: x, interval: 1, timeout: 1}\n", want: "multiple YAML documents"},
		{name: "boolean repo", data: strings.Replace(valid, "repo: owner/name", "repo: true", 1), want: "must be a YAML string scalar"},
		{name: "numeric agent name", data: strings.Replace(valid, "builder:", "1:", 1), want: "must be a YAML string scalar"},
		{name: "boolean poll", data: strings.Replace(valid, `"forest poll builder"`, "true", 1), want: "must be a YAML string scalar"},
		{name: "numeric check name", data: strings.Replace(valid, "name: test", "name: 1", 1), want: "must be a YAML string scalar"},
		{name: "fractional interval", data: strings.Replace(valid, "interval: 5", "interval: 1.5", 1), want: "must be a YAML integer scalar"},
		{name: "fractional timeout", data: strings.Replace(valid, "timeout: 20", "timeout: 20.9", 1), want: "must be a YAML integer scalar"},
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
  builder: {poll: "forest poll builder", interval: 5, timeout: 20}
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

func TestConfigRejectsDurationOverflow(t *testing.T) {
	overflow := int(^uint(0) >> 1)
	if int64(overflow) <= maxDurationSeconds {
		t.Skip("int range cannot overflow time.Duration seconds")
	}
	cfg := Config{
		Repo:   "owner/name",
		Agents: map[string]AgentConfig{"builder": {Poll: "poll", Interval: overflow, Timeout: 1}},
		Checks: []Check{{Name: "test", Run: "true"}},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("interval overflow error=%v", err)
	}
	cfg.Agents["builder"] = AgentConfig{Poll: "poll", Interval: 1, Timeout: overflow}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("timeout overflow error=%v", err)
	}
}
