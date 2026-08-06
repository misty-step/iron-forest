package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runStats is the session-level token and cost accounting for one agent run.
type runStats struct {
	tokensIn   int64
	tokensOut  int64
	cacheRead  int64
	cacheWrite int64
	costUSD    float64
}

// runPhase executes one named agent with opencode in a worktree and streams its
// JSON event stream into the trace file. The run is bounded only when the agent
// declares a budget. A harness exit outside the documented out-of-steps case
// marks the run failed: the error carries the exit status and stderr so a crash
// or truncation is never mistaken for work the gate can publish.
func runPhase(wtDir string, a *Agent, userPrompt, tracePath string, budget time.Duration) (runStats, error) {
	var stats runStats
	if err := renderMarkdown(wtDir, a); err != nil {
		return stats, err
	}
	if err := os.MkdirAll(filepath.Dir(tracePath), 0o755); err != nil {
		return stats, err
	}
	trace, err := os.Create(tracePath)
	if err != nil {
		return stats, err
	}
	defer trace.Close()

	ctx, cancel := phaseContext(budget)
	defer cancel()

	cmd := exec.CommandContext(ctx, "opencode", "run",
		"--format", "json", "--model", a.Model, "--agent", a.Name,
		"--auto", userPrompt)
	cmd.Dir = wtDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return stats, err
	}
	if err := cmd.Start(); err != nil {
		return stats, err
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	sawStepFinish := false
	for sc.Scan() {
		line := sc.Bytes()
		if _, err := trace.Write(append(line, '\n')); err != nil {
			return stats, err
		}
		if st, ok := parseStepFinish(line); ok {
			sawStepFinish = true
			stats.tokensIn += st.tokensIn
			stats.tokensOut += st.tokensOut
			stats.cacheRead += st.cacheRead
			stats.cacheWrite += st.cacheWrite
		}
	}
	waitErr := cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		return stats, fmt.Errorf("agent timed out after %s", budget)
	}
	if sc.Err() != nil {
		return stats, sc.Err()
	}
	if waitErr != nil && !isOutOfSteps(waitErr, sawStepFinish, stderr.String()) {
		// The harness exited outside the documented out-of-steps case: the run
		// is failed. Record the status and stderr so the failure is auditable.
		return stats, fmt.Errorf("agent exited %q: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return stats, nil
}

// isOutOfSteps reports whether a non-zero opencode exit is the documented
// step-limit case. The harness announces that it hit the agent's step budget on
// stderr with the "out of steps" message; only when that explicit announcement
// is present after at least one completed step_finish does the gate decide the
// run, so a provider or API crash that exits non-zero after earlier work is
// never mistaken for an out-of-steps stop. Every other non-zero exit is a crash
// or a truncation. Step completion is tracked as a boolean rather than the
// token totals so a zero-token step_finish still counts as work.
func isOutOfSteps(waitErr error, sawStepFinish bool, stderr string) bool {
	ee, ok := waitErr.(*exec.ExitError)
	if !ok {
		return false
	}
	// A negative exit code means the process was killed by a signal rather than
	// exiting through os.Exit: that is a crash, not a stop at the step budget.
	if ee.ExitCode() < 0 {
		return false
	}
	return sawStepFinish && strings.Contains(stderr, "out of steps")
}

// phaseContext bounds one agent run. A budget of zero or less means no deadline:
// an agent reasoning at maximum effort can work for a long time and still be
// productive, so the harness waits rather than killing the run and discarding
// every token it already spent. The operator's escape is a signal, not a clock.
func phaseContext(budget time.Duration) (context.Context, context.CancelFunc) {
	if budget <= 0 {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeout(context.Background(), budget)
}

type stepTokens struct {
	tokensIn   int64
	tokensOut  int64
	cacheRead  int64
	cacheWrite int64
}

// parseStepFinish extracts per-step token deltas from a step_finish event.
func parseStepFinish(line []byte) (stepTokens, bool) {
	var ev struct {
		Type string `json:"type"`
		Part struct {
			Tokens struct {
				Input  int64 `json:"input"`
				Output int64 `json:"output"`
				Cache  struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
		} `json:"part"`
	}
	if err := json.Unmarshal(line, &ev); err != nil || ev.Type != "step_finish" {
		return stepTokens{}, false
	}
	return stepTokens{tokensIn: ev.Part.Tokens.Input, tokensOut: ev.Part.Tokens.Output,
		cacheRead: ev.Part.Tokens.Cache.Read, cacheWrite: ev.Part.Tokens.Cache.Write}, true
}

// price computes US dollar cost from token counts using the agent's own price
// table; opencode reports zero cost for model ids outside its catalog.
func price(st runStats, a *Agent) float64 {
	inUSD := float64(st.tokensIn) / 1e6 * a.PriceInUSDPerM
	outUSD := float64(st.tokensOut) / 1e6 * a.PriceOutUSDPerM
	return inUSD + outUSD
}
