package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRunnerCompletionSeparatesExecutionFromProfileEvidence(t *testing.T) {
	for _, test := range []struct {
		name        string
		configured  bool
		output      string
		commandExit string
		status      string
	}{
		{name: "unconfigured"},
		{name: "required receipt missing", configured: true, output: `{"schema":"forest.completion.v1","status":"incomplete","reason":"required review receipt is missing"}`, commandExit: "0", status: "incomplete"},
		{name: "changes receipt completes review", configured: true, output: `{"schema":"forest.completion.v1","status":"completed","evidence":"https://review.example/receipts/changes-7"}`, commandExit: "0", status: "completed"},
		{name: "completed without evidence", configured: true, output: `{"schema":"forest.completion.v1","status":"completed"}`, commandExit: "0", status: "unknown"},
		{name: "failed observer cannot claim completion", configured: true, output: `{"schema":"forest.completion.v1","status":"completed","evidence":"https://review.example/receipts/7"}`, commandExit: "9", status: "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, _ := testClone(t)
			state := t.TempDir()
			inputPath := filepath.Join(state, "completion-context.json")
			callsPath := filepath.Join(state, "completion-calls")
			t.Setenv("COMPLETION_INPUT", inputPath)
			t.Setenv("COMPLETION_CALLS", callsPath)
			t.Setenv("COMPLETION_OUTPUT", test.output)
			t.Setenv("COMPLETION_EXIT", test.commandExit)
			frontmatter := "model: local\n"
			if test.configured {
				frontmatter += `completion: |
  set -eu
  test "$(pwd -P)" = "$(cd "$FOREST_ROOT" && pwd -P)"
  test -d ".iron-forest/runtime/worktrees/$FOREST_RUN_ID"
  test -f ".iron-forest/runtime/runs/live-verifier.json"
  cat > "$COMPLETION_INPUT"
  printf x >> "$COMPLETION_CALLS"
  printf '%s\n' "$COMPLETION_OUTPUT"
  exit "$COMPLETION_EXIT"
`
			}
			writeAgentFiles(t, root, "verifier", frontmatter, "Review the explicit request", "Standing review task")
			runGitDir(t, root, "add", profileName)
			runGitDir(t, root, "commit", "-m", "completion observer profile")
			runGitDir(t, root, "push", "origin", "HEAD:master")
			declaration, err := loadDeclaration(root, "verifier")
			if err != nil {
				t.Fatal(err)
			}
			request := &RunRequest{Schema: "forest.request.v1", ID: "review-request", Prompt: "Review the current candidate", Work: &WorkReference{System: "tracker", ID: "immutable-item"}}
			declaration.Request = request
			pi := filepath.Join(state, "pi")
			if err := os.WriteFile(pi, []byte("#!/bin/sh\nprintf '%s\\n' '{\"usage\":{\"input\":1,\"output\":2}}'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			runner := NewRunner(root)
			runner.PiPath = pi
			record, err := runner.Run(context.Background(), declaration)
			if err != nil || record.Exit != 0 || record.ProcessExit == nil || *record.ProcessExit != 0 || record.Outcome != runOutcomeCompleted {
				t.Fatalf("profile observation replaced execution result: %#v %v", record, err)
			}
			retained, found, err := FindRun(root, record.RunID)
			if err != nil || !found {
				t.Fatalf("observed Run missing from the Ledger: found=%t err=%v", found, err)
			}
			// Exact struct equality is not an invariant: RunRecord omits empty
			// fields, so a non-nil empty ExtensionSHA map is legitimately absent
			// from the row. Assert the execution and completion facts a consumer
			// reads instead of the in-memory nil-ness of unrelated maps.
			if retained.RunID != record.RunID || retained.Agent != record.Agent ||
				retained.Exit != record.Exit || retained.Outcome != record.Outcome ||
				retained.Started != record.Started || retained.Duration != record.Duration ||
				!reflect.DeepEqual(retained.ProcessExit, record.ProcessExit) ||
				!reflect.DeepEqual(retained.Completion, record.Completion) {
				t.Fatalf("Ledger row lost observed execution or completion facts: retained=%#v returned=%#v", retained, record)
			}
			if !test.configured {
				if record.Completion != nil {
					t.Fatalf("unconfigured observer invented evidence: %#v", record.Completion)
				}
				return
			}
			if record.Completion == nil || record.Completion.Status != test.status {
				t.Fatalf("completion=%#v, want %s", record.Completion, test.status)
			}
			if test.status == "completed" && record.Completion.Evidence != "https://review.example/receipts/changes-7" {
				t.Fatalf("changes review evidence was not retained: %#v", record.Completion)
			}
			if test.status != "completed" && record.Completion.Reason == "" {
				t.Fatalf("non-completion lost its reason: %#v", record.Completion)
			}
			if calls := string(mustReadFile(t, callsPath)); calls != "x" {
				t.Fatalf("completion observer retried: %q", calls)
			}
			var input completionContext
			if err := json.Unmarshal(mustReadFile(t, inputPath), &input); err != nil {
				t.Fatal(err)
			}
			if input.Schema != "forest.completion-context.v1" || input.Run.RunID != record.RunID || input.Run.Agent != "verifier" || input.Run.ProcessExit == nil || *input.Run.ProcessExit != 0 || !reflect.DeepEqual(input.Request, request) || !reflect.DeepEqual(input.Run.Work, request.Work) {
				t.Fatalf("observer could not bind external evidence to this attempt: %#v", input)
			}
			if _, err := os.Stat(forestPath(root, "worktrees", record.RunID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("completion observer prevented Run cleanup: %v", err)
			}
		})
	}
}

func TestCompletionObserverRejectsInvalidProtocol(t *testing.T) {
	for _, input := range []string{
		``,
		`null`,
		`{"schema":"forest.completion.v2","status":"completed","evidence":"receipt"}`,
		`{"schema":"forest.completion.v1","status":"completed","evidence":"   "}`,
		`{"schema":"forest.completion.v1","status":"unknown"}`,
		`{"schema":"forest.completion.v1","status":"delivered","evidence":"receipt"}`,
		`{"schema":"forest.completion.v1","status":"completed","evidence":"receipt","merge":true}`,
		`{"schema":"forest.completion.v1","status":"completed","evidence":"receipt"} {}`,
	} {
		if completion, err := decodeRunCompletion([]byte(input)); err == nil || completion != nil {
			t.Errorf("invalid completion accepted: %q => %#v %v", input, completion, err)
		}
	}
}

func TestCompletionObserverBoundsProcessesAndOutput(t *testing.T) {
	t.Run("deadline stops observer", func(t *testing.T) {
		root := t.TempDir()
		_, heartbeat := processHeartbeatFixture(t)
		command := `printf '%s\n' "$$" > "$CHILD_PID"
trap '' TERM
printf '%s\n' '{"schema":"forest.completion.v1","status":"completed","evidence":"receipt"}'
while :; do printf x >> "$HEARTBEAT"; sleep 0.02; done`
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		started := time.Now()
		completion := NewRunner(root).completionForRun(ctx, Declaration{CompletionCommand: command}, RunRecord{RunID: "1-verifier"}, nil, io.Discard)
		if completion.Status != "unknown" || !strings.Contains(completion.Reason, context.DeadlineExceeded.Error()) || time.Since(started) >= 4*time.Second {
			t.Fatalf("observer deadline became success or lost its bound: %#v", completion)
		}
		assertProcessQuiescent(t, heartbeat, "completion observer", "deadline")
	})
	t.Run("output overflow is unknown", func(t *testing.T) {
		completion := NewRunner(t.TempDir()).completionForRun(context.Background(), Declaration{CompletionCommand: "printf '%1100000s' x"}, RunRecord{RunID: "1-verifier"}, nil, io.Discard)
		if completion.Status != "unknown" || !strings.Contains(completion.Reason, errTrustedTransportOutputOverflow.Error()) {
			t.Fatalf("unbounded observer output accepted: %#v", completion)
		}
	})
}

func TestRunRecoveryRetainsStopCausesAndExecutionFacts(t *testing.T) {
	for _, test := range []struct {
		name      string
		outcome   string
		exit      int
		rawExit   int
		finalized bool
		cancelled bool
		want      string
	}{
		{name: "timeout survives later cancel", outcome: runOutcomeTimedOut, exit: runTimedOutExit, rawExit: -1, cancelled: true, want: runOutcomeTimedOut},
		{name: "operator cancellation survives marker loss", outcome: runOutcomeCancelled, exit: runCancelledExit, rawExit: -1, want: runOutcomeCancelled},
		{name: "cleanup interruption is not execution success", outcome: runOutcomeCompleted, rawExit: 0, want: runOutcomeInterrupted},
		{name: "finalized result survives publication interruption", outcome: runOutcomeCompleted, rawExit: 0, finalized: true, want: runOutcomeCompleted},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			record := RunRecord{RunID: "1-verifier", Agent: "verifier", Outcome: test.outcome, Exit: test.exit, ProcessExit: &test.rawExit,
				Completion: &RunCompletion{Schema: "forest.completion.v1", Status: "completed", Evidence: "https://review.example/receipts/changes-7"}}
			live := liveRecord(record)
			live.Finalized = test.finalized
			if err := writeLiveRun(liveRunPath(root, record.Agent), live); err != nil {
				t.Fatal(err)
			}
			if test.cancelled {
				if err := writeRunCancellationMarker(root, record.RunID); err != nil {
					t.Fatal(err)
				}
			}
			if err := recoverInterruptedRuns(root); err != nil {
				t.Fatal(err)
			}
			retained, found, err := FindRun(root, record.RunID)
			if err != nil || !found || retained.Outcome != test.want || retained.ProcessExit == nil || *retained.ProcessExit != test.rawExit || !reflect.DeepEqual(retained.Completion, record.Completion) {
				t.Fatalf("recovery invented or discarded known facts: %#v found=%t err=%v", retained, found, err)
			}
			if test.want == runOutcomeTimedOut && retained.Exit != runTimedOutExit {
				t.Fatalf("recovery relabeled timeout as operator cancellation: %#v", retained)
			}
		})
	}
}

