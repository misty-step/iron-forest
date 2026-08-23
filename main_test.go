package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
			if code != exitInvalidArg {
				t.Fatalf("runCLI(%q) code=%d, want %d", tc.args, code, exitInvalidArg)
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
		{name: "selfcheck operational failure", args: []string{"selfcheck"}, want: exitError},
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

func TestPollAgentCancellationWinsFinalResult(t *testing.T) {
	for _, agent := range []string{"builder", "verifier", "fixer"} {
		t.Run(agent, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			poller := &Poller{
				Root: t.TempDir(),
				Repo: "owner/name",
				Run: func(_ context.Context, tool string, _ ...string) ([]byte, error) {
					cancel()
					if tool == "gh" {
						return []byte(`[[]]`), nil
					}
					return nil, nil
				},
			}
			if code := pollAgent(ctx, poller, agent); code != 2 {
				t.Fatalf("pollAgent after cancellation code=%d, want 2", code)
			}
		})
	}
}

func TestCLISignalsStopPollDescendants(t *testing.T) {
	if command := os.Getenv("FOREST_CLI_SIGNAL_HELPER"); command != "" {
		os.Exit(runCLI([]string{command, "builder"}))
	}
	signals := []struct {
		name   string
		signal syscall.Signal
	}{
		{name: "SIGINT", signal: syscall.SIGINT},
		{name: "SIGTERM", signal: syscall.SIGTERM},
	}
	for _, cliCommand := range []string{"once", "poll"} {
		for _, tc := range signals {
			t.Run(cliCommand+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				if cliCommand == "once" {
					poll := `/bin/sh -c 'trap "" HUP INT TERM; while :; do printf x >> "$HEARTBEAT"; sleep 0.02; done' </dev/null >/dev/null 2>&1 & child=$!; while [ ! -s "$HEARTBEAT" ]; do sleep 0.01; done; printf '%s\n' "$child" > "$CHILD_PID"; wait`
					writeCLIConfig(t, root, poll)
				} else {
					writeCLIConfig(t, root, "exit 1")
					bin := t.TempDir()
					gh := "#!/bin/sh\ntrap '' HUP INT TERM\nprintf '%s\\n' \"$$\" > \"$CHILD_PID\"\nwhile :; do printf x >> \"$HEARTBEAT\"; /bin/sleep 0.02; done\n"
					if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755); err != nil {
						t.Fatal(err)
					}
					t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
				}
				state, heartbeat := processHeartbeatFixture(t)
				command := exec.Command(os.Args[0], "-test.run=^TestCLISignalsStopPollDescendants$")
				command.Dir = root
				command.Env = append(os.Environ(), "FOREST_CLI_SIGNAL_HELPER="+cliCommand)
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
						t.Fatalf("forest %s after %s: err=%v stderr=%s, want exit 2", cliCommand, tc.name, err, stderr.String())
					}
				case <-time.After(pollStopGrace + time.Second):
					t.Fatalf("forest %s did not finish cleanup after %s", cliCommand, tc.name)
				}
				assertProcessQuiescent(t, heartbeat, "Poll descendant", tc.name)
				if cliCommand != "once" {
					return
				}
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
}

// Corrupt trigger state is a warning and renders as unknown. A configured agent
// with no row yet is NOT corrupt: an empty object means the Kernel has recorded
// nothing, which is what a missing file also means, so both stay silent.
func TestStatusTreatsInvalidTriggerStateAsUnknown(t *testing.T) {
	cases := []struct {
		name       string
		state      string
		wantStderr bool
	}{
		{name: "missing"},
		{name: "no rows recorded yet", state: `{}`},
		{name: "malformed", state: "{", wantStderr: true},
		{name: "null document", state: "null", wantStderr: true},
		{name: "null entry", state: `{"builder":null}`, wantStderr: true},
		{name: "missing identity", state: `{"builder":{}}`, wantStderr: true},
		{name: "empty identity", state: `{"builder":{"agent":""}}`, wantStderr: true},
		{name: "mismatched identity", state: `{"builder":{"agent":"fixer"}}`, wantStderr: true},
		{name: "state for an agent the config dropped", state: `{"fixer":{"agent":"fixer"}}`, wantStderr: true},
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
			code, stdout, stderr := captureCLIOutput(t, func() int { return runCLI([]string{"status", "--root", root}) })
			if code != 0 {
				t.Fatalf("status code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			// Liveness is unknown only when the state file cannot be read. A file
			// that simply records nothing proves no Run is live.
			wantLive := "live runs: none"
			if tc.wantStderr {
				wantLive = "live runs: unknown"
			}
			for _, want := range []string{"builder state=unknown", wantLive} {
				if !strings.Contains(stdout, want) {
					t.Fatalf("status stdout missing %q: %s", want, stdout)
				}
			}
			if strings.Contains(stdout, "builder errors=") {
				t.Fatalf("status stdout reported state it does not have: %s", stdout)
			}
			if tc.wantStderr != strings.Contains(stderr, "trigger state unknown:") {
				t.Fatalf("status stderr=%q, trigger-state error wanted=%t", stderr, tc.wantStderr)
			}
		})
	}
}

