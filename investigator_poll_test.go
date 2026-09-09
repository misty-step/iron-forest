package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

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

func TestRetiredInvestigatorPollsRemainIdle(t *testing.T) {
	for _, path := range []string{"./.iron-forest/agents/critic/poll.sh", "./.iron-forest/agents/tester/poll.sh"} {
		for _, configuration := range []struct {
			name string
			env  map[string]string
		}{
			{name: "no credentials"},
			{name: "legacy credentials", env: map[string]string{
				"POWDER_AGENT":        "critic",
				"POWDER_URL":          "http://powder.invalid",
				"POWDER_API_BASE_URL": "http://powder.invalid",
			}},
		} {
			t.Run(path+"/"+configuration.name, func(t *testing.T) {
				if got := runPollScript(t, path, configuration.env); got != 1 {
					t.Fatalf("retired poll %s exit=%d, want healthy skip", path, got)
				}
			})
		}
	}
}