func TestLegacyRunExitAndErrorDoNotInventOutcomeOrCompletion(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	writeLedgerRows(t, root,
		`{"run_id":"old-cancel","agent":"builder","exit":130,"error":"run cancelled by operator"}`,
		`{"run_id":"old-zero","agent":"verifier","exit":0}`,
	)
	for _, runID := range []string{"old-cancel", "old-zero"} {
		_, envelope, _ := decodeEnvelope(t, "run", "show", runID, "--json", "--root", root)
		keys := payloadKeys(t, envelope)
		for _, field := range []string{"process_exit", "outcome", "completion"} {
			if _, exists := keys[field]; exists {
				t.Fatalf("legacy Run %s fabricated %s: %#v", runID, field, keys)
			}
		}
	}
}

func TestRunnerCleanupDoesNotRelabelExecutionWithLateContextCancellation(t *testing.T) {
	root, _ := testClone(t)
	state := t.TempDir()
	entered := filepath.Join(state, "cleanup-entered")
	release := filepath.Join(state, "cleanup-release")
	gitPath, err := trustedExecutable(root, "git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLEANUP_ENTERED", entered)
	t.Setenv("CLEANUP_RELEASE", release)
	t.Setenv("REAL_GIT", gitPath)
	wrapper := filepath.Join(state, "git")
	script := `#!/bin/sh
if [ "$1" = worktree ] && [ "$2" = remove ]; then
  printf ready > "$CLEANUP_ENTERED"
  while [ ! -f "$CLEANUP_RELEASE" ]; do sleep 0.01; done
  exit 9
fi
exec "$REAL_GIT" "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	pi := filepath.Join(state, "pi")
	if err := os.WriteFile(pi, []byte("#!/bin/sh\nprintf '%s\\n' '{\"usage\":{\"input\":1}}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.GitPath, runner.PiPath = wrapper, pi
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan RunRecord, 1)
	go func() {
		record, _ := runner.Run(ctx, Declaration{Name: "builder", Model: "local", TaskPrompt: "x"})
		done <- record
	}()
	waitForCLIFile(t, entered)
	cancel()
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case record := <-done:
		if record.Outcome != runOutcomeCompleted || record.Exit != 0 || record.ProcessExit == nil || *record.ProcessExit != 0 || record.Error != "" || !strings.Contains(record.CleanupError, "exit status 9") {
			t.Fatalf("cleanup or later caller cancellation changed successful execution: %#v", record)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not finish deferred cleanup")
	}
}
