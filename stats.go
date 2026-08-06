package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// statsCmd aggregates valid ledger rows without changing the append-only file.
type statsCmd struct {
	first   time.Time
	last    time.Time
	runs    []runRecord
	invalid int
}

type groupTotals struct {
	count int
	in    int64
	out   int64
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
	k.totals[key] = t
}

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

func (s *statsCmd) load(path string) error {
	runs, invalid, err := loadLedger(path)
	if err != nil {
		return err
	}
	s.runs = runs
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

func (s *statsCmd) totals() (int64, int64) {
	var in, out int64
	for _, r := range s.runs {
		in += r.TokensIn
		out += r.TokOut
	}
	return in, out
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
	in, out := s.totals()
	fmt.Fprintf(w, "tokens:    input=%d output=%d\n", in, out)

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
		fmt.Fprintf(w, "  %-20s count=%d tokens_in=%d tokens_out=%d\n",
			item["key"], item["count"], item["tokens_in"], item["tokens_out"])
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
	in, out := s.totals()
	if err := enc.Encode(map[string]any{"group": "tokens_in", "total": in}); err != nil {
		return 1
	}
	if err := enc.Encode(map[string]any{"group": "tokens_out", "total": out}); err != nil {
		return 1
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
			"key":        name,
			"count":      t.count,
			"tokens_in":  t.in,
			"tokens_out": t.out,
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
