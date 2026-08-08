package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRunPhaseKeepsConfigOutOfWorktree pins option 1 of #174 against opencode's
// supported external configuration mechanism: the per-run config root is handed
// to opencode through XDG_CONFIG_HOME in the child environment, so opencode
// reads the named agent from that root's opencode/agents/ (the declaration the
// factory renders into the factory-owned config space), keeps the provider
// configuration a real run actually uses from the factory project's own
// .opencode/opencode.json, and installs the dependencies it needs under that
// root. The stub is opencode's real config/agent interface in miniature: it
// fails the run unless XDG_CONFIG_HOME is set, the agent loads, the provider
// config survived, and no .opencode ever appeared in the managed worktree.
func TestRunPhaseKeepsConfigOutOfWorktree(t *testing.T) {
	// The factory project's own .opencode/opencode.json is the provider config a
	// real run actually uses; it must be preserved beside the rendered agent.
	factory := t.TempDir()
	factoryOC := filepath.Join(factory, ".opencode")
	if err := os.MkdirAll(factoryOC, 0o755); err != nil {
		t.Fatal(err)
	}
	providerCfg := []byte(`{"provider":{"mint":{"options":{"apiKey":"__mint.tests__"}}}}`)
	if err := os.WriteFile(filepath.Join(factoryOC, "opencode.json"), providerCfg, 0o644); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "loaded.txt")
	script := "#!/bin/sh\n" +
		"if [ -z \"$XDG_CONFIG_HOME\" ]; then echo 'opencode: XDG_CONFIG_HOME not set' >&2; exit 1; fi\n" +
		"base=\"$XDG_CONFIG_HOME/opencode\"\n" +
		"agent=\nprev=\nfor a in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"--agent\" ]; then agent=$a; fi\n" +
		"  prev=$a\n" +
		"done\n" +
		"if [ -z \"$agent\" ]; then echo 'opencode: missing --agent' >&2; exit 1; fi\n" +
		"if [ ! -f \"$base/agents/$agent.md\" ]; then echo \"opencode: agent $agent failed to load from $base/agents/\" >&2; exit 1; fi\n" +
		"if [ ! -f \"$base/opencode.json\" ]; then echo 'opencode: provider config missing' >&2; exit 1; fi\n" +
		// The dependencies opencode installs for its provider packages land in the
		// run's own config root, never in the managed worktree's .opencode/.
		"mkdir -p \"$base/node_modules\"\n" +
		"if [ -e \".opencode\" ]; then echo 'opencode: created project .opencode in the worktree' >&2; exit 1; fi\n" +
		"printf 'loaded\\n' > " + marker + "\n" +
		"exit 0\n"
	wt, trace := fakeOpencode(t, script)
	a := &Agent{Name: "probe", Model: "probe-model", Instructions: "probe"}
	if _, err := runPhase(factory, wt, a, "task", trace); err != nil {
		t.Fatalf("runPhase: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("stub did not confirm the external config route: %v", err)
	}
	// The config root the stub read is discarded by runPhase; its agent and
	// provider config reached the stub from outside the worktree, so no
	// .opencode must ever appear inside the worktree either.
	if _, err := os.Stat(filepath.Join(wt, ".opencode")); !os.IsNotExist(err) {
		t.Fatalf("runPhase left .opencode in the worktree: %v", err)
	}
}

