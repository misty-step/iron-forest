package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// decodeEnvelope runs one read-surface command with --json and returns the single
// envelope it must emit.
func decodeEnvelope(t *testing.T, args ...string) (int, cliEnvelope, string) {
	t.Helper()
	code, stdout, stderr := captureCLIOutput(t, func() int { return runSurfaceCommand(args) })
	var envelope cliEnvelope
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("%v did not emit one envelope: %v (stdout=%q stderr=%q)", args, err, stdout, stderr)
	}
	if envelope.Schema != "forest.cli.v1" {
		t.Fatalf("schema=%q, want forest.cli.v1", envelope.Schema)
	}
	if envelope.Exit != code {
		t.Fatalf("envelope exit=%d, process exit=%d", envelope.Exit, code)
	}
	return code, envelope, stderr
}

// payloadKeys decodes the envelope data as a generic object so a test can assert
// the published key spelling.
func payloadKeys(t *testing.T, envelope cliEnvelope) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatalf("data is not an object: %v (%s)", err, encoded)
	}
	return keys
}

func TestCLIEnvelopeSeparatesCommandFromOperands(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	writeTestDeclaration(t, root, "builder")

	_, envelope, _ := decodeEnvelope(t, "declaration", "show", "builder", "--json", "--root", root)
	if envelope.Command != "declaration show" {
		t.Fatalf("command=%q, want %q: a consumer selects its parser by this field", envelope.Command, "declaration show")
	}
	if len(envelope.Args) != 1 || envelope.Args[0] != "builder" {
		t.Fatalf("args=%v, want [builder]", envelope.Args)
	}
	if envelope.Error != nil {
		t.Fatalf("error=%v, want null", *envelope.Error)
	}
}

// The schema publishes snake_case everywhere. Go field names reaching a payload
// would force a consumer to special-case that one command.
func TestCLIPayloadsUseSnakeCaseKeys(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	writeTestDeclaration(t, root, "builder")

	_, config, _ := decodeEnvelope(t, "config", "show", "--json", "--root", root)
	configKeys := payloadKeys(t, config)
	for _, key := range []string{"repo", "agents", "checks"} {
		if _, ok := configKeys[key]; !ok {
			t.Fatalf("config show payload missing %q: %v", key, configKeys)
		}
	}
	agents, ok := configKeys["agents"].(map[string]any)
	if !ok {
		t.Fatalf("agents is not an object: %v", configKeys["agents"])
	}
	builder, ok := agents["builder"].(map[string]any)
	if !ok {
		t.Fatalf("agents.builder is not an object: %v", agents)
	}
	for _, key := range []string{"poll", "interval", "timeout"} {
		if _, ok := builder[key]; !ok {
			t.Fatalf("agent payload missing %q: %v", key, builder)
		}
	}
	if _, leaked := builder["Poll"]; leaked {
		t.Fatalf("agent payload leaks Go field names: %v", builder)
	}

	_, declaration, _ := decodeEnvelope(t, "declaration", "show", "builder", "--json", "--root", root)
	declarationKeys := payloadKeys(t, declaration)
	for _, key := range []string{"name", "model", "tools", "thinking", "system_prompt", "task_prompt"} {
		if _, ok := declarationKeys[key]; !ok {
			t.Fatalf("declaration payload missing %q: %v", key, declarationKeys)
		}
	}
	if _, leaked := declarationKeys["SystemPrompt"]; leaked {
		t.Fatalf("declaration payload leaks Go field names: %v", declarationKeys)
	}
}

