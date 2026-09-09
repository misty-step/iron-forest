package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExplicitRequestBypassesSelectionAndSurvivesFailedRun(t *testing.T) {
	root, _ := testClone(t)
	state := t.TempDir()
	selection := filepath.Join(state, "selected")
	writeTree(t, root, profileName+"/config.yaml", "repo: owner/name\nprimary: refs/heads/master\ndelivery: external\nagents:\n  builder: {poll: \"touch "+selection+"; exit 1\", interval: 1}\n")
	writeAgentFiles(t, root, "builder", "model: local\nrequest: touch "+selection+"; exit 1\n", "Standing system", "Standing task")
	runGitDir(t, root, "add", profileName)
	runGitDir(t, root, "commit", "-m", "explicit request profile")
	runGitDir(t, root, "push", "origin", "HEAD:master")
	argsPath := filepath.Join(state, "args")
	t.Setenv("REQUEST_TEST_ARGS", argsPath)
	pi := filepath.Join(state, "pi")
	if err := os.WriteFile(pi, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$REQUEST_TEST_ARGS\"\nprintf '%s\\n' '{\"type\":\"turn_end\",\"message\":{\"usage\":{\"input\":3,\"output\":2}}}'\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.PiPath = pi
	cfg, err := loadConfig(configPath(root))
	if err != nil {
		t.Fatal(err)
	}
	request := &RunRequest{Schema: "forest.request.v1", ID: "request-immutable", Prompt: "Fix only the supplied request.", Work: &WorkReference{System: "https://tracker.example", ID: "immutable-item-id", Key: "EX-9"}}
	scheduler := NewScheduler(root, cfg, runner)
	dispatched, err := scheduler.OnceRequest(context.Background(), "builder", request)
	if !dispatched || err == nil {
		t.Fatalf("failed harness: dispatched=%t err=%v", dispatched, err)
	}
	if _, err := os.Stat(selection); !os.IsNotExist(err) {
		t.Fatalf("explicit request invoked hidden selection: %v", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil || !strings.Contains(string(args), "Standing task") || !strings.Contains(string(args), request.Prompt) {
		t.Fatalf("Run did not receive both standing task and explicit request: %q %v", args, err)
	}
	rows, err := readLedger(root, -1)
	if err != nil || len(rows) != 1 || rows[0].Exit != 7 || rows[0].RequestID != request.ID || !reflect.DeepEqual(rows[0].Work, request.Work) {
		t.Fatalf("failed Run lost immutable attribution: %#v %v", rows, err)
	}
	retained, err := readRunRequest(forestPath(root, "runs", rows[0].RunID+".request.json"))
	if err != nil || !reflect.DeepEqual(retained, request) {
		t.Fatalf("retained failed request=%#v err=%v", retained, err)
	}
}

func TestScheduledRequestReceivesRunIDBeforePreparationFailure(t *testing.T) {
	root := t.TempDir()
	runner := NewRunner(root)
	record, err := runner.Run(context.Background(), Declaration{Name: "builder", Model: "local", TaskPrompt: "Standing task", RequestCommand: `printf '{"schema":"forest.request.v1","id":"%s","prompt":"One request","work":{"system":"tracker","id":"immutable"}}\n' "$FOREST_RUN_ID"`})
	if err == nil || record.Exit == 0 || record.RequestID != record.RunID || record.Work == nil || record.Work.ID != "immutable" {
		t.Fatalf("preparation failure lost selected request: %#v %v", record, err)
	}
	retained, found, err := FindRun(root, record.RunID)
	if err != nil || !found || retained.RequestID != record.RequestID || !reflect.DeepEqual(retained.Work, record.Work) {
		t.Fatalf("preparation failure missing from Ledger: %#v found=%t err=%v", retained, found, err)
	}
}

func TestScheduledRequestDistinguishesNoWorkFromFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
		noWork  bool
	}{
		{"no work", "exit 1", true},
		{"request failure", "exit 2", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, profileName+"/config.yaml", "repo: owner/name\nprimary: refs/heads/master\ndelivery: external\nagents:\n  builder: {poll: \"true\", interval: 1}\n")
			writeAgentFiles(t, root, "builder", "model: local\nrequest: "+test.command+"\n", "System", "Standing task")
			cfg, err := loadConfig(configPath(root))
			if err != nil {
				t.Fatal(err)
			}
			scheduler := NewScheduler(root, cfg, NewRunner(root))
			dispatched, runErr := scheduler.Once(context.Background(), "builder")
			if dispatched == test.noWork || (runErr == nil) != test.noWork {
				t.Fatalf("request outcome dispatched=%t error=%v; no work=%t", dispatched, runErr, test.noWork)
			}
			rows, err := readLedger(root, -1)
			if err != nil || len(rows) != 1 || rows[0].NoWork != test.noWork || rows[0].Exit == 0 {
				t.Fatalf("request receipt=%#v error=%v", rows, err)
			}
			if test.noWork {
				aggregates := computeLedgerAggregates(rows)
				if aggregates.Runs != 0 || len(aggregates.RecentFailures) != 0 || scheduler.health["builder"].RunError != "" {
					t.Fatalf("no-work selection fabricated an execution outcome: %#v health=%#v", aggregates, scheduler.health["builder"])
				}
			}
		})
	}
}

