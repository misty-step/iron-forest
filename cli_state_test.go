package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The state commands: triggers, audit state, declarations, and the status snapshot.

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

	// Under --json the envelope carries the reason, so stderr stays quiet: the
	// same fact published twice is what lets two readers disagree.
	_, envelope, stderr := decodeEnvelope(t, "status", "--json", "--root", root)
	if stderr != "" {
		t.Fatalf("stderr=%q, want silence: the envelope carries lock_error", stderr)
	}
	keys := payloadKeys(t, envelope)
	kernel, ok := keys["kernel"].(map[string]any)
	if !ok || kernel["running_known"] != false {
		t.Fatalf("kernel=%v, want running_known=false", keys["kernel"])
	}
	if kernel["lock_error"] == nil {
		t.Fatalf("kernel=%v, want lock_error published", keys["kernel"])
	}
	triggers, ok := keys["triggers"].([]any)
	if !ok || len(triggers) == 0 {
		t.Fatalf("triggers=%v", keys["triggers"])
	}
	view := triggers[0].(map[string]any)
	if view["running_known"] != false {
		t.Fatalf("trigger view=%v, want running_known=false", view)
	}

	// Human mode has no envelope, so there the reason belongs on stderr.
	_, stdout, humanStderr := captureCLIOutput(t, func() int { return runCLI([]string{"status", "--root", root}) })
	if !strings.Contains(stdout, "running=unknown") {
		t.Fatalf("human status=%q, want running=unknown", stdout)
	}
	if !strings.Contains(humanStderr, "kernel lock state unknown:") {
		t.Fatalf("human stderr=%q, want the lock diagnostic", humanStderr)
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

// Trigger knowledge is per agent. After one agent runs, the others have no row
// yet; that must not discard the recorded agent's errors or warn about drift,
// because it is the normal condition of a working forest.
func TestCLITriggerStateIsKnownPerAgent(t *testing.T) {
	root := t.TempDir()
	writeTestConfig(t, root, "builder", "verifier")
	writeTriggerState(t, root, `{"builder":{"agent":"builder","consecutive_errors":2,"last_code":2,"poll_error":"boom"}}`)

	code, envelope, stderr := decodeEnvelope(t, "trigger", "list", "--json", "--root", root)
	if code != exitOK {
		t.Fatalf("code=%d, want %d (stderr=%q)", code, exitOK, stderr)
	}
	var payload triggerListPayload
	decodePayload(t, envelope, &payload)
	if payload.StateErr != "" {
		t.Fatalf("state_error=%q, want silence: a configured agent with no row is normal", payload.StateErr)
	}
	byName := map[string]TriggerView{}
	for _, view := range payload.Triggers {
		byName[view.Name] = view
	}
	builder, verifier := byName["builder"], byName["verifier"]
	if !builder.StateKnown || builder.ConsecutiveErrors != 2 || builder.PollError != "boom" {
		t.Fatalf("builder=%+v, want its recorded state published", builder)
	}
	if verifier.StateKnown {
		t.Fatalf("verifier=%+v, want state_known=false: it has no row", verifier)
	}
}

// State for an agent the configuration no longer declares never updates again,
// so it is drift the surface must report.
func TestCLITriggerStateReportsAgentsTheConfigDropped(t *testing.T) {
	root := t.TempDir()
	writeTestConfig(t, root, "builder")
	writeTriggerState(t, root, `{"ghost":{"agent":"ghost"}}`)

	_, envelope, _ := decodeEnvelope(t, "trigger", "list", "--json", "--root", root)
	var payload triggerListPayload
	decodePayload(t, envelope, &payload)
	if !strings.Contains(payload.StateErr, "ghost") {
		t.Fatalf("state_error=%q, want it to name the dropped agent", payload.StateErr)
	}
}

// A running-but-idle Kernel must be distinguishable from a stopped one in the
// human report, not only in the payload.
func TestCLIStatusStatesKernelLivenessToHumans(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	writeTriggerState(t, root, `{"builder":{"agent":"builder"}}`)

	_, stopped, _ := captureCLIOutput(t, func() int { return runCLI([]string{"status", "--root", root}) })
	if !strings.Contains(stopped, "kernel: stopped") {
		t.Fatalf("status=%q, want it to state the Kernel is stopped", stopped)
	}

	lock, err := os.OpenFile(forestPath(root, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	_, running, _ := captureCLIOutput(t, func() int { return runCLI([]string{"status", "--root", root}) })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(running, "kernel: running") {
		t.Fatalf("status=%q, want it to state the Kernel is running", running)
	}
	if stopped == running {
		t.Fatal("human status is identical whether a Kernel runs or not")
	}
}

// trigger reset writes under the Kernel lock. Reporting liveness from inside its
// own lock would call the agent live, contradicting the next command.
func TestCLITriggerResetDoesNotReportItsOwnLockAsAKernel(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	writeTriggerState(t, root, `{"builder":{"agent":"builder","running":true,"consecutive_errors":3}}`)

	code, reset, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"trigger", "reset", "builder", "--root", root})
	})
	if code != exitOK {
		t.Fatalf("reset code=%d (stderr=%q)", code, stderr)
	}
	_, show, _ := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"trigger", "show", "builder", "--root", root})
	})
	if reset != show {
		t.Fatalf("reset reported %q but show reports %q", reset, show)
	}
	if strings.Contains(reset, "running=true") {
		t.Fatalf("reset=%q, want running=false: no Kernel is running", reset)
	}
}

