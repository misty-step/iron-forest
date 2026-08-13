package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

func main() { os.Exit(runCLI(os.Args[1:])) }

func runCLI(args []string) int {
	if len(args) == 0 {
		printUsage()
		return exitInvalidArg
	}
	// Engine commands act on the current checkout and hold the Kernel lock, so
	// they take no flags. Everything else is the read surface.
	switch args[0] {
	case "serve", "once", "poll":
		return runEngineCommand(args[0], args[1:])
	case "help", "--help", "-h":
		// Asking for usage is not a usage error.
		printUsage()
		return exitOK
	}
	return runSurfaceCommand(args)
}

func runEngineCommand(command string, rest []string) int {
	wantArgs := 0
	if command != "serve" {
		wantArgs = 1
	}
	if len(rest) != wantArgs {
		printUsage()
		return exitInvalidArg
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	switch command {
	case "serve":
		return serve(root)
	case "once":
		return once(root, rest[0])
	default:
		return poll(root, rest[0])
	}
}

func printUsage() {
	var usage strings.Builder
	usage.WriteString(`usage: forest <command> [flags]

engine:
  forest serve                  run the scheduler until interrupted
  forest once <agent>           poll once, dispatch on exit 0
  forest poll <agent>           evaluate the built-in trigger for builder,
                                verifier, or fixer

inspect:`)
	for _, command := range cliCommands() {
		fmt.Fprintf(&usage, "\n  %s", command.usage())
	}
	usage.WriteString(`

flags:
  --json        emit one forest.cli.v2 envelope on stdout
  --root <dir>  read another checkout
  --limit N     bound a listing
  --after <id>  continue a listing after one identity
  --rescan      re-run the Auditor before reporting (audit show)
  --follow      stream a Run log until it completes; excludes --json (run logs)
  exit: 0 ok, 1 no work, 2 error, 4 not found, 5 conflict, 6 invalid arg
        run logs --follow instead exits with the followed Run's own code`)
	fmt.Fprintln(os.Stderr, usage.String())
}

// withLock runs an engine command while holding the Kernel lock. It reports
// through stderr and a process code; the read surface uses withKernelLock, which
// is the same acquisition reported through the envelope.
func withLock(root string, fn func() int) int {
	outcome := withKernelLock(root, func() cliOutcome { return cliOutcome{Exit: fn()} })
	if outcome.ErrText != "" {
		fmt.Fprintln(os.Stderr, outcome.ErrText)
	}
	return outcome.Exit
}

// withKernelLock runs a mutation while holding the Kernel lock, so a Kernel that
// starts mid-command cannot overwrite the result from a stale snapshot. Probing
// the lock and releasing it would prove nothing about the write that follows.
func withKernelLock(root string, fn func() cliOutcome) cliOutcome {
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		return failure(exitError, "%s", err)
	}
	file, err := os.OpenFile(forestPath(root, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return failure(exitError, "%s", err)
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return failure(exitConflict, "a Kernel is running; stop it first")
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return fn()
}

func serve(root string) int {
	return withLock(root, func() int {
		cfg, err := loadConfig(configPath(root))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitError
		}
		scheduler := NewScheduler(root, cfg, NewRunner(root))
		if scheduler.startupErr != nil {
			fmt.Fprintln(os.Stderr, scheduler.startupErr)
			return exitError
		}
		defaults, _, err := loadDefaults(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitError
		}
		for _, name := range agentNames(cfg) {
			if _, err := loadDeclarationWithDefaults(root, name, defaults); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return exitError
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := scheduler.Serve(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, err)
			return exitError
		}
		return exitOK
	})
}

func once(root, agent string) int {
	return withLock(root, func() int {
		cfg, err := loadConfig(configPath(root))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitError
		}
		scheduler := NewScheduler(root, cfg, NewRunner(root))
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		dispatched, err := scheduler.Once(ctx, agent)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitError
		}
		if dispatched {
			return exitOK
		}
		return exitNoWork
	})
}

