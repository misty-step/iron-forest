package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

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

func waitRunCommandContext(ctx context.Context, cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() {
		done <- waitRunCommand(cmd)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = killRunProcessGroup(cmd.Process.Pid)
		}
		return <-done
	}
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
	line = redactSecretShaped(strings.TrimSpace(line))
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

// runPhase executes one named agent with OpenCode in a worktree and streams its
// JSON event stream into the trace file. The prompt is written to a .prompt.txt
// file beside the trace and streamed to OpenCode on stdin, so only the model
// context bounds its size. repoDir is the factory project whose provider
// configuration is staged into the run's external config root.
// The run is unbounded in steps: no step ceiling, because a fixed bound is a guess
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
// runPhase is a package variable so failure-path tests can substitute the
// harness without launching a provider. The concrete implementation follows.
var runPhase = runPhaseImpl

type traceScanResult struct {
	stats     runStats
	lastTrace string
	err       error
}

func scanRunTrace(stdout io.Reader, trace io.StringWriter) traceScanResult {
	var result traceScanResult
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		traceLine := redactSecretShaped(string(line))
		if _, err := trace.WriteString(traceLine + "\n"); err != nil {
			result.err = err
			return result
		}
		if len(line) > 0 {
			result.lastTrace = traceLine
		}
		if step, ok := parseStepFinish(line); ok {
			result.stats.tokensIn += step.tokensIn
			result.stats.tokensOut += step.tokensOut
			result.stats.cacheRead += step.cacheRead
			result.stats.cacheWrite += step.cacheWrite
			result.stats.reasoning += step.reasoning
		}
	}
	result.err = scanner.Err()
	return result
}

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
		deadline, err := agentDeadline(a.DeadlineSeconds)
		if err != nil {
			return stats, err
		}
		ctx, cancel = context.WithTimeout(context.Background(), deadline)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()

	env, cleanup, err := childEnvironment(repoDir, wtDir, a)
	if err != nil {
		return stats, err
	}
	defer cleanup()

	// The opencode config root lives outside the worktree so the managed
	// repository's working tree never carries a factory artifact a hook or a
	// working-tree secret scanner would read. The rendered agent declaration and
	// repository-declared provider configuration both land in the run's global
	// opencode config directory, and opencode receives that root through
	// XDG_CONFIG_HOME. The node_modules
	// opencode installs for its provider packages also lands in that root, never
	// under the worktree's .opencode/. The root is per-run and removed when the
	// run is done.
	cfgDir, err := newRunConfigDir(childEnvValue(env, "HOME"), repoDir, a)
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

	// The full prompt goes to OpenCode on stdin, never in argv. A redacted copy
	// stays beside the trace for audit without retaining credential-shaped text
	// from mutable Tracker or repository content.
	promptPath := filepath.Join(filepath.Dir(tracePath), filepath.Base(tracePath)+".prompt.txt")
	if err := os.WriteFile(promptPath, []byte(redactSecretShaped(userPrompt)), 0o600); err != nil {
		return stats, fmt.Errorf("write prompt audit: %w", err)
	}

	sandbox, err := prepareChildSandbox(repoDir, wtDir, env)
	if err != nil {
		return stats, err
	}
	if err := validateSandboxOpenCode(ctx, sandbox); err != nil {
		return stats, err
	}
	cmd, err := sandbox.commandWithFiles(ctx, nil, "opencode", "run",
		"--format", "json", "--model", a.Model, "--agent", a.Name,
		"--auto")
	if err != nil {
		return stats, err
	}
	cmd.Stdin = strings.NewReader(userPrompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return stats, err
	}
	if err := startManagedCommand(cmd); err != nil {
		return stats, fmt.Errorf("start agent: %w", err)
	}
	waited := false
	defer func() {
		if !waited {
			abortRunCommand(cmd)
		}
	}()

	scanDone := make(chan traceScanResult, 1)
	go func() {
		scanDone <- scanRunTrace(stdout, trace)
	}()

	var scan traceScanResult
	var waitErr error
	select {
	case scan = <-scanDone:
		if scan.err != nil {
			abortRunCommand(cmd)
			waited = true
		} else {
			waitErr = waitRunCommandContext(ctx, cmd)
			waited = true
		}
	case <-ctx.Done():
		// Bubblewrap's default PID-1 reaper owns the complete child namespace.
		// Killing the managed Bubblewrap process therefore kills a descendant
		// even after setsid, and Wait closes StdoutPipe so a detached writer
		// cannot hold this trace drain past the declared deadline.
		abortRunCommand(cmd)
		waited = true
		scan = <-scanDone
	}
	stats = scan.stats
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return stats, &runTimeoutError{
			elapsed:   time.Since(started),
			lastEvent: traceEventLabel(scan.lastTrace),
		}
	}
	if scan.err != nil {
		return stats, scan.err
	}
	if waitErr != nil {
		// A non-zero exit is a crash or a truncation. Record the status and
		// stderr so the failure is auditable.
		return stats, fmt.Errorf("agent exited %q: %s", waitErr, redactSecretShaped(strings.TrimSpace(stderr.String())))
	}
	return stats, nil
}

