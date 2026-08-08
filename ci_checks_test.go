package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ciWorkflow mirrors just the fields the drift test reads from
// .github/workflows/ci.yml.
type ciWorkflow struct {
	Jobs map[string]struct {
		Steps []struct {
			Name string `yaml:"name"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// normalizeCheckRun strips the `mise exec -- ` toolchain driver forest.yaml
// prefixes a check with, so a declared command compares with the bare
// `go ...` command CI runs.
func normalizeCheckRun(run string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(run), "mise exec -- "))
}

// TestCIJobRunsExactlyTheForestChecks is the drift guard between the factory's
// own gate and repository CI. Actions cannot derive a job's steps from
// forest.yaml at parse time, so the two lists are restated by necessity; this
// test fails when they disagree. A check added to forest.yaml but not to
// .github/workflows/ci.yml — or one left in CI after being removed from
// forest.yaml — breaks the pipeline.
func TestCIJobRunsExactlyTheForestChecks(t *testing.T) {
	cfg, err := loadConfig(filepath.Join("forest.yaml"))
	if err != nil {
		t.Fatalf("load forest.yaml: %v", err)
	}
	var want []string
	for _, c := range cfg.Checks {
		want = append(want, normalizeCheckRun(c.Run))
	}

	raw, err := os.ReadFile(filepath.Join(".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read .github/workflows/ci.yml: %v", err)
	}
	var wf ciWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse .github/workflows/ci.yml: %v", err)
	}
	job, ok := wf.Jobs["test"]
	if !ok {
		t.Fatal("ci.yml has no test job")
	}
	var have []string
	for _, step := range job.Steps {
		// Only single-command steps are checks. The gofmt scaffolding step is a
		// multi-line shell block and the checkout/setup-go steps carry no run,
		// so both are excluded by construction.
		if step.Run == "" || strings.Contains(step.Run, "\n") {
			continue
		}
		have = append(have, normalizeCheckRun(step.Run))
	}
	sort.Strings(want)
	sort.Strings(have)

	missing := difference(want, have)
	extra := difference(have, want)
	if len(missing) != 0 || len(extra) != 0 {
		t.Fatalf(
			"forest.yaml checks and .github/workflows/ci.yml differ:\n"+
				"  in forest.yaml but not CI: %v\n"+
				"  in CI but not forest.yaml: %v",
			missing, extra,
		)
	}
}

// difference returns the strings present in a but absent from b, both sorted.
func difference(a, b []string) []string {
	var out []string
	for _, s := range a {
		if _, ok := set(b)[s]; !ok {
			out = append(out, s)
		}
	}
	return out
}

func set(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}
