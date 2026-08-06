package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultAgentsDir is where agent declarations live, one directory each.
	DefaultAgentsDir = "agents"
	// WorkspaceDir holds the ledger, traces, and per-run worktrees.
	WorkspaceDir = ".forest"

	claimLabel       = "forest:wip"
	failedLabel      = "forest:failed"
	commitAuthorName = "forest"
	commitAuthorMail = "forest@mistystep.io"
	modelDefault     = "openrouter-mint/deepseek-v4-flash-0731"
)

// defaultMaxReactionFixes is the single source of truth for how many re-entry
// build passes the reaction loop makes on one PR before parking it. It is
// applied both when composing the default config and when a config omits the
// value or sets it to zero.
const defaultMaxReactionFixes = 2

// Config is the composition file forest.yaml. It declares the backlog source,
// the poll cadence, factory-wide protected paths, and the workflow: which
// named agents implement the build and review phases and how many corrective
// build passes are allowed before the review verdict is final.
type Config struct {
	Repo            string   `yaml:"repo"`
	PollIntervalSec int      `yaml:"poll_interval_seconds"`
	Protected       []string `yaml:"protected"`
	Workflow        Workflow `yaml:"workflow"`
}

// Workflow names the agents that drive the phases of one item. An empty
// Review disables the review phase entirely (build -> publish). AutoMerge
// gates the reaction loop's deterministic merge; it defaults off until the
// operator has proven one manual merge.
type Workflow struct {
	Build            string `yaml:"build"`
	Review           string `yaml:"review"`
	MaxFixIterations int    `yaml:"max_fix_iterations"`
	AutoMerge        bool   `yaml:"auto_merge"`
	MaxReactionFixes int    `yaml:"max_reaction_fixes"`
}

func defaultConfig() Config {
	return Config{
		PollIntervalSec: 30,
		Protected: []string{
			".forest/", "forest.yaml", "agents/", ".opencode/opencode.json",
		},
		Workflow: Workflow{
			Build: "beaver", Review: "owl", MaxFixIterations: 1,
			AutoMerge: false, MaxReactionFixes: defaultMaxReactionFixes,
		},
	}
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Repo == "" {
		return cfg, fmt.Errorf("%s: repo is required", path)
	}
	if cfg.PollIntervalSec <= 0 {
		cfg.PollIntervalSec = 30
	}
	if cfg.Workflow.Build == "" {
		cfg.Workflow.Build = "beaver"
	}
	if cfg.Workflow.MaxFixIterations < 0 {
		cfg.Workflow.MaxFixIterations = 0
	}
	if cfg.Workflow.MaxReactionFixes <= 0 {
		cfg.Workflow.MaxReactionFixes = defaultMaxReactionFixes
	}
	return cfg, nil
}