func TestAdmissionPauseDoesNotWaitForItsActiveRun(t *testing.T) {
	root := t.TempDir()
	active, err := reserveRunAdmission(root, RunRecord{RunID: "1-builder", Agent: "builder"})
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	done := make(chan cliOutcome, 1)
	go func() { done <- runAdmissionPause(nil, cliFlags{root: root}) }()
	select {
	case outcome := <-done:
		if outcome.Exit != exitOK {
			t.Fatalf("in-Run pause failed: %#v", outcome)
		}
		state, err := readAdmission(root)
		if err != nil || !state.Paused {
			t.Fatalf("in-Run pause was not durable: %#v %v", state, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pause waited for its own active Run")
	}
}

func TestAdmissionPauseWinsPollRaceAndSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "'true'")
	writeTestDeclaration(t, root, "builder")
	cfg, err := loadConfig(configPath(root))
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(root, cfg, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	releasePoll := sync.OnceFunc(func() { close(release) })
	defer releasePoll()
	scheduler.Poll = func(context.Context, string) PollResult { close(entered); <-release; return PollResult{} }
	scheduler.Run = func(context.Context, Declaration) (RunRecord, error) {
		t.Error("paused admission executed a Run")
		return RunRecord{}, nil
	}
	done := make(chan bool, 1)
	go func() { dispatched, _ := scheduler.Once(context.Background(), "builder"); done <- dispatched }()
	<-entered
	if outcome := runAdmissionPause(nil, cliFlags{root: root}); outcome.Exit != exitOK {
		t.Fatal(outcome.ErrText)
	}
	releasePoll()
	if <-done {
		t.Fatal("Poll completing after pause bypassed the admission fence")
	}
	request := &RunRequest{Schema: "forest.request.v1", ID: "request", Prompt: "Do one thing"}
	if code := once(root, "builder", request); code != exitConflict {
		t.Fatalf("fresh Kernel once bypassed persistent pause: exit=%d", code)
	}
	if outcome := runAdmissionResume(nil, cliFlags{root: root}); outcome.Exit != exitOK {
		t.Fatal(outcome.ErrText)
	}
	state, err := readAdmission(root)
	if err != nil || state.Paused {
		t.Fatalf("resume did not persist: %#v %v", state, err)
	}
}

func TestAdmissionDrainWaitsWithoutCancellingAndBlocksResume(t *testing.T) {
	root := t.TempDir()
	active, err := reserveRunAdmission(root, RunRecord{RunID: "1-builder", Agent: "builder", Started: time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- drainAdmission(ctx, root) }()
	for {
		state, err := readAdmission(root)
		if err != nil {
			t.Fatal(err)
		}
		if state.Paused {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("drain did not pause admission")
		case <-time.After(time.Millisecond):
		}
	}
	view, err := admissionSnapshot(root)
	if err != nil || view.Drained || view.ActiveCount != 1 || view.ActiveRuns[0].RunID != "1-builder" {
		t.Fatalf("active Run disappeared during drain: %#v %v", view, err)
	}
	if outcome := runAdmissionResume(nil, cliFlags{root: root}); outcome.Exit != exitConflict {
		t.Fatalf("resume bypassed active drain: %#v", outcome)
	}
	select {
	case err := <-done:
		t.Fatalf("drain returned with Run admitted: %v", err)
	default:
	}
	if hasRunCancellationMarker(root, "1-builder") {
		t.Fatal("drain cancelled the admitted Run")
	}
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	view, err = admissionSnapshot(root)
	if err != nil || !view.Paused || !view.Drained {
		t.Fatalf("drain receipt=%#v err=%v", view, err)
	}
}

func TestInterruptedRunRecoveryPreservesAttributionWithoutDuplicates(t *testing.T) {
	root := t.TempDir()
	live := liveRunRecord{RunID: "1-builder", Agent: "builder", StartedAt: "2026-01-01T00:00:00Z", RequestID: "request", Work: &WorkReference{System: "tracker", ID: "immutable"}, DefinitionSHA: "definition", ExtensionSHA: map[string]string{".iron-forest/usage.ts": "digest"}}
	if err := writeLiveRun(liveRunPath(root, live.Agent), live); err != nil {
		t.Fatal(err)
	}
	if err := recoverInterruptedRuns(root); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after Ledger publication but before live-record removal.
	if err := writeLiveRun(liveRunPath(root, live.Agent), live); err != nil {
		t.Fatal(err)
	}
	if err := recoverInterruptedRuns(root); err != nil {
		t.Fatal(err)
	}
	rows, err := readLedger(root, -1)
	if err != nil || len(rows) != 1 || rows[0].Exit != 137 || rows[0].RequestID != live.RequestID || !reflect.DeepEqual(rows[0].Work, live.Work) || !reflect.DeepEqual(rows[0].ExtensionSHA, live.ExtensionSHA) {
		t.Fatalf("interrupted attribution duplicated or lost: %#v %v", rows, err)
	}
}

func TestRunRefusesExtensionBytesChangedInFetchedRevision(t *testing.T) {
	root, origin := testClone(t)
	writeAgentFiles(t, root, "builder", "model: local\nextensions: [.iron-forest/extensions/usage.ts]\n", "System", "Task")
	writeTree(t, root, profileName+"/extensions/usage.ts", "export default function () {}\n")
	runGitDir(t, root, "add", profileName)
	runGitDir(t, root, "commit", "-m", "reviewed extension")
	runGitDir(t, root, "push", "origin", "HEAD:master")
	declaration, err := loadDeclaration(root, "builder")
	if err != nil {
		t.Fatal(err)
	}
	author := filepath.Join(t.TempDir(), "author")
	runGit(t, "clone", origin, author)
	configGit(t, author, "Author", "author@example.invalid")
	writeTree(t, author, profileName+"/extensions/usage.ts", "throw new Error('unreviewed');\n")
	runGitDir(t, author, "commit", "-am", "change extension")
	runGitDir(t, author, "push", "origin", "HEAD:master")
	marker := filepath.Join(t.TempDir(), "invoked")
	pi := filepath.Join(t.TempDir(), "pi")
	if err := os.WriteFile(pi, []byte("#!/bin/sh\ntouch "+quoteShellPath(marker)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.PiPath = pi
	record, err := runner.Run(context.Background(), declaration)
	if err == nil || record.Exit == 0 {
		t.Fatalf("changed extension dispatched: %#v %v", record, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Pi executed unreviewed extension: %v", err)
	}
}

func TestExternalDeliveryRefusesNativePublicationAndDoesNotAudit(t *testing.T) {
	root, _ := testClone(t)
	writeTree(t, root, profileName+"/config.yaml", "repo: owner/name\nprimary: refs/heads/master\ndelivery: external\nagents:\n  builder: {poll: true-command, interval: 1}\n")
	if _, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root: root, RunID: "1-builder", Role: "builder", Branch: "forest/one-request",
		PayloadPath: filepath.Join(root, "absent-request.json"),
	}); !publishConflict(err) {
		t.Fatalf("external review-request publication was not refused: %v", err)
	}
	if _, err := publishVerdict(context.Background(), publishVerdictInput{
		Root: root, RunID: "1-verifier", ChecksPath: filepath.Join(root, "absent-checks.json"),
		VerdictPath: filepath.Join(root, "absent-verdict.json"),
	}); !publishConflict(err) {
		t.Fatalf("external verdict publication was not refused: %v", err)
	}
	result, err := audit(context.Background(), root)
	if err != nil || !result.NotApplicable {
		t.Fatalf("external audit attempted native Git work: %#v %v", result, err)
	}
	outcome := runAuditShow(nil, cliFlags{root: root, rescan: true})
	encoded, err := json.Marshal(outcome.Data)
	if err != nil || outcome.Exit != exitOK || !strings.Contains(string(encoded), `"last_result":"not_applicable"`) {
		t.Fatalf("external audit projection=%s err=%v", encoded, err)
	}
	if _, err := os.Stat(auditStatePath(root)); !os.IsNotExist(err) {
		t.Fatalf("external audit fabricated a persisted native outcome: %v", err)
	}
}
