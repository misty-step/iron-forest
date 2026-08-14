package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Runner's dispatch: invocation, identity, usage accounting, and cleanup.

// runnerPrivateRefSetup is the per-Run private note publication a stub harness
// performs, so a test exercises the same ref layout a real Run creates.
const runnerPrivateRefSetup = `revision=$(git rev-parse HEAD)
git update-ref "refs/notes/forest/private/$FOREST_RUN_ID/$ROLE/$NOTE_KIND/$revision/publication" "$revision"
git update-ref "refs/notes/forest/private/$FOREST_RUN_ID/$ROLE/$NOTE_KIND/$revision/base" "$revision"
`

func TestRunnerWorktreeHarnessAndLedger(t *testing.T) {
	for _, test := range []struct {
		role, name, email, noteKind string
	}{
		{role: "builder", name: "Iron Forest Builder", email: "builder@forest.invalid", noteKind: "review-request"},
		{role: "verifier", name: "Iron Forest Verifier", email: "verifier@forest.invalid", noteKind: "checks"},
		{role: "fixer", name: "Iron Forest Fixer", email: "fixer@forest.invalid", noteKind: "review-request"},
	} {
		t.Run(test.role, func(t *testing.T) {
			root, _ := testClone(t)
			state := t.TempDir()
			omp := filepath.Join(state, "omp")
			argsFile := filepath.Join(state, "args")
			identityFile := filepath.Join(state, "identity")
			t.Setenv("ARGS_FILE", argsFile)
			t.Setenv("IDENTITY_FILE", identityFile)
			t.Setenv("ROLE", test.role)
			t.Setenv("NOTE_KIND", test.noteKind)
			canonical := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			runGitDir(t, root, "update-ref", "refs/notes/forest/"+test.noteKind, canonical)
			t.Setenv("GIT_AUTHOR_NAME", "Wrong")
			t.Setenv("GIT_AUTHOR_EMAIL", "wrong@example.invalid")
			t.Setenv("GIT_COMMITTER_NAME", "Wrong")
			t.Setenv("GIT_COMMITTER_EMAIL", "wrong@example.invalid")
			t.Setenv("FOREST_RUN_ID", "wrong-run")
			script := `#!/bin/sh
set -eu
printf '%s\n' "$@" > "$ARGS_FILE"
printf identity > identity.txt
git add identity.txt
git commit -m identity >/dev/null
git log -1 --format='%an%n%ae%n%cn%n%ce' > "$IDENTITY_FILE"
` + runnerPrivateRefSetup + `printf '%s\n' '{"type":"message_end","message":{"usage":{"input":2,"output":3,"cacheRead":5,"cacheWrite":7}}}'
printf '%s\n' '{"type":"turn_end","message":{"usage":{"input":2,"output":3,"cacheRead":5,"cacheWrite":7}}}'
printf '%s\n' '{"type":"turn_end","message":{"usage":{"input":11,"output":13,"cacheRead":17,"cacheWrite":19}}}'
`
			if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			runner := NewRunner(root)
			runner.PiPath = omp
			record, err := runner.Run(context.Background(), Declaration{
				Name:         test.role,
				Model:        "local",
				Tools:        StringList{"read", "bash"},
				SystemPrompt: "system",
				TaskPrompt:   "Reply",
				SkillPaths:   []string{"agents/_shared/skills", "agents/" + test.role + "/skills"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if record.Exit != 0 || record.TokensIn != 13 || record.TokensOut != 16 || record.CacheRead != 22 || record.CacheWrite != 26 {
				t.Fatalf("record=%#v", record)
			}
			identityBytes, err := os.ReadFile(identityFile)
			if err != nil {
				t.Fatal(err)
			}
			identity := strings.Split(strings.TrimSpace(string(identityBytes)), "\n")
			if len(identity) != 4 || identity[0] != test.name || identity[1] != test.email || identity[2] != test.name || identity[3] != test.email {
				t.Fatalf("Git actor=%q, want author and committer %s <%s>", identity, test.name, test.email)
			}
			config := string(runGitDir(t, root, "config", "--get-regexp", `^user\.(name|email)$`))
			if !strings.Contains(config, "user.name Builder\n") || !strings.Contains(config, "user.email builder@forest.invalid\n") {
				t.Fatalf("root Git actor changed:\n%s", config)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{
				"--mode\njson", "--model\nlocal", "--tools\nread,bash",
				"--system-prompt\nsystem", "Reply", "--approve",
				"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes",
				"--session-id\n" + record.RunID,
				"--skill\nagents/_shared/skills", "--skill\nagents/" + test.role + "/skills",
			} {
				if !strings.Contains(string(args), value) {
					t.Fatalf("harness args missing %q:\n%s", value, args)
				}
			}
			assertRunnerPrivateRefsClean(t, root, record.RunID, canonical, test.noteKind)
			rows, err := ReadLedger(root)
			if err != nil || len(rows) != 1 {
				t.Fatalf("ledger rows=%v err=%v", rows, err)
			}
			logInfo, err := os.Stat(forestPath(root, "runs", record.RunID+".log"))
			if err != nil || logInfo.Mode().Perm() != 0o600 {
				t.Fatalf("run log mode=%v info=%v, want 0600", err, logInfo)
			}
			entries, err := os.ReadDir(forestPath(root, "worktrees"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("worktrees not cleaned: entries=%v err=%v", entries, err)
			}
		})
	}
}

func assertRunnerPrivateRefsClean(t *testing.T, root, runID, revision, noteKind string, remaining ...string) {
	t.Helper()
	refs := strings.Fields(string(runGitDir(t, root, "for-each-ref", "--format=%(refname)", "refs/notes/forest/private/")))
	if got, want := strings.Join(refs, "\n"), strings.Join(remaining, "\n"); got != want {
		t.Fatalf("private refs=%v, want %v after cleaning run %s", refs, remaining, runID)
	}
	canonical := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "refs/notes/forest/"+noteKind)))
	if canonical != revision {
		t.Fatalf("canonical %s ref=%s, want %s", noteKind, canonical, revision)
	}
}

func TestRunnerCleansPrivateRefsAfterAgentFailureAndCancellation(t *testing.T) {
	for name, cancelRun := range map[string]bool{"failure": false, "cancellation": true} {
		t.Run(name, func(t *testing.T) {
			root, _ := testClone(t)
			omp := filepath.Join(t.TempDir(), "omp")
			t.Setenv("ROLE", "builder")
			t.Setenv("NOTE_KIND", "review-request")
			canonical := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
			runGitDir(t, root, "update-ref", "refs/notes/forest/review-request", canonical)
			behavior := "exit 7\n"
			wantExit := 7
			if cancelRun {
				behavior = "while :; do /bin/sleep 1; done\n"
				wantExit = 130
			}
			script := "#!/bin/sh\nset -eu\n" + runnerPrivateRefSetup +
				"printf '%s\\n' '{\"usage\":{\"input\":1}}'\n" + behavior
			if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			runner := NewRunner(root)
			runner.PiPath = omp
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelRun {
				timer := time.AfterFunc(2*time.Second, cancel)
				defer timer.Stop()
			}
			record, err := runner.Run(ctx, Declaration{Name: "builder", Model: "local", TaskPrompt: "x"})
			if record.Exit != wantExit || err == nil {
				t.Fatalf("record=%#v err=%v, want exit %d", record, err, wantExit)
			}
			if cancelRun && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation err=%v", err)
			}
			assertRunnerPrivateRefsClean(t, root, record.RunID, canonical, "review-request")
		})
	}
}

// TestRunnerRejectsChangedDeclarationBundle pins #144: a run executes only the
// declared agent files, unchanged since they were loaded. Editing agent.md or
// task.md between load and exec aborts the run with a digest mismatch, records
// a nonzero-exit Ledger row, and refuses to start Pi. An unchanged declaration
// dispatches normally and records its digest on the Ledger row.
func TestRunnerRejectsChangedDeclarationBundle(t *testing.T) {
	root, _ := testClone(t)
	writeTestDeclaration(t, root, "builder")
	runGitDir(t, root, "add", "agents")
	runGitDir(t, root, "commit", "-m", "declaration")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/master")
	omp := filepath.Join(t.TempDir(), "omp")
	marker := filepath.Join(t.TempDir(), "invoked")
	t.Setenv("MARKER_FILE", marker)
	script := "#!/bin/sh\nset -eu\nprintf ran > \"$MARKER_FILE\"\nprintf '%s\\n' '{\"type\":\"message_end\",\"message\":{\"usage\":{\"input\":1,\"output\":2}}}'\nprintf '%s\\n' '{\"type\":\"turn_end\",\"message\":{\"usage\":{\"input\":1,\"output\":2}}}'\n"
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.PiPath = omp

	// Load the declaration exactly as the Kernel does; this records the digest.
	declaration, err := loadDeclaration(root, "builder")
	if err != nil {
		t.Fatal(err)
	}
	if declaration.DefinitionSHA == "" {
		t.Fatal("loadDeclaration recorded no definition digest")
	}

	// Unchanged since load, the run dispatches normally and records its digest.
	record, err := runner.Run(context.Background(), declaration, 10)
	if err != nil {
		t.Fatalf("unchanged bundle refused: %v", err)
	}
	if record.Exit != 0 || record.DefinitionSHA != declaration.DefinitionSHA {
		t.Fatalf("record=%#v", record)
	}
	if body, readErr := os.ReadFile(marker); readErr != nil || string(body) != "ran" {
		t.Fatalf("Pi did not start for an unchanged bundle: %q err=%v", body, readErr)
	}
	rows, ledgerErr := ReadLedger(root)
	if ledgerErr != nil || len(rows) != 1 || rows[0].Exit != 0 || rows[0].DefinitionSHA != declaration.DefinitionSHA {
		t.Fatalf("ledger rows=%v err=%v, want one passing row with the digest", rows, ledgerErr)
	}

	// A host Write changes task.md after load but before dispatch; Pi must not
	// start and the Ledger row must record a nonzero exit.
	if err := os.WriteFile(filepath.Join(root, "agents", "builder", "task.md"), []byte("do the work differently\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	record, err = runner.Run(context.Background(), declaration, 10)
	if err == nil || !strings.Contains(err.Error(), "bundle changed since load") {
		t.Fatalf("changed bundle record=%#v err=%v, want digest mismatch", record, err)
	}
	if strings.Contains(err.Error(), "parse harness usage") {
		t.Fatalf("mismatch error leaked usage parse: %v", err)
	}
	if record.Exit == 0 {
		t.Fatalf("changed bundle exit=%d, want nonzero", record.Exit)
	}

	if body, readErr := os.ReadFile(marker); !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("Pi started despite the changed bundle: %q err=%v", body, readErr)
	}
	rows, ledgerErr = ReadLedger(root)
	if ledgerErr != nil || len(rows) != 2 || rows[1].Exit == 0 || rows[1].DefinitionSHA != "" {
		t.Fatalf("ledger rows=%v err=%v, want a second nonzero-exit row without a verified digest", rows, ledgerErr)
	}

}

func TestRunnerRejectsInvalidUsageBeforeLedgerAppend(t *testing.T) {
	root, _ := testClone(t)
	omp := filepath.Join(t.TempDir(), "omp")
	if err := os.WriteFile(omp, []byte("#!/bin/sh\nprintf '%s\\n' '{\"usage\":{\"input\":-1}}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.PiPath = omp
	record, err := runner.Run(context.Background(), Declaration{Name: "builder", Model: "local", TaskPrompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "parse harness usage") || !strings.Contains(err.Error(), "nonnegative") {
		t.Fatalf("invalid usage record=%#v err=%v", record, err)
	}
	if record.Exit != 1 {
		t.Fatalf("invalid usage exit=%d, want 1", record.Exit)
	}
	rows, ledgerErr := ReadLedger(root)
	if ledgerErr != nil || len(rows) != 1 || rows[0].Exit != 1 {
		t.Fatalf("invalid usage ledger=%v err=%v, want one failing row", rows, ledgerErr)
	}
	if rows[0].TokensIn != 0 || rows[0].TokensOut != 0 || rows[0].CacheRead != 0 || rows[0].CacheWrite != 0 || rows[0].Reasoning != 0 {
		t.Fatalf("invalid usage leaked into ledger: %#v", rows[0])
	}
	if _, statErr := os.Stat(forestPath(root, "worktrees", record.RunID)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid usage worktree survived: %v", statErr)
	}
}

func TestRunnerRejectsTerminalPiErrorDespiteZeroProcessExit(t *testing.T) {
	root, _ := testClone(t)
	state := t.TempDir()
	pi := filepath.Join(state, "pi")
	eventPath := filepath.Join(state, "agent-end.jsonl")
	event := []byte(`{"type":"agent_end","messages":[{"role":"assistant","content":"`)
	event = append(event, bytes.Repeat([]byte("x"), 2*runLogHalfLimit+4096)...)
	event = append(event, []byte(`","usage":{"input":0,"output":0},"stopReason":"error","errorMessage":"401: missing authentication"}],"willRetry":false}`+"\n")...)
	if err := os.WriteFile(eventPath, event, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_EVENT", eventPath)
	script := `#!/bin/sh
printf '%s\n' '{"type":"turn_end","message":{"usage":{"input":1,"output":2}}}'
cat "$PI_EVENT"
exit 0
`
	if err := os.WriteFile(pi, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.PiPath = pi
	record, err := runner.Run(context.Background(), Declaration{Name: "builder", Model: "local", TaskPrompt: "x"})
	if err == nil || record.Exit != 1 || !strings.Contains(err.Error(), "pi agent ended with error") {
		t.Fatalf("terminal Pi error record=%#v err=%v, want failing Run", record, err)
	}
	if record.TokensIn != 1 || record.TokensOut != 2 {
		t.Fatalf("terminal Pi error usage=%#v, want earlier valid usage", record)
	}
	rows, ledgerErr := ReadLedger(root)
	if ledgerErr != nil || len(rows) != 1 || rows[0].RunID != record.RunID || rows[0].Exit != 1 {
		t.Fatalf("terminal Pi error ledger=%v err=%v, want one failing row", rows, ledgerErr)
	}
}

func TestRunnerRejectsTerminalPiErrorMessageWithoutStopReason(t *testing.T) {
	root, _ := testClone(t)
	pi := filepath.Join(t.TempDir(), "pi")
	script := `#!/bin/sh
printf '%s\n' '{"type":"turn_end","message":{"usage":{"input":1,"output":1}}}'
printf '%s\n' '{"type":"agent_end","messages":[{"role":"assistant","errorMessage":"provider unavailable"}],"willRetry":false}'
exit 0
`
	if err := os.WriteFile(pi, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.PiPath = pi
	record, err := runner.Run(context.Background(), Declaration{Name: "builder", Model: "local", TaskPrompt: "x"})
	if err == nil || record.Exit != 1 || !strings.Contains(err.Error(), "pi agent ended with error") {
		t.Fatalf("terminal Pi errorMessage record=%#v err=%v, want failing Run", record, err)
	}
}

func TestRunnerAcceptsSuccessfulAssistantAfterRetriedPiError(t *testing.T) {
	root, _ := testClone(t)
	pi := filepath.Join(t.TempDir(), "pi")
	script := `#!/bin/sh
printf '%s\n' '{"type":"turn_end","message":{"usage":{"input":3,"output":5}}}'
printf '%s\n' '{"type":"agent_end","messages":[{"role":"assistant","stopReason":"error","errorMessage":"transient"},{"role":"user","content":"retry"},{"role":"assistant","stopReason":"stop"}],"willRetry":false}'
`
	if err := os.WriteFile(pi, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.PiPath = pi
	record, err := runner.Run(context.Background(), Declaration{Name: "builder", Model: "local", TaskPrompt: "x"})
	if err != nil || record.Exit != 0 || record.TokensIn != 3 || record.TokensOut != 5 {
		t.Fatalf("successful Pi retry record=%#v err=%v", record, err)
	}
}

func TestRunnerRejectsUsageWithoutRecognizedAlias(t *testing.T) {
	for _, test := range []struct {
		name  string
		usage string
	}{
		{name: "empty", usage: `{}`},
		{name: "unknown only", usage: `{"future_tokens":1}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, _ := testClone(t)
			omp := filepath.Join(t.TempDir(), "omp")
			script := "#!/bin/sh\nprintf '%s\\n' '{\"usage\":" + test.usage + "}'\n"
			if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			runner := NewRunner(root)
			runner.PiPath = omp
			record, err := runner.Run(context.Background(), Declaration{Name: "builder", Model: "local", TaskPrompt: "x"})
			if err == nil || record.Exit != 1 || !strings.Contains(err.Error(), "parse harness usage") {
				t.Fatalf("drifted usage record=%#v err=%v", record, err)
			}
			rows, ledgerErr := ReadLedger(root)
			if ledgerErr != nil || len(rows) != 1 || rows[0].Exit != 1 {
				t.Fatalf("drifted usage ledger=%v err=%v, want one failing row", rows, ledgerErr)
			}
		})
	}
}

func TestPiUsageParser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pi.jsonl")
	data := `{"message":{"usage":{"input":3,"output":5,"cacheRead":7,"cacheWrite":11,"reasoning":13}}}
{"message":{"usage":{"input":17,"output":19,"cacheRead":23,"cacheWrite":29,"reasoning":31}}}
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	usage, err := parseAgentUsage(path)
	if err != nil {
		t.Fatal(err)
	}
	if usage.TokensIn != 17 || usage.TokensOut != 19 || usage.CacheRead != 23 || usage.CacheWrite != 29 || usage.Reasoning != 31 {
		t.Fatalf("usage=%#v", usage)
	}
}

func TestParseOMPUsageAggregatesTurnsAfterLargeRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "omp.jsonl")
	data := `{"type":"message_update","payload":"` + strings.Repeat("x", 128*1024) + `"}` + "\n" +
		`{"type":"message_end","message":{"usage":{"input":2,"output":3,"cacheRead":5,"cacheWrite":7}}}` + "\n" +
		`{"type":"turn_end","message":{"usage":{"input":2,"output":3,"cacheRead":5,"cacheWrite":7}}}` + "\n" +
		`{"type":"turn_end","message":{"usage":{"input":11,"output":13,"cacheRead":17,"cacheWrite":19,"reasoningTokens":23}}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	usage, err := parseAgentUsage(path)
	if err != nil {
		t.Fatal(err)
	}
	if usage != (Usage{TokensIn: 13, TokensOut: 16, CacheRead: 22, CacheWrite: 26, Reasoning: 23}) {
		t.Fatalf("usage=%#v", usage)
	}
}

func TestParseOMPUsagePreservesExactIntegers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "omp.jsonl")
	data := `{"usage":{"input":9007199254740993,"output":9223372036854775807,"cacheRead":0}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	usage, err := parseAgentUsage(path)
	if err != nil {
		t.Fatal(err)
	}
	if usage.TokensIn != 9007199254740993 || usage.TokensOut != 9223372036854775807 {
		t.Fatalf("usage=%#v", usage)
	}
}

func TestParseOMPUsageRejectsInvalidNumbers(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "fraction", value: "1.5"},
		{name: "negative", value: "-1"},
		{name: "int64 overflow", value: "9223372036854775808"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "omp.jsonl")
			data := `{"usage":{"input":` + test.value + `}}`
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
			if usage, err := parseAgentUsage(path); err == nil {
				t.Fatalf("accepted usage %#v", usage)
			}
		})
	}
}

func TestParseOMPUsageRejectsEveryAggregateOverflow(t *testing.T) {
	for _, key := range []string{"input", "output", "cacheRead", "cacheWrite", "reasoning"} {
		t.Run(key, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "omp.jsonl")
			data := `{"type":"turn_end","message":{"usage":{"` + key + `":9223372036854775807}}}` + "\n" +
				`{"type":"turn_end","message":{"usage":{"` + key + `":1}}}`
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
			if usage, err := parseAgentUsage(path); err == nil {
				t.Fatalf("accepted overflowing %s usage %#v", key, usage)
			}
		})
	}
}

func TestParseOMPUsageAllowsEveryAggregateMaxPlusZero(t *testing.T) {
	const max = int64(^uint64(0) >> 1)
	for _, key := range []string{"input", "output", "cacheRead", "cacheWrite", "reasoning"} {
		t.Run(key, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "omp.jsonl")
			data := `{"type":"turn_end","message":{"usage":{"` + key + `":9223372036854775807}}}` + "\n" +
				`{"type":"turn_end","message":{"usage":{"` + key + `":0}}}`
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
			usage, err := parseAgentUsage(path)
			if err != nil {
				t.Fatal(err)
			}
			var got int64
			switch key {
			case "input":
				got = usage.TokensIn
			case "output":
				got = usage.TokensOut
			case "cacheRead":
				got = usage.CacheRead
			case "cacheWrite":
				got = usage.CacheWrite
			case "reasoning":
				got = usage.Reasoning
			}
			if got != max {
				t.Fatalf("%s max-plus-zero=%d, want %d", key, got, max)
			}
		})
	}
}

func TestRunnerAllowsRunsPastFormerMinimumDeadline(t *testing.T) {
	root, _ := testClone(t)
	pi := filepath.Join(t.TempDir(), "pi")
	script := "#!/bin/sh\n/bin/sleep 1.2\nprintf '%s\\n' '{\"usage\":{\"input\":0}}'\n"
	if err := os.WriteFile(pi, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.PiPath = pi
	record, err := runner.Run(context.Background(), Declaration{Name: "builder", Model: "local", TaskPrompt: "x"})
	if err != nil || record.Exit != 0 {
		t.Fatalf("long Run record=%#v err=%v", record, err)
	}
	if record.Duration < 1 {
		t.Fatalf("long Run duration=%v, want at least one second", record.Duration)
	}
	rows, ledgerErr := ReadLedger(root)
	if ledgerErr != nil || len(rows) != 1 || rows[0].Exit != 0 {
		t.Fatalf("long Run ledger=%v err=%v", rows, ledgerErr)
	}
}

func TestRunnerStopsGroupChildAfterLeaderSuccess(t *testing.T) {
	root, _ := testClone(t)
	omp := filepath.Join(t.TempDir(), "omp")
	_, heartbeat := processHeartbeatFixture(t)
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
while [ ! -s "$HEARTBEAT" ]; do sleep 0.01; done
printf '%s\n' "$child" > "$CHILD_PID"
printf '%s\n' '{"usage":{"input":0}}'
exit 0
`
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.PiPath = omp
	record, err := runner.Run(context.Background(), Declaration{Name: "builder", Model: "local", TaskPrompt: "x"})
	if err != nil || record.Exit != 0 {
		t.Fatalf("leader-success record=%#v err=%v", record, err)
	}
	assertProcessQuiescent(t, heartbeat, "group child", "successful leader")
	rows, ledgerErr := ReadLedger(root)
	if ledgerErr != nil || len(rows) != 1 || rows[0].Exit != 0 {
		t.Fatalf("leader-success ledger=%v err=%v, want one success row", rows, ledgerErr)
	}
}

func TestRunnerTerminatesProcessTreeOnCallerDeadline(t *testing.T) {
	root, _ := testClone(t)
	omp := filepath.Join(t.TempDir(), "omp")
	marker := filepath.Join(t.TempDir(), "term")
	t.Setenv("MARKER", marker)
	script := "#!/bin/sh\ntrap 'printf term > \"$MARKER\"; exit 0' TERM\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.PiPath = omp
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	record, err := runner.Run(ctx, Declaration{Name: "builder", Model: "local", TaskPrompt: "x"})
	if err == nil || record.Exit != 124 || time.Since(started) > 4*time.Second {
		t.Fatalf("deadline record=%#v err=%v elapsed=%v", record, err, time.Since(started))
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "term" {
		t.Fatalf("TERM marker=%q err=%v", data, err)
	}
}

func TestRunnerKillsGroupChildAfterLeaderExitsOnTERM(t *testing.T) {
	root, _ := testClone(t)
	omp := filepath.Join(t.TempDir(), "omp")
	state, heartbeat := processHeartbeatFixture(t)
	marker := filepath.Join(state, "leader-term")
	t.Setenv("TERM_MARKER", marker)
	script := `#!/bin/sh
set -eu
trap 'printf leader-term > "$TERM_MARKER"; exit 0' TERM
(
    trap '' TERM
    printf '%s\n' "$$" > "$CHILD_PID"
    while :; do
        printf x >> "$HEARTBEAT"
        sleep 0.02
    done
) &
while :; do sleep 1; done
`
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.PiPath = omp
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	record, err := runner.Run(ctx, Declaration{Name: "builder", Model: "local", TaskPrompt: "x"})
	if err == nil || record.Exit != 124 {
		t.Fatalf("leader-stop record=%#v err=%v", record, err)
	}
	rows, ledgerErr := ReadLedger(root)
	if ledgerErr != nil || len(rows) != 1 || rows[0].Exit != 124 {
		t.Fatalf("leader-stop ledger=%v err=%v, want one deadline row", rows, ledgerErr)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "leader-term" {
		t.Fatalf("leader TERM marker=%q err=%v", data, err)
	}
	assertProcessQuiescent(t, heartbeat, "group child", "leader stop")
}

func TestRunnerGitStopsRealDescendantAfterLeaderSuccess(t *testing.T) {
	root, _ := testClone(t)
	gitWrapper := filepath.Join(t.TempDir(), "git")
	_, heartbeat := processHeartbeatFixture(t)
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
printf '%s\n' git-output
exit 0
`
	if err := os.WriteFile(gitWrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.GitPath = gitWrapper
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := runner.git(ctx, root, "--version")
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "git-output\n" {
		t.Fatalf("Git output=%q, want leader output", output)
	}
	assertProcessQuiescent(t, heartbeat, "Git descendant", "leader success")
}

func TestRunnerGitCancellationEscalatesForRealDescendant(t *testing.T) {
	root, _ := testClone(t)
	gitWrapper := filepath.Join(t.TempDir(), "git")
	state, heartbeat := processHeartbeatFixture(t)
	termMarker := filepath.Join(state, "term")
	t.Setenv("TERM_MARKER", termMarker)
	script := `#!/bin/sh
set -eu
(
	trap 'printf term > "$TERM_MARKER"' TERM
	while :; do
		printf x >> "$HEARTBEAT"
		sleep 0.02 || :
	done
) &
child=$!
printf '%s\n' "$child" > "$CHILD_PID"
while [ ! -s "$HEARTBEAT" ]; do sleep 0.01; done
printf '%s\n' git-started
trap 'sleep 0.2; exit 0' TERM
while :; do sleep 1; done
`
	if err := os.WriteFile(gitWrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.GitPath = gitWrapper
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	output, err := runner.git(ctx, root, "fetch")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 3*time.Second {
		t.Fatalf("Git cancellation output=%q err=%v elapsed=%v", output, err, time.Since(started))
	}
	if string(output) != "git-started\n" {
		t.Fatalf("Git cancellation output=%q, want pre-cancellation output", output)
	}
	if data, readErr := os.ReadFile(termMarker); readErr != nil || string(data) != "term" {
		t.Fatalf("Git descendant TERM marker=%q err=%v", data, readErr)
	}
	assertProcessQuiescent(t, heartbeat, "Git descendant", "cancellation")
}

func TestRunnerCallerDeadlineIncludesPreparation(t *testing.T) {
	root, _ := testClone(t)
	slowGit := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(slowGit, []byte("#!/bin/sh\nexec /bin/sleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.GitPath = slowGit
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	record, err := runner.Run(ctx, Declaration{Name: "builder"})
	if err == nil || record.Exit != 124 || time.Since(started) > 3*time.Second {
		t.Fatalf("prepare deadline record=%#v err=%v elapsed=%v", record, err, time.Since(started))
	}
	rows, readErr := ReadLedger(root)
	if readErr != nil || len(rows) != 1 || rows[0].Exit != 124 {
		t.Fatalf("ledger=%v err=%v", rows, readErr)
	}
}

func TestRunnerCleansWorktreeWhenAddOutlivesCallerDeadline(t *testing.T) {
	root, _ := testClone(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "worktree-added")
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("WORKTREE_ADDED", marker)
	gitWrapper := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
if [ "$1" = worktree ] && [ "$2" = add ]; then
	"$REAL_GIT" "$@"
	: > "$WORKTREE_ADDED"
	exec /bin/sleep 10
fi
exec "$REAL_GIT" "$@"
`
	if err := os.WriteFile(gitWrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.GitPath = gitWrapper
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	record, err := runner.Run(ctx, Declaration{Name: "builder"})
	if err == nil || record.Exit != 124 {
		t.Fatalf("prepare deadline record=%#v err=%v", record, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("worktree add did not complete before caller deadline: %v", err)
	}
	worktree := forestPath(root, "worktrees", record.RunID)
	if _, err := os.Stat(worktree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial worktree path survived: %v", err)
	}
	if list := string(runGitDir(t, root, "worktree", "list", "--porcelain")); strings.Contains(list, worktree) {
		t.Fatalf("partial worktree registration survived:\n%s", list)
	}
}

func TestRunnerPreservesPreparationAndCleanupErrors(t *testing.T) {
	root, _ := testClone(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("REAL_GIT", realGit)
	gitWrapper := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
if [ "$1" = worktree ] && [ "$2" = add ]; then exit 7; fi
if [ "$1" = worktree ] && [ "$2" = remove ]; then exit 9; fi
exec "$REAL_GIT" "$@"
`
	if err := os.WriteFile(gitWrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.GitPath = gitWrapper
	record, err := runner.Run(context.Background(), Declaration{Name: "builder"})
	if err == nil || record.Exit != 1 {
		t.Fatalf("prepare failure record=%#v err=%v", record, err)
	}
	for _, want := range []string{"add worktree", "exit status 7", "cleanup worktree", "exit status 9"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestRunnerJoinsCleanupErrorsAndPreservesConcurrentRunRefs(t *testing.T) {
	root, _ := testClone(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	pruneMarker := filepath.Join(t.TempDir(), "pruned")
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("PRUNE_MARKER", pruneMarker)
	t.Setenv("ROLE", "builder")
	t.Setenv("NOTE_KIND", "review-request")
	canonical := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "update-ref", "refs/notes/forest/review-request", canonical)
	otherRef := "refs/notes/forest/private/live-run/verifier/checks/" + canonical + "/publication"
	runGitDir(t, root, "update-ref", otherRef, canonical)
	gitWrapper := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
if [ "$1" = worktree ] && [ "$2" = remove ]; then exit 9; fi
if [ "$1" = worktree ] && [ "$2" = prune ]; then
	"$REAL_GIT" "$@"
	: > "$PRUNE_MARKER"
	exit 11
fi
exec "$REAL_GIT" "$@"
`
	if err := os.WriteFile(gitWrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	omp := filepath.Join(t.TempDir(), "omp")
	ompScript := "#!/bin/sh\nset -eu\n" + runnerPrivateRefSetup + "printf '%s\\n' '{\"usage\":{\"input\":1}}'\n"
	if err := os.WriteFile(omp, []byte(ompScript), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.GitPath, runner.PiPath = gitWrapper, omp
	record, err := runner.Run(context.Background(), Declaration{Name: "builder"})
	if err == nil || record.Exit != 1 {
		t.Fatalf("cleanup record=%#v err=%v", record, err)
	}
	for _, want := range []string{"git worktree remove", "exit status 9", "git worktree prune", "exit status 11"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("cleanup error %q missing %q", err, want)
		}
	}
	assertRunnerPrivateRefsClean(t, root, record.RunID, canonical, "review-request", otherRef)
	worktree := forestPath(root, "worktrees", record.RunID)
	if _, statErr := os.Stat(worktree); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed Git remove left filesystem residue: %v", statErr)
	}
	if _, statErr := os.Stat(pruneMarker); statErr != nil {
		t.Fatalf("Git registry prune was not attempted: %v", statErr)
	}
	if list := string(runGitDir(t, root, "worktree", "list", "--porcelain")); strings.Contains(list, worktree) {
		t.Fatalf("failed Git remove left registry residue:\n%s", list)
	}
	rows, readErr := ReadLedger(root)
	if readErr != nil || len(rows) != 1 || rows[0].Exit != 1 {
		t.Fatalf("ledger=%v err=%v", rows, readErr)
	}
}

func TestRunnerPrunesRegistryAfterRemoveConsumesCleanupShare(t *testing.T) {
	root, _ := testClone(t)
	worktree := forestPath(root, "worktrees", "delayed-remove")
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "worktree", "add", "--detach", worktree, "HEAD")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	removeMarker := filepath.Join(t.TempDir(), "remove-started")
	pruneMarker := filepath.Join(t.TempDir(), "pruned")
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("REMOVE_MARKER", removeMarker)
	t.Setenv("PRUNE_MARKER", pruneMarker)
	gitWrapper := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
if [ "$1" = worktree ] && [ "$2" = remove ]; then
	: > "$REMOVE_MARKER"
	exec /bin/sleep 30
fi
if [ "$1" = worktree ] && [ "$2" = prune ]; then
	"$REAL_GIT" "$@"
	: > "$PRUNE_MARKER"
	exit 0
fi
exec "$REAL_GIT" "$@"
`
	if err := os.WriteFile(gitWrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.GitPath = gitWrapper
	started := time.Now()
	err = runner.cleanupWorktree(worktree, "delayed-remove")
	if err == nil || !strings.Contains(err.Error(), "git worktree remove") || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("delayed cleanup err=%v", err)
	}
	if elapsed := time.Since(started); elapsed >= cleanupTimeout {
		t.Fatalf("cleanup took %s, want less than %s", elapsed, cleanupTimeout)
	}
	if _, statErr := os.Stat(removeMarker); statErr != nil {
		t.Fatalf("delayed remove did not start: %v", statErr)
	}
	if _, statErr := os.Stat(pruneMarker); statErr != nil {
		t.Fatalf("bounded registry prune was not attempted: %v", statErr)
	}
	if _, statErr := os.Stat(worktree); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("delayed remove left filesystem residue: %v", statErr)
	}
	if list := string(runGitDir(t, root, "worktree", "list", "--porcelain")); strings.Contains(list, worktree) {
		t.Fatalf("delayed remove left registry residue:\n%s", list)
	}
}

func TestRunnerStopsBlockedFilesystemDeletionAndStillPrunes(t *testing.T) {
	root, _ := testClone(t)
	worktree := forestPath(root, "worktrees", "blocked-delete")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	removePID := filepath.Join(t.TempDir(), "remove-pid")
	pruneMarker := filepath.Join(t.TempDir(), "pruned")
	t.Setenv("REMOVE_PID", removePID)
	t.Setenv("PRUNE_MARKER", pruneMarker)
	rmDir := t.TempDir()
	rmWrapper := filepath.Join(rmDir, "rm")
	if err := os.WriteFile(rmWrapper, []byte("#!/bin/sh\nprintf '%s\\n' \"$$\" > \"$REMOVE_PID\"\nexec /bin/sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", rmDir)
	gitWrapper := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
if [ "$1" = worktree ] && [ "$2" = remove ]; then exit 9; fi
if [ "$1" = worktree ] && [ "$2" = prune ]; then
	: > "$PRUNE_MARKER"
	exit 0
fi
exec "$REAL_GIT" "$@"
`
	t.Setenv("REAL_GIT", realGit)
	if err := os.WriteFile(gitWrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.GitPath = gitWrapper
	started := time.Now()
	err = runner.cleanupWorktree(worktree, "blocked-delete")
	if err == nil || !strings.Contains(err.Error(), "git worktree remove") || !strings.Contains(err.Error(), "remove worktree path") || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("blocked cleanup err=%v", err)
	}
	if elapsed := time.Since(started); elapsed >= cleanupTimeout {
		t.Fatalf("blocked cleanup took %s, want less than %s", elapsed, cleanupTimeout)
	}
	pidBytes, err := os.ReadFile(removePID)
	if err != nil {
		t.Fatalf("filesystem deletion did not start: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("filesystem deletion pid %q: %v", pidBytes, err)
	}
	if processGroupExists(pid) {
		t.Fatalf("filesystem deletion process group %d survived", pid)
	}
	if _, statErr := os.Stat(pruneMarker); statErr != nil {
		t.Fatalf("bounded registry prune was not attempted: %v", statErr)
	}
	if _, statErr := os.Stat(worktree); statErr != nil {
		t.Fatalf("blocked filesystem residue was not preserved for retry: %v", statErr)
	}
}

func TestProcessResultHonorsCanceledContext(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if exit, err := processResult(canceled, nil); exit != 130 || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled exit=%d err=%v", exit, err)
	}
	deadline, stop := context.WithTimeout(context.Background(), time.Nanosecond)
	defer stop()
	<-deadline.Done()
	if exit, err := processResult(deadline, nil); exit != 124 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline exit=%d err=%v", exit, err)
	}
}

func TestRunnerCleanupStopsTermIgnoringRemoveAndFilesystemGroupsBeforePrune(t *testing.T) {
	root, _ := testClone(t)
	worktree := forestPath(root, "worktrees", "blocked-both")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	gitPID := filepath.Join(state, "git-pid")
	rmPID := filepath.Join(state, "rm-pid")
	pruneMarker := filepath.Join(state, "pruned")
	t.Setenv("GIT_PID", gitPID)
	t.Setenv("RM_PID", rmPID)
	t.Setenv("PRUNE_MARKER", pruneMarker)

	gitWrapper := filepath.Join(t.TempDir(), "git")
	gitScript := `#!/bin/sh
set -eu
if [ "$1" = worktree ] && [ "$2" = remove ]; then
	printf '%s\n' "$$" > "$GIT_PID"
	trap '' TERM
	while :; do /bin/sleep 1; done
fi
if [ "$1" = worktree ] && [ "$2" = prune ]; then
	: > "$PRUNE_MARKER"
	exit 11
fi
exit 12
`
	if err := os.WriteFile(gitWrapper, []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	rmDir := t.TempDir()
	rmWrapper := filepath.Join(rmDir, "rm")
	rmScript := `#!/bin/sh
set -eu
printf '%s\n' "$$" > "$RM_PID"
trap '' TERM
while :; do /bin/sleep 1; done
`
	if err := os.WriteFile(rmWrapper, []byte(rmScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", rmDir)

	runner := NewRunner(root)
	runner.GitPath = gitWrapper
	started := time.Now()
	err := runner.cleanupWorktree(worktree, "blocked-both")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("cleanup unexpectedly succeeded")
	}
	for _, want := range []string{"git worktree remove", "remove worktree path", "git worktree prune", "exit status 11"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("cleanup error %q missing %q", err, want)
		}
	}
	if strings.Count(err.Error(), context.DeadlineExceeded.Error()) < 2 {
		t.Fatalf("cleanup error did not join both execution deadlines: %v", err)
	}
	if elapsed >= cleanupTimeout-time.Second {
		t.Fatalf("cleanup took %s, want at least one second before %s bound", elapsed, cleanupTimeout)
	}
	if _, statErr := os.Stat(pruneMarker); statErr != nil {
		t.Fatalf("registry prune was not attempted: %v", statErr)
	}
	for name, path := range map[string]string{"Git remove": gitPID, "filesystem rm": rmPID} {
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("%s did not start: %v", name, readErr)
		}
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(body)))
		if parseErr != nil {
			t.Fatalf("%s pid %q: %v", name, body, parseErr)
		}
		if processGroupExists(pid) {
			t.Fatalf("%s process group %d survived cleanup", name, pid)
		}
	}
}

func TestRunEnvironmentUsesScratchPiDirectoryAndInheritedCredentials(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", "/operator/pi")
	t.Setenv("SERVICE_API_TOKEN", "inherited-only")
	t.Setenv("GIT_AUTHOR_NAME", "ambient")
	t.Setenv("GIT_AUTHOR_EMAIL", "ambient@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "ambient")
	t.Setenv("GIT_COMMITTER_EMAIL", "ambient@example.invalid")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "ambient")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'user.name'='ambient' 'user.email'='ambient@example.invalid'")
	environment, err := runEnvironment(root, "Iron Forest Builder", "builder@forest.invalid", "1-builder", "/tmp/run-pi")
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string)
	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	for key, want := range map[string]string{
		"PI_CODING_AGENT_DIR": "/tmp/run-pi",
		"SERVICE_API_TOKEN":   "inherited-only",
		"FOREST_RUN_ID":       "1-builder",
		"GIT_CONFIG_COUNT":    "2",
		"GIT_CONFIG_KEY_0":    "user.name",
		"GIT_CONFIG_VALUE_0":  "Iron Forest Builder",
		"GIT_CONFIG_KEY_1":    "user.email",
		"GIT_CONFIG_VALUE_1":  "builder@forest.invalid",
	} {
		if values[key] != want {
			t.Fatalf("%s=%q, want %q", key, values[key], want)
		}
	}
	for _, key := range []string{
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
		"GIT_CONFIG_PARAMETERS",
	} {
		if _, exists := values[key]; exists {
			t.Fatalf("%s leaked into Run environment", key)
		}
	}
}

func TestRunnerRejectsSkillSymlinkIntroducedInRunRevision(t *testing.T) {
	root, origin := testClone(t)
	outside := t.TempDir()
	author := t.TempDir()
	runGit(t, "clone", "--branch", "master", origin, author)
	configGit(t, author, "Builder", "builder@forest.invalid")
	if err := os.RemoveAll(filepath.Join(author, "agents", "_shared", "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(author, "agents", "_shared", "skills")); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, author, "add", "agents/_shared/skills")
	runGitDir(t, author, "commit", "-m", "introduce skill symlink")
	runGitDir(t, author, "push", "origin", "HEAD:master")
	runGitDir(t, root, "fetch", "origin", "master")

	pi := filepath.Join(t.TempDir(), "pi")
	marker := filepath.Join(t.TempDir(), "invoked")
	t.Setenv("PI_INVOKED", marker)
	if err := os.WriteFile(pi, []byte("#!/bin/sh\ntouch \"$PI_INVOKED\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.PiPath = pi
	record, err := runner.Run(context.Background(), Declaration{
		Name:         "builder",
		Model:        "local",
		SystemPrompt: "system",
		TaskPrompt:   "task",
		SkillPaths:   []string{"agents/_shared/skills"},
	})
	if err == nil || record.Exit != 1 || !strings.Contains(err.Error(), "validate Run skills") {
		t.Fatalf("record=%#v err=%v, want Run-skill validation failure", record, err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Pi ran despite invalid worktree skills: %v", statErr)
	}
	logData, readErr := os.ReadFile(runLogPath(root, record.RunID))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(logData), `"type":"forest.run"`) {
		t.Fatalf("failed validation published invocation evidence: %s", logData)
	}
}

func TestRunnerUsesAnEmptyTemporaryPiDirectoryAndRemovesIt(t *testing.T) {
	root, _ := testClone(t)
	state := t.TempDir()
	pi := filepath.Join(state, "pi")
	piDirFile := filepath.Join(state, "pi-dir")
	hostPiDir := filepath.Join(state, "operator-pi")
	if err := os.MkdirAll(filepath.Join(hostPiDir, "extensions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostPiDir, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostPiDir, "extensions", "host-mcp.js"), []byte("host extension fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_DIR_FILE", piDirFile)
	t.Setenv("HOST_PI_DIR", hostPiDir)
	t.Setenv("PI_CODING_AGENT_DIR", hostPiDir)
	t.Setenv("SERVICE_API_TOKEN", "service-credential")
	script := `#!/bin/sh
set -eu
test "$PI_CODING_AGENT_DIR" != "$HOST_PI_DIR"
test -d "$PI_CODING_AGENT_DIR"
test -w "$PI_CODING_AGENT_DIR"
test -z "$(ls -A "$PI_CODING_AGENT_DIR")"
test "$SERVICE_API_TOKEN" = service-credential
printf '%s' "$PI_CODING_AGENT_DIR" > "$PI_DIR_FILE"
printf '%s\n' '{"type":"message_end","message":{"usage":{"input":1,"output":2}}}'
`
	if err := os.WriteFile(pi, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.PiPath = pi
	record, err := runner.Run(context.Background(), Declaration{Name: "builder", Model: "local", SystemPrompt: "system", TaskPrompt: "task"})
	if err != nil || record.Exit != 0 {
		t.Fatalf("record=%#v err=%v", record, err)
	}
	piDirBytes, err := os.ReadFile(piDirFile)
	if err != nil {
		t.Fatal(err)
	}
	piDir := string(piDirBytes)
	if !filepath.IsAbs(piDir) || piDir == hostPiDir {
		t.Fatalf("Run Pi directory=%q", piDir)
	}
	if _, err := os.Stat(piDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Run Pi directory survived invocation: %v", err)
	}
	for _, relative := range []string{"auth.json", filepath.Join("extensions", "host-mcp.js")} {
		if _, err := os.Stat(filepath.Join(hostPiDir, relative)); err != nil {
			t.Fatalf("operator Pi fixture %s changed: %v", relative, err)
		}
	}
}

func TestRunnerEnablesOpenRouterSessionCorrelation(t *testing.T) {
	root, _ := testClone(t)
	state := t.TempDir()
	pi := filepath.Join(state, "pi")
	configFile := filepath.Join(state, "models.json")
	t.Setenv("MODEL_CONFIG_FILE", configFile)
	script := `#!/bin/sh
set -eu
test "$(stat -c %a "$PI_CODING_AGENT_DIR/models.json")" = 600
cat "$PI_CODING_AGENT_DIR/models.json" > "$MODEL_CONFIG_FILE"
printf '%s\n' '{"type":"message_end","message":{"usage":{"input":1,"output":2}}}'
`
	if err := os.WriteFile(pi, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.PiPath = pi
	record, err := runner.Run(context.Background(), Declaration{
		Name:         "verifier",
		Model:        "openrouter/deepseek/deepseek-v4-flash-0731",
		SystemPrompt: "system",
		TaskPrompt:   "task",
	})
	if err != nil || record.Exit != 0 {
		t.Fatalf("record=%#v err=%v", record, err)
	}
	config, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "providers": {
    "openrouter": {
      "compat": {
        "sendSessionAffinityHeaders": true,
        "sessionAffinityFormat": "openrouter"
      }
    }
  }
}
`
	if string(config) != want {
		t.Fatalf("models.json:\n%s\nwant:\n%s", config, want)
	}
}
