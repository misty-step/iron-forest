package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestPollCommandExitSemantics(t *testing.T) {
	if result := runPollCommand(context.Background(), "exit 0"); result.Code != 0 {
		t.Fatalf("exit 0 result=%#v", result)
	}
	if result := runPollCommand(context.Background(), "printf no-work; exit 1"); result.Code != 1 {
		t.Fatalf("exit 1 result=%#v", result)
	}
	if result := runPollCommand(context.Background(), "exit 3"); result.Code != 3 {
		t.Fatalf("exit 3 result=%#v", result)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result := runPollCommand(ctx, "sleep 1"); result.Code != 2 {
		t.Fatalf("cancel result=%#v", result)
	}
}

func TestPollLeaderDeadlineWinsCompletedShellAndPreventsDispatch(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		t.Fatalf("successful shell wait: %v", waitErr)
	}
	pollCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	<-pollCtx.Done()

	result := finishPollLeader(pollCtx, cmd.Process.Pid, waitErr)
	if result.Code != 2 || !errors.Is(result.Err, context.DeadlineExceeded) {
		t.Fatalf("completed shell after Poll deadline result=%#v", result)
	}

	root := t.TempDir()
	writeTestDeclaration(t, root, "builder")
	cfg := Config{
		Repo:   "owner/name",
		Agents: map[string]AgentConfig{"builder": {Poll: "poll", Interval: 1}},
		Checks: []Check{{Name: "test", Run: "true"}},
	}
	scheduler := NewScheduler(root, cfg, nil)
	scheduler.Poll = func(context.Context, string) PollResult { return result }
	runCalled := false
	scheduler.Run = func(context.Context, Declaration) (RunRecord, error) {
		runCalled = true
		return RunRecord{}, nil
	}
	dispatched, err := scheduler.Once(context.Background(), "builder")
	if dispatched || err == nil {
		t.Fatalf("timed-out successful Poll: dispatched=%v err=%v", dispatched, err)
	}
	if runCalled {
		t.Fatal("Run dispatched from a timed-out successful Poll")
	}
}

func TestPollCommandDrainsResidualProcessGroup(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "output")
	ready := filepath.Join(root, "ready")
	outputPath, readyPath := quoteShellPath(output), quoteShellPath(ready)
	command := "(printf x >> " + outputPath + "; touch " + readyPath + "; while :; do printf x >> " + outputPath + "; sleep 0.01; done) & while [ ! -f " + readyPath + " ]; do sleep 0.01; done; exit 0"

	result := runPollCommand(context.Background(), command)
	if result.Code != 0 || result.Err != nil {
		t.Fatalf("residual child poll result=%#v", result)
	}
	assertProcessQuiescent(t, output, "residual Poll child", "Poll return")
}

func TestPollCommandCancellationKillsDescendantsBeforeReturn(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "output")
	ready := filepath.Join(root, "ready")
	outputPath, readyPath := quoteShellPath(output), quoteShellPath(ready)
	command := "(trap '' TERM; printf x >> " + outputPath + "; touch " + readyPath + "; i=0; while [ \"$i\" -lt 500 ]; do printf x >> " + outputPath + "; i=$((i + 1)); sleep 0.01; done) & wait"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan PollResult, 1)
	go func() { done <- runPollCommand(ctx, command) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("TERM-ignoring poll child did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	var result PollResult
	select {
	case result = <-done:
	case <-time.After(pollStopGrace + time.Second):
		t.Fatal("canceled poll did not return")
	}
	if result.Code != 2 || !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("canceled poll result=%#v", result)
	}
	assertProcessQuiescent(t, output, "Poll descendant", "cancellation")
}

func TestPollCommandDiscardsOutput(t *testing.T) {
	result := runPollCommand(context.Background(), "printf noisy >&2; exit 3")
	if result.Code != 3 || result.Err == nil || strings.Contains(result.Err.Error(), "noisy") {
		t.Fatalf("noisy poll result=%#v", result)
	}
}

