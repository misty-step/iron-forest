package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The Run commands: listing, paging, one row, and log reading or following.

func TestCLIRunListPagesNewestFirstAndTerminates(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	ids := []string{"run-a", "run-b", "run-c", "run-d", "run-e"}
	for index, id := range ids {
		if err := AppendRun(root, RunRecord{RunID: id, Agent: "builder", Exit: index}); err != nil {
			t.Fatal(err)
		}
	}

	var seen []string
	cursor := ""
	for page := 0; page < len(ids); page++ {
		args := []string{"run", "list", "--limit", "2", "--json", "--root", root}
		if cursor != "" {
			args = append(args, "--after", cursor)
		}
		_, envelope, _ := decodeEnvelope(t, args...)
		encoded, err := json.Marshal(envelope.Data)
		if err != nil {
			t.Fatal(err)
		}
		var payload runListPayload
		if err := json.Unmarshal(encoded, &payload); err != nil {
			t.Fatal(err)
		}
		for _, record := range payload.Runs {
			seen = append(seen, record.RunID)
		}
		if payload.NextAfter == "" {
			break
		}
		cursor = payload.NextAfter
	}
	want := []string{"run-e", "run-d", "run-c", "run-b", "run-a"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("paged order=%v, want %v", seen, want)
	}
}

// A cursor that names no run must be reported. Silently restarting at page one
// makes a paging consumer loop forever.
func TestCLIRunListUnknownCursorIsNotFound(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := AppendRun(root, RunRecord{RunID: "run-a", Agent: "builder"}); err != nil {
		t.Fatal(err)
	}
	code, envelope, _ := decodeEnvelope(t, "run", "list", "--after", "evicted", "--json", "--root", root)
	if code != exitNotFound {
		t.Fatalf("code=%d, want %d (envelope=%+v)", code, exitNotFound, envelope)
	}
}

func TestCLIRunLogsJSONCarriesTheLog(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := AppendRun(root, RunRecord{RunID: "run-7-builder", Agent: "builder", Exit: 3}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(runLogPath(root, "run-7-builder")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runLogPath(root, "run-7-builder"), []byte("step one\nstep two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, envelope, _ := decodeEnvelope(t, "run", "logs", "run-7-builder", "--json", "--root", root)
	keys := payloadKeys(t, envelope)
	if keys["text"] != "step one\nstep two\n" {
		t.Fatalf("payload text=%v, want the log content", keys["text"])
	}
	if keys["retained"] != true || keys["complete"] != true {
		t.Fatalf("payload=%v, want retained and complete", keys)
	}

	code, stdout, _ := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "logs", "run-7-builder", "--root", root})
	})
	if code != exitOK || stdout != "step one\nstep two\n" {
		t.Fatalf("human logs code=%d stdout=%q", code, stdout)
	}
}

// A retained-but-evicted log is distinguishable from an unknown run.
func TestCLIRunLogsSeparatesEvictedFromUnknown(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := AppendRun(root, RunRecord{RunID: "run-old", Agent: "builder", Exit: 0}); err != nil {
		t.Fatal(err)
	}

	code, envelope, _ := decodeEnvelope(t, "run", "logs", "run-old", "--json", "--root", root)
	if code != exitOK {
		t.Fatalf("evicted log code=%d, want %d", code, exitOK)
	}
	if keys := payloadKeys(t, envelope); keys["retained"] != false {
		t.Fatalf("payload=%v, want retained=false", keys)
	}

	code, stdout, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "logs", "run-old", "--root", root})
	})
	if code != exitOK {
		t.Fatalf("human evicted log code=%d, want %d", code, exitOK)
	}
	if stdout != "" {
		t.Fatalf("human evicted log stdout=%q, want empty", stdout)
	}
	if !strings.Contains(stderr, `run "run-old" has no retained log`) {
		t.Fatalf("human evicted log stderr=%q, want warning", stderr)
	}

	code, _, _ = captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "logs", "ghost", "--root", root})
	})
	if code != exitNotFound {
		t.Fatalf("unknown run code=%d, want %d", code, exitNotFound)
	}
}

