package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const checkOutputTailBytes = 4000

// The note records check facts for later flows.
func runChecks(cfg Config, wtDir, runID string) (checksNote, error) {
	note := checksNote{
		Status: "pass",
		RunID:  runID,
		Time:   time.Now().UTC().Format(time.RFC3339),
	}
	env, cleanup, err := checkEnvironment()
	if err != nil {
		return note, err
	}
	defer cleanup()

	var startErr error
	for _, check := range cfg.Checks {
		started := time.Now()
		cmd := exec.Command("sh", "-c", check.Run)
		cmd.Dir = wtDir
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		result := checkResult{
			Name:    check.Name,
			Seconds: time.Since(started).Seconds(),
			Output:  tailCheckOutput(output),
		}
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				result.Code = exitErr.ExitCode()
				note.Status = "fail"
			} else {
				result.Code = -1
				note.Status = "fail"
				if startErr == nil {
					startErr = fmt.Errorf("start check %q: %w", check.Name, err)
				}
			}
		}
		note.Results = append(note.Results, result)
	}
	if startErr != nil {
		return note, startErr
	}
	return note, nil
}

// No per-check deadline: factory commands have their own bounds; killing one reports a failure this runner did not cause.
func tailCheckOutput(output []byte) string {
	if len(output) > checkOutputTailBytes {
		output = output[len(output)-checkOutputTailBytes:]
	}
	return string(output)
}

func checksSummary(c checksNote) string {
	if c.Status == "pass" {
		return "checks pass"
	}
	failed := make([]string, 0, len(c.Results))
	for _, result := range c.Results {
		if result.Code != 0 {
			failed = append(failed, fmt.Sprintf("%s(exit %d)", result.Name, result.Code))
		}
	}
	if len(failed) == 0 {
		return "checks fail"
	}
	return "checks fail: " + strings.Join(failed, ", ")
}
