package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPhaseContextUnboundedWithoutBudget pins the no-deadline contract. An agent
// reasoning at maximum effort can work for a long time and still be productive,
// so an undeclared budget must produce a context with no deadline: a clock here
// kills a working run and throws away every token it already spent.
func TestPhaseContextUnboundedWithoutBudget(t *testing.T) {
	for _, budget := range []time.Duration{0, -time.Second} {
		ctx, cancel := phaseContext(budget)
		if deadline, ok := ctx.Deadline(); ok {
			t.Errorf("budget %v produced a deadline at %v", budget, deadline)
		}
		cancel()
	}
}

// TestPhaseContextHonorsDeclaredBudget keeps the opt-in bound working: an agent
// that declares a budget is still stopped at it.
func TestPhaseContextHonorsDeclaredBudget(t *testing.T) {
	ctx, cancel := phaseContext(30 * time.Second)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("declared budget produced no deadline")
	}
	if left := time.Until(deadline); left <= 0 || left > 30*time.Second {
		t.Fatalf("deadline in %v, want inside 30s", left)
	}
}

// fakeOpencode puts an executable named "opencode" on PATH so runPhase drives
// the stub instead of the real harness, and returns the trace path.
func fakeOpencode(t *testing.T, script string) (string, string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	wt := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(t.TempDir(), "run", "agent.jsonl")
	return wt, trace
}

// TestRunPhaseFailsOnHarnessCrash proves the gate never gets a run the harness
// did not finish: a non-zero exit outside the out-of-steps case marks the run
// failed and records the stderr in the error.
func TestRunPhaseFailsOnHarnessCrash(t *testing.T) {
	wt, trace := fakeOpencode(t, "#!/bin/sh\nprintf 'model call rejected\\n' >&2\nexit 1\n")
	a := &Agent{Name: "probe", Model: "probe-model", Steps: 5, Instructions: "probe"}
	_, err := runPhase(wt, a, "task", trace, 0)
	if err == nil {
		t.Fatal("runPhase returned nil error on a crashed harness")
	}
	if !strings.Contains(err.Error(), "model call rejected") {
		t.Errorf("error %q did not record harness stderr", err)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("error %q did not record the exit status", err)
	}
}

// TestRunPhaseFailsOnKilledHarness proves a signal-killed run is a crash, not
// an out-of-steps stop: it must fail even if it already streamed agent work.
func TestRunPhaseFailsOnKilledHarness(t *testing.T) {
	wt, trace := fakeOpencode(t,
		"#!/bin/sh\nprintf '{\"type\":\"step_finish\",\"part\":{\"tokens\":{\"input\":1}}}\n'\nkill -KILL $$\n")
	a := &Agent{Name: "probe", Model: "probe-model", Steps: 5, Instructions: "probe"}
	_, err := runPhase(wt, a, "task", trace, 0)
	if err == nil {
		t.Fatal("runPhase returned nil error on a signal-killed harness")
	}
}

// TestRunPhaseFailsOnCrashAfterWork proves a normal (non-signal) non-zero exit
// after streamed work is still a crash unless the harness announced the
// documented out-of-steps condition. A provider failure that exits 1 after an
// earlier step_finish must fail and record both status and stderr, so runPick
// never reaches the gate with partial work.
func TestRunPhaseFailsOnCrashAfterWork(t *testing.T) {
	wt, trace := fakeOpencode(t,
		"#!/bin/sh\nprintf '{\"type\":\"step_finish\",\"part\":{\"tokens\":{\"input\":2}}}\n'\nprintf 'model call rejected\\n' >&2\nexit 1\n")
	a := &Agent{Name: "probe", Model: "probe-model", Steps: 5, Instructions: "probe"}
	_, err := runPhase(wt, a, "task", trace, 0)
	if err == nil {
		t.Fatal("runPhase returned nil error on a non-zero crash after work")
	}
	if !strings.Contains(err.Error(), "model call rejected") {
		t.Errorf("error %q did not record harness stderr", err)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("error %q did not record the exit status", err)
	}
}

// TestRunPhaseOutOfStepsWithZeroTokenEvents proves a step_finish counts as work
// even when it reports zero tokens: failure classification tracks the presence
// of completed steps, not the token totals.
func TestRunPhaseOutOfStepsWithZeroTokenEvents(t *testing.T) {
	wt, trace := fakeOpencode(t,
		"#!/bin/sh\nprintf '{\"type\":\"step_finish\",\"part\":{\"tokens\":{\"input\":0}}}\n'\nprintf 'ran out of steps\\n' >&2\nexit 1\n")
	a := &Agent{Name: "probe", Model: "probe-model", Steps: 5, Instructions: "probe"}
	if _, err := runPhase(wt, a, "task", trace, 0); err != nil {
		t.Fatalf("out-of-steps with zero-token step_finish must not fail: %v", err)
	}
}

// TestRunPhaseToleratesOutOfStepsExit pins the documented out-of-steps case:
// opencode exits non-zero after producing work, and the gate decides.
func TestRunPhaseToleratesOutOfStepsExit(t *testing.T) {
	wt, trace := fakeOpencode(t,
		"#!/bin/sh\nprintf '{\"type\":\"step_finish\",\"part\":{\"tokens\":{\"input\":2}}}\n'\nprintf 'ran out of steps\\n' >&2\nexit 1\n")
	a := &Agent{Name: "probe", Model: "probe-model", Steps: 5, Instructions: "probe"}
	stats, err := runPhase(wt, a, "task", trace, 0)
	if err != nil {
		t.Fatalf("out-of-steps non-zero exit must not fail the run: %v", err)
	}
	if stats.tokensIn != 2 {
		t.Errorf("tokensIn = %d, want 2", stats.tokensIn)
	}
}

// TestRunPhaseSuccessWithoutSteps covers a clean exit with no agent work: not a
// crash, not out of steps, so no error.
func TestRunPhaseSuccessWithoutSteps(t *testing.T) {
	wt, trace := fakeOpencode(t, "#!/bin/sh\nexit 0\n")
	a := &Agent{Name: "probe", Model: "probe-model", Steps: 5, Instructions: "probe"}
	if _, err := runPhase(wt, a, "task", trace, 0); err != nil {
		t.Fatalf("clean zero exit must succeed: %v", err)
	}
}
