package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

func runPollCommand(ctx context.Context, command string) PollResult {
	pollCtx, cancel := context.WithTimeout(ctx, pollCommandTimeout)
	defer cancel()
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	result := PollResult{}
	if err := cmd.Start(); err != nil {
		result.Code, result.Err = 2, err
		return result
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err := <-wait:
		return finishPollLeader(pollCtx, cmd.Process.Pid, err)
	case <-pollCtx.Done():
		result.Code = 2
		result.Err = errors.Join(pollCtx.Err(), stopResidualProcessGroup(cmd.Process.Pid, pollStopGrace))
		return result
	}
}

func finishPollLeader(pollCtx context.Context, pid int, waitErr error) PollResult {
	result := PollResult{}
	if err := pollCtx.Err(); err != nil {
		result.Code, result.Err = 2, err
	} else if waitErr == nil {
		result.Code = 0
	} else {
		if exitErr, ok := waitErr.(*exec.ExitError); ok && exitErr.ProcessState != nil {
			result.Code = exitErr.ProcessState.ExitCode()
		} else {
			result.Code = 2
		}
		if result.Code < 0 {
			result.Code = 2
		}
		result.Err = waitErr
	}
	if err := stopResidualProcessGroup(pid, pollStopGrace); err != nil {
		result.Code = 2
		result.Err = errors.Join(result.Err, err)
	}
	return result
}

const (
	directPollTimeout  = 60 * time.Second
	pollCommandTimeout = directPollTimeout + 5*time.Second
	pollStopGrace      = pollCommandTimeout - directPollTimeout
	auditTimeout       = 60 * time.Second
)

type PollResult struct {
	Code int
	Err  error
}

type TriggerHealth struct {
	Agent             string `json:"agent"`
	ConsecutiveErrors int    `json:"consecutive_errors"`
	LastCode          int    `json:"last_code"`
	PollError         string `json:"poll_error,omitempty"`
	RunError          string `json:"run_error,omitempty"`
	AuditError        string `json:"audit_error,omitempty"`
	LastRun           string `json:"last_run,omitempty"`
	Running           bool   `json:"running"`
}

type Scheduler struct {
	Root       string
	Config     Config
	Poll       func(context.Context, string) PollResult
	Run        func(context.Context, Declaration) (RunRecord, error)
	mu         sync.Mutex
	runs       sync.WaitGroup
	health     map[string]TriggerHealth
	inFlight   map[string]bool
	startupErr error
}

func NewScheduler(root string, cfg Config, runner *Runner) *Scheduler {
	s := &Scheduler{Root: root, Config: cfg, health: make(map[string]TriggerHealth), inFlight: make(map[string]bool), Poll: runPollCommand}
	if err := cfg.Validate(); err != nil {
		s.startupErr = fmt.Errorf("invalid config: %w", err)
		return s
	}
	if runner != nil {
		s.Run = runner.Run
		if err := cleanupReservedResidue(root, runner); err != nil {
			s.startupErr = fmt.Errorf("reserved garbage collection: %w", err)
			return s
		}
	}
	values, _, err := readTriggerHealth(root)
	if err != nil {
		s.startupErr = err
		return s
	}
	stale := false
	for name, health := range values {
		if _, configured := cfg.Agents[name]; !configured {
			stale = true
			continue
		}
		if health.Running {
			health.Running = false
			stale = true
		}
		s.health[name] = health
	}
	if stale {
		if err := s.saveHealthLocked(); err != nil {
			s.startupErr = fmt.Errorf("clear stale trigger state: %w", err)
		}
	}
	return s
}

func (s *Scheduler) Tick(ctx context.Context, agent string) (bool, error) {
	declaration, run, claimed, err := s.claimRun(ctx, agent)
	if err != nil || !claimed {
		return false, err
	}
	s.runs.Add(1)
	go func() {
		defer s.runs.Done()
		record, runErr := run(context.Background(), declaration)
		if err := s.completeRun(agent, record, runErr); err != nil {
			fmt.Fprintf(os.Stderr, "scheduler state %s: %v\n", agent, err)
		}
	}()
	return true, nil
}

func (s *Scheduler) Once(ctx context.Context, agent string) (bool, error) {
	declaration, run, claimed, err := s.claimRun(ctx, agent)
	if err != nil || !claimed {
		return false, err
	}
	record, runErr := run(ctx, declaration)
	if err := s.completeRun(agent, record, runErr); err != nil && runErr == nil {
		runErr = err
	}
	return true, runErr
}

