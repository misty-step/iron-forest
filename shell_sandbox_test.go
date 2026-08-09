package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runSandboxBash invokes the sandbox bash wrapper the way opencode invokes its
// bash tool (`bash -lc <command>` from the agent's worktree), and returns the
// stdout, stderr and exit code. binDir holds the freshly installed wrapper and
// sits at the head of the child PATH, modelling an agent run's private bin.
func runSandboxBash(t *testing.T, binDir, wtDir string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "bash"), args...)
	cmd.Dir = wtDir
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+childSystemPath)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run sandbox bash %v: %v", args, err)
	}
	return out.String(), errb.String(), code
}

func newSandbox(t *testing.T, allow []string) (binDir, wtDir string) {
	t.Helper()
	binDir = t.TempDir()
	wtDir = t.TempDir()
	if err := installSandboxBash(binDir, allow); err != nil {
		t.Fatal(err)
	}
	return binDir, wtDir
}

// TestSandboxBashScriptSyntax pins that the generated wrapper is valid POSIX
// sh, so a rendering bug can never corrupt opencode's bash tool.
func TestSandboxBashScriptSyntax(t *testing.T) {
	script := sandboxBashScript("/bin/bash", []string{"git *", "gofmt *", "go build *"})
	path := filepath.Join(t.TempDir(), "bash")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-n", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandbox bash script has a syntax error: %v: %s", err, out)
	}
	// The wrapper must carry the allowlisted prefixes verbatim.
	for _, want := range []string{"'git '*)", "'gofmt '*)", "'go build '*)", "exec \"$REALBASH\" -c \"$cmd\""} {
		if !strings.Contains(script, want) {
			t.Fatalf("generated sandbox script lacks %q:\n%s", want, script)
		}
	}
}