const childSystemPath = "/usr/sbin:/usr/bin:/sbin:/bin"

// childEnvironment gives one agent run a private home, repository-pinned
// toolchains, and a shell that accepts only plain bash_allow commands.
func childEnvironment(repoDir, wtDir string, a *Agent) ([]string, func(), error) {
	if err := validatePinnedTools(repoDir); err != nil {
		return nil, func() {}, err
	}
	env, cleanup, err := childBaseEnv(false, wtDir)
	if err != nil {
		return nil, cleanup, err
	}
	home := childEnvValue(env, "HOME")
	miseSource, err := hostMiseDataDir()
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if err := stagePinnedOpenCode(home, miseSource); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if err := stagePinnedToolDrivers(home); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if a != nil && a.BashAllow != nil {
		if err := installAgentShell(home, a.BashAllow); err != nil {
			cleanup()
			return nil, func() {}, err
		}
	}
	return env, cleanup, nil
}

// checkEnvironment adds operator-declared Host toolchain inputs only for
// declared checks. Agent runs never receive those paths or metadata.
var checkEnvironment = func(repoDir, wtDir string) ([]string, func(), error) {
	if err := validatePinnedTools(repoDir); err != nil {
		return nil, func() {}, err
	}
	return childBaseEnv(true, wtDir)
}

// childBaseEnv builds the child environment used by every run. When
// hostToolchain is true (check runs only), the operator-declared host toolchain
// directories and allowlisted metadata are added. Agent runs receive neither.
func childBaseEnv(hostToolchain bool, forbiddenRoots ...string) ([]string, func(), error) {
	mise, err := locateRequiredExecutable("mise")
	if err != nil {
		return nil, func() {}, err
	}
	home, err := createPrivateChildHome(forbiddenRoots...)
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(home) }
	binDir := filepath.Join(home, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("create child command directory: %w", err)
	}
	runtimeDir := filepath.Join(home, "run")
	miseConfigDir := filepath.Join(home, "mise-config")
	miseStateDir := filepath.Join(home, "state", "mise")
	cacheDir := filepath.Join(home, "cache")
	miseCacheDir := filepath.Join(cacheDir, "mise")
	goModCache := filepath.Join(cacheDir, "go-mod")
	goBuildCache := filepath.Join(cacheDir, "go-build")
	for _, path := range []string{runtimeDir, miseConfigDir, miseStateDir, miseCacheDir, goModCache, goBuildCache} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("create private child directory: %w", err)
		}
	}
	// gh can read an operating-system keyring even with a private HOME and no
	// token variables. Shadow normal child lookup before any Host path.
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte("#!/bin/sh\nexit 127\n"), 0o555); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("block gh for child: %w", err)
	}
	if err := copySandboxExecutable(mise, filepath.Join(binDir, "mise")); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("stage mise for child: %w", err)
	}

	var hostBins []string
	if hostToolchain {
		hostBins = checkHostBins()
	}
	env := []string{
		"HOME=" + home,
		"XDG_RUNTIME_DIR=" + runtimeDir,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + filepath.Join(runtimeDir, "no-session-bus"),
		"SSH_AUTH_SOCK=" + filepath.Join(runtimeDir, "no-ssh-agent"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"PATH=" + childPath(binDir, hostBins),
		"MISE_CONFIG_DIR=" + miseConfigDir,
		"MISE_DATA_DIR=" + filepath.Join(home, "mise"),
		"MISE_STATE_DIR=" + miseStateDir,
		"MISE_CACHE_DIR=" + miseCacheDir,
		"GOMODCACHE=" + goModCache,
		"GOCACHE=" + goBuildCache,
	}
	if hostToolchain {
		env = append(env, checkHostEnv()...)
	}
	return env, cleanup, nil
}

