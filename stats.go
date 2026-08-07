package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/misty-step/iron-forest/core"
)

// statsCmd aggregates valid ledger rows without changing the append-only file.
type statsCmd struct {
	first   time.Time
	last    time.Time
	runs    []runRecord
	invalid int
}

type groupTotals struct {
	count      int
	in         int64
	out        int64
	cacheRead  int64
	cacheWrite int64
	reasoning  int64
}

// keyed groups rows in first-seen order for stable operator output.
type keyed struct {
	totals map[string]groupTotals
	order  []string
	seen   map[string]bool
}

func newKeyed() *keyed {
	return &keyed{
		totals: make(map[string]groupTotals),
		seen:   make(map[string]bool),
	}
}

func (k *keyed) add(key string, r runRecord) {
	if key == "" {
		key = "(none)"
	}
	if !k.seen[key] {
		k.seen[key] = true
		k.order = append(k.order, key)
	}
	t := k.totals[key]
	t.count++
	t.in += r.TokensIn
	t.out += r.TokOut
	t.cacheRead += r.CacheRead
	t.cacheWrite += r.CacheWrite
	t.reasoning += r.Reasoning
	k.totals[key] = t
}

func cmdStats(api core.API, args []string) int {
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
	if err := s.load(api); err != nil {
		fmt.Fprintf(os.Stderr, "forest: stats: %v\n", err)
		return 1
	}
	if asJSON {
		return s.emitJSON(os.Stdout)
	}
	return s.emitText(os.Stdout)
}

// coreRunRecord adapts a core ledger row back into the aggregator's internal
// shape so the reporting code below needs no further churn.
func coreRunRecord(r core.RunRecord) runRecord {
	return runRecord{
		Time:          r.Time,
		RunID:         r.RunID,
		Flow:          r.Flow,
		Subject:       r.Subject,
		Revision:      r.Revision,
		ID:            r.ID,
		Branch:        r.Branch,
		PRURL:         r.PRURL,
		Status:        r.Status,
		TokensIn:      r.TokensIn,
		TokOut:        r.TokensOut,
		Agent:         r.Agent,
		Model:         r.Model,
		BaseSHA:       r.BaseSHA,
		DefSHA:        r.DefSHA,
		ReviewVerdict: r.ReviewVerdict,
		Error:         r.Error,
	}
}

func (s *statsCmd) load(api core.API) error {
	records, invalid, err := api.Ledger(core.LedgerQuery{})
	if err != nil {
		return err
	}
	s.runs = make([]runRecord, 0, len(records))
	for _, r := range records {
		s.runs = append(s.runs, coreRunRecord(r))
	}
	s.invalid = invalid
	s.computeRange()
	return nil
}

func (s *statsCmd) categories() map[string]int {
	m := map[string]int{"progress": 0, "failed": 0, "other": 0}
	for _, r := range s.runs {
		m[runCategory(r.Status)]++
	}
	return m
}

func (s *statsCmd) computeRange() {
	for _, r := range s.runs {
		t, err := time.Parse(time.RFC3339, r.Time)
		if err != nil {
			continue
		}
		if s.first.IsZero() || t.Before(s.first) {
			s.first = t
		}
		if s.last.IsZero() || t.After(s.last) {
			s.last = t
		}
	}
}

// ledgerTotals holds the summed token classes across aggregate rows. Cached
// input stays apart from fresh input because the two bill at different rates.
type ledgerTotals struct {
	in         int64
	out        int64
	cacheRead  int64
	cacheWrite int64
	reasoning  int64
}

func (s *statsCmd) totals() ledgerTotals {
	var t ledgerTotals
	for _, r := range s.runs {
		t.in += r.TokensIn
		t.out += r.TokOut
		t.cacheRead += r.CacheRead
		t.cacheWrite += r.CacheWrite
		t.reasoning += r.Reasoning
	}
	return t
}

