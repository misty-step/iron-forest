package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The command grammar: the envelope, flag parsing, dispatch, usage, and selfcheck.

// decodeEnvelope runs one read-surface command with --json and returns the single
// envelope it must emit.
func decodeEnvelope(t *testing.T, args ...string) (int, cliEnvelope, string) {
	t.Helper()
	code, stdout, stderr := captureCLIOutput(t, func() int { return runSurfaceCommand(args) })
	var envelope cliEnvelope
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("%v did not emit one envelope: %v (stdout=%q stderr=%q)", args, err, stdout, stderr)
	}
	if envelope.Schema != "forest.cli.v2" {
		t.Fatalf("schema=%q, want forest.cli.v2", envelope.Schema)
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

// decodePayload decodes envelope data into the command's published payload type,
// so a test asserts against the same shape an integrator receives.
func decodePayload(t *testing.T, envelope cliEnvelope, payload any) {
	t.Helper()
	encoded, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, payload); err != nil {
		t.Fatalf("data does not match %T: %v (%s)", payload, err, encoded)
	}
}

// writeTestConfig writes a configuration declaring the named agents.
func writeTestConfig(t *testing.T, root string, agents ...string) {
	t.Helper()
	config := "repo: owner/name\nagents:\n"
	for _, agent := range agents {
		config += "  " + agent + ":\n    poll: \"exit 1\"\n    interval: 1\n"
	}
	config += "checks:\n  - name: test\n    run: \"true\"\n"
	if err := os.WriteFile(configPath(root), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTriggerState(t *testing.T, root, state string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forestPath(root, "triggers.json"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeLedgerRows(t *testing.T, root string, rows ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledgerPath(root), []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	for _, key := range []string{"poll", "interval"} {
		if _, ok := builder[key]; !ok {
			t.Fatalf("agent payload missing %q: %v", key, builder)
		}
	}
	if _, present := builder["timeout"]; present {
		t.Fatalf("agent payload retains removed timeout: %v", builder)
	}
	if _, leaked := builder["Poll"]; leaked {
		t.Fatalf("agent payload leaks Go field names: %v", builder)
	}

	_, declaration, _ := decodeEnvelope(t, "declaration", "show", "builder", "--json", "--root", root)
	declarationKeys := payloadKeys(t, declaration)
	for _, key := range []string{"name", "model", "tools", "thinking", "system_prompt", "task_prompt", "skills"} {
		if _, ok := declarationKeys[key]; !ok {
			t.Fatalf("declaration payload missing %q: %v", key, declarationKeys)
		}
	}
	if skills, ok := declarationKeys["skills"].([]any); !ok || len(skills) != 0 {
		t.Fatalf("declaration skills=%v, want []", declarationKeys["skills"])
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

func TestCLISelfcheckEmitsEnvelope(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	writeTestDeclaration(t, root, "builder")
	bin := t.TempDir()
	for _, name := range []string{"git", "gh", "pi"} {
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

func TestCLIVersionReportsBuildInfo(t *testing.T) {
	restore := overrideVersionGlobals(t, "abc123", "2026-08-21T20:00:00Z", "true")
	defer restore()
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")

	code, stdout, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"version", "--root", root})
	})
	if code != exitOK {
		t.Fatalf("version code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	if stderr != "" {
		t.Fatalf("version stderr=%q, want empty", stderr)
	}
	want := "build_sha: abc123\ncommit_time: 2026-08-21T20:00:00Z\ndirty: true\n"
	if stdout != want {
		t.Fatalf("version stdout=%q, want %q", stdout, want)
	}
}

func TestCLIVersionEnvelopeUsesSnakeCaseKeys(t *testing.T) {
	restore := overrideVersionGlobals(t, "def456", "2026-08-22T00:00:00Z", "false")
	defer restore()
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")

	code, envelope, stderr := decodeEnvelope(t, "version", "--json", "--root", root)
	if code != exitOK || stderr != "" {
		t.Fatalf("version code=%d stderr=%q", code, stderr)
	}
	encoded, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var payload versionPayload
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("data does not match versionPayload: %v (%s)", err, encoded)
	}
	if payload.BuildSHA != "def456" || payload.CommitTime != "2026-08-22T00:00:00Z" || payload.Dirty {
		t.Fatalf("version payload=%+v, want injected values", payload)
	}
}

func overrideVersionGlobals(t *testing.T, sha, commitTime, dirty string) func() {
	t.Helper()
	oldSHA, oldTime, oldDirty := buildSHA, buildTime, buildDirty
	buildSHA, buildTime, buildDirty = sha, commitTime, dirty
	return func() { buildSHA, buildTime, buildDirty = oldSHA, oldTime, oldDirty }
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

// A value-taking flag must not consume a following flag token as its value, and
// the refusal belongs in the --json envelope when the caller asked for it.
func TestCLIValueFlagsRejectFollowingFlag(t *testing.T) {
	cases := [][]string{
		{"status", "--root", "--json"},
		{"config", "show", "-C", "--json"},
		{"run", "list", "--after", "--json"},
		{"run", "list", "--limit", "--json"},
		{"publish", "review-request", "builder", "branch", "payload", "--rejected", "--json"},
		{"run", "list", "--agent", "--json"},
		{"run", "list", "--exit", "--json"},
		{"run", "list", "--since", "--json"},
	}
	for _, args := range cases {
		code, envelope, _ := decodeEnvelope(t, args...)
		if code != exitInvalidArg {
			t.Fatalf("%v code=%d, want %d", args, code, exitInvalidArg)
		}
		if envelope.Error == nil || !strings.Contains(*envelope.Error, " requires a value") {
			t.Fatalf("%v error=%v, want a missing-value refusal", args, envelope.Error)
		}
	}
}

// A value-taking flag with no following argument is a missing value, never an
// empty value or a late command failure.
func TestCLIValueFlagsRejectAtEndOfArgs(t *testing.T) {
	cases := [][]string{
		{"status", "--root"},
		{"config", "show", "-C"},
		{"run", "list", "--after"},
		{"run", "list", "--limit"},
		{"publish", "review-request", "builder", "branch", "payload", "--rejected"},
		{"run", "list", "--agent"},
		{"run", "list", "--exit"},
		{"run", "list", "--since"},
	}
	for _, args := range cases {
		code, _, stderr := captureCLIOutput(t, func() int { return runSurfaceCommand(args) })
		if code != exitInvalidArg {
			t.Fatalf("%v code=%d, want %d (stderr=%q)", args, code, exitInvalidArg, stderr)
		}
		if !strings.Contains(stderr, " requires a value") {
			t.Fatalf("%v stderr=%q, want a missing-value refusal", args, stderr)
		}
	}
}

// selfcheck publishes the paths it resolved, not a constant list of names.
func TestCLISelfcheckPublishesResolvedToolPaths(t *testing.T) {
	t.Setenv("FOREST_DEFAULTS", "")
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	writeTestDeclaration(t, root, "builder")
	bin := t.TempDir()
	for _, name := range []string{"git", "gh", "pi"} {
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
	if payload.DefaultsSource != "" || payload.Defaults.Model != "" {
		t.Fatalf("absent defaults leaked into selfcheck: %#v", payload)
	}
	defaultsPath := filepath.Join(root, "forest.defaults.yaml")
	if err := os.WriteFile(defaultsPath, []byte("model: host/model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, envelope, _ = decodeEnvelope(t, "selfcheck", "--json", "--root", root)
	encoded, err = json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.DefaultsSource != defaultsPath || payload.Defaults.Model != "host/model" {
		t.Fatalf("selfcheck defaults=%#v source=%q", payload.Defaults, payload.DefaultsSource)
	}
}

// Reading a directory that holds no configuration must be an error. Reporting
// zero violations there is a clean bill of health for a forest that is absent.
func TestCLIEveryCommandRefusesADirectoryThatIsNotAForest(t *testing.T) {
	root := t.TempDir()
	for _, command := range cliCommands() {
		args := strings.Split(command.phrase, " ")
		for range command.args {
			args = append(args, "operand")
		}
		args = append(args, "--root", root)
		code, stdout, stderr := captureCLIOutput(t, func() int { return runSurfaceCommand(args) })
		if code != exitError {
			t.Fatalf("%s code=%d, want %d (stdout=%q stderr=%q)", command.phrase, code, exitError, stdout, stderr)
		}
		if !strings.Contains(stderr, "not an Iron Forest checkout") {
			t.Fatalf("%s stderr=%q, want it to name the missing checkout", command.phrase, stderr)
		}
	}
}

// A group name is incomplete, not unknown: the answer names its subcommands.
func TestCLIGroupNameReportsItsSubcommands(t *testing.T) {
	for group, want := range map[string]string{
		"run":         "list, show, cancel, logs",
		"trigger":     "list, show, reset",
		"declaration": "list, show",
		"audit":       "show, log",
		"publish":     "review-request",
	} {
		_, _, stderr := captureCLIOutput(t, func() int { return runSurfaceCommand([]string{group}) })
		if !strings.Contains(stderr, group+" requires a subcommand: "+want) {
			t.Fatalf("%s stderr=%q, want the subcommand list %q", group, stderr, want)
		}
	}
}

// The envelope is chosen by the caller's intent to use it, so a malformed --json
// is still reported inside one.
func TestCLIMalformedJSONFlagStillEmitsAnEnvelope(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	code, envelope, stderr := decodeEnvelope(t, "status", "--json=false", "--root", root)
	if code != exitInvalidArg {
		t.Fatalf("code=%d, want %d (stderr=%q)", code, exitInvalidArg, stderr)
	}
	if envelope.Error == nil || !strings.Contains(*envelope.Error, "does not take a value") {
		t.Fatalf("envelope error=%v, want the flag refusal", envelope.Error)
	}
}

// Asking for usage is not a usage error.
func TestCLIHelpSucceeds(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		code, _, stderr := captureCLIOutput(t, func() int { return runCLI(args) })
		if code != exitOK {
			t.Fatalf("%v code=%d, want %d", args, code, exitOK)
		}
		if !strings.Contains(stderr, "usage: forest") {
			t.Fatalf("%v printed no usage", args)
		}
	}
}
