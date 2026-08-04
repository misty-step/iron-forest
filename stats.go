package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// statsCmd aggregates the run ledger (.forest/runs.jsonl) into numbers an
// operator can read. It is the CLI face of what the reaction loop (R3) needs
// to prove it works. Reading is append-only and never mutates the ledger.
type statsCmd struct {
	first   time.Time   // timestamp of the first run in the ledger
	last    time.Time   // timestamp of the last run in the ledger
	runs    []runRecord // parsed, valid ledger lines
	invalid int         // ledger lines that could not be parsed
}

// keyed is a rate-based accumulator used for the by-agent, by-model and
// by-def_sha breakdowns: total cost and run count per key, in encounter order.
type keyed struct {
	cost  map[string]float64
	count map[string]int
	order []string
	seen  map[string]bool
}

func newKeyed() *keyed {
	return &keyed{
		cost:  make(map[string]float64),
		count: make(map[string]int),
		seen:  make(map[string]bool),
	}
}

// add records one run against a key, preserving the first-seen order.
func (k *keyed) add(key string, cost float64) {
	if key == "" {
		key = "(none)"
	}
	if !k.seen[key] {
		k.seen[key] = true
		k.order = append(k.order, key)
	}
	k.cost[key] += cost
	k.count[key]++
}

// cmdStats runs `forest stats` against the ledger under repoDir and prints the
// aggregate. Passing --json emits one JSON object per aggregate group.
func cmdStats(repoDir string, args []string) int {
	asJSON := false
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		default:
			fmt.Fprintf(os.Stderr, "forest: stats: unknown flag %q\n", a)
			return 2
		}
	}
	s := &statsCmd{}
	if err := s.load(filepath.Join(repoDir, WorkspaceDir, "runs.jsonl")); err != nil {
		fmt.Fprintf(os.Stderr, "forest: stats: %v\n", err)
		return 1
	}
	if asJSON {
		return s.emitJSON(os.Stdout)
	}
	return s.emitText(os.Stdout)
}

// load reads and validates every ledger line. Unparseable lines are counted
// and skipped so one bad artifact never breaks the whole report.
func (s *statsCmd) load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// An empty ledger is a valid state: report zeroes rather than fail.
			return nil
		}
		return fmt.Errorf("open ledger: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r runRecord
		if err := json.Unmarshal(line, &r); err != nil {
			s.invalid++
			continue
		}
		s.runs = append(s.runs, r)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read ledger: %w", err)
	}
	s.computeRange()
	return nil
}

// computeRange fills first/last from the RFC3339 time fields of the runs.
func (s *statsCmd) computeRange() {
	var first, last time.Time
	for _, r := range s.runs {
		t, err := time.Parse(time.RFC3339, r.Time)
		if err != nil {
			continue
		}
		if first.IsZero() || t.Before(first) {
			first = t
		}
		if last.IsZero() || t.After(last) {
			last = t
		}
	}
	s.first, s.last = first, last
}

