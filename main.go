package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() { os.Exit(runCLI(os.Args[1:])) }

func runCLI(args []string) int {
	if len(args) == 0 {
		printUsage()
		return 2
	}
	command := args[0]
	rest := args[1:]
	switch command {
	case "config", "declaration", "trigger", "run", "audit":
		return runObjectCommand(args)
	case "serve":
		if len(rest) != 0 {
			printUsage()
			return 2
		}
		root, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return serve(root)
	case "once", "poll":
		if len(rest) != 1 {
			printUsage()
			return 2
		}
		root, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if command == "once" {
			return once(root, rest[0])
		}
		return poll(root, rest[0])
	case "status", "selfcheck":
		// flag-parsed below
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		printUsage()
		return 2
	}
	positional, flags, err := parseCLIFlags(rest)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitInvalidArg
	}
	if len(positional) != 0 {
		printUsage()
		return 2
	}
	root := flags.root
	if root == "" {
		var getErr error
		root, getErr = os.Getwd()
		if getErr != nil {
			fmt.Fprintln(os.Stderr, getErr)
			return 2
		}
	}
	if command == "status" {
		return status(root, flags)
	}
	return selfcheck(root, flags)
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `usage: forest <command> [flags]

engine:
  serve                 run the scheduler until interrupted
  once <agent>          poll once, dispatch on exit 0
  poll <agent>          evaluate one declaration's trigger

inspect:
  status [--json] [--root <dir>]          composition snapshot
  config show [--json] [--root <dir>]
  declaration list|show <name> [--json] [--root <dir>]
  trigger list|show <agent>|reset <agent> [--json] [--root <dir>]
  run list|show <run-id> [--json] [--root <dir>] [--limit N] [--after <id>]
  run logs [--follow] <run-id> [--root <dir>]
  audit show [--rescan]|log [--json] [--root <dir>] [--limit N]
  selfcheck [--root <dir>]

flags:
  --json        emit one forest.cli.v1 envelope on stdout
  --root <dir>  inspect another checkout
  exit: 0 ok · 1 no work · 2 error · 4 not found · 5 conflict · 6 invalid arg`)
}

