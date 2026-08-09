package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type hardStopProcessFlow struct {
	heartbeat    string
	pidFile      string
	worktreeFile string
}

func (f hardStopProcessFlow) Name() string                  { return "hard-stop" }
func (f hardStopProcessFlow) Interval(Config) time.Duration { return time.Hour }
func (f hardStopProcessFlow) Enabled(Config) bool           { return true }
func (f hardStopProcessFlow) Select(Config, string) ([]Subject, error) {
	return []Subject{{Key: "hard-stop-subject", Revision: "rev-1"}}, nil
}
func (f hardStopProcessFlow) Act(_ Config, repoDir string, _ Subject, _ string) (Outcome, error) {
	wtDir, _, _, err := createWorktree(repoDir, workspaceDir(repoDir), "hard-stop", "controlled-hang")
	if err != nil {
		return Outcome{}, err
	}
	if err := os.WriteFile(f.worktreeFile, []byte(wtDir), 0o644); err != nil {
		return Outcome{}, err
	}
	script := "(while :; do printf x >> '" + f.heartbeat + "'; sleep 0.05; done) & " +
		"child=$!; printf '%s' \"$child\" > '" + f.pidFile + "'; wait"
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = wtDir
	_, err = runCombinedOutput(cmd)
	return Outcome{Status: "done"}, err
}

type idleServeFlow struct{ ready string }

func (idleServeFlow) Name() string                  { return "idle" }
func (idleServeFlow) Interval(Config) time.Duration { return time.Hour }
func (idleServeFlow) Enabled(Config) bool           { return true }
func (f idleServeFlow) Select(Config, string) ([]Subject, error) {
	if f.ready != "" {
		if err := os.WriteFile(f.ready, []byte("ready"), 0o644); err != nil {
			return nil, err
		}
	}
	return nil, nil
}
func (idleServeFlow) Act(Config, string, Subject, string) (Outcome, error) {
	return Outcome{}, errors.New("idle Flow acted")
}

type startupCleanupFlow struct {
	repo    string
	checked chan error
}

func (startupCleanupFlow) Name() string                  { return "startup-cleanup" }
func (startupCleanupFlow) Interval(Config) time.Duration { return time.Hour }
func (startupCleanupFlow) Enabled(Config) bool           { return true }
func (f startupCleanupFlow) Select(Config, string) ([]Subject, error) {
	for _, name := range []string{"forest.prev", "forest.next", "forest.next.tmp"} {
		if _, err := os.Stat(filepath.Join(f.repo, name)); !os.IsNotExist(err) {
			f.checked <- fmt.Errorf("startup retained %s: %v", name, err)
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
			return nil, nil
		}
	}
	f.checked <- nil
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	return nil, nil
}
func (startupCleanupFlow) Act(Config, string, Subject, string) (Outcome, error) {
	return Outcome{}, errors.New("startup cleanup Flow acted")
}

