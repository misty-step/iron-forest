package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunCancelAlreadyFinishedAndUnknown(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := AppendRun(root, RunRecord{RunID: "run-1-builder", Agent: "builder", Exit: 3}); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "cancel", "run-1-builder", "--root", root})
	})
	if code != exitOK || stderr != "" {
		t.Fatalf("finished cancel code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "already_finished") {
		t.Fatalf("finished cancel stdout=%q, want already_finished", stdout)
	}

	_, envelope, _ := decodeEnvelope(t, "run", "cancel", "run-1-builder", "--json", "--root", root)
	keys := payloadKeys(t, envelope)
	if keys["run_id"] != "run-1-builder" || keys["state"] != "already_finished" {
		t.Fatalf("finished cancel payload=%v, want run_id and already_finished", keys)
	}

	code, _, stderr = captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "cancel", "ghost", "--root", root})
	})
	if code != exitNotFound {
		t.Fatalf("unknown cancel code=%d, want %d (stderr=%q)", code, exitNotFound, stderr)
	}
}

func TestRunCancelAlreadyFinishedFromMarker(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := writeRunCancellationMarker(root, "run-2-builder"); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "cancel", "run-2-builder", "--root", root})
	})
	if code != exitOK || stderr != "" || !strings.Contains(stdout, "already_finished") {
		t.Fatalf("marker cancel code=%d stderr=%q stdout=%q, want already_finished", code, stderr, stdout)
	}
}

func TestRunCancelStopsLiveRunAndRecordsCancellation(t *testing.T) {
	root, _ := testClone(t)
	state := t.TempDir()
	runIDFile := filepath.Join(state, "run-id")
	rootFile := filepath.Join(state, "run-root")
	_, heartbeat := processHeartbeatFixture(t)
	t.Setenv("RUN_ID_FILE", runIDFile)
	t.Setenv("RUN_ROOT_FILE", rootFile)

	pi := filepath.Join(t.TempDir(), "pi")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$FOREST_RUN_ID" > "$RUN_ID_FILE"
printf '%s\n' "$FOREST_ROOT" > "$RUN_ROOT_FILE"
printf '%s\n' "$$" > "$CHILD_PID"
trap '' TERM
while :; do
	printf x >> "$HEARTBEAT"
	sleep 0.02
done
`
	if err := os.WriteFile(pi, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(root)
	runner.PiPath = pi
	type runResult struct {
		record RunRecord
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		record, err := runner.Run(context.Background(), Declaration{Name: "builder", Model: "local", TaskPrompt: "x"})
		done <- runResult{record: record, err: err}
	}()

	waitForCLIFile(t, runIDFile)
	runID := strings.TrimSpace(string(mustReadFile(t, runIDFile)))
	processRoot := strings.TrimSpace(string(mustReadFile(t, rootFile)))
	if processRoot != root {
		t.Fatalf("FOREST_ROOT=%q, want checkout root %q", processRoot, root)
	}

	code, stdout, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "cancel", runID, "--root", root})
	})
	if code != exitOK || stderr != "" {
		t.Fatalf("live cancel code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "cancelled") {
		t.Fatalf("live cancel stdout=%q, want cancelled", stdout)
	}

	// A second cancel before the Runner has necessarily reached the Ledger must
	// still be idempotent.
	code, stdout, stderr = captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "cancel", runID, "--root", root})
	})
	if code != exitOK || stderr != "" || !strings.Contains(stdout, "already_finished") {
		t.Fatalf("second cancel code=%d stderr=%q stdout=%q, want already_finished", code, stderr, stdout)
	}

	select {
	case result := <-done:
		if result.record.Exit == 0 || result.record.Error != runCancelledError {
			t.Fatalf("cancelled run record=%#v err=%v, want nonzero exit and cancellation cause", result.record, result.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Runner did not finish after cancel")
	}

	rows, err := ReadLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("ledger rows=%v, want one cancelled row", rows)
	}
	if rows[0].RunID != runID || rows[0].Exit == 0 || rows[0].Error != runCancelledError {
		t.Fatalf("cancelled ledger row=%#v, want run %s with nonzero exit and cancellation cause", rows[0], runID)
	}
	assertProcessQuiescent(t, heartbeat, "cancelled run", "run cancel")
}

func TestRunCancelDoesNotCancelAnotherCheckout(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	otherRoot := t.TempDir()
	state, _ := processHeartbeatFixture(t)
	runID := "run-1-builder"

	command := exec.Command("/bin/sh", "-c", `#!/bin/sh
printf '%s\n' "$$" > "$CHILD_PID"
trap '' TERM
while :; do
	printf x >> "$HEARTBEAT"
	sleep 0.02
done
`)
	command.Env = append(os.Environ(), "FOREST_RUN_ID="+runID, "FOREST_ROOT="+otherRoot)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	childPID := filepath.Join(state, "child-pid")
	waitForCLIFile(t, childPID)
	pidData := mustReadFile(t, childPID)
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil || pid <= 1 {
		t.Fatalf("foreign run pid=%q err=%v", pidData, err)
	}

	code, _, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "cancel", runID, "--root", root})
	})
	if code != exitNotFound {
		t.Fatalf("foreign checkout cancel code=%d, want %d (stderr=%q)", code, exitNotFound, stderr)
	}
	if !processGroupExists(pid) {
		t.Fatalf("foreign checkout process group %d was cancelled", pid)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