// A failure under --json must still be one envelope; a consumer never has to
// sniff stderr to learn why a command failed.
func TestCLIFailuresEmitOneEnvelopeUnderJSON(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")

	cases := []struct {
		name string
		args []string
		want int
	}{
		{name: "rejected limit", args: []string{"run", "list", "--limit", "0", "--json", "--root", root}, want: exitInvalidArg},
		{name: "rejected limit before json", args: []string{"run", "list", "--limit", "0", "--root", root, "--json"}, want: exitInvalidArg},
		{name: "unknown flag", args: []string{"config", "show", "--bogus", "--json", "--root", root}, want: exitInvalidArg},
		{name: "unknown command", args: []string{"widget", "show", "--json", "--root", root}, want: exitInvalidArg},
		{name: "missing operand", args: []string{"run", "show", "--json", "--root", root}, want: exitInvalidArg},
		{name: "not found", args: []string{"run", "show", "ghost", "--json", "--root", root}, want: exitNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, envelope, _ := decodeEnvelope(t, tc.args...)
			if code != tc.want {
				t.Fatalf("code=%d, want %d", code, tc.want)
			}
			if envelope.Error == nil || *envelope.Error == "" {
				t.Fatalf("envelope carries no error text: %+v", envelope)
			}
		})
	}
}

func TestCLIRejectsMalformedLimit(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	for _, raw := range []string{"12abc", "", "-3", "0", "1.5", "12 34"} {
		code, _, _ := captureCLIOutput(t, func() int {
			return runSurfaceCommand([]string{"run", "list", "--limit", raw, "--root", root})
		})
		if code != exitInvalidArg {
			t.Fatalf("--limit %q code=%d, want %d", raw, code, exitInvalidArg)
		}
	}
}

// Each command declares the flags it implements, so an unsupported flag is an
// error rather than a silent no-op.
func TestCLIRejectsFlagsTheCommandDoesNotImplement(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	cases := [][]string{
		{"config", "show", "--limit", "5"},
		{"audit", "log", "--after", "x"},
		{"run", "show", "id", "--follow"},
		{"status", "--rescan"},
		{"trigger", "list", "--follow"},
	}
	for _, args := range cases {
		full := append(append([]string{}, args...), "--root", root)
		code, _, stderr := captureCLIOutput(t, func() int { return runSurfaceCommand(full) })
		if code != exitInvalidArg {
			t.Fatalf("%v code=%d, want %d (stderr=%q)", args, code, exitInvalidArg, stderr)
		}
	}
}

func TestCLIRunListPagesNewestFirstAndTerminates(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	ids := []string{"run-a", "run-b", "run-c", "run-d", "run-e"}
	for index, id := range ids {
		if err := AppendRun(root, RunRecord{RunID: id, Agent: "builder", Exit: index}); err != nil {
			t.Fatal(err)
		}
	}

	var seen []string
	cursor := ""
	for page := 0; page < len(ids); page++ {
		args := []string{"run", "list", "--limit", "2", "--json", "--root", root}
		if cursor != "" {
			args = append(args, "--after", cursor)
		}
		_, envelope, _ := decodeEnvelope(t, args...)
		encoded, err := json.Marshal(envelope.Data)
		if err != nil {
			t.Fatal(err)
		}
		var payload runListPayload
		if err := json.Unmarshal(encoded, &payload); err != nil {
			t.Fatal(err)
		}
		for _, record := range payload.Runs {
			seen = append(seen, record.RunID)
		}
		if payload.NextAfter == "" {
			break
		}
		cursor = payload.NextAfter
	}
	want := []string{"run-e", "run-d", "run-c", "run-b", "run-a"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("paged order=%v, want %v", seen, want)
	}
}

// A cursor that names no run must be reported. Silently restarting at page one
// makes a paging consumer loop forever.
func TestCLIRunListUnknownCursorIsNotFound(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := AppendRun(root, RunRecord{RunID: "run-a", Agent: "builder"}); err != nil {
		t.Fatal(err)
	}
	code, envelope, _ := decodeEnvelope(t, "run", "list", "--after", "evicted", "--json", "--root", root)
	if code != exitNotFound {
		t.Fatalf("code=%d, want %d (envelope=%+v)", code, exitNotFound, envelope)
	}
}