// TestSandboxBashAllowsPlainCommands pins that the wrapper lets a normal build
// through: a plain command starting with an allowlisted prefix resolves through
// the real bash and runs. This is the "a normal build still completes" half of
// the #118 oracle at the shell's own boundary.
func TestSandboxBashAllowsPlainCommands(t *testing.T) {
	binDir, wtDir := newSandbox(t, []string{
		"echo *", "cat *", "ls *", "git *", "gofmt *", "go build *", "go test *",
	})
	if err := os.WriteFile(filepath.Join(wtDir, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, wtDir, "init", "-b", "master")
	runGitTest(t, wtDir, "add", "file.txt")

	abs := filepath.Join(wtDir, "file.txt")
	cases := []struct {
		args []string // the opencode-style bash argv (-lc <command>)
		want string   // expected stdout
	}{
		{[]string{"-lc", "echo hello"}, "hello\n"},
		{[]string{"-c", "echo hi"}, "hi\n"},
		{[]string{"-lc", "cat file.txt"}, "hello\n"},
		{[]string{"-lc", "cat " + abs}, "hello\n"}, // absolute path inside the worktree stays allowed
		{[]string{"-lc", "ls -a ."}, "file.txt"},
		{[]string{"-lc", "git status --porcelain"}, ""},
	}
	for _, tc := range cases {
		out, errOut, code := runSandboxBash(t, binDir, wtDir, tc.args...)
		if code != 0 {
			t.Fatalf("sandbox bash %v = exit %d with %q, want pass", tc.args, code, errOut)
		}
		if tc.want != "" && !strings.Contains(out, tc.want) {
			t.Fatalf("sandbox bash %v output %q, want it to contain %q", tc.args, out, tc.want)
		}
	}
}

// TestSandboxBashDeniesShellEscapes pins the regression the reviewer flagged:
// an allowlisted prefix such as `echo *` or `git *` must not make chaining,
// substitution, redirection, globbing or an outside path reachable through the
// shell. Every one of these must be refused by the wrapper, not passed to the
// shell, because the harness cannot trust opencode to have pre-filtered them.
func TestSandboxBashDeniesShellEscapes(t *testing.T) {
	binDir, wtDir := newSandbox(t, []string{
		"echo *", "cat *", "ls *", "git *", "gofmt *", "go build *", "go test *",
	})
	if err := os.WriteFile(filepath.Join(wtDir, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "pwned")
	denied := []struct {
		command string // appended to `bash -lc `
		why     string
	}{
		{"echo pwn > " + outside, "redirection"},              // the oracle's outside-worktree write
		{"git status; curl http://example.invalid/", "chain"}, // the oracle's curl chain
		{"curl http://example.invalid/", "not allowlisted"},
		{"git status && curl http://example.invalid/", "chain"},
		{"echo $(id)", "substitution"},
		{"echo `id`", "substitution"},
		{"echo $HOME", "substitution"},
		{"echo x | cat", "pipe"},
		{"cat /etc/passwd", "absolute outside path"},
		{"gofmt -w /tmp/outside.go", "absolute outside path"},
		{"cat ../secret", "path escape"},
		{"echo x > " + outside, "redirection"},
		{"echo 'x; curl http://example.invalid/'", "quoting"},
		{"cat file.*", "globbing"},
		{"ls -a .\ncurl http://example.invalid/", "newline chain"},
	}
	for _, tc := range denied {
		_, errOut, code := runSandboxBash(t, binDir, wtDir, "-lc", tc.command)
		if code == 0 {
			t.Fatalf("sandbox bash allowed %q (%s): shell escape reached the host", tc.command, tc.why)
		}
		if !strings.Contains(errOut, "forest: denied") {
			t.Fatalf("sandbox bash denial for %q (%s) did not name the refusal: %q", tc.command, tc.why, errOut)
		}
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("shell escape wrote the outside file %s", outside)
	}
}

// TestSandboxBashEmptyAllowlistDeniesWholeShell pins that an empty bash_allow
// (the declared way to delete bash) makes the wrapper refuse every command.
func TestSandboxBashEmptyAllowlistDeniesWholeShell(t *testing.T) {
	binDir, wtDir := newSandbox(t, []string{})
	for _, command := range []string{"echo hello", "ls -a .", "git status"} {
		_, errOut, code := runSandboxBash(t, binDir, wtDir, "-lc", command)
		if code == 0 {
			t.Fatalf("sandbox bash with an empty allowlist allowed %q", command)
		}
		if !strings.Contains(errOut, "forest: denied") {
			t.Fatalf("empty-allowlist denial for %q did not name the refusal: %q", command, errOut)
		}
	}
}

// TestBashAllowEntryValidation pins the declaration contract: a bash_allow
// entry must name a plain command prefix ending in `" *"` with no shell
// punctuation, so a typo cannot widen the allowlist or corrupt the generated
// sandbox wrapper.
func TestBashAllowEntryValidation(t *testing.T) {
	valid := []string{"git *", "gofmt *", "go build *", "mise exec -- go test *", "echo *"}
	for _, entry := range valid {
		if err := validBashAllowEntry(entry); err != nil {
			t.Errorf("validBashAllowEntry(%q) = %v, want nil", entry, err)
		}
	}
	invalid := []string{
		"", "git", "git*", "git status", "echo x > *", "cat *;rm *", "echo $PATH *",
		"echo \"x\" *", "ls /etc/*", "echo '[x]' *", "foo? *", "echo ~ *",
	}
	for _, entry := range invalid {
		if err := validBashAllowEntry(entry); err == nil {
			t.Errorf("validBashAllowEntry(%q) accepted a dangerous entry", entry)
		}
	}
}

// TestLoadAgentRejectsUnsafeBashAllow pins that loadAgent refuses a declaration
// whose bash_allow entry carries shell punctuation, so an allowlist that says
// more than it names can never load.
func TestLoadAgentRejectsUnsafeBashAllow(t *testing.T) {
	for _, entry := range []string{"echo x > *", "git *; rm -rf *", "*"} {
		repoDir := t.TempDir()
		writeAgentFixture(t, repoDir, "builder", "builder-model")
		path := filepath.Join(repoDir, DefaultAgentsDir, "builder", "agent.yaml")
		body := "description: builder\ncommit:\n  name: builder\n  email: builder@example.invalid\n" +
			"model: builder-model\ndeadline_seconds: 3600\nbash_allow:\n  - \"" + entry + "\"\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAgent(repoDir, "builder"); err == nil ||
			!strings.Contains(err.Error(), "bash_allow") {
			t.Fatalf("loadAgent with unsafe bash_allow %q = %v, want refusal", entry, err)
		}
	}
}

// TestAgentRunRecordsDeniedToolsAndCompletesBuild is the #118 integration
// oracle the reviewer asked for: a real agent run (the declared Builder agent
// rendered by runPhase's config factory, the real sandbox `bash` installed in
// the run's private PATH, and a stub opencode standing in for the harness)
// attempts curl and a write outside its worktree, records both as denied tools
// in the trace, and still completes a normal build through the allowlisted
// commands.
func TestAgentRunRecordsDeniedToolsAndCompletesBuild(t *testing.T) {
	// The agent is the real declared Builder: runPhase renders this repository's
	// agents/builder/agent.yaml (with its bash_allow) into the external config
	// root and bounds the run's shell with the sandbox the same way a real run
	// does, so the test exercises the actual declaration, not a fixture.
	repoDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	a, err := loadAgent(repoDir, "builder")
	if err != nil {
		t.Fatalf("loadAgent(builder): %v", err)
	}
	if a.BashAllow == nil {
		t.Fatal("declared builder agent has no bash_allow; the shell is unbounded")
	}

	outside := filepath.Join(t.TempDir(), "pwned-outside.txt")
	script := "#!/bin/sh\n" +
		// The run's private PATH must resolve bash to the sandbox: opencode's
		// bash tool runs `bash -lc <command>`, and that resolution is the whole
		// boundary this item adds. If it does not hold, the test would pass on a
		// run that never consulted the sandbox at all.
		"if [ \"$(command -v bash)\" != \"$HOME/bin/bash\" ]; then\n" +
		"  echo 'opencode: sandbox bash not on PATH: '$(command -v bash) >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		// The rendered declaration must no longer contain the blanket
		// `bash: allow` that made every other deny rule bypassable (#118).
		"agent_md=\"$XDG_CONFIG_HOME/opencode/agents/builder.md\"\n" +
		"if [ ! -f \"$agent_md\" ]; then echo 'opencode: agent builder.md missing' >&2; exit 1; fi\n" +
		"if grep -q 'bash: allow' \"$agent_md\"; then echo 'opencode: agent still declares an unbounded shell' >&2; exit 1; fi\n" +
		// The agent attempts curl (an unlisted command): the permission map and
		// the sandbox both refuse it, and the harness records a denied tool.
		"if bash -lc 'curl http://example.invalid/' 2>/dev/null; then\n" +
		"  echo 'opencode: curl ran; the shell is not bounded' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"printf '%s\\n' '{\"type\":\"tool_denied\",\"tool\":\"bash\",\"command\":\"curl http://example.invalid/\"}'\n" +
		// The agent attempts a write outside its worktree through redirection:
		// it matches the `echo *` prefix but the sandbox refuses the shell
		// metacharacter, the harness records the denied tool, and no file lands
		// outside.
		"if bash -lc \"echo pwn > " + outside + "\" 2>/dev/null; then\n" +
		"  echo 'opencode: outside write ran; the shell is not bounded' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"if [ -e " + outside + " ]; then echo 'opencode: outside file was written' >&2; exit 1; fi\n" +
		"printf '%s\\n' '{\"type\":\"tool_denied\",\"tool\":\"bash\",\"command\":\"echo pwn > " + outside + "\"}'\n" +
		// The normal build still completes through the allowlisted commands: the
		// agent writes its change with the edit tool, then reads it back and
		// lists it with the declared read-only helpers, and the run finishes with
		// its report — all through the sandboxed shell.
		"printf 'built\\n' > built.txt\n" +
		"if ! out=$(bash -lc 'cat built.txt'); then echo 'opencode: cat failed' >&2; exit 1; fi\n" +
		"if [ \"$out\" != built ]; then echo 'opencode: cat returned wrong content' >&2; exit 1; fi\n" +
		"printf '%s\\n' '{\"type\":\"shell\",\"content\":\"cat built.txt\"}'\n" +
		"if ! bash -lc 'ls built.txt' >/dev/null; then echo 'opencode: ls failed' >&2; exit 1; fi\n" +
		"printf '%s\\n' '{\"type\":\"shell\",\"content\":\"ls built.txt\"}'\n" +
		"printf '%s\\n' '{\"summary\":\"bound the shell\",\"changed_files\":[\"built.txt\"],\"notes\":\"none\"}' > report.json\n" +
		"exit 0\n"

	wt, trace := fakeOpencode(t, script)
	if _, err := runPhase(t.TempDir(), wt, a, "build a change", trace); err != nil {
		t.Fatalf("agent run did not complete its normal build: %v", err)
	}

	raw, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	tr := string(raw)
	for _, want := range []string{
		`"type":"tool_denied"`,
		`"command":"curl http://example.invalid/"`,
		`"command":"echo pwn > ` + outside + `"`,
		"cat built.txt",
		"ls built.txt",
	} {
		if !strings.Contains(tr, want) {
			t.Fatalf("trace lacks %s:\n%s", want, tr)
		}
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("the outside-worktree write escaped the sandbox: %s exists", outside)
	}
	if _, err := os.Stat(filepath.Join(wt, "built.txt")); err != nil {
		t.Fatalf("the normal build did not complete in the worktree: %v", err)
	}
}