// TestRunPhaseIgnoresWorktreeProjectConfig pins the other half of option 1 for
// #174: opencode must not read a project-local .opencode/opencode.json it would
// discover inside the managed worktree. A real pinned opencode, given a local
// project config, installs the provider packages it needs into .opencode/ beside
// that config and then reads it — writing a per-run artifact into the tree a
// hook or a working-tree secret scanner reads. The run therefore disables local
// project config discovery in the child environment while supplying the copied
// provider config through the external root. The stub models opencode's
// behaviour: it fails the run unless local project config discovery is disabled
// and, were it not disabled, would simulate the unwanted install into the
// managed tree.
func TestRunPhaseIgnoresWorktreeProjectConfig(t *testing.T) {
	// A managed repository that ships a .opencode/opencode.json of its own would
	// otherwise be discovered as opencode's local project config.
	factory := t.TempDir()
	factoryOC := filepath.Join(factory, ".opencode")
	if err := os.MkdirAll(factoryOC, 0o755); err != nil {
		t.Fatal(err)
	}
	providerCfg := []byte(`{"provider":{"mint":{"options":{"apiKey":"__mint.tests__"}}}}`)
	if err := os.WriteFile(filepath.Join(factoryOC, "opencode.json"), providerCfg, 0o644); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "loaded.txt")
	script := "#!/bin/sh\n" +
		// A real opencode with local project config discovery enabled reads the
		// worktree's .opencode/opencode.json and installs node_modules beside it.
		"if [ \"$OPENCODE_DISABLE_PROJECT_CONFIG\" != \"1\" ]; then\n" +
		"  mkdir -p .opencode/node_modules\n" +
		"  printf 'installed\\n' > .opencode/node_modules/thing.js\n" +
		"  echo 'opencode: local project config was discovered' >&2\n" +
		"  exit 0\n" +
		"fi\n" +
		// With local project config disabled, only the external root is read.
		"base=\"$XDG_CONFIG_HOME/opencode\"\n" +
		"if [ ! -f \"$base/opencode.json\" ]; then echo 'opencode: provider config missing' >&2; exit 1; fi\n" +
		"if [ ! -f \"$base/agents/probe.md\" ]; then echo 'opencode: agent probe failed to load' >&2; exit 1; fi\n" +
		"printf 'loaded\\n' > " + marker + "\n" +
		"exit 0\n"

	// The managed worktree starts by carrying its own project-local
	// .opencode/opencode.json, exactly the shape the old per-worktree stub could
	// not see. It must survive the run unchanged, with no factory artifact added.
	wt, trace := fakeOpencode(t, script)
	wtOC := filepath.Join(wt, ".opencode")
	if err := os.MkdirAll(wtOC, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtOC, "opencode.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &Agent{Name: "probe", Model: "probe-model", Instructions: "probe"}
	if _, err := runPhase(factory, wt, a, "task", trace); err != nil {
		t.Fatalf("runPhase: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("stub did not confirm the external config was used: %v", err)
	}
	ents, err := os.ReadDir(wtOC)
	if err != nil {
		t.Fatalf("managed worktree project config lost: %v", err)
	}
	if len(ents) != 1 || ents[0].Name() != "opencode.json" {
		t.Fatalf("runPhase wrote factory artifacts into the managed tree's .opencode: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(wt, ".opencode", "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("runPhase left node_modules in the managed worktree")
	}
}

// TestRunPhaseFallsBackToGlobalProviderConfig proves a factory that ships no
// project .opencode/opencode.json still gets a usable run: the operator's global
// opencode provider configuration is preserved as the fallback. The stub checks
// that a provider config made it into the run config root without requiring the
// factory project to carry one.
func TestRunPhaseFallsBackToGlobalProviderConfig(t *testing.T) {
	// The operator's global opencode provider configuration is the fallback when
	// the factory ships no project config of its own.
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
	script := "#!/bin/sh\n" +
		"base=\"$XDG_CONFIG_HOME/opencode\"\n" +
		"if [ ! -f \"$base/opencode.json\" ]; then echo 'opencode: provider config missing' >&2; exit 1; fi\n" +
		"printf 'ok\\n' > " + marker + "\n" +
		"exit 0\n"
	factory := t.TempDir() // ships no .opencode/opencode.json
	wt, trace := fakeOpencode(t, script)
	a := &Agent{Name: "probe", Model: "probe-model", Instructions: "probe"}
	if _, err := runPhase(factory, wt, a, "task", trace); err != nil {
		t.Fatalf("runPhase: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("stub did not confirm the fallback provider config: %v", err)
	}
}

// TestRunPhaseFailsWhenAgentIsUnloadable proves runPhase surfaces an opencode
// failure to load the agent from the external config root. A stub pinned to the
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
	_, err := runPhase(t.TempDir(), wt, a, "task", trace)
	if err == nil {
		t.Fatal("runPhase must fail when opencode cannot load the agent")
	}
	if !strings.Contains(err.Error(), "failed to load") {
		t.Errorf("error %q did not record why the agent could not be loaded", err)
	}
}

// TestRunPhaseDeliversLargePromptViaStdin pins the #204 fix: a prompt larger
// than Linux's single-argument ceiling (maxArgLen = 131072) must still reach the
// agent. runPhase streamed the prompt as one argv entry, so anything over that
// failed with a raw fork/exec "argument list too long" before opencode was
// reached. The harness now writes the full prompt to a .prompt.txt file beside
// the trace and streams the same text on stdin, which has no per-entry ceiling.
// The stub captures both channels: runPhase must succeed (never E2BIG), the
// prompt must arrive whole on stdin, and it must not appear in argv.
func TestRunPhaseDeliversLargePromptViaStdin(t *testing.T) {
	big := strings.Repeat("prompt-payload", (200*1024)/len("prompt-payload")) + "END-MARKER"
	if len(big) <= maxArgLen {
		t.Fatalf("test prompt is %d bytes, must exceed maxArgLen %d", len(big), maxArgLen)
	}
	stdinMarker := filepath.Join(t.TempDir(), "stdin.txt")
	argvMarker := filepath.Join(t.TempDir(), "argv.txt")
	script := "#!/bin/sh\n" +
		"cat > '" + stdinMarker + "'\n" +
		"printf '%s\\n' \"$@\" > '" + argvMarker + "'\n" +
		"exit 0\n"
	wt, trace := fakeOpencode(t, script)
	a := &Agent{Name: "probe", Model: "probe-model", Instructions: "probe"}
	if _, err := runPhase(t.TempDir(), wt, a, big, trace); err != nil {
		t.Fatalf("runPhase failed on a %d-byte prompt: %v", len(big), err)
	}
	got, err := os.ReadFile(stdinMarker)
	if err != nil {
		t.Fatalf("prompt did not reach the agent on stdin: %v", err)
	}
	if string(got) != big {
		t.Errorf("stdin prompt mismatch: forwarded %d bytes, want %d", len(got), len(big))
	}
	argv, err := os.ReadFile(argvMarker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), "prompt-payload") {
		t.Errorf("prompt leaked into argv: %.120q", string(argv))
	}
}

// TestPromptDeliveryErrorNamesSizeAndLimit pins requirement 3 of #204: a prompt
// that cannot be delivered whole fails with a named error stating the prompt
// size and the delivery ceiling, never the kernel's raw fork/exec "argument
// list too long" message. Naming both makes a mechanical delivery failure
// auditable and distinguishable from a content problem.
func TestPromptDeliveryErrorNamesSizeAndLimit(t *testing.T) {
	err := &promptDeliveryError{size: 134718, limit: maxArgLen}
	msg := err.Error()
	if !strings.Contains(msg, "134718") {
		t.Errorf("error %q did not name the prompt size", msg)
	}
	if !strings.Contains(msg, strconv.Itoa(maxArgLen)) {
		t.Errorf("error %q did not name the delivery ceiling %d", msg, maxArgLen)
	}
	if strings.Contains(msg, "fork/exec") || strings.Contains(msg, "argument list too long") {
		t.Errorf("error %q leaked the raw kernel fork/exec message", msg)
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
	_, err := runPhase(t.TempDir(), wt, a, "task", trace)
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
	_, err := runPhase(t.TempDir(), wt, a, "task", trace)
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
	_, err := runPhase(t.TempDir(), wt, a, "task", trace)
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
	stats, err := runPhase(t.TempDir(), wt, a, "task", trace)
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
	if _, err := runPhase(t.TempDir(), wt, a, "task", trace); err != nil {
		t.Fatalf("clean zero exit must succeed: %v", err)
	}
}

// TestNoTokenClassDroppedBetweenTraceAndRow is the #205 regression test: a
// measured token class must survive the whole chain from the step_finish event
// to the serialized ledger row. The stub reports every class at distinct values;
// the phase statistics, the flow's copy of them, and the round-tripped row must
// all keep each one, or a class was dropped and the ledger under-states spend.
func TestNoTokenClassDroppedBetweenTraceAndRow(t *testing.T) {
	step := `{"type":"step_finish","part":{"tokens":{"input":11,"output":22,"reasoning":33,"cache":{"read":44,"write":55}}}}`
	script := "#!/bin/sh\nprintf '%s\\n' '" + step + "'\nexit 0\n"
	wt, trace := fakeOpencode(t, script)
	a := &Agent{Name: "probe", Model: "probe-model", Instructions: "probe"}
	stats, err := runPhase(t.TempDir(), wt, a, "task", trace)
	if err != nil {
		t.Fatal(err)
	}
	want := runStats{tokensIn: 11, tokensOut: 22, cacheRead: 44, cacheWrite: 55, reasoning: 33}
	if stats != want {
		t.Fatalf("runPhase dropped a token class: stats = %+v, want %+v", stats, want)
	}

	// Copy the statistics into an outcome the way every flow's Act does, then
	// onto a ledger record the way the supervisor's append does, and round-trip
	// the row through JSON exactly as the ledger persists it: the row is what a
	// person reads, so every class must survive to that shape.
	var out Outcome
	out.addTokens(stats)
	var rec runRecord
	rec.setTokens(out)
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var got runRecord
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("ledger row does not round-trip: %v", err)
	}
	for _, c := range []struct {
		field string
		have  int64
		want  int64
	}{
		{"tokens_in", got.TokensIn, want.tokensIn},
		{"tokens_out", got.TokOut, want.tokensOut},
		{"cache_read", got.CacheRead, want.cacheRead},
		{"cache_write", got.CacheWrite, want.cacheWrite},
		{"reasoning", got.Reasoning, want.reasoning},
	} {
		if c.have != c.want {
			t.Errorf("ledger %s = %d, want %d: a token class was dropped between trace and row",
				c.field, c.have, c.want)
		}
	}
}

// TestRunStatsTokenFieldsHaveLedgerConsumers pins requirement 4 of #205: every
// field on runStats is a measured token class that must reach the ledger row,
// and every class on the row must be fed from the phase statistics. A field
// with no consumer — or a measurement a flow stops copying — is the silent
// under-statement the ledger exists to prevent. Add a class to both sides of
// this table when a new token kind is added.
func TestRunStatsTokenFieldsHaveLedgerConsumers(t *testing.T) {
	st := reflect.TypeOf(runStats{})
	wantFields := map[string]bool{
		"tokensIn": true, "tokensOut": true,
		"cacheRead": true, "cacheWrite": true, "reasoning": true,
	}
	for i := 0; i < st.NumField(); i++ {
		name := st.Field(i).Name
		if !wantFields[name] {
			t.Errorf("runStats field %q has no ledger consumer; add it to addTokens, runRecord, and stats", name)
		}
	}
	if st.NumField() != len(wantFields) {
		t.Errorf("runStats has %d fields, want %d; a measured class is either missing or not carried to the ledger",
			st.NumField(), len(wantFields))
	}
}

func TestHardStopAgentProcessHelper(t *testing.T) {
	heartbeat := os.Getenv("FOREST_HARD_STOP_HEARTBEAT")
	if heartbeat == "" {
		return
	}
	cmd := exec.Command("/bin/sh", "-c", "(while :; do printf x >> '"+heartbeat+"'; sleep 0.05; done) & wait")
	if err := startManagedCommand(cmd); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if body, err := os.ReadFile(heartbeat); err == nil && len(body) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent descendant did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if errs := hardStopRunCommands(); len(errs) != 0 {
		t.Fatalf("hard stop: %v", errs)
	}
	if err := waitRunCommand(cmd); err == nil {
		t.Fatal("hard-stopped agent exited successfully")
	}
}

func TestHardStopKillsAgentDescendants(t *testing.T) {
	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHardStopAgentProcessHelper$")
	cmd.Env = append(os.Environ(), "FOREST_HARD_STOP_HEARTBEAT="+heartbeat)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hard-stop helper: %v: %s", err, out)
	}
	before, err := os.ReadFile(heartbeat)
	if err != nil || len(before) == 0 {
		t.Fatalf("agent descendant produced no heartbeat: %d bytes, %v", len(before), err)
	}
	time.Sleep(300 * time.Millisecond)
	after, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("agent descendant survived hard stop: heartbeat grew from %d to %d bytes", len(before), len(after))
	}
}

