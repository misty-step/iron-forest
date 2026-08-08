package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// failingFlow is a Flow stub whose Act always fails like a crashed agent. Tests
// use it to drive actOnSubject without talking to a real tracker or opencode.
type failingFlow struct{}

func (failingFlow) Name() string                             { return "builder" }
func (failingFlow) Select(Config, string) ([]Subject, error) { return aSubject, nil }
func (failingFlow) Interval(Config) time.Duration            { return 0 }
func (failingFlow) Enabled(Config) bool                      { return true }
func (failingFlow) Act(Config, string, Subject, string) (Outcome, error) {
	return Outcome{TokIn: 7}, errAgentCrash
}

type shutdownFlow struct{ drain *int32 }

func (shutdownFlow) Name() string                             { return "builder" }
func (shutdownFlow) Select(Config, string) ([]Subject, error) { return aSubject, nil }
func (shutdownFlow) Interval(Config) time.Duration            { return 0 }
func (shutdownFlow) Enabled(Config) bool                      { return true }
func (f shutdownFlow) Act(Config, string, Subject, string) (Outcome, error) {
	*f.drain = 1
	return Outcome{TokIn: 7}, errAgentCrash
}

type captureRunIDFlow struct{ got *string }

func (f captureRunIDFlow) Name() string                             { return "capture" }
func (f captureRunIDFlow) Select(Config, string) ([]Subject, error) { return nil, nil }
func (f captureRunIDFlow) Interval(Config) time.Duration            { return 0 }
func (f captureRunIDFlow) Enabled(Config) bool                      { return true }
func (f captureRunIDFlow) Act(_ Config, _ string, _ Subject, runID string) (Outcome, error) {
	*f.got = runID
	return Outcome{Status: "done"}, nil
}

type reloadConfigFlow struct {
	observed chan bool
	release  chan struct{}
	drain    *int32
	passes   int32
}

func (*reloadConfigFlow) Name() string                  { return "reload" }
func (*reloadConfigFlow) Interval(Config) time.Duration { return time.Millisecond }
func (*reloadConfigFlow) Enabled(Config) bool           { return true }
func (f *reloadConfigFlow) Select(cfg Config, _ string) ([]Subject, error) {
	pass := atomic.AddInt32(&f.passes, 1)
	f.observed <- cfg.Flows.Verifier.AutoMerge
	if pass == 1 {
		<-f.release
	} else {
		atomic.StoreInt32(f.drain, 1)
	}
	return nil, nil
}
func (*reloadConfigFlow) Act(Config, string, Subject, string) (Outcome, error) {
	panic("reloadConfigFlow has no Subjects")
}

type gateHoldingFlow struct {
	ready    chan string
	releases map[string]chan struct{}
}

func (gateHoldingFlow) Name() string                             { return "gate-holder" }
func (gateHoldingFlow) Select(Config, string) ([]Subject, error) { return nil, nil }
func (gateHoldingFlow) Interval(Config) time.Duration            { return 0 }
func (gateHoldingFlow) Enabled(Config) bool                      { return true }
func (f gateHoldingFlow) Act(_ Config, _ string, s Subject, _ string) (Outcome, error) {
	f.ready <- s.Key
	<-f.releases[s.Key]
	return Outcome{Status: "done"}, nil
}

var errAgentCrash = errors.New("agent: agent exited \"signal: terminated\"")

// aSubject is the sole subject a failingFlow selects.
var aSubject = []Subject{{Key: "item-1", Revision: "rev-1", ID: "1"}}

func TestRunIDsAreFlatAndUnique(t *testing.T) {
	runID := newRunID()
	if filepath.Base(runID) != runID || strings.Contains(runID, "..") {
		t.Fatalf("run id %q is not one flat path segment", runID)
	}
	if next := newRunID(); next == runID {
		t.Fatalf("two runs received the same id %q", runID)
	}
}

