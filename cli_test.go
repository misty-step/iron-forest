package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCLIConfigShowHumanAndJSON(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")

	code, stdout, stderr := captureCLIOutput(t, func() int { return runObjectCommand([]string{"config", "show", "--root", root}) })
	if code != exitOK || stderr != "" {
		t.Fatalf("config show code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	for _, want := range []string{"repo: owner/name", "agent builder: poll="} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("config show missing %q in %s", want, stdout)
		}
	}

	code, stdout, stderr = captureCLIOutput(t, func() int { return runObjectCommand([]string{"config", "show", "--json", "--root", root}) })
	if code != exitOK || stderr != "" {
		t.Fatalf("config show --json code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("config show --json not an envelope: %v", err)
	}
	if envelope["schema"] != "forest.cli.v1" || envelope["exit"] != float64(exitOK) || envelope["error"] != nil {
		t.Fatalf("bad envelope: %#v", envelope)
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok || data["repo"] != "owner/name" {
		t.Fatalf("bad data: %#v", data)
	}
}

func TestCLIDeclarationListAndShowNotFound(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	writeTestDeclaration(t, root, "builder")

	code, stdout, stderr := captureCLIOutput(t, func() int { return runObjectCommand([]string{"declaration", "list", "--json", "--root", root}) })
	if code != exitOK || stderr != "" {
		t.Fatalf("declaration list code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("declaration list --json: %v", err)
	}
	decls := envelope["data"].(map[string]any)["declarations"].([]any)
	if len(decls) != 1 {
		t.Fatalf("declaration list length=%d, want 1", len(decls))
	}

	code, stdout, stderr = captureCLIOutput(t, func() int {
		return runObjectCommand([]string{"declaration", "show", "builder", "--json", "--root", root})
	})
	if code != exitOK || !strings.Contains(stdout, "forest.cli.v1") {
		t.Fatalf("declaration show code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}

	code, _, stderr = captureCLIOutput(t, func() int { return runObjectCommand([]string{"declaration", "show", "missing", "--root", root}) })
	if code != exitNotFound {
		t.Fatalf("declaration show missing code=%d, want %d (stderr=%s)", code, exitNotFound, stderr)
	}
}

func TestCLITriggerResetClearsErrorsAndIsAtomic(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		t.Fatal(err)
	}
	triggers := []byte(`{"builder":{"agent":"builder","consecutive_errors":4,"last_code":2,"poll_error":"poll down","run_error":"run down","audit_error":"audit down","last_run":"2026-01-01T00:00:00Z"}}`)
	if err := os.WriteFile(forestPath(root, "triggers.json"), triggers, 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := captureCLIOutput(t, func() int { return runObjectCommand([]string{"trigger", "reset", "builder", "--json", "--root", root}) })
	if code != exitOK || stderr != "" {
		t.Fatalf("trigger reset code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("trigger reset --json: %v", err)
	}
	trigger := envelope["data"].(map[string]any)["trigger"].(map[string]any)
	if trigger["consecutive_errors"] != float64(0) || trigger["poll_error"] != nil || trigger["run_error"] != nil {
		t.Fatalf("reset did not clear errors: %#v", trigger)
	}
	// last_run and identity survive the reset.
	if trigger["last_run"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("reset dropped last_run: %#v", trigger)
	}

	code, _, stderr = captureCLIOutput(t, func() int { return runObjectCommand([]string{"trigger", "reset", "missing", "--root", root}) })
	if code != exitNotFound {
		t.Fatalf("trigger reset missing code=%d, want %d (stderr=%s)", code, exitNotFound, stderr)
	}
}

func TestCLIRunListPagingNewestFirst(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	for index := range 5 {
		record := RunRecord{RunID: "run-" + string(rune('a'+index)), Agent: "builder", Exit: index, Duration: float64(index)}
		if err := AppendRun(root, record); err != nil {
			t.Fatal(err)
		}
	}

	page1 := runListForTest(t, root, 2, "")
	if len(page1.runs) != 2 {
		t.Fatalf("page1 len=%d, want 2", len(page1.runs))
	}
	if page1.runs[0].RunID != "run-e" || page1.runs[1].RunID != "run-d" {
		t.Fatalf("page1 newest-first order wrong: %v", runIDs(page1.runs))
	}
	if page1.nextAfter != "run-d" {
		t.Fatalf("page1 next_after=%q, want run-d", page1.nextAfter)
	}

	page2 := runListForTest(t, root, 2, page1.nextAfter)
	if len(page2.runs) != 2 || page2.runs[0].RunID != "run-c" || page2.runs[1].RunID != "run-b" {
		t.Fatalf("page2 wrong: %v", runIDs(page2.runs))
	}
	if page2.nextAfter != "run-b" {
		t.Fatalf("page2 next_after=%q, want run-b", page2.nextAfter)
	}
}

type listPage struct {
	runs      []RunRecord
	nextAfter string
}

func runListForTest(t *testing.T, root string, limit int, after string) listPage {
	t.Helper()
	args := []string{"run", "list", "--json", "--root", root}
	if limit > 0 {
		args = append(args, "--limit", string(rune('0'+limit)))
	}
	if after != "" {
		args = append(args, "--after", after)
	}
	code, stdout, stderr := captureCLIOutput(t, func() int { return runObjectCommand(args) })
	if code != exitOK || stderr != "" {
		t.Fatalf("run list code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	var envelope struct {
		Data runListPage `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("run list envelope decode: %v (%s)", err, stdout)
	}
	return listPage{runs: envelope.Data.Runs, nextAfter: envelope.Data.NextAfter}
}

type runListPage struct {
	Runs      []RunRecord `json:"runs"`
	NextAfter string      `json:"next_after"`
}

func runIDs(records []RunRecord) []string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.RunID)
	}
	return ids
}

func TestCLIRunShowNotFoundAndFound(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := AppendRun(root, RunRecord{RunID: "run-1", Agent: "builder", Exit: 0, Duration: 1.0}); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := captureCLIOutput(t, func() int { return runObjectCommand([]string{"run", "show", "run-missing", "--root", root}) })
	if code != exitNotFound {
		t.Fatalf("run show missing code=%d, want %d (stderr=%s)", code, exitNotFound, stderr)
	}

	code, stdout, stderr := captureCLIOutput(t, func() int { return runObjectCommand([]string{"run", "show", "run-1", "--json", "--root", root}) })
	if code != exitOK || stderr != "" || !strings.Contains(stdout, "run-1") {
		t.Fatalf("run show code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
}

func TestCLIRunLogsFinishedAndExitsWithRunCode(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := AppendRun(root, RunRecord{RunID: "run-7-builder", Agent: "builder", Exit: 3, Duration: 2.0}); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(root, workspaceName, "runs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "run-7-builder.log"), []byte("step one\nstep two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := captureCLIOutput(t, func() int { return runObjectCommand([]string{"run", "logs", "run-7-builder", "--root", root}) })
	if code != exitOK || stderr != "" || stdout != "step one\nstep two\n" {
		t.Fatalf("run logs code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	code, stdout, _ = captureCLIOutput(t, func() int {
		return runObjectCommand([]string{"run", "logs", "--follow", "run-7-builder", "--root", root})
	})
	if code != 3 {
		t.Fatalf("run logs --follow code=%d, want run exit 3 (stdout=%q)", code, stdout)
	}
	if !strings.Contains(stdout, "step two") {
		t.Fatalf("run logs --follow stdout missing content: %q", stdout)
	}
}

func TestCLIRunLogsMissingRunIsNotFound(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	code, _, stderr := captureCLIOutput(t, func() int { return runObjectCommand([]string{"run", "logs", "ghost", "--root", root}) })
	if code != exitNotFound {
		t.Fatalf("run logs ghost code=%d, want %d (stderr=%s)", code, exitNotFound, stderr)
	}
}

func TestCLIRunLogsFollowWaitsForCompletion(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	logDir := filepath.Join(root, workspaceName, "runs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logDir, "run-live-builder.log")
	if err := os.WriteFile(logPath, []byte("started\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		appendFile := func(path, text string) {
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			if err == nil {
				_, _ = file.WriteString(text)
				_ = file.Close()
			}
		}
		appendFile(logPath, "finished\n")
		if err := AppendRun(root, RunRecord{RunID: "run-live-builder", Agent: "builder", Exit: 0, Duration: 1.0}); err != nil {
			t.Error(err)
		}
	}()

	code, stdout, stderr := captureCLIOutput(t, func() int {
		return runObjectCommand([]string{"run", "logs", "--follow", "run-live-builder", "--root", root})
	})
	if code != exitOK {
		t.Fatalf("run logs --follow live code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "finished") {
		t.Fatalf("run logs --follow stdout=%q missing finished", stdout)
	}
}

func TestCLIAuditShowAndLog(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	if err := os.MkdirAll(filepath.Join(root, workspaceName), 0o755); err != nil {
		t.Fatal(err)
	}
	state := AuditState{Baseline: "a", LastMaster: "abc123", LastAt: "2026-01-01T00:00:00Z", LastResult: "violations", Violations: []string{"v1", "v2"}}
	if err := writeAuditState(root, state, defaultAuditDependencies()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auditLogPath(root), []byte("h1\nh2\nh3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := captureCLIOutput(t, func() int { return runObjectCommand([]string{"audit", "show", "--json", "--root", root}) })
	if code != exitOK || stderr != "" || !strings.Contains(stdout, "violations") {
		t.Fatalf("audit show code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}

	code, stdout, stderr = captureCLIOutput(t, func() int {
		return runObjectCommand([]string{"audit", "log", "--limit", "2", "--json", "--root", root})
	})
	if code != exitOK || stderr != "" {
		t.Fatalf("audit log limit code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	var page struct {
		Data struct {
			Entries []string `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &page); err != nil {
		t.Fatalf("audit log envelope decode: %v (%s)", err, stdout)
	}
	if len(page.Data.Entries) != 2 || page.Data.Entries[0] != "h2" || page.Data.Entries[1] != "h3" {
		t.Fatalf("audit log latest-2 entries=%v", page.Data.Entries)
	}
}

func TestCLIInvalidArgumentExitCodes(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")

	cases := []struct {
		name string
		args []string
		want int
	}{
		{name: "unknown flag", args: []string{"config", "show", "--bogus", "--root", root}, want: exitInvalidArg},
		{name: "bad limit", args: []string{"run", "list", "--limit", "0", "--root", root}, want: exitInvalidArg},
		{name: "missing subcommand", args: []string{"trigger", "--root", root}, want: exitInvalidArg},
		{name: "unknown run verb", args: []string{"run", "frobnicate", "--root", root}, want: exitInvalidArg},
		{name: "status unknown flag", args: []string{"status", "--bogus", "--root", root}, want: exitInvalidArg},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _ := captureCLIOutput(t, func() int {
				if tc.args[0] == "status" {
					return runCLI(tc.args)
				}
				return runObjectCommand(tc.args)
			})
			if code != tc.want {
				t.Fatalf("code=%d, want %d", code, tc.want)
			}
		})
	}
}

func TestCLIObjectCommandsRequireConfig(t *testing.T) {
	root := t.TempDir()
	code, _, stderr := captureCLIOutput(t, func() int { return runObjectCommand([]string{"config", "show", "--root", root}) })
	if code != exitError || stderr == "" {
		t.Fatalf("config show without config code=%d stderr=%q", code, stderr)
	}
}
