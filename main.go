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
	switch command {
	case "serve", "status", "selfcheck":
		if len(args) != 1 {
			printUsage()
			return 2
		}
	case "once", "poll":
		if len(args) != 2 {
			printUsage()
			return 2
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		printUsage()
		return 2
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	switch command {
	case "serve":
		return serve(root)
	case "once":
		return once(root, args[1])
	case "poll":
		return poll(root, args[1])
	case "status":
		return status(root)
	default:
		return selfcheck(root)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: forest serve | once <agent> | poll <agent> | status | selfcheck")
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
	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
	defer cancel()
	switch agent {
	case "builder":
		return poller.builder(ctx)
	case "verifier":
		return poller.verifier(ctx)
	case "fixer":
		return poller.fixer(ctx)
	default:
		fmt.Fprintf(os.Stderr, "unknown poll agent %q\n", agent)
		return 2
	}
}

func status(root string) int {
	cfg, err := loadConfig(configPath(root))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Println("triggers:")
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
	if healthErr != nil && !os.IsNotExist(healthErr) {
		fmt.Fprintf(os.Stderr, "trigger state unknown: %v\n", healthErr)
	}
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
		if value.LastError != "" {
			fmt.Printf(" error=%s", value.LastError)
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
	state, stateErr := readAuditState(root)
	if stateErr != nil {
		fmt.Fprintln(os.Stderr, stateErr)
		return 2
	}
	fmt.Printf("last audit: %s master=%s\n", state.LastResult, state.LastMaster)
	for _, violation := range state.Violations {
		fmt.Printf("audit violation: %s\n", violation)
	}
	records, err := ReadLedger(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Println("recent runs:")
	start := len(records) - 10
	if start < 0 {
		start = 0
	}
	for _, record := range records[start:] {
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

func selfcheck(root string) int {
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
