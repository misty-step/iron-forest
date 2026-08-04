package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the composition file forest.yaml. It declares the backlog
// source, the poll cadence, and the agent it dispatches per item.
type Config struct {
	Repo            string   `yaml:"repo"`
	PollIntervalSec int      `yaml:"poll_interval_seconds"`
	Agent           AgentCfg `yaml:"agent"`
	PriceInUSDPerm  float64  `yaml:"price_usd_per_m_input"`
	PriceOutUSDPerm float64  `yaml:"price_usd_per_m_output"`
}

// AgentCfg is the agent definition: which harness runs it, which model it
// uses, which prompt file is its system prompt, and which paths are off
// limits to it. This is the primitive-composition surface of the factory.
type AgentCfg struct {
	Harness      string   `yaml:"harness"`
	Model        string   `yaml:"model"`
	SystemPrompt string   `yaml:"system_prompt"`
	Protected    []string `yaml:"protected"`
}

const (
	modelDefault     = "openrouter-mint/deepseek-v4-flash-0731"
	promptDefault    = "agents/chew.md"
	claimLabel       = "forest:wip"
	failedLabel      = "forest:failed"
	commitAuthorName = "forest"
	commitAuthorMail = "forest@mistystep.io"
)

func defaultConfig() Config {
	return Config{
		PollIntervalSec: 30,
		Agent: AgentCfg{
			Harness:      "opencode",
			Model:        modelDefault,
			SystemPrompt: promptDefault,
			Protected:    []string{".forest/", "forest.yaml"},
		},
		PriceInUSDPerm:  0.09, // deepseek-v4-flash-0731, from the OpenRouter catalog
		PriceOutUSDPerm: 0.18,
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
	if cfg.Agent.Model == "" {
		cfg.Agent.Model = modelDefault
	}
	if cfg.Agent.SystemPrompt == "" {
		cfg.Agent.SystemPrompt = promptDefault
	}
	return cfg, nil
}
