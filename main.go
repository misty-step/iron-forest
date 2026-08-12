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
  forest poll <agent>           evaluate one declaration's trigger

inspect:`)
	for _, command := range cliCommands() {
		fmt.Fprintf(&usage, "\n  %s", command.usage())
	}
	usage.WriteString(`

flags:
  --json        emit one forest.cli.v1 envelope on stdout
  --root <dir>  read another checkout
  --limit N     bound a listing
  --after <id>  continue a listing after one identity
  exit: 0 ok · 1 no work · 2 error · 4 not found · 5 conflict · 6 invalid arg`)
	fmt.Fprintln(os.Stderr, usage.String())
}

func withLock(root string, fn func() int) int {
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	file, err := os.OpenFile(forestPath(root, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintln(os.Stderr, "another Kernel is already running")
		return exitConflict
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return fn()
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
		for _, name := range agentNames(cfg) {
			if _, err := loadDeclaration(root, name); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return exitError
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		scheduler := NewScheduler(root, cfg, NewRunner(root))
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
	switch agent {
	case "builder":
		code = poller.builder(ctx)
	case "verifier":
		code = poller.verifier(ctx)
	case "fixer":
		code = poller.fixer(ctx)
	default:
		fmt.Fprintf(os.Stderr, "unknown poll agent %q\n", agent)
		return exitError
	}
	if ctx.Err() != nil {
		return exitError
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
	// Unreadable lock or state is a warning, not a failure: the snapshot still
	// reports every other fact, and the payload marks what is unknown.
	if state.LockErr != nil {
		fmt.Fprintf(os.Stderr, "kernel lock state unknown: %v\n", state.LockErr)
	}
	if state.StateErr != nil {
		fmt.Fprintf(os.Stderr, "trigger state unknown: %v\n", state.StateErr)
	}
	sections := []string{
		triggerViewsHuman(state.Views),
		liveRunsHuman(state.Views),
		auditStateHuman(audit, statusViolations),
		"recent runs:",
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
	return cliOutcome{Exit: exitOK, Data: payload, Human: strings.Join(sections, "\n")}
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
}

// runSelfcheck validates the configuration, the declarations, and the trusted
// tool paths this checkout would run with. The payload reports the paths it
// resolved, which is the fact the check establishes.
func runSelfcheck(_ []string, flags cliFlags) cliOutcome {
	cfg, err := loadConfig(configPath(flags.root))
	if err != nil {
		return failure(exitError, "%s", err)
	}
	names := agentNames(cfg)
	for _, name := range names {
		if _, err := loadDeclaration(flags.root, name); err != nil {
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
		{name: "omp", resolve: runner.ompExecutable},
	}
	resolved := make([]toolPath, 0, len(tools))
	for _, tool := range tools {
		path, err := tool.resolve()
		if err != nil {
			return failure(exitError, "%s unavailable: %s", tool.name, err)
		}
		resolved = append(resolved, toolPath{Name: tool.name, Path: path})
	}
	return cliOutcome{
		Exit:  exitOK,
		Data:  selfcheckPayload{Repo: cfg.Repo, Declarations: names, Tools: resolved},
		Human: "selfcheck: ok",
	}
}