// mean returns average of sum over count, or 0 when count is zero.
func mean(sum float64, count int) float64 {
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

// emitText prints the human-readable aggregate report.
func (s *statsCmd) emitText(w io.Writer) int {
	if len(s.runs) == 0 {
		fmt.Fprintf(w, "runs:      0 (empty ledger or no valid lines)\n")
		if s.invalid > 0 {
			fmt.Fprintf(w, "invalid:   %d\n", s.invalid)
		}
		return 0
	}
	fmt.Fprintf(w, "runs:      %d\n", len(s.runs))
	if !s.first.IsZero() && !s.last.IsZero() {
		fmt.Fprintf(w, "range:     %s..%s\n", s.first.Format(time.RFC3339), s.last.Format(time.RFC3339))
	}

	fmt.Fprintf(w, "\nstatus:\n")
	byStatus := map[string]int{}
	var statusKeys []string
	for _, r := range s.runs {
		if byStatus[r.Status] == 0 {
			statusKeys = append(statusKeys, r.Status)
		}
		byStatus[r.Status]++
	}
	sort.Strings(statusKeys)
	for _, k := range statusKeys {
		fmt.Fprintf(w, "  %-20s %d\n", k, byStatus[k])
	}

	var totalCost, totalIn, totalOut float64
	for _, r := range s.runs {
		totalCost += r.CostUSD
		totalIn += float64(r.TokensIn)
		totalOut += float64(r.TokOut)
	}
	n := len(s.runs)
	fmt.Fprintf(w, "\ncost_usd   total=%8.4f mean=%8.4f\n", totalCost, mean(totalCost, n))
	fmt.Fprintf(w, "tokens_in  total=%10d mean=%10.1f\n", int64(totalIn), mean(totalIn, n))
	fmt.Fprintf(w, "tokens_out total=%10d mean=%10.1f\n", int64(totalOut), mean(totalOut, n))

	s.emitBreakdown(w, "by agent:", func(r runRecord) string { return r.Agent })
	s.emitBreakdown(w, "by model:", func(r runRecord) string { return r.Model })
	s.emitBreakdown(w, "by def_sha:", func(r runRecord) string { return r.DefSHA })

	if s.invalid > 0 {
		fmt.Fprintf(w, "\ninvalid:   %d unparseable line(s) skipped\n", s.invalid)
	}
	return 0
}

// emitBreakdown prints one grouped table (agent/model/def_sha) for text mode.
func (s *statsCmd) emitBreakdown(w io.Writer, title string, key func(runRecord) string) {
	k := newKeyed()
	for _, r := range s.runs {
		k.add(key(r), r.CostUSD)
	}
	fmt.Fprintf(w, "\n%s\n", title)
	for _, name := range k.order {
		fmt.Fprintf(w, "  %-24s cost=%8.4f count=%d\n", name, k.cost[name], k.count[name])
	}
}

// emitJSON prints one JSON object per aggregate group, one object per line so
// the stream is trivially parseable and grep-able.
func (s *statsCmd) emitJSON(w io.Writer) int {
	enc := json.NewEncoder(w)

	enc.Encode(map[string]any{
		"group":   "overview",
		"runs":    len(s.runs),
		"first":   fmtTime(s.first),
		"last":    fmtTime(s.last),
		"invalid": s.invalid,
	})

	byStatus := map[string]int{}
	for _, r := range s.runs {
		byStatus[r.Status]++
	}
	enc.Encode(map[string]any{"group": "status", "counts": byStatus})

	var totalCost, totalIn, totalOut float64
	for _, r := range s.runs {
		totalCost += r.CostUSD
		totalIn += float64(r.TokensIn)
		totalOut += float64(r.TokOut)
	}
	n := len(s.runs)
	enc.Encode(map[string]any{"group": "cost_usd", "total": totalCost, "mean": mean(totalCost, n)})
	enc.Encode(map[string]any{"group": "tokens_in", "total": totalIn, "mean": mean(totalIn, n)})
	enc.Encode(map[string]any{"group": "tokens_out", "total": totalOut, "mean": mean(totalOut, n)})

	enc.Encode(map[string]any{"group": "by_agent", "items": s.breakdown(func(r runRecord) string { return r.Agent })})
	enc.Encode(map[string]any{"group": "by_model", "items": s.breakdown(func(r runRecord) string { return r.Model })})
	enc.Encode(map[string]any{"group": "by_def_sha", "items": s.breakdown(func(r runRecord) string { return r.DefSHA })})
	return 0
}

// breakdown returns the per-key cost/count rows for JSON mode.
func (s *statsCmd) breakdown(key func(runRecord) string) []map[string]any {
	k := newKeyed()
	for _, r := range s.runs {
		k.add(key(r), r.CostUSD)
	}
	var out []map[string]any
	for _, name := range k.order {
		out = append(out, map[string]any{
			"key":   name,
			"cost":  k.cost[name],
			"count": k.count[name],
		})
	}
	return out
}

// fmtTime renders a timestamp for JSON, or "" when zero.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