func poll(root, agent string) int {
	cfg, err := loadConfig(configPath(root))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	poller := NewPoller(root, cfg.Repo)
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, directPollTimeout)
	defer cancel()
	return pollAgent(ctx, poller, agent)
}

func pollAgent(ctx context.Context, poller *Poller, agent string) int {
	var code int
	var err error
	switch agent {
	case "builder":
		code, err = poller.builder(ctx)
	case "verifier":
		code, err = poller.verifier(ctx)
	case "fixer":
		code, err = poller.fixer(ctx)
	default:
		fmt.Fprintf(os.Stderr, "unknown poll agent %q\n", agent)
		return exitError
	}
	if ctx.Err() != nil {
		return exitError
	}
	// A Poll that fails must say why. Silence would leave the operator with an
	// exit code and no cause, and a direct Poll records nothing to inspect.
	if err != nil {
		fmt.Fprintf(os.Stderr, "poll %s: %v\n", agent, err)
	}
	return code
}

type statusPayload struct {
	Repo     string        `json:"repo"`
	Kernel   kernelView    `json:"kernel"`
	Triggers []TriggerView `json:"triggers"`
	// TriggerStateError reports why trigger state is unknown, so a machine
	// reader learns the reason its sibling lock_error already publishes.
	TriggerStateError string      `json:"trigger_state_error,omitempty"`
	Audit             AuditState  `json:"audit"`
	Recent            []RunRecord `json:"recent"`
}

type kernelView struct {
	Running      bool   `json:"running"`
	RunningKnown bool   `json:"running_known"`
	LockError    string `json:"lock_error,omitempty"`
}

// kernelHuman states whether a Kernel holds the workspace lock. Without this the
// human report of a running-but-idle Kernel is identical to a stopped one, while
// the payload says otherwise.
func kernelHuman(kernel kernelView) string {
	if !kernel.RunningKnown {
		return "unknown"
	}
	if kernel.Running {
		return "running"
	}
	return "stopped"
}

// runStatus composes the read surface into one snapshot. It renders from the
// same resolved values it publishes, so the two views cannot diverge.
func runStatus(_ []string, flags cliFlags) cliOutcome {
	cfg, err := loadConfig(configPath(flags.root))
	if err != nil {
		return failure(exitError, "%s", err)
	}
	state, err := resolveTriggerState(flags.root)
	if err != nil {
		return failure(exitError, "%s", err)
	}
	kernel := kernelView{Running: state.LockHeld, RunningKnown: state.LockErr == nil}
	if state.LockErr != nil {
		kernel.LockError = state.LockErr.Error()
	}
	audit, err := readAuditState(flags.root)
	if err != nil {
		return failure(exitError, "%s", err)
	}
	records, err := ReadLedgerTail(flags.root, statusRecentRuns)
	if err != nil {
		return failure(exitError, "%s", err)
	}
	// An unreadable lock or state is a warning, not a failure: the snapshot still
	// reports every other fact, and the payload marks what is unknown. Under
	// --json the envelope carries both reasons, so warning again would publish
	// the same fact twice.
	warnings := make([]string, 0, 2)
	if state.LockErr != nil {
		warnings = append(warnings, fmt.Sprintf("kernel lock state unknown: %v", state.LockErr))
	}
	if reason := stateWarning(state); reason != "" {
		warnings = append(warnings, reason)
	}
	// The repository and the ordering are stated, so a pasted status can be
	// attributed to a factory and its newest Run cannot be mistaken for its
	// oldest. `run list` pages the other way.
	recent := fmt.Sprintf("recent runs (oldest first, at most %d):", statusRecentRuns)
	sections := []string{
		"repo: " + oneLine(cfg.Repo),
		"kernel: " + kernelHuman(kernel),
		triggerViewsHuman(state.Views),
		liveRunsHuman(state),
		auditStateHuman(audit, statusViolations),
		recent,
	}
	if rows := runRecordsHuman(records, "  "); rows != "" {
		sections = append(sections, rows)
	}
	payload := statusPayload{
		Repo:     cfg.Repo,
		Kernel:   kernel,
		Triggers: state.Views,
		Audit:    audit,
		Recent:   records,
	}
	if state.StateErr != nil {
		payload.TriggerStateError = state.StateErr.Error()
	}
	return cliOutcome{
		Exit:    exitOK,
		Data:    payload,
		Human:   strings.Join(sections, "\n"),
		Warning: strings.Join(warnings, "\n"),
	}
}

