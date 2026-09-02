package main

import (
	"math"
	"testing"
)

func TestComputeLedgerAggregatesRollsUpPassRateDurationsAndTokens(t *testing.T) {
	records := []RunRecord{
		{RunID: "a1", Agent: "builder", Exit: 0, Duration: 1, TokensIn: 10, TokensOut: 20, CacheRead: 30, CacheWrite: 40, Reasoning: 50},
		{RunID: "a2", Agent: "builder", Exit: 0, Duration: 2, TokensIn: 1, TokensOut: 2, CacheRead: 3, CacheWrite: 4, Reasoning: 5},
		{RunID: "a3", Agent: "builder", Exit: 1, Duration: 3, TokensIn: 100, TokensOut: 200, CacheRead: 300, CacheWrite: 400, Reasoning: 500},
		{RunID: "v1", Agent: "verifier", Exit: 0, Duration: 4, TokensIn: 7, TokensOut: 8, CacheRead: 9, CacheWrite: 10, Reasoning: 11},
	}
	aggregates := computeLedgerAggregates(records)
	if aggregates.Runs != 4 {
		t.Fatalf("runs=%d, want 4", aggregates.Runs)
	}
	if math.Abs(aggregates.PassRate-0.75) > 1e-9 {
		t.Fatalf("pass_rate=%v, want 0.75", aggregates.PassRate)
	}
	byAgent := map[string]statusAgentLedger{}
	for _, agent := range aggregates.Agents {
		byAgent[agent.Agent] = agent
	}
	builder := byAgent["builder"]
	if builder.Runs != 3 || math.Abs(builder.PassRate-2.0/3.0) > 1e-9 {
		t.Fatalf("builder=%+v, want 3 runs at 2/3 pass", builder)
	}
	// Sorted durations are [1,2,3]; nearest-rank p50 is 2 and p95 is 3.
	if builder.DurationP50 != 2 || builder.DurationP95 != 3 {
		t.Fatalf("builder durations p50=%v p95=%v, want 2 and 3", builder.DurationP50, builder.DurationP95)
	}
	if builder.TokensIn != 111 || builder.TokensOut != 222 || builder.CacheRead != 333 || builder.CacheWrite != 444 || builder.Reasoning != 555 {
		t.Fatalf("builder token totals=%+v, want 111/222/333/444/555", builder)
	}
	verifier := byAgent["verifier"]
	if verifier.Runs != 1 || verifier.DurationP50 != 4 || verifier.DurationP95 != 4 {
		t.Fatalf("verifier=%+v, want one 4s run", verifier)
	}
	if len(aggregates.RecentFailures) != 1 || aggregates.RecentFailures[0].RunID != "a3" || aggregates.RecentFailures[0].Agent != "builder" {
		t.Fatalf("recent failures=%+v, want only a3 newest-first", aggregates.RecentFailures)
	}
}

func TestComputeLedgerAggregatesEmptyLedgerHasNoInventedRows(t *testing.T) {
	aggregates := computeLedgerAggregates(nil)
	if aggregates.Runs != 0 || aggregates.PassRate != 0 {
		t.Fatalf("empty aggregates=%+v, want zero runs and pass rate", aggregates)
	}
	if aggregates.Agents == nil || len(aggregates.Agents) != 0 {
		t.Fatalf("empty agents=%v, want empty list", aggregates.Agents)
	}
	if aggregates.RecentFailures == nil || len(aggregates.RecentFailures) != 0 {
		t.Fatalf("empty recent failures=%v, want empty list", aggregates.RecentFailures)
	}
}

func TestStatusPublishesLedgerAggregatesInJSON(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "poll")
	writeLedgerRows(t, root,
		`{"run_id":"ok-1","agent":"builder","started":"2026-08-21T00:00:00Z","duration":1,"exit":0,"tokens_in":10,"tokens_out":20,"cache_read":30,"cache_write":40,"reasoning":50}`,
		`{"run_id":"fail-1","agent":"builder","started":"2026-08-22T00:00:00Z","duration":2,"exit":2,"error":"boom","tokens_in":1,"tokens_out":2,"cache_read":3,"cache_write":4,"reasoning":5}`,
	)

	_, envelope, stderr := decodeEnvelope(t, "status", "--json", "--root", root)
	if stderr != "" {
		t.Fatalf("stderr=%q, want silence", stderr)
	}
	keys := payloadKeys(t, envelope)
	ledger, ok := keys["ledger"].(map[string]any)
	if !ok {
		t.Fatalf("ledger=%v, want an aggregate object", keys["ledger"])
	}
	if ledger["runs"] != float64(2) {
		t.Fatalf("ledger.runs=%v, want 2", ledger["runs"])
	}
	if math.Abs(ledger["pass_rate"].(float64)-0.5) > 1e-9 {
		t.Fatalf("ledger.pass_rate=%v, want 0.5", ledger["pass_rate"])
	}
	agents, ok := ledger["agents"].([]any)
	if !ok || len(agents) != 1 {
		t.Fatalf("ledger.agents=%v, want one agent", ledger["agents"])
	}
	agent := agents[0].(map[string]any)
	if agent["agent"] != "builder" || agent["runs"] != float64(2) {
		t.Fatalf("agent=%v, want builder with 2 runs", agent)
	}
	if agent["tokens_in"] != float64(11) || agent["reasoning"] != float64(55) {
		t.Fatalf("agent token totals=%v, want tokens_in=11 reasoning=55", agent)
	}
	failures, ok := ledger["recent_failures"].([]any)
	if !ok || len(failures) != 1 {
		t.Fatalf("ledger.recent_failures=%v, want one failure", ledger["recent_failures"])
	}
	failure := failures[0].(map[string]any)
	if failure["run_id"] != "fail-1" || failure["exit"] != float64(2) || failure["error"] != "boom" {
		t.Fatalf("failure=%v, want fail-1 exit 2 error boom", failure)
	}
}