func TestCLIRunLogsJSONCarriesTheLog(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := AppendRun(root, RunRecord{RunID: "run-7-builder", Agent: "builder", Exit: 3}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(runLogPath(root, "run-7-builder")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runLogPath(root, "run-7-builder"), []byte("step one\nstep two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, envelope, _ := decodeEnvelope(t, "run", "logs", "run-7-builder", "--json", "--root", root)
	keys := payloadKeys(t, envelope)
	if keys["text"] != "step one\nstep two\n" {
		t.Fatalf("payload text=%v, want the log content", keys["text"])
	}
	if keys["retained"] != true || keys["complete"] != true {
		t.Fatalf("payload=%v, want retained and complete", keys)
	}

	code, stdout, _ := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "logs", "run-7-builder", "--root", root})
	})
	if code != exitOK || stdout != "step one\nstep two\n" {
		t.Fatalf("human logs code=%d stdout=%q", code, stdout)
	}
}

// A retained-but-evicted log is distinguishable from an unknown run.
func TestCLIRunLogsSeparatesEvictedFromUnknown(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := AppendRun(root, RunRecord{RunID: "run-old", Agent: "builder", Exit: 0}); err != nil {
		t.Fatal(err)
	}

	code, envelope, _ := decodeEnvelope(t, "run", "logs", "run-old", "--json", "--root", root)
	if code != exitOK {
		t.Fatalf("evicted log code=%d, want %d", code, exitOK)
	}
	if keys := payloadKeys(t, envelope); keys["retained"] != false {
		t.Fatalf("payload=%v, want retained=false", keys)
	}

	code, _, _ = captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "logs", "ghost", "--root", root})
	})
	if code != exitNotFound {
		t.Fatalf("unknown run code=%d, want %d", code, exitNotFound)
	}
}

// --follow on a run that does not exist must fail, not wait for a Run that will
// never appear.
func TestCLIRunLogsFollowRejectsUnknownRun(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	done := make(chan int, 1)
	go func() {
		code, _, _ := captureCLIOutput(t, func() int {
			return runSurfaceCommand([]string{"run", "logs", "--follow", "ghost", "--root", root})
		})
		done <- code
	}()
	select {
	case code := <-done:
		if code != exitNotFound {
			t.Fatalf("code=%d, want %d", code, exitNotFound)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run logs --follow on an unknown run did not return")
	}
}

func TestCLIRunLogsFollowStreamsUntilTheRunCompletes(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	logPath := runLogPath(root, "run-live-builder")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("started\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A live Run implies a Kernel holding the workspace lock, which is what tells
	// follow the Run is still alive.
	lock, err := os.OpenFile(forestPath(root, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	// The Kernel goroutine owns the lock and the handle for the rest of the test,
	// so the test body never touches either concurrently.
	finished := make(chan struct{})
	defer func() {
		<-finished
		_ = lock.Close()
	}()
	go func() {
		defer close(finished)
		time.Sleep(followPollInterval)
		file, openErr := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
		if openErr == nil {
			_, _ = file.WriteString("finished\n")
			_ = file.Close()
		}
		_ = AppendRun(root, RunRecord{RunID: "run-live-builder", Agent: "builder", Exit: 7})
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	}()

	code, stdout, _ := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "logs", "--follow", "run-live-builder", "--root", root})
	})
	if code != 7 {
		t.Fatalf("follow code=%d, want the Run's exit 7 (stdout=%q)", code, stdout)
	}
	for _, want := range []string{"started", "finished"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout=%q missing %q", stdout, want)
		}
	}
}

// A Run whose Kernel died without recording an outcome must be reported, not
// followed forever.
func TestCLIRunLogsFollowReportsARunWhoseKernelDied(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	logPath := runLogPath(root, "run-orphan-builder")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("partial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _, _ := captureCLIOutput(t, func() int {
			return runSurfaceCommand([]string{"run", "logs", "--follow", "run-orphan-builder", "--root", root})
		})
		done <- code
	}()
	select {
	case code := <-done:
		if code != exitError {
			t.Fatalf("orphaned run code=%d, want %d", code, exitError)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("follow did not report a Run whose Kernel died")
	}
}