// statusRecentRuns bounds both views of recent Runs. statusViolations bounds only
// the human view, which reports the remainder as omitted; the payload carries the
// full violation set the Auditor recorded.
const (
	statusRecentRuns = 10
	statusViolations = 10
)

// kernelLockHeld reports whether a Kernel holds the workspace lock. The probe
// takes a shared lock, so concurrent read-only commands never mistake each other
// for a Kernel; a shared lock still conflicts with the Kernel's exclusive one.
// This answers a reporting question only. A mutation must hold the lock through
// withKernelLock rather than act on a released probe.
func kernelLockHeld(root string) (bool, error) {
	file, err := os.OpenFile(forestPath(root, "lock"), os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return true, nil
		}
		return false, err
	}
	return false, syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

type toolPath struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type selfcheckPayload struct {
	Repo         string     `json:"repo"`
	Declarations []string   `json:"declarations"`
	Tools        []toolPath `json:"tools"`
	// Defaults reports the instance layer the declarations resolve against, and
	// DefaultsSource names its file, so a host that supplies no defaults is
	// visibly different from one that supplies empty ones.
	Defaults       Defaults `json:"defaults"`
	DefaultsSource string   `json:"defaults_source,omitempty"`
}

// runSelfcheck validates the configuration, the declarations, and the trusted
// tool paths this checkout would run with. The payload reports the paths it
// resolved, which is the fact the check establishes.
func runSelfcheck(_ []string, flags cliFlags) cliOutcome {
	cfg, err := loadConfig(configPath(flags.root))
	if err != nil {
		return failure(exitError, "%s", err)
	}
	defaults, defaultsSource, err := loadDefaults(flags.root)
	if err != nil {
		return failure(exitError, "%s", err)
	}
	names := agentNames(cfg)
	for _, name := range names {
		if _, err := loadDeclarationWithDefaults(flags.root, name, defaults); err != nil {
			return failure(exitError, "%s", err)
		}
	}
	runner := NewRunner(flags.root)
	tools := []struct {
		name    string
		resolve func() (string, error)
	}{
		{name: "git", resolve: func() (string, error) { return trustedExecutable(flags.root, "git") }},
		{name: "gh", resolve: func() (string, error) { return trustedExecutable(flags.root, "gh") }},
		{name: "pi", resolve: runner.piExecutable},
	}
	resolved := make([]toolPath, 0, len(tools))
	for _, tool := range tools {
		path, err := tool.resolve()
		if err != nil {
			return failure(exitError, "%s unavailable: %s", tool.name, err)
		}
		resolved = append(resolved, toolPath{Name: tool.name, Path: path})
	}
	human := fmt.Sprintf("selfcheck: ok\nrepo: %s\ndeclarations: %s\ntools: %s",
		oneLine(cfg.Repo), strings.Join(names, " "), strings.Join(toolNames(resolved), " "))
	if defaultsSource != "" {
		human += fmt.Sprintf("\ndefaults: %s (model=%s)", oneLine(defaultsSource), oneLine(defaults.Model))
	}
	return cliOutcome{
		Exit: exitOK,
		Data: selfcheckPayload{
			Repo:           cfg.Repo,
			Declarations:   names,
			Tools:          resolved,
			Defaults:       defaults,
			DefaultsSource: defaultsSource,
		},
		Human: human,
	}
}

// toolNames lists resolved tools as name=path, so the human surface reports the
// same paths the payload publishes.
func toolNames(tools []toolPath) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name+"="+tool.Path)
	}
	return names
}