func TestSchedulerServeDrainsInFlightRun(t *testing.T) {
	root := t.TempDir()
	writeTestDeclaration(t, root, "builder")
	cfg := Config{Repo: "owner/name", Agents: map[string]AgentConfig{"builder": {Poll: "poll", Interval: 1}}, Checks: []Check{{Name: "test", Run: "true"}}}
	scheduler := NewScheduler(root, cfg, nil)
	scheduler.Poll = func(context.Context, string) PollResult { return PollResult{Code: 0} }
	started := make(chan struct{})
	release := make(chan struct{})
	scheduler.Run = func(context.Context, Declaration) (RunRecord, error) {
		close(started)
		<-release
		return RunRecord{Started: "drained"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.Serve(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not start run")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Serve returned before run drained: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after run drained")
	}
}

func TestSchedulerSerializesConcurrentDispatches(t *testing.T) {
	cases := []struct {
		name   string
		first  string
		second string
	}{
		{name: "Tick-Tick", first: "tick", second: "tick"},
		{name: "Once-Once", first: "once", second: "once"},
		{name: "Tick-Once", first: "tick", second: "once"},
	}
	type dispatchResult struct {
		dispatched bool
		err        error
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeTestDeclaration(t, root, "builder")
			cfg := Config{
				Repo:   "owner/name",
				Agents: map[string]AgentConfig{"builder": {Poll: "poll", Interval: 1}},
				Checks: []Check{{Name: "test", Run: "true"}},
			}
			scheduler := NewScheduler(root, cfg, nil)
			var pollCalls atomic.Int32
			pollEntered := make(chan struct{}, 1)
			pollRelease := make(chan struct{})
			releasePoll := sync.OnceFunc(func() { close(pollRelease) })
			defer releasePoll()
			scheduler.Poll = func(context.Context, string) PollResult {
				pollCalls.Add(1)
				pollEntered <- struct{}{}
				<-pollRelease
				return PollResult{Code: 0}
			}
			var runCalls atomic.Int32
			runEntered := make(chan struct{}, 1)
			runRelease := make(chan struct{})
			releaseRun := sync.OnceFunc(func() { close(runRelease) })
			defer releaseRun()
			scheduler.Run = func(context.Context, Declaration) (RunRecord, error) {
				runCalls.Add(1)
				runEntered <- struct{}{}
				<-runRelease
				return RunRecord{Started: "now"}, nil
			}
			results := make(chan dispatchResult, 2)
			invoke := func(mode string) {
				var dispatched bool
				var err error
				if mode == "tick" {
					dispatched, err = scheduler.Tick(context.Background(), "builder")
				} else {
					dispatched, err = scheduler.Once(context.Background(), "builder")
				}
				results <- dispatchResult{dispatched: dispatched, err: err}
			}

			go invoke(tc.first)
			select {
			case <-pollEntered:
			case <-time.After(2 * time.Second):
				t.Fatal("first call did not enter Poll")
			}
			go invoke(tc.second)
			select {
			case result := <-results:
				if result.err != nil || result.dispatched {
					t.Fatalf("second call: dispatched=%v err=%v", result.dispatched, result.err)
				}
			case <-pollEntered:
				t.Fatal("second call entered Poll")
			case <-time.After(2 * time.Second):
				t.Fatal("second call did not skip the in-flight transition")
			}
			if got := pollCalls.Load(); got != 1 {
				t.Fatalf("Poll calls=%d, want 1", got)
			}

			releasePoll()
			select {
			case <-runEntered:
			case <-time.After(2 * time.Second):
				t.Fatal("first call did not start a Run")
			}
			if got := pollCalls.Load(); got != 1 {
				t.Fatalf("Poll calls=%d, want 1", got)
			}
			if got := runCalls.Load(); got != 1 {
				t.Fatalf("Run calls=%d, want 1", got)
			}
			releaseRun()
			select {
			case result := <-results:
				if result.err != nil || !result.dispatched {
					t.Fatalf("first call: dispatched=%v err=%v", result.dispatched, result.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("first call did not return")
			}
			scheduler.runs.Wait()
			if got := pollCalls.Load(); got != 1 {
				t.Fatalf("Poll calls=%d, want 1", got)
			}
			if got := runCalls.Load(); got != 1 {
				t.Fatalf("Run calls=%d, want 1", got)
			}
		})
	}
}

func schedulerHealth(scheduler *Scheduler, agent string) TriggerHealth {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	return scheduler.health[agent]
}

func TestSchedulerRestartDropsRemovedDeclarationHealth(t *testing.T) {
	root := t.TempDir()
	cfg := Config{
		Repo: "owner/name",
		Agents: map[string]AgentConfig{
			"builder": {Poll: "poll", Interval: 1},
			"fixer":   {Poll: "poll", Interval: 1},
		},
		Checks: []Check{{Name: "test", Run: "true"}},
	}
	scheduler := NewScheduler(root, cfg, nil)
	scheduler.mu.Lock()
	scheduler.health["builder"] = TriggerHealth{Agent: "builder"}
	scheduler.health["fixer"] = TriggerHealth{Agent: "fixer", PollError: "old failure"}
	saveErr := scheduler.saveHealthLocked()
	scheduler.mu.Unlock()
	if saveErr != nil {
		t.Fatal(saveErr)
	}

	restarted := NewScheduler(root, Config{
		Repo:   cfg.Repo,
		Agents: map[string]AgentConfig{"builder": cfg.Agents["builder"]},
		Checks: cfg.Checks,
	}, nil)
	if restarted.startupErr != nil {
		t.Fatal(restarted.startupErr)
	}
	if _, present := restarted.health["fixer"]; present {
		t.Fatalf("removed declaration remains in Scheduler health: %#v", restarted.health)
	}
	persisted, _, err := readTriggerHealth(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted["builder"].Agent != "builder" {
		t.Fatalf("persisted health after declaration removal=%#v", persisted)
	}
}

func TestSchedulerSkipWhileRunningAndUnhealthy(t *testing.T) {
	root := t.TempDir()
	writeTestDeclaration(t, root, "builder")
	cfg := Config{Repo: "owner/name", Agents: map[string]AgentConfig{"builder": {Poll: "poll", Interval: 1}}, Checks: []Check{{Name: "test", Run: "true"}}}
	scheduler := NewScheduler(root, cfg, nil)
	var release = make(chan struct{})
	var once sync.Once
	scheduler.Poll = func(context.Context, string) PollResult { return PollResult{Code: 0} }
	scheduler.Run = func(context.Context, Declaration) (RunRecord, error) {
		once.Do(func() { <-release })
		return RunRecord{Started: "now"}, nil
	}
	dispatched, err := scheduler.Tick(context.Background(), "builder")
	if err != nil || !dispatched {
		t.Fatalf("first tick: dispatched=%v err=%v", dispatched, err)
	}
	deadline := time.Now().Add(time.Second)
	for !schedulerHealth(scheduler, "builder").Running && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	dispatched, err = scheduler.Tick(context.Background(), "builder")
	if err != nil || dispatched {
		t.Fatalf("busy tick: dispatched=%v err=%v", dispatched, err)
	}
	close(release)
	deadline = time.Now().Add(time.Second)
	for schedulerHealth(scheduler, "builder").Running && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	pollErr := errors.New("forge down")
	scheduler.Poll = func(context.Context, string) PollResult { return PollResult{Code: 2, Err: pollErr} }
	for range 2 {
		dispatched, err = scheduler.Tick(context.Background(), "builder")
		if dispatched || err == nil || !strings.Contains(err.Error(), pollErr.Error()) {
			t.Fatalf("failed Poll dispatched=%v err=%v", dispatched, err)
		}
	}
	health := schedulerHealth(scheduler, "builder")
	if health.ConsecutiveErrors != 2 || health.LastCode != 2 || health.PollError != pollErr.Error() {
		t.Fatalf("unhealthy Poll state=%#v", health)
	}
}

func TestSchedulerOnceRunsBeforeReturn(t *testing.T) {
	root := t.TempDir()
	writeTestDeclaration(t, root, "builder")
	cfg := Config{Repo: "owner/name", Agents: map[string]AgentConfig{"builder": {Poll: "poll", Interval: 1}}, Checks: []Check{{Name: "test", Run: "true"}}}
	scheduler := NewScheduler(root, cfg, nil)
	called := false
	scheduler.Poll = func(context.Context, string) PollResult { return PollResult{Code: 0} }
	scheduler.Run = func(context.Context, Declaration) (RunRecord, error) {
		called = true
		return RunRecord{Started: "now"}, nil
	}
	dispatched, err := scheduler.Once(context.Background(), "builder")
	if err != nil || !dispatched || !called {
		t.Fatalf("once dispatched=%v called=%v err=%v", dispatched, called, err)
	}
}

func TestSchedulerPersistsAndClearsCauseSpecificErrors(t *testing.T) {
	root, origin := testClone(t)
	writeTestDeclaration(t, root, "builder")
	if _, err := audit(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "remote", "remove", "origin")
	cfg := Config{
		Repo:   "owner/name",
		Agents: map[string]AgentConfig{"builder": {Poll: "poll", Interval: 1}},
		Checks: []Check{{Name: "test", Run: "true"}},
	}
	scheduler := NewScheduler(root, cfg, nil)
	scheduler.Poll = func(context.Context, string) PollResult { return PollResult{Code: 0} }
	runFailure := errors.New("run failed")
	scheduler.Run = func(context.Context, Declaration) (RunRecord, error) {
		return RunRecord{Started: "failed"}, runFailure
	}
	dispatched, err := scheduler.Once(context.Background(), "builder")
	if !dispatched || !errors.Is(err, runFailure) {
		t.Fatalf("failed Run dispatched=%v err=%v", dispatched, err)
	}
	health := schedulerHealth(scheduler, "builder")
	if health.PollError != "" || health.RunError != runFailure.Error() || health.AuditError == "" {
		t.Fatalf("Run and Audit failure state=%#v", health)
	}
	auditFailure := health.AuditError

	pollFailure := errors.New("poll failed")
	scheduler.Poll = func(context.Context, string) PollResult {
		return PollResult{Code: 2, Err: pollFailure}
	}
	dispatched, err = scheduler.Once(context.Background(), "builder")
	if dispatched || err == nil {
		t.Fatalf("failed Poll dispatched=%v err=%v", dispatched, err)
	}
	health = schedulerHealth(scheduler, "builder")
	if health.PollError != pollFailure.Error() || health.RunError != runFailure.Error() || health.AuditError != auditFailure {
		t.Fatalf("Poll failure replaced another cause=%#v", health)
	}

	scheduler.Poll = func(context.Context, string) PollResult { return PollResult{Code: 1} }
	dispatched, err = scheduler.Once(context.Background(), "builder")
	if dispatched || err != nil {
		t.Fatalf("healthy Poll skip dispatched=%v err=%v", dispatched, err)
	}
	health = schedulerHealth(scheduler, "builder")
	if health.PollError != "" || health.RunError != runFailure.Error() || health.AuditError != auditFailure {
		t.Fatalf("healthy Poll cleared another cause=%#v", health)
	}
	persisted, _, err := readTriggerHealth(root)
	if err != nil || persisted["builder"] != health {
		t.Fatalf("persisted trigger state=%#v err=%v, want %#v", persisted["builder"], err, health)
	}
	auditState, err := readAuditState(root)
	if err != nil || auditState.LastResult != "pass" {
		t.Fatalf("last successful audit state=%#v err=%v", auditState, err)
	}

	runGitDir(t, root, "remote", "add", "origin", origin)
	if err := os.WriteFile(filepath.Join(root, "ungated"), []byte("advance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "ungated")
	runGitDir(t, root, "commit", "-m", "ungated")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/master")
	scheduler.Poll = func(context.Context, string) PollResult { return PollResult{Code: 0} }
	dispatched, err = scheduler.Once(context.Background(), "builder")
	if !dispatched || !errors.Is(err, runFailure) {
		t.Fatalf("failed Run with successful Audit dispatched=%v err=%v", dispatched, err)
	}
	health = schedulerHealth(scheduler, "builder")
	if health.PollError != "" || health.RunError != runFailure.Error() || health.AuditError != "" {
		t.Fatalf("successful Audit cleared a non-Audit cause=%#v", health)
	}
	auditState, err = readAuditState(root)
	if err != nil || auditState.LastResult != "violations" || len(auditState.Violations) == 0 {
		t.Fatalf("successful Audit policy state=%#v err=%v", auditState, err)
	}

	runGitDir(t, root, "remote", "remove", "origin")
	scheduler.Run = func(context.Context, Declaration) (RunRecord, error) {
		return RunRecord{Started: "passed"}, nil
	}
	dispatched, err = scheduler.Once(context.Background(), "builder")
	if !dispatched || err != nil {
		t.Fatalf("successful Run with failed Audit dispatched=%v err=%v", dispatched, err)
	}
	health = schedulerHealth(scheduler, "builder")
	if health.PollError != "" || health.RunError != "" || health.AuditError == "" {
		t.Fatalf("successful Run cleared a non-Run cause=%#v", health)
	}

	runGitDir(t, root, "remote", "add", "origin", origin)
	dispatched, err = scheduler.Once(context.Background(), "builder")
	if !dispatched || err != nil {
		t.Fatalf("successful Run dispatched=%v err=%v", dispatched, err)
	}
	health = schedulerHealth(scheduler, "builder")
	if health.PollError != "" || health.RunError != "" || health.AuditError != "" {
		t.Fatalf("successful Poll, Run, and Audit state=%#v", health)
	}
}

func TestNewSchedulerRunsReservedGarbageCollectionBeforeHealth(t *testing.T) {
	root, _ := testClone(t)
	if err := os.MkdirAll(forestPath(root), 0o755); err != nil {
		t.Fatal(err)
	}
	staleTemp := forestPath(root, ".audit.json-dead")
	if err := os.WriteFile(staleTemp, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	health := `{"builder":{"agent":"builder","poll_error":"preserved health"}}`
	if err := os.WriteFile(forestPath(root, "triggers.json"), []byte(health), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Repo: "owner/name", Agents: map[string]AgentConfig{"builder": {Poll: "poll", Interval: 1}}, Checks: []Check{{Name: "test", Run: "true"}}}

	scheduler := NewScheduler(root, cfg, NewRunner(root))
	if scheduler.startupErr != nil {
		t.Fatalf("NewScheduler() startup error=%v", scheduler.startupErr)
	}
	if _, err := os.Stat(staleTemp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reserved temp survived Scheduler startup: %v", err)
	}
	if got := schedulerHealth(scheduler, "builder").PollError; got != "preserved health" {
		t.Fatalf("loaded Poll health=%q, want preserved health", got)
	}
}

func TestNewSchedulerCleanupFailureBlocksBeforeHealthLoad(t *testing.T) {
	root, _ := testClone(t)
	if err := os.MkdirAll(forestPath(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forestPath(root, "triggers.json"), []byte("{malformed"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.GitPath = filepath.Join(t.TempDir(), "missing-git")
	cfg := Config{Repo: "owner/name", Agents: map[string]AgentConfig{"builder": {Poll: "poll", Interval: 1}}, Checks: []Check{{Name: "test", Run: "true"}}}

	scheduler := NewScheduler(root, cfg, runner)
	if scheduler.startupErr == nil || !strings.Contains(scheduler.startupErr.Error(), "reserved garbage collection") {
		t.Fatalf("NewScheduler() startup error=%v, want reserved garbage collection failure", scheduler.startupErr)
	}
	if strings.Contains(scheduler.startupErr.Error(), "invalid character") {
		t.Fatalf("health loaded before failed reserved garbage collection: %v", scheduler.startupErr)
	}
	pollCalled := false
	scheduler.Poll = func(context.Context, string) PollResult {
		pollCalled = true
		return PollResult{Code: 0}
	}
	dispatched, err := scheduler.Once(context.Background(), "builder")
	if dispatched || err == nil || pollCalled {
		t.Fatalf("failed startup dispatched=%t pollCalled=%t err=%v", dispatched, pollCalled, err)
	}
}

func TestNewSchedulerWithoutRunnerSkipsReservedGarbageCollection(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(forestPath(root), 0o755); err != nil {
		t.Fatal(err)
	}
	staleTemp := forestPath(root, "triggers.json.dead.tmp")
	if err := os.WriteFile(staleTemp, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Repo: "owner/name", Agents: map[string]AgentConfig{"builder": {Poll: "poll", Interval: 1}}, Checks: []Check{{Name: "test", Run: "true"}}}

	scheduler := NewScheduler(root, cfg, nil)
	if scheduler.startupErr != nil {
		t.Fatalf("NewScheduler() startup error=%v", scheduler.startupErr)
	}
	if _, err := os.Stat(staleTemp); err != nil {
		t.Fatalf("nil-Runner Scheduler removed reserved temp: %v", err)
	}
}
