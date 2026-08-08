package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// maxArgLen is the Linux ceiling on a single argv entry: MAX_ARG_STRLEN is
// PAGE_SIZE * 32, or 131072 bytes on a 4 KiB page. It is not ARG_MAX (4 MiB
// here). A prompt passed as one argv argument trips it with a raw fork/exec
// "argument list too long" before the agent is reached, so the harness never
// delivers a prompt through argv: it streams the prompt on stdin, which has no
// such per-entry ceiling. The value is named so a delivery failure can state
// the limit it is designed around.
const maxArgLen = 131072

type runProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	once sync.Once
	err  error
}

var runProcesses = struct {
	sync.Mutex
	stopping bool
	runs     map[int]*runProcess
}{runs: make(map[int]*runProcess)}

func killRunProcessGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

func startManagedCommand(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return os.ErrProcessDone
			}
			return killRunProcessGroup(cmd.Process.Pid)
		}
	}

	runProcesses.Lock()
	defer runProcesses.Unlock()
	if runProcesses.stopping {
		return errors.New("run start refused during hard stop")
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	process := &runProcess{cmd: cmd, done: make(chan struct{})}
	runProcesses.runs[cmd.Process.Pid] = process
	return nil
}

func beginRunWait(process *runProcess) {
	process.once.Do(func() {
		go func() {
			process.err = process.cmd.Wait()
			close(process.done)
		}()
	})
}

func waitRunCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	pid := cmd.Process.Pid
	runProcesses.Lock()
	process := runProcesses.runs[pid]
	runProcesses.Unlock()
	if process == nil {
		return os.ErrProcessDone
	}
	beginRunWait(process)
	<-process.done
	runProcesses.Lock()
	delete(runProcesses.runs, pid)
	err := process.err
	runProcesses.Unlock()
	return err
}

func runCommand(cmd *exec.Cmd) error {
	if err := startManagedCommand(cmd); err != nil {
		return err
	}
	return waitRunCommand(cmd)
}

func runOutput(cmd *exec.Cmd) ([]byte, error) {
	var output bytes.Buffer
	cmd.Stdout = &output
	err := runCommand(cmd)
	return output.Bytes(), err
}

func runCombinedOutput(cmd *exec.Cmd) ([]byte, error) {
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := runCommand(cmd)
	return output.Bytes(), err
}

func abortRunCommand(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = killRunProcessGroup(cmd.Process.Pid)
	_ = waitRunCommand(cmd)
}

func hardStopRunCommands() []error {
	runProcesses.Lock()
	runProcesses.stopping = true
	runs := make(map[int]*runProcess, len(runProcesses.runs))
	for pid, process := range runProcesses.runs {
		runs[pid] = process
	}
	runProcesses.Unlock()
	var errs []error
	for pid := range runs {
		if err := killRunProcessGroup(pid); err != nil && !errors.Is(err, os.ErrProcessDone) {
			errs = append(errs, fmt.Errorf("kill run process group %d: %w", pid, err))
		}
	}
	for _, process := range runs {
		beginRunWait(process)
	}
	if len(runs) == 0 {
		return errs
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for pid, process := range runs {
		select {
		case <-process.done:
		case <-timer.C:
			errs = append(errs, fmt.Errorf("wait for run process group %d: timeout", pid))
			return errs
		}
	}
	return errs
}

// promptDeliveryError reports a prompt that could not be delivered whole to the
// agent. It names both the prompt size and the delivery ceiling so the failure
// is auditable, and it deliberately replaces the kernel's raw fork/exec
// "argument list too long" message. A delivery failure is mechanical: the same
// prompt will keep failing identically, so treating it as content to repair
// would spend Fixer attempts on an unchanged situation. The durable stalled
// brake parks it instead.
type promptDeliveryError struct {
	size  int
	limit int
}

func (e *promptDeliveryError) Error() string {
	return fmt.Sprintf("prompt of %d bytes cannot be delivered whole; the delivery ceiling is %d bytes", e.size, e.limit)
}

// isPromptDelivery reports whether err is, or wraps, a promptDeliveryError. A
// flow uses it to classify a mechanical prompt-delivery failure apart from a
// content or agent failure: the same prompt keeps failing identically, so it
// must park (name prompt_failed) instead of spending Fixer attempts on an
// unchanged situation.
func isPromptDelivery(err error) bool {
	var pde *promptDeliveryError
	return errors.As(err, &pde)
}

// runTimeoutError reports a run that exceeded its declared wall-clock deadline
// and was cancelled. It is mechanical: the same run keeps exceeding the same
// declared bound, so it is never a content rejection a Fixer attempt could
// repair; it must park (name timeout_failed) instead of spending attempts on an
// unchanged situation. It names the elapsed time and the last observed trace
// event so an operator can see where the run stopped, and it carries the
// elapsed duration itself so a caller can reason about it.
type runTimeoutError struct {
	elapsed   time.Duration
	lastEvent string
}

func (e *runTimeoutError) Error() string {
	return fmt.Sprintf("agent run exceeded its deadline after %s; last trace event: %s", e.elapsed, e.lastEvent)
}

// isRunTimeout reports whether err is, or wraps, a runTimeoutError. A flow uses
// it to classify a mechanical deadline timeout apart from a content or agent
// failure: the same run keeps hitting the same declared bound, so it must park
// (name timeout_failed) instead of spending a Fixer attempt on an unchanged
// situation.
func isRunTimeout(err error) bool {
	var rte *runTimeoutError
	return errors.As(err, &rte)
}

// maxTraceEventLabel caps how much of a trace event an error message carries.
// A giant step event must not bloat a ledger row; the label names where the run
// stopped, not the whole event.
const maxTraceEventLabel = 200

// traceEventLabel renders one trace line for an error message, truncated so a
// huge event cannot bloat a ledger row. An empty trace reports "(none)".
func traceEventLabel(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return "(none)"
	}
	if len(line) > maxTraceEventLabel {
		return line[:maxTraceEventLabel] + "..."
	}
	return line
}