func TestServeStartupRemovesInterruptedUpdateArtifacts(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	body := "repo: " + admissionTestRepo + "\nchecks:\n  - name: test\n    run: \"true\"\n"
	if err := os.WriteFile(configPath(repo), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "forest.yaml")
	runGitTest(t, repo, "commit", "-m", "test config")
	runGitTest(t, repo, "push", "-u", "origin", "master")
	for _, name := range []string{"forest.prev", "forest.next", "forest.next.tmp"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	checked := make(chan error, 1)
	if code := serveSelected(Config{Repo: admissionTestRepo}, repo, nil,
		[]Flow{startupCleanupFlow{repo: repo, checked: checked}}); code != 0 {
		t.Fatalf("serveSelected = %d, want 0", code)
	}
	if err := <-checked; err != nil {
		t.Fatal(err)
	}
}

func TestServeSecondSignalHelper(t *testing.T) {
	repo := os.Getenv("FOREST_SERVE_HARD_STOP_REPO")
	if repo == "" {
		return
	}
	if ready := os.Getenv("FOREST_SERVE_IDLE_READY"); ready != "" {
		if code := serveSelected(Config{Repo: admissionTestRepo}, repo, nil, []Flow{idleServeFlow{ready: ready}}); code != 0 {
			t.Fatalf("idle serve returned %d", code)
		}
		return
	}
	if started := os.Getenv("FOREST_SERVE_UPDATE_STARTED"); started != "" {
		ticks := make(chan time.Time, 1)
		ticks <- time.Now()
		selfUpdateTicker = func() (<-chan time.Time, func()) { return ticks, func() {} }
		selfUpdateCheck = func(_ string, drain *int32) {
			if err := os.WriteFile(started, []byte("started"), 0o644); err != nil {
				return
			}
			for atomic.LoadInt32(drain) == 0 {
				time.Sleep(time.Millisecond)
			}
			time.Sleep(5 * time.Second)
			_ = os.WriteFile(os.Getenv("FOREST_SERVE_UPDATE_FINISHED"), []byte("finished"), 0o644)
		}
		factoryDir = repo
		t.Fatalf("serve returned %d", serveSelected(Config{Repo: admissionTestRepo}, repo, nil, []Flow{idleServeFlow{}}))
	}
	body := "repo: " + admissionTestRepo + "\nchecks:\n  - name: test\n    run: \"true\"\n"
	if err := os.WriteFile(configPath(repo), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := git(repo, "add", "forest.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := gitAsIdentity(repo, testCommitIdentity(), "commit", "-m", "test config"); err != nil {
		t.Fatal(err)
	}
	if err := git(repo, "push", "-u", "origin", "master"); err != nil {
		t.Fatal(err)
	}
	staleDir, _, _, err := createWorktree(repo, workspaceDir(repo), "startup-stale", "startup-stale")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(staleDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("FOREST_SERVE_STALE_WORKTREE"), []byte(staleDir), 0o644); err != nil {
		t.Fatal(err)
	}
	flow := hardStopProcessFlow{
		heartbeat:    os.Getenv("FOREST_SERVE_HARD_STOP_HEARTBEAT"),
		pidFile:      os.Getenv("FOREST_SERVE_HARD_STOP_PID"),
		worktreeFile: os.Getenv("FOREST_SERVE_HARD_STOP_WORKTREE"),
	}
	t.Fatalf("serve returned %d", serveSelected(Config{Repo: admissionTestRepo}, repo, nil, []Flow{flow}))
}

func TestServeFirstSignalWakesIdleFlows(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	body := "repo: " + admissionTestRepo + "\nchecks:\n  - name: test\n    run: \"true\"\n"
	if err := os.WriteFile(configPath(repo), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "forest.yaml")
	runGitTest(t, repo, "commit", "-m", "test config")
	runGitTest(t, repo, "push", "-u", "origin", "master")

	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestServeSecondSignalHelper$")
	cmd.Env = append(os.Environ(),
		"FOREST_SERVE_HARD_STOP_REPO="+repo,
		"FOREST_SERVE_IDLE_READY="+ready,
	)
	done, markWaited := startTestProcess(t, cmd)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle serve did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		markWaited()
		if err != nil {
			t.Fatalf("idle serve graceful stop = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle serve ignored first signal during its interval")
	}
}

func TestServeFirstSignalWaitsForActiveUpdater(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	body := "repo: " + admissionTestRepo + "\nchecks:\n  - name: test\n    run: \"true\"\n"
	if err := os.WriteFile(configPath(repo), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "forest.yaml")
	runGitTest(t, repo, "commit", "-m", "test config")
	runGitTest(t, repo, "push", "-u", "origin", "master")
	oldTicker, oldCheck, oldFactory := selfUpdateTicker, selfUpdateCheck, factoryDir
	t.Cleanup(func() {
		selfUpdateTicker, selfUpdateCheck, factoryDir = oldTicker, oldCheck, oldFactory
	})
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	started := make(chan struct{})
	release := make(chan struct{})
	selfUpdateTicker = func() (<-chan time.Time, func()) { return ticks, func() {} }
	selfUpdateCheck = func(string, *int32) {
		close(started)
		<-release
	}
	factoryDir = repo
	done := make(chan int, 1)
	go func() {
		done <- serveSelected(Config{Repo: admissionTestRepo}, repo, nil, []Flow{idleServeFlow{}})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("updater did not start")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		t.Fatalf("serve exited with %d before its active updater returned", code)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve after updater release = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not exit after its active updater returned")
	}
}

func TestServeSecondSignalKillsRunDescendants(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	state := t.TempDir()
	heartbeat := filepath.Join(state, "heartbeat")
	pidFile := filepath.Join(state, "pid")
	worktreeFile := filepath.Join(state, "worktree")
	staleWorktreeFile := filepath.Join(state, "stale-worktree")
	cmd := exec.Command(os.Args[0], "-test.run=^TestServeSecondSignalHelper$")
	cmd.Env = append(os.Environ(),
		"FOREST_SERVE_HARD_STOP_REPO="+repo,
		"FOREST_SERVE_HARD_STOP_HEARTBEAT="+heartbeat,
		"FOREST_SERVE_HARD_STOP_PID="+pidFile,
		"FOREST_SERVE_HARD_STOP_WORKTREE="+worktreeFile,
		"FOREST_SERVE_STALE_WORKTREE="+staleWorktreeFile,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderrPath := filepath.Join(state, "stderr")
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	defer stderrFile.Close()
	cmd.Stderr = stderrFile
	cmd.Stdout = stderrFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		if body, err := os.ReadFile(pidFile); err == nil {
			if pid, parseErr := strconv.Atoi(string(body)); parseErr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		heartbeatBody, heartbeatErr := os.ReadFile(heartbeat)
		worktreeBody, worktreeErr := os.ReadFile(worktreeFile)
		if heartbeatErr == nil && len(heartbeatBody) > 0 && worktreeErr == nil && len(worktreeBody) > 0 {
			break
		}
		if time.Now().After(deadline) {
			diagnostics, _ := os.ReadFile(stderrPath)
			t.Fatalf("serve run did not start: %s", diagnostics)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	drainDeadline := time.Now().Add(5 * time.Second)
	for {
		diagnostics, _ := os.ReadFile(stderrPath)
		if strings.Contains(string(diagnostics), "forest: draining") {
			break
		}
		if time.Now().After(drainDeadline) {
			t.Fatalf("serve did not enter drain: %s", diagnostics)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		waited = true
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			diagnostics, _ := os.ReadFile(stderrPath)
			t.Fatalf("serve hard stop = %v, want exit 1: %s", err, diagnostics)
		}
	case <-time.After(10 * time.Second):
		diagnostics, _ := os.ReadFile(stderrPath)
		t.Fatalf("serve ignored second signal: %s", diagnostics)
	}

	time.Sleep(100 * time.Millisecond)
	before, err := os.ReadFile(heartbeat)
	if err != nil || len(before) == 0 {
		t.Fatalf("run descendant produced no heartbeat: %d bytes, %v", len(before), err)
	}
	time.Sleep(300 * time.Millisecond)
	after, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("run descendant survived serve hard stop: heartbeat grew from %d to %d bytes", len(before), len(after))
	}
	pidBody, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidBody))
	if err != nil {
		t.Fatal(err)
	}
	processDeadline := time.Now().Add(5 * time.Second)
	for {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if err != nil {
			t.Fatalf("probe run descendant %d: %v", pid, err)
		}
		if time.Now().After(processDeadline) {
			t.Fatalf("run descendant %d still exists after serve hard stop", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	worktreeBody, err := os.ReadFile(worktreeFile)
	if err != nil {
		t.Fatal(err)
	}
	wtDir := string(worktreeBody)
	reapOrphanWorktrees(repo)
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("restart reap left worktree %s: %v", wtDir, err)
	}
	for _, registered := range currentWorktrees(t, repo) {
		if filepath.Clean(registered) == filepath.Clean(wtDir) {
			t.Fatalf("restart reap left worktree %s registered", wtDir)
		}
	}
	staleBody, err := os.ReadFile(staleWorktreeFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, registered := range currentWorktrees(t, repo) {
		if filepath.Clean(registered) == filepath.Clean(string(staleBody)) {
			t.Fatalf("serve startup left missing worktree %s registered", staleBody)
		}
	}
}

func TestServeSecondSignalDoesNotWaitForUpdater(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	body := "repo: " + admissionTestRepo + "\nchecks:\n  - name: test\n    run: \"true\"\n"
	if err := os.WriteFile(configPath(repo), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "forest.yaml")
	runGitTest(t, repo, "commit", "-m", "test config")
	runGitTest(t, repo, "push", "-u", "origin", "master")

	state := t.TempDir()
	started := filepath.Join(state, "update-started")
	finished := filepath.Join(state, "update-finished")
	diagnosticsPath := filepath.Join(state, "diagnostics")
	diagnostics, err := os.Create(diagnosticsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostics.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestServeSecondSignalHelper$")
	cmd.Env = append(os.Environ(),
		"FOREST_SERVE_HARD_STOP_REPO="+repo,
		"FOREST_SERVE_UPDATE_STARTED="+started,
		"FOREST_SERVE_UPDATE_FINISHED="+finished,
	)
	cmd.Stdout, cmd.Stderr = diagnostics, diagnostics
	done, markWaited := startTestProcess(t, cmd)
	startDeadline := time.Now().Add(10 * time.Second)
	for {
		if body, err := os.ReadFile(started); err == nil && string(body) == "started" {
			break
		}
		select {
		case err := <-done:
			markWaited()
			log, _ := os.ReadFile(diagnosticsPath)
			t.Fatalf("updater serve exited before start: %v: %s", err, log)
		default:
		}
		if time.Now().After(startDeadline) {
			log, _ := os.ReadFile(diagnosticsPath)
			t.Fatalf("updater did not start: %s", log)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	drainDeadline := time.Now().Add(5 * time.Second)
	for {
		log, _ := os.ReadFile(diagnosticsPath)
		if strings.Contains(string(log), "forest: draining") {
			break
		}
		if time.Now().After(drainDeadline) {
			t.Fatalf("updater serve did not drain: %s", log)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		markWaited()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			log, _ := os.ReadFile(diagnosticsPath)
			t.Fatalf("updater hard stop = %v, want exit 1: %s", err, log)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("updater serve ignored second signal")
	}
	if _, err := os.Stat(finished); !os.IsNotExist(err) {
		t.Fatalf("hard stop waited for active updater: %v", err)
	}
}