func TestStatusBoundsAuditViolationsWithoutDuplicatingErrors(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		t.Fatal(err)
	}
	triggers := []byte(`{"builder":{"agent":"builder","poll_error":"poll failed","run_error":"run failed","audit_error":"audit transport failed"}}`)
	if err := os.WriteFile(forestPath(root, "triggers.json"), triggers, 0o644); err != nil {
		t.Fatal(err)
	}
	violations := make([]string, 12)
	for i := range violations {
		violations[i] = "policy-" + strconv.Itoa(i)
	}
	if err := writeAuditState(root, AuditState{
		LastMaster: "abc123",
		LastResult: "violations",
		Violations: violations,
	}, defaultAuditDependencies()); err != nil {
		t.Fatal(err)
	}
	if err := AppendRun(root, RunRecord{RunID: "status-run", Agent: "builder", Exit: 0, Duration: 1.25}); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := captureCLIOutput(t, func() int { return runCLI([]string{"status", "--root", root}) })
	if code != 0 || stderr != "" {
		t.Fatalf("status code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	want := `repo: owner/name
kernel: stopped
triggers:
  builder errors=0 code=0 running=false stale=false poll_error=poll failed run_error=run failed audit_error=audit transport failed
live runs: none
last audit: violations master=abc123
audit violations: total=12 omitted=2
audit violation: policy-0
audit violation: policy-1
audit violation: policy-2
audit violation: policy-3
audit violation: policy-4
audit violation: policy-5
audit violation: policy-6
audit violation: policy-7
audit violation: policy-8
audit violation: policy-9
recent runs (oldest first, at most 10):
  exit=0 duration=1.250s agent=builder run=status-run
`
	if stdout != want {
		t.Fatalf("status stdout:\n%s\nwant:\n%s", stdout, want)
	}
	if strings.Count(stdout, "audit transport failed") != 1 {
		t.Fatalf("status duplicated AuditError: %s", stdout)
	}
}

func TestStatusReportsExactlyTenRecentRunsInOrder(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	for i := range 12 {
		record := RunRecord{
			RunID:    "status-run-" + strconv.Itoa(i),
			Agent:    "builder",
			Started:  "2026-08-10T00:00:00Z",
			Duration: float64(i),
			Exit:     i,
		}
		if err := AppendRun(root, record); err != nil {
			t.Fatal(err)
		}
	}

	code, stdout, stderr := captureCLIOutput(t, func() int { return runCLI([]string{"status", "--root", root}) })
	if code != 0 || stderr != "" {
		t.Fatalf("status code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	var runLines []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, " run=status-run-") {
			runLines = append(runLines, line)
		}
	}
	if len(runLines) != 10 {
		t.Fatalf("recent run lines=%d, want 10: %s", len(runLines), stdout)
	}
	for i, line := range runLines {
		index := i + 2
		want := "  exit=" + strconv.Itoa(index) +
			" duration=" + strconv.Itoa(index) + ".000s" +
			" agent=builder run=status-run-" + strconv.Itoa(index)
		if line != want {
			t.Fatalf("recent run line %d=%q, want %q", i, line, want)
		}
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
			name: "held lock",
			lock: "held",
			want: []string{
				"builder errors=0 code=0 running=true",
				"live runs:\n  run_id=1787410241954942809-builder agent=builder started_at=2026-08-22T14:50:41Z elapsed=",
				`cancel="forest run cancel 1787410241954942809-builder"`,
			},
			forbid: []string{"stale=true", "running=unknown", "live runs: none", "agent=builder running=true"},
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
			if tc.lock == "held" {
				record := liveRunRecord{RunID: "1787410241954942809-builder", Agent: "builder", StartedAt: "2026-08-22T14:50:41Z"}
				if err := writeLiveRun(liveRunPath(root, "builder"), record); err != nil {
					t.Fatal(err)
				}
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

			code, stdout, stderr := captureCLIOutput(t, func() int { return runCLI([]string{"status", "--root", root}) })
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

func TestStatusPublishesLiveRunDetailsInJSON(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "poll")
	writeTriggerState(t, root, `{"builder":{"agent":"builder","running":true}}`)
	record := liveRunRecord{RunID: "run-live-builder", Agent: "builder", StartedAt: "2026-08-22T14:50:41Z"}
	if err := writeLiveRun(liveRunPath(root, "builder"), record); err != nil {
		t.Fatal(err)
	}

	_, envelope, stderr := decodeEnvelope(t, "status", "--json", "--root", root)
	if stderr != "" {
		t.Fatalf("stderr=%q, want silence", stderr)
	}
	keys := payloadKeys(t, envelope)
	liveRuns, ok := keys["live_runs"].([]any)
	if !ok || len(liveRuns) != 1 {
		t.Fatalf("live_runs=%v, want one live Run", keys["live_runs"])
	}
	run := liveRuns[0].(map[string]any)
	if run["run_id"] != "run-live-builder" || run["agent"] != "builder" || run["started_at"] != "2026-08-22T14:50:41Z" {
		t.Fatalf("live run=%v, want exact recorded identity and start", run)
	}
	if run["elapsed"] == "" || run["elapsed"] == nil {
		t.Fatalf("live run elapsed=%v, want a derived value", run["elapsed"])
	}
	if run["cancel"] != "forest run cancel run-live-builder" {
		t.Fatalf("live run cancel=%v, want the published cancellation command", run["cancel"])
	}
}

func TestLiveRunElapsedDerivesFromRecordedStart(t *testing.T) {
	record := liveRunRecord{RunID: "run-live-builder", Agent: "builder", StartedAt: "2026-08-22T14:50:41Z"}
	now := time.Date(2026, 8, 22, 14, 51, 5, 0, time.UTC)
	if view := liveRunView(record, now); view.Elapsed != "24s" {
		t.Fatalf("elapsed=%q, want 24s from recorded start", view.Elapsed)
	}
	// A clock behind the recorded start is clamped, never a negative age.
	before := time.Date(2026, 8, 22, 14, 50, 40, 0, time.UTC)
	if view := liveRunView(record, before); view.Elapsed != "0s" {
		t.Fatalf("elapsed=%q, want 0s clamp for a clock behind the start", view.Elapsed)
	}
}

func TestStatusPublishesLiveRunErrorInJSON(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "poll")
	writeTriggerState(t, root, `{"builder":{"agent":"builder","running":true}}`)
	// A regular file where the live-run directory belongs makes the read fail,
	// so the payload must carry the reason instead of a silently empty list.
	if err := os.WriteFile(forestPath(root, "runs"), []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, envelope, stderr := decodeEnvelope(t, "status", "--json", "--root", root)
	if stderr != "" {
		t.Fatalf("stderr=%q, want JSON warnings carried in the payload", stderr)
	}
	keys := payloadKeys(t, envelope)
	liveRuns, ok := keys["live_runs"].([]any)
	if !ok || len(liveRuns) != 0 {
		t.Fatalf("live_runs=%v, want empty list when the live-run read fails", keys["live_runs"])
	}
	liveRunError, ok := keys["live_run_error"].(string)
	if !ok || liveRunError == "" {
		t.Fatalf("live_run_error=%v, want a non-empty machine-readable reason", keys["live_run_error"])
	}
}

func TestRunCLISelfcheckRejectsWhitespaceSystemPrompt(t *testing.T) {
	root := t.TempDir()
	writeTestDeclaration(t, root, "builder")
	writeCLIConfig(t, root, "poll")
	agentPath := filepath.Join(root, "agents", "builder", "agent.md")
	if err := os.WriteFile(agentPath, []byte("---\nmodel: local\n---\n \n\t\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	code, _, stderr := captureCLIOutput(t, func() int { return runCLI([]string{"selfcheck"}) })
	if code != exitError || !strings.Contains(stderr, "agent builder system prompt is empty") {
		t.Fatalf("selfcheck code=%d stderr=%q, want declaration failure", code, stderr)
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
	for _, name := range []string{"git", "gh", "pi"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", bin); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })
	if code := runCLI([]string{"selfcheck", "--root", root}); code == 0 {
		t.Fatal("selfcheck accepted repository tool path")
	}
}

func writeCLIConfig(t *testing.T, root, poll string) {
	t.Helper()
	config := "repo: owner/name\nprimary: refs/heads/master\nagents:\n  builder:\n    poll: " + poll + "\n    interval: 1\nchecks:\n  - name: test\n    run: \"true\"\n"
	if err := os.WriteFile(configPath(root), []byte(config), 0o644); err != nil {
		t.Fatal(err)
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

// A Poll that fails must name its cause. A direct Poll records no state, so
// silence would leave the operator with an exit code and nothing to inspect.
func TestPollCommandReportsWhyItFailed(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	writeTestDeclaration(t, root, "builder")
	t.Chdir(root)

	for _, agent := range []string{"builder", "verifier", "fixer"} {
		code, stdout, stderr := captureCLIOutput(t, func() int { return runCLI([]string{"poll", agent}) })
		if code != exitError {
			t.Fatalf("poll %s code=%d, want %d (stdout=%q stderr=%q)", agent, code, exitError, stdout, stderr)
		}
		if !strings.Contains(stderr, "poll "+agent+": ") {
			t.Fatalf("poll %s failed silently: stderr=%q", agent, stderr)
		}
	}
}
