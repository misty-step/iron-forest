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

// runStats is the session-level token accounting for one agent run.
type runStats struct {
	tokensIn   int64
	tokensOut  int64
	cacheRead  int64
	cacheWrite int64
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

	env, cleanup, err := childEnvironment()
	if err != nil {
		return stats, err
	}
	defer cleanup()

	cmd := exec.CommandContext(ctx, "opencode", "run",
		"--format", "json", "--model", a.Model, "--agent", a.Name,
		"--auto", userPrompt)
	cmd.Dir = wtDir
	cmd.Env = env
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

const childSystemPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// childEnvironment gives each run a private home so operator configuration and
// credentials stay unreachable. The home sits outside the worktree: the worktree
// is committed with `git add -A`, so a tool that writes to $HOME there would put
// its cache directories into the published branch.
// It preserves only the pinned toolchain and caches the factory checks need, and
// excludes gh, whose credential would reach far beyond the given worktree.
func childEnvironment() ([]string, func(), error) {
	mise, err := exec.LookPath("mise")
	if err != nil {
		return nil, func() {}, fmt.Errorf("locate mise: %w", err)
	}
	home, err := os.MkdirTemp("", "forest-home-")
	if err != nil {
		return nil, func() {}, fmt.Errorf("create child home: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(home) }
	binDir := filepath.Join(home, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("create child command directory: %w", err)
	}
	if err := os.Symlink(mise, filepath.Join(binDir, "mise")); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("stage mise for child: %w", err)
	}

	miseDataDir, miseShims := miseLocations(mise)
	env := []string{
		"HOME=" + home,
		"PATH=" + strings.Join([]string{binDir, miseShims, childSystemPath}, string(os.PathListSeparator)),
		"MISE_CONFIG_DIR=" + filepath.Join(binDir, "config"),
		"MISE_DATA_DIR=" + miseDataDir,
		"GOMODCACHE=" + goModuleCache(),
		// The compiler caches hold build products, never credentials. Leaving
		// them under the per-run HOME made every declared check compile the
		// world: measured 22s cold against about 1s warm.
		"GOCACHE=" + goBuildCache(),
	}
	return env, cleanup, nil
}

func goModuleCache() string {
	if cache := os.Getenv("GOMODCACHE"); cache != "" {
		return cache
	}
	home := os.Getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		gopath = filepath.Join(home, "go")
	}
	return filepath.Join(gopath, "pkg", "mod")
}

func goBuildCache() string {
	if cache := os.Getenv("GOCACHE"); cache != "" {
		return cache
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(os.Getenv("HOME"), ".cache", "go-build")
	}
	return filepath.Join(dir, "go-build")
}

func miseLocations(mise string) (string, string) {
	if dataDir := os.Getenv("MISE_DATA_DIR"); dataDir != "" {
		return dataDir, filepath.Join(dataDir, "shims")
	}
	for _, entry := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		entry = filepath.Clean(entry)
		if filepath.Base(entry) == "shims" && filepath.Base(filepath.Dir(entry)) == "mise" {
			return filepath.Dir(entry), entry
		}
	}
	dataDir := filepath.Clean(filepath.Join(filepath.Dir(mise), "..", "share", "mise"))
	return dataDir, filepath.Join(dataDir, "shims")
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