func (s *Scheduler) claimRun(ctx context.Context, agent string) (Declaration, func(context.Context, Declaration) (RunRecord, error), bool, error) {
	cfg, ok := s.Config.Agents[agent]
	if !ok {
		return Declaration{}, nil, false, fmt.Errorf("agent %q is not configured", agent)
	}
	s.mu.Lock()
	if s.startupErr != nil {
		err := s.startupErr
		s.mu.Unlock()
		return Declaration{}, nil, false, err
	}
	if s.health[agent].Running || s.inFlight[agent] {
		s.mu.Unlock()
		return Declaration{}, nil, false, nil
	}
	if isProviderBudgetError(s.health[agent].RunError) {
		s.mu.Unlock()
		return Declaration{}, nil, false, nil
	}
	poll := s.Poll
	if poll == nil {
		s.mu.Unlock()
		return Declaration{}, nil, false, fmt.Errorf("poller is not configured")
	}
	s.inFlight[agent] = true
	s.mu.Unlock()
	runStarted := false
	defer func() {
		if !runStarted {
			s.mu.Lock()
			delete(s.inFlight, agent)
			s.mu.Unlock()
		}
	}()

	result := poll(ctx, cfg.Poll)
	s.mu.Lock()
	health := s.health[agent]
	health.Agent = agent
	health.LastCode = result.Code
	health.PollError = ""
	if result.Code > 1 {
		health.ConsecutiveErrors++
		if result.Err != nil {
			health.PollError = result.Err.Error()
		} else {
			health.PollError = fmt.Sprintf("exit %d", result.Code)
		}
	} else {
		health.ConsecutiveErrors = 0
	}
	s.health[agent] = health
	persistErr := s.saveHealthLocked()
	s.mu.Unlock()
	if persistErr != nil {
		return Declaration{}, nil, false, fmt.Errorf("persist trigger state: %w", persistErr)
	}
	if result.Code > 1 {
		return Declaration{}, nil, false, fmt.Errorf("poll %s failed (exit %d): %s", agent, result.Code, health.PollError)
	}
	if result.Code != 0 {
		return Declaration{}, nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return Declaration{}, nil, false, err
	}
	declaration, err := loadDeclaration(s.Root, agent)
	if err != nil {
		return Declaration{}, nil, false, err
	}
	declaration.MaxDuration = cfg.MaxDuration

	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return Declaration{}, nil, false, err
	}
	run := s.Run
	if run == nil {
		s.mu.Unlock()
		return Declaration{}, nil, false, fmt.Errorf("runner is not configured")
	}
	health = s.health[agent]
	health.Agent = agent
	health.Running = true
	s.health[agent] = health
	if err := s.saveHealthLocked(); err != nil {
		health.Running = false
		s.health[agent] = health
		s.mu.Unlock()
		return Declaration{}, nil, false, fmt.Errorf("persist running state: %w", err)
	}
	runStarted = true
	s.mu.Unlock()
	return declaration, run, true, nil
}

func (s *Scheduler) completeRun(agent string, record RunRecord, runErr error) error {
	s.mu.Lock()
	delete(s.inFlight, agent)
	defer s.mu.Unlock()
	auditCtx, cancel := context.WithTimeout(context.Background(), auditTimeout)
	auditMessage := auditSummary(auditCtx, s.Root)
	cancel()
	if auditMessage == "" {
		for name, health := range s.health {
			health.AuditError = ""
			s.health[name] = health
		}
	}
	health := s.health[agent]
	health.Agent = agent
	health.Running = false
	health.LastRun = record.Started
	if record.Error != "" {
		health.RunError = record.Error
	} else if runErr != nil {
		health.RunError = runErr.Error()
	} else {
		health.RunError = ""
	}
	health.AuditError = auditMessage
	s.health[agent] = health
	return s.saveHealthLocked()
}

func (s *Scheduler) Serve(ctx context.Context) error {
	if s.startupErr != nil {
		return s.startupErr
	}
	var group sync.WaitGroup
	for _, agent := range agentNames(s.Config) {
		group.Add(1)
		go func(name string) {
			defer group.Done()
			cfg := s.Config.Agents[name]
			interval, err := durationFromSeconds(cfg.Interval)
			if err != nil {
				fmt.Fprintf(os.Stderr, "serve interval %s: %v\n", name, err)
				return
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				if _, err := s.Tick(ctx, name); err != nil {
					fmt.Fprintf(os.Stderr, "serve tick %s: %v\n", name, err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}(agent)
	}
	group.Wait()
	s.runs.Wait()
	return ctx.Err()
}

func (s *Scheduler) saveHealthLocked() error {
	if s.Root == "" {
		return nil
	}
	return writeTriggerHealth(s.Root, s.health)
}

func auditSummary(ctx context.Context, root string) string {
	_, err := audit(ctx, root)
	if err != nil {
		return err.Error()
	}
	return ""
}
