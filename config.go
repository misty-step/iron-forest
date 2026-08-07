package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultAgentsDir is where agent declarations live, one directory each.
	DefaultAgentsDir = "agents"
	// WorkspaceDir holds the ledger, traces, and per-run worktrees.
	WorkspaceDir = ".forest"
	// BranchPrefix names every branch a flow creates.
	BranchPrefix = "forest/"

	// failedLabel is the human hint a lane leaves when a subject needs a person.
	// It is a tracker convenience, never state the factory reads back.
	failedLabel = "forest:failed"
)

// CommitIdentity is the author every flow's commits carry. It is declared, not
// derived from a host account, so a run is attributable in any repository.
type CommitIdentity struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email"`
}

// configPath is the composition file for a checkout.
func configPath(repoDir string) string { return filepath.Join(repoDir, "forest.yaml") }

// workspaceDir is where a checkout keeps its ledger, traces, and worktrees.
func workspaceDir(repoDir string) string { return filepath.Join(repoDir, WorkspaceDir) }

// Config is forest.yaml: the work source, the paths no agent may touch, the
// lease policy, the checks the factory runs itself, the flows that are on, and
// the optional human-facing projection.
type Config struct {
	Repo       string         `yaml:"repo"`
	Protected  []string       `yaml:"protected"`
	Lease      LeasePolicy    `yaml:"lease"`
	Commit     CommitIdentity `yaml:"commit"`
	Checks     []Check        `yaml:"checks"`
	Flows      Flows          `yaml:"flows"`
	Projection Projection     `yaml:"projection"`
}

// LeasePolicy bounds how long one worker may own a subject. A lease older than
// TTLSeconds may be broken by another worker, so a host that dies mid-run
// cannot make its subject unworkable forever. Zero disables breaking.
type LeasePolicy struct {
	TTLSeconds int `yaml:"ttl_seconds"`
}

// TTL is the lease policy as a duration.
func (l LeasePolicy) TTL() time.Duration { return time.Duration(l.TTLSeconds) * time.Second }

// Check is one command the factory runs itself against a worktree. The result
// is a fact the factory writes to a note, not a status it reads from a host.
type Check struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
}

// Flows declares the lanes. Each lane runs independently, on its own clock,
// coordinating only through leases and notes in the repository.
type Flows struct {
	Builder  BuilderFlowCfg  `yaml:"builder"`
	Verifier VerifierFlowCfg `yaml:"verifier"`
	Fixer    FixerFlowCfg    `yaml:"fixer"`
}

// FlowCfg is what every lane declares: whether it is on, which agent it runs,
// and how long it sleeps between passes.
type FlowCfg struct {
	Enabled     bool   `yaml:"enabled"`
	Agent       string `yaml:"agent"`
	IntervalSec int    `yaml:"interval_seconds"`
}

// BuilderFlowCfg turns tracker items into branches. ExcludeLabels are the
// tracker-side signals that make an item ineligible; they are a convenience of
// the current tracker, not part of the factory's state. RequireLabels, when
// non-empty, switches selection from opt-out to opt-in: an open item is
// eligible only when it carries every required label. ExcludeLabels
// composes with the opt-in, so a braked item stays ineligible. An empty
// RequireLabels keeps the opt-out selection unchanged.
type BuilderFlowCfg struct {
	FlowCfg       `yaml:",inline"`
	ExcludeLabels []string `yaml:"exclude_labels"`
	RequireLabels []string `yaml:"require_labels"`
}

// VerifierFlowCfg reviews branches and owns the merge. Merge names the history
// shape: squash keeps one commit per subject on the target branch; ff keeps the
// branch's own commits and refuses when the branch is behind. AutoMerge gates
// the merge effect itself, so a factory can review without merging.
type VerifierFlowCfg struct {
	FlowCfg   `yaml:",inline"`
	Merge     string `yaml:"merge"`
	AutoMerge bool   `yaml:"auto_merge"`
}

// FixerFlowCfg repairs branches that were rejected or failed their checks.
// Attempts bounds how many repairs one branch may receive before it waits for
// a human.
type FixerFlowCfg struct {
	FlowCfg  `yaml:",inline"`
	Attempts int `yaml:"attempts"`
}