func withLock(root string, fn func() int) int {
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	file, err := os.OpenFile(forestPath(root, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintln(os.Stderr, "another Kernel is already running")
		return 2
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return fn()
}

func serve(root string) int {
	return withLock(root, func() int {
		cfg, err := loadConfig(configPath(root))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		for _, name := range agentNames(cfg) {
			if _, err := loadDeclaration(root, name); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		scheduler := NewScheduler(root, cfg, NewRunner(root))
		if err := scheduler.Serve(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return 0
	})
}

func once(root, agent string) int {
	return withLock(root, func() int {
		cfg, err := loadConfig(configPath(root))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		scheduler := NewScheduler(root, cfg, NewRunner(root))
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		dispatched, err := scheduler.Once(ctx, agent)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if dispatched {
			return 0
		}
		return 1
	})
}

func poll(root, agent string) int {
	cfg, err := loadConfig(configPath(root))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
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
		return 2
	}
	if ctx.Err() != nil {
		return 2
	}
	return code
}

func status(root string, flags cliFlags) int {
	cfg, err := loadConfig(configPath(root))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	lockHeld, lockErr := kernelLockHeld(root)
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "kernel lock state unknown: %v\n", lockErr)
	}
	names := agentNames(cfg)
	health, healthErr := readTriggerHealth(root)
	known := healthErr == nil && len(health) == len(names)
	for _, name := range names {
		_, present := health[name]
		known = known && present
	}
	if healthErr == nil && !known {
		healthErr = errors.New("trigger state does not match configured agents")
	}
	if healthErr != nil && !os.IsNotExist(healthErr) && !flags.json {
		fmt.Fprintf(os.Stderr, "trigger state unknown: %v\n", healthErr)
	}

	// Build the JSON view of every fact the human view prints, so --json never
	// diverges from the human scope.
	type triggerView struct {
		Name              string `json:"name"`
		StateKnown        bool   `json:"state_known"`
		ConsecutiveErrors int    `json:"consecutive_errors"`
		LastCode          int    `json:"last_code"`
		Running           bool   `json:"running"`
		Stale             bool   `json:"stale"`
		PollError         string `json:"poll_error,omitempty"`
		RunError          string `json:"run_error,omitempty"`
		AuditError        string `json:"audit_error,omitempty"`
	}
	triggers := make([]triggerView, 0, len(names))
	for _, name := range names {
		value, present := health[name]
		view := triggerView{Name: name, StateKnown: known && present}
		if known && present {
			view.ConsecutiveErrors = value.ConsecutiveErrors
			view.LastCode = value.LastCode
			view.Running = lockHeld && value.Running
			view.Stale = value.Running && lockErr == nil && !lockHeld
			view.PollError = value.PollError
			view.RunError = value.RunError
			view.AuditError = value.AuditError
		}
		triggers = append(triggers, view)
	}

	state, stateErr := readAuditState(root)
	if stateErr != nil {
		fmt.Fprintln(os.Stderr, stateErr)
		return 2
	}
	records, recordsErr := ReadLedgerTail(root, 10)
	if recordsErr != nil {
		fmt.Fprintln(os.Stderr, recordsErr)
		return 2
	}

	if flags.json {
		emitEnvelope(os.Stdout, "status", 0, map[string]any{
			"repo":     cfg.Repo,
			"kernels":  map[string]any{"running": lockHeld, "stale_unknown": lockErr != nil},
			"triggers": triggers,
			"audit":    map[string]any{"last_result": state.LastResult, "last_master": state.LastMaster, "last_at": state.LastAt, "violations": state.Violations},
			"recent":   records,
		}, "")
		return 0
	}

	fmt.Println("triggers:")
	for _, name := range names {
		value, present := health[name]
		if !known || !present {
			fmt.Printf("  %s state=unknown\n", name)
			continue
		}
		fmt.Printf("  %s errors=%d code=%d running=", name, value.ConsecutiveErrors, value.LastCode)
		if lockErr != nil {
			fmt.Print("unknown")
		} else {
			fmt.Printf("%t", lockHeld && value.Running)
		}
		if value.Running && lockErr == nil && !lockHeld {
			fmt.Print(" stale=true")
		}
		if value.PollError != "" {
			fmt.Printf(" poll_error=%s", value.PollError)
		}
		if value.RunError != "" {
			fmt.Printf(" run_error=%s", value.RunError)
		}
		if value.AuditError != "" {
			fmt.Printf(" audit_error=%s", value.AuditError)
		}
		fmt.Println()
	}
	if !known || lockErr != nil {
		fmt.Println("live runs: unknown")
	} else {
		anyLive := false
		for _, value := range health {
			anyLive = anyLive || lockHeld && value.Running
		}
		if !anyLive {
			fmt.Println("live runs: none")
		} else {
			fmt.Println("live runs:")
			for agent, value := range health {
				if lockHeld && value.Running {
					fmt.Printf("  agent=%s running=true\n", agent)
				}
			}
		}
	}
	fmt.Printf("last audit: %s master=%s\n", state.LastResult, state.LastMaster)
	shown := min(len(state.Violations), 10)
	fmt.Printf("audit violations: total=%d", len(state.Violations))
	if omitted := len(state.Violations) - shown; omitted > 0 {
		fmt.Printf(" omitted=%d", omitted)
	}
	fmt.Println()
	for _, violation := range state.Violations[:shown] {
		fmt.Printf("audit violation: %s\n", violation)
	}
	fmt.Println("recent runs:")
	for _, record := range records {
		fmt.Printf("  %s agent=%s exit=%d duration=%.3f\n", record.RunID, record.Agent, record.Exit, record.Duration)
	}
	return 0
}

func kernelLockHeld(root string) (bool, error) {
	file, err := os.OpenFile(forestPath(root, "lock"), os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return true, nil
		}
		return false, err
	}
	return false, syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func readTriggerHealth(root string) (map[string]TriggerHealth, error) {
	data, err := os.ReadFile(forestPath(root, "triggers.json"))
	if err != nil {
		return nil, err
	}
	var decoded map[string]*TriggerHealth
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("parse trigger state: %w", err)
	}
	if decoded == nil {
		return nil, fmt.Errorf("parse trigger state: expected object")
	}
	values := make(map[string]TriggerHealth, len(decoded))
	for agent, health := range decoded {
		if health == nil {
			return nil, fmt.Errorf("parse trigger state: entry %q is null", agent)
		}
		if health.Agent != agent {
			return nil, fmt.Errorf("parse trigger state: entry %q has agent %q", agent, health.Agent)
		}
		values[agent] = *health
	}
	return values, nil
}

func selfcheck(root string, _ cliFlags) int {
	cfg, err := loadConfig(configPath(root))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, name := range agentNames(cfg) {
		if _, err := loadDeclaration(root, name); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	runner := NewRunner(root)
	tools := []struct {
		name    string
		resolve func() (string, error)
	}{
		{name: "git", resolve: func() (string, error) { return trustedExecutable(root, "git") }},
		{name: "gh", resolve: func() (string, error) { return trustedExecutable(root, "gh") }},
		{name: "omp", resolve: runner.ompExecutable},
	}
	for _, tool := range tools {
		if _, err := tool.resolve(); err != nil {
			fmt.Fprintf(os.Stderr, "%s unavailable: %v\n", tool.name, err)
			return 1
		}
	}
	fmt.Println("selfcheck: ok")
	return 0
}