func TestResolveSelectedSubjectRejectsOpaqueIDKeyCollision(t *testing.T) {
	subjects := []Subject{
		{Key: "item-2", ID: "2"},
		{Key: "item-item-2", ID: "item-2"},
	}
	if _, found, err := resolveSelectedSubject(subjects, "item-2"); err == nil || found {
		t.Fatalf("ambiguous opaque Item resolution = (found=%v, err=%v), want refusal", found, err)
	}
	got, found, err := resolveSelectedSubject(subjects, "item-item-2")
	if err != nil || !found || got.ID != "item-2" {
		t.Fatalf("unambiguous Subject key resolution = (%#v, %v, %v)", got, found, err)
	}
}

func TestActOnSubjectUsesGeneratedRunID(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	var got string
	s := Subject{Key: `item-/../../outside\trace?[x]`, Revision: "r1"}
	if code := actOnSubject(captureRunIDFlow{got: &got}, Config{Repo: admissionTestRepo}, repo, s, nil); code != 0 {
		t.Fatalf("actOnSubject code = %d, want 0", code)
	}
	if got == "" || filepath.Base(got) != got || strings.Contains(got, "..") {
		t.Fatalf("Act received unsafe run id %q", got)
	}
	rows, _, err := loadLedger(ledgerPath(repo))
	if err != nil || len(rows) != 1 || rows[0].RunID != got {
		t.Fatalf("Ledger run id = (%#v, %v), want %q", rows, err, got)
	}
}

