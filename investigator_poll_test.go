package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The Critic and Tester Polls are shell triggers with a Powder gate: without a
// configured identity and origin they must exit 1 (healthy skip) so a
// GitHub-only deployment wakes no investigator Run.
func TestInvestigatorPollCommandsUsePowderGateScripts(t *testing.T) {
	cfg, err := loadConfig(configPath("."))
	if err != nil {
		t.Fatal(err)
	}
	for agent, want := range map[string]string{
		"critic": "./agents/critic/poll.sh",
		"tester": "./agents/tester/poll.sh",
	} {
		got, ok := cfg.Agents[agent]
		if !ok {
			t.Fatalf("agent %q not configured", agent)
		}
		if got.Poll != want {
			t.Fatalf("agent %q poll=%q, want %q", agent, got.Poll, want)
		}
	}
}

func runPollScript(t *testing.T, path string, powderEnv map[string]string) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", path)
	env := make([]string, 0, len(os.Environ())+len(powderEnv))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "POWDER_AGENT=") ||
			strings.HasPrefix(entry, "POWDER_URL=") ||
			strings.HasPrefix(entry, "POWDER_API_BASE_URL=") {
			continue
		}
		env = append(env, entry)
	}
	for key, value := range powderEnv {
		env = append(env, key+"="+value)
	}
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s: %v", path, err)
		}
		return exitErr.ExitCode()
	}
	return 0
}

func TestInvestigatorPollScriptsSkipWithoutPowder(t *testing.T) {
	for _, path := range []string{"./agents/critic/poll.sh", "./agents/tester/poll.sh"} {
		if got := runPollScript(t, path, nil); got != 1 {
			t.Fatalf("%s without Powder env exit=%d, want 1", path, got)
		}
		if got := runPollScript(t, path, map[string]string{"POWDER_AGENT": "critic"}); got != 1 {
			t.Fatalf("%s with only POWDER_AGENT exit=%d, want 1", path, got)
		}
		if got := runPollScript(t, path, map[string]string{"POWDER_URL": "http://powder.invalid"}); got != 1 {
			t.Fatalf("%s with only POWDER_URL exit=%d, want 1", path, got)
		}
	}
}

func TestInvestigatorPollScriptsDispatchWithPowder(t *testing.T) {
	for _, path := range []string{"./agents/critic/poll.sh", "./agents/tester/poll.sh"} {
		if got := runPollScript(t, path, map[string]string{
			"POWDER_AGENT": "critic",
			"POWDER_URL":   "http://powder.invalid",
		}); got != 0 {
			t.Fatalf("%s with POWDER_URL exit=%d, want 0", path, got)
		}
		if got := runPollScript(t, path, map[string]string{
			"POWDER_AGENT":        "critic",
			"POWDER_API_BASE_URL": "http://powder.invalid",
		}); got != 0 {
			t.Fatalf("%s with POWDER_API_BASE_URL exit=%d, want 0", path, got)
		}
	}
}
