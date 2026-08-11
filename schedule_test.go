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
	cfg := Config{Repo: "owner/name", Agents: map[string]AgentConfig{
		"builder": {Poll: "poll", Interval: 1, Timeout: 1},
	}}
	scheduler := NewScheduler(root, cfg, nil)
	scheduler.Poll = func(context.Context, string) PollResult { return result }
	runCalled := false
	scheduler.Run = func(context.Context, Declaration, int) (RunRecord, error) {
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
	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("residual child produced no output")
	}
	time.Sleep(100 * time.Millisecond)
	after, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("residual child continued writing after poll returned: before=%d after=%d", len(before), len(after))
	}
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
	case <-time.After(3 * time.Second):
		t.Fatal("canceled poll did not return")
	}
	if result.Code != 2 || !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("canceled poll result=%#v", result)
	}
	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	after, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("poll descendant continued after return: before=%d after=%d", len(before), len(after))
	}
}

func TestPollCommandDiscardsOutput(t *testing.T) {
	result := runPollCommand(context.Background(), "printf noisy >&2; exit 3")
	if result.Code != 3 || result.Err == nil || strings.Contains(result.Err.Error(), "noisy") {
		t.Fatalf("noisy poll result=%#v", result)
	}
}

func TestConfigRejectsDurationOverflow(t *testing.T) {
	overflow := int(^uint(0) >> 1)
	if int64(overflow) <= maxDurationSeconds {
		t.Skip("int range cannot overflow time.Duration seconds")
	}
	cfg := Config{Repo: "owner/name", Agents: map[string]AgentConfig{
		"builder": {Poll: "poll", Interval: overflow, Timeout: 1},
	}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("interval overflow error=%v", err)
	}
	cfg.Agents["builder"] = AgentConfig{Poll: "poll", Interval: 1, Timeout: overflow}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("timeout overflow error=%v", err)
	}
}

func TestSchedulerServeDrainsInFlightRun(t *testing.T) {
	root := t.TempDir()
	writeTestDeclaration(t, root, "builder")
	cfg := Config{Repo: "owner/name", Agents: map[string]AgentConfig{"builder": {Poll: "poll", Interval: 1, Timeout: 1}}}
	scheduler := NewScheduler(root, cfg, nil)
	scheduler.Poll = func(context.Context, string) PollResult { return PollResult{Code: 0} }
	started := make(chan struct{})
	release := make(chan struct{})
	scheduler.Run = func(context.Context, Declaration, int) (RunRecord, error) {
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
			cfg := Config{Repo: "owner/name", Agents: map[string]AgentConfig{
				"builder": {Poll: "poll", Interval: 1, Timeout: 1},
			}}
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
			scheduler.Run = func(context.Context, Declaration, int) (RunRecord, error) {
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

func TestStatusReportsKernelLockTruth(t *testing.T) {
	cases := []struct {
		name       string
		lock       string
		want       []string
		forbid     []string
		wantStderr string
	}{
		{
			name:   "held lock",
			lock:   "held",
			want:   []string{"builder errors=0 code=0 running=true", "live runs:\n  agent=builder running=true"},
			forbid: []string{"stale=true", "running=unknown", "live runs: none"},
		},
		{
			name:   "free lock",
			lock:   "free",
			want:   []string{"builder errors=0 code=0 running=false stale=true", "live runs: none"},
			forbid: []string{"running=true", "running=unknown", "live runs:\n"},
		},
		{
			name:       "lock lookup error",
			lock:       "error",
			want:       []string{"builder errors=0 code=0 running=unknown", "live runs: unknown"},
			forbid:     []string{"running=true", "running=false", "stale=true"},
			wantStderr: "kernel lock state unknown:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
				t.Fatal(err)
			}
			writeCLIConfig(t, root, "poll")
			triggers := []byte(`{"builder":{"agent":"builder","running":true}}`)
			if err := os.WriteFile(forestPath(root, "triggers.json"), triggers, 0o644); err != nil {
				t.Fatal(err)
			}
			lockPath := forestPath(root, "lock")
			if tc.lock == "error" {
				if err := os.Mkdir(lockPath, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				if tc.lock == "held" {
					if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
						t.Fatal(err)
					}
					defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
				}
			}

			code, stdout, stderr := captureCLIOutput(t, func() int { return status(root) })
			if code != 0 {
				t.Fatalf("status code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(stdout, want) {
					t.Fatalf("status stdout missing %q: %s", want, stdout)
				}
			}
			for _, forbid := range tc.forbid {
				if strings.Contains(stdout, forbid) {
					t.Fatalf("status stdout contains %q: %s", forbid, stdout)
				}
			}
			if tc.wantStderr == "" {
				if stderr != "" {
					t.Fatalf("status stderr=%q, want empty", stderr)
				}
			} else if !strings.Contains(stderr, tc.wantStderr) {
				t.Fatalf("status stderr=%q, want substring %q", stderr, tc.wantStderr)
			}
		})
	}
}

func TestSelfcheckRejectsRepositoryToolPath(t *testing.T) {
	root := t.TempDir()
	writeTestDeclaration(t, root, "builder")
	writeCLIConfig(t, root, "poll")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git", "gh", "omp"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", bin); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })
	if code := selfcheck(root); code == 0 {
		t.Fatal("selfcheck accepted repository tool path")
	}
}

func quoteShellPath(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}
