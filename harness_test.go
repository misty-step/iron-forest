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

// TestRunPhaseKeepsConfigOutOfWorktree pins option 1 of #174 against the real
// opencode config/agent-directory interface: opencode is pointed at a config
// directory outside the worktree, it loads the named agent from that directory's
// agents/ (the declaration written by the factory into the factory-owned config
// space), and the operator's provider configuration survives beside it. The
// harness is no longer just a recorder of --config: it reads the config directory
// like opencode does, so an invalid path form or an agent it cannot load fails
// the run instead of being silently accepted.
func TestRunPhaseKeepsConfigOutOfWorktree(t *testing.T) {
	// The operator's global opencode provider configuration lives outside every
	// worktree and must be preserved for the run.
	xdg := t.TempDir()
	opencfg := filepath.Join(xdg, "opencode")
	if err := os.MkdirAll(opencfg, 0o755); err != nil {
		t.Fatal(err)
	}
	providerCfg := []byte(`{"provider":{"mint":{"options":{"apiKey":"__mint.tests__"}}}}`)
	if err := os.WriteFile(filepath.Join(opencfg, "opencode.json"), providerCfg, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)

	marker := filepath.Join(t.TempDir(), "loaded.txt")
	// The stub is opencode's real config/agent interface in miniature: it reads
	// the --config directory, loads the named agent from agents/, confirms the
	// operator's provider configuration survived next to it, and fails the run if
	// either piece is unusable.
	script := "#!/bin/sh\ncfg=\nagent=\nprev=\nfor a in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"--config\" ]; then cfg=$a; fi\n" +
		"  if [ \"$prev\" = \"--agent\" ]; then agent=$a; fi\n" +
		"  prev=$a\n" +
		"done\n" +
		"if [ -z \"$cfg\" ] || [ -z \"$agent\" ]; then echo 'opencode: missing --config or --agent' >&2; exit 1; fi\n" +
		"if [ ! -f \"$cfg/agents/$agent.md\" ]; then echo \"opencode: agent $agent failed to load from $cfg/agents/\" >&2; exit 1; fi\n" +
		"preserved=no\n" +
		"test -f \"$cfg/opencode.json\" && preserved=yes\n" +
		"printf '%s %s\\n' \"$agent\" \"$preserved\" > " + marker + "\n" +
		"exit 0\n"
	wt, trace := fakeOpencode(t, script)
	a := &Agent{Name: "probe", Model: "probe-model", Instructions: "probe"}
	if _, err := runPhase(wt, a, "task", trace); err != nil {
		t.Fatalf("runPhase: %v", err)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != "probe yes\n" {
		t.Fatalf("stub observed %q, want the agent loaded and the provider config preserved", got)
	}
	// The config dir the stub read is discarded by runPhase; its agent and
	// provider config reached the stub from outside the worktree, so no .opencode
	// must ever appear inside it.
	if _, err := os.Stat(filepath.Join(wt, ".opencode")); !os.IsNotExist(err) {
		t.Fatalf("runPhase left .opencode in the worktree: %v", err)
	}
}

// TestRunPhaseFailsWhenAgentIsUnloadable proves runPhase surfaces an opencode
// failure to load the agent from the external config dir. The shallow recorder
// could not catch this (it never read the config directory); a stub pinned to the
// pre-#174 agent location under the worktree's .opencode/ cannot find the
// declaration the factory now writes into the factory-owned config space, so the
// run must fail with the load error recorded.
func TestRunPhaseFailsWhenAgentIsUnloadable(t *testing.T) {
	wt := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(wt, ".opencode", "agents", "probe.md")
	script := "#!/bin/sh\n" +
		"if [ ! -f \"" + old + "\" ]; then echo 'opencode: agent probe failed to load' >&2; exit 1; fi\n" +
		"exit 0\n"
	trace := filepath.Join(t.TempDir(), "run", "agent.jsonl")
	fakeOpencode(t, script)
	a := &Agent{Name: "probe", Model: "probe-model", Instructions: "probe"}
	_, err := runPhase(wt, a, "task", trace)
	if err == nil {
		t.Fatal("runPhase must fail when opencode cannot load the agent")
	}
	if !strings.Contains(err.Error(), "failed to load") {
		t.Errorf("error %q did not record why the agent could not be loaded", err)
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