// TestRunPhaseTimesOutPastDeclaredBound is the falsifier for #207: a run whose
// agent and descendant produce output past its declared deadline must be
// cancelled as one process group and reported as a timeout.
func TestRunPhaseTimesOutPastDeclaredBound(t *testing.T) {
	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	wt, trace := fakeOpencode(t,
		"#!/bin/sh\n(while :; do printf x >> '"+heartbeat+"'; sleep 0.05; done) &\n"+
			"printf '{\"type\":\"shell\",\"content\":\"sleep\"}\\n'\nwait\n")
	a := &Agent{Name: "probe", Model: "probe-model", Instructions: "probe", DeadlineSeconds: 1}
	start := time.Now()
	_, err := runPhase(t.TempDir(), wt, a, "task", trace)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a run past its declared bound returned no error")
	}
	if !isRunTimeout(err) {
		t.Fatalf("error %v is not a runTimeoutError", err)
	}
	if elapsed > 15*time.Second {
		t.Errorf("run was not cancelled at the bound: returned after %s, but the stub sleeps 30s", elapsed)
	}
	if !strings.Contains(err.Error(), "deadline") {
		t.Errorf("error %q did not name the deadline", err)
	}
	if !strings.Contains(err.Error(), "sleep") {
		t.Errorf("error %q did not name the last trace event", err)
	}
	time.Sleep(100 * time.Millisecond)
	before, err := os.ReadFile(heartbeat)
	if err != nil || len(before) == 0 {
		t.Fatalf("agent descendant produced no heartbeat: %d bytes, %v", len(before), err)
	}
	time.Sleep(300 * time.Millisecond)
	after, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("agent descendant survived cancellation: heartbeat grew from %d to %d bytes", len(before), len(after))
	}
}