// --follow on a run that does not exist must fail, not wait for a Run that will
// never appear.
func TestCLIRunLogsFollowRejectsUnknownRun(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	done := make(chan int, 1)
	go func() {
		code, _, _ := captureCLIOutput(t, func() int {
			return runSurfaceCommand([]string{"run", "logs", "--follow", "ghost", "--root", root})
		})
		done <- code
	}()
	select {
	case code := <-done:
		if code != exitNotFound {
			t.Fatalf("code=%d, want %d", code, exitNotFound)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run logs --follow on an unknown run did not return")
	}
}

func TestCLIRunLogsFollowStreamsUntilTheRunCompletes(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	logPath := runLogPath(root, "run-live-builder")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("started\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A live Run implies a Kernel holding the workspace lock, which is what tells
	// follow the Run is still alive.
	lock, err := os.OpenFile(forestPath(root, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	// The Kernel goroutine owns the lock and the handle for the rest of the test,
	// so the test body never touches either concurrently.
	finished := make(chan struct{})
	defer func() {
		<-finished
		_ = lock.Close()
	}()
	go func() {
		defer close(finished)
		time.Sleep(followPollInterval)
		file, openErr := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
		if openErr == nil {
			_, _ = file.WriteString("finished\n")
			_ = file.Close()
		}
		_ = AppendRun(root, RunRecord{RunID: "run-live-builder", Agent: "builder", Exit: 7})
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	}()

	code, stdout, _ := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "logs", "--follow", "run-live-builder", "--root", root})
	})
	if code != 7 {
		t.Fatalf("follow code=%d, want the Run's exit 7 (stdout=%q)", code, stdout)
	}
	for _, want := range []string{"started", "finished"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout=%q missing %q", stdout, want)
		}
	}
}

// A Run whose Kernel died without recording an outcome must be reported, not
// followed forever.
func TestCLIRunLogsFollowReportsARunWhoseKernelDied(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	logPath := runLogPath(root, "run-orphan-builder")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("partial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _, _ := captureCLIOutput(t, func() int {
			return runSurfaceCommand([]string{"run", "logs", "--follow", "run-orphan-builder", "--root", root})
		})
		done <- code
	}()
	select {
	case code := <-done:
		if code != exitError {
			t.Fatalf("orphaned run code=%d, want %d", code, exitError)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("follow did not report a Run whose Kernel died")
	}
}

func TestCLIRunLogsFollowRejectsJSON(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	code, _, _ := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "logs", "--follow", "id", "--json", "--root", root})
	})
	if code != exitInvalidArg {
		t.Fatalf("code=%d, want %d", code, exitInvalidArg)
	}
}

// A cursor that names two rows can never advance, so a client paging with it
// would loop forever. The ledger says so instead.
func TestCLIRunListRefusesAnAmbiguousCursor(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	writeLedgerRows(t, root, `{"run_id":"a","agent":"builder"}`, `{"run_id":"dup","agent":"builder"}`, `{"run_id":"dup","agent":"builder"}`)

	code, _, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "list", "--limit", "1", "--after", "dup", "--root", root})
	})
	if code != exitError {
		t.Fatalf("code=%d, want %d (stderr=%q)", code, exitError, stderr)
	}
	if !strings.Contains(stderr, "more than one row") {
		t.Fatalf("stderr=%q, want it to name the ambiguity", stderr)
	}
}

// An empty identity at a page boundary would publish the same cursor the
// protocol uses for "last page", hiding every older Run.
func TestCLIRunListRefusesAnEmptyIdentityCursor(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	writeLedgerRows(t, root, `{"run_id":"x","agent":"builder"}`, `{"run_id":"","agent":"builder"}`)

	code, _, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "list", "--limit", "1", "--root", root})
	})
	if code != exitError {
		t.Fatalf("code=%d, want %d (stderr=%q)", code, exitError, stderr)
	}
}

// Paging a healthy ledger still visits every row exactly once and terminates.
func TestCLIRunListPagesEveryRowExactlyOnce(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	rows := make([]string, 0, 40)
	for index := range 40 {
		rows = append(rows, fmt.Sprintf(`{"run_id":"r%02d","agent":"builder"}`, index))
	}
	writeLedgerRows(t, root, rows...)

	seen := map[string]int{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 40 {
			t.Fatal("paging did not terminate")
		}
		args := []string{"run", "list", "--limit", "7", "--root", root, "--json"}
		if cursor != "" {
			args = append(args, "--after", cursor)
		}
		code, envelope, stderr := decodeEnvelope(t, args...)
		if code != exitOK {
			t.Fatalf("page %d code=%d stderr=%q", pages, code, stderr)
		}
		var payload runListPayload
		decodePayload(t, envelope, &payload)
		for _, record := range payload.Runs {
			seen[record.RunID]++
		}
		if cursor = payload.NextAfter; cursor == "" {
			break
		}
	}
	if len(seen) != 40 {
		t.Fatalf("visited %d distinct rows, want 40", len(seen))
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("row %s delivered %d times, want once", id, count)
		}
	}
}

