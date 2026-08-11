package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunCLIRequiresExactArityBeforeSideEffects(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "missing command"},
		{name: "serve extra", args: []string{"serve", "extra"}},
		{name: "once missing agent", args: []string{"once"}},
		{name: "once extra", args: []string{"once", "builder", "extra"}},
		{name: "poll missing agent", args: []string{"poll"}},
		{name: "poll extra", args: []string{"poll", "builder", "extra"}},
		{name: "status extra", args: []string{"status", "extra"}},
		{name: "selfcheck extra", args: []string{"selfcheck", "extra"}},
		{name: "unknown command", args: []string{"stats"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			code, _, stderr := captureCLIOutput(t, func() int { return runCLI(tc.args) })
			if code != 2 {
				t.Fatalf("runCLI(%q) code=%d, want 2", tc.args, code)
			}
			if !strings.Contains(stderr, "usage: forest") {
				t.Fatalf("runCLI(%q) stderr=%q, want usage", tc.args, stderr)
			}
			if _, err := os.Stat(filepath.Join(root, workspaceName)); !os.IsNotExist(err) {
				t.Fatalf("runCLI(%q) created workspace before rejecting usage: %v", tc.args, err)
			}
		})
	}
}

func TestRunCLIBoundedExitSemantics(t *testing.T) {
	cases := []struct {
		name string
		args []string
		poll string
		want int
	}{
		{name: "status success", args: []string{"status"}, poll: "exit 1", want: 0},
		{name: "once healthy skip", args: []string{"once", "builder"}, poll: "exit 1", want: 1},
		{name: "once operational failure", args: []string{"once", "builder"}, poll: "exit 2", want: 2},
		{name: "poll operational failure", args: []string{"poll", "unknown"}, poll: "exit 1", want: 2},
		{name: "status operational failure", args: []string{"status"}, want: 2},
		{name: "selfcheck operational failure", args: []string{"selfcheck"}, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.poll != "" {
				writeCLIConfig(t, root, tc.poll)
			}
			t.Chdir(root)
			code, _, _ := captureCLIOutput(t, func() int { return runCLI(tc.args) })
			if code != tc.want {
				t.Fatalf("runCLI(%q) code=%d, want %d", tc.args, code, tc.want)
			}
		})
	}
}

func TestOnceSignalStopsPollDescendantAndReleasesLock(t *testing.T) {
	if os.Getenv("FOREST_ONCE_SIGNAL_HELPER") == "1" {
		os.Exit(runCLI([]string{"once", "builder"}))
	}
	poll := `/bin/sh -c 'trap "" HUP INT TERM; while :; do printf x >> "$HEARTBEAT"; sleep 0.02; done' </dev/null >/dev/null 2>&1 & child=$!; while [ ! -s "$HEARTBEAT" ]; do sleep 0.01; done; printf '%s\n' "$child" > "$CHILD_PID"; wait`
	for _, tc := range []struct {
		name   string
		signal syscall.Signal
	}{
		{name: "SIGINT", signal: syscall.SIGINT},
		{name: "SIGTERM", signal: syscall.SIGTERM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeCLIConfig(t, root, poll)
			state, heartbeat := processHeartbeatFixture(t)
			command := exec.Command(os.Args[0], "-test.run=^TestOnceSignalStopsPollDescendantAndReleasesLock$")
			command.Dir = root
			command.Env = append(os.Environ(), "FOREST_ONCE_SIGNAL_HELPER=1")
			var stderr strings.Builder
			command.Stderr = &stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			wait := make(chan error, 1)
			go func() { wait <- command.Wait() }()
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = command.Process.Kill()
					<-wait
				}
			})
			waitForCLIFile(t, filepath.Join(state, "child-pid"))
			waitForCLIFile(t, heartbeat)
			if err := command.Process.Signal(tc.signal); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-wait:
				waited = true
				exitErr, ok := err.(*exec.ExitError)
				if !ok || exitErr.ExitCode() != 2 {
					t.Fatalf("forest once after %s: err=%v stderr=%s, want exit 2", tc.name, err, stderr.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("forest once did not finish cleanup after %s", tc.name)
			}
			assertProcessQuiescent(t, heartbeat, "detached Poll descendant", tc.name)

			lock, err := os.OpenFile(forestPath(root, "lock"), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatalf("Kernel lock remained held after %s cleanup: %v", tc.name, err)
			}
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		})
	}
}

func waitForCLIFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := os.Stat(path)
		if err == nil && info.Size() > 0 {
			return
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStatusTreatsInvalidTriggerStateAsUnknown(t *testing.T) {
	cases := []struct {
		name       string
		state      string
		wantStderr bool
	}{
		{name: "missing"},
		{name: "missing entry", state: `{}`, wantStderr: true},
		{name: "malformed", state: "{", wantStderr: true},
		{name: "null document", state: "null", wantStderr: true},
		{name: "null entry", state: `{"builder":null}`, wantStderr: true},
		{name: "missing identity", state: `{"builder":{}}`, wantStderr: true},
		{name: "empty identity", state: `{"builder":{"agent":""}}`, wantStderr: true},
		{name: "mismatched identity", state: `{"builder":{"agent":"fixer"}}`, wantStderr: true},
		{name: "mismatched agents", state: `{"fixer":{"agent":"fixer"}}`, wantStderr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeCLIConfig(t, root, "exit 1")
			if tc.state != "" {
				if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(forestPath(root, "triggers.json"), []byte(tc.state), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			code, stdout, stderr := captureCLIOutput(t, func() int { return status(root) })
			if code != 0 {
				t.Fatalf("status code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			for _, want := range []string{"builder state=unknown", "live runs: unknown"} {
				if !strings.Contains(stdout, want) {
					t.Fatalf("status stdout missing %q: %s", want, stdout)
				}
			}
			for _, forbidden := range []string{"builder errors=", "live runs: none"} {
				if strings.Contains(stdout, forbidden) {
					t.Fatalf("status stdout contains %q: %s", forbidden, stdout)
				}
			}
			if tc.wantStderr != strings.Contains(stderr, "trigger state unknown:") {
				t.Fatalf("status stderr=%q, trigger-state error wanted=%t", stderr, tc.wantStderr)
			}
		})
	}
}

func writeCLIConfig(t *testing.T, root, poll string) {
	t.Helper()
	config := "repo: owner/name\nagents:\n  builder:\n    poll: " + poll + "\n    interval: 1\n    timeout: 1\n"
	if err := os.WriteFile(configPath(root), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
}

func captureCLIOutput(t *testing.T, run func() int) (int, string, string) {
	t.Helper()
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		t.Fatal(err)
	}
	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdoutWriter, stderrWriter
	code := run()
	os.Stdout, os.Stderr = oldStdout, oldStderr
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	stdout, stdoutErr := io.ReadAll(stdoutReader)
	stderr, stderrErr := io.ReadAll(stderrReader)
	_ = stdoutReader.Close()
	_ = stderrReader.Close()
	if stdoutErr != nil {
		t.Fatal(stdoutErr)
	}
	if stderrErr != nil {
		t.Fatal(stderrErr)
	}
	return code, string(stdout), string(stderr)
}
