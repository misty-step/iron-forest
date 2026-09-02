package main

import (
	"testing"
)

func TestCLIRunListFiltersByAgent(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	rows := []RunRecord{
		{RunID: "b-1", Agent: "builder", Exit: 0},
		{RunID: "v-1", Agent: "verifier", Exit: 0},
		{RunID: "b-2", Agent: "builder", Exit: 1},
	}
	for _, record := range rows {
		if err := AppendRun(root, record); err != nil {
			t.Fatal(err)
		}
	}

	_, envelope, _ := decodeEnvelope(t, "run", "list", "--agent", "builder", "--json", "--root", root)
	var payload runListPayload
	decodePayload(t, envelope, &payload)
	if len(payload.Runs) != 2 {
		t.Fatalf("builder runs=%d, want 2", len(payload.Runs))
	}
	for _, record := range payload.Runs {
		if record.Agent != "builder" {
			t.Fatalf("filtered run agent=%q, want builder", record.Agent)
		}
	}
	// Newest first: b-2 was appended last.
	if payload.Runs[0].RunID != "b-2" || payload.Runs[1].RunID != "b-1" {
		t.Fatalf("filtered order=%v, want [b-2 b-1]", []string{payload.Runs[0].RunID, payload.Runs[1].RunID})
	}
}

func TestCLIRunListFiltersByExit(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	rows := []RunRecord{
		{RunID: "ok-1", Agent: "builder", Exit: 0},
		{RunID: "fail-1", Agent: "builder", Exit: 1},
		{RunID: "ok-2", Agent: "builder", Exit: 0},
	}
	for _, record := range rows {
		if err := AppendRun(root, record); err != nil {
			t.Fatal(err)
		}
	}

	_, envelope, _ := decodeEnvelope(t, "run", "list", "--exit", "1", "--json", "--root", root)
	var payload runListPayload
	decodePayload(t, envelope, &payload)
	if len(payload.Runs) != 1 || payload.Runs[0].RunID != "fail-1" {
		t.Fatalf("exit=1 runs=%+v, want [fail-1]", payload.Runs)
	}
}

func TestCLIRunListFiltersBySince(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	rows := []RunRecord{
		{RunID: "old", Agent: "builder", Started: "2026-08-09T00:00:00Z", Exit: 0},
		{RunID: "new", Agent: "builder", Started: "2026-08-11T00:00:00Z", Exit: 0},
		{RunID: "newer", Agent: "builder", Started: "2026-08-12T00:00:00Z", Exit: 0},
	}
	for _, record := range rows {
		if err := AppendRun(root, record); err != nil {
			t.Fatal(err)
		}
	}

	_, envelope, _ := decodeEnvelope(t, "run", "list", "--since", "2026-08-11T00:00:00Z", "--json", "--root", root)
	var payload runListPayload
	decodePayload(t, envelope, &payload)
	if len(payload.Runs) != 2 {
		t.Fatalf("since runs=%+v, want 2", payload.Runs)
	}
	if payload.Runs[0].RunID != "newer" || payload.Runs[1].RunID != "new" {
		t.Fatalf("since order=%v, want [newer new]", []string{payload.Runs[0].RunID, payload.Runs[1].RunID})
	}
}

func TestCLIRunListFilterAndCursorCompose(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	for i := 0; i < 5; i++ {
		record := RunRecord{RunID: "run-" + string(rune('a'+i)), Agent: "builder", Exit: 0}
		if err := AppendRun(root, record); err != nil {
			t.Fatal(err)
		}
	}
	if err := AppendRun(root, RunRecord{RunID: "run-f", Agent: "verifier", Exit: 0}); err != nil {
		t.Fatal(err)
	}

	_, first, _ := decodeEnvelope(t, "run", "list", "--agent", "builder", "--limit", "2", "--json", "--root", root)
	var firstPayload runListPayload
	decodePayload(t, first, &firstPayload)
	if len(firstPayload.Runs) != 2 || firstPayload.NextAfter == "" {
		t.Fatalf("first filtered page=%+v, want 2 rows and a cursor", firstPayload)
	}

	_, second, _ := decodeEnvelope(t, "run", "list", "--agent", "builder", "--limit", "2", "--after", firstPayload.NextAfter, "--json", "--root", root)
	var secondPayload runListPayload
	decodePayload(t, second, &secondPayload)
	if len(secondPayload.Runs) != 2 {
		t.Fatalf("second filtered page=%+v, want 2 rows", secondPayload.Runs)
	}
	seen := append([]string{firstPayload.Runs[0].RunID, firstPayload.Runs[1].RunID}, secondPayload.Runs[0].RunID, secondPayload.Runs[1].RunID)
	want := []string{"run-e", "run-d", "run-c", "run-b"}
	if len(seen) != len(want) {
		t.Fatalf("filtered walk=%v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("filtered walk=%v, want %v", seen, want)
		}
	}
}

func TestCLIRunListRejectsBadSince(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	code, _, _ := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"run", "list", "--since", "not-a-time", "--root", root})
	})
	if code != exitInvalidArg {
		t.Fatalf("code=%d, want %d", code, exitInvalidArg)
	}
}