// runStats is the session-level token accounting for one agent run. Every field
// is a measured token class that must reach the ledger row; a class with no
// consumer is the under-statement the ledger exists to prevent.
type runStats struct {
	tokensIn   int64
	tokensOut  int64
	cacheRead  int64
	cacheWrite int64
	reasoning  int64
}

// runPhase executes one named agent with opencode in a worktree and streams its
// JSON event stream into the trace file. The prompt is written to a .prompt.txt
// file beside the trace and streamed to opencode on stdin, so its size is
// bounded by the model's context, not by Linux's per-argument ceiling (see
// maxArgLen). repoDir is the factory project: the provider configuration the
// run actually needs is read from its
// .opencode/opencode.json and staged into the run's external config root. The
// run is unbounded in steps: no step ceiling, because a fixed bound is a guess
// about how much work an item needs, and a wrong guess stops real work partway
// and reports it as a gate failure. Wall time carries the agent's declared
// deadline_seconds: loadAgent guarantees every loaded agent has a positive,
// finite bound, and a run that exceeds it is cancelled and returned as a
// runTimeoutError (see #207) so a stalled provider can never hold a lane
// forever. The context stays cancellable so a supervisor can stop a run on
// evidence rather than on a constant. Any non-zero harness exit marks the run
// failed: the error carries the exit status and stderr so a crash or truncation
// is never mistaken for work the gate can publish.
//
// runPhase is a package variable (see the indirection below) so a test can
// force a promptDeliveryError and drive a flow's mechanical classification end
// to end; the concrete implementation is runPhaseImpl.
var runPhase = runPhaseImpl