// A log that is not a regular file would block every read forever.
func TestCLIRunLogsRefusesANonRegularLog(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	writeLedgerRows(t, root, `{"run_id":"fifo","agent":"builder"}`)
	logPath := runLogPath(root, "fifo")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(logPath, 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	for _, args := range [][]string{
		{"run", "logs", "fifo", "--root", root},
		{"run", "logs", "--follow", "fifo", "--root", root},
	} {
		done := make(chan int, 1)
		go func() {
			code, _, _ := captureCLIOutput(t, func() int { return runSurfaceCommand(args) })
			done <- code
		}()
		select {
		case code := <-done:
			if code != exitError {
				t.Fatalf("%v code=%d, want %d", args, code, exitError)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%v blocked on a FIFO", args)
		}
	}
}

// Paging must distinguish the end of a walk from an empty ledger, and must say
// when more rows exist.
func TestCLIRunListDistinguishesEndOfPagesFromEmptiness(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	writeLedgerRows(t, root, `{"run_id":"r1","agent":"builder"}`, `{"run_id":"r2","agent":"builder"}`)

	_, page, _ := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "list", "--limit", "1", "--root", root})
	})
	if !strings.Contains(page, "more runs: forest run list --after r2") {
		t.Fatalf("page=%q, want the continuation cue", page)
	}
	_, end, _ := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "list", "--after", "r1", "--root", root})
	})
	if !strings.Contains(end, "no more runs after r1") {
		t.Fatalf("end=%q, want it distinguished from an empty ledger", end)
	}
	empty := t.TempDir()
	writeCLIConfig(t, empty, "exit 1")
	_, none, _ := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "list", "--root", empty})
	})
	if strings.TrimSpace(none) != "no runs" {
		t.Fatalf("empty ledger=%q, want %q", none, "no runs")
	}
}

// A Run with a log but no ledger row is a real state that `run logs` serves, so
// `run show` must point at it rather than deny the Run exists.
func TestCLIRunShowPointsAtALogWithoutALedgerRow(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	logPath := runLogPath(root, "inflight")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("started\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "show", "inflight", "--root", root})
	})
	if code != exitNotFound {
		t.Fatalf("code=%d, want %d", code, exitNotFound)
	}
	if !strings.Contains(stderr, "forest run logs inflight") {
		t.Fatalf("stderr=%q, want it to name the command that serves the log", stderr)
	}
}

// An in-flight Run has no exit code, so the payload must not publish 0 as one.
func TestCLIRunLogsOmitsExitForAnIncompleteRun(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	logPath := runLogPath(root, "live")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("working\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, envelope, _ := decodeEnvelope(t, "run", "logs", "live", "--json", "--root", root)
	keys := payloadKeys(t, envelope)
	if keys["complete"] != false {
		t.Fatalf("payload=%v, want complete=false", keys)
	}
	if keys["exit"] != nil {
		t.Fatalf("payload=%v, want no exit code for an incomplete Run", keys)
	}
}

func TestCLIRunRowShowsExitInside80Columns(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	runID := strings.Repeat("n", 200)
	agent := strings.Repeat("builder", 20)
	if err := AppendRun(root, RunRecord{RunID: runID, Agent: agent, Exit: 0, Duration: 1.25}); err != nil {
		t.Fatal(err)
	}

	code, listOut, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "list", "--root", root})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("run list code=%d stderr=%q", code, stderr)
	}
	code, showOut, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "show", runID, "--root", root})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("run show code=%d stderr=%q", code, stderr)
	}
	code, statusOut, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"status", "--root", root})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("status code=%d stderr=%q", code, stderr)
	}

	for name, out := range map[string]string{"run list": listOut, "run show": showOut, "status": statusOut} {
		line := strings.Split(out, "\n")[0]
		if name == "status" {
			for _, candidate := range strings.Split(out, "\n") {
				if strings.Contains(candidate, "exit=0") {
					line = candidate
					break
				}
			}
		}
		head := line
		if len(head) > 80 {
			head = head[:80]
		}
		if !strings.Contains(head, "exit=0") || !strings.Contains(head, "duration=1.250s") {
			t.Fatalf("%s first 80=%q, want exit and duration", name, head)
		}
	}

	_, envelope, _ := decodeEnvelope(t, "run", "show", runID, "--json", "--root", root)
	keys := payloadKeys(t, envelope)
	if keys["run_id"] != runID || keys["agent"] != agent {
		t.Fatalf("JSON hid identity: %v", keys)
	}
}

