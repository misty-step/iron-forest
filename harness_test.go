package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderedAgentDeclaresNoStepCeiling pins the unbounded contract. opencode
// reads an absent `steps` key as no limit; any value there is a guess about how
// much work an item needs, and a wrong guess stops a working run partway and
// reports it as a gate failure. No agent definition may reintroduce one.
func TestRenderedAgentDeclaresNoStepCeiling(t *testing.T) {
	cfgDir, err := os.MkdirTemp("", "forest-opencode-config-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(cfgDir)
	a := &Agent{Name: "probe", Model: "m", Mode: "primary", Instructions: "do work"}
	if err := renderMarkdown(cfgDir, a); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cfgDir, "agents", "probe.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"steps:", "maxSteps:"} {
		if strings.Contains(string(b), key) {
			t.Errorf("rendered agent declares %q; opencode must run unbounded", key)
		}
	}
}

// TestRunPhaseKeepsConfigOutOfWorktree pins option 1 of #174: the rendered
// declaration is written outside the worktree and opencode is pointed at it with
// --config, so a working-tree tool can never read a factory artifact. The fake
// harness records its arguments and proves the config dir lies outside the
// worktree and that no .opencode ever appears inside it.
func TestRunPhaseKeepsConfigOutOfWorktree(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	script := "#!/bin/sh\nout=\nprev=\nfor a in \"$@\"; do\n  if [ \"$prev\" = \"--config\" ]; then out=$a; fi\n  prev=$a\ndone\nprintf '%s\\n' \"$@\" > " + argsFile + "\nmkdir -p \"$out/agents\"\nexit 0\n"
	wt, trace := fakeOpencode(t, script)
	a := &Agent{Name: "probe", Model: "probe-model", Instructions: "probe"}
	if _, err := runPhase(wt, a, "task", trace); err != nil {
		t.Fatalf("runPhase: %v", err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(args))
	cfg := ""
	for i, f := range fields {
		if f == "--config" && i+1 < len(fields) {
			cfg = fields[i+1]
		}
	}
	if cfg == "" {
		t.Fatalf("opencode was not pointed at an external config dir: %q", string(args))
	}
	if strings.HasPrefix(filepath.Clean(cfg), filepath.Clean(wt)+string(os.PathSeparator)) {
		t.Fatalf("config dir %q is inside the worktree %q", cfg, wt)
	}
	if _, err := os.Stat(filepath.Join(wt, ".opencode")); !os.IsNotExist(err) {
		t.Fatalf("runPhase left .opencode in the worktree: %v", err)
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
	a := &Agent{Name: "probe", Model: "probe-model", Instructions: "probe"}
	_, err := runPhase(wt, a, "task", trace)
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
	a := &Agent{Name: "probe", Model: "probe-model", Instructions: "probe"}
	_, err := runPhase(wt, a, "task", trace)
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
	a := &Agent{Name: "probe", Model: "probe-model", Instructions: "probe"}
	_, err := runPhase(wt, a, "task", trace)
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

// TestRunPhaseFailsOnAnyNonZeroExit pins the stricter contract left behind by
// deleting the step ceiling. With no ceiling opencode never announces "out of
// steps", so that exit no longer has a benign reading: a non-zero exit after
// real work is a crash or a truncation, and tokens already spent must not buy
// the run a trip to the gate.
func TestRunPhaseFailsOnAnyNonZeroExit(t *testing.T) {
	wt, trace := fakeOpencode(t,
		"#!/bin/sh\nprintf '{\"type\":\"step_finish\",\"part\":{\"tokens\":{\"input\":2}}}\n'\nprintf 'ran out of steps\\n' >&2\nexit 1\n")
	a := &Agent{Name: "probe", Model: "probe-model", Instructions: "probe"}
	stats, err := runPhase(wt, a, "task", trace)
	if err == nil {
		t.Fatal("non-zero exit must fail the run")
	}
	if stats.tokensIn != 2 {
		t.Errorf("tokensIn = %d, want the spend recorded even on failure", stats.tokensIn)
	}
}

// TestRunPhaseSuccessWithoutSteps covers a clean exit with no agent work: not a
// crash, so no error.
func TestRunPhaseSuccessWithoutSteps(t *testing.T) {
	wt, trace := fakeOpencode(t, "#!/bin/sh\nexit 0\n")
	a := &Agent{Name: "probe", Model: "probe-model", Instructions: "probe"}
	if _, err := runPhase(wt, a, "task", trace); err != nil {
		t.Fatalf("clean zero exit must succeed: %v", err)
	}
}