func TestCLIRunLogsFollowRejectsJSON(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	code, _, _ := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "logs", "--follow", "id", "--json", "--root", root})
	})
	if code != exitInvalidArg {
		t.Fatalf("code=%d, want %d", code, exitInvalidArg)
	}
}

// A checkout where the Kernel has never run has no trigger state. That is a
// readable answer, not a failure.
func TestCLITriggerListToleratesUnwrittenState(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")

	code, envelope, stderr := decodeEnvelope(t, "trigger", "list", "--json", "--root", root)
	if code != exitOK {
		t.Fatalf("code=%d, want %d (stderr=%q)", code, exitOK, stderr)
	}
	keys := payloadKeys(t, envelope)
	if keys["state_present"] != false {
		t.Fatalf("payload=%v, want state_present=false", keys)
	}
	triggers, ok := keys["triggers"].([]any)
	if !ok || len(triggers) != 1 {
		t.Fatalf("triggers=%v, want one configured agent", keys["triggers"])
	}
	view, ok := triggers[0].(map[string]any)
	if !ok || view["state_known"] != false {
		t.Fatalf("view=%v, want state_known=false", triggers[0])
	}
}

func TestCLITriggerResetClearsErrorsAndKeepsIdentity(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		t.Fatal(err)
	}
	triggers := []byte(`{"builder":{"agent":"builder","consecutive_errors":4,"last_code":2,` +
		`"poll_error":"poll down","run_error":"run down","audit_error":"audit down","last_run":"2026-01-01T00:00:00Z"}}`)
	if err := os.WriteFile(forestPath(root, "triggers.json"), triggers, 0o644); err != nil {
		t.Fatal(err)
	}

	_, envelope, _ := decodeEnvelope(t, "trigger", "reset", "builder", "--json", "--root", root)
	keys := payloadKeys(t, envelope)
	if keys["consecutive_errors"] != float64(0) {
		t.Fatalf("reset left errors: %v", keys)
	}
	for _, cleared := range []string{"poll_error", "run_error", "audit_error"} {
		if _, present := keys[cleared]; present {
			t.Fatalf("reset left %s: %v", cleared, keys)
		}
	}
	if keys["last_run"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("reset dropped last_run: %v", keys)
	}

	persisted, exists, err := readTriggerHealth(root)
	if err != nil || !exists {
		t.Fatalf("read persisted state: %v exists=%t", err, exists)
	}
	if persisted["builder"].ConsecutiveErrors != 0 || persisted["builder"].PollError != "" {
		t.Fatalf("reset did not persist: %+v", persisted["builder"])
	}
	if persisted["builder"].Agent != "builder" {
		t.Fatalf("reset broke identity: %+v", persisted["builder"])
	}
}