// Projection is the optional, one-way human surface: publish a branch as a pull
// request and mirror decisions as comments. The factory never reads it back.
// MergeViaHost is for a protected target branch, where only the host may merge.
type Projection struct {
	Enabled      bool `yaml:"enabled"`
	MergeViaHost bool `yaml:"merge_via_host"`
}

func defaultConfig() Config {
	return Config{
		Protected: []string{
			".forest/", "forest.yaml", "agents/", ".opencode/opencode.json",
		},
		Lease: LeasePolicy{TTLSeconds: 7200},
		// The identity is generic on purpose. A repository that wants its own
		// author declares it; the factory never assumes an organization.
		Commit: CommitIdentity{Name: "forest", Email: "forest@invalid"},
		Flows: Flows{
			Builder: BuilderFlowCfg{
				FlowCfg:       FlowCfg{Enabled: true, Agent: "builder", IntervalSec: 30},
				ExcludeLabels: []string{"parked", failedLabel},
			},
			Verifier: VerifierFlowCfg{
				FlowCfg: FlowCfg{Enabled: true, Agent: "verifier", IntervalSec: 20},
				Merge:   "squash", AutoMerge: false,
			},
			Fixer: FixerFlowCfg{
				FlowCfg: FlowCfg{Enabled: true, Agent: "builder", IntervalSec: 40},
				// A repair loop that keeps producing new commits is working, so this
				// ceiling is a runaway guard, not a policy on how many passes a fix
				// may take. Progress on one revision is bounded separately.
				Attempts: 10,
			},
		},
		Projection: Projection{Enabled: true, MergeViaHost: false},
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
	d := defaultConfig()
	// The checks are the gate, and only this repository knows how to build and
	// test itself. A factory that guessed a toolchain would verify the wrong
	// thing, so an undeclared gate is a configuration error.
	if len(cfg.Checks) == 0 {
		return cfg, fmt.Errorf("%s: at least one check is required", path)
	}
	if cfg.Commit.Name == "" {
		cfg.Commit.Name = d.Commit.Name
	}
	if cfg.Commit.Email == "" {
		cfg.Commit.Email = d.Commit.Email
	}
	if cfg.Lease.TTLSeconds < 0 {
		cfg.Lease.TTLSeconds = 0
	}
	if cfg.Flows.Builder.Agent == "" {
		cfg.Flows.Builder.Agent = d.Flows.Builder.Agent
	}
	if cfg.Flows.Verifier.Agent == "" {
		cfg.Flows.Verifier.Agent = d.Flows.Verifier.Agent
	}
	if cfg.Flows.Fixer.Agent == "" {
		cfg.Flows.Fixer.Agent = d.Flows.Fixer.Agent
	}
	if cfg.Flows.Builder.IntervalSec <= 0 {
		cfg.Flows.Builder.IntervalSec = d.Flows.Builder.IntervalSec
	}
	if cfg.Flows.Verifier.IntervalSec <= 0 {
		cfg.Flows.Verifier.IntervalSec = d.Flows.Verifier.IntervalSec
	}
	if cfg.Flows.Fixer.IntervalSec <= 0 {
		cfg.Flows.Fixer.IntervalSec = d.Flows.Fixer.IntervalSec
	}
	switch cfg.Flows.Verifier.Merge {
	case "":
		cfg.Flows.Verifier.Merge = d.Flows.Verifier.Merge
	case "squash", "ff":
	default:
		return cfg, fmt.Errorf("%s: merge must be squash or ff, got %q", path, cfg.Flows.Verifier.Merge)
	}
	if cfg.Flows.Fixer.Attempts <= 0 {
		cfg.Flows.Fixer.Attempts = d.Flows.Fixer.Attempts
	}
	for i, c := range cfg.Checks {
		if c.Name == "" || c.Run == "" {
			return cfg, fmt.Errorf("%s: checks[%d] needs a name and a run", path, i)
		}
	}
	return cfg, nil
}