func createPrivateChildHome(forbiddenRoots ...string) (string, error) {
	var lastErr error
	for _, base := range []string{os.TempDir(), "/tmp", "/var/tmp"} {
		resolvedBase, err := filepath.EvalSymlinks(base)
		if err != nil {
			lastErr = err
			continue
		}
		blocked := false
		for _, root := range forbiddenRoots {
			if root == "" {
				continue
			}
			resolvedRoot, err := filepath.EvalSymlinks(root)
			if err != nil {
				return "", fmt.Errorf("resolve private-home exclusion %q: %w", root, err)
			}
			if sandboxPathWithin(resolvedRoot, resolvedBase) {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		home, err := os.MkdirTemp(resolvedBase, "forest-home-")
		if err != nil {
			lastErr = err
			continue
		}
		overlap := false
		for _, root := range forbiddenRoots {
			if root == "" {
				continue
			}
			resolvedRoot, _ := filepath.EvalSymlinks(root)
			if sandboxPathWithin(resolvedRoot, home) || sandboxPathWithin(home, resolvedRoot) {
				overlap = true
				break
			}
		}
		if overlap {
			_ = os.RemoveAll(home)
			continue
		}
		return home, nil
	}
	if lastErr != nil {
		return "", fmt.Errorf("create child home outside protected paths: %w", lastErr)
	}
	return "", fmt.Errorf("create child home outside protected paths: no safe temporary root")
}

const (
	requiredGoVersion       = "1.26.5"
	requiredOpenCodeVersion = "1.18.11"
)

func validatePinnedTools(repoDir string) error {
	body, err := readRepositoryFile(repoDir, ".mise.toml")
	if err != nil {
		return fmt.Errorf("read pinned tool declaration: %w", err)
	}
	found := make(map[string]string)
	section := ""
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if section != "tools" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key != "go" && key != "opencode" {
			continue
		}
		if len(value) < 2 || value[0] != value[len(value)-1] ||
			value[0] != '"' && value[0] != '\'' {
			return fmt.Errorf("pinned tool %s must use a literal version", key)
		}
		if _, exists := found[key]; exists {
			return fmt.Errorf("pinned tool %s is declared more than once", key)
		}
		found[key] = value[1 : len(value)-1]
	}
	for tool, version := range map[string]string{
		"go": requiredGoVersion, "opencode": requiredOpenCodeVersion,
	} {
		if found[tool] != version {
			return fmt.Errorf("pinned tool %s version %q does not match required %s", tool, found[tool], version)
		}
	}
	return nil
}

func hostMiseDataDir() (string, error) {
	mise, err := locateRequiredExecutable("mise")
	if err != nil {
		return "", err
	}
	dataDir, _ := miseLocations(mise)
	return validateMiseDataDir(dataDir)
}

func pinnedMiseInstall(miseData, tool, version string) (string, error) {
	expected := filepath.Join(miseData, "installs", tool, version)
	root, err := filepath.EvalSymlinks(expected)
	if err != nil {
		return "", fmt.Errorf("resolve pinned %s installation: %w", tool, err)
	}
	if !sandboxPathWithin(filepath.Join(miseData, "installs", tool), root) {
		return "", fmt.Errorf("pinned %s installation %q escapes mise data", tool, root)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return "", fmt.Errorf("pinned %s installation %q is not a directory", tool, root)
	}
	return root, nil
}

func stagePinnedOpenCode(home, miseData string) error {
	source, err := locatePinnedOpenCode(miseData)
	if err != nil {
		return err
	}
	if err := copySandboxExecutable(source, filepath.Join(home, "bin", "opencode")); err != nil {
		return fmt.Errorf("stage opencode for child: %w", err)
	}
	return nil
}

func locatePinnedOpenCode(miseData string) (string, error) {
	installRoot, err := pinnedMiseInstall(miseData, "opencode", requiredOpenCodeVersion)
	if err != nil {
		return "", err
	}
	for _, candidate := range []string{
		filepath.Join(installRoot, "opencode"),
		filepath.Join(installRoot, "bin", "opencode"),
	} {
		source, err := validateExecutablePath("opencode", candidate)
		if err == nil && sandboxPathWithin(installRoot, source) {
			return source, nil
		}
	}
	return "", fmt.Errorf("locate opencode in pinned installation %q", installRoot)
}

func stagePinnedToolDrivers(home string) error {
	for _, name := range []string{"go", "gofmt"} {
		script := fmt.Sprintf(
			"#!/bin/sh\nexec \"$MISE_DATA_DIR/installs/go/%s/bin/%s\" \"$@\"\n",
			requiredGoVersion, name,
		)
		if err := os.WriteFile(filepath.Join(home, "bin", name), []byte(script), 0o555); err != nil {
			return fmt.Errorf("stage pinned %s driver: %w", name, err)
		}
	}
	return nil
}

func validateOpenCodeVersionOutput(out []byte) error {
	if version := strings.TrimSpace(string(out)); version != requiredOpenCodeVersion {
		return fmt.Errorf("opencode version %q does not match required %s", version, requiredOpenCodeVersion)
	}
	return nil
}

func validateSandboxOpenCode(parent context.Context, sandbox childSandbox) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd, err := sandbox.commandWithFiles(ctx, nil, "opencode", "--version")
	if err != nil {
		return err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("verify sandboxed opencode version: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return validateOpenCodeVersionOutput(out)
}

func installAgentShell(home string, allow []string) error {
	if err := validateBashAllow(allow); err != nil {
		return fmt.Errorf("install agent shell: %w", err)
	}
	var arms, nestedArms strings.Builder
	hasMise := false
	for _, pattern := range allow {
		prefix, _ := bashPatternPrefix(pattern)
		if strings.Fields(prefix)[0] == "mise" {
			hasMise = true
			continue
		}
		arms.WriteString("  '" + prefix + "'|'" + prefix + " '*) ;;\n")
		nestedArms.WriteString("      '" + prefix + "'|'" + prefix + " '*) ;;\n")
	}
	if hasMise {
		arms.WriteString("  'mise exec -- '*)\n")
		arms.WriteString("    nested=${cmd#mise exec -- }\n")
		arms.WriteString("    case \"$nested\" in\n")
		arms.WriteString(nestedArms.String())
		arms.WriteString("      *) echo \"forest: denied nested shell command: $cmd\" >&2; exit 126 ;;\n")
		arms.WriteString("    esac ;;\n")
	}
	arms.WriteString("  *) echo \"forest: denied shell command: $cmd\" >&2; exit 126 ;;\n")
	script := `#!/bin/sh
cmd=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c|-lc|-ic|-rc)
      shift
      cmd=${1-}
      break ;;
  esac
  shift
done
[ -n "$cmd" ] || { echo 'forest: denied shell invocation without a command' >&2; exit 126; }
safe=$(printf '%s' "$cmd" | LC_ALL=C tr -cd 'A-Za-z0-9._+:/%@=, -')
[ "$safe" = "$cmd" ] || { echo "forest: denied shell metacharacter: $cmd" >&2; exit 126; }
set -f
set -- $cmd
wt=$PWD
for word in "$@"; do
  path=$word
    case "$path" in
      -?..|-?../*|-?/*) path=${path#-?} ;;
    esac
  while :; do
    case "$path" in
      ..|../*|*/..|*/../*)
        echo "forest: denied path outside worktree: $cmd" >&2
        exit 126 ;;
      /*)
        case "$path" in
          "$wt"|"$wt"/*) ;;
          *) echo "forest: denied path outside worktree: $cmd" >&2; exit 126 ;;
        esac ;;
    esac
    case "$path" in
      *=*) path=${path#*=} ;;
      *) break ;;
    esac
  done
done
case "$cmd" in
` + arms.String() + `esac
exec "$@"
`
	path := filepath.Join(home, "bin", "forest-shell")
	if err := os.WriteFile(path, []byte(script), 0o555); err != nil {
		return fmt.Errorf("install agent shell: %w", err)
	}
	return nil
}

func locateRequiredExecutable(name string) (string, error) {
	source, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("locate %s: %w", name, err)
	}
	return validateExecutablePath(name, source)
}

func validateExecutablePath(name, source string) (string, error) {
	if !filepath.IsAbs(source) {
		return "", fmt.Errorf("locate %s: executable path %q is not absolute", name, source)
	}
	source, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", fmt.Errorf("locate %s: resolve executable: %w", name, err)
	}
	info, err := os.Stat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("locate %s: resolved path %q is not a regular executable", name, source)
	}
	return source, nil
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