// The Ledger token classes are observability, not accounting. Every run read
// surface must publish the five canonical snake_case fields and nothing that
// looks like a monetary field.
func TestCLIRunSurfacesPublishCanonicalTokenFieldsWithoutCost(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	want := RunRecord{
		RunID:      "run-tokens",
		Agent:      "builder",
		Started:    "2026-08-10T00:00:00Z",
		Duration:   1.25,
		Exit:       0,
		TokensIn:   3,
		TokensOut:  5,
		CacheRead:  7,
		CacheWrite: 11,
		Reasoning:  13,
	}
	if err := AppendRun(root, want); err != nil {
		t.Fatal(err)
	}

	_, show, _ := decodeEnvelope(t, "run", "show", "run-tokens", "--json", "--root", root)
	var showRecord RunRecord
	decodePayload(t, show, &showRecord)
	requireTokenFields(t, showRecord, want, "run show")
	requireNoMonetaryFields(t, payloadKeys(t, show), "run show")

	_, list, _ := decodeEnvelope(t, "run", "list", "--json", "--root", root)
	var listPayload runListPayload
	decodePayload(t, list, &listPayload)
	if len(listPayload.Runs) != 1 {
		t.Fatalf("run list runs=%d, want 1", len(listPayload.Runs))
	}
	requireTokenFields(t, listPayload.Runs[0], want, "run list")
	requireNoMonetaryFields(t, nestedRunKeys(t, list), "run list")

	_, status, _ := decodeEnvelope(t, "status", "--json", "--root", root)
	var statusPayload statusPayload
	decodePayload(t, status, &statusPayload)
	if len(statusPayload.Recent) != 1 {
		t.Fatalf("status recent=%d, want 1", len(statusPayload.Recent))
	}
	requireTokenFields(t, statusPayload.Recent[0], want, "status")
	requireNoMonetaryFields(t, nestedRunKeys(t, status), "status")
}

func TestCLIRunStatusEmptyLedgerPublishesEmptyRecentArray(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	_, envelope, _ := decodeEnvelope(t, "status", "--json", "--root", root)
	encoded, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(encoded, &data); err != nil {
		t.Fatal(err)
	}
	recent, ok := data["recent"].([]any)
	if !ok {
		t.Fatalf("status --json empty ledger recent=%v (%T), want []", data["recent"], data["recent"])
	}
	if len(recent) != 0 {
		t.Fatalf("status --json empty ledger recent length=%d, want 0", len(recent))
	}
}

func requireTokenFields(t *testing.T, got, want RunRecord, surface string) {
	t.Helper()
	for name, pair := range map[string][2]int64{
		"tokens_in":   {got.TokensIn, want.TokensIn},
		"tokens_out":  {got.TokensOut, want.TokensOut},
		"cache_read":  {got.CacheRead, want.CacheRead},
		"cache_write": {got.CacheWrite, want.CacheWrite},
		"reasoning":   {got.Reasoning, want.Reasoning},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("%s %s=%d, want %d", surface, name, pair[0], pair[1])
		}
	}
}

func requireNoMonetaryFields(t *testing.T, keys map[string]any, surface string) {
	t.Helper()
	for _, key := range []string{"cost", "price", "spend", "currency"} {
		if _, ok := keys[key]; ok {
			t.Fatalf("%s publishes monetary field %q: %v", surface, key, keys)
		}
	}
}

func nestedRunKeys(t *testing.T, envelope cliEnvelope) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(encoded, &data); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"runs", "recent"} {
		if rows, ok := data[key].([]any); ok && len(rows) > 0 {
			if row, ok := rows[0].(map[string]any); ok {
				return row
			}
		}
	}
	return data
}