type admissionProcessFlow struct {
	name    string
	ready   io.Writer
	release io.Reader
}

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
	if err := git(repo, "commit", "-m", "test config"); err != nil {
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
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			<-done
		}
	})
	startDeadline := time.Now().Add(10 * time.Second)
	for {
		if body, err := os.ReadFile(started); err == nil && string(body) == "started" {
			break
		}
		select {
		case err := <-done:
			waited = true
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
		waited = true
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

func (f admissionProcessFlow) Name() string                             { return f.name }
func (f admissionProcessFlow) Select(Config, string) ([]Subject, error) { return nil, nil }
func (f admissionProcessFlow) Interval(Config) time.Duration            { return 0 }
func (f admissionProcessFlow) Enabled(Config) bool                      { return true }
func (f admissionProcessFlow) Act(Config, string, Subject, string) (Outcome, error) {
	if _, err := f.ready.Write([]byte{1}); err != nil {
		return Outcome{}, err
	}
	var release [1]byte
	if _, err := io.ReadFull(f.release, release[:]); err != nil {
		return Outcome{}, err
	}
	return Outcome{Status: "done"}, nil
}

type selectionFlow struct {
	subjects []Subject
	acted    *[]string
}

func (f selectionFlow) Name() string                             { return "builder" }
func (f selectionFlow) Select(Config, string) ([]Subject, error) { return f.subjects, nil }
func (f selectionFlow) Interval(Config) time.Duration            { return 0 }
func (f selectionFlow) Enabled(Config) bool                      { return true }
func (f selectionFlow) Act(_ Config, _ string, s Subject, _ string) (Outcome, error) {
	*f.acted = append(*f.acted, s.Key)
	return Outcome{Status: "done"}, nil
}

func TestActOnSubjectAdmissionHelper(t *testing.T) {
	repo := os.Getenv("FOREST_ACT_ADMISSION_HELPER")
	if repo == "" {
		return
	}
	var subject Subject
	if err := json.Unmarshal([]byte(os.Getenv("FOREST_ACT_SUBJECT")), &subject); err != nil {
		t.Fatal(err)
	}
	ready := os.NewFile(3, "ready")
	release := os.NewFile(4, "release")
	if ready == nil || release == nil {
		t.Fatal("missing synchronization pipe")
	}
	defer ready.Close()
	defer release.Close()
	if code := actOnSubject(admissionProcessFlow{
		name: "builder", ready: ready, release: release,
	}, Config{Repo: admissionTestRepo}, repo, subject, nil); code != 0 {
		t.Fatalf("holder action code = %d, want success", code)
	}
}

// TestActOnSubjectAdmissionClaimsAcrossItemAndBranch selects the two real
// Subject forms, then proves separate processes cannot both enter Act.
func TestActOnSubjectAdmissionClaimsAcrossItemAndBranch(t *testing.T) {
	remote, repoA, _ := notesTestRepository(t)
	repoB := filepath.Join(t.TempDir(), "second-checkout")
	runGitTest(t, "", "clone", remote, repoB)
	const itemID = "hab-50"
	oldTrackerFor := trackerFor
	trackerFor = func(string) Tracker {
		return trackerStub{items: []Item{{
			ID: itemID, Title: "change", UpdatedAt: "item-rev", Tags: []string{"forest:ready"},
		}}}
	}
	defer func() { trackerFor = oldTrackerFor }()

	cfg := defaultConfig()
	cfg.Repo = admissionTestRepo
	cfg.Flows.Builder.RequireLabels = []string{"forest:ready"}
	cfg.Flows.Builder.ExcludeLabels = nil
	items, err := builderFlow{}.Select(cfg, repoA)
	if err != nil || len(items) != 1 {
		t.Fatalf("builder subjects = (%#v, %v), want one item", items, err)
	}

	branchName := BranchPrefix + encodeBranchID(itemID) + "-change"
	runGitTest(t, repoA, "checkout", "-q", "-b", branchName)
	if err := os.WriteFile(filepath.Join(repoA, "change.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repoA, "add", "change.txt")
	runGitTest(t, repoA, "commit", "-qm", "change")
	runGitTest(t, repoA, "push", "-q", "-u", "origin", branchName)
	branches, err := verifierFlow{}.Select(cfg, repoA)
	if err != nil || len(branches) != 1 {
		t.Fatalf("verifier subjects = (%#v, %v), want one branch", branches, err)
	}
	if canonicalAdmissionKey(items[0]) != canonicalAdmissionKey(branches[0]) {
		t.Fatalf("admission keys differ: item=%q branch=%q", canonicalAdmissionKey(items[0]), canonicalAdmissionKey(branches[0]))
	}

	subjectJSON, err := json.Marshal(items[0])
	if err != nil {
		t.Fatal(err)
	}
	ready, childReady, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	childRelease, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	defer release.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestActOnSubjectAdmissionHelper$")
	cmd.Env = append(os.Environ(),
		"FOREST_ACT_ADMISSION_HELPER="+repoA,
		"FOREST_ACT_SUBJECT="+string(subjectJSON),
	)
	cmd.ExtraFiles = []*os.File{childReady, childRelease}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	helperDone, markWaited := startTestProcess(t, cmd)
	_ = childReady.Close()
	_ = childRelease.Close()

	started := make(chan error, 1)
	go func() {
		var one [1]byte
		_, err := io.ReadFull(ready, one[:])
		started <- err
	}()
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("holder readiness: %v: %s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("holder did not enter Act: %s", stderr.String())
	}

	var contenderActed bytes.Buffer
	code := actOnSubject(admissionProcessFlow{
		name: "verifier", ready: &contenderActed, release: bytes.NewReader([]byte{1}),
	}, cfg, repoB, branches[0], nil)
	if code != codeBusy {
		t.Fatalf("contender action code = %d, want %d", code, codeBusy)
	}
	if contenderActed.Len() != 0 {
		t.Fatal("busy contender entered Act")
	}
	if _, err := release.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-helperDone:
		markWaited()
		if err != nil {
			t.Fatalf("holder process: %v: %s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("holder did not finish: %s", stderr.String())
	}

	holderRows, _, err := loadLedger(ledgerPath(repoA))
	if err != nil {
		t.Fatal(err)
	}
	contenderRows, _, err := loadLedger(ledgerPath(repoB))
	if err != nil {
		t.Fatal(err)
	}
	if len(holderRows) != 1 || holderRows[0].Subject != items[0].Key || len(contenderRows) != 0 {
		t.Fatalf("Ledger rows = holder %#v, contender %#v; want one holder outcome for %q", holderRows, contenderRows, items[0].Key)
	}
}

func TestRunFlowPassContinuesAfterBusySubject(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	busy := Subject{Key: "item-21", Kind: "item", ID: "21", Revision: "r1"}
	available := Subject{Key: "item-22", Kind: "item", ID: "22", Revision: "r2"}
	release, err := claimAdmission(repo, admissionTestRepo, "verifier", busy)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	var acted []string
	code, key := runFlowPass(selectionFlow{
		subjects: []Subject{busy, available}, acted: &acted,
	}, Config{Repo: admissionTestRepo}, repo, nil)
	if code != 0 || key != available.Key {
		t.Fatalf("runFlowPass = (%d, %q), want success for %q", code, key, available.Key)
	}
	if len(acted) != 1 || acted[0] != available.Key {
		t.Fatalf("Act subjects = %v, want only %q", acted, available.Key)
	}
	rows, _, err := loadLedger(ledgerPath(repo))
	if err != nil || len(rows) != 1 || rows[0].Subject != available.Key {
		t.Fatalf("Ledger = (%#v, %v), want one %q outcome", rows, err, available.Key)
	}
}

// TestShutdownIsNotAnAgentFailure pins the operator-shutdown card: a run that
// ends while the daemon is draining records a distinct shutdown status, keeps
// the tokens spent, and never increments the repeat-failure brake.
func TestShutdownIsNotAnAgentFailure(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	var drain int32
	for range stalledRunLimit + 1 {
		drain = 0
		if code := actOnSubject(shutdownFlow{drain: &drain}, Config{}, repo, aSubject[0], &drain); code != 1 {
			t.Fatalf("draining failing run code = %d, want 1", code)
		}
	}
	rows, _, err := loadLedger(ledgerPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	last := rows[len(rows)-1]
	if last.Status != shutdownStatus {
		t.Errorf("draining run status = %q, want %q", last.Status, shutdownStatus)
	}
	if last.TokensIn != 7 {
		t.Errorf("draining run tokensIn = %d, want 7 (spend kept)", last.TokensIn)
	}
	stalled, err := stalledOn(repo, "builder", aSubject[0].Key, aSubject[0].Revision)
	if err != nil {
		t.Fatal(err)
	}
	if stalled {
		t.Fatalf("shutdown reached the failure brake; it must not count")
	}
}

func TestActOnSubjectRefusesNewWorkAfterDrain(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	var got string
	var drain int32 = 1
	code := actOnSubject(captureRunIDFlow{got: &got}, Config{Repo: admissionTestRepo}, repo, aSubject[0], &drain)
	if code != 1 || got != "" {
		t.Fatalf("act after drain = (%d, %q), want refusal before Act", code, got)
	}
	rows, _, err := loadLedger(ledgerPath(repo))
	if err != nil || len(rows) != 0 {
		t.Fatalf("Ledger after drained refusal = (%#v, %v), want empty", rows, err)
	}
}

// TestAgentFailureStillCounts pins the other side of the boundary: a real
// non-zero agent exit, with no shutdown in progress, still records agent_failed
// and still drives the repeat-failure brake.
func TestAgentFailureStillCounts(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	for range stalledRunLimit {
		if code := actOnSubject(failingFlow{}, Config{}, repo, aSubject[0], nil); code != 1 {
			t.Fatalf("failing run code = %d, want 1", code)
		}
	}
	rows, _, err := loadLedger(ledgerPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	if rows[len(rows)-1].Status != "agent_failed" {
		t.Errorf("real failure status = %q, want agent_failed", rows[len(rows)-1].Status)
	}
	stalled, err := stalledOn(repo, "builder", aSubject[0].Key, aSubject[0].Revision)
	if err != nil {
		t.Fatal(err)
	}
	if !stalled {
		t.Fatalf("real failures did not reach the brake; they must count")
	}
}

// TestBuilderPromptDeliveryFailureParksNotFixes drives #204's mechanical
// classification end to end. A prompt that cannot be delivered is a mechanical
// failure: the same prompt fails identically on every retry, so it must be
// named prompt_failed for an operator and must never become a Fixer subject
// that spends a repair attempt on an unchanged situation. The builder flow
// fails inside runPhase before it ever publishes a branch, so nothing on
// origin offers the head to the Fixer and the attempt counter stays at zero.
func TestBuilderPromptDeliveryFailureParksNotFixes(t *testing.T) {
	repo := setupTestRepo(t)
	writeAgentFixture(t, repo, "builder", "builder-model")

	tk := newMemoryTracker()
	tk.seed(Item{ID: "9", Title: "wide change", UpdatedAt: "u1"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	oldRun := runPhase
	runPhase = func(_ string, _ string, _ *Agent, userPrompt, tracePath string) (runStats, error) {
		return runStats{}, &promptDeliveryError{size: len(userPrompt), limit: maxArgLen}
	}
	defer func() { runPhase = oldRun }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{}

	it := Item{ID: "9", Title: "wide change", UpdatedAt: "u1"}
	out, err := (builderFlow{}).Act(cfg, repo, Subject{
		Key: "item-9", Kind: "item", Revision: "u1", ID: "9", Item: it,
	}, "run-prompt")
	if err == nil {
		t.Fatalf("a prompt-delivery failure returned no error: %#v", out)
	}
	if !isPromptDelivery(err) {
		t.Fatalf("error %v does not wrap a promptDeliveryError", err)
	}
	if out.Status != "prompt_failed" {
		t.Fatalf("prompt-delivery status = %q, want prompt_failed (mechanical)", out.Status)
	}

	// The mechanical failure must not enter the Fixer. Because the run never
	// published a branch, the Fixer has nothing to repair on origin, and no
	// repair attempt was spent on the subject.
	subjects, err := (fixerFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatalf("fixer Select: %v", err)
	}
	if len(subjects) != 0 {
		t.Fatalf("a prompt-delivery failure was offered to the Fixer: %#v", subjects)
	}
	if n, err := readAttempts(repo, "branch-forest/9-wide-change"); err != nil || n != 0 {
		t.Fatalf("fixer attempts = (%d, %v), want 0; a mechanical failure must not spend a repair attempt", n, err)
	}
}

// TestBuilderTimeoutFailureParksNotFixes drives #207's mechanical classification
// end to end. A run that exceeds its declared wall-clock deadline is a
// mechanical failure: the same run keeps exceeding the same declared bound, so
// it must be named timeout_failed for an operator — never treated as a rejected
// change — and must never become a Fixer subject that spends a repair attempt on
// an unchanged situation. The builder flow fails inside runPhase before it ever
// publishes a branch, so nothing on origin offers the head to the Fixer and the
// attempt counter stays at zero.
func TestBuilderTimeoutFailureParksNotFixes(t *testing.T) {
	repo := setupTestRepo(t)
	writeAgentFixture(t, repo, "builder", "builder-model")

	tk := newMemoryTracker()
	tk.seed(Item{ID: "9", Title: "wide change", UpdatedAt: "u1"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	oldRun := runPhase
	runPhase = func(_ string, _ string, _ *Agent, userPrompt, tracePath string) (runStats, error) {
		return runStats{}, &runTimeoutError{elapsed: 3 * time.Minute, lastEvent: "step_finish"}
	}
	defer func() { runPhase = oldRun }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{}

	it := Item{ID: "9", Title: "wide change", UpdatedAt: "u1"}
	out, err := (builderFlow{}).Act(cfg, repo, Subject{
		Key: "item-9", Kind: "item", Revision: "u1", ID: "9", Item: it,
	}, "run-timeout")
	if err == nil {
		t.Fatalf("a runaway run returned no error: %#v", out)
	}
	if !isRunTimeout(err) {
		t.Fatalf("error %v does not wrap a runTimeoutError", err)
	}
	if out.Status != "timeout_failed" {
		t.Fatalf("timeout status = %q, want timeout_failed (mechanical)", out.Status)
	}
	if !strings.Contains(err.Error(), "3m0s") || !strings.Contains(err.Error(), "step_finish") {
		t.Errorf("error %q did not name the elapsed time and last trace event", err)
	}

	subjects, err := (fixerFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatalf("fixer Select: %v", err)
	}
	if len(subjects) != 0 {
		t.Fatalf("a timeout failure was offered to the Fixer: %#v", subjects)
	}
	if n, err := readAttempts(repo, "branch-forest/9-wide-change"); err != nil || n != 0 {
		t.Fatalf("fixer attempts = (%d, %v), want 0; a timeout must not spend a repair attempt", n, err)
	}
}

func TestRunFlowLoopReloadsConfigBeforeNextPass(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	writeConfig := func(autoMerge bool, message string) {
		t.Helper()
		body := "repo: " + admissionTestRepo + "\n" +
			"checks:\n  - name: test\n    run: \"true\"\n" +
			"flows:\n  verifier:\n    auto_merge: " + strconv.FormatBool(autoMerge) + "\n"
		if err := os.WriteFile(configPath(repo), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitTest(t, repo, "add", "forest.yaml")
		runGitTest(t, repo, "commit", "-m", message)
	}
	writeConfig(false, "initial config")
	cfg, err := loadConfig(configPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	var drain int32
	flow := &reloadConfigFlow{
		observed: make(chan bool, 2),
		release:  make(chan struct{}, 1),
		drain:    &drain,
	}
	done := make(chan struct{})
	go func() {
		runFlowLoop(flow, cfg, repo, &drain, nil)
		close(done)
	}()
	t.Cleanup(func() {
		select {
		case flow.release <- struct{}{}:
		default:
		}
	})
	select {
	case first := <-flow.observed:
		if first {
			t.Fatal("first pass observed auto_merge=true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first Flow pass did not start")
	}
	writeConfig(true, "reload config")
	flow.release <- struct{}{}
	select {
	case second := <-flow.observed:
		if !second {
			t.Fatal("next pass did not observe the committed config edit")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Flow pass did not start")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Flow loop did not stop after the reload proof")
	}
}

func TestUpdateGateWaitsForEveryActiveFlow(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	first := Subject{Key: "item-update-a", Kind: "item", ID: "update-a", Revision: "r1"}
	second := Subject{Key: "item-update-b", Kind: "item", ID: "update-b", Revision: "r1"}
	flow := gateHoldingFlow{
		ready: make(chan string, 2),
		releases: map[string]chan struct{}{
			first.Key:  make(chan struct{}, 1),
			second.Key: make(chan struct{}, 1),
		},
	}
	t.Cleanup(func() {
		for _, release := range flow.releases {
			select {
			case release <- struct{}{}:
			default:
			}
		}
	})
	results := make(chan struct {
		key  string
		code int
	}, 2)
	for _, subject := range []Subject{first, second} {
		go func(s Subject) {
			results <- struct {
				key  string
				code int
			}{s.Key, actOnSubject(flow, Config{Repo: admissionTestRepo}, repo, s, nil)}
		}(subject)
	}
	for range 2 {
		select {
		case <-flow.ready:
		case <-time.After(5 * time.Second):
			t.Fatal("Flow action did not acquire the update read gate")
		}
	}
	updateAcquired := make(chan struct{})
	ticks := make(chan time.Time, 1)
	oldTicker, oldCheck, oldFactoryDir := selfUpdateTicker, selfUpdateCheck, factoryDir
	t.Cleanup(func() {
		selfUpdateTicker, selfUpdateCheck, factoryDir = oldTicker, oldCheck, oldFactoryDir
	})
	selfUpdateTicker = func() (<-chan time.Time, func()) { return ticks, func() {} }
	selfUpdateCheck = func(string, *int32) { close(updateAcquired) }
	factoryDir = repo
	var drain int32
	updateDone := make(chan struct{})
	go func() {
		selfUpdateLoop(repo, &drain, nil)
		close(updateDone)
	}()
	ticks <- time.Now()
	select {
	case <-updateAcquired:
		t.Fatal("source update entered while two Flow actions were active")
	case <-time.After(100 * time.Millisecond):
	}
	flow.releases[first.Key] <- struct{}{}
	select {
	case result := <-results:
		if result.key != first.Key || result.code != 0 {
			t.Fatalf("first released result = %#v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first Flow action did not finish")
	}
	select {
	case <-updateAcquired:
		t.Fatal("source update entered while the second Flow action was active")
	case <-time.After(100 * time.Millisecond):
	}
	flow.releases[second.Key] <- struct{}{}
	select {
	case <-updateAcquired:
	case <-time.After(5 * time.Second):
		t.Fatal("source update did not enter after every Flow action finished")
	}
	select {
	case result := <-results:
		if result.key != second.Key || result.code != 0 {
			t.Fatalf("second released result = %#v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Flow action did not finish")
	}
	close(ticks)
	select {
	case <-updateDone:
	case <-time.After(5 * time.Second):
		t.Fatal("source update loop did not stop")
	}
}
