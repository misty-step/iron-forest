package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestProcessGroupOutputBoundsOverflowAndStopsDescendant(t *testing.T) {
	const limit = 1 << 20
	if trustedTransportOutputLimit != limit {
		t.Fatalf("trusted transport limit=%d, want %d", trustedTransportOutputLimit, limit)
	}
	state, heartbeat := processHeartbeatFixture(t)
	commandPath := filepath.Join(state, "noisy-transport")
	script := `#!/bin/sh
set -eu
(
	trap '' HUP TERM
	while :; do
		printf x >> "$HEARTBEAT"
		sleep 0.02
	done
) &
child=$!
printf '%s\n' "$child" > "$CHILD_PID"
while [ ! -s "$HEARTBEAT" ]; do sleep 0.01; done
/usr/bin/dd if=/dev/zero bs=1048577 count=1 2>/dev/null | /usr/bin/tr '\000' x
exit 7
`
	if err := os.WriteFile(commandPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	output, err := processGroupOutput(context.Background(), exec.Command(commandPath))
	if !errors.Is(err, errTrustedTransportOutputOverflow) {
		t.Fatalf("transport err=%v, want named overflow", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("transport err=%v, want preserved command exit 7", err)
	}
	if len(output) != limit || !bytes.Equal(output, bytes.Repeat([]byte("x"), limit)) {
		t.Fatalf("transport retained %d bytes, want exact first %d", len(output), limit)
	}
	assertProcessQuiescent(t, heartbeat, "noisy transport descendant", "overflow completion")
}

func TestRunnerBoundsNoisyOMPLogPreservesTailUsageAndStopsDescendant(t *testing.T) {
	const half = 1 << 20
	const marker = "\n--- Iron Forest Run log truncated; retained first 1 MiB and last 1 MiB ---\n"
	if runLogHalfLimit != half || runLogTruncationMarker != marker {
		t.Fatalf("Run log contract half=%d marker=%q", runLogHalfLimit, runLogTruncationMarker)
	}
	root, _ := testClone(t)
	state, heartbeat := processHeartbeatFixture(t)
	firstPath := filepath.Join(state, "first")
	middlePath := filepath.Join(state, "middle")
	tailPath := filepath.Join(state, "tail")
	usageLine := []byte("\n{\"type\":\"turn_end\",\"message\":{\"usage\":{\"input\":101,\"output\":103,\"cacheRead\":107,\"cacheWrite\":109,\"reasoningTokens\":113}}}\n")
	first := bytes.Repeat([]byte("F"), half)
	middle := bytes.Repeat([]byte("M"), 64*1024)
	tail := append(bytes.Repeat([]byte("L"), half-len(usageLine)), usageLine...)
	for path, data := range map[string][]byte{firstPath: first, middlePath: middle, tailPath: tail} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FIRST_OUTPUT", firstPath)
	t.Setenv("MIDDLE_OUTPUT", middlePath)
	t.Setenv("TAIL_OUTPUT", tailPath)
	omp := filepath.Join(state, "omp")
	script := `#!/bin/sh
set -eu
(
	trap '' HUP TERM
	while :; do
		printf x >> "$HEARTBEAT"
		sleep 0.02
	done
) &
child=$!
printf '%s\n' "$child" > "$CHILD_PID"
while [ ! -s "$HEARTBEAT" ]; do sleep 0.01; done
cat "$FIRST_OUTPUT" "$MIDDLE_OUTPUT" "$TAIL_OUTPUT"
`
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.OMPPath = omp
	record, err := runner.Run(context.Background(), Declaration{Name: "builder", Model: "local", TaskPrompt: "x"}, 10)
	if err != nil || record.Exit != 0 {
		t.Fatalf("noisy Run record=%#v err=%v", record, err)
	}
	if record.TokensIn != 101 || record.TokensOut != 103 || record.CacheRead != 107 || record.CacheWrite != 109 || record.Reasoning != 113 {
		t.Fatalf("noisy Run usage=%#v", record)
	}
	log, err := os.ReadFile(forestPath(root, "runs", record.RunID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 2*half+len(marker) {
		t.Fatalf("bounded Run log=%d bytes, want %d", len(log), 2*half+len(marker))
	}
	if !bytes.Equal(log[:half], first) || string(log[half:half+len(marker)]) != marker || !bytes.Equal(log[half+len(marker):], tail) {
		t.Fatal("bounded Run log did not retain the exact first half, marker, and last half")
	}
	assertProcessQuiescent(t, heartbeat, "noisy OMP descendant", "leader completion")
}

func TestRunnerRetainsNewestCompletedLogsAndPreservesActiveAndForeignFiles(t *testing.T) {
	if completedRunLogRetention != 32 {
		t.Fatalf("completed Run log retention=%d, want 32", completedRunLogRetention)
	}
	root, _ := testClone(t)
	runsDir := forestPath(root, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Unix(1, 0)
	fixtureName := func(index int) string { return fmt.Sprintf("%019d-builder.log", index+1) }
	for index := range 33 {
		path := filepath.Join(runsDir, fixtureName(index))
		if err := os.WriteFile(path, []byte("completed"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	foreignLog := filepath.Join(runsDir, "foreign.log")
	foreignData := filepath.Join(runsDir, "keep.data")
	foreignDirectory := filepath.Join(runsDir, "nested.log")
	if err := os.WriteFile(foreignLog, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreignData, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(foreignDirectory, 0o700); err != nil {
		t.Fatal(err)
	}

	state := t.TempDir()
	activeIDPath := filepath.Join(state, "active-id")
	releasePath := filepath.Join(state, "release")
	t.Setenv("ACTIVE_ID", activeIDPath)
	t.Setenv("RELEASE_ACTIVE", releasePath)
	activeOMP := filepath.Join(state, "active-omp")
	activeScript := `#!/bin/sh
set -eu
printf '%s' "$FOREST_RUN_ID" > "$ACTIVE_ID"
while [ ! -e "$RELEASE_ACTIVE" ]; do sleep 0.02; done
printf '%s\n' '{"usage":{"input":1}}'
`
	if err := os.WriteFile(activeOMP, []byte(activeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	quickOMP := filepath.Join(state, "quick-omp")
	if err := os.WriteFile(quickOMP, []byte("#!/bin/sh\nprintf '%s\\n' '{\"usage\":{\"input\":2}}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	type runResult struct {
		record RunRecord
		err    error
	}
	activeResult := make(chan runResult, 1)
	activeDone := make(chan struct{})
	activeRunner := NewRunner(root)
	activeRunner.OMPPath = activeOMP
	go func() {
		defer close(activeDone)
		record, err := activeRunner.Run(ctx, Declaration{Name: "builder", Model: "local", TaskPrompt: "x"}, 10)
		activeResult <- runResult{record: record, err: err}
	}()
	t.Cleanup(func() {
		cancel()
		_ = os.WriteFile(releasePath, nil, 0o600)
		select {
		case <-activeDone:
		case <-time.After(3 * time.Second):
		}
	})
	activeID := waitForFileText(t, activeIDPath)
	activeLog := filepath.Join(runsDir, activeID+".log")

	quickRunner := NewRunner(root)
	quickRunner.OMPPath = quickOMP
	quickRecord, err := quickRunner.Run(context.Background(), Declaration{Name: "fixer", Model: "local", TaskPrompt: "x"}, 10)
	if err != nil || quickRecord.Exit != 0 {
		t.Fatalf("quick Run record=%#v err=%v", quickRecord, err)
	}
	if _, err := os.Stat(activeLog); err != nil {
		t.Fatalf("active log was pruned: %v", err)
	}
	assertPathAbsent(t, filepath.Join(runsDir, fixtureName(0)))
	assertPathAbsent(t, filepath.Join(runsDir, fixtureName(1)))
	if _, err := os.Stat(filepath.Join(runsDir, fixtureName(2))); err != nil {
		t.Fatalf("deterministic retention removed the wrong tied log: %v", err)
	}
	assertForeignRunFiles(t, foreignLog, foreignData, foreignDirectory)
	if got := completedRunLogNames(t, runsDir, activeID); len(got) != completedRunLogRetention {
		t.Fatalf("completed logs=%d, want %d while %s is active: %v", len(got), completedRunLogRetention, activeID, got)
	}

	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-activeResult:
		if result.err != nil || result.record.Exit != 0 || result.record.RunID != activeID {
			t.Fatalf("active Run record=%#v err=%v", result.record, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active Run did not finish")
	}
	assertPathAbsent(t, filepath.Join(runsDir, fixtureName(2)))
	assertForeignRunFiles(t, foreignLog, foreignData, foreignDirectory)
	if got := completedRunLogNames(t, runsDir, ""); len(got) != completedRunLogRetention {
		t.Fatalf("completed logs=%d, want %d after active completion: %v", len(got), completedRunLogRetention, got)
	}
}

func waitForFileText(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) != 0 {
			return string(data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return ""
}

func assertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path %s exists or has unexpected error: %v", path, err)
	}
}

func assertForeignRunFiles(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("foreign Run-directory path %s was removed: %v", path, err)
		}
	}
}

func completedRunLogNames(t *testing.T, dir, activeID string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.Name() != activeID+".log" && !entry.IsDir() && isReservedRunLogName(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}
