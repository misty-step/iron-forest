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

// runAgent executes one bounded agent phase with opencode in a worktree and
// streams its JSON event stream into the trace file. The exit is not the
// verdict: a non-zero exit with work produced is judged by the gate.
func runAgent(wtDir, prompt, model, tracePath string, timeout time.Duration) (runStats, error) {
	var stats runStats
	if err := os.MkdirAll(filepath.Dir(tracePath), 0o755); err != nil {
		return stats, err
	}
	trace, err := os.Create(tracePath)
	if err != nil {
		return stats, err
	}
	defer trace.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "opencode", "run",
		"--format", "json", "--model", model, "--auto")
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
		_, _ = trace.Write(append(append([]byte{}, line...), '\n'))
		if ev, ok := parseStepFinish(line); ok {
			stats.tokensIn += ev.tokensIn
			stats.tokensOut += ev.tokensOut
			stats.cacheRead += ev.cacheRead
			stats.cacheWrite += ev.cacheWrite
		}
	}
	waitErr := cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		return stats, fmt.Errorf("agent timed out after %s", timeout)
	}
	_ = stderr
	return stats, waitErr
}

type stepTokens struct {
	tokensIn  int64
	tokensOut int64
	cacheRead int64
	cacheWrite int64
}

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
	return stepTokens{
		tokensIn:   ev.Part.Tokens.Input,
		tokensOut:  ev.Part.Tokens.Output,
		cacheRead:  ev.Part.Tokens.Cache.Read,
		cacheWrite: ev.Part.Tokens.Cache.Write,
	}, true
}

// price computes US dollar cost from token counts using forest's own price
// table; opencode reports zero cost for model ids outside its catalog.
func price(st runStats, cfg Config) float64 {
	totalIn := st.tokensIn + st.cacheRead + st.cacheWrite
	return float64(totalIn)/1e6*cfg.PriceInUSDPerm + float64(st.tokensOut)/1e6*cfg.PriceOutUSDPerm
}