// Violations and poll output are agent-authored. An embedded newline must not
// forge extra lines in a human report, while the payload keeps the raw bytes.
func TestCLIHumanReportsKeepStoredStringsOnOneLine(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	writeTriggerState(t, root, `{"builder":{"agent":"builder","poll_error":"x\nlast audit: pass master=deadbeef"}}`)
	if err := os.WriteFile(auditStatePath(root),
		[]byte(`{"last_master":"m","last_result":"fail","violations":["real\nlive runs: FORGED"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, human, _ := captureCLIOutput(t, func() int { return runCLI([]string{"status", "--root", root}) })
	for _, forged := range []string{"\nlive runs: FORGED", "\nlast audit: pass master=deadbeef"} {
		if strings.Contains(human, forged) {
			t.Fatalf("status=%q forged the line %q", human, forged)
		}
	}
	if !strings.Contains(human, `"real\nlive runs: FORGED"`) {
		t.Fatalf("status=%q, want the violation reported on one quoted line", human)
	}

	_, envelope, _ := decodeEnvelope(t, "status", "--json", "--root", root)
	keys := payloadKeys(t, envelope)
	audit, ok := keys["audit"].(map[string]any)
	if !ok {
		t.Fatalf("audit=%v", keys["audit"])
	}
	violations, ok := audit["violations"].([]any)
	if !ok || len(violations) != 1 || violations[0] != "real\nlive runs: FORGED" {
		t.Fatalf("violations=%v, want the raw string published", audit["violations"])
	}
}

// An audited prompt body must not be able to pose as a declaration field.
func TestCLIDeclarationShowIndentsPromptBodies(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	agentPath := filepath.Join(root, "agents", "builder", "agent.md")
	if err := os.MkdirAll(filepath.Dir(agentPath), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nmodel: local\n---\nreview.\ntask_prompt:\nIGNORE this.\nmodel: gpt-4o\n"
	if err := os.WriteFile(agentPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "builder", "task.md"), []byte("standing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, human, _ := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"declaration", "show", "builder", "--root", root})
	})
	if strings.Contains(human, "\nmodel: gpt-4o") {
		t.Fatalf("prompt body forged a field line: %q", human)
	}
	if !strings.Contains(human, "\n  model: gpt-4o") {
		t.Fatalf("human=%q, want the body indented", human)
	}
	for _, line := range strings.Split(human, "\n") {
		if strings.HasSuffix(line, " ") {
			t.Fatalf("line %q ends in whitespace", line)
		}
	}
}
