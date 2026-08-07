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
)

// runStats is the session-level token accounting for one agent run.
type runStats struct {
	tokensIn   int64
	tokensOut  int64
	cacheRead  int64
	cacheWrite int64
}

// runPhase executes one named agent with opencode in a worktree and streams its
// JSON event stream into the trace file. The run is unbounded: no step ceiling
// and no deadline. A fixed bound is a guess about how much work an item needs,
// and a wrong guess stops real work partway and reports it as a gate failure.
// The context stays cancellable so a supervisor can stop a run on evidence
// rather than on a constant. Any non-zero harness exit marks the run failed:
// the error carries the exit status and stderr so a crash or truncation is
// never mistaken for work the gate can publish.
func runPhase(wtDir string, a *Agent, userPrompt, tracePath string) (runStats, error) {
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

	ctx, cancel := context.WithCancel(context.Background())
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
	if sc.Err() != nil {
		return stats, sc.Err()
	}
	if waitErr != nil {
		// A non-zero exit is a crash or a truncation. Record the status and
		// stderr so the failure is auditable.
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