// runPhaseImpl is the concrete implementation behind runPhase.
func runPhaseImpl(repoDir, wtDir string, a *Agent, userPrompt, tracePath string) (runStats, error) {
	var stats runStats
	if err := os.MkdirAll(filepath.Dir(tracePath), 0o755); err != nil {
		return stats, err
	}
	trace, err := os.Create(tracePath)
	if err != nil {
		return stats, err
	}
	defer trace.Close()

	// The run gets a wall-clock deadline from the agent's declaration. A bound
	// on wall time is the mechanism that ends every stall, whatever its cause: a
	// provider that accepts a connection and never answers leaves the process
	// sleeping in epoll with the socket established, so no socket error ever
	// fires and a cancellable-but-deadline-free context would hold the lane open
	// forever. The deadline is per-lane because each agent declares its own, and
	// loadAgent guarantees every loaded agent carries a positive, finite bound.
	// The positive check below is kept defensively so even a zero value handed
	// in directly never arms an immediate-timeout context in an unintended way;
	// production runs always set the timeout because the deadline is validated
	// at load time.
	started := time.Now()
	var ctx context.Context
	var cancel context.CancelFunc
	if a.DeadlineSeconds > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(a.DeadlineSeconds)*time.Second)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()

	env, cleanup, err := childEnvironment()
	if err != nil {
		return stats, err
	}
	defer cleanup()

	// The opencode config root lives outside the worktree so the managed
	// repository's working tree never carries a factory artifact a hook or a
	// working-tree secret scanner would read. The rendered agent declaration and
	// the provider configuration the factory project actually uses both land in
	// the run's global opencode config directory, and opencode is pointed at that
	// root with XDG_CONFIG_HOME set in the child environment. The node_modules
	// opencode installs for its provider packages also lands in that root, never
	// under the worktree's .opencode/. The root is per-run and removed when the
	// run is done.
	cfgDir, err := newRunConfigDir(repoDir, a)
	if err != nil {
		return stats, err
	}
	defer os.RemoveAll(cfgDir)

	env = append(env, "XDG_CONFIG_HOME="+cfgDir)
	// opencode would otherwise also read a project-local .opencode/opencode.json
	// it discovers in the worktree and install the provider packages it needs
	// beside it, writing into a managed repository the factory commits to. That
	// local project context is disabled here so opencode reads only the external
	// root: the managed worktree stays free of factory artifacts regardless of
	// whether it happens to ship a .opencode of its own.
	env = append(env, "OPENCODE_DISABLE_PROJECT_CONFIG=1")

	// The full prompt is delivered to opencode on its stdin, never as an argv
	// entry: Linux caps one argument at maxArgLen, so a large prompt passed on
	// the command line fails with a raw fork/exec "argument list too long"
	// before opencode is reached. Stdin has no such per-entry ceiling, so the
	// prompt's only remaining bound is the model's context. A redacted copy is
	// written beside the trace so a run stays auditable without retaining
	// credential-shaped values from mutable Tracker or repository content.
	promptPath := filepath.Join(filepath.Dir(tracePath), filepath.Base(tracePath)+".prompt.txt")
	if err := os.WriteFile(promptPath, []byte(redactSecretShaped(userPrompt)), 0o600); err != nil {
		return stats, &promptDeliveryError{size: len(userPrompt), limit: maxArgLen}
	}

	cmd := exec.CommandContext(ctx, "opencode", "run",
		"--format", "json", "--model", a.Model, "--agent", a.Name,
		"--auto")
	cmd.Dir = wtDir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(userPrompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return stats, err
	}
	if err := startManagedCommand(cmd); err != nil {
		// With stdin delivery an argument-limit start error is not expected, but
		// if one ever surfaces it must be named, never the raw fork/exec text.
		if errors.Is(err, syscall.E2BIG) {
			return stats, &promptDeliveryError{size: len(userPrompt), limit: maxArgLen}
		}
		return stats, err
	}
	waited := false
	defer func() {
		if !waited {
			abortRunCommand(cmd)
		}
	}()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	lastTrace := ""
	for sc.Scan() {
		line := sc.Bytes()
		if _, err := trace.Write(append(line, '\n')); err != nil {
			return stats, err
		}
		if len(line) > 0 {
			lastTrace = string(line)
		}
		if st, ok := parseStepFinish(line); ok {
			stats.tokensIn += st.tokensIn
			stats.tokensOut += st.tokensOut
			stats.cacheRead += st.cacheRead
			stats.cacheWrite += st.cacheWrite
			stats.reasoning += st.reasoning
		}
	}
	waitErr := waitRunCommand(cmd)
	waited = true
	// A deadline expiry is detected before any exit-status reading: when the
	// context's timer fires, exec.CommandContext kills the child and Wait returns
	// because of that cancellation, not because of anything the agent did. That
	// is a mechanical stop, so name it timeout and record where the run stopped.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return stats, &runTimeoutError{
			elapsed:   time.Since(started),
			lastEvent: traceEventLabel(lastTrace),
		}
	}
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
// its cache directories into the published branch. It preserves only the pinned
// toolchain and caches the factory needs, and excludes gh, whose credential
// would reach far beyond the given worktree. Non-Go tools the managed repo
// declares reach the check child through mise-managed tools with working shims
// (already on PATH) or the host toolchain mechanism (see checkEnvironment); an
// agent run gets neither, so host toolchain reach stays scoped to checks.
func childEnvironment() ([]string, func(), error) {
	return childBaseEnv(false)
}

// checkEnvironment is the child environment for a runChecks run: the common
// environment plus the operator-declared host toolchain mechanism that lets a
// managed repo's checks: reach a non-Go toolchain whose driver lives outside
// the scrubbed PATH. The mechanism is applied only here and never to an agent
// run (see childEnvironment), so neither host binaries on PATH nor allowlisted
// toolchain metadata escape into an opencode run. Host toolchain directories
// are named in FOREST_CHECK_PATH (see checkHostBins); toolchain metadata a host
// proxy must read to resolve its real driver arrives via FOREST_CHECK_ENV (see
// checkHostEnv). It is a variable so a test can force a preflight failure and
// drive the durable-fact path end to end.
var checkEnvironment = func() ([]string, func(), error) {
	return childBaseEnv(true)
}