// The Scheduler owns trigger state while it runs, so a reset refuses instead of
// writing a snapshot the Kernel would overwrite.
func TestCLITriggerResetRefusesWhileKernelHoldsLock(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forestPath(root, "triggers.json"),
		[]byte(`{"builder":{"agent":"builder","consecutive_errors":3}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(forestPath(root, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	code, _, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"trigger", "reset", "builder", "--root", root})
	})
	if code != exitConflict {
		t.Fatalf("code=%d, want %d (stderr=%q)", code, exitConflict, stderr)
	}
	persisted, _, err := readTriggerHealth(root)
	if err != nil {
		t.Fatal(err)
	}
	if persisted["builder"].ConsecutiveErrors != 3 {
		t.Fatalf("refused reset still wrote state: %+v", persisted["builder"])
	}
}

func TestCLITriggerNotFoundIsNotFound(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	for _, verb := range []string{"show", "reset"} {
		code, _, _ := captureCLIOutput(t, func() int {
			return runSurfaceCommand([]string{"trigger", verb, "missing", "--root", root})
		})
		if code != exitNotFound {
			t.Fatalf("trigger %s missing code=%d, want %d", verb, code, exitNotFound)
		}
	}
}

// Empty collections marshal as [] so a consumer can iterate without a null check.
func TestCLIEmptyCollectionsAreArrays(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")

	_, envelope, _ := decodeEnvelope(t, "audit", "log", "--json", "--root", root)
	encoded, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"entries":[]`) {
		t.Fatalf("audit log payload=%s, want entries as an empty array", encoded)
	}

	_, runs, _ := decodeEnvelope(t, "run", "list", "--json", "--root", root)
	encoded, err = json.Marshal(runs.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"runs":[]`) {
		t.Fatalf("run list payload=%s, want runs as an empty array", encoded)
	}
}

func TestCLIAuditLogReturnsNewestEntriesWithinLimit(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auditLogPath(root), []byte("h1\nh2\nh3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, envelope, _ := decodeEnvelope(t, "audit", "log", "--limit", "2", "--json", "--root", root)
	encoded, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var payload auditLogPayload
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if strings.Join(payload.Entries, ",") != "h2,h3" {
		t.Fatalf("entries=%v, want the newest two in order", payload.Entries)
	}
}

// `audit show` and `status` publish the same audit shape, so one consumer parses
// both.
func TestCLIAuditShapeMatchesStatus(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		t.Fatal(err)
	}
	state := AuditState{Baseline: "base", LastMaster: "abc123", LastAt: "2026-01-01T00:00:00Z", LastResult: "violations", Violations: []string{"v1"}}
	if err := writeAuditState(root, state, defaultAuditDependencies()); err != nil {
		t.Fatal(err)
	}

	_, direct, _ := decodeEnvelope(t, "audit", "show", "--json", "--root", root)
	directKeys := payloadKeys(t, direct)
	_, snapshot, _ := decodeEnvelope(t, "status", "--json", "--root", root)
	snapshotKeys := payloadKeys(t, snapshot)
	nested, ok := snapshotKeys["audit"].(map[string]any)
	if !ok {
		t.Fatalf("status payload has no audit object: %v", snapshotKeys)
	}
	for key := range directKeys {
		if _, ok := nested[key]; !ok {
			t.Fatalf("status audit object is missing %q that audit show publishes: %v", key, nested)
		}
	}
	if directKeys["last_result"] != "violations" || directKeys["last_master"] != "abc123" {
		t.Fatalf("audit show reports the wrong fields: %v", directKeys)
	}
}

func TestCLISelfcheckEmitsEnvelope(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	writeTestDeclaration(t, root, "builder")
	bin := t.TempDir()
	for _, name := range []string{"git", "gh", "omp"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)

	code, envelope, stderr := decodeEnvelope(t, "selfcheck", "--json", "--root", root)
	if code != exitOK {
		t.Fatalf("code=%d, want %d (stderr=%q)", code, exitOK, stderr)
	}
	keys := payloadKeys(t, envelope)
	tools, ok := keys["tools"].([]any)
	if !ok || len(tools) != 3 {
		t.Fatalf("selfcheck payload tools=%v, want three resolved tools", keys["tools"])
	}
}

// status must not claim a trigger is idle when it cannot read the Kernel lock;
// the human view says unknown, so the payload must say so too.
func TestCLIStatusMarksLivenessUnknownInBothViews(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forestPath(root, "triggers.json"),
		[]byte(`{"builder":{"agent":"builder","running":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory where the lock file belongs makes the lock state unreadable.
	if err := os.Mkdir(forestPath(root, "lock"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, envelope, stderr := decodeEnvelope(t, "status", "--json", "--root", root)
	if !strings.Contains(stderr, "kernel lock state unknown:") {
		t.Fatalf("stderr=%q, want the lock diagnostic", stderr)
	}
	keys := payloadKeys(t, envelope)
	kernel, ok := keys["kernel"].(map[string]any)
	if !ok || kernel["running_known"] != false {
		t.Fatalf("kernel=%v, want running_known=false", keys["kernel"])
	}
	triggers, ok := keys["triggers"].([]any)
	if !ok || len(triggers) == 0 {
		t.Fatalf("triggers=%v", keys["triggers"])
	}
	view := triggers[0].(map[string]any)
	if view["running_known"] != false {
		t.Fatalf("trigger view=%v, want running_known=false", view)
	}

	_, stdout, _ := captureCLIOutput(t, func() int { return runCLI([]string{"status", "--root", root}) })
	if !strings.Contains(stdout, "running=unknown") {
		t.Fatalf("human status=%q, want running=unknown", stdout)
	}
}

// The usage text is generated from the command table, so the grammar cannot
// drift from the dispatcher.
func TestCLIUsageListsEveryCommand(t *testing.T) {
	_, _, stderr := captureCLIOutput(t, func() int { return runCLI(nil) })
	for _, command := range cliCommands() {
		if !strings.Contains(stderr, "forest "+command.phrase) {
			t.Fatalf("usage omits %q: %s", command.phrase, stderr)
		}
	}
}

// A boolean flag with a value is rejected, so the pre-parse --json detection and
// the parser can never disagree about the output contract.
func TestCLIBooleanFlagsRejectValues(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	for _, arg := range []string{"--json=false", "--json=true", "--follow=false", "--rescan=1"} {
		code, stdout, stderr := captureCLIOutput(t, func() int {
			return runSurfaceCommand([]string{"run", "list", arg, "--root", root})
		})
		if code != exitInvalidArg {
			t.Fatalf("%s code=%d, want %d (stdout=%q stderr=%q)", arg, code, exitInvalidArg, stdout, stderr)
		}
	}
}

// One failure must produce one output contract regardless of argument order.
func TestCLIFailureContractIsOrderIndependent(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	orders := [][]string{
		{"run", "list", "--limit", "abc", "--json", "--root", root},
		{"run", "list", "--json", "--limit", "abc", "--root", root},
		{"--json", "run", "list", "--limit", "abc", "--root", root},
	}
	for _, args := range orders {
		code, stdout, _ := captureCLIOutput(t, func() int { return runSurfaceCommand(args) })
		if code != exitInvalidArg {
			t.Fatalf("%v code=%d, want %d", args, code, exitInvalidArg)
		}
		var envelope cliEnvelope
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
			t.Fatalf("%v emitted no envelope: %v (stdout=%q)", args, err, stdout)
		}
		if envelope.Error == nil {
			t.Fatalf("%v envelope has no error", args)
		}
	}
}

// An empty flag value must not read as "flag absent" and slip past the allowlist.
func TestCLIRejectsEmptyFlagValues(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	cases := [][]string{
		{"run", "list", "--after=", "--root", root},
		{"run", "list", "--after", "", "--root", root},
		{"status", "--after=", "--root", root},
		{"config", "show", "--root", ""},
	}
	for _, args := range cases {
		code, _, stderr := captureCLIOutput(t, func() int { return runSurfaceCommand(args) })
		if code != exitInvalidArg {
			t.Fatalf("%v code=%d, want %d (stderr=%q)", args, code, exitInvalidArg, stderr)
		}
	}
}

// A passing audit is the most common state; it must not publish null violations.
func TestCLIPassingAuditPublishesEmptyViolations(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		t.Fatal(err)
	}
	// A pass writes violations as nil, which is exactly the case that used to
	// serialize as null.
	if err := writeAuditState(root, AuditState{LastMaster: "abc", LastResult: "pass", Violations: nil}, defaultAuditDependencies()); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"audit", "show", "--json", "--root", root}, {"status", "--json", "--root", root}} {
		_, envelope, _ := decodeEnvelope(t, args...)
		encoded, err := json.Marshal(envelope.Data)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"violations":[]`) {
			t.Fatalf("%v payload=%s, want violations as an empty array", args, encoded)
		}
	}
}

// A declaration without a tools key must publish an empty list, not null.
func TestCLIDeclarationWithoutToolsPublishesEmptyList(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	dir := filepath.Join(root, "agents", "builder")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.md"), []byte("---\nmodel: local\n---\nsystem\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte("task\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, envelope, _ := decodeEnvelope(t, "declaration", "show", "builder", "--json", "--root", root)
	encoded, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"tools":[]`) {
		t.Fatalf("payload=%s, want tools as an empty array", encoded)
	}
}

// Resetting an agent that is no longer configured must not write state and then
// report an error about the write.
func TestCLITriggerResetSeparatesUnconfiguredFromUnwritten(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		t.Fatal(err)
	}
	// State left behind by an agent that has since been removed from forest.yaml.
	if err := os.WriteFile(forestPath(root, "triggers.json"),
		[]byte(`{"retired":{"agent":"retired","consecutive_errors":3}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"trigger", "reset", "retired", "--root", root})
	})
	if code != exitNotFound {
		t.Fatalf("unconfigured agent code=%d, want %d (stderr=%q)", code, exitNotFound, stderr)
	}
	persisted, _, err := readTriggerHealth(root)
	if err != nil {
		t.Fatal(err)
	}
	if persisted["retired"].ConsecutiveErrors != 3 {
		t.Fatalf("refused reset still wrote state: %+v", persisted["retired"])
	}

	// A configured agent with no persisted state is a no-op, not a failure.
	code, _, stderr = captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"trigger", "reset", "builder", "--root", root})
	})
	if code != exitNoWork {
		t.Fatalf("unwritten state code=%d, want %d (stderr=%q)", code, exitNoWork, stderr)
	}
}

// Two concurrent read-only probes must not report each other as a Kernel.
func TestCLIConcurrentStatusProbesDoNotSeeEachOther(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forestPath(root, "lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// A concurrent reader holds the same shared probe the CLI uses.
	other, err := os.OpenFile(forestPath(root, "lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(other.Fd()), syscall.LOCK_UN)

	held, err := kernelLockHeld(root)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("a concurrent read-only probe was reported as a running Kernel")
	}
}

// The lock a mutation takes must be the Kernel's own, so a Kernel cannot start
// mid-write.
func TestCLIResetHoldsTheKernelLockDuringTheWrite(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forestPath(root, "triggers.json"),
		[]byte(`{"builder":{"agent":"builder","consecutive_errors":9}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	observed := make(chan bool, 1)
	outcome := withKernelLock(root, func() cliOutcome {
		// While the mutation holds the lock, a Kernel start must fail.
		file, err := os.OpenFile(forestPath(root, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			t.Error(err)
			return cliOutcome{Exit: exitError}
		}
		defer file.Close()
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		observed <- err != nil
		if err == nil {
			_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		}
		return cliOutcome{Exit: exitOK}
	})
	if outcome.Exit != exitOK {
		t.Fatalf("withKernelLock outcome=%+v", outcome)
	}
	if blocked := <-observed; !blocked {
		t.Fatal("a Kernel could take the lock while a mutation held it")
	}
}

// selfcheck publishes the paths it resolved, not a constant list of names.
func TestCLISelfcheckPublishesResolvedToolPaths(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	writeTestDeclaration(t, root, "builder")
	bin := t.TempDir()
	for _, name := range []string{"git", "gh", "omp"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)

	_, envelope, _ := decodeEnvelope(t, "selfcheck", "--json", "--root", root)
	encoded, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var payload selfcheckPayload
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Tools) != 3 {
		t.Fatalf("tools=%+v, want three", payload.Tools)
	}
	for _, tool := range payload.Tools {
		if !filepath.IsAbs(tool.Path) {
			t.Fatalf("tool %s path=%q, want an absolute resolved path", tool.Name, tool.Path)
		}
	}
	// git and gh resolve through PATH, so they must land in the trusted bin.
	for _, tool := range payload.Tools[:2] {
		if tool.Path != filepath.Join(bin, tool.Name) {
			t.Fatalf("tool %s path=%q, want %s", tool.Name, tool.Path, filepath.Join(bin, tool.Name))
		}
	}
}
