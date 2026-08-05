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
// declares a budget. The exit code is not the verdict: a non-zero exit with work
// produced is judged by the gate.
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
	for sc.Scan() {
		line := sc.Bytes()
		if _, err := trace.Write(append(line, '\n')); err != nil {
			return stats, err
		}
		if st, ok := parseStepFinish(line); ok {
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
	_ = stderr
	if sc.Err() != nil {
		return stats, sc.Err()
	}
	// opencode exits non-zero when it runs out of steps but still produced
	// work; the gate decides, so a non-nil waitErr is not a failure here.
	_ = waitErr
	return stats, nil
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