func (s *statsCmd) emitText(w io.Writer) int {
	fmt.Fprintf(w, "runs:      %d\n", len(s.runs))
	if !s.first.IsZero() && !s.last.IsZero() {
		fmt.Fprintf(w, "range:     %s..%s\n", s.first.Format(time.RFC3339), s.last.Format(time.RFC3339))
	}

	fmt.Fprintln(w, "\nstatus:")
	s.emitTextGroup(w, func(r runRecord) string { return r.Status })
	categories := s.categories()
	fmt.Fprintf(w, "\ncategory:  progress=%d failed=%d other=%d\n", categories["progress"], categories["failed"], categories["other"])
	t := s.totals()
	fmt.Fprintf(w, "tokens:    input=%d cache_read=%d cache_write=%d reasoning=%d output=%d\n",
		t.in, t.cacheRead, t.cacheWrite, t.reasoning, t.out)

	s.emitTextBreakdown(w, "by flow:", func(r runRecord) string { return r.Flow })
	s.emitTextBreakdown(w, "by agent:", func(r runRecord) string { return r.Agent })
	s.emitTextBreakdown(w, "by model:", func(r runRecord) string { return r.Model })
	if s.invalid > 0 {
		fmt.Fprintf(w, "\ninvalid:   %d unparseable line(s) skipped\n", s.invalid)
	}
	return 0
}

func (s *statsCmd) emitTextGroup(w io.Writer, key func(runRecord) string) {
	items := s.breakdown(key)
	for _, item := range items {
		fmt.Fprintf(w, "  %-20s count=%d tokens_in=%d cache_read=%d cache_write=%d reasoning=%d tokens_out=%d\n",
			item["key"], item["count"], item["tokens_in"], item["cache_read"],
			item["cache_write"], item["reasoning"], item["tokens_out"])
	}
}

func (s *statsCmd) emitTextBreakdown(w io.Writer, title string, key func(runRecord) string) {
	fmt.Fprintf(w, "\n%s\n", title)
	s.emitTextGroup(w, key)
}

func (s *statsCmd) emitJSON(w io.Writer) int {
	enc := json.NewEncoder(w)
	if err := enc.Encode(map[string]any{
		"group":   "overview",
		"runs":    len(s.runs),
		"first":   fmtTime(s.first),
		"last":    fmtTime(s.last),
		"invalid": s.invalid,
		"status":  s.categories(),
	}); err != nil {
		return 1
	}
	for _, group := range []struct {
		name string
		key  func(runRecord) string
	}{
		{"status", func(r runRecord) string { return r.Status }},
		{"by_flow", func(r runRecord) string { return r.Flow }},
		{"by_agent", func(r runRecord) string { return r.Agent }},
		{"by_model", func(r runRecord) string { return r.Model }},
	} {
		if err := enc.Encode(map[string]any{"group": group.name, "items": s.breakdown(group.key)}); err != nil {
			return 1
		}
	}
	t := s.totals()
	for _, c := range []struct {
		group string
		total int64
	}{
		{"tokens_in", t.in},
		{"tokens_out", t.out},
		{"cache_read", t.cacheRead},
		{"cache_write", t.cacheWrite},
		{"reasoning", t.reasoning},
	} {
		if err := enc.Encode(map[string]any{"group": c.group, "total": c.total}); err != nil {
			return 1
		}
	}
	return 0
}

func (s *statsCmd) breakdown(key func(runRecord) string) []map[string]any {
	k := newKeyed()
	for _, r := range s.runs {
		k.add(key(r), r)
	}
	var out []map[string]any
	for _, name := range k.order {
		t := k.totals[name]
		out = append(out, map[string]any{
			"key":         name,
			"count":       t.count,
			"tokens_in":   t.in,
			"tokens_out":  t.out,
			"cache_read":  t.cacheRead,
			"cache_write": t.cacheWrite,
			"reasoning":   t.reasoning,
		})
	}
	return out
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