// childBaseEnv builds the child environment used by every run. When
// hostToolchain is true (check runs only), the operator-declared host toolchain
// directories and metadata are applied: FOREST_CHECK_PATH directories go on the
// child PATH ahead of the mise shims so a working host driver resolves before a
// dead shim, and allowlisted FOREST_CHECK_ENV metadata is appended. Agent runs
// pass false and get neither.
func childBaseEnv(hostToolchain bool) ([]string, func(), error) {
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

	var hostBins []string
	if hostToolchain {
		hostBins = checkHostBins()
	}
	miseDataDir, miseShims := miseLocations(mise)
	env := []string{
		"HOME=" + home,
		"PATH=" + childPath(binDir, miseShims, hostBins),
		"MISE_CONFIG_DIR=" + filepath.Join(binDir, "config"),
		"MISE_DATA_DIR=" + miseDataDir,
		"GOMODCACHE=" + goModuleCache(),
		// The compiler caches hold build products, never credentials. Leaving
		// them under the per-run HOME made every declared check compile the
		// world: measured 22s cold against about 1s warm.
		"GOCACHE=" + goBuildCache(),
	}
	// Operator-declared host toolchain metadata (see checkHostEnv) is appended
	// after the private environment only for check runs. checkHostEnv drops any
	// key outside the allowlist, so FOREST_CHECK_ENV can never shadow the
	// private HOME, the scrubbed PATH, or a managed cache, nor introduce a
	// credential by value or by path.
	if hostToolchain {
		env = append(env, checkHostEnv()...)
	}
	return env, cleanup, nil
}

// checkHostBins reads the operator-declared host toolchain directories from
// FOREST_CHECK_PATH, a platform path-list. These are directories whose drivers
// live outside the scrubbed child PATH (for example rustup's ~/.cargo/bin).
// They are inserted ahead of the mise shims so a working host binary resolves
// before a dead shim. Blank entries are ignored.
func checkHostBins() []string {
	var dirs []string
	for _, entry := range filepath.SplitList(os.Getenv("FOREST_CHECK_PATH")) {
		if entry == "" {
			continue
		}
		dirs = append(dirs, filepath.Clean(entry))
	}
	return dirs
}

// hostEnvAllowlist is the set of host toolchain metadata variables checkHostEnv
// is allowed to carry into a check child. It is deliberately small and curated:
// an explicit allowlist is sound where a substring denylist is not, so a
// credential under any name — CI_JOB_JWT, AWS_ACCESS_KEY_ID, KUBECONFIG,
// GIT_CONFIG_GLOBAL, GH_TOKEN — can never reach the child. Only variables that
// name a metadata store that provably holds no credentials are listed.
// RUSTUP_HOME points at the rustup install root (settings and toolchains), which
// holds no credentials; CARGO_HOME is deliberately absent because ~/.cargo holds
// credentials.toml, so pointing a check at it would expose the operator's
// registry token.
var hostEnvAllowlist = map[string]bool{
	"RUSTUP_HOME": true,
}

// checkHostEnv reads the operator-declared host toolchain metadata from
// FOREST_CHECK_ENV, a newline-separated list of KEY=VALUE pairs, and returns
// them as child environment entries. Only entries whose key is on
// hostEnvAllowlist are carried in; every other key is dropped. Blank lines and
// entries with no "=" separator are ignored. Because os/exec.Cmd.Env resolves
// duplicates by last occurrence, dropping anything outside the allowlist is what
// keeps the private environment authoritative and guarantees a credential cannot
// leak into the child either by value or by the path a value names.
func checkHostEnv() []string {
	var entries []string
	for _, line := range strings.Split(os.Getenv("FOREST_CHECK_ENV"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if !hostEnvAllowed(key) {
			continue
		}
		entries = append(entries, line)
	}
	return entries
}

// hostEnvAllowed reports whether an operator-declared FOREST_CHECK_ENV key is on
// the curated metadata allowlist. Anything not listed is refused: the mechanism
// is non-credential by construction, so it never needs to guess a secret's name
// from substrings and can never be fooled by an unlisted variant (a JWT, an
// access-keys value, a kubeconfig path, a git config path).
func hostEnvAllowed(key string) bool {
	return hostEnvAllowlist[strings.ToUpper(key)]
}

// childPath assembles the PATH for a child environment. Order is load-bearing:
// the private bin directory (the mise symlink) comes first so the harness is
// authoritative, then the declared host toolchain directories, then the mise
// shims, then the fixed system path. Host toolchains precede the shims so a
// working host binary wins over a dead mise shim.
func childPath(binDir, miseShims string, hostBins []string) string {
	dirs := make([]string, 0, 2+len(hostBins))
	dirs = append(dirs, binDir)
	dirs = append(dirs, hostBins...)
	dirs = append(dirs, miseShims, childSystemPath)
	return strings.Join(dirs, string(os.PathListSeparator))
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
	reasoning  int64
}

// parseStepFinish extracts per-step token deltas from a step_finish event.
func parseStepFinish(line []byte) (stepTokens, bool) {
	var ev struct {
		Type string `json:"type"`
		Part struct {
			Tokens struct {
				Input     int64 `json:"input"`
				Output    int64 `json:"output"`
				Reasoning int64 `json:"reasoning"`
				Cache     struct {
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
		reasoning:  ev.Part.Tokens.Reasoning,
	}, true
}
