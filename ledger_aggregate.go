package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// statusRecentFailures bounds the most recent failure reasons `status` reports.
const statusRecentFailures = 10

// statusLedgerAggregates is the Ledger roll-up `status` publishes. It is
// observability only: token fields are the five retained Ledger token classes
// (ADR 0011), and no cost, price, spend, or currency value is computed.
type statusLedgerAggregates struct {
	Runs           int                 `json:"runs"`
	PassRate       float64             `json:"pass_rate"`
	Agents         []statusAgentLedger `json:"agents"`
	RecentFailures []statusRunFailure  `json:"recent_failures"`
}

// statusAgentLedger is one agent's Ledger roll-up. Agents with no rows are
// absent, not zero-filled: the Ledger only knows about agents that ran.
type statusAgentLedger struct {
	Agent       string  `json:"agent"`
	Runs        int     `json:"runs"`
	PassRate    float64 `json:"pass_rate"`
	DurationP50 float64 `json:"duration_p50"`
	DurationP95 float64 `json:"duration_p95"`
	TokensIn    int64   `json:"tokens_in"`
	TokensOut   int64   `json:"tokens_out"`
	CacheRead   int64   `json:"cache_read"`
	CacheWrite  int64   `json:"cache_write"`
	Reasoning   int64   `json:"reasoning"`
}

// statusRunFailure is one non-zero Ledger row, newest first. Error carries the
// recorded failure reason; it stays empty when the row recorded no reason.
type statusRunFailure struct {
	RunID string `json:"run_id"`
	Agent string `json:"agent"`
	Exit  int    `json:"exit"`
	Error string `json:"error,omitempty"`
}

// tailRuns returns the last n records in Ledger order, or the whole slice when
// it has n or fewer rows. It preserves the oldest-first order of readLedger.
func tailRuns(records []RunRecord, n int) []RunRecord {
	if len(records) <= n {
		return records
	}
	return records[len(records)-n:]
}

// computeLedgerAggregates rolls up every Ledger row in one pass. records must
// be in Ledger order (oldest first), which is what readLedger(root, -1) returns.
func computeLedgerAggregates(records []RunRecord) statusLedgerAggregates {
	aggregates := statusLedgerAggregates{Agents: []statusAgentLedger{}}
	type agentAcc struct {
		passes     int
		durations  []float64
		tokensIn   int64
		tokensOut  int64
		cacheRead  int64
		cacheWrite int64
		reasoning  int64
	}
	byAgent := make(map[string]*agentAcc)
	total, passes := 0, 0
	for _, record := range records {
		total++
		if record.Exit == 0 {
			passes++
		}
		acc := byAgent[record.Agent]
		if acc == nil {
			acc = &agentAcc{}
			byAgent[record.Agent] = acc
		}
		if record.Exit == 0 {
			acc.passes++
		}
		acc.durations = append(acc.durations, record.Duration)
		acc.tokensIn += record.TokensIn
		acc.tokensOut += record.TokensOut
		acc.cacheRead += record.CacheRead
		acc.cacheWrite += record.CacheWrite
		acc.reasoning += record.Reasoning
	}
	aggregates.Runs = total
	if total > 0 {
		aggregates.PassRate = float64(passes) / float64(total)
	}
	names := make([]string, 0, len(byAgent))
	for name := range byAgent {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		acc := byAgent[name]
		durations := append([]float64(nil), acc.durations...)
		sort.Float64s(durations)
		agent := statusAgentLedger{
			Agent:       name,
			Runs:        len(acc.durations),
			DurationP50: percentile(durations, 50),
			DurationP95: percentile(durations, 95),
			TokensIn:    acc.tokensIn,
			TokensOut:   acc.tokensOut,
			CacheRead:   acc.cacheRead,
			CacheWrite:  acc.cacheWrite,
			Reasoning:   acc.reasoning,
		}
		if len(acc.durations) > 0 {
			agent.PassRate = float64(acc.passes) / float64(len(acc.durations))
		}
		aggregates.Agents = append(aggregates.Agents, agent)
	}

	failures := make([]statusRunFailure, 0, statusRecentFailures)
	for i := len(records) - 1; i >= 0 && len(failures) < statusRecentFailures; i-- {
		record := records[i]
		if record.Exit == 0 {
			continue
		}
		failures = append(failures, statusRunFailure{
			RunID: record.RunID,
			Agent: record.Agent,
			Exit:  record.Exit,
			Error: record.Error,
		})
	}
	aggregates.RecentFailures = failures
	return aggregates
}

// ledgerAggregatesHuman renders the roll-up for the human status view.
func ledgerAggregatesHuman(aggregates statusLedgerAggregates) string {
	var human strings.Builder
	fmt.Fprintf(&human, "ledger: runs=%d pass_rate=%.3f", aggregates.Runs, aggregates.PassRate)
	for _, agent := range aggregates.Agents {
		fmt.Fprintf(&human,
			"\n  %s runs=%d pass_rate=%.3f duration_p50=%.3fs duration_p95=%.3fs tokens_in=%d tokens_out=%d cache_read=%d cache_write=%d reasoning=%d",
			oneLine(agent.Agent), agent.Runs, agent.PassRate, agent.DurationP50, agent.DurationP95,
			agent.TokensIn, agent.TokensOut, agent.CacheRead, agent.CacheWrite, agent.Reasoning)
	}
	return human.String()
}

// recentFailuresHuman renders the most recent non-zero Ledger rows for the human
// status view. It reports the same run_id + reason facts the payload publishes.
func recentFailuresHuman(failures []statusRunFailure) string {
	if len(failures) == 0 {
		return "recent failures: none"
	}
	var human strings.Builder
	fmt.Fprintf(&human, "recent failures (newest first, at most %d):", statusRecentFailures)
	for _, failure := range failures {
		row := fmt.Sprintf("\n  exit=%d agent=%s run=%s", failure.Exit, oneLine(failure.Agent), oneLine(failure.RunID))
		if failure.Error != "" {
			row += " error=" + oneLine(failure.Error)
		}
		human.WriteString(row)
	}
	return human.String()
}

// percentile returns the nearest-rank percentile of a sorted, ascending slice.
// The empty slice returns zero so an agent with no rows has no invented timing.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	rank := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	return sorted[rank]
}
